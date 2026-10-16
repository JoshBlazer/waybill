// Package payout sends naira to contractors for settled naira invoices
// (docs/design/payout.md). Every step goes through the pure
// statemachine.PayoutTransition, is recorded in payout_events (which also
// updates the tracking page), and posts its ledger entry in the same
// database transaction.
//
// Steps, run by Tick:
//   - queued: convert what arrived at the invoice's locked rate (or a new
//     quote when the money arrived after the quote expired). Card payments
//     are already naira.
//   - converted: once the bank account's 24-hour wait is over, record
//     submitting, commit, then ask the provider to transfer with the
//     payout's reference.
//   - unknown (or a submitting left by a crash): ask the provider about the
//     reference; resubmit with the same reference only if it has nothing.
//   - submitted: ask the provider until the transfer succeeds or fails.
package payout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshBlazer/waybill/api/internal/bankverify"
	"github.com/JoshBlazer/waybill/api/internal/invoice"
	"github.com/JoshBlazer/waybill/api/internal/ledger"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/paystack"
	"github.com/JoshBlazer/waybill/api/internal/rates"
	"github.com/JoshBlazer/waybill/api/internal/statemachine"
	"github.com/JoshBlazer/waybill/api/internal/store"
)

// Transfers is the part of the Paystack client payouts use.
type Transfers interface {
	CreateRecipient(ctx context.Context, name string, a bankverify.Account) (string, error)
	Initiate(ctx context.Context, t paystack.Transfer) (paystack.TransferStatus, error)
	Verify(ctx context.Context, ref string) (paystack.TransferStatus, error)
}

// Service runs payouts.
type Service struct {
	Pool      *pgxpool.Pool
	Transfers Transfers
	Quoter    *rates.Quoter // for late payments
	Log       *slog.Logger
	Now       func() time.Time
	// StuckSubmitting is how long a payout may sit in submitting before it
	// is treated as a crash mid-call and reconciled. Default 2 minutes.
	StuckSubmitting time.Duration
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Reference is the provider reference for a payout: the same on every
// attempt, so the provider refuses a second transfer.
func Reference(id uuid.UUID) string { return "po_" + strings.ReplaceAll(id.String(), "-", "") }

// Run ticks every interval until ctx is done.
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			s.Log.Warn("payout: tick failed; will retry", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick creates payouts for newly settled naira invoices, handles stored
// transfer callbacks, and moves every active payout one step if it can.
// Errors from one payout do not stop the others.
func (s *Service) Tick(ctx context.Context) error {
	if err := s.enqueue(ctx); err != nil {
		return err
	}
	if err := s.callbacks(ctx); err != nil {
		return err
	}
	active, err := store.New(s.Pool).ListActivePayouts(ctx)
	if err != nil {
		return err
	}
	var firstErr error
	for _, p := range active {
		if err := s.advance(ctx, p.ID); err != nil {
			s.Log.Warn("payout: step failed; will retry", "payout", p.ID, "state", p.State, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// enqueue makes one payout per settled naira invoice.
func (s *Service) enqueue(ctx context.Context) error {
	q := store.New(s.Pool)
	due, err := q.ListSettledNairaWithoutPayout(ctx)
	if err != nil {
		return err
	}
	for _, d := range due {
		asset, amount := money.USDC.Code, d.ChainAmount
		if d.ChainAmount.IsZero() {
			asset, amount = money.NGN.Code, d.CardAmount
		}
		if amount.Sign() <= 0 {
			s.Log.Error("payout: settled naira invoice with nothing received", "invoice", d.ID)
			continue
		}
		id := uuid.New()
		if _, err := q.InsertPayout(ctx, store.InsertPayoutParams{
			ID: id, InvoiceID: d.ID, ContractorID: d.ContractorID, SourceAsset: string(asset),
			SourceAmount: amount, Reference: Reference(id),
		}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	return nil
}

// callbacks turns stored transfer.* callbacks into steps of the payout they
// name. The step asks the provider itself; the callback body is not trusted.
func (s *Service) callbacks(ctx context.Context) error {
	q := store.New(s.Pool)
	for _, typ := range []string{"transfer.success", "transfer.failed", "transfer.reversed"} {
		events, err := q.ListUnprocessedProviderEvents(ctx, store.ListUnprocessedProviderEventsParams{Provider: "paystack", EventType: typ})
		if err != nil {
			return err
		}
		for _, e := range events {
			var body struct {
				Data struct {
					Reference string `json:"reference"`
				} `json:"data"`
			}
			_ = json.Unmarshal(e.Raw, &body)
			p, err := q.GetPayoutByReference(ctx, body.Data.Reference)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				// not one of ours
			case err != nil:
				return err
			case typ == "transfer.reversed" && p.State == string(statemachine.PayoutSucceeded):
				if err := s.checkReversal(ctx, p); err != nil {
					return err
				}
			default:
				if err := s.Step(ctx, p.ID); err != nil {
					return err
				}
			}
			if err := q.MarkProviderEventProcessed(ctx, e.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// advance steps a payout until it stops moving (waiting on the provider,
// the cooling-off period, or a person), at most a few steps per tick.
func (s *Service) advance(ctx context.Context, id uuid.UUID) error {
	q := store.New(s.Pool)
	for range 6 {
		before, err := q.LockPayout(ctx, id)
		if err != nil {
			return err
		}
		if err := s.Step(ctx, id); err != nil {
			return err
		}
		after, err := q.LockPayout(ctx, id)
		if err != nil {
			return err
		}
		if after.State == before.State {
			return nil
		}
	}
	return nil
}

// Step moves one payout forward if it can.
func (s *Service) Step(ctx context.Context, id uuid.UUID) error {
	p, err := store.New(s.Pool).LockPayout(ctx, id) // a plain read here; each step locks again in its transaction
	if err != nil {
		return err
	}
	switch statemachine.PayoutState(p.State) {
	case statemachine.PayoutQueued:
		return s.convert(ctx, p)
	case statemachine.PayoutConverted:
		return s.submit(ctx, p)
	case statemachine.PayoutSubmitting:
		stuck := s.StuckSubmitting
		if stuck == 0 {
			stuck = 2 * time.Minute
		}
		if s.now().Sub(p.UpdatedAt) < stuck {
			return nil // a submission may be in flight in another worker
		}
		// The call's outcome was never recorded: as good as no answer.
		if err := s.transition(ctx, p.ID, statemachine.EvProviderUnknown, map[string]any{"why": "submission not recorded; reconciling"}, nil); err != nil {
			return err
		}
		return s.reconcile(ctx, p.ID)
	case statemachine.PayoutUnknown:
		return s.reconcile(ctx, p.ID)
	case statemachine.PayoutSubmitted:
		return s.follow(ctx, p)
	}
	return nil
}

// transition applies ev to the payout inside its own transaction (or tx
// when given), records the event, and runs then (the step's ledger work)
// in the same transaction. An illegal transition changes nothing.
func (s *Service) transition(ctx context.Context, id uuid.UUID, ev statemachine.PayoutEvent, data map[string]any, then func(tx pgx.Tx, p store.Payout) error) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		return s.transitionTx(ctx, tx, id, ev, data, then)
	})
}

func (s *Service) transitionTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, ev statemachine.PayoutEvent, data map[string]any, then func(tx pgx.Tx, p store.Payout) error) error {
	q := store.New(tx)
	p, err := q.LockPayout(ctx, id)
	if err != nil {
		return err
	}
	from := statemachine.PayoutState(p.State)
	to, err := statemachine.PayoutTransition(from, ev)
	if err != nil {
		return err
	}
	if then != nil {
		if err := then(tx, p); err != nil {
			return err
		}
	}
	var failure *string
	if f, ok := data["failure"].(string); ok {
		failure = &f
	}
	if err := q.SetPayoutState(ctx, store.SetPayoutStateParams{ID: id, State: string(to), Failure: failure}); err != nil {
		return err
	}
	if data == nil {
		data = map[string]any{}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if err := q.InsertPayoutEvent(ctx, store.InsertPayoutEventParams{
		PayoutID: id, InvoiceID: p.InvoiceID, Type: string(ev), FromState: string(from), ToState: string(to), Data: raw,
	}); err != nil {
		return err
	}
	s.Log.Info("payout: "+string(to), "payout", id, "event", ev)
	return nil
}

// convert fixes the naira amount and moves the contractor's balance from
// the stablecoin to naira at that rate.
func (s *Service) convert(ctx context.Context, p store.Payout) error {
	q := store.New(s.Pool)
	if p.SourceAsset == string(money.NGN.Code) {
		// Paid by card: the money is already naira.
		return s.transition(ctx, p.ID, statemachine.EvConverted, map[string]any{"naira": p.SourceAmount.String(), "rate": "already naira"},
			func(tx pgx.Tx, _ store.Payout) error {
				return store.New(tx).SetPayoutConversion(ctx, store.SetPayoutConversionParams{ID: p.ID, NairaAmount: p.SourceAmount})
			})
	}
	inv, err := q.GetInvoiceForUpdate(ctx, p.InvoiceID) // plain read outside a transaction
	if err != nil {
		return err
	}
	if inv.RateQuoteID == nil {
		return fmt.Errorf("payout %s: naira invoice %s has no quote", p.ID, inv.ID)
	}
	quote, err := q.GetRateQuote(ctx, *inv.RateQuoteID)
	if err != nil {
		return err
	}
	rate, quoteID, why := quote.Rate, quote.ID, "locked when the invoice was created"
	first, err := q.FirstPaymentDetectedAt(ctx, inv.ID)
	if err != nil {
		return fmt.Errorf("payout %s: when the money arrived: %w", p.ID, err)
	}
	if first.After(quote.ExpiresAt) {
		// The money arrived after the locked rate expired: re-quote.
		if s.Quoter == nil {
			return fmt.Errorf("payout %s: late payment needs a new rate and no quoter is configured", p.ID)
		}
		nq, err := s.Quoter.Quote(ctx, invoice.NairaPair)
		if err != nil {
			return fmt.Errorf("payout %s: re-quote: %w", p.ID, err) // stays queued; retried
		}
		if err := q.InsertRateQuote(ctx, store.InsertRateQuoteParams{
			ID: nq.ID, FromAsset: string(nq.Pair.From), ToAsset: string(nq.Pair.To), Mid: nq.Mid, Rate: nq.Rate,
			SpreadBps: int32(nq.SpreadBps), Source: nq.Source, ObservedAt: nq.ObservedAt, QuotedAt: nq.QuotedAt, ExpiresAt: nq.ExpiresAt, //nolint:gosec // 0 to 9999
		}); err != nil {
			return err
		}
		rate, quoteID, why = nq.Rate, nq.ID, "re-quoted: the payment arrived after the locked rate expired"
	}
	naira := rates.Convert(p.SourceAmount, money.USDC, rate)
	if naira.Sign() <= 0 {
		return fmt.Errorf("payout %s: %s USDC converts to nothing", p.ID, p.SourceAmount)
	}
	data := map[string]any{"usdc": p.SourceAmount.String(), "naira": naira.String(), "rate": rate.String(), "rateQuoteId": quoteID, "why": why}
	return s.transition(ctx, p.ID, statemachine.EvConverted, data, func(tx pgx.Tx, _ store.Payout) error {
		if err := store.New(tx).SetPayoutConversion(ctx, store.SetPayoutConversionParams{ID: p.ID, NairaAmount: naira, RateQuoteID: &quoteID}); err != nil {
			return err
		}
		return postConversion(ctx, tx, p, naira)
	})
}

// postConversion: the contractor is owed naira instead of stablecoin. The
// USDC stays in the vault against an FX position; the naira is funded from
// the Paystack balance (pre-funded in test mode). Settling the position is
// treasury work, outside this stage (RISKS.md).
func postConversion(ctx context.Context, tx pgx.Tx, p store.Payout, naira money.Amount) error {
	usdc, ngn := money.USDC.Code, money.NGN.Code
	acct := func(code string, asset money.AssetCode, kind ledger.AccountKind, rule ledger.BalanceRule) (ledger.Account, error) {
		return ledger.EnsureAccount(ctx, tx, ledger.Account{Code: code, Asset: asset, Kind: kind, Rule: rule})
	}
	owedUSDC, err := acct(fmt.Sprintf("liability:contractor:%s:USDC:available", p.ContractorID), usdc, ledger.KindLiability, ledger.NonPositive)
	if err != nil {
		return err
	}
	fx, err := acct("equity:fx-position:USDC", usdc, ledger.KindEquity, ledger.AnyBalance)
	if err != nil {
		return err
	}
	float, err := acct("custody:paystack:NGN", ngn, ledger.KindAsset, ledger.NonNegative)
	if err != nil {
		return err
	}
	owedNGN, err := acct(fmt.Sprintf("liability:contractor:%s:NGN:available", p.ContractorID), ngn, ledger.KindLiability, ledger.NonPositive)
	if err != nil {
		return err
	}
	_, err = ledger.Post(ctx, tx, ledger.Entry{
		IdempotencyKey: "payout:" + p.ID.String() + ":converted",
		Kind:           "payout.converted",
		RefType:        "payout",
		RefID:          p.ID.String(),
		Postings: []ledger.Posting{
			{Account: owedUSDC.ID, Asset: usdc, Amount: p.SourceAmount},
			{Account: fx.ID, Asset: usdc, Amount: p.SourceAmount.Neg()},
			{Account: float.ID, Asset: ngn, Amount: naira},
			{Account: owedNGN.ID, Asset: ngn, Amount: naira.Neg()},
		},
	})
	return err
}

// submit records submitting, then calls the provider. The order matters: a
// crash after the call leaves submitting on record, which Step reconciles.
func (s *Service) submit(ctx context.Context, p store.Payout) error {
	q := store.New(s.Pool)
	acct, err := q.GetBankAccount(ctx, p.ContractorID)
	if err != nil {
		return fmt.Errorf("payout %s: bank account: %w", p.ID, err)
	}
	if s.now().Before(acct.PayoutsAllowedFrom) {
		return nil // the account changed recently: wait (RISKS §2)
	}
	if s.Transfers == nil {
		return nil // no provider configured (no Paystack key): converted payouts wait
	}
	recipient := ""
	if acct.RecipientCode != nil {
		recipient = *acct.RecipientCode
	} else {
		rc, err := s.Transfers.CreateRecipient(ctx, acct.AccountName, bankverify.Account{BankCode: acct.BankCode, AccountNumber: acct.AccountNumber})
		switch {
		case errors.Is(err, paystack.ErrRejected):
			// The provider will not pay this account: fail without sending.
			return s.fail(ctx, p.ID, "the bank account was refused: "+err.Error())
		case err != nil:
			return err // could not find out: try again later
		}
		if err := q.SetBankRecipient(ctx, store.SetBankRecipientParams{ContractorID: p.ContractorID, RecipientCode: &rc}); err != nil {
			return err
		}
		recipient = rc
	}
	if err := s.transition(ctx, p.ID, statemachine.EvSubmit, map[string]any{"reference": p.Reference}, nil); err != nil {
		return err
	}
	st, err := s.Transfers.Initiate(ctx, paystack.Transfer{
		Reference: p.Reference, Amount: p.NairaAmount, Recipient: recipient, Reason: "Waybill invoice payout",
	})
	switch {
	case errors.Is(err, paystack.ErrRejected):
		return s.transition(ctx, p.ID, statemachine.EvProviderRejected, map[string]any{"failure": err.Error()}, nil)
	case err != nil:
		return s.transition(ctx, p.ID, statemachine.EvProviderUnknown, map[string]any{"why": err.Error()}, nil)
	}
	if err := s.accepted(ctx, p.ID, statemachine.EvProviderAccepted, st); err != nil {
		return err
	}
	return s.settleStatus(ctx, p.ID, st)
}

// fail moves a converted payout to failed without sending anything: it
// goes through submitting so the state machine's rules hold.
func (s *Service) fail(ctx context.Context, id uuid.UUID, reason string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := s.transitionTx(ctx, tx, id, statemachine.EvSubmit, map[string]any{"why": "checking the account"}, nil); err != nil {
			return err
		}
		return s.transitionTx(ctx, tx, id, statemachine.EvProviderRejected, map[string]any{"failure": reason}, nil)
	})
}

func (s *Service) accepted(ctx context.Context, id uuid.UUID, ev statemachine.PayoutEvent, st paystack.TransferStatus) error {
	return s.transition(ctx, id, ev, map[string]any{"status": st.Status, "transferCode": st.TransferCode}, func(tx pgx.Tx, _ store.Payout) error {
		if st.TransferCode == "" {
			return nil
		}
		return store.New(tx).SetPayoutTransferCode(ctx, store.SetPayoutTransferCodeParams{ID: id, TransferCode: &st.TransferCode})
	})
}

// reconcile asks the provider about an unknown payout by reference.
func (s *Service) reconcile(ctx context.Context, id uuid.UUID) error {
	if s.Transfers == nil {
		return nil
	}
	p, err := store.New(s.Pool).LockPayout(ctx, id)
	if err != nil {
		return err
	}
	st, err := s.Transfers.Verify(ctx, p.Reference)
	switch {
	case errors.Is(err, paystack.ErrNotFound):
		// Never reached the provider: safe to submit again, with the same
		// reference.
		return s.transition(ctx, id, statemachine.EvReconciledNotFound, map[string]any{"why": "the provider has no transfer with this reference"}, nil)
	case err != nil:
		return err // still unknown (I12)
	}
	if err := s.accepted(ctx, id, statemachine.EvReconciledAccepted, st); err != nil {
		return err
	}
	return s.settleStatus(ctx, id, st)
}

// follow asks the provider how a submitted transfer is doing.
func (s *Service) follow(ctx context.Context, p store.Payout) error {
	if s.Transfers == nil {
		return nil
	}
	st, err := s.Transfers.Verify(ctx, p.Reference)
	if err != nil {
		return err // no answer: it stays submitted
	}
	return s.settleStatus(ctx, p.ID, st)
}

// settleStatus applies a final provider status to a submitted payout.
func (s *Service) settleStatus(ctx context.Context, id uuid.UUID, st paystack.TransferStatus) error {
	switch st.Status {
	case "success":
		return s.transition(ctx, id, statemachine.EvProviderSucceeded, map[string]any{"status": st.Status}, func(tx pgx.Tx, p store.Payout) error {
			return postSent(ctx, tx, p)
		})
	case "failed", "reversed", "abandoned", "rejected":
		return s.transition(ctx, id, statemachine.EvProviderFailed, map[string]any{"failure": "the provider reports the transfer as " + st.Status}, nil)
	case "otp":
		s.Log.Warn("payout: transfer waits for an OTP; turn off \"Confirm transfers before sending\" in Paystack", "payout", id)
	}
	return nil // pending, queued, processing, otp: still on its way
}

// postSent: the naira left the Paystack balance and reached the contractor.
func postSent(ctx context.Context, tx pgx.Tx, p store.Payout) error {
	ngn := money.NGN.Code
	float, err := ledger.EnsureAccount(ctx, tx, ledger.Account{Code: "custody:paystack:NGN", Asset: ngn, Kind: ledger.KindAsset, Rule: ledger.NonNegative})
	if err != nil {
		return err
	}
	owed, err := ledger.EnsureAccount(ctx, tx, ledger.Account{
		Code: fmt.Sprintf("liability:contractor:%s:NGN:available", p.ContractorID), Asset: ngn, Kind: ledger.KindLiability, Rule: ledger.NonPositive,
	})
	if err != nil {
		return err
	}
	_, err = ledger.Post(ctx, tx, ledger.Entry{
		IdempotencyKey: "payout:" + p.ID.String() + ":sent",
		Kind:           "payout.sent",
		RefType:        "payout",
		RefID:          p.ID.String(),
		Postings: []ledger.Posting{
			{Account: owed.ID, Asset: ngn, Amount: p.NairaAmount},
			{Account: float.ID, Asset: ngn, Amount: p.NairaAmount.Neg()},
		},
	})
	return err
}

// checkReversal handles a reversal callback for a succeeded payout: if the
// provider confirms it, the payout is reversed and the payout.sent entry is
// reversed, so the contractor is owed the naira again.
func (s *Service) checkReversal(ctx context.Context, p store.Payout) error {
	if s.Transfers == nil {
		return nil
	}
	st, err := s.Transfers.Verify(ctx, p.Reference)
	if err != nil {
		return err
	}
	if st.Status != "reversed" {
		return nil
	}
	return s.transition(ctx, p.ID, statemachine.EvProviderReversed, map[string]any{"status": st.Status}, func(tx pgx.Tx, _ store.Payout) error {
		e, err := store.New(tx).GetJournalEntryByIdempotencyKey(ctx, "payout:"+p.ID.String()+":sent")
		if err != nil {
			return err
		}
		_, err = ledger.Reverse(ctx, tx, ledger.EntryID(e.ID), "payout:"+p.ID.String()+":reversed", "the provider reversed the transfer")
		return err
	})
}
