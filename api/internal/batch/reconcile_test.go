package batch_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"

	"github.com/JoshBlazer/waybill/api/internal/batch"
	"github.com/JoshBlazer/waybill/api/internal/ledger"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/reconcile"
	"github.com/JoshBlazer/waybill/api/internal/safety"
)

// The reconciliation tests live with the batch rig: it is the whole stage
// 4 money path on a real Anvil (docs/design/reconciliation.md §6).

func (r rig) reconciler() *reconcile.Reconciler {
	return &reconcile.Reconciler{
		Pool:     r.pool,
		Networks: map[safety.Network]reconcile.Network{anvil: {Chain: r.a.Client, Deployment: r.d}},
	}
}

// settle lets in-flight work finish between two runs, as a finality
// period does in production.
func (r rig) settle(ctx context.Context) error {
	for range 6 {
		r.a.Mine(1)
		if err := r.w.Tick(ctx); err != nil {
			return err
		}
		if err := r.m.Tick(ctx); err != nil {
			return err
		}
		if err := r.sender.Tick(ctx); err != nil {
			return err
		}
	}
	return nil
}

func check(t *testing.T, rep reconcile.Report, id string) reconcile.Check {
	t.Helper()
	for _, c := range rep.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("report has no check %s", id)
	return reconcile.Check{}
}

func wantClean(t *testing.T, rep reconcile.Report) {
	t.Helper()
	if rep.Differences != 0 {
		t.Fatalf("report has %d differences: %+v", rep.Differences, rep.Checks)
	}
	for _, c := range rep.Checks {
		want := reconcile.OK
		if c.ID == "R5" {
			want = reconcile.Skipped // no provider in this rig
		}
		if c.Status != want {
			t.Fatalf("check %s is %s, want %s: %+v", c.ID, c.Status, want, c)
		}
	}
}

// TestExitCheck_ReconciliationAfterDualApprovalRunIsClean is the second
// half of the stage 4 exit check: after a run above the threshold is
// approved by a second member and paid, the reconciliation report shows
// zero differences.
func TestExitCheck_ReconciliationAfterDualApprovalRunIsClean(t *testing.T) {
	r := setup(t)
	ctx := context.Background()
	r.fundAndSweep(t, 1000)
	run := r.create(t,
		batch.LineInput{ContractorID: r.contractors[0], Amount: "400", Reference: "October retainer"},
		batch.LineInput{ContractorID: r.contractors[1], Amount: "250.5", Reference: "October retainer"},
	)
	if run.State != "awaiting_approval" {
		t.Fatalf("run is %s; want awaiting_approval", run.State)
	}
	if _, err := r.svc.Approve(ctx, r.approver, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Send(ctx, r.maker, run.ID); err != nil {
		t.Fatal(err)
	}
	r.drive(t, func() bool { return r.state(t, run.ID) == "paid" })

	rep, err := r.reconciler().RunSettled(ctx, reconcile.Options{Full: true}, r.settle)
	if err != nil {
		t.Fatal(err)
	}
	wantClean(t, rep)
	if c := check(t, rep, "R2:"+string(anvil)); c.Chain != "349500000" || c.Ledger != "349500000" {
		t.Fatalf("vault check: chain %s, ledger %s; want 349.5 USDC both", c.Chain, c.Ledger)
	}
	if c := check(t, rep, "R4:"+string(anvil)); c.Checked != 2 {
		t.Fatalf("record check covered %d records; want the funding payment and the run", c.Checked)
	}
	var stored int
	_ = r.pool.QueryRow(ctx, `SELECT differences FROM reconciliation_runs WHERE id = $1`, rep.ID).Scan(&stored)
	if stored != 0 {
		t.Fatalf("stored report has %d differences", stored)
	}
}

// Money in flight is listed, not reported: a run paid on chain but not yet
// final. A deposit seen but not yet final needs no entry: the ledger books
// deposit custody at first sighting.
func TestReconcile_MoneyInFlightIsExplained(t *testing.T) {
	r := setup(t)
	ctx := context.Background()
	r.fundAndSweep(t, 300)
	run := r.create(t, batch.LineInput{ContractorID: r.contractors[0], Amount: "100", Reference: "x"})
	if _, err := r.svc.Send(ctx, r.maker, run.ID); err != nil {
		t.Fatal(err)
	}
	r.drive(t, func() bool { return r.state(t, run.ID) == "confirming" })

	f, _ := r.orgs.Funding(ctx, r.orgID)
	r.a.Mint(r.d.Token, common.HexToAddress(f.Addresses[0].Address), 5_000_000)
	r.a.Mine(1)
	if err := r.w.Tick(ctx); err != nil {
		t.Fatal(err)
	}

	rep, err := r.reconciler().Run(ctx, reconcile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	wantClean(t, rep)
	vault := check(t, rep, "R2:"+string(anvil))
	if len(vault.InFlight) != 1 || vault.Explained != "-100000000" {
		t.Fatalf("vault in flight %+v; want the run of 100 USDC", vault.InFlight)
	}
	deposit := check(t, rep, "R3:"+string(anvil))
	if len(deposit.InFlight) != 0 || deposit.Chain != "5000000" || deposit.Ledger != "5000000" {
		t.Fatalf("deposit check %+v; want 5 USDC on both sides and nothing in flight", deposit)
	}
}

// USDC that reaches the vault outside Waybill is unsettled on the first run
// and a difference on the second.
func TestReconcile_UnbookedVaultMoneyIsADifference(t *testing.T) {
	r := setup(t)
	ctx := context.Background()
	r.fundAndSweep(t, 100)
	r.a.Mint(r.d.Token, r.d.Vault, 7_000_000)

	rc := r.reconciler()
	first, err := rc.Run(ctx, reconcile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if c := check(t, first, "R2:"+string(anvil)); c.Status != reconcile.Unsettled || first.Differences != 0 {
		t.Fatalf("first run: vault %s, %d differences; want unsettled and none yet", c.Status, first.Differences)
	}
	second, err := rc.RunSettled(ctx, reconcile.Options{}, r.settle)
	if err != nil {
		t.Fatal(err)
	}
	c := check(t, second, "R2:"+string(anvil))
	if c.Status != reconcile.Difference || c.Unexplained != "7000000" || second.Differences != 1 {
		t.Fatalf("second run: vault %s, unexplained %s, %d differences; want one difference of 7 USDC", c.Status, c.Unexplained, second.Differences)
	}
	if other := check(t, second, "R3:"+string(anvil)); other.Status != reconcile.OK {
		t.Fatalf("deposit check is %s; only the vault is off", other.Status)
	}
}

// A balanced entry that books vault money nobody moved is caught by the
// vault check, though the ledger itself still balances.
func TestReconcile_LedgerOnlyCreditIsADifference(t *testing.T) {
	r := setup(t)
	ctx := context.Background()
	r.fundAndSweep(t, 100)
	// Book 1 USDC into vault custody against the organisation, in the
	// ledger only.
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		vault, err := ledger.EnsureAccount(ctx, tx, ledger.Account{Code: "custody:vault:evm_31337:USDC", Asset: "USDC", Kind: ledger.KindAsset, Rule: ledger.NonNegative})
		if err != nil {
			return err
		}
		owed, err := ledger.EnsureAccount(ctx, tx, ledger.Account{Code: "liability:org:" + r.orgID.String() + ":USDC:available", Asset: "USDC", Kind: ledger.KindLiability, Rule: ledger.NonPositive})
		if err != nil {
			return err
		}
		_, err = ledger.Post(ctx, tx, ledger.Entry{IdempotencyKey: "test:phantom", Kind: "test.phantom", RefType: "test", RefID: "phantom",
			Postings: []ledger.Posting{{Account: vault.ID, Asset: "USDC", Amount: money.FromInt64(1_000_000)}, {Account: owed.ID, Asset: "USDC", Amount: money.FromInt64(-1_000_000)}}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	rc := r.reconciler()
	if _, err := rc.Run(ctx, reconcile.Options{}); err != nil {
		t.Fatal(err)
	}
	rep, err := rc.Run(ctx, reconcile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if c := check(t, rep, "R2:"+string(anvil)); c.Status != reconcile.Difference || c.Unexplained != "-1000000" {
		t.Fatalf("vault %s, unexplained %s; want a difference of -1 USDC", c.Status, c.Unexplained)
	}
}

// A final payment whose recorded amount differs from its Transfer log is a
// record difference.
func TestReconcile_PaymentAmountMismatchIsADifference(t *testing.T) {
	r := setup(t)
	ctx := context.Background()
	r.fundAndSweep(t, 100)
	if _, err := r.pool.Exec(ctx, `UPDATE payments SET amount = amount + 1 WHERE state = 'final'`); err != nil {
		t.Fatal(err)
	}
	rep, err := r.reconciler().Run(ctx, reconcile.Options{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	c := check(t, rep, "R4:"+string(anvil))
	if c.Status != reconcile.Difference || len(c.Problems) != 1 || !strings.Contains(c.Problems[0], "the chain moved 100000000") {
		t.Fatalf("record check %s, problems %v; want one amount mismatch", c.Status, c.Problems)
	}
}
