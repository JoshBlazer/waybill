package txmgr

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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshBlazer/waybill/api/internal/evm"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/safety"
	"github.com/JoshBlazer/waybill/api/internal/store"
)

// Chain is the part of an EVM client the manager uses. *ethclient.Client
// implements it.
type Chain interface {
	BlockNumber(ctx context.Context) (uint64, error)
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
	SuggestGasTipCap(ctx context.Context) (*big.Int, error)
	EstimateGas(ctx context.Context, msg ethereum.CallMsg) (uint64, error)
	PendingNonceAt(ctx context.Context, account common.Address) (uint64, error)
	SendTransaction(ctx context.Context, tx *types.Transaction) error
	TransactionReceipt(ctx context.Context, hash common.Hash) (*types.Receipt, error)
}

// ErrEstimateFailed means the node says the transaction would revert. Nothing was
// broadcast and no nonce was used; the row records why. Retrying will not
// help, so callers treat it as permanent and alert.
var ErrEstimateFailed = errors.New("txmgr: gas estimation failed; transaction would revert")

// Request is one transaction to send.
type Request struct {
	Purpose string // e.g. "sweep"; selects the Hook run when it is mined
	RefID   string // unique per purpose: submitting it again returns the first transaction
	To      common.Address
	Data    []byte
	Value   *big.Int // nil means zero
}

// Hook runs in the transaction that records a sent transaction as mined or
// reverted, so its effects (for example a ledger entry) commit with it.
type Hook func(ctx context.Context, tx pgx.Tx, row store.OutboundTx, receipt *types.Receipt) error

// Manager sends transactions for one signer on one network. Design:
// docs/design/txmgr.md §7.
type Manager struct {
	Pool    *pgxpool.Pool
	Chain   Chain
	Signer  Signer
	Network safety.Network
	ChainID *big.Int
	Owner   string // lease owner, unique per process
	Log     *slog.Logger
	Hooks   map[string]Hook

	LeaseTTL      time.Duration // how long one Submit or Tick may hold the key
	MaxFeeCap     *big.Int      // never pay more than this per gas; above it, wait for a person
	BumpPercent   uint64        // the node's replacement price bump (geth: 10)
	StuckAfter    time.Duration // replace an attempt not mined after this long
	Confirmations uint64        // blocks, including the receipt's, before mined counts
	Now           func() time.Time
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) signer() string { return evm.NormalizeAddress(m.Signer.Address()) }

// Submit records req and broadcasts it. It is idempotent per (Purpose,
// RefID): a second call returns the first row and sends nothing.
//
// Gas is estimated first. If the node says the call reverts, a failed row
// is recorded with the reason, no nonce is assigned, nothing is broadcast,
// and the error wraps ErrEstimateFailed. Any other estimation error (the
// node could not be reached) records nothing: "could not find out" is not
// "would revert" (I12).
//
// Once a nonce is assigned the row must be mined eventually; a failed first
// broadcast is retried by Tick.
func (m *Manager) Submit(ctx context.Context, req Request) (store.OutboundTx, error) {
	q := store.New(m.Pool)
	row, err := q.GetOutboundTxByRef(ctx, store.GetOutboundTxByRefParams{Purpose: req.Purpose, RefID: req.RefID})
	switch {
	case err == nil:
		if row.State == "failed" {
			return row, fmt.Errorf("%w: %s", ErrEstimateFailed, deref(row.Failure))
		}
		return row, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return store.OutboundTx{}, err
	}

	value := req.Value
	if value == nil {
		value = new(big.Int)
	}
	if req.Data == nil {
		req.Data = []byte{} // a plain transfer; stored as empty, not NULL
	}
	valueAmt, err := money.FromBig(value)
	if err != nil {
		return store.OutboundTx{}, fmt.Errorf("txmgr: value: %w", err)
	}
	to := req.To
	gas, err := m.Chain.EstimateGas(ctx, ethereum.CallMsg{From: m.Signer.Address(), To: &to, Data: req.Data, Value: value})
	if err != nil {
		reason, reverted := RevertReason(err)
		if !reverted {
			return store.OutboundTx{}, fmt.Errorf("txmgr: estimate gas (not recorded; will retry): %w", err)
		}
		failed, ierr := q.InsertFailedOutboundTx(ctx, store.InsertFailedOutboundTxParams{
			Signer: m.signer(), Network: string(m.Network), Purpose: req.Purpose, RefID: req.RefID,
			ToAddress: evm.NormalizeAddress(req.To), Data: req.Data, Value: valueAmt, Failure: &reason,
		})
		if ierr != nil {
			return store.OutboundTx{}, ierr
		}
		m.Log.Error("txmgr: transaction would revert; not sent", "purpose", req.Purpose, "ref", req.RefID, "reason", reason)
		return failed, fmt.Errorf("%w: %s", ErrEstimateFailed, reason)
	}
	// 20 % headroom: state can change between estimate and inclusion.
	gasLimit := int64(gas + gas/5) //nolint:gosec // block gas limits are far below 2^63

	lease, err := AcquireLease(ctx, m.Pool, m.Signer.Address(), m.Network, m.Owner, m.LeaseTTL)
	if err != nil {
		return store.OutboundTx{}, err
	}
	pending, err := m.Chain.PendingNonceAt(ctx, m.Signer.Address())
	if err != nil {
		return store.OutboundTx{}, fmt.Errorf("txmgr: pending nonce: %w", err)
	}
	err = pgx.BeginFunc(ctx, m.Pool, func(tx pgx.Tx) error {
		nonce, err := NextNonce(ctx, tx, lease, pending)
		if err != nil {
			return err
		}
		n := int64(nonce) //nolint:gosec // NextNonce keeps nonces below 2^63
		row, err = store.New(tx).InsertOutboundTx(ctx, store.InsertOutboundTxParams{
			Signer: m.signer(), Network: string(m.Network), Nonce: &n, Purpose: req.Purpose, RefID: req.RefID,
			ToAddress: evm.NormalizeAddress(req.To), Data: req.Data, Value: valueAmt, GasLimit: &gasLimit,
		})
		return err
	})
	if err != nil {
		return store.OutboundTx{}, err
	}
	if err := m.broadcastInitial(ctx, row); err != nil {
		m.Log.Warn("txmgr: first broadcast failed; Tick retries", "tx", row.ID, "err", err)
	}
	return row, nil
}

// market returns what a fresh transaction would pay now.
func (m *Manager) market(ctx context.Context) (Fees, error) {
	h, err := m.Chain.HeaderByNumber(ctx, nil)
	if err != nil {
		return Fees{}, fmt.Errorf("txmgr: latest header: %w", err)
	}
	if h.BaseFee == nil {
		return Fees{}, errors.New("txmgr: chain has no base fee (pre-London)")
	}
	tip, err := m.Chain.SuggestGasTipCap(ctx)
	if err != nil {
		return Fees{}, fmt.Errorf("txmgr: suggested tip: %w", err)
	}
	return InitialFees(h.BaseFee, tip, new(big.Int).Lsh(big.NewInt(1), 255))
}

func (m *Manager) broadcastInitial(ctx context.Context, row store.OutboundTx) error {
	f, err := m.market(ctx)
	if err != nil {
		return err
	}
	if f.MaxFee.Cmp(m.MaxFeeCap) > 0 {
		return fmt.Errorf("%w: market max fee %s wei above cap %s; waiting", ErrFeeCapExceeded, f.MaxFee, m.MaxFeeCap)
	}
	return m.broadcast(ctx, row, f)
}

// broadcast signs row with fees, records the hash, then sends it. The hash
// is recorded first: if the process dies after sending, the hash is still
// known and watched (I11).
func (m *Manager) broadcast(ctx context.Context, row store.OutboundTx, f Fees) error {
	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   m.ChainID,
		Nonce:     uint64(*row.Nonce),    //nolint:gosec // CHECK (nonce >= 0)
		Gas:       uint64(*row.GasLimit), //nolint:gosec // CHECK (gas_limit > 0)
		GasTipCap: f.Tip,
		GasFeeCap: f.MaxFee,
		To:        ptr(common.HexToAddress(row.ToAddress)),
		Value:     row.Value.Big(),
		Data:      row.Data,
	})
	signed, err := m.Signer.SignTx(tx, m.ChainID)
	if err != nil {
		return fmt.Errorf("txmgr: sign: %w", err)
	}
	maxFee, err := money.FromBig(f.MaxFee)
	if err != nil {
		return err
	}
	tip, err := money.FromBig(f.Tip)
	if err != nil {
		return err
	}
	hash := strings.ToLower(signed.Hash().Hex())
	if _, err := store.New(m.Pool).InsertOutboundAttempt(ctx, store.InsertOutboundAttemptParams{
		OutboundTxID: row.ID, Hash: hash, MaxFee: maxFee, Tip: tip,
	}); err != nil {
		return fmt.Errorf("txmgr: record attempt: %w", err)
	}
	if err := m.Chain.SendTransaction(ctx, signed); err != nil {
		// "already known" and "nonce too low" mean the node has this or an
		// earlier attempt; receipts settle which one mined.
		return fmt.Errorf("txmgr: send %s: %w", hash, err)
	}
	m.Log.Info("txmgr: broadcast", "tx", row.ID, "purpose", row.Purpose, "nonce", *row.Nonce, "hash", hash,
		"maxFee", f.MaxFee.String(), "tip", f.Tip.String())
	return nil
}

// Tick follows every pending transaction of this signer: it broadcasts ones
// that were never sent, records the one attempt that was mined (running its
// purpose's Hook in the same database transaction), and replaces attempts
// that have waited longer than StuckAfter with both fees bumped (I11).
// Only the lease holder ticks; another process holding the key is not an
// error. An RPC error stops the tick with nothing recorded (I12).
func (m *Manager) Tick(ctx context.Context) error {
	if _, err := AcquireLease(ctx, m.Pool, m.Signer.Address(), m.Network, m.Owner, m.LeaseTTL); err != nil {
		if errors.Is(err, ErrLeaseHeld) {
			return nil
		}
		return err
	}
	q := store.New(m.Pool)
	rows, err := q.ListPendingOutboundTxs(ctx, store.ListPendingOutboundTxsParams{Signer: m.signer(), Network: string(m.Network)})
	if err != nil || len(rows) == 0 {
		return err
	}
	head, err := m.Chain.BlockNumber(ctx)
	if err != nil {
		return fmt.Errorf("txmgr: head: %w", err)
	}
	for _, row := range rows {
		if err := m.follow(ctx, row, head); err != nil {
			return fmt.Errorf("txmgr: tx %s: %w", row.ID, err)
		}
	}
	return nil
}

func (m *Manager) follow(ctx context.Context, row store.OutboundTx, head uint64) error {
	attempts, err := store.New(m.Pool).ListOutboundAttempts(ctx, row.ID)
	if err != nil {
		return err
	}
	if len(attempts) == 0 {
		if err := m.broadcastInitial(ctx, row); err != nil {
			m.Log.Warn("txmgr: broadcast not done; will retry", "tx", row.ID, "err", err)
		}
		return nil
	}
	// Only one attempt can be mined: they share a nonce.
	for _, a := range attempts {
		r, err := m.Chain.TransactionReceipt(ctx, common.HexToHash(a.Hash))
		if errors.Is(err, ethereum.NotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("receipt %s: %w", a.Hash, err)
		}
		if r.BlockNumber.Uint64()+m.Confirmations-1 > head {
			return nil // mined, not yet deep enough
		}
		return m.finish(ctx, row, a.Hash, r)
	}
	last := attempts[len(attempts)-1]
	if m.now().Sub(last.BroadcastAt) < m.StuckAfter {
		return nil
	}
	market, err := m.market(ctx)
	if err != nil {
		return err
	}
	next, err := Replacement(Fees{MaxFee: last.MaxFee.Big(), Tip: last.Tip.Big()}, market, m.BumpPercent, m.MaxFeeCap)
	if errors.Is(err, ErrFeeCapExceeded) {
		m.Log.Error("txmgr: stuck transaction needs fees above the cap; waiting for a person", "tx", row.ID, "err", err)
		return nil
	}
	if err != nil {
		return err
	}
	m.Log.Warn("txmgr: replacing stuck transaction", "tx", row.ID, "nonce", *row.Nonce, "previous", last.Hash, "waited", m.now().Sub(last.BroadcastAt).Round(time.Second))
	if err := m.broadcast(ctx, row, next); err != nil {
		m.Log.Warn("txmgr: replacement broadcast failed; will retry", "tx", row.ID, "err", err)
	}
	return nil
}

func (m *Manager) finish(ctx context.Context, row store.OutboundTx, hash string, r *types.Receipt) error {
	state := "mined"
	if r.Status != types.ReceiptStatusSuccessful {
		state = "reverted"
	}
	block := r.BlockNumber.Int64()
	return pgx.BeginFunc(ctx, m.Pool, func(tx pgx.Tx) error {
		n, err := store.New(tx).FinishOutboundTx(ctx, store.FinishOutboundTxParams{
			ID: row.ID, State: state, MinedHash: &hash, MinedBlock: &block,
		})
		if err != nil || n == 0 {
			return err // n == 0: already finished
		}
		if state == "reverted" {
			m.Log.Error("txmgr: transaction reverted on chain", "tx", row.ID, "purpose", row.Purpose, "hash", hash)
		}
		if h, ok := m.Hooks[row.Purpose]; ok {
			if err := h(ctx, tx, row, r); err != nil {
				return fmt.Errorf("hook %s: %w", row.Purpose, err)
			}
		}
		return nil
	})
}

// Run ticks every poll until ctx is done.
func (m *Manager) Run(ctx context.Context, poll time.Duration) {
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		if err := m.Tick(ctx); err != nil && ctx.Err() == nil {
			m.Log.Warn("txmgr: tick failed; will retry", "network", m.Network, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func ptr[T any](v T) *T { return &v }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
