package btcwatch_test

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshBlazer/waybill/api/internal/btc"
	"github.com/JoshBlazer/waybill/api/internal/btcwatch"
	"github.com/JoshBlazer/waybill/api/internal/invoice"
	"github.com/JoshBlazer/waybill/api/internal/ledger"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/safety"
	"github.com/JoshBlazer/waybill/api/internal/store"
	"github.com/JoshBlazer/waybill/api/internal/testdb"
)

// testTpub is the BIP84 test vector's account key with test-network
// version bytes; only its public half exists anywhere in Waybill.
const testTpub = "tpubDCxX2sYFS5bDkSe5GKKYHjBW7tgyN1R3UchpLJvdbf54ohxeGRtd8MbDUe1cguVHe4vnK68DsuD5MXjxi9EXx16rb9EnNsaF5KT99CinaJz"

// fakeChain is a scripted Bitcoin chain: blocks[h] is the block at height
// h, replaceable to simulate a reorg.
type fakeChain struct {
	mu     sync.Mutex
	blocks []fakeBlock
}

type fakeBlock struct {
	hash string
	outs []btc.Output
}

func (c *fakeChain) mine(outs ...btc.Output) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blocks = append(c.blocks, fakeBlock{hash: fmt.Sprintf("%064x", len(c.blocks)*1000+1), outs: outs})
}

// reorg replaces every block from height h with empty ones (a fork that
// left the payments out), one more than before.
func (c *fakeChain) reorg(h int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(c.blocks)
	c.blocks = c.blocks[:h]
	for i := h; i <= n; i++ {
		c.blocks = append(c.blocks, fakeBlock{hash: fmt.Sprintf("%064x", i*1000+7)})
	}
}

func (c *fakeChain) Height(context.Context) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return uint64(len(c.blocks) - 1), nil
}

func (c *fakeChain) BlockHash(_ context.Context, h uint64) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if h >= uint64(len(c.blocks)) {
		return "", btc.ErrNotFound
	}
	return c.blocks[h].hash, nil
}

func (c *fakeChain) BlockOutputs(_ context.Context, hash string) ([]btc.Output, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, b := range c.blocks {
		if b.hash == hash {
			return b.outs, nil
		}
	}
	return nil, btc.ErrNotFound
}

func (c *fakeChain) Unspent(context.Context, []string) (money.Amount, error) { return money.Zero, nil }

type rig struct {
	pool  *pgxpool.Pool
	chain *fakeChain
	w     *btcwatch.Watcher
	inv   invoice.Invoice
	addr  string
}

// setup: a BTC invoice for 0.01 BTC on regtest, and a chain of one block.
func setup(t *testing.T) rig {
	t.Helper()
	pool := testdb.New(t)
	ctx := context.Background()
	c, err := store.New(pool).UpsertContractor(ctx, store.UpsertContractorParams{ID: uuid.New(), LegalName: "Adaeze Okafor"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := btc.ParseXPub(safety.BTCRegtest, testTpub)
	if err != nil {
		t.Fatal(err)
	}
	svc := &invoice.Service{BTC: map[safety.Network]*btc.Deriver{safety.BTCRegtest: d}, Now: time.Now}
	var inv invoice.Invoice
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		inv, err = svc.Create(ctx, tx, c.ID, invoice.Input{Description: "Audit", Amount: "0.01", Asset: "BTC", Payout: "hold"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(inv.DepositAddresses) != 1 || inv.DepositAddresses[0].Network != safety.BTCRegtest {
		t.Fatalf("addresses %+v; want one regtest address", inv.DepositAddresses)
	}
	chain := &fakeChain{}
	chain.mine()
	return rig{
		pool: pool, chain: chain, inv: inv, addr: inv.DepositAddresses[0].Address,
		w: &btcwatch.Watcher{Pool: pool, Chain: chain, Log: slog.New(slog.DiscardHandler),
			Cfg: btcwatch.Config{Network: safety.BTCRegtest, Confirmations: 1, Finality: 3}},
	}
}

func (r rig) tick(t *testing.T) {
	t.Helper()
	if err := r.w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (r rig) state(t *testing.T) (string, map[string]int) {
	t.Helper()
	ctx := context.Background()
	var st string
	if err := r.pool.QueryRow(ctx, `SELECT state FROM invoices WHERE id = $1`, r.inv.ID).Scan(&st); err != nil {
		t.Fatal(err)
	}
	ps := map[string]int{}
	rows, _ := r.pool.Query(ctx, `SELECT state, count(*) FROM btc_payments GROUP BY state`)
	defer rows.Close()
	for rows.Next() {
		var s string
		var n int
		_ = rows.Scan(&s, &n)
		ps[s] = n
	}
	return st, ps
}

func (r rig) balance(t *testing.T, code string) string {
	t.Helper()
	var b string
	if err := r.pool.QueryRow(context.Background(), `SELECT coalesce(sum(b.balance), 0)::text FROM ledger_balances b
		JOIN ledger_accounts a ON a.id = b.account_id WHERE a.code LIKE $1`, code).Scan(&b); err != nil {
		t.Fatal(err)
	}
	return b
}

func sats(n int64) money.Amount { return money.FromInt64(n) }

// A payment is detected, booked when confirmed, and settles the invoice at
// finality; the same output seen again is not counted twice.
func TestBTCWatch_PaymentConfirmsAndSettles(t *testing.T) {
	r := setup(t)
	r.tick(t)
	out := btc.Output{TxID: fmt.Sprintf("%064x", 0xabc), Vout: 1, Address: r.addr, Amount: sats(1_000_000)}
	r.chain.mine(out, btc.Output{TxID: fmt.Sprintf("%064x", 0xdef), Address: "bcrt1qsomeoneelse0000000000000000000000000", Amount: sats(5)})
	r.tick(t)
	if st, ps := r.state(t); st != "confirmed" || ps["confirmed"] != 1 {
		t.Fatalf("after one block: invoice %s, payments %v; want confirmed (Confirmations 1)", st, ps)
	}
	if p := r.balance(t, "liability:contractor:%:BTC:pending"); p != "-1000000" {
		t.Fatalf("pending %s", p)
	}
	r.chain.mine()
	r.chain.mine()
	r.tick(t)
	r.tick(t) // ticking again changes nothing
	if st, ps := r.state(t); st != "settled" || ps["final"] != 1 || len(ps) != 1 {
		t.Fatalf("at depth 3: invoice %s, payments %v; want settled with one final payment", st, ps)
	}
	if a, c := r.balance(t, "liability:contractor:%:BTC:available"), r.balance(t, "custody:xpub:btc_regtest:BTC"); a != "-1000000" || c != "1000000" {
		t.Fatalf("available %s, custody %s", a, c)
	}
	tb, _ := ledger.TrialBalance(context.Background(), r.pool)
	for asset, v := range tb {
		if !v.IsZero() {
			t.Fatalf("trial balance %s = %s", asset, v)
		}
	}
}

// Two invoices get different addresses: an address is never reused.
func TestBTCWatch_AddressesAreNeverReused(t *testing.T) {
	r := setup(t)
	ctx := context.Background()
	d, _ := btc.ParseXPub(safety.BTCRegtest, testTpub)
	svc := &invoice.Service{BTC: map[safety.Network]*btc.Deriver{safety.BTCRegtest: d}, Now: time.Now}
	var second invoice.Invoice
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		second, err = svc.Create(ctx, tx, r.inv.ContractorID, invoice.Input{Description: "Second", Amount: "0.5", Asset: "BTC", Payout: "hold"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	want0, _ := d.Address(0)
	want1, _ := d.Address(1)
	if r.addr != want0 || second.DepositAddresses[0].Address != want1 {
		t.Fatalf("addresses %s, %s; want indexes 0 and 1: %s, %s", r.addr, second.DepositAddresses[0].Address, want0, want1)
	}
	err = pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		_, err := svc.Create(ctx, tx, r.inv.ContractorID, invoice.Input{Description: "Naira", Amount: "1", Asset: "BTC", Payout: "naira"})
		return err
	})
	if err == nil {
		t.Fatal("a BTC invoice with naira payout was accepted")
	}
}

// A confirmed payment whose block is replaced is reorged, its booking
// reversed, and the invoice reopens; the same output mined again counts once.
func TestBTCWatch_ReorgReversesConfirmedPayment(t *testing.T) {
	r := setup(t)
	r.tick(t)
	out := btc.Output{TxID: fmt.Sprintf("%064x", 0xabc), Address: r.addr, Amount: sats(1_000_000)}
	r.chain.mine(out)
	r.tick(t)
	if st, _ := r.state(t); st != "confirmed" {
		t.Fatalf("before the reorg: %s", st)
	}
	r.chain.reorg(1) // block 1, with the payment, is replaced
	r.tick(t)
	if st, ps := r.state(t); st != "open" || ps["reorged"] != 1 || len(ps) != 1 {
		t.Fatalf("after the reorg: invoice %s, payments %v; want open with the payment reorged", st, ps)
	}
	if p, c := r.balance(t, "liability:contractor:%:BTC:pending"), r.balance(t, "custody:xpub:%"); p != "0" || c != "0" {
		t.Fatalf("pending %s, custody %s after the reorg; want 0", p, c)
	}
	r.chain.mine(out) // the transaction comes back in a later block
	r.tick(t)
	if st, ps := r.state(t); st != "confirmed" || ps["reorged"] != 1 || ps["confirmed"] != 1 {
		t.Fatalf("after re-inclusion: invoice %s, payments %v", st, ps)
	}
	if p := r.balance(t, "liability:contractor:%:BTC:pending"); p != "-1000000" {
		t.Fatalf("pending %s; the payment must count exactly once", p)
	}
}
