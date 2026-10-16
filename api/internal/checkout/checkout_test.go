package checkout_test

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

	"github.com/JoshBlazer/waybill/api/internal/checkout"
	"github.com/JoshBlazer/waybill/api/internal/deployments"
	"github.com/JoshBlazer/waybill/api/internal/evm"
	"github.com/JoshBlazer/waybill/api/internal/invoice"
	"github.com/JoshBlazer/waybill/api/internal/ledger"
	"github.com/JoshBlazer/waybill/api/internal/money"
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

// fakePaystack answers verify with whatever the test set for a reference.
type fakePaystack struct {
	mu        sync.Mutex
	inits     []paystack.Charge
	statuses  map[string]paystack.ChargeStatus
	verifyErr error
	initErr   error
}

func (f *fakePaystack) InitializeTransaction(_ context.Context, ch paystack.Charge) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.initErr != nil {
		return "", f.initErr
	}
	f.inits = append(f.inits, ch)
	return "https://checkout.paystack.com/" + ch.Reference, nil
}

func (f *fakePaystack) VerifyTransaction(_ context.Context, ref string) (paystack.ChargeStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.verifyErr != nil {
		return paystack.ChargeStatus{}, f.verifyErr
	}
	st, ok := f.statuses[ref]
	if !ok {
		return paystack.ChargeStatus{}, paystack.ErrNotFound
	}
	return st, nil
}

func (f *fakePaystack) set(ref, status string, kobo int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses[ref] = paystack.ChargeStatus{Status: status, ID: "4099260516", Amount: money.FromInt64(kobo), Currency: "NGN"}
}

type rig struct {
	pool       *pgxpool.Pool
	ps         *fakePaystack
	svc        *checkout.Service
	now        time.Time
	naira      invoice.Invoice // 100 USDC at ₦1,550.00: 15,500,000 kobo
	hold       invoice.Invoice
	contractor uuid.UUID
}

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
	svc := &invoice.Service{
		Deployments: map[safety.Network]deployments.Deployment{"evm:31337": deployment}, Now: time.Now,
		Quoter: &rates.Quoter{Provider: &rates.Fake{Rates: map[rates.Pair]money.Amount{invoice.NairaPair: money.FromInt64(155_000)}}, TTL: 48 * time.Hour},
	}
	r := &rig{pool: pool, ps: &fakePaystack{statuses: map[string]paystack.ChargeStatus{}}, now: now, contractor: c.ID}
	for _, mode := range []string{"naira", "hold"} {
		var inv invoice.Invoice
		if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			inv, err = svc.Create(ctx, tx, c.ID, invoice.Input{Description: "Work", Amount: "100", Asset: "USDC", Payout: mode})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if mode == "naira" {
			r.naira = inv
		} else {
			r.hold = inv
		}
	}
	r.svc = &checkout.Service{
		Pool: pool, Charger: r.ps, WebURL: "http://web.test", Log: slog.New(slog.DiscardHandler),
		Tokens: map[safety.Network]string{"evm:31337": evm.NormalizeAddress(deployment.Token)},
		Now:    func() time.Time { return r.now },
	}
	return r
}

func (r *rig) invoiceState(t *testing.T) string {
	t.Helper()
	var s string
	if err := r.pool.QueryRow(context.Background(), `SELECT state FROM invoices WHERE id = $1`, r.naira.ID).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (r *rig) checkoutState(t *testing.T, ref string) string {
	t.Helper()
	var s string
	if err := r.pool.QueryRow(context.Background(), `SELECT state FROM checkouts WHERE reference = $1`, ref).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
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

func TestStart_OneOpenCheckoutAtTheLockedNairaAmount(t *testing.T) {
	r := setup(t)
	ctx := context.Background()
	c, _, err := r.svc.Start(ctx, r.naira.TrackingCode)
	if err != nil {
		t.Fatal(err)
	}
	if c.Amount.String() != "15500000" || c.AuthorizationUrl == nil || !strings.HasPrefix(*c.AuthorizationUrl, "https://") ||
		c.ExpiresAt.Sub(r.now.Add(checkout.TTL)).Abs() > time.Millisecond || !strings.HasPrefix(c.Reference, "chk_") {
		t.Fatalf("checkout %+v", c)
	}
	init := r.ps.inits[0]
	if init.Amount.String() != "15500000" || init.CallbackURL != "http://web.test/t/"+r.naira.TrackingCode || !strings.HasSuffix(init.Email, "@example.com") {
		t.Fatalf("initialize %+v", init)
	}
	again, _, err := r.svc.Start(ctx, r.naira.TrackingCode)
	if err != nil || again.Reference != c.Reference || len(r.ps.inits) != 1 {
		t.Fatalf("second start: %s (%v), %d initializations; want the same open checkout", again.Reference, err, len(r.ps.inits))
	}
	// Once it has expired, a new one opens and the old one is closed.
	r.now = r.now.Add(checkout.TTL + time.Minute)
	fresh, _, err := r.svc.Start(ctx, r.naira.TrackingCode)
	if err != nil || fresh.Reference == c.Reference || r.checkoutState(t, c.Reference) != "expired" {
		t.Fatalf("after expiry: %s (%v), old %s", fresh.Reference, err, r.checkoutState(t, c.Reference))
	}
}

func TestStart_Refusals(t *testing.T) {
	r := setup(t)
	ctx := context.Background()
	if _, _, err := r.svc.Start(ctx, "WB-0000-0000-0000-0000"); !errors.Is(err, checkout.ErrNotFound) {
		t.Errorf("unknown link: %v", err)
	}
	if _, _, err := r.svc.Start(ctx, r.hold.TrackingCode); !errors.Is(err, checkout.ErrNotPayable) {
		t.Errorf("stablecoin invoice: %v, want ErrNotPayable", err)
	}
	r.now = r.now.Add(49 * time.Hour)
	if _, _, err := r.svc.Start(ctx, r.naira.TrackingCode); !errors.Is(err, checkout.ErrNotPayable) {
		t.Errorf("expired rate: %v, want ErrNotPayable", err)
	}
	r.now = time.Now()
	if _, err := r.pool.Exec(ctx, `UPDATE invoices SET state = 'received' WHERE id = $1`, r.naira.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.svc.Start(ctx, r.naira.TrackingCode); !errors.Is(err, checkout.ErrNotPayable) {
		t.Errorf("already being paid: %v, want ErrNotPayable", err)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE invoices SET state = 'open' WHERE id = $1`, r.naira.ID); err != nil {
		t.Fatal(err)
	}
	r.svc.Charger = nil
	if _, _, err := r.svc.Start(ctx, r.naira.TrackingCode); !errors.Is(err, checkout.ErrUnavailable) {
		t.Errorf("no Paystack key: %v, want ErrUnavailable", err)
	}
	r.svc.Charger = r.ps
	r.ps.initErr = fmt.Errorf("%w: HTTP 502", paystack.ErrUnknown)
	if _, _, err := r.svc.Start(ctx, r.naira.TrackingCode); !errors.Is(err, checkout.ErrUnavailable) {
		t.Errorf("Paystack down: %v, want ErrUnavailable", err)
	}
	var open int
	_ = r.pool.QueryRow(ctx, `SELECT count(*) FROM checkouts WHERE state = 'open'`).Scan(&open)
	if open != 0 {
		t.Fatalf("%d checkouts left open by failed starts", open)
	}
}

func TestReconcile_SuccessSettlesTheInvoiceOnce(t *testing.T) {
	r := setup(t)
	ctx := context.Background()
	c, _, err := r.svc.Start(ctx, r.naira.TrackingCode)
	if err != nil {
		t.Fatal(err)
	}
	r.ps.set(c.Reference, "success", 15_500_000)

	var wg sync.WaitGroup
	for range 4 { // reconciled from several places at once
		wg.Add(1)
		go func() { defer wg.Done(); _ = r.svc.Reconcile(ctx, c.Reference) }()
	}
	wg.Wait()
	_ = r.svc.Reconcile(ctx, c.Reference)

	if st := r.checkoutState(t, c.Reference); st != "paid" {
		t.Fatalf("checkout %s, want paid", st)
	}
	if st := r.invoiceState(t); st != "settled" {
		t.Fatalf("invoice %s, want settled (a successful charge is final)", st)
	}
	var entries int
	_ = r.pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries WHERE kind = 'checkout.paid'`).Scan(&entries)
	if entries != 1 {
		t.Fatalf("%d checkout entries, want 1", entries)
	}
	held, owed := r.balance(t, "custody:paystack:NGN"), r.balance(t, "liability:contractor:"+r.contractor.String()+":NGN:available")
	if held != "15500000" || owed != "-15500000" {
		t.Fatalf("ledger: held at Paystack %s, owed to contractor %s; want ±15500000", held, owed)
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

func TestReconcile_ShortChargeLeavesTheInvoiceUnderpaid(t *testing.T) {
	r := setup(t)
	ctx := context.Background()
	c, _, _ := r.svc.Start(ctx, r.naira.TrackingCode)
	r.ps.set(c.Reference, "success", 15_000_000)
	if err := r.svc.Reconcile(ctx, c.Reference); err != nil {
		t.Fatal(err)
	}
	if st := r.invoiceState(t); st != "underpaid" {
		t.Fatalf("invoice %s, want underpaid", st)
	}
	if owed := r.balance(t, "liability:contractor:"+r.contractor.String()+":NGN:available"); owed != "-15000000" {
		t.Fatalf("owed %s; the ledger records what was paid", owed)
	}
}

// Not knowing is not "not paid" (I12): an unanswered verify changes nothing.
func TestReconcile_NoAnswerChangesNothing(t *testing.T) {
	r := setup(t)
	ctx := context.Background()
	c, _, _ := r.svc.Start(ctx, r.naira.TrackingCode)
	r.ps.verifyErr = fmt.Errorf("%w: timeout", paystack.ErrUnknown)
	r.now = r.now.Add(time.Hour) // even past the checkout window
	if err := r.svc.Reconcile(ctx, c.Reference); err == nil {
		t.Fatal("reconcile reported success without an answer")
	}
	if st := r.checkoutState(t, c.Reference); st != "open" || r.invoiceState(t) != "open" {
		t.Fatalf("checkout %s, invoice %s; want both untouched", st, r.invoiceState(t))
	}
}

func TestReconcile_AbandonedAndFailed(t *testing.T) {
	r := setup(t)
	ctx := context.Background()
	c, _, _ := r.svc.Start(ctx, r.naira.TrackingCode)
	r.ps.set(c.Reference, "abandoned", 15_500_000)
	_ = r.svc.Reconcile(ctx, c.Reference)
	if st := r.checkoutState(t, c.Reference); st != "open" {
		t.Fatalf("abandoned within the window: %s, want open (the payer may come back)", st)
	}
	r.now = r.now.Add(checkout.TTL + time.Minute)
	_ = r.svc.Reconcile(ctx, c.Reference)
	if st := r.checkoutState(t, c.Reference); st != "expired" {
		t.Fatalf("abandoned past the window: %s, want expired", st)
	}

	r.now = time.Now()
	c2, _, err := r.svc.Start(ctx, r.naira.TrackingCode)
	if err != nil {
		t.Fatal(err)
	}
	r.ps.set(c2.Reference, "failed", 15_500_000)
	_ = r.svc.Reconcile(ctx, c2.Reference)
	if st := r.checkoutState(t, c2.Reference); st != "failed" || r.invoiceState(t) != "open" {
		t.Fatalf("declined card: checkout %s, invoice %s; want failed and open", st, r.invoiceState(t))
	}
}

// TestTick_ChargeCallbackIsCheckedNotTrusted: a stored charge.success
// callback triggers a verify by reference and is processed once; the
// amount comes from Paystack's verify, not from the callback body.
func TestTick_ChargeCallbackIsCheckedNotTrusted(t *testing.T) {
	r := setup(t)
	ctx := context.Background()
	c, _, _ := r.svc.Start(ctx, r.naira.TrackingCode)
	r.ps.set(c.Reference, "success", 15_500_000)
	body := `{"event":"charge.success","data":{"id":1,"reference":"` + c.Reference + `","amount":999999999,"currency":"NGN"}}`
	if _, err := r.pool.Exec(ctx, `INSERT INTO provider_events (provider, event_id, event_type, raw) VALUES ('paystack', 'charge.success:1', 'charge.success', $1)`, []byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	var processed bool
	_ = r.pool.QueryRow(ctx, `SELECT processed_at IS NOT NULL FROM provider_events`).Scan(&processed)
	if !processed || r.checkoutState(t, c.Reference) != "paid" {
		t.Fatalf("processed %v, checkout %s", processed, r.checkoutState(t, c.Reference))
	}
	if held := r.balance(t, "custody:paystack:NGN"); held != "15500000" {
		t.Fatalf("held %s; the callback's amount must not be trusted", held)
	}
}
