package httpapi_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/JoshBlazer/waybill/api/internal/batch"
	"github.com/JoshBlazer/waybill/api/internal/deployments"
	"github.com/JoshBlazer/waybill/api/internal/ledger"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/safety"
	"github.com/JoshBlazer/waybill/api/internal/store"
)

// batchEnv is an organisation (threshold 500 USDC) and two verified
// contractors with wallets past their wait.
type batchEnv struct {
	orgEnv
	c1, c2 uuid.UUID
}

const wallet1, wallet2 = "0x1111111111111111111111111111111111111111", "0x2222222222222222222222222222222222222222"

func setupBatches(t *testing.T) batchEnv {
	t.Helper()
	e := setupOrg(t)
	dep := anvilDeployment
	dep.BatchPayout = anvilDeployment.Vault // any non-zero address: these tests never reach the chain
	e.server.Batches = &batch.Service{
		Pool: e.pool, Deployments: map[safety.Network]deployments.Deployment{"evm:31337": dep}, Log: slog.New(slog.DiscardHandler),
	}
	ctx := context.Background()
	q := store.New(e.pool)
	now := time.Now()
	c2, err := q.UpsertContractor(ctx, store.UpsertContractorParams{ID: uuid.New(), LegalName: "Bello Tunde", VerifiedAt: &now})
	if err != nil {
		t.Fatal(err)
	}
	b := batchEnv{orgEnv: e, c1: e.contractor, c2: c2.ID}
	for c, w := range map[uuid.UUID]string{b.c1: wallet1, b.c2: wallet2} {
		if _, err := e.pool.Exec(ctx, `INSERT INTO contractor_wallets (contractor_id, network, address, payouts_allowed_from)
			VALUES ($1, 'evm:31337', $2, now() - interval '1 hour')`, c, w); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

// fund credits the organisation's available balance as a final funding
// deposit would.
func (b batchEnv) fund(t *testing.T, usdc int64) {
	t.Helper()
	ctx := context.Background()
	err := pgx.BeginFunc(ctx, b.pool, func(tx pgx.Tx) error {
		custody, err := ledger.EnsureAccount(ctx, tx, ledger.Account{Code: "custody:vault:evm_31337:USDC", Asset: "USDC", Kind: ledger.KindAsset, Rule: ledger.NonNegative})
		if err != nil {
			return err
		}
		avail, err := ledger.EnsureAccount(ctx, tx, ledger.Account{Code: "liability:org:" + b.orgID.String() + ":USDC:available", Asset: "USDC", Kind: ledger.KindLiability, Rule: ledger.NonPositive})
		if err != nil {
			return err
		}
		amt := money.FromInt64(usdc * 1_000_000)
		_, err = ledger.Post(ctx, tx, ledger.Entry{
			IdempotencyKey: "test-fund:" + uuid.NewString(), Kind: "test.fund", RefType: "test", RefID: "fund",
			Postings: []ledger.Posting{{Account: custody.ID, Asset: "USDC", Amount: amt}, {Account: avail.ID, Asset: "USDC", Amount: amt.Neg()}},
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (b batchEnv) create(t *testing.T, key, idem string, lines ...string) (int, map[string]any) {
	t.Helper()
	r, _ := http.NewRequest(http.MethodPost, b.srv.URL+"/v1/batches", strings.NewReader(`{"lines":[`+strings.Join(lines, ",")+`]}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("Idempotency-Key", idem)
	return do(t, r)
}

func line(c uuid.UUID, amount, ref string) string {
	return fmt.Sprintf(`{"contractorId":%q,"amount":%q,"reference":%q}`, c, amount, ref)
}

func (b batchEnv) act(t *testing.T, key, id, action string) (int, map[string]any) {
	t.Helper()
	return b.req(t, key, http.MethodPost, "/v1/batches/"+id+"/"+action, "")
}

func TestPayoutWallet_SetAndCoolingOff(t *testing.T) {
	e := setupBatches(t)
	put := func(addr string) (int, map[string]any) {
		return e.req(t, e.key, http.MethodPut, "/v1/me/payout-wallet", `{"network":"evm:31337","address":"`+addr+`"}`)
	}
	code, first := put(wallet1) // the address it already has: nothing changes
	if code != http.StatusOK {
		t.Fatalf("same wallet: %d %v", code, first)
	}
	if from, _ := time.Parse(time.RFC3339, first["payoutsAllowedFrom"].(string)); from.After(time.Now()) {
		t.Fatalf("saving the same wallet restarted the wait: %v", first)
	}
	code, changed := put("0x3333333333333333333333333333333333333333")
	from, _ := time.Parse(time.RFC3339, changed["payoutsAllowedFrom"].(string))
	if code != http.StatusOK || time.Until(from) < 23*time.Hour {
		t.Fatalf("changed wallet: %d %v; want a 24-hour wait", code, changed)
	}
	if code, _ := put(anvilDeployment.Vault.Hex()); code != http.StatusUnprocessableEntity {
		t.Fatalf("Waybill's own vault as a wallet: %d, want 422", code)
	}
	if code, _ := e.req(t, e.key, http.MethodPut, "/v1/me/payout-wallet", `{"network":"evm:1","address":"`+wallet1+`"}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("unconfigured network: %d, want 422", code)
	}
	// A run to the changed wallet is held back by the wait.
	e.fund(t, 1000)
	_, run := e.create(t, e.adminKey, "w1", line(e.c1, "10", "after change"))
	lines, _ := run["lines"].([]any)
	l0, _ := lines[0].(map[string]any)
	if run["state"] != "draft" || !strings.Contains(fmt.Sprint(l0["problem"]), "changed their wallet") {
		t.Fatalf("run to a wallet in its wait: %v", run)
	}
}

func TestBatch_LineChecks(t *testing.T) {
	e := setupBatches(t)
	e.fund(t, 1000)
	q := store.New(e.pool)
	unverified, _ := q.UpsertContractor(context.Background(), store.UpsertContractorParams{ID: uuid.New(), LegalName: "Not Checked"})
	code, run := e.create(t, e.adminKey, "c1",
		line(e.c1, "10", "ok"),
		line(unverified.ID, "10", "unverified"),
		line(e.c2, "10", "twice"),
		line(e.c2, "10", "twice"),
	)
	if code != http.StatusCreated || run["state"] != "draft" {
		t.Fatalf("run with bad lines: %d %v; want a draft", code, run)
	}
	want := []string{"", "not verified", "", "twice in this run"}
	for i, l := range run["lines"].([]any) {
		p := fmt.Sprint(l.(map[string]any)["problem"])
		if (want[i] == "" && p != "<nil>") || (want[i] != "" && !strings.Contains(p, want[i])) {
			t.Errorf("line %d problem %q, want %q", i+1, p, want[i])
		}
	}
	if code, _ := e.act(t, e.adminKey, run["id"].(string), "send"); code != http.StatusUnprocessableEntity {
		t.Fatalf("sending a draft: %d, want 422", code)
	}
	// More than the balance: the run says so.
	_, big := e.create(t, e.adminKey, "c2", line(e.c1, "5000", "too much"))
	if big["state"] != "draft" || !strings.Contains(fmt.Sprint(big["problem"]), "available balance") {
		t.Fatalf("run above the balance: %v", big)
	}
	// Bad input is refused outright.
	if code, _ := e.create(t, e.adminKey, "c5", line(uuid.New(), "1", "nobody")); code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown contractor: %d, want 422", code)
	}
	if code, _ := e.create(t, e.adminKey, "c3", line(e.c1, "0", "zero")); code != http.StatusUnprocessableEntity {
		t.Fatalf("zero amount: %d, want 422", code)
	}
	if code, _ := e.create(t, e.approverKy, "c4", line(e.c1, "1", "x")); code != http.StatusForbidden {
		t.Fatalf("an approver building a run: %d, want 403", code)
	}
}

// TestBatch_SelfApprovalRejected is invariant I17: nobody approves their
// own run, in the API and in the database itself.
func TestBatch_SelfApprovalRejected(t *testing.T) {
	e := setupBatches(t)
	e.fund(t, 1000)
	// The admin also holds the approver role: still not their own run.
	if _, err := e.pool.Exec(context.Background(), `UPDATE org_members SET roles = '{admin,maker,approver}' WHERE id = $1`, e.admin); err != nil {
		t.Fatal(err)
	}
	_, run := e.create(t, e.adminKey, "s1", line(e.c1, "400", "a"), line(e.c2, "200", "b")) // 600 > 500
	id := run["id"].(string)
	if run["state"] != "awaiting_approval" || run["needsApproval"] != true {
		t.Fatalf("run above the threshold: %v", run)
	}
	if code, body := e.act(t, e.adminKey, id, "approve"); code != http.StatusForbidden || !strings.Contains(fmt.Sprint(body["type"]), "self-approval") {
		t.Fatalf("creator approving: %d %v, want 403 self-approval", code, body)
	}
	if code, _ := e.act(t, e.adminKey, id, "send"); code != http.StatusConflict {
		t.Fatalf("sending before approval: %d, want 409", code)
	}
	_, err := e.pool.Exec(context.Background(), `INSERT INTO approvals (batch_id, creator_id, approver_id, decision) VALUES ($1, $2, $2, 'approved')`, id, e.admin)
	if err == nil || !strings.Contains(err.Error(), "approvals_not_creator") {
		t.Fatalf("direct SQL self-approval: %v; the database must refuse it", err)
	}
	if code, body := e.act(t, e.approverKy, id, "approve"); code != http.StatusOK || body["state"] != "approved" {
		t.Fatalf("another member approving: %d %v", code, body)
	}
	if code, _ := e.act(t, e.approverKy, id, "approve"); code != http.StatusConflict {
		t.Fatalf("approving twice: %d, want 409", code)
	}
}

func TestBatch_RejectedAndCancelledRunsNeverSend(t *testing.T) {
	e := setupBatches(t)
	e.fund(t, 2000)
	_, a := e.create(t, e.adminKey, "r1", line(e.c1, "600", "big"))
	if code, body := e.act(t, e.approverKy, a["id"].(string), "reject"); code != http.StatusOK || body["state"] != "rejected" {
		t.Fatalf("reject: %d %v", code, body)
	}
	if code, _ := e.act(t, e.adminKey, a["id"].(string), "send"); code != http.StatusConflict {
		t.Fatalf("sending a rejected run: %d, want 409", code)
	}
	_, b := e.create(t, e.adminKey, "r2", line(e.c1, "10", "small"))
	if code, body := e.act(t, e.adminKey, b["id"].(string), "cancel"); code != http.StatusOK || body["state"] != "cancelled" {
		t.Fatalf("cancel: %d %v", code, body)
	}
	if code, _ := e.act(t, e.adminKey, b["id"].(string), "send"); code != http.StatusConflict {
		t.Fatalf("sending a cancelled run: %d, want 409", code)
	}
}

func TestBatch_SendReservesFundsOnceAndQueuesOneTransaction(t *testing.T) {
	e := setupBatches(t)
	e.fund(t, 300)
	_, a := e.create(t, e.adminKey, "f1", line(e.c1, "200", "first"))
	_, b := e.create(t, e.adminKey, "f2", line(e.c2, "200", "second"))
	if a["state"] != "ready" || b["state"] != "ready" {
		t.Fatalf("both runs fit the balance when built: %v / %v", a["state"], b["state"])
	}
	if code, body := e.act(t, e.adminKey, a["id"].(string), "send"); code != http.StatusOK || body["state"] != "sending" {
		t.Fatalf("send: %d %v", code, body)
	}
	// The first run's reservation leaves 100: the second cannot be sent.
	if code, body := e.act(t, e.adminKey, b["id"].(string), "send"); code != http.StatusUnprocessableEntity || !strings.Contains(fmt.Sprint(body["type"]), "insufficient-funds") {
		t.Fatalf("second send: %d %v; want 422 insufficient-funds", code, body)
	}
	if code, _ := e.act(t, e.adminKey, a["id"].(string), "send"); code != http.StatusConflict {
		t.Fatalf("sending twice: %d, want 409", code)
	}
	if code, _ := e.act(t, e.adminKey, a["id"].(string), "cancel"); code != http.StatusConflict {
		t.Fatalf("cancelling a sending run: %d, want 409", code)
	}
	var jobs int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox WHERE topic = 'batch.send'`).Scan(&jobs)
	_, f := e.req(t, e.adminKey, http.MethodGet, "/v1/org/funding", "")
	av, _ := f["available"].(map[string]any)
	if jobs != 1 || av["minor"] != "100000000" {
		t.Fatalf("%d send jobs, available %v; want 1 job and 100 USDC left", jobs, av)
	}
}

func TestBatch_IdempotentCreate(t *testing.T) {
	e := setupBatches(t)
	e.fund(t, 100)
	_, a := e.create(t, e.adminKey, "same", line(e.c1, "10", "x"))
	_, b := e.create(t, e.adminKey, "same", line(e.c1, "10", "x"))
	if a["id"] != b["id"] {
		t.Fatalf("retry created a second run: %v vs %v", a["id"], b["id"])
	}
	if code, _ := e.create(t, e.adminKey, "same", line(e.c1, "11", "x")); code != http.StatusUnprocessableEntity {
		t.Fatalf("same key, different body: %d, want 422", code)
	}
}

func TestTracking_BatchLine(t *testing.T) {
	e := setupBatches(t)
	e.fund(t, 100)
	_, run := e.create(t, e.adminKey, "t1", line(e.c1, "10", "x"))
	code := run["lines"].([]any)[0].(map[string]any)["trackingCode"].(string)
	status, tr := get(t, e.srv.URL+"/v1/track/"+code)
	steps, _ := tr["steps"].([]any)
	if status != http.StatusOK || tr["contractorName"] != "Adaeze Okafor" || len(steps) != 4 {
		t.Fatalf("line tracking: %d %v", status, tr)
	}
	if s := steps[2].(map[string]any)["status"]; s != "skipped" {
		t.Fatalf("converted step %v, want skipped: the contractor receives stablecoin", s)
	}
}
