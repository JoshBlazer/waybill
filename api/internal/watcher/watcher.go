// Package watcher follows an EVM chain and turns token transfers into
// payments: detected, then confirmed after N blocks, then final after M
// blocks (invariant I8). Stage 1 version: reorgs are not yet reversed (that
// is stage 2), but a payment whose block hash has changed is never
// confirmed, and an RPC failure never advances anything (invariant I12).
package watcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
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
	Finality      uint64 // blocks before "final"
	BatchBlocks   uint64 // max blocks per log query
	PollInterval  time.Duration
}

// Watcher follows one network.
type Watcher struct {
	Pool  *pgxpool.Pool
	Chain Chain
	Cfg   Config
	Log   *slog.Logger
}

// Run polls until ctx is done. Each tick is independent: a failed tick is
// logged and retried, never skipped past.
func (w *Watcher) Run(ctx context.Context) error {
	t := time.NewTicker(w.Cfg.PollInterval)
	defer t.Stop()
	for {
		if err := w.Tick(ctx); err != nil && ctx.Err() == nil {
			w.Log.Warn("watcher: tick failed; will retry", "network", w.Cfg.Network, "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Tick scans new blocks for payments, then advances payments that have
// enough confirmations.
func (w *Watcher) Tick(ctx context.Context) error {
	head, err := w.Chain.BlockNumber(ctx)
	if err != nil {
		return fmt.Errorf("head: %w", err) // unknown, not "nothing happened"
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
	next, err := q.GetChainCursor(ctx, string(w.Cfg.Network))
	if errors.Is(err, pgx.ErrNoRows) {
		next = 0 // test chains are short; scan from genesis
	} else if err != nil {
		return err
	}
	for from := uint64(next); from <= head; {
		to := min(head, from+w.Cfg.BatchBlocks-1)
		if err := w.scanRange(ctx, from, to); err != nil {
			return err // cursor stays at `from`
		}
		if err := q.SetChainCursor(ctx, store.SetChainCursorParams{Network: string(w.Cfg.Network), NextBlock: int64(to + 1)}); err != nil { //nolint:gosec // block numbers are far below 2^63
			return err
		}
		from = to + 1
	}
	return nil
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
		case p.State == "confirmed" && depth >= w.Cfg.Finality:
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

var errBlockReplaced = errors.New("block at the payment's height has a different hash (reorg); handled in stage 2")

func (w *Watcher) stillCanonical(ctx context.Context, p store.Payment) error {
	h, err := w.Chain.HeaderByNumber(ctx, big.NewInt(p.BlockNumber))
	if err != nil {
		return fmt.Errorf("could not check block: %w", err) // unknown: do nothing
	}
	if lowerHex(h.Hash().Hex()) != p.BlockHash {
		return errBlockReplaced
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
