// Package checkout lets a payer pay a naira invoice by card or bank
// transfer on Paystack's test checkout (ADR-041).
//
// Start creates one open checkout per invoice, for exactly the naira amount
// at the invoice's locked rate, and returns Paystack's page. Reconcile asks
// Paystack about a checkout by reference; only that answer, never a
// webhook body alone, marks a checkout paid. When it is, one transaction
// records the money held at Paystack against what Waybill owes the
// contractor, and moves the invoice through received, confirmed and
// settled: a successful Paystack charge is final.
package checkout

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

	"github.com/JoshBlazer/waybill/api/internal/invoice"
	"github.com/JoshBlazer/waybill/api/internal/ledger"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/paystack"
	"github.com/JoshBlazer/waybill/api/internal/safety"
	"github.com/JoshBlazer/waybill/api/internal/statemachine"
	"github.com/JoshBlazer/waybill/api/internal/store"
)

// Charger is the part of the Paystack client checkout uses.
type Charger interface {
	InitializeTransaction(ctx context.Context, ch paystack.Charge) (string, error)
	VerifyTransaction(ctx context.Context, ref string) (paystack.ChargeStatus, error)
}

// Errors returned to API callers.
var (
	ErrNotFound    = errors.New("checkout: no such payment link")
	ErrNotPayable  = errors.New("checkout: this link cannot be paid by card or bank")
	ErrUnavailable = errors.New("checkout: Paystack is not available")
)

// TTL is how long a checkout stays open before a new one is needed.
const TTL = 30 * time.Minute

// Service starts and reconciles checkouts.
type Service struct {
	Pool    *pgxpool.Pool
	Charger Charger // nil when no Paystack key is configured
	Tokens  map[safety.Network]string
	WebURL  string
	Log     *slog.Logger
	Now     func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Payable reports whether inv can be paid by card or bank transfer now:
// nil, ErrNotPayable (with the reason) or ErrUnavailable.
func (s *Service) Payable(inv invoice.Invoice) error {
	switch {
	case inv.Quote == nil:
		return fmt.Errorf("%w: it is paid out in stablecoin; pay in stablecoin", ErrNotPayable)
	case statemachine.InvoiceState(inv.State) != statemachine.InvoiceOpen:
		return fmt.Errorf("%w: it is %s", ErrNotPayable, inv.State)
	case !s.now().Before(inv.Quote.ExpiresAt):
		return fmt.Errorf("%w: its naira rate expired; ask the contractor for a new link", ErrNotPayable)
	case s.Charger == nil:
		return ErrUnavailable
	}
	return nil
}

// Start returns the invoice's open checkout, or creates one. The invoice
// must be open, pay out in naira, and have an unexpired quote.
func (s *Service) Start(ctx context.Context, code string) (store.Checkout, invoice.Invoice, error) {
	inv, err := invoice.Load(ctx, s.Pool, s.Tokens, code)
	if errors.Is(err, invoice.ErrNotFound) {
		return store.Checkout{}, invoice.Invoice{}, ErrNotFound
	}
	if err != nil {
		return store.Checkout{}, invoice.Invoice{}, err
	}
	now := s.now()
	if err := s.Payable(inv); err != nil {
		return store.Checkout{}, inv, err
	}
	q := store.New(s.Pool)
	if open, err := q.GetOpenCheckout(ctx, inv.ID); err == nil && open.AuthorizationUrl != nil && now.Before(open.ExpiresAt) {
		return open, inv, nil
	} else if err == nil {
		// Expired or never started: close it so a fresh one can open.
		if err := q.CloseCheckout(ctx, store.CloseCheckoutParams{ID: open.ID, State: "expired", Failure: ptr("replaced by a new checkout")}); err != nil {
			return store.Checkout{}, inv, err
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return store.Checkout{}, inv, err
	}

	id := uuid.New()
	row, err := q.InsertCheckout(ctx, store.InsertCheckoutParams{
		ID: id, InvoiceID: inv.ID, Reference: "chk_" + strings.ReplaceAll(id.String(), "-", ""),
		Amount: inv.NairaAmount(), ExpiresAt: now.Add(TTL),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return s.Start(ctx, code) // another request opened one first
	}
	if err != nil {
		return store.Checkout{}, inv, err
	}
	url, err := s.Charger.InitializeTransaction(ctx, paystack.Charge{
		Reference: row.Reference, Amount: row.Amount,
		// Payers have no account, and Paystack requires an email. Test mode
		// accepts any address; this one identifies the link.
		Email:       "pay-" + strings.ToLower(inv.TrackingCode) + "@example.com",
		CallbackURL: s.WebURL + "/t/" + inv.TrackingCode,
	})
	if err != nil {
		reason := err.Error()
		if cerr := q.CloseCheckout(ctx, store.CloseCheckoutParams{ID: row.ID, State: "failed", Failure: &reason}); cerr != nil {
			return store.Checkout{}, inv, cerr
		}
		return store.Checkout{}, inv, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	row, err = q.SetCheckoutURL(ctx, store.SetCheckoutURLParams{ID: row.ID, AuthorizationUrl: &url})
	return row, inv, err
}

// Reconcile asks Paystack about the checkout with this reference and
// records the answer. Safe to call any number of times.
func (s *Service) Reconcile(ctx context.Context, ref string) error {
	q := store.New(s.Pool)
	c, err := q.GetCheckoutByReference(ctx, ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // not ours (for example another app on the same Paystack account)
	}
	if err != nil || c.State != "open" {
		return err
	}
	if s.Charger == nil {
		return ErrUnavailable
	}
	st, err := s.Charger.VerifyTransaction(ctx, ref)
	switch {
	case errors.Is(err, paystack.ErrNotFound):
		return s.expireIfDue(ctx, c) // the payer never reached Paystack
	case err != nil:
		return fmt.Errorf("checkout: verify %s: %w", ref, err) // unknown: try again later (I12)
	}
	switch st.Status {
	case "success":
		return s.recordPaid(ctx, c, st)
	case "failed", "reversed":
		reason := "Paystack: " + st.Status
		return q.CloseCheckout(ctx, store.CloseCheckoutParams{ID: c.ID, State: "failed", Failure: &reason})
	default: // abandoned, ongoing, pending, …: the payer may still pay
		return s.expireIfDue(ctx, c)
	}
}

func (s *Service) expireIfDue(ctx context.Context, c store.Checkout) error {
	if s.now().Before(c.ExpiresAt) {
		return nil
	}
	return store.New(s.Pool).CloseCheckout(ctx, store.CloseCheckoutParams{ID: c.ID, State: "expired", Failure: ptr("not paid within the checkout window")})
}

// recordPaid marks the checkout paid and, in the same transaction, posts
// the ledger entry and moves the invoice to settled.
func (s *Service) recordPaid(ctx context.Context, c store.Checkout, st paystack.ChargeStatus) error {
	if st.Currency != "" && st.Currency != string(money.NGN.Code) {
		return fmt.Errorf("checkout: %s was paid in %s, not NGN", c.Reference, st.Currency)
	}
	if st.Amount.Sign() <= 0 {
		return fmt.Errorf("checkout: %s reported as paid with amount %s", c.Reference, st.Amount)
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		locked, err := q.LockCheckout(ctx, c.ID)
		if err != nil || locked.State != "open" {
			return err // another reconcile got here first
		}
		inv, err := q.GetInvoiceForUpdate(ctx, c.InvoiceID)
		if err != nil {
			return err
		}
		var txID *string
		if st.ID != "" {
			txID = &st.ID
		}
		if err := q.MarkCheckoutPaid(ctx, store.MarkCheckoutPaidParams{
			ID: c.ID, PaidAt: ptr(s.now()), PaidAmount: st.Amount, ProviderTransactionID: txID,
		}); err != nil {
			return err
		}
		if err := s.post(ctx, tx, c, inv.ContractorID, st.Amount); err != nil {
			return err
		}
		data := map[string]any{"checkout": c.Reference, "naira": st.Amount.String(), "channel": "paystack"}
		if _, err := invoice.Apply(ctx, tx, inv.ID, statemachine.EvPaymentDetected, data); err != nil {
			return fmt.Errorf("checkout: invoice %s: %w", inv.ID, err)
		}
		ev := statemachine.ClassifyConfirmation(st.Amount, c.Amount) // naira paid against naira due
		if _, err := invoice.Apply(ctx, tx, inv.ID, ev, data); err != nil {
			return fmt.Errorf("checkout: invoice %s: %w", inv.ID, err)
		}
		if ev == statemachine.EvConfirmedShort {
			return nil // underpaid: the contractor decides (accept-shortfall)
		}
		if _, err := invoice.Apply(ctx, tx, inv.ID, statemachine.EvPaymentFinal, data); err != nil {
			return fmt.Errorf("checkout: invoice %s: %w", inv.ID, err)
		}
		s.Log.Info("checkout: paid", "reference", c.Reference, "naira", st.Amount.String())
		return nil
	})
}

// post records naira held at Paystack against what Waybill owes the
// contractor, available at once: a successful charge is final.
func (s *Service) post(ctx context.Context, tx pgx.Tx, c store.Checkout, contractor uuid.UUID, amount money.Amount) error {
	ngn := money.NGN.Code
	held, err := ledger.EnsureAccount(ctx, tx, ledger.Account{
		Code: "custody:paystack:NGN", Asset: ngn, Kind: ledger.KindAsset, Rule: ledger.NonNegative,
	})
	if err != nil {
		return err
	}
	owed, err := ledger.EnsureAccount(ctx, tx, ledger.Account{
		Code: fmt.Sprintf("liability:contractor:%s:NGN:available", contractor), Asset: ngn, Kind: ledger.KindLiability, Rule: ledger.NonPositive,
	})
	if err != nil {
		return err
	}
	_, err = ledger.Post(ctx, tx, ledger.Entry{
		IdempotencyKey: "checkout:" + c.ID.String() + ":paid",
		Kind:           "checkout.paid",
		RefType:        "checkout",
		RefID:          c.ID.String(),
		Postings: []ledger.Posting{
			{Account: held.ID, Asset: ngn, Amount: amount},
			{Account: owed.ID, Asset: ngn, Amount: amount.Neg()},
		},
	})
	return err
}

// Run reconciles open checkouts and stored charge.success callbacks every
// interval until ctx is done.
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			s.Log.Warn("checkout: tick failed; will retry", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick reconciles stored charge.success callbacks first (each processed
// once, I14), then every open checkout.
func (s *Service) Tick(ctx context.Context) error {
	q := store.New(s.Pool)
	events, err := q.ListUnprocessedProviderEvents(ctx, store.ListUnprocessedProviderEventsParams{Provider: "paystack", EventType: "charge.success"})
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
		if body.Data.Reference != "" {
			if err := s.Reconcile(ctx, body.Data.Reference); err != nil {
				return err // stays unprocessed; retried next tick
			}
		}
		if err := q.MarkProviderEventProcessed(ctx, e.ID); err != nil {
			return err
		}
	}
	open, err := q.ListOpenCheckouts(ctx)
	if err != nil {
		return err
	}
	var firstErr error
	for _, c := range open {
		if err := s.Reconcile(ctx, c.Reference); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func ptr[T any](v T) *T { return &v }
