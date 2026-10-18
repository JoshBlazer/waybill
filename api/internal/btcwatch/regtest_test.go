package btcwatch_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/JoshBlazer/waybill/api/internal/btc"
	"github.com/JoshBlazer/waybill/api/internal/btcwatch"
	"github.com/JoshBlazer/waybill/api/internal/invoice"
	"github.com/JoshBlazer/waybill/api/internal/reconcile"
	"github.com/JoshBlazer/waybill/api/internal/safety"
	"github.com/JoshBlazer/waybill/api/internal/store"
	"github.com/JoshBlazer/waybill/api/internal/testchain"
	"github.com/JoshBlazer/waybill/api/internal/testdb"
)

// TestBTCWatch_RegtestEndToEnd runs the Bitcoin path against a real
// bitcoind: Waybill gets only the custody wallet's xpub, derives the same
// addresses bitcoind does, sees a payment from another wallet, and settles
// the invoice at finality; the node's unspent total matches the ledger.
func TestBTCWatch_RegtestEndToEnd(t *testing.T) {
	node := testchain.StartBitcoind(t)
	ctx := context.Background()
	xpub, desc := node.CustodyXPub()
	d, err := btc.ParseXPub(safety.BTCRegtest, xpub)
	if err != nil {
		t.Fatalf("bitcoind's xpub: %v", err)
	}
	// The derivation agrees with bitcoind's own.
	var theirs []string
	node.Call(&theirs, "", "deriveaddresses", desc, []int{0, 2})
	for i, want := range theirs {
		got, err := d.Address(uint32(i))
		if err != nil || got != want {
			t.Fatalf("index %d: Waybill %s, bitcoind %s", i, got, want)
		}
	}

	chain, err := btc.Dial(node.URL)
	if err != nil {
		t.Fatal(err)
	}
	pool := testdb.New(t)
	c, err := store.New(pool).UpsertContractor(ctx, store.UpsertContractorParams{ID: uuid.New(), LegalName: "Adaeze Okafor"})
	if err != nil {
		t.Fatal(err)
	}
	svc := &invoice.Service{BTC: map[safety.Network]*btc.Deriver{safety.BTCRegtest: d}, Now: time.Now}
	var inv invoice.Invoice
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		inv, err = svc.Create(ctx, tx, c.ID, invoice.Input{Description: "Audit", Amount: "0.015", Asset: "BTC", Payout: "hold"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	addr := inv.DepositAddresses[0].Address
	head, err := chain.Height(ctx)
	if err != nil {
		t.Fatal(err)
	}
	w := &btcwatch.Watcher{Pool: pool, Chain: chain, Log: slog.New(slog.DiscardHandler),
		Cfg: btcwatch.Config{Network: safety.BTCRegtest, Confirmations: 1, Finality: 3, StartHeight: head}}
	if err := w.Tick(ctx); err != nil {
		t.Fatal(err)
	}

	node.Pay(addr, "0.015")
	for range 3 {
		if err := w.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		node.Mine(1)
	}
	if err := w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	var st string
	_ = pool.QueryRow(ctx, `SELECT state FROM invoices WHERE id = $1`, inv.ID).Scan(&st)
	var final int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM btc_payments WHERE state = 'final' AND amount = 1500000`).Scan(&final)
	if st != "settled" || final != 1 {
		t.Fatalf("invoice %s, %d final payments of 0.015 BTC; want settled with one", st, final)
	}
	unspent, err := chain.Unspent(ctx, []string{addr})
	if err != nil {
		t.Fatal(err)
	}
	var custody string
	_ = pool.QueryRow(ctx, `SELECT b.balance::text FROM ledger_balances b JOIN ledger_accounts a ON a.id = b.account_id
		WHERE a.code = 'custody:xpub:btc_regtest:BTC'`).Scan(&custody)
	if unspent.String() != "1500000" || custody != "1500000" {
		t.Fatalf("node holds %s sats at the address, ledger custody %s; want 1500000 both", unspent, custody)
	}

	// Reconciliation agrees: xpub custody equals the unspent value on the node.
	rep, err := (&reconcile.Reconciler{Pool: pool, BTC: map[safety.Network]btc.Chain{safety.BTCRegtest: chain}}).Run(ctx, reconcile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range rep.Checks {
		if c.ID == "R3:btc:regtest" && (c.Status != reconcile.OK || c.Chain != "1500000") {
			t.Fatalf("bitcoin custody check %+v", c)
		}
	}
	if rep.Differences != 0 {
		t.Fatalf("%d differences: %+v", rep.Differences, rep.Checks)
	}
}
