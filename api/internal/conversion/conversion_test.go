package conversion_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshBlazer/waybill/api/internal/bankverify"
	"github.com/JoshBlazer/waybill/api/internal/conversion"
	"github.com/JoshBlazer/waybill/api/internal/invoice"
	"github.com/JoshBlazer/waybill/api/internal/ledger"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/payout"
	"github.com/JoshBlazer/waybill/api/internal/paystack"
	"github.com/JoshBlazer/waybill/api/internal/rates"
	"github.com/JoshBlazer/waybill/api/internal/reconcile"
	"github.com/JoshBlazer/waybill/api/internal/store"
	"github.com/JoshBlazer/waybill/api/internal/testdb"
)

type rig struct {
	pool       *pgxpool.Pool
	svc        *conversion.Service
	contractor uuid.UUID
	now        time.Time
}

// setup: a contractor with a verified bank account and 100 USDC available.
// The fake market rate is ₦1,550.00, and the spread is 0, so 1 USDC
// converts to exactly ₦1,550.00.
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
	r := &rig{pool: pool, contractor: c.ID, now: now}
	r.credit(t, 100_000_000)
	r.svc = &conversion.Service{
		Pool:   pool,
		Quoter: &rates.Quoter{Provider: &rates.Fake{Rates: map[rates.Pair]money.Amount{invoice.NairaPair: money.FromInt64(155_000)}}, TTL: conversion.QuoteTTL, Now: func() time.Time { return r.now }},
		Now:    func() time.Time { return r.now },
	}
	return r
}

func (r *rig) credit(t *testing.T, micro int64) {
	t.Helper()
	ctx := context.Background()
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		custody, err := ledger.EnsureAccount(ctx, tx, ledger.Account{Code: "custody:vault:evm_31337:USDC", Asset: "USDC", Kind: ledger.KindAsset, Rule: ledger.NonNegative})
		if err != nil {
			return err
		}
		owed, err := ledger.EnsureAccount(ctx, tx, ledger.Account{Code: "liability:contractor:" + r.contractor.String() + ":USDC:available", Asset: "USDC", Kind: ledger.KindLiability, Rule: ledger.NonPositive})
		if err != nil {
			return err
		}
		_, err = ledger.Post(ctx, tx, ledger.Entry{IdempotencyKey: "test:credit:" + uuid.NewString(), Kind: "test.credit", RefType: "test", RefID: "1",
			Postings: []ledger.Posting{{Account: custody.ID, Asset: "USDC", Amount: money.FromInt64(micro)}, {Account: owed.ID, Asset: "USDC", Amount: money.FromInt64(-micro)}}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (r *rig) accept(id uuid.UUID) (conversion.Conversion, error) {
	var out conversion.Conversion
	err := pgx.BeginFunc(context.Background(), r.pool, func(tx pgx.Tx) error {
		var err error
		out, err = r.svc.Accept(context.Background(), tx, r.contractor, id)
		return err
	})
	return out, err
}

func (r *rig) balance(t *testing.T) conversion.Balance {
	t.Helper()
	b, err := r.svc.Balance(context.Background(), r.contractor)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (r *rig) entries(t *testing.T) int {
	t.Helper()
	var n int
	_ = r.pool.QueryRow(context.Background(), `SELECT count(*) FROM journal_entries WHERE kind = 'conversion.accepted'`).Scan(&n)
	return n
}

func TestConversion_QuoteThenAcceptBooksOnceAndQueuesAPayout(t *testing.T) {
	r := setup(t)
	ctx := context.Background()
	c, err := r.svc.Quote(ctx, r.contractor, "40")
	if err != nil {
		t.Fatal(err)
	}
	if c.State != "quoted" || c.Naira.String() != "6200000" || !c.ExpiresAt.Equal(r.now.Add(conversion.QuoteTTL)) {
		t.Fatalf("quote %+v; want ₦62,000.00 for 40 USDC, valid for %s", c, conversion.QuoteTTL)
	}
	if b := r.balance(t); b.USDCAvailable.String() != "100000000" {
		t.Fatalf("a quote moved money: available %s", b.USDCAvailable)
	}

	got, err := r.accept(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "accepted" || got.Payout == nil || got.Payout.State != "converted" || got.Payout.NairaAmount.String() != "6200000" {
		t.Fatalf("accepted %+v, payout %+v", got, got.Payout)
	}
	b := r.balance(t)
	if b.USDCAvailable.String() != "60000000" || b.NGNOnItsWay.String() != "6200000" || len(b.Recent) != 1 {
		t.Fatalf("balance after accept: %+v", b)
	}
	// Accepting again returns the same conversion and books nothing.
	again, err := r.accept(c.ID)
	if err != nil || again.Payout.ID != got.Payout.ID || r.entries(t) != 1 {
		t.Fatalf("second accept: %v, payout %v, %d entries", err, again.Payout, r.entries(t))
	}
	tb, _ := ledger.TrialBalance(ctx, r.pool)
	for a, v := range tb {
		if !v.IsZero() {
			t.Fatalf("trial balance %s = %s", a, v)
		}
	}
}

// An expired quote is refused and books nothing; it is never re-quoted.
func TestConversion_ExpiredQuoteIsRefused(t *testing.T) {
	r := setup(t)
	c, err := r.svc.Quote(context.Background(), r.contractor, "10")
	if err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(conversion.QuoteTTL)
	if _, err := r.accept(c.ID); !errors.Is(err, conversion.ErrExpired) {
		t.Fatalf("accept at expiry: %v, want ErrExpired", err)
	}
	if r.entries(t) != 0 || r.balance(t).USDCAvailable.String() != "100000000" {
		t.Fatal("an expired quote booked money")
	}
	got, err := r.svc.Get(context.Background(), r.pool, r.contractor, c.ID)
	if err != nil || got.State != "expired" || got.Naira.String() != c.Naira.String() {
		t.Fatalf("expired conversion reads %+v, %v", got, err)
	}
}

// Two quotes for the whole balance, accepted at once: exactly one wins.
func TestConversion_ConcurrentAcceptsCannotOverdraw(t *testing.T) {
	r := setup(t)
	ctx := context.Background()
	a, err := r.svc.Quote(ctx, r.contractor, "100")
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.svc.Quote(ctx, r.contractor, "100")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, id := range []uuid.UUID{a.ID, b.ID} {
		wg.Add(1)
		go func() { defer wg.Done(); _, errs[i] = r.accept(id) }()
	}
	wg.Wait()
	ok, short := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, conversion.ErrInsufficient):
			short++
		default:
			t.Fatalf("unexpected error %v", err)
		}
	}
	if ok != 1 || short != 1 || r.entries(t) != 1 || !r.balance(t).USDCAvailable.IsZero() {
		t.Fatalf("%d accepted, %d refused, %d entries, available %s", ok, short, r.entries(t), r.balance(t).USDCAvailable)
	}
}

func TestConversion_RefusesBadRequests(t *testing.T) {
	r := setup(t)
	ctx := context.Background()
	for amount, want := range map[string]error{
		"0.999999": conversion.ErrInvalidAmount,
		"abc":      conversion.ErrInvalidAmount,
		"100.01":   conversion.ErrInsufficient,
	} {
		if _, err := r.svc.Quote(ctx, r.contractor, amount); !errors.Is(err, want) {
			t.Errorf("quote %s: %v, want %v", amount, err, want)
		}
	}
	c, err := r.svc.Quote(ctx, r.contractor, "1")
	if err != nil {
		t.Fatal(err)
	}
	// Another contractor's quote does not exist for them.
	other := &conversion.Service{Pool: r.pool, Quoter: r.svc.Quoter}
	if _, err := other.Get(ctx, r.pool, uuid.New(), c.ID); !errors.Is(err, conversion.ErrNotFound) {
		t.Fatalf("another contractor reading: %v", err)
	}
	err = pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		_, err := r.svc.Accept(ctx, tx, uuid.New(), c.ID)
		return err
	})
	if !errors.Is(err, conversion.ErrNotFound) {
		t.Fatalf("another contractor accepting: %v", err)
	}
	// No bank account: no quote.
	if _, err := r.pool.Exec(ctx, `DELETE FROM contractor_bank_accounts WHERE contractor_id = $1`, r.contractor); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Quote(ctx, r.contractor, "1"); !errors.Is(err, conversion.ErrNoBankAccount) {
		t.Fatalf("no bank account: %v", err)
	}
}

// fakeBank accepts every transfer and confirms it for the amount sent.
type fakeBank struct {
	mu   sync.Mutex
	sent map[string]money.Amount
}

func (f *fakeBank) CreateRecipient(context.Context, string, bankverify.Account) (string, error) {
	return "RCP_test", nil
}

func (f *fakeBank) Initiate(_ context.Context, t paystack.Transfer) (paystack.TransferStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent[t.Reference] = t.Amount
	return paystack.TransferStatus{Status: "pending", Reference: t.Reference, Amount: t.Amount}, nil
}

func (f *fakeBank) Verify(_ context.Context, ref string) (paystack.TransferStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.sent[ref]
	if !ok {
		return paystack.TransferStatus{}, paystack.ErrNotFound
	}
	return paystack.TransferStatus{Status: "success", Reference: ref, Amount: a}, nil
}

func (f *fakeBank) VerifyTransaction(context.Context, string) (paystack.ChargeStatus, error) {
	return paystack.ChargeStatus{}, paystack.ErrNotFound
}

// The conversion's payout goes out through the existing pipeline, the naira
// leaves the balance, and reconciliation against the provider is clean.
func TestConversion_PayoutIsSentAndReconciles(t *testing.T) {
	r := setup(t)
	ctx := context.Background()
	c, err := r.svc.Quote(ctx, r.contractor, "25.5")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.accept(c.ID); err != nil {
		t.Fatal(err)
	}
	bank := &fakeBank{sent: map[string]money.Amount{}}
	po := &payout.Service{Pool: r.pool, Transfers: bank, Log: slog.New(slog.DiscardHandler), Now: time.Now}
	for range 3 {
		if err := po.Tick(ctx); err != nil {
			t.Logf("tick: %v", err)
		}
	}
	got, err := r.svc.Get(ctx, r.pool, r.contractor, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Payout.State != "succeeded" || bank.sent[got.Payout.Reference].String() != "3952500" {
		t.Fatalf("payout %s, sent %v; want succeeded with ₦39,525.00", got.Payout.State, bank.sent)
	}
	if b := r.balance(t); !b.NGNOnItsWay.IsZero() || b.USDCAvailable.String() != "74500000" {
		t.Fatalf("balance after sending: %+v", b)
	}
	rep, err := (&reconcile.Reconciler{Pool: r.pool, Provider: bank}).Run(ctx, reconcile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Differences != 0 {
		t.Fatalf("reconciliation: %+v", rep.Checks)
	}
}
