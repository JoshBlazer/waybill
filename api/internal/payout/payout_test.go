package payout_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshBlazer/waybill/api/internal/bankverify"
	"github.com/JoshBlazer/waybill/api/internal/deployments"
	"github.com/JoshBlazer/waybill/api/internal/invoice"
	"github.com/JoshBlazer/waybill/api/internal/ledger"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/payout"
	"github.com/JoshBlazer/waybill/api/internal/paystack"
	"github.com/JoshBlazer/waybill/api/internal/rates"
	"github.com/JoshBlazer/waybill/api/internal/safety"
	"github.com/JoshBlazer/waybill/api/internal/store"
	"github.com/JoshBlazer/waybill/api/internal/testdb"
)

var deployment = deployments.Deployment{
	Network: "evm:31337", ChainID: 31337,
	Token:          common.HexToAddress("0x5FbDB2315678afecb367f032d93F642f64180aa3"),
	Vault:          common.HexToAddress("0xe7f1725E7734CE288F8367e1Bb143E90bb3F0512"),
	Factory:        common.HexToAddress("0x9fE46736679d2D9a65F0992F2272dE9f3c7fa6e0"),
	Implementation: common.HexToAddress("0x75537828f2ce51be7289709686A69CbFDbB714F1"),
}

// fakePaystack is a transfer provider whose answers each test scripts. It
// remembers every transfer it was asked to make, by reference.
type fakePaystack struct {
	mu           sync.Mutex
	initiateErr  []error  // returned by successive Initiate calls (nil: accepted)
	initStatus   string   // status Initiate reports when it accepts
	verify       []string // statuses for successive Verify calls; "" means not found; "?" means no answer
	recipientErr error
	initiated    []string // references Initiate received
	sent         map[string]bool
}

func newFake() *fakePaystack { return &fakePaystack{initStatus: "pending", sent: map[string]bool{}} }

func (f *fakePaystack) CreateRecipient(context.Context, string, bankverify.Account) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.recipientErr != nil {
		return "", f.recipientErr
	}
	return "RCP_test", nil
}

func (f *fakePaystack) Initiate(_ context.Context, t paystack.Transfer) (paystack.TransferStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.initiated = append(f.initiated, t.Reference)
	var err error
	if len(f.initiateErr) > 0 {
		err, f.initiateErr = f.initiateErr[0], f.initiateErr[1:]
	}
	if err != nil {
		if errors.Is(err, paystack.ErrUnknown) {
			f.sent[t.Reference] = true // it went through; the answer was lost
		}
		return paystack.TransferStatus{}, err
	}
	f.sent[t.Reference] = true
	return paystack.TransferStatus{Status: f.initStatus, TransferCode: "TRF_test", Reference: t.Reference, Amount: t.Amount}, nil
}

func (f *fakePaystack) Verify(_ context.Context, ref string) (paystack.TransferStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := "pending"
	if len(f.verify) > 0 {
		st, f.verify = f.verify[0], f.verify[1:]
	}
	switch st {
	case "?":
		return paystack.TransferStatus{}, fmt.Errorf("%w: timeout", paystack.ErrUnknown)
	case "":
		return paystack.TransferStatus{}, fmt.Errorf("%w: transfer not found", paystack.ErrNotFound)
	}
	return paystack.TransferStatus{Status: st, TransferCode: "TRF_test", Reference: ref}, nil
}

func (f *fakePaystack) initiatedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.initiated)
}

type rig struct {
	pool       *pgxpool.Pool
	ps         *fakePaystack
	svc        *payout.Service
	inv        invoice.Invoice
	contractor uuid.UUID
	now        time.Time
}

// setup makes a naira invoice locked at ₦1,550.00 per USDC (no spread).
func setup(t *testing.T) *rig {
	t.Helper()
	pool := testdb.New(t)
	ctx := context.Background()
	q := store.New(pool)
	c, err := q.UpsertContractor(ctx, store.UpsertContractorParams{ID: uuid.New(), LegalName: "Adaeze Okafor"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := q.UpsertBankAccount(ctx, store.UpsertBankAccountParams{
		ContractorID: c.ID, BankCode: "058", BankName: "Guaranty Trust Bank", AccountNumber: "0123456789",
		AccountName: "OKAFOR ADAEZE", VerifiedAt: now, PayoutsAllowedFrom: now,
	}); err != nil {
		t.Fatal(err)
	}
	quoter := &rates.Quoter{Provider: &rates.Fake{Rates: map[rates.Pair]money.Amount{invoice.NairaPair: money.FromInt64(155_000)}}, TTL: 48 * time.Hour}
	svc := &invoice.Service{Deployments: map[safety.Network]deployments.Deployment{"evm:31337": deployment}, Now: time.Now, Quoter: quoter}
	var inv invoice.Invoice
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		inv, err = svc.Create(ctx, tx, c.ID, invoice.Input{Description: "Work", Amount: "100", Asset: "USDC", Payout: "naira"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	r := &rig{pool: pool, ps: newFake(), inv: inv, contractor: c.ID, now: now}
	r.svc = &payout.Service{
		Pool: pool, Transfers: r.ps, Log: slog.New(slog.DiscardHandler), Now: func() time.Time { return r.now },
		// Late payments are re-quoted at ₦1,600.00.
		Quoter: &rates.Quoter{Provider: &rates.Fake{Rates: map[rates.Pair]money.Amount{invoice.NairaPair: money.FromInt64(160_000)}}, TTL: time.Hour},
	}
	return r
}

// paidOnChain records what the watcher would: a final payment of micro
// USDC, credited to the contractor, and the invoice settled.
func (r *rig) paidOnChain(t *testing.T, micro int64, detectedAt time.Time) {
	t.Helper()
	ctx := context.Background()
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO payments (network, tx_hash, log_index, block_number, block_hash, token_address, from_address,
			                      to_address, invoice_id, asset_code, amount, state, detected_at)
			SELECT network, '0x' || repeat('a', 64), 0, 10, '0x' || repeat('b', 64), '0x' || repeat('c', 40),
			       '0x' || repeat('d', 40), address, invoice_id, 'USDC', $2, 'final', $3
			FROM deposit_addresses WHERE invoice_id = $1`, r.inv.ID, micro, detectedAt); err != nil {
			return err
		}
		custody, err := ledger.EnsureAccount(ctx, tx, ledger.Account{Code: "custody:deposit:evm_31337:USDC", Asset: "USDC", Kind: ledger.KindAsset, Rule: ledger.NonNegative})
		if err != nil {
			return err
		}
		owed, err := ledger.EnsureAccount(ctx, tx, ledger.Account{Code: "liability:contractor:" + r.contractor.String() + ":USDC:available", Asset: "USDC", Kind: ledger.KindLiability, Rule: ledger.NonPositive})
		if err != nil {
			return err
		}
		if _, err := ledger.Post(ctx, tx, ledger.Entry{IdempotencyKey: "test:paid:" + r.inv.ID.String(), Kind: "test.paid", RefType: "test", RefID: "1",
			Postings: []ledger.Posting{{Account: custody.ID, Asset: "USDC", Amount: money.FromInt64(micro)}, {Account: owed.ID, Asset: "USDC", Amount: money.FromInt64(-micro)}}}); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE invoices SET state = 'settled' WHERE id = $1`, r.inv.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (r *rig) tick(t *testing.T) {
	t.Helper()
	if err := r.svc.Tick(context.Background()); err != nil {
		t.Logf("tick: %v", err) // a step may fail and retry; tests assert on state
	}
}

func (r *rig) payout(t *testing.T) store.Payout {
	t.Helper()
	p, err := store.New(r.pool).GetPayoutByInvoice(context.Background(), r.inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (r *rig) balance(t *testing.T, code string) string {
	t.Helper()
	var b string
	if err := r.pool.QueryRow(context.Background(), `
		SELECT coalesce(sum(b.balance), 0)::text FROM ledger_balances b JOIN ledger_accounts a ON a.id = b.account_id
		WHERE a.code = $1`, code).Scan(&b); err != nil {
		t.Fatal(err)
	}
	return b
}

func (r *rig) states(t *testing.T) string {
	t.Helper()
	rows, err := r.pool.Query(context.Background(), `SELECT to_state FROM payout_events WHERE invoice_id = $1 ORDER BY id`, r.inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return strings.Join(out, ",")
}

func trialBalanceZero(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	tb, err := ledger.TrialBalance(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	for a, v := range tb {
		if !v.IsZero() {
			t.Fatalf("trial balance %s = %s", a, v)
		}
	}
}

func TestPayout_SettledNairaInvoiceIsConvertedAndSent(t *testing.T) {
	r := setup(t)
	r.paidOnChain(t, 100_000_000, time.Now())
	r.ps.verify = []string{"pending", "success"}

	r.tick(t) // queued → converted → submitting → submitted
	p := r.payout(t)
	if p.State != "submitted" || p.NairaAmount.String() != "15500000" || p.Reference != payout.Reference(p.ID) {
		t.Fatalf("after one tick: %+v; want submitted for ₦155,000.00", p)
	}
	// The contractor is owed naira now, not USDC.
	if u, n := r.balance(t, "liability:contractor:"+r.contractor.String()+":USDC:available"), r.balance(t, "liability:contractor:"+r.contractor.String()+":NGN:available"); u != "0" || n != "-15500000" {
		t.Fatalf("owed USDC %s, NGN %s after conversion", u, n)
	}
	r.tick(t) // pending
	r.tick(t) // success
	if p := r.payout(t); p.State != "succeeded" {
		t.Fatalf("payout %s, want succeeded", p.State)
	}
	if got := r.states(t); got != "converted,submitting,submitted,succeeded" {
		t.Fatalf("states %s", got)
	}
	if n, f := r.balance(t, "liability:contractor:"+r.contractor.String()+":NGN:available"), r.balance(t, "custody:paystack:NGN"); n != "0" || f != "0" {
		t.Fatalf("after sending: owed NGN %s, Paystack balance %s; want both 0", n, f)
	}
	if r.balance(t, "equity:fx-position:USDC") != "-100000000" {
		t.Fatal("the USDC sold for naira is not on the FX position")
	}
	trialBalanceZero(t, r.pool)
	if r.ps.initiatedCount() != 1 {
		t.Fatalf("%d transfers requested, want 1", r.ps.initiatedCount())
	}
	// Ticking on does nothing more.
	r.tick(t)
	if r.ps.initiatedCount() != 1 || r.payout(t).State != "succeeded" {
		t.Fatal("a finished payout moved again")
	}
}

// TestPayout_ProviderTimeoutGoesUnknownNotFailed (I12 for payouts, end to
// end): the transfer request times out; the payout is unknown, never
// failed, and reconciliation finds the transfer instead of sending again.
func TestPayout_ProviderTimeoutGoesUnknownNotFailed(t *testing.T) {
	r := setup(t)
	r.paidOnChain(t, 100_000_000, time.Now())
	r.ps.initiateErr = []error{fmt.Errorf("%w: timeout", paystack.ErrUnknown)}
	r.ps.verify = []string{"?", "?"} // and the provider cannot be asked either
	r.tick(t)
	if p := r.payout(t); p.State != "unknown" {
		t.Fatalf("after a timeout: %s, want unknown", p.State)
	}
	r.tick(t) // still no answer
	if p := r.payout(t); p.State != "unknown" {
		t.Fatalf("after an unanswered check: %s, want unknown", p.State)
	}
	r.ps.verify = []string{"pending", "success"}
	r.tick(t) // found: submitted
	r.tick(t) // success
	if p := r.payout(t); p.State != "succeeded" {
		t.Fatalf("payout %s, want succeeded", p.State)
	}
	if r.ps.initiatedCount() != 1 {
		t.Fatalf("%d transfer requests; a timeout must be reconciled, never resent blind", r.ps.initiatedCount())
	}
}

// When reconciliation finds nothing, the payout is submitted again with
// the same reference, so the provider would refuse a duplicate anyway.
func TestPayout_NotFoundIsResubmittedWithTheSameReference(t *testing.T) {
	r := setup(t)
	r.paidOnChain(t, 100_000_000, time.Now())
	r.ps.initiateErr = []error{fmt.Errorf("%w: connection reset", paystack.ErrUnknown)}
	// The request never arrived; the resubmission is accepted and succeeds.
	r.ps.verify = []string{"", "success"}
	r.tick(t) // unknown → converted → submitted again
	r.tick(t) // success
	if p := r.payout(t); p.State != "succeeded" {
		t.Fatalf("payout %s, want succeeded", p.State)
	}
	if len(r.ps.initiated) != 2 || r.ps.initiated[0] != r.ps.initiated[1] {
		t.Fatalf("references sent %v; want the same reference twice", r.ps.initiated)
	}
}

// TestPayout_CrashAfterSubmitDoesNotDoublePay: the worker recorded
// submitting and died; the transfer may have gone out. A later worker finds
// the stale submitting payout, treats it as unknown and reconciles; it does
// not call the provider again.
func TestPayout_CrashAfterSubmitDoesNotDoublePay(t *testing.T) {
	r := setup(t)
	r.paidOnChain(t, 100_000_000, time.Now())
	ctx := context.Background()
	r.svc.Transfers = nil // no provider yet: conversion only
	r.tick(t)
	r.svc.Transfers = r.ps
	if _, err := r.pool.Exec(ctx, `UPDATE payouts SET state = 'submitting', updated_at = now() - interval '10 minutes'`); err != nil {
		t.Fatal(err)
	}
	r.ps.sent[r.payout(t).Reference] = true // the transfer did go out before the crash
	r.ps.verify = []string{"success"}
	r.tick(t)
	if p := r.payout(t); p.State != "succeeded" {
		t.Fatalf("payout %s, want succeeded", p.State)
	}
	if r.ps.initiatedCount() != 0 {
		t.Fatalf("the provider was asked to transfer %d more times after the crash", r.ps.initiatedCount())
	}
	trialBalanceZero(t, r.pool)
}

func TestPayout_RejectedFailsAndKeepsTheNairaOwed(t *testing.T) {
	r := setup(t)
	r.paidOnChain(t, 100_000_000, time.Now())
	r.ps.initiateErr = []error{fmt.Errorf("%w: HTTP 400: Your balance is not enough", paystack.ErrRejected)}
	r.tick(t)
	p := r.payout(t)
	if p.State != "failed" || p.Failure == nil || !strings.Contains(*p.Failure, "balance") {
		t.Fatalf("payout %+v; want failed with the reason", p)
	}
	if n := r.balance(t, "liability:contractor:"+r.contractor.String()+":NGN:available"); n != "-15500000" {
		t.Fatalf("owed NGN %s; a failed transfer must leave the naira owed", n)
	}
}

func TestPayout_RefusedAccountFailsWithoutSending(t *testing.T) {
	r := setup(t)
	r.paidOnChain(t, 100_000_000, time.Now())
	r.ps.recipientErr = fmt.Errorf("%w: HTTP 422: Account number is invalid", paystack.ErrRejected)
	r.tick(t)
	if p := r.payout(t); p.State != "failed" || r.ps.initiatedCount() != 0 {
		t.Fatalf("payout %s, %d transfers; want failed and nothing sent", p.State, r.ps.initiatedCount())
	}
}

// A bank account changed less than 24 hours ago receives nothing yet.
func TestPayout_WaitsForTheBankAccountCoolingOff(t *testing.T) {
	r := setup(t)
	r.paidOnChain(t, 100_000_000, time.Now())
	if _, err := r.pool.Exec(context.Background(), `UPDATE contractor_bank_accounts SET payouts_allowed_from = now() + interval '20 hours'`); err != nil {
		t.Fatal(err)
	}
	r.tick(t)
	if p := r.payout(t); p.State != "converted" || r.ps.initiatedCount() != 0 {
		t.Fatalf("payout %s, %d transfers; want converted and waiting", p.State, r.ps.initiatedCount())
	}
	r.now = r.now.Add(21 * time.Hour)
	r.tick(t)
	if p := r.payout(t); p.State != "submitted" {
		t.Fatalf("after the wait: %s, want submitted", p.State)
	}
}

// Money that arrived after the locked rate expired is converted at a new
// quote (PRODUCT §5).
func TestPayout_LatePaymentIsRequoted(t *testing.T) {
	r := setup(t)
	r.paidOnChain(t, 100_000_000, time.Now().Add(49*time.Hour))
	r.tick(t)
	p := r.payout(t)
	if p.NairaAmount.String() != "16000000" || p.RateQuoteID == nil || *p.RateQuoteID == *r.inv.RateQuoteID {
		t.Fatalf("late payout %+v; want ₦160,000.00 at a new quote", p)
	}
}

func TestPayout_CardPaymentIsAlreadyNaira(t *testing.T) {
	r := setup(t)
	ctx := context.Background()
	if _, err := r.pool.Exec(ctx, `
		INSERT INTO checkouts (id, invoice_id, reference, amount, state, expires_at, paid_at, paid_amount)
		VALUES (gen_random_uuid(), $1, 'chk_test_0000000000000001', 15500000, 'paid', now() + interval '1 hour', now(), 15500000)`, r.inv.ID); err != nil {
		t.Fatal(err)
	}
	// What the checkout posts: naira at Paystack, owed to the contractor.
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		float, _ := ledger.EnsureAccount(ctx, tx, ledger.Account{Code: "custody:paystack:NGN", Asset: "NGN", Kind: ledger.KindAsset, Rule: ledger.NonNegative})
		owed, _ := ledger.EnsureAccount(ctx, tx, ledger.Account{Code: "liability:contractor:" + r.contractor.String() + ":NGN:available", Asset: "NGN", Kind: ledger.KindLiability, Rule: ledger.NonPositive})
		if _, err := ledger.Post(ctx, tx, ledger.Entry{IdempotencyKey: "test:card", Kind: "checkout.paid", RefType: "test", RefID: "1",
			Postings: []ledger.Posting{{Account: float.ID, Asset: "NGN", Amount: money.FromInt64(15_500_000)}, {Account: owed.ID, Asset: "NGN", Amount: money.FromInt64(-15_500_000)}}}); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE invoices SET state = 'settled' WHERE id = $1`, r.inv.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	r.ps.initStatus = "success" // test mode: transfers succeed at once
	r.tick(t)
	if p := r.payout(t); p.State != "succeeded" || p.SourceAsset != "NGN" || p.NairaAmount.String() != "15500000" {
		t.Fatalf("card payout %+v", p)
	}
	var conversions int
	_ = r.pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries WHERE kind = 'payout.converted'`).Scan(&conversions)
	if conversions != 0 {
		t.Fatal("a card payment was converted again")
	}
	trialBalanceZero(t, r.pool)
}

// TestPayout_CallbacksAreCheckedAndProcessedOnce: a stored transfer.success
// callback moves the payout only after the provider confirms; a reversal
// callback reverses the payout and the ledger entry.
func TestPayout_CallbacksAreCheckedAndProcessedOnce(t *testing.T) {
	r := setup(t)
	r.paidOnChain(t, 100_000_000, time.Now())
	r.tick(t) // submitted
	ref := r.payout(t).Reference
	ctx := context.Background()
	store := func(id, typ string) {
		body := `{"event":"` + typ + `","data":{"id":1,"reference":"` + ref + `","status":"success"}}`
		if _, err := r.pool.Exec(ctx, `INSERT INTO provider_events (provider, event_id, event_type, raw) VALUES ('paystack', $1, $2, $3)`, id, typ, []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	store("transfer.success:1", "transfer.success")
	r.ps.verify = []string{"success"}
	r.tick(t)
	if p := r.payout(t); p.State != "succeeded" {
		t.Fatalf("after the callback: %s, want succeeded", p.State)
	}
	store("transfer.reversed:1", "transfer.reversed")
	r.ps.verify = []string{"reversed"}
	r.tick(t)
	if p := r.payout(t); p.State != "reversed" {
		t.Fatalf("after the reversal: %s, want reversed", p.State)
	}
	if n := r.balance(t, "liability:contractor:"+r.contractor.String()+":NGN:available"); n != "-15500000" {
		t.Fatalf("owed NGN %s after a reversal; the contractor is owed the naira again", n)
	}
	var unprocessed int
	_ = r.pool.QueryRow(ctx, `SELECT count(*) FROM provider_events WHERE processed_at IS NULL`).Scan(&unprocessed)
	if unprocessed != 0 {
		t.Fatalf("%d callbacks left unprocessed", unprocessed)
	}
	trialBalanceZero(t, r.pool)
}

// Several workers tick at once: one conversion, one transfer request.
func TestPayout_ConcurrentWorkersPayOnce(t *testing.T) {
	r := setup(t)
	r.paidOnChain(t, 100_000_000, time.Now())
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = r.svc.Tick(context.Background()) }()
	}
	wg.Wait()
	var conversions int
	_ = r.pool.QueryRow(context.Background(), `SELECT count(*) FROM journal_entries WHERE kind = 'payout.converted'`).Scan(&conversions)
	if conversions != 1 || r.ps.initiatedCount() != 1 {
		t.Fatalf("%d conversions, %d transfer requests; want 1 and 1", conversions, r.ps.initiatedCount())
	}
}
