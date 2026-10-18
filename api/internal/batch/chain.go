package batch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/JoshBlazer/waybill/api/internal/ledger"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/outbox"
	"github.com/JoshBlazer/waybill/api/internal/safety"
	"github.com/JoshBlazer/waybill/api/internal/statemachine"
	"github.com/JoshBlazer/waybill/api/internal/store"
	"github.com/JoshBlazer/waybill/api/internal/txmgr"
)

// Topic is the outbox topic that sends a run; Purpose is the transaction
// manager purpose of its transaction.
const (
	Topic   = "batch.send"
	Purpose = "batch_payout"
)

// Job is the outbox payload for Topic.
type Job struct {
	BatchID uuid.UUID `json:"batchId"`
}

// Policy retries a run every 15 seconds for 15 minutes: the usual wait is
// for the organisation's funding to be swept into the vault.
func Policy() outbox.Policy {
	return outbox.Policy{MaxAttempts: 60, Backoff: func(int) time.Duration { return 15 * time.Second }}
}

// errVaultShort: the vault does not hold enough swept money yet for every
// run in flight. The job retries; nothing is sent.
var errVaultShort = errors.New("batch: waiting for the organisation's funding to reach the vault")

var payBatchSelector = crypto.Keccak256([]byte("payBatch(address,bytes32,(address,uint256,bytes32)[])"))[:4]

// Calldata encodes BatchPayout.payBatch(token, batchId, items). Items are
// static tuples, so the array is its length followed by three words each.
func Calldata(token common.Address, batchID [32]byte, items []Item) []byte {
	word := func(b []byte) []byte { return common.LeftPadBytes(b, 32) }
	out := make([]byte, 0, 4+32*4+len(items)*96)
	out = append(out, payBatchSelector...)
	out = append(out, word(token.Bytes())...)
	out = append(out, batchID[:]...)
	out = append(out, word(big.NewInt(96).Bytes())...) // offset of the array: after three head words
	out = append(out, word(big.NewInt(int64(len(items))).Bytes())...)
	for _, it := range items {
		out = append(out, word(it.To.Bytes())...)
		out = append(out, word(it.Amount.Bytes())...)
		out = append(out, it.PayoutID[:]...)
	}
	return out
}

// Item is one BatchPayout.Item.
type Item struct {
	To       common.Address
	Amount   *big.Int
	PayoutID [32]byte
}

// Sender sends queued runs through each network's transaction manager.
type Sender struct {
	Service  *Service
	Managers map[safety.Network]*txmgr.Manager
	// Finality per network: blocks deep before a mined run is final. Zero
	// means use the node's finalized block instead.
	Finality map[safety.Network]uint64
	Chains   map[safety.Network]ChainReader
}

// ChainReader is what finality checks need from a node.
type ChainReader interface {
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
	TransactionReceipt(ctx context.Context, hash common.Hash) (*types.Receipt, error)
}

// Handle submits a sending run's transaction. Submitting is idempotent per
// run, so a retried job never sends twice.
func (x *Sender) Handle(ctx context.Context, j outbox.Job) error {
	var job Job
	if err := json.Unmarshal(j.Payload, &job); err != nil {
		return fmt.Errorf("%w: payload: %w", outbox.ErrPermanent, err)
	}
	s := x.Service
	q := store.New(s.Pool)
	b, err := q.GetBatchForUpdate(ctx, job.BatchID) // outside a transaction: a plain read
	if err != nil {
		return fmt.Errorf("%w: %w", outbox.ErrPermanent, err)
	}
	if statemachine.BatchState(b.State) != statemachine.BatchSending {
		return nil // already past sending
	}
	net := safety.Network(b.Network)
	m, ok := x.Managers[net]
	d, okd := s.Deployments[net]
	if !ok || !okd || d.BatchPayout == (common.Address{}) {
		return fmt.Errorf("%w: no signer or BatchPayout contract for %s", outbox.ErrPermanent, net)
	}
	// The vault pays from what has been swept into it. Funding becomes
	// available at finality and is swept shortly after; until the vault
	// covers every run in flight, wait rather than send a transaction that
	// would revert.
	inVault, err := q.GetBalanceByCode(ctx, fmt.Sprintf("custody:vault:%s:%s", strings.ReplaceAll(b.Network, ":", "_"), money.USDC.Code))
	if err != nil {
		return err
	}
	inFlight, err := q.SumBatchesInFlight(ctx)
	if err != nil {
		return err
	}
	if inVault.Cmp(inFlight) < 0 {
		return errVaultShort
	}
	lines, err := q.ListBatchLines(ctx, b.ID)
	if err != nil {
		return err
	}
	items := make([]Item, len(lines))
	for i, l := range lines {
		if l.WalletAddress == nil {
			return fmt.Errorf("%w: line %d has no wallet", outbox.ErrPermanent, l.LineNo)
		}
		items[i] = Item{To: common.HexToAddress(*l.WalletAddress), Amount: l.Amount.Big(), PayoutID: [32]byte(l.PayoutID)}
	}
	row, err := m.Submit(ctx, txmgr.Request{
		Purpose: Purpose, RefID: "batch:" + b.ID.String(), To: d.BatchPayout,
		Data: Calldata(d.Token, [32]byte(b.ChainBatchID), items),
	})
	if errors.Is(err, txmgr.ErrEstimateFailed) {
		// The node says it would revert (a limit, a pause, a lost role):
		// nothing was sent. The run fails and its money returns.
		if ferr := s.fail(ctx, b.ID, err.Error()); ferr != nil {
			return ferr
		}
		return fmt.Errorf("%w: %w", outbox.ErrPermanent, err)
	}
	if err != nil {
		return err
	}
	return q.SetBatchOutboundTx(ctx, store.SetBatchOutboundTxParams{ID: b.ID, OutboundTxID: &row.ID})
}

// fail ends a sending run that never reached the chain, or reverted, and
// returns its reservation to the organisation.
func (s *Service) fail(ctx context.Context, id uuid.UUID, reason string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error { return s.failTx(ctx, tx, id, reason) })
}

func (s *Service) failTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, reason string) error {
	q := store.New(tx)
	b, err := q.GetBatchForUpdate(ctx, id)
	if err != nil {
		return err
	}
	if _, err := s.apply(ctx, q, b, statemachine.EvBatchReverted, nil, map[string]any{"failure": reason}); err != nil {
		return err
	}
	e, err := q.GetJournalEntryByIdempotencyKey(ctx, "batch:"+id.String()+":reserved")
	if err != nil {
		return fmt.Errorf("find reservation of %s: %w", id, err)
	}
	_, err = ledger.Reverse(ctx, tx, ledger.EntryID(e.ID), "batch:"+id.String()+":returned", "run failed: "+reason)
	return err
}

// Hook returns the transaction-manager hook for runs on network: a mined
// run is confirming, a reverted one failed with its money returned.
func (s *Service) Hook(network safety.Network) txmgr.Hook {
	return func(ctx context.Context, tx pgx.Tx, row store.OutboundTx, r *types.Receipt) error {
		q := store.New(tx)
		b, err := q.GetBatchByOutboundTx(ctx, &row.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			// The job recorded the transaction but not yet the link (a
			// crash between the two). Find the run by its reference.
			id, perr := uuid.Parse(row.RefID[len("batch:"):])
			if perr != nil {
				return perr
			}
			if b, err = q.GetBatchForUpdate(ctx, id); err == nil {
				err = q.SetBatchOutboundTx(ctx, store.SetBatchOutboundTxParams{ID: id, OutboundTxID: &row.ID})
			}
		}
		if err != nil {
			return err
		}
		hash := r.TxHash.Hex()
		if r.Status != types.ReceiptStatusSuccessful {
			return s.failTx(ctx, tx, b.ID, "the transaction "+hash+" reverted")
		}
		_, err = s.apply(ctx, q, b, statemachine.EvBatchMined, nil, map[string]any{
			"txHash": hash, "block": r.BlockNumber.String(), "network": string(network),
		})
		return err
	}
}

// Tick makes confirming runs final once their block is final, and records
// the money leaving the vault.
func (x *Sender) Tick(ctx context.Context) error {
	s := x.Service
	q := store.New(s.Pool)
	runs, err := q.ListBatchesInState(ctx, string(statemachine.BatchConfirming))
	if err != nil {
		return err
	}
	for _, b := range runs {
		if err := x.finalize(ctx, b); err != nil {
			s.log().Warn("batch: not final yet", "batch", b.ID, "err", err)
		}
	}
	return nil
}

var errNotFinal = errors.New("block not final yet")

func (x *Sender) finalize(ctx context.Context, b store.Batch) error {
	s := x.Service
	net := safety.Network(b.Network)
	c, ok := x.Chains[net]
	if !ok || b.OutboundTxID == nil {
		return fmt.Errorf("no chain for %s", net)
	}
	t, err := store.New(s.Pool).GetOutboundTx(ctx, *b.OutboundTxID)
	if err != nil || t.MinedHash == nil || t.MinedBlock == nil {
		return fmt.Errorf("transaction not recorded as mined: %w", err)
	}
	r, err := c.TransactionReceipt(ctx, common.HexToHash(*t.MinedHash))
	if errors.Is(err, ethereum.NotFound) {
		// Mined, deep enough for the transaction manager, and now gone: a
		// reorg deeper than its confirmations. Resending needs the signed
		// transaction again; a person decides (docs/design/teams.md §5).
		s.log().Error("batch: a mined run's transaction left the chain; needs a person", "batch", b.ID, "tx", *t.MinedHash)
		return err
	}
	if err != nil {
		return err
	}
	var final bool
	if depth := x.Finality[net]; depth > 0 {
		head, err := c.HeaderByNumber(ctx, nil)
		if err != nil {
			return err
		}
		final = head.Number.Uint64() >= r.BlockNumber.Uint64()+depth-1
	} else {
		h, err := c.HeaderByNumber(ctx, big.NewInt(int64(rpc.FinalizedBlockNumber)))
		if err != nil {
			return err // unknown finality is not finality
		}
		final = h.Number.Cmp(r.BlockNumber) >= 0
	}
	if !final {
		return errNotFinal
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		b, err := q.GetBatchForUpdate(ctx, b.ID)
		if err != nil {
			return err
		}
		if statemachine.BatchState(b.State) != statemachine.BatchConfirming {
			return nil
		}
		if err := s.payOut(ctx, tx, b); err != nil {
			return err
		}
		_, err = s.apply(ctx, q, b, statemachine.EvBatchFinal, nil, map[string]any{"txHash": *t.MinedHash})
		return err
	})
}

// payOut records the run's money leaving the vault: the in-flight
// liability is settled against vault custody.
func (s *Service) payOut(ctx context.Context, tx pgx.Tx, b store.Batch) error {
	asset := money.USDC.Code
	net := strings.ReplaceAll(b.Network, ":", "_")
	inflight, err := ledger.EnsureAccount(ctx, tx, ledger.Account{Code: batchAccount(b.ID), Asset: asset, Kind: ledger.KindLiability, Rule: ledger.NonPositive})
	if err != nil {
		return err
	}
	vault, err := ledger.EnsureAccount(ctx, tx, ledger.Account{
		Code: fmt.Sprintf("custody:vault:%s:%s", net, asset), Asset: asset, Kind: ledger.KindAsset, Rule: ledger.NonNegative,
	})
	if err != nil {
		return err
	}
	_, err = ledger.Post(ctx, tx, ledger.Entry{
		IdempotencyKey: "batch:" + b.ID.String() + ":paid",
		Kind:           "batch.paid",
		RefType:        "batch",
		RefID:          b.ID.String(),
		Postings: []ledger.Posting{
			{Account: inflight.ID, Asset: asset, Amount: b.Total},
			{Account: vault.ID, Asset: asset, Amount: b.Total.Neg()},
		},
	})
	return err
}

// Run ticks every poll until ctx is done.
func (x *Sender) Run(ctx context.Context, poll time.Duration) {
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		if err := x.Tick(ctx); err != nil && ctx.Err() == nil {
			x.Service.log().Warn("batch: tick failed; will retry", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
