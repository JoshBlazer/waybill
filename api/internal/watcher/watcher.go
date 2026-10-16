// Package watcher follows an EVM chain and turns token transfers into
// payments: detected, then confirmed after N blocks, then final after M
// blocks (invariant I8). Before each scan it checks that the blocks it has
// already read are still on the canonical chain; if they are not, it walks
// back to the common ancestor, marks payments in replaced blocks reorged,
// reverses their ledger entries and rescans. An RPC failure never advances
// or rewinds anything (invariant I12).
package watcher

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshBlazer/waybill/api/internal/evm"
	"github.com/JoshBlazer/waybill/api/internal/invoice"
	"github.com/JoshBlazer/waybill/api/internal/ledger"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/safety"
	"github.com/JoshBlazer/waybill/api/internal/statemachine"
	"github.com/JoshBlazer/waybill/api/internal/store"
)

// transferTopic is keccak256("Transfer(address,address,uint256)").
var transferTopic = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))

// Chain is the part of an EVM client the watcher uses. *ethclient.Client
// implements it.
type Chain interface {
	BlockNumber(ctx context.Context) (uint64, error)
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
	FilterLogs(ctx context.Context, q ethereum.FilterQuery) ([]types.Log, error)
}

// Config is one network's watching policy.
type Config struct {
	Network       safety.Network
	Token         common.Address
	Asset         money.Asset
	Confirmations uint64 // blocks (including the payment's) before "confirmed"
	// Finality: when FinalizedTag is set, a payment is final once its block
	// is at or below the chain's "finalized" block (real networks). Otherwise
	// it is final at Finality blocks deep (Anvil, which has no consensus).
	FinalizedTag bool
	Finality     uint64
	BatchBlocks  uint64 // max blocks per log query
	StartBlock   uint64 // first block to scan when the network has no cursor yet
	PollInterval time.Duration
}

// keepBlocks is how far below the newest scanned block the watcher keeps
// block hashes for finding a common ancestor. It is far deeper than
// finality on every supported network.
const keepBlocks = 4096

// Watcher follows one network.
type Watcher struct {
	Pool  *pgxpool.Pool
	Chain Chain
	Cfg   Config
	Log   *slog.Logger
	Now   func() time.Time // nil means time.Now
}

func (w *Watcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// Run polls until ctx is done. Each tick is independent: a failed tick is
// logged and retried, never skipped past.
func (w *Watcher) Run(ctx context.Context) error {
	t := time.NewTicker(w.Cfg.PollInterval)
	defer t.Stop()
	for {
		if err := w.Tick(ctx); err != nil && ctx.Err() == nil {
			w.Log.Warn("watcher: tick failed; will retry", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Tick undoes any reorg of blocks already read, scans new blocks for
// payments, then advances payments that have enough confirmations.
func (w *Watcher) Tick(ctx context.Context) error {
	head, err := w.Chain.BlockNumber(ctx)
	if err != nil {
		return fmt.Errorf("head: %w", err) // unknown, not "nothing happened"
	}
	if err := w.checkReorg(ctx, head); err != nil {
		return fmt.Errorf("reorg check: %w", err)
	}
	if err := w.scan(ctx, head); err != nil {
		return fmt.Errorf("scan: %w", err)
	}
	if err := w.advance(ctx, head); err != nil {
		return fmt.Errorf("advance: %w", err)
	}
	return nil
}

func (w *Watcher) scan(ctx context.Context, head uint64) error {
	q := store.New(w.Pool)
	from := w.Cfg.StartBlock // first run: begin where the contracts were deployed
	next, err := q.GetChainCursor(ctx, string(w.Cfg.Network))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return err
	default:
		from = uint64(next) //nolint:gosec // cursors are only ever written from uint64 block numbers
	}
	for from <= head {
		to := min(head, from+w.Cfg.BatchBlocks-1)
		// Read the range's last block before its logs. If a reorg lands
		// between the two calls, the stored hash is the old one and the next
		// tick rewinds and reads the range again.
		toHash, err := w.blockHash(ctx, to)
		if err != nil {
			return err
		}
		if err := w.scanRange(ctx, from, to); err != nil {
			return err // cursor stays at `from`
		}
		if err := w.recordScanned(ctx, to, toHash); err != nil {
			return err
		}
		from = to + 1
	}
	return nil
}

// recordScanned stores the range's last block hash and moves the cursor
// past it, in one transaction.
func (w *Watcher) recordScanned(ctx context.Context, to uint64, hash string) error {
	net := string(w.Cfg.Network)
	height := int64(to) //nolint:gosec // block numbers are far below 2^63
	return pgx.BeginFunc(ctx, w.Pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if err := q.PutChainBlock(ctx, store.PutChainBlockParams{Network: net, Height: height, Hash: hash}); err != nil {
			return err
		}
		if height > keepBlocks {
			if err := q.PruneChainBlocks(ctx, store.PruneChainBlocksParams{Network: net, Height: height - keepBlocks}); err != nil {
				return err
			}
		}
		return q.SetChainCursor(ctx, store.SetChainCursorParams{Network: net, NextBlock: height + 1})
	})
}

func (w *Watcher) scanRange(ctx context.Context, from, to uint64) error {
	logs, err := w.Chain.FilterLogs(ctx, ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(from),
		ToBlock:   new(big.Int).SetUint64(to),
		Addresses: []common.Address{w.Cfg.Token},
		Topics:    [][]common.Hash{{transferTopic}},
	})
	if err != nil {
		return err
	}
	if len(logs) == 0 {
		return nil
	}
	watched, err := w.watchedAddresses(ctx)
	if err != nil {
		return err
	}
	for _, l := range logs {
		if l.Removed || len(l.Topics) != 3 || len(l.Data) != 32 {
			continue
		}
		to := common.BytesToAddress(l.Topics[2].Bytes())
		invoiceID, ok := watched[evm.NormalizeAddress(to)]
		if !ok {
			continue
		}
		if err := w.recordPayment(ctx, l, invoiceID); err != nil {
			return err
		}
	}
	return nil
}

func (w *Watcher) watchedAddresses(ctx context.Context) (map[string]uuid.UUID, error) {
	rows, err := store.New(w.Pool).ListWatchedAddresses(ctx, string(w.Cfg.Network))
	if err != nil {
		return nil, err
	}
	out := make(map[string]uuid.UUID, len(rows))
	for _, r := range rows {
		out[r.Address] = r.InvoiceID
	}
	return out, nil
}

// recordPayment stores a newly seen transfer and moves the invoice to
// received, in one transaction. Seeing the same log again does nothing.
func (w *Watcher) recordPayment(ctx context.Context, l types.Log, invoiceID uuid.UUID) error {
	amount, err := money.FromBig(new(big.Int).SetBytes(l.Data))
	if err != nil || amount.Sign() <= 0 {
		return nil // zero-value or absurd transfer: nothing to credit
	}
	return pgx.BeginFunc(ctx, w.Pool, func(tx pgx.Tx) error {
		p, err := store.New(tx).InsertPayment(ctx, store.InsertPaymentParams{
			Network:      string(w.Cfg.Network),
			TxHash:       lowerHex(l.TxHash.Hex()),
			LogIndex:     int32(l.Index),       //nolint:gosec // log index within a block is small
			BlockNumber:  int64(l.BlockNumber), //nolint:gosec // block numbers are far below 2^63
			BlockHash:    lowerHex(l.BlockHash.Hex()),
			TokenAddress: evm.NormalizeAddress(l.Address),
			FromAddress:  evm.NormalizeAddress(common.BytesToAddress(l.Topics[1].Bytes())),
			ToAddress:    evm.NormalizeAddress(common.BytesToAddress(l.Topics[2].Bytes())),
			InvoiceID:    invoiceID,
			AssetCode:    string(w.Cfg.Asset.Code),
			Amount:       amount,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // already recorded (I9)
		}
		if err != nil {
			return err
		}
		_, err = invoice.Apply(ctx, tx, invoiceID, statemachine.EvPaymentDetected, map[string]any{
			"paymentId": p.ID, "txHash": p.TxHash, "amount": amount.String(),
		})
		if errors.Is(err, statemachine.ErrIllegalTransition) {
			// For example, money arriving after confirmation. The payment is
			// recorded; resolving it is stage 2 (over- and late payments).
			w.Log.Warn("watcher: payment recorded but invoice state unchanged", "invoice", invoiceID, "err", err)
			return nil
		}
		return err
	})
}

// advance confirms and finalizes open payments that are deep enough.
func (w *Watcher) advance(ctx context.Context, head uint64) error {
	open, err := store.New(w.Pool).ListOpenPayments(ctx, string(w.Cfg.Network))
	if err != nil {
		return err
	}
	if len(open) == 0 {
		return nil
	}
	finalized := uint64(0)
	haveFinalized := false
	if w.Cfg.FinalizedTag {
		h, err := w.Chain.HeaderByNumber(ctx, big.NewInt(int64(rpc.FinalizedBlockNumber)))
		if err == nil {
			finalized, haveFinalized = h.Number.Uint64(), true
		} else {
			// Unknown finality is not finality: confirm as usual, finalise nothing.
			w.Log.Warn("watcher: could not read finalized block", "err", err)
		}
	}
	isFinal := func(block, depth uint64) bool {
		if w.Cfg.FinalizedTag {
			return haveFinalized && block <= finalized
		}
		return depth >= w.Cfg.Finality
	}
	for _, p := range open {
		depth := head - uint64(p.BlockNumber) + 1 //nolint:gosec // block numbers are non-negative
		switch {
		case p.State == "detected" && depth >= w.Cfg.Confirmations:
			if err := w.stillCanonical(ctx, p); err != nil {
				w.Log.Warn("watcher: not confirming", "payment", p.ID, "err", err)
				continue
			}
			if err := pgx.BeginFunc(ctx, w.Pool, func(tx pgx.Tx) error { return w.confirm(ctx, tx, p) }); err != nil {
				return fmt.Errorf("confirm %s: %w", p.ID, err)
			}
		case p.State == "confirmed" && isFinal(uint64(p.BlockNumber), depth): //nolint:gosec // non-negative
			if err := w.stillCanonical(ctx, p); err != nil {
				w.Log.Warn("watcher: not finalizing", "payment", p.ID, "err", err)
				continue
			}
			if err := pgx.BeginFunc(ctx, w.Pool, func(tx pgx.Tx) error { return w.finalize(ctx, tx, p) }); err != nil {
				return fmt.Errorf("finalize %s: %w", p.ID, err)
			}
		}
	}
	return nil
}

var errBlockReplaced = errors.New("block at the payment's height has a different hash (reorg); the next tick rewinds")

func (w *Watcher) stillCanonical(ctx context.Context, p store.Payment) error {
	h, err := w.blockHash(ctx, uint64(p.BlockNumber)) //nolint:gosec // block numbers are non-negative
	if err != nil {
		return fmt.Errorf("could not check block: %w", err) // unknown: do nothing
	}
	if h != p.BlockHash {
		return errBlockReplaced
	}
	return nil
}

func (w *Watcher) blockHash(ctx context.Context, height uint64) (string, error) {
	h, err := w.Chain.HeaderByNumber(ctx, new(big.Int).SetUint64(height))
	if err != nil {
		return "", fmt.Errorf("header %d: %w", height, err)
	}
	return lowerHex(h.Hash().Hex()), nil
}

// errNodeBehind: the node's head is below a block already scanned. Chains
// replace blocks rather than shrink, so this is a lagging or misconfigured
// node, and the safe answer is to wait.
var errNodeBehind = errors.New("node head is below blocks already scanned")

// checkReorg compares the newest scanned block and the blocks of open
// payments with the node's chain. On a mismatch it finds the common ancestor
// and rewinds to it. Any RPC error leaves everything as it was.
func (w *Watcher) checkReorg(ctx context.Context, head uint64) error {
	q := store.New(w.Pool)
	net := string(w.Cfg.Network)
	tip, err := q.GetChainTip(ctx, net)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // nothing scanned yet
	}
	if err != nil {
		return err
	}
	if uint64(tip.Height) > head { //nolint:gosec // non-negative
		return fmt.Errorf("%w: head %d, scanned to %d", errNodeBehind, head, tip.Height)
	}
	open, err := q.ListOpenPayments(ctx, net)
	if err != nil {
		return err
	}
	type seen struct {
		height uint64
		hash   string
	}
	checks := []seen{{uint64(tip.Height), tip.Hash}} //nolint:gosec // non-negative
	for _, p := range open {
		checks = append(checks, seen{uint64(p.BlockNumber), p.BlockHash}) //nolint:gosec // non-negative
	}
	// Lowest first, so the rewind starts at the deepest replaced block.
	slices.SortFunc(checks, func(a, b seen) int { return cmp.Compare(a.height, b.height) })
	for _, c := range checks {
		got, err := w.blockHash(ctx, c.height)
		if err != nil {
			return err
		}
		if got != c.hash {
			next, err := w.ancestorNext(ctx, c.height)
			if err != nil {
				return err
			}
			return w.rewind(ctx, next, c.height)
		}
	}
	return nil
}

// ancestorNext walks the stored blocks below the replaced height, newest
// first, and returns the block after the first one that still matches. With
// no match (a reorg deeper than the stored history) it rescans from the
// deployment's start block: slow, but rescanning is always safe.
func (w *Watcher) ancestorNext(ctx context.Context, replaced uint64) (uint64, error) {
	below, err := store.New(w.Pool).ListChainBlocksBelow(ctx, store.ListChainBlocksBelowParams{
		Network: string(w.Cfg.Network), Height: int64(replaced), //nolint:gosec // block numbers are far below 2^63
	})
	if err != nil {
		return 0, err
	}
	for _, b := range below {
		got, err := w.blockHash(ctx, uint64(b.Height)) //nolint:gosec // non-negative
		if err != nil {
			return 0, err
		}
		if got == b.Hash {
			return uint64(b.Height) + 1, nil //nolint:gosec // non-negative
		}
	}
	w.Log.Error("watcher: no stored block matches the chain; rescanning from the start block", "network", w.Cfg.Network, "replaced", replaced)
	return w.Cfg.StartBlock, nil
}

// rewind undoes everything read from blocks at or above next that are no
// longer canonical, in one transaction: payments in replaced blocks become
// reorged, confirmed ones have their ledger entry reversed, their invoices
// re-derive their state from the payments that remain, and the cursor moves
// back so the next scan reads those blocks again. Final payments are never
// touched: a reorg below finality is an alert, not a correction.
func (w *Watcher) rewind(ctx context.Context, next, replaced uint64) error {
	net := string(w.Cfg.Network)
	open, err := store.New(w.Pool).ListOpenPayments(ctx, net)
	if err != nil {
		return err
	}
	// Every RPC call happens before the transaction: an error here changes
	// nothing.
	var gone []store.Payment
	for _, p := range open {
		if uint64(p.BlockNumber) < next { //nolint:gosec // non-negative
			continue
		}
		h, err := w.blockHash(ctx, uint64(p.BlockNumber)) //nolint:gosec // non-negative
		if err != nil {
			return err
		}
		if h != p.BlockHash {
			gone = append(gone, p)
		}
	}
	err = pgx.BeginFunc(ctx, w.Pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		byInvoice := map[uuid.UUID][]uuid.UUID{}
		var order []uuid.UUID
		for _, p := range gone {
			n, err := q.MarkPaymentReorged(ctx, p.ID)
			if err != nil {
				return err
			}
			if n == 0 {
				continue // moved on since it was listed; nothing to undo
			}
			if p.State == "confirmed" {
				if err := reverseConfirmation(ctx, tx, p); err != nil {
					return err
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
		height := int64(next) //nolint:gosec // block numbers are far below 2^63
		if err := q.DeleteChainBlocksFrom(ctx, store.DeleteChainBlocksFromParams{Network: net, Height: height}); err != nil {
			return err
		}
		return q.SetChainCursor(ctx, store.SetChainCursorParams{Network: net, NextBlock: height})
	})
	if err != nil {
		return err
	}
	w.Log.Warn("watcher: reorg; rewound", "network", w.Cfg.Network, "replaced", replaced, "rescanFrom", next, "paymentsReorged", len(gone))
	return nil
}

// reverseConfirmation reverses a payment's deposit.confirmed entry: custody
// and the contractor's pending balance both go back down.
func reverseConfirmation(ctx context.Context, tx pgx.Tx, p store.Payment) error {
	e, err := store.New(tx).GetJournalEntryByIdempotencyKey(ctx, "payment:"+p.ID.String()+":confirmed")
	if err != nil {
		return fmt.Errorf("find confirmation of %s: %w", p.ID, err)
	}
	_, err = ledger.Reverse(ctx, tx, ledger.EntryID(e.ID), "payment:"+p.ID.String()+":reorged",
		"block "+p.BlockHash+" left the canonical chain")
	if err != nil {
		return fmt.Errorf("ledger: %w", err)
	}
	return nil
}

// rederive returns the invoice to open, then replays detection and
// confirmation for the payments that are still there, so its state comes
// from what is on the chain rather than from guessing which payment went.
// An invoice past its expiry with nothing left is expired again.
func (w *Watcher) rederive(ctx context.Context, tx pgx.Tx, id uuid.UUID, reorged []uuid.UUID) error {
	st, err := invoice.Apply(ctx, tx, id, statemachine.EvPaymentReorged, map[string]any{"paymentIds": reorged})
	if errors.Is(err, statemachine.ErrIllegalTransition) {
		// Settled, or another state a reorg cannot reach: finality was
		// violated or the invoice was closed. Leave it for a person.
		w.Log.Error("watcher: reorg reached an invoice that cannot reopen", "invoice", id, "state", st, "payments", reorged)
		return nil
	}
	if err != nil {
		return err
	}
	replay := func(ev statemachine.InvoiceEvent, data map[string]any) error {
		data["replay"] = true
		next, err := invoice.Apply(ctx, tx, id, ev, data)
		if errors.Is(err, statemachine.ErrIllegalTransition) {
			w.Log.Warn("watcher: replay after reorg stopped", "invoice", id, "event", ev, "err", err)
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
	live, err := q.ListLivePaymentsByInvoice(ctx, id)
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
	total, err := q.SumPaymentsByState(ctx, store.SumPaymentsByStateParams{InvoiceID: id, States: []string{"confirmed", "final"}})
	if err != nil {
		return err
	}
	if total.IsZero() {
		return nil
	}
	if err := replay(statemachine.ClassifyConfirmation(total, inv.Amount), map[string]any{"confirmedTotal": total.String()}); err != nil {
		return err
	}
	allFinal := !slices.ContainsFunc(live, func(p store.Payment) bool { return p.State != "final" })
	if allFinal && (st == statemachine.InvoiceConfirmed || st == statemachine.InvoiceOverpaid) {
		return replay(statemachine.EvPaymentFinal, map[string]any{})
	}
	return nil
}

// confirm marks a payment confirmed, credits the contractor's pending
// balance and moves the invoice on.
func (w *Watcher) confirm(ctx context.Context, tx pgx.Tx, p store.Payment) error {
	q := store.New(tx)
	if err := q.MarkPaymentConfirmed(ctx, p.ID); err != nil {
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
		IdempotencyKey: "payment:" + p.ID.String() + ":confirmed",
		Kind:           "deposit.confirmed",
		RefType:        "payment",
		RefID:          p.ID.String(),
		Postings: []ledger.Posting{
			{Account: custody.ID, Asset: w.Cfg.Asset.Code, Amount: p.Amount},
			{Account: pending.ID, Asset: w.Cfg.Asset.Code, Amount: p.Amount.Neg()},
		},
	}); err != nil {
		return fmt.Errorf("ledger: %w", err)
	}
	total, err := q.SumPaymentsByState(ctx, store.SumPaymentsByStateParams{InvoiceID: inv.ID, States: []string{"confirmed", "final"}})
	if err != nil {
		return err
	}
	ev := statemachine.ClassifyConfirmation(total, inv.Amount)
	_, err = invoice.Apply(ctx, tx, inv.ID, ev, map[string]any{"paymentId": p.ID, "confirmedTotal": total.String()})
	if errors.Is(err, statemachine.ErrIllegalTransition) {
		w.Log.Warn("watcher: payment confirmed but invoice state unchanged", "invoice", inv.ID, "err", err)
		return nil
	}
	return err
}

// finalize marks a payment final, moves its amount from the contractor's
// pending to available balance, and settles the invoice once every
// confirmed payment is final.
func (w *Watcher) finalize(ctx context.Context, tx pgx.Tx, p store.Payment) error {
	q := store.New(tx)
	if err := q.MarkPaymentFinal(ctx, p.ID); err != nil {
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
		IdempotencyKey: "payment:" + p.ID.String() + ":final",
		Kind:           "deposit.final",
		RefType:        "payment",
		RefID:          p.ID.String(),
		Postings: []ledger.Posting{
			{Account: pending.ID, Asset: w.Cfg.Asset.Code, Amount: p.Amount},
			{Account: available.ID, Asset: w.Cfg.Asset.Code, Amount: p.Amount.Neg()},
		},
	}); err != nil {
		return fmt.Errorf("ledger: %w", err)
	}
	stillConfirmed, err := q.SumPaymentsByState(ctx, store.SumPaymentsByStateParams{InvoiceID: inv.ID, States: []string{"confirmed"}})
	if err != nil {
		return err
	}
	if !stillConfirmed.IsZero() {
		return nil // other payments are not final yet
	}
	_, err = invoice.Apply(ctx, tx, inv.ID, statemachine.EvPaymentFinal, map[string]any{"paymentId": p.ID})
	if errors.Is(err, statemachine.ErrIllegalTransition) {
		w.Log.Warn("watcher: payment final but invoice state unchanged", "invoice", inv.ID, "err", err)
		return nil
	}
	return err
}

// accounts returns (creating if needed) the custody account for this
// network and the contractor's pending and available balances.
func (w *Watcher) accounts(ctx context.Context, tx pgx.Tx, contractor uuid.UUID) (custody, pending, available ledger.Account, err error) {
	asset := w.Cfg.Asset.Code
	net := strings.ReplaceAll(string(w.Cfg.Network), ":", "_")
	custody, err = ledger.EnsureAccount(ctx, tx, ledger.Account{
		Code: fmt.Sprintf("custody:deposit:%s:%s", net, asset), Asset: asset, Kind: ledger.KindAsset, Rule: ledger.NonNegative,
	})
	if err != nil {
		return
	}
	pending, err = ledger.EnsureAccount(ctx, tx, ledger.Account{
		Code: fmt.Sprintf("liability:contractor:%s:%s:pending", contractor, asset), Asset: asset, Kind: ledger.KindLiability, Rule: ledger.NonPositive,
	})
	if err != nil {
		return
	}
	available, err = ledger.EnsureAccount(ctx, tx, ledger.Account{
		Code: fmt.Sprintf("liability:contractor:%s:%s:available", contractor, asset), Asset: asset, Kind: ledger.KindLiability, Rule: ledger.NonPositive,
	})
	return
}

func lowerHex(s string) string { return strings.ToLower(s) }
