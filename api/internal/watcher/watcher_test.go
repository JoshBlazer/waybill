package watcher_test

import (
	"context"
	"errors"
	"log/slog"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshBlazer/waybill/api/internal/deployments"
	"github.com/JoshBlazer/waybill/api/internal/invoice"
	"github.com/JoshBlazer/waybill/api/internal/ledger"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/safety"
	"github.com/JoshBlazer/waybill/api/internal/store"
	"github.com/JoshBlazer/waybill/api/internal/testdb"
	"github.com/JoshBlazer/waybill/api/internal/watcher"
)

var (
	token        = common.HexToAddress("0x5FbDB2315678afecb367f032d93F642f64180aa3")
	otherToken   = common.HexToAddress("0x00000000000000000000000000000000000000aa")
	payer        = common.HexToAddress("0x00000000000000000000000000000000000000bb")
	transferSig  = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))
	deploymentOK = deployments.Deployment{
		Network: "evm:31337", ChainID: 31337, Token: token,
		Vault:          common.HexToAddress("0xe7f1725E7734CE288F8367e1Bb143E90bb3F0512"),
		Factory:        common.HexToAddress("0x9fE46736679d2D9a65F0992F2272dE9f3c7fa6e0"),
		Implementation: common.HexToAddress("0x75537828f2ce51be7289709686A69CbFDbB714F1"),
	}
)

// fakeChain is an in-memory chain. Each block's header carries a
// "version" in Extra, so replacing a block changes its hash.
type fakeChain struct {
	mu       sync.Mutex
	head     uint64
	versions map[uint64]byte
	logs     []types.Log
	failHead bool
	failLogs bool
}

func newFakeChain() *fakeChain { return &fakeChain{versions: map[uint64]byte{}} }

func (c *fakeChain) header(n uint64) *types.Header {
	return &types.Header{Number: new(big.Int).SetUint64(n), Extra: []byte{c.versions[n]}, Difficulty: big.NewInt(0)}
}

func (c *fakeChain) BlockNumber(context.Context) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failHead {
		return 0, errors.New("rpc: connection refused")
	}
	return c.head, nil
}

func (c *fakeChain) HeaderByNumber(_ context.Context, n *big.Int) (*types.Header, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.header(n.Uint64()), nil
}

func (c *fakeChain) FilterLogs(_ context.Context, q ethereum.FilterQuery) ([]types.Log, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failLogs {
		return nil, errors.New("rpc: 503")
	}
	var out []types.Log
	for _, l := range c.logs {
		if l.BlockNumber < q.FromBlock.Uint64() || l.BlockNumber > q.ToBlock.Uint64() {
			continue
		}
		if len(q.Addresses) > 0 && l.Address != q.Addresses[0] {
			continue
		}
		out = append(out, l)
	}
	return out, nil
}

// transfer adds a Transfer log in block n and returns it.
func (c *fakeChain) transfer(n uint64, tokenAddr, to common.Address, amount int64, idx uint) types.Log {
	c.mu.Lock()
	defer c.mu.Unlock()
	l := types.Log{
		Address:     tokenAddr,
		Topics:      []common.Hash{transferSig, common.BytesToHash(payer.Bytes()), common.BytesToHash(to.Bytes())},
		Data:        common.LeftPadBytes(big.NewInt(amount).Bytes(), 32),
		BlockNumber: n,
		BlockHash:   c.header(n).Hash(),
		TxHash:      crypto.Keccak256Hash([]byte{byte(n), byte(idx)}),
		Index:       idx,
	}
	c.logs = append(c.logs, l)
	return l
}

type rig struct {
	pool  *pgxpool.Pool
	chain *fakeChain
	w     *watcher.Watcher
	inv   invoice.Invoice
	dep   common.Address
}

func setup(t *testing.T) rig {
	t.Helper()
	pool := testdb.New(t)
	ctx := context.Background()
	c, err := store.New(pool).UpsertContractor(ctx, store.UpsertContractorParams{ID: uuid.New(), LegalName: "Adaeze Okafor"})
	if err != nil {
		t.Fatal(err)
	}
	svc := &invoice.Service{Deployments: map[safety.Network]deployments.Deployment{"evm:31337": deploymentOK}, Now: time.Now}
	var inv invoice.Invoice
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		inv, err = svc.Create(ctx, tx, c.ID, invoice.Input{Description: "Work", Amount: "100", Asset: "USDC", Payout: "hold"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	chain := newFakeChain()
	return rig{
		pool:  pool,
		chain: chain,
		inv:   inv,
		dep:   common.HexToAddress(inv.DepositAddresses[0].Address),
		w: &watcher.Watcher{Pool: pool, Chain: chain, Log: slog.New(slog.DiscardHandler), Cfg: watcher.Config{
			Network: "evm:31337", Token: token, Asset: money.USDC,
			Confirmations: 3, Finality: 6, BatchBlocks: 4, PollInterval: time.Millisecond,
		}},
	}
}

func (r rig) state(t *testing.T) (invoiceState string, payments map[string]int) {
	t.Helper()
	ctx := context.Background()
	if err := r.pool.QueryRow(ctx, `SELECT state FROM invoices WHERE id = $1`, r.inv.ID).Scan(&invoiceState); err != nil {
		t.Fatal(err)
	}
	payments = map[string]int{}
	rows, err := r.pool.Query(ctx, `SELECT state, count(*) FROM payments GROUP BY state`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			t.Fatal(err)
		}
		payments[s] = n
	}
	return invoiceState, payments
}

func (r rig) cursor(t *testing.T) int64 {
	t.Helper()
	var n int64
	err := r.pool.QueryRow(context.Background(), `SELECT next_block FROM chain_cursors WHERE network = 'evm:31337'`).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return -1
	}
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestWatcher_DetectsPaymentToDepositAddress(t *testing.T) {
	r := setup(t)
	r.chain.transfer(5, token, r.dep, 100_000_000, 0)
	r.chain.head = 6 // depth 2 < 3 confirmations
	if err := r.w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, ps := r.state(t)
	if st != "received" || ps["detected"] != 1 {
		t.Fatalf("invoice %s, payments %v; want received with one detected", st, ps)
	}
	if c := r.cursor(t); c != 7 {
		t.Fatalf("cursor = %d, want 7", c)
	}
}

func TestWatcher_DuplicateLogIsNoop(t *testing.T) {
	r := setup(t)
	r.chain.transfer(5, token, r.dep, 100_000_000, 0)
	r.chain.head = 6
	ctx := context.Background()
	if err := r.w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// Rewind the cursor so the same log is seen again.
	if _, err := r.pool.Exec(ctx, `UPDATE chain_cursors SET next_block = 0`); err != nil {
		t.Fatal(err)
	}
	if err := r.w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	var events int
	_ = r.pool.QueryRow(ctx, `SELECT count(*) FROM invoice_events WHERE type = 'payment_detected'`).Scan(&events)
	if _, ps := r.state(t); ps["detected"] != 1 || events != 1 {
		t.Fatalf("payments %v, detection events %d; want exactly one of each", ps, events)
	}
}

func TestWatcher_IgnoresOtherAddressesAndTokens(t *testing.T) {
	r := setup(t)
	r.chain.transfer(3, token, common.HexToAddress("0x00000000000000000000000000000000000000cc"), 5, 0)
	r.chain.transfer(4, otherToken, r.dep, 100_000_000, 0) // right address, wrong token
	r.chain.head = 20
	if err := r.w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st, ps := r.state(t); st != "open" || len(ps) != 0 {
		t.Fatalf("invoice %s, payments %v; want untouched", st, ps)
	}
}

func TestWatcher_RPCErrorDoesNotAdvance(t *testing.T) {
	r := setup(t)
	r.chain.transfer(5, token, r.dep, 100_000_000, 0)
	r.chain.head = 6
	ctx := context.Background()

	r.chain.failHead = true
	if err := r.w.Tick(ctx); err == nil {
		t.Fatal("Tick succeeded with an unreachable RPC")
	}
	r.chain.failHead, r.chain.failLogs = false, true
	if err := r.w.Tick(ctx); err == nil {
		t.Fatal("Tick succeeded when log queries fail")
	}
	if c := r.cursor(t); c > 0 {
		t.Fatalf("cursor advanced to %d despite RPC errors; the payment would be missed", c)
	}
	r.chain.failLogs = false
	if err := r.w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := r.state(t); st != "received" {
		t.Fatalf("after recovery invoice is %s; the payment was lost", st)
	}
}

func TestWatcher_DoesNotConfirmReplacedBlock(t *testing.T) {
	r := setup(t)
	r.chain.transfer(5, token, r.dep, 100_000_000, 0)
	r.chain.head = 6
	ctx := context.Background()
	if err := r.w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	r.chain.versions[5] = 1 // a reorg replaced block 5
	r.chain.head = 30
	if err := r.w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ps := r.state(t); ps["detected"] != 1 || ps["confirmed"] != 0 {
		t.Fatalf("payments %v; a payment in a replaced block must not be confirmed", ps)
	}
}

// TestWatcher_ConfirmsAndSettles needs the hand-written ledger engine
// (ledger.Post); it fails until that exists.
func TestWatcher_ConfirmsAndSettles(t *testing.T) {
	r := setup(t)
	ctx := context.Background()
	r.chain.transfer(5, token, r.dep, 100_000_000, 0) // exactly the 100 USDC due

	r.chain.head = 7 // depth 3 = confirmations
	if err := r.w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if st, ps := r.state(t); st != "confirmed" || ps["confirmed"] != 1 {
		t.Fatalf("after 3 blocks: invoice %s, payments %v; want confirmed", st, ps)
	}

	r.chain.head = 10 // depth 6 = finality
	if err := r.w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if st, ps := r.state(t); st != "settled" || ps["final"] != 1 {
		t.Fatalf("after 6 blocks: invoice %s, payments %v; want settled", st, ps)
	}

	var available string
	if err := r.pool.QueryRow(ctx, `
		SELECT b.balance::text FROM ledger_balances b JOIN ledger_accounts a ON a.id = b.account_id
		WHERE a.code LIKE 'liability:contractor:%:USDC:available'`).Scan(&available); err != nil {
		t.Fatal(err)
	}
	if available != "-100000000" {
		t.Fatalf("contractor available balance = %s, want -100000000 (we owe 100 USDC)", available)
	}
	tb, err := ledger.TrialBalance(ctx, r.pool)
	if err != nil {
		t.Fatal(err)
	}
	for a, v := range tb {
		if !v.IsZero() {
			t.Fatalf("trial balance %s = %s", a, v)
		}
	}
}
