// Package btcwatch follows a Bitcoin test network and credits payments to
// BTC invoices (docs/design/stage5.md §2, ADR-045). It applies the same
// rules as the EVM watcher:
//
//   - every block is read and its hash stored; an output paying one of our
//     addresses becomes a payment (I9: the same output is recorded once);
//   - at Confirmations deep it is confirmed and booked (custody up, the
//     contractor's pending balance up) and the invoice is classified;
//   - at Finality deep it is final: pending moves to available, and the
//     invoice settles once every payment is final;
//   - a replaced block rewinds: payments in it are reorged, their
//     confirmation reversed, and their invoice re-derived from what remains.
//
// Bitcoin custody is booked as custody:xpub:{network}:BTC. Waybill cannot
// spend it; the holder of the private key can.
package btcwatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/JoshBlazer/waybill/api/internal/btc"
	"github.com/JoshBlazer/waybill/api/internal/invoice"
	"github.com/JoshBlazer/waybill/api/internal/ledger"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/safety"
	"github.com/JoshBlazer/waybill/api/internal/statemachine"
	"github.com/JoshBlazer/waybill/api/internal/store"
	"github.com/JoshBlazer/waybill/api/internal/telemetry"
)

// Config is how one network is watched.
type Config struct {
	Network       safety.Network
	Confirmations uint64 // deep enough to book; 1 means in a block
	Finality      uint64 // deep enough to settle
	// StartHeight is where the first run begins (0 on regtest; a recent
	// height on signet, which has a long history).
	StartHeight  uint64
	PollInterval time.Duration
}

// Watcher follows one Bitcoin network.
type Watcher struct {
	Pool  *pgxpool.Pool
	Chain btc.Chain
	Cfg   Config
	Log   *slog.Logger
	Now   func() time.Time
}

// keepBlocks is how many block hashes are kept for reorg detection.
const keepBlocks = 500

func (w *Watcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// Run ticks until ctx is done.
func (w *Watcher) Run(ctx context.Context) {
	t := time.NewTicker(w.Cfg.PollInterval)
	defer t.Stop()
	for {
		if err := w.Tick(ctx); err != nil && ctx.Err() == nil {
			w.Log.Warn("btcwatch: tick failed; will retry", "network", w.Cfg.Network, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick rewinds any reorg, reads new blocks, then advances open payments.
func (w *Watcher) Tick(ctx context.Context) (err error) {
	ctx, span := telemetry.Tracer().Start(ctx, "btc watcher tick", trace.WithAttributes(attribute.String("network", string(w.Cfg.Network))))
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "tick failed")
		}
		span.End()
	}()
	head, err := w.Chain.Height(ctx)
	if err != nil {
		return fmt.Errorf("height: %w", err) // unknown, never "nothing arrived"
	}
	if err := w.checkReorg(ctx, head); err != nil {
		return fmt.Errorf("reorg check: %w", err)
	}
	if err := w.scan(ctx, head); err != nil {
		return fmt.Errorf("scan: %w", err)
	}
	return w.advance(ctx, head)
}

func (w *Watcher) scan(ctx context.Context, head uint64) error {
	q := store.New(w.Pool)
	net := string(w.Cfg.Network)
	next := w.Cfg.StartHeight
	switch n, err := q.GetBTCCursor(ctx, net); {
	case err == nil:
		next = uint64(n) //nolint:gosec // written from uint64 heights
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	if next > head {
		return nil
	}
	rows, err := q.ListBTCAddresses(ctx, net)
	if err != nil {
		return err
	}
	ours := make(map[string]uuid.UUID, len(rows))
	for _, r := range rows {
		ours[r.Address] = r.InvoiceID
	}
	for h := next; h <= head; h++ {
		hash, err := w.Chain.BlockHash(ctx, h)
		if err != nil {
			return err
		}
		outs, err := w.Chain.BlockOutputs(ctx, hash)
		if err != nil {
			return err
		}
		if err := pgx.BeginFunc(ctx, w.Pool, func(tx pgx.Tx) error {
			for _, o := range outs {
				inv, ok := ours[o.Address]
				if !ok || o.Amount.Sign() <= 0 {
					continue
				}
				if err := w.record(ctx, tx, h, hash, o, inv); err != nil {
					return err
				}
			}
			q := store.New(tx)
			height := int64(h) //nolint:gosec // block heights are small
			if err := q.PutBTCBlock(ctx, store.PutBTCBlockParams{Network: net, Height: height, Hash: hash}); err != nil {
				return err
			}
			if height > keepBlocks {
				if err := q.PruneBTCBlocks(ctx, store.PruneBTCBlocksParams{Network: net, Height: height - keepBlocks}); err != nil {
					return err
				}
			}
			return q.SetBTCCursor(ctx, store.SetBTCCursorParams{Network: net, NextHeight: height + 1})
		}); err != nil {
			return fmt.Errorf("block %d: %w", h, err)
		}
	}
	return nil
}

// record stores one output paying an invoice, and tells the invoice.
func (w *Watcher) record(ctx context.Context, tx pgx.Tx, height uint64, hash string, o btc.Output, inv uuid.UUID) error {
	p, err := store.New(tx).InsertBTCPayment(ctx, store.InsertBTCPaymentParams{
		Network: string(w.Cfg.Network), Txid: strings.ToLower(o.TxID), Vout: int32(o.Vout), //nolint:gosec // output index
		BlockHeight: int64(height), BlockHash: hash, Address: o.Address, InvoiceID: inv, Amount: o.Amount, //nolint:gosec // height
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // already recorded (I9)
	}
	if err != nil {
		return err
	}
	_, err = invoice.Apply(ctx, tx, inv, statemachine.EvPaymentDetected, map[string]any{
		"paymentId": p.ID, "txid": p.Txid, "vout": p.Vout, "amount": o.Amount.String(),
	})
	if errors.Is(err, statemachine.ErrIllegalTransition) {
		// A settled or cancelled invoice cannot move; the money is still
		// recorded and credited, never dropped.
		w.Log.Warn("btcwatch: payment recorded but invoice state unchanged", "invoice", inv, "err", err)
		return nil
	}
	return err
}

// advance confirms and finalises open payments that are deep enough.
func (w *Watcher) advance(ctx context.Context, head uint64) error {
	open, err := store.New(w.Pool).ListOpenBTCPayments(ctx, string(w.Cfg.Network))
	if err != nil {
		return err
	}
	for _, p := range open {
		depth := head - uint64(p.BlockHeight) + 1 //nolint:gosec // heights are non-negative
		var step func(context.Context, pgx.Tx, store.BtcPayment) error
		switch {
		case p.State == "detected" && depth >= w.Cfg.Confirmations:
			step = w.confirm
		case p.State == "confirmed" && depth >= w.Cfg.Finality:
			step = w.finalize
		default:
			continue
		}
		if err := pgx.BeginFunc(ctx, w.Pool, func(tx pgx.Tx) error { return step(ctx, tx, p) }); err != nil {
			return fmt.Errorf("payment %s: %w", p.ID, err)
		}
	}
	return nil
}

func (w *Watcher) accounts(ctx context.Context, tx pgx.Tx, contractor uuid.UUID) (custody, pending, available ledger.Account, err error) {
	asset := money.BTC.Code
	net := strings.ReplaceAll(string(w.Cfg.Network), ":", "_")
	ensure := func(code string, kind ledger.AccountKind, rule ledger.BalanceRule) (ledger.Account, error) {
		return ledger.EnsureAccount(ctx, tx, ledger.Account{Code: code, Asset: asset, Kind: kind, Rule: rule})
	}
	if custody, err = ensure(fmt.Sprintf("custody:xpub:%s:BTC", net), ledger.KindAsset, ledger.NonNegative); err != nil {
		return
	}
	if pending, err = ensure(fmt.Sprintf("liability:contractor:%s:BTC:pending", contractor), ledger.KindLiability, ledger.NonPositive); err != nil {
		return
	}
	available, err = ensure(fmt.Sprintf("liability:contractor:%s:BTC:available", contractor), ledger.KindLiability, ledger.NonPositive)
	return
}

func (w *Watcher) confirm(ctx context.Context, tx pgx.Tx, p store.BtcPayment) error {
	q := store.New(tx)
	if err := q.MarkBTCPaymentConfirmed(ctx, p.ID); err != nil {
		return err
	}
	inv, err := q.GetInvoiceForUpdate(ctx, p.InvoiceID)
	if err != nil {
		return err
	}
	custody, pending, _, err := w.accounts(ctx, tx, inv.ContractorID)
	if err != nil {
		return err
	}
	if _, err := ledger.Post(ctx, tx, ledger.Entry{
		IdempotencyKey: "btc-payment:" + p.ID.String() + ":confirmed",
		Kind:           "deposit.confirmed",
		RefType:        "btc_payment",
		RefID:          p.ID.String(),
		Postings: []ledger.Posting{
			{Account: custody.ID, Asset: money.BTC.Code, Amount: p.Amount},
			{Account: pending.ID, Asset: money.BTC.Code, Amount: p.Amount.Neg()},
		},
	}); err != nil {
		return fmt.Errorf("ledger: %w", err)
	}
	// As for EVM: with a payment still unconfirmed the total is not known,
	// so wait for it before classifying.
	unconfirmed, err := q.SumBTCPaymentsByState(ctx, store.SumBTCPaymentsByStateParams{InvoiceID: inv.ID, States: []string{"detected"}})
	if err != nil {
		return err
	}
	if !unconfirmed.IsZero() {
		return nil
	}
	total, err := q.SumBTCPaymentsByState(ctx, store.SumBTCPaymentsByStateParams{InvoiceID: inv.ID, States: []string{"confirmed", "final"}})
	if err != nil {
		return err
	}
	_, err = invoice.Apply(ctx, tx, inv.ID, statemachine.ClassifyConfirmation(total, inv.Amount), map[string]any{"paymentId": p.ID, "confirmedTotal": total.String()})
	if errors.Is(err, statemachine.ErrIllegalTransition) {
		w.Log.Warn("btcwatch: payment confirmed but invoice state unchanged", "invoice", inv.ID, "err", err)
		return nil
	}
	return err
}

func (w *Watcher) finalize(ctx context.Context, tx pgx.Tx, p store.BtcPayment) error {
	q := store.New(tx)
	if err := q.MarkBTCPaymentFinal(ctx, p.ID); err != nil {
		return err
	}
	inv, err := q.GetInvoiceForUpdate(ctx, p.InvoiceID)
	if err != nil {
		return err
	}
	_, pending, available, err := w.accounts(ctx, tx, inv.ContractorID)
	if err != nil {
		return err
	}
	if _, err := ledger.Post(ctx, tx, ledger.Entry{
		IdempotencyKey: "btc-payment:" + p.ID.String() + ":final",
		Kind:           "deposit.final",
		RefType:        "btc_payment",
		RefID:          p.ID.String(),
		Postings: []ledger.Posting{
			{Account: pending.ID, Asset: money.BTC.Code, Amount: p.Amount},
			{Account: available.ID, Asset: money.BTC.Code, Amount: p.Amount.Neg()},
		},
	}); err != nil {
		return fmt.Errorf("ledger: %w", err)
	}
	notFinal, err := q.SumBTCPaymentsByState(ctx, store.SumBTCPaymentsByStateParams{InvoiceID: inv.ID, States: []string{"detected", "confirmed"}})
	if err != nil {
		return err
	}
	if !notFinal.IsZero() || statemachine.InvoiceState(inv.State) == statemachine.InvoiceUnderpaid {
		return nil
	}
	_, err = invoice.Apply(ctx, tx, inv.ID, statemachine.EvPaymentFinal, map[string]any{"paymentId": p.ID})
	if errors.Is(err, statemachine.ErrIllegalTransition) {
		w.Log.Warn("btcwatch: payment final but invoice state unchanged", "invoice", inv.ID, "err", err)
		return nil
	}
	return err
}

// checkReorg compares the stored tip, and every open payment's block, with
// the chain. On a mismatch it finds the last stored block still on the
// chain and rewinds to just after it.
func (w *Watcher) checkReorg(ctx context.Context, head uint64) error {
	q := store.New(w.Pool)
	net := string(w.Cfg.Network)
	tip, err := q.GetBTCTip(ctx, net)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	type seen struct {
		height uint64
		hash   string
	}
	checks := []seen{{uint64(tip.Height), tip.Hash}} //nolint:gosec // non-negative
	open, err := q.ListOpenBTCPayments(ctx, net)
	if err != nil {
		return err
	}
	for _, p := range open {
		checks = append(checks, seen{uint64(p.BlockHeight), p.BlockHash}) //nolint:gosec // non-negative
	}
	slices.SortFunc(checks, func(a, b seen) int { return int(a.height) - int(b.height) }) //nolint:gosec // small heights
	for _, c := range checks {
		if c.height > head {
			// The node is behind what we read: treat the missing blocks as
			// replaced (a reorg to a shorter chain, or a node reset).
			return w.rewind(ctx, c.height)
		}
		got, err := w.Chain.BlockHash(ctx, c.height)
		if err != nil {
			return err
		}
		if got != c.hash {
			return w.rewind(ctx, c.height)
		}
	}
	return nil
}

// rewind finds the newest stored block below replaced that is still on the
// chain and undoes everything read after it, in one transaction.
func (w *Watcher) rewind(ctx context.Context, replaced uint64) error {
	q := store.New(w.Pool)
	net := string(w.Cfg.Network)
	below, err := q.ListBTCBlocksBelow(ctx, store.ListBTCBlocksBelowParams{Network: net, Height: int64(replaced)}) //nolint:gosec // small
	if err != nil {
		return err
	}
	next := w.Cfg.StartHeight
	for _, b := range below {
		got, err := w.Chain.BlockHash(ctx, uint64(b.Height)) //nolint:gosec // non-negative
		if errors.Is(err, btc.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if got == b.Hash {
			next = uint64(b.Height) + 1 //nolint:gosec // non-negative
			break
		}
	}
	open, err := q.ListOpenBTCPayments(ctx, net)
	if err != nil {
		return err
	}
	var gone []store.BtcPayment
	for _, p := range open {
		if uint64(p.BlockHeight) >= next { //nolint:gosec // non-negative
			gone = append(gone, p)
		}
	}
	err = pgx.BeginFunc(ctx, w.Pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		byInvoice := map[uuid.UUID][]uuid.UUID{}
		var order []uuid.UUID
		for _, p := range gone {
			n, err := q.MarkBTCPaymentReorged(ctx, p.ID)
			if err != nil {
				return err
			}
			if n == 0 {
				continue
			}
			if p.State == "confirmed" {
				e, err := q.GetJournalEntryByIdempotencyKey(ctx, "btc-payment:"+p.ID.String()+":confirmed")
				if err != nil {
					return fmt.Errorf("find confirmation of %s: %w", p.ID, err)
				}
				if _, err := ledger.Reverse(ctx, tx, ledger.EntryID(e.ID), "btc-payment:"+p.ID.String()+":reorged",
					"block "+p.BlockHash+" left the best chain"); err != nil {
					return fmt.Errorf("ledger: %w", err)
				}
			}
			if _, ok := byInvoice[p.InvoiceID]; !ok {
				order = append(order, p.InvoiceID)
			}
			byInvoice[p.InvoiceID] = append(byInvoice[p.InvoiceID], p.ID)
		}
		for _, id := range order {
			if err := w.rederive(ctx, tx, id, byInvoice[id]); err != nil {
				return fmt.Errorf("invoice %s: %w", id, err)
			}
		}
		h := int64(next) //nolint:gosec // small
		if err := q.DeleteBTCBlocksFrom(ctx, store.DeleteBTCBlocksFromParams{Network: net, Height: h}); err != nil {
			return err
		}
		return q.SetBTCCursor(ctx, store.SetBTCCursorParams{Network: net, NextHeight: h})
	})
	if err != nil {
		return err
	}
	w.Log.Warn("btcwatch: reorg; rewound", "network", net, "replaced", replaced, "rescanFrom", next, "paymentsReorged", len(gone))
	return nil
}

// rederive returns the invoice to open and replays what is still on the
// chain, as the EVM watcher does.
func (w *Watcher) rederive(ctx context.Context, tx pgx.Tx, id uuid.UUID, reorged []uuid.UUID) error {
	st, err := invoice.Apply(ctx, tx, id, statemachine.EvPaymentReorged, map[string]any{"paymentIds": reorged})
	if errors.Is(err, statemachine.ErrIllegalTransition) {
		w.Log.Error("btcwatch: reorg reached an invoice that cannot reopen", "invoice", id, "state", st, "payments", reorged)
		return nil
	}
	if err != nil {
		return err
	}
	replay := func(ev statemachine.InvoiceEvent, data map[string]any) error {
		data["replay"] = true
		next, err := invoice.Apply(ctx, tx, id, ev, data)
		if errors.Is(err, statemachine.ErrIllegalTransition) {
			w.Log.Warn("btcwatch: replay after reorg stopped", "invoice", id, "event", ev, "err", err)
			return nil
		}
		st = next
		return err
	}
	q := store.New(tx)
	inv, err := q.GetInvoiceForUpdate(ctx, id)
	if err != nil {
		return err
	}
	live, err := q.ListLiveBTCPaymentsByInvoice(ctx, id)
	if err != nil {
		return err
	}
	if len(live) == 0 {
		if !w.now().Before(inv.ExpiresAt) {
			return replay(statemachine.EvExpire, map[string]any{})
		}
		return nil
	}
	if err := replay(statemachine.EvPaymentDetected, map[string]any{"paymentId": live[0].ID}); err != nil {
		return err
	}
	total, err := q.SumBTCPaymentsByState(ctx, store.SumBTCPaymentsByStateParams{InvoiceID: id, States: []string{"confirmed", "final"}})
	if err != nil || total.IsZero() {
		return err
	}
	if err := replay(statemachine.ClassifyConfirmation(total, inv.Amount), map[string]any{"confirmedTotal": total.String()}); err != nil {
		return err
	}
	allFinal := !slices.ContainsFunc(live, func(p store.BtcPayment) bool { return p.State != "final" })
	if allFinal && (st == statemachine.InvoiceConfirmed || st == statemachine.InvoiceOverpaid) {
		return replay(statemachine.EvPaymentFinal, map[string]any{})
	}
	return nil
}
