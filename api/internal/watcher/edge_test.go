package watcher_test

import (
	"context"
	"testing"
	"time"

	"github.com/JoshBlazer/waybill/api/internal/invoice"
)

const availableLike = "liability:contractor:%:USDC:available"

// A payment in two parts, both seen before either is confirmed. Classifying
// at the first confirmation would call it underpaid, and the second part
// would then have no legal way in.
func TestWatcher_TwoPartPaymentIsConfirmedNotStuckUnderpaid(t *testing.T) {
	r := setup(t)
	r.chain.transfer(5, token, r.dep, 60_000_000, 0)
	r.chain.transfer(6, token, r.dep, 40_000_000, 0)
	r.chain.head = 7 // first part confirmed (depth 3), second not yet (depth 2)
	r.tick(t)
	if st, ps := r.state(t); st != "received" || ps["confirmed"] != 1 || ps["detected"] != 1 {
		t.Fatalf("invoice %s, payments %v; want received while the second part is unconfirmed", st, ps)
	}
	r.chain.head = 8
	r.tick(t)
	if st, _ := r.state(t); st != "confirmed" {
		t.Fatalf("invoice %s; want confirmed once both parts are", st)
	}
	r.chain.head = 20
	r.tick(t)
	if st, ps := r.state(t); st != "settled" || ps["final"] != 2 {
		t.Fatalf("invoice %s, payments %v; want settled", st, ps)
	}
	if a := r.balance(t, availableLike); a != "-100000000" {
		t.Fatalf("available %s; want -100000000", a)
	}
	r.assertTrialBalanceZero(t)
}

func TestWatcher_UnderpaidThenTopUp(t *testing.T) {
	r := setup(t)
	r.chain.transfer(5, token, r.dep, 60_000_000, 0)
	r.chain.head = 7
	r.tick(t)
	if st, _ := r.state(t); st != "underpaid" {
		t.Fatalf("invoice %s; want underpaid", st)
	}
	r.chain.transfer(9, token, r.dep, 40_000_000, 0)
	r.chain.head = 9
	r.tick(t)
	if st, _ := r.state(t); st != "received" {
		t.Fatalf("invoice %s; want received while the top-up is checked", st)
	}
	r.chain.head = 11
	r.tick(t)
	if st, _ := r.state(t); st != "confirmed" {
		t.Fatalf("invoice %s; want confirmed after the top-up", st)
	}
	r.chain.head = 20
	r.tick(t)
	if st, _ := r.state(t); st != "settled" {
		t.Fatalf("invoice %s; want settled", st)
	}
	r.assertTrialBalanceZero(t)
}

// Short and final: the money is credited to the contractor (not left in
// limbo) and the invoice waits for the contractor or a top-up.
func TestWatcher_UnderpaidStaysUnderpaidAtFinality(t *testing.T) {
	r := setup(t)
	r.chain.transfer(5, token, r.dep, 60_000_000, 0)
	r.chain.head = 20
	r.tick(t)
	r.tick(t)
	if st, ps := r.state(t); st != "underpaid" || ps["final"] != 1 {
		t.Fatalf("invoice %s, payments %v; want underpaid with the payment final", st, ps)
	}
	if a := r.balance(t, availableLike); a != "-60000000" {
		t.Fatalf("available %s; want -60000000 (60 USDC credited)", a)
	}
	r.assertTrialBalanceZero(t)
}

func TestWatcher_OverpaymentIsCreditedInFull(t *testing.T) {
	r := setup(t)
	r.chain.transfer(5, token, r.dep, 130_000_000, 0)
	r.chain.head = 7
	r.tick(t)
	if st, _ := r.state(t); st != "overpaid" {
		t.Fatalf("invoice %s; want overpaid", st)
	}
	r.chain.head = 20
	r.tick(t)
	if st, _ := r.state(t); st != "settled" {
		t.Fatalf("invoice %s; want settled", st)
	}
	if a := r.balance(t, availableLike); a != "-130000000" {
		t.Fatalf("available %s; want -130000000 (all 130 USDC credited)", a)
	}
	r.assertTrialBalanceZero(t)
}

// The payer sends the full amount twice in two transactions: a duplicate
// payment, not a duplicate sighting. Both are real money.
func TestWatcher_PayerPaysTwiceIsOverpaid(t *testing.T) {
	r := setup(t)
	r.chain.transfer(5, token, r.dep, 100_000_000, 0)
	r.chain.head = 7
	r.tick(t)
	if st, _ := r.state(t); st != "confirmed" {
		t.Fatalf("invoice %s; want confirmed after the first payment", st)
	}
	r.chain.transfer(8, token, r.dep, 100_000_000, 0)
	r.chain.head = 8
	r.tick(t)
	if st, ps := r.state(t); st != "confirmed" || ps["detected"] != 1 {
		t.Fatalf("invoice %s, payments %v; want confirmed with the second payment detected", st, ps)
	}
	r.chain.head = 11 // first payment final (depth 7); second confirmed
	r.tick(t)
	if st, _ := r.state(t); st != "overpaid" {
		t.Fatalf("invoice %s; want overpaid", st)
	}
	r.chain.head = 20
	r.tick(t)
	if st, ps := r.state(t); st != "settled" || ps["final"] != 2 {
		t.Fatalf("invoice %s, payments %v; want settled with both final", st, ps)
	}
	if a := r.balance(t, availableLike); a != "-200000000" {
		t.Fatalf("available %s; want -200000000", a)
	}
	r.assertTrialBalanceZero(t)
}

// Settled is terminal, but money that arrives afterwards is still recorded,
// confirmed and credited.
func TestWatcher_PaymentAfterSettlementIsCredited(t *testing.T) {
	r := setup(t)
	r.chain.transfer(5, token, r.dep, 100_000_000, 0)
	r.chain.head = 20
	r.tick(t)
	r.tick(t)
	if st, _ := r.state(t); st != "settled" {
		t.Fatalf("invoice %s; want settled", st)
	}
	r.chain.transfer(21, token, r.dep, 5_000_000, 0)
	r.chain.head = 40
	r.tick(t)
	r.tick(t)
	if st, ps := r.state(t); st != "settled" || ps["final"] != 2 {
		t.Fatalf("invoice %s, payments %v; want settled with both payments final", st, ps)
	}
	if a := r.balance(t, availableLike); a != "-105000000" {
		t.Fatalf("available %s; want -105000000", a)
	}
	r.assertTrialBalanceZero(t)
}

// TestInvoice_LatePaymentAccepted: an invoice expires, then is paid. The
// payment is accepted, never dropped (docs/RISKS.md §7).
func TestInvoice_LatePaymentAccepted(t *testing.T) {
	r := setup(t)
	ctx := context.Background()
	future := time.Now().Add(365 * 24 * time.Hour)
	n, err := invoice.ExpireDue(ctx, r.pool, future)
	if err != nil || n != 1 {
		t.Fatalf("ExpireDue = %d, %v; want 1 invoice expired", n, err)
	}
	if st, _ := r.state(t); st != "expired" {
		t.Fatalf("invoice %s; want expired", st)
	}
	r.chain.transfer(5, token, r.dep, 100_000_000, 0)
	r.chain.head = 5
	r.tick(t)
	if st, _ := r.state(t); st != "received" {
		t.Fatalf("invoice %s; want received (late payment accepted)", st)
	}
	r.chain.head = 20
	r.tick(t)
	r.tick(t)
	if st, _ := r.state(t); st != "settled" {
		t.Fatalf("invoice %s; want settled", st)
	}
	var from string
	if err := r.pool.QueryRow(ctx, `SELECT from_state FROM invoice_events WHERE type = 'payment_detected'`).Scan(&from); err != nil || from != "expired" {
		t.Fatalf("detection recorded from %q (%v); want from expired, so the late arrival is on record", from, err)
	}
}

func TestExpireDue_OnlyOpenAndDue(t *testing.T) {
	r := setup(t)
	ctx := context.Background()
	if n, err := invoice.ExpireDue(ctx, r.pool, time.Now()); err != nil || n != 0 {
		t.Fatalf("ExpireDue before expiry = %d, %v; want 0", n, err)
	}
	r.chain.transfer(5, token, r.dep, 100_000_000, 0)
	r.chain.head = 5
	r.tick(t) // received: a payment is on its way
	if n, err := invoice.ExpireDue(ctx, r.pool, time.Now().Add(365*24*time.Hour)); err != nil || n != 0 {
		t.Fatalf("ExpireDue on a received invoice = %d, %v; want 0", n, err)
	}
	if st, _ := r.state(t); st != "received" {
		t.Fatalf("invoice %s; a paid invoice must not expire", st)
	}
}
