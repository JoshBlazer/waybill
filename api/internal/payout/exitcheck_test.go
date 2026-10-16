package payout_test

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/JoshBlazer/waybill/api/internal/bankverify"
	"github.com/JoshBlazer/waybill/api/internal/deployments"
	"github.com/JoshBlazer/waybill/api/internal/invoice"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/payout"
	"github.com/JoshBlazer/waybill/api/internal/paystack"
	"github.com/JoshBlazer/waybill/api/internal/rates"
	"github.com/JoshBlazer/waybill/api/internal/safety"
	"github.com/JoshBlazer/waybill/api/internal/store"
	"github.com/JoshBlazer/waybill/api/internal/testchain"
	"github.com/JoshBlazer/waybill/api/internal/testdb"
	"github.com/JoshBlazer/waybill/api/internal/tracking"
	"github.com/JoshBlazer/waybill/api/internal/watcher"
)

// TestExitCheck_LinkPaidAndNairaPayoutCompletes is the stage 3 exit check
// (ROADMAP): a naira payment link is paid on-chain, and a naira payout
// completes end to end.
//
// The chain is a real Anvil with Waybill's contracts; the watcher, the
// ledger and the payout service are the real ones. With PAYSTACK_SECRET_KEY
// set to a test key, the bank account is resolved and the transfer is made
// through Paystack's test mode, and the result is confirmed by asking
// Paystack about the reference. Without it, the same path runs against a
// fake provider, so CI always proves the flow. (Paystack test-mode
// transfers move no money and always succeed.)
func TestExitCheck_LinkPaidAndNairaPayoutCompletes(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t)
	a := testchain.Start(t)
	d := a.DeployWaybill(testchain.Dev0)
	const anvil = safety.EVMAnvil
	q := store.New(pool)

	var transfers payout.Transfers = newFakeWithStatus("success")
	var live *paystack.Client
	acct := bankverify.Account{BankCode: envOr("PAYSTACK_TEST_BANK_CODE", "058"), AccountNumber: envOr("PAYSTACK_TEST_ACCOUNT", "0000000000")}
	accountName := "OKAFOR ADAEZE"
	if key := os.Getenv("PAYSTACK_SECRET_KEY"); key != "" {
		var err error
		if live, err = paystack.New(key, "", 30*time.Second); err != nil {
			t.Fatal(err)
		}
		if accountName, err = live.ResolveAccount(ctx, acct); err != nil {
			t.Fatalf("resolve the test account with Paystack: %v", err)
		}
		transfers = live
		t.Log("running against Paystack test mode")
	} else {
		t.Log("PAYSTACK_SECRET_KEY not set: the transfer goes to a fake provider")
	}

	// A contractor with a verified account and a naira invoice at ₦1,550.00.
	c, err := q.UpsertContractor(ctx, store.UpsertContractorParams{ID: uuid.New(), LegalName: "Adaeze Okafor"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.UpsertBankAccount(ctx, store.UpsertBankAccountParams{
		ContractorID: c.ID, BankCode: acct.BankCode, BankName: "Test bank", AccountNumber: acct.AccountNumber,
		AccountName: accountName, VerifiedAt: time.Now(), PayoutsAllowedFrom: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := q.UpsertChainToken(ctx, store.UpsertChainTokenParams{Network: string(anvil), ContractAddress: strings.ToLower(d.Token.Hex()), AssetCode: "USDC"}); err != nil {
		t.Fatal(err)
	}
	quoter := &rates.Quoter{Provider: &rates.Fake{Rates: map[rates.Pair]money.Amount{invoice.NairaPair: money.FromInt64(155_000)}}, TTL: 48 * time.Hour}
	svc := &invoice.Service{Deployments: map[safety.Network]deployments.Deployment{anvil: d}, Now: time.Now, Quoter: quoter}
	var inv invoice.Invoice
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		inv, err = svc.Create(ctx, tx, c.ID, invoice.Input{Description: "Exit check", Amount: "100", Asset: "USDC", Payout: "naira"})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// The payer pays the link on-chain; the watcher takes it to settled.
	log := slog.New(slog.DiscardHandler)
	w := &watcher.Watcher{Pool: pool, Chain: a.Client, Log: log, Cfg: watcher.Config{
		Network: anvil, Token: d.Token, Asset: money.USDC, Confirmations: 2, Finality: 3, BatchBlocks: 500,
	}}
	a.Mint(d.Token, ethcommon.HexToAddress(inv.DepositAddresses[0].Address), 100_000_000)
	for range 10 {
		a.Mine(1)
		if err := w.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := invoice.LoadByID(ctx, pool, nil, inv.ID); got.State != "settled" {
		t.Fatalf("invoice %s after payment, want settled", got.State)
	}

	// The payout runs until the naira is sent.
	ps := &payout.Service{Pool: pool, Transfers: transfers, Quoter: quoter, Log: log}
	var p store.Payout
	for range 30 {
		if err := ps.Tick(ctx); err != nil {
			t.Logf("tick: %v", err)
		}
		if p, err = q.GetPayoutByInvoice(ctx, inv.ID); err == nil && (p.State == "succeeded" || p.State == "failed") {
			break
		}
		time.Sleep(time.Second)
	}
	if p.State != "succeeded" || p.NairaAmount.String() != "15500000" {
		t.Fatalf("payout %+v; want succeeded for ₦155,000.00", p)
	}
	if live != nil {
		st, err := live.Verify(ctx, p.Reference)
		if err != nil || st.Status != "success" || st.Amount.String() != "15500000" {
			t.Fatalf("Paystack's record of %s: %+v, %v", p.Reference, st, err)
		}
		t.Logf("Paystack test mode confirms transfer %s (%s): %s kobo, %s", p.Reference, st.TransferCode, st.Amount, st.Status)
	}

	// The tracking page shows all four steps done, and the books balance.
	got, err := invoice.LoadByID(ctx, pool, nil, inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range got.Steps() {
		if s.Status != tracking.Done {
			t.Fatalf("tracking step %s is %s, want done", s.Name, s.Status)
		}
	}
	trialBalanceZero(t, pool)
	var owedNGN, owedUSDC string
	_ = pool.QueryRow(ctx, `SELECT coalesce(sum(b.balance),0)::text FROM ledger_balances b JOIN ledger_accounts a ON a.id=b.account_id WHERE a.code LIKE 'liability:contractor:%:NGN:available'`).Scan(&owedNGN)
	_ = pool.QueryRow(ctx, `SELECT coalesce(sum(b.balance),0)::text FROM ledger_balances b JOIN ledger_accounts a ON a.id=b.account_id WHERE a.code LIKE 'liability:contractor:%:USDC:available'`).Scan(&owedUSDC)
	if owedNGN != "0" || owedUSDC != "0" {
		t.Fatalf("still owed: NGN %s, USDC %s; the contractor has been paid in full", owedNGN, owedUSDC)
	}
}

func newFakeWithStatus(status string) *fakePaystack {
	f := newFake()
	f.initStatus = status
	return f
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
