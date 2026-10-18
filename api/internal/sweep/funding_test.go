package sweep_test

import (
	"context"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"

	"github.com/JoshBlazer/waybill/api/internal/deployments"
	"github.com/JoshBlazer/waybill/api/internal/ledger"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/org"
	"github.com/JoshBlazer/waybill/api/internal/safety"
	"github.com/JoshBlazer/waybill/api/internal/store"
	"github.com/JoshBlazer/waybill/api/internal/sweep"
)

// TestFunding_OrgDepositIsCreditedThenSwept: test USDC sent to an
// organisation's funding address is pending at confirmation, available at
// finality, and then swept into the vault through the organisation's own
// CREATE2 forwarder (docs/design/teams.md §6). No invoice moves.
func TestFunding_OrgDepositIsCreditedThenSwept(t *testing.T) {
	r := setup(t, nil)
	ctx := context.Background()
	o, err := store.New(r.pool).UpsertOrg(ctx, store.UpsertOrgParams{ID: uuid.New(), Name: "Lagoon Labs", ApprovalThreshold: money.FromInt64(0)})
	if err != nil {
		t.Fatal(err)
	}
	svc := &org.Service{Pool: r.pool, Deployments: map[safety.Network]deployments.Deployment{anvil: r.d}}
	f, err := svc.Funding(ctx, o.ID)
	if err != nil || len(f.Addresses) != 1 {
		t.Fatalf("funding: %+v, %v", f, err)
	}
	addr := common.HexToAddress(f.Addresses[0].Address)
	if addr == r.dep {
		t.Fatal("the funding address must differ from the invoice's deposit address")
	}

	r.a.Mint(r.d.Token, addr, 700_000_000) // 700 USDC
	r.a.Mine(2)
	if err := r.w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if f, _ = svc.Funding(ctx, o.ID); f.Pending.String() != "700000000" || !f.Available.IsZero() {
		t.Fatalf("after confirmation: pending %s, available %s; want 700 pending", f.Pending, f.Available)
	}
	r.watch(t, 6)
	if f, _ = svc.Funding(ctx, o.ID); !f.Pending.IsZero() || f.Available.String() != "700000000" {
		t.Fatalf("after finality: pending %s, available %s; want 700 available", f.Pending, f.Available)
	}
	if st := r.invoiceState(t); st != "open" {
		t.Fatalf("the unrelated invoice is %s; funding must not touch invoices", st)
	}

	if n, err := sweep.EnqueueDue(ctx, r.pool, anvil); err != nil || n != 1 {
		t.Fatalf("EnqueueDue = %d, %v; want 1", n, err)
	}
	r.runJobs(t)
	var state string
	for range 10 {
		r.a.Mine(1)
		if err := r.m.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		_ = r.pool.QueryRow(ctx, `SELECT state FROM outbound_txs WHERE purpose = 'sweep'`).Scan(&state)
		if state != "pending" {
			break
		}
	}
	if got := r.a.TokenBalance(r.d.Token, r.d.Vault); state != "mined" || got.Cmp(big.NewInt(700_000_000)) != 0 {
		t.Fatalf("sweep %q, vault holds %s; want mined and 700000000", state, got)
	}
	if v, dp := r.balance(t, "custody:vault:evm_31337:USDC"), r.balance(t, "custody:deposit:evm_31337:USDC"); v != "700000000" || dp != "0" {
		t.Fatalf("ledger: vault %s, deposit %s", v, dp)
	}
	if f, _ = svc.Funding(ctx, o.ID); f.Available.String() != "700000000" {
		t.Fatalf("available %s after the sweep; sweeping must not change what is owed", f.Available)
	}
	tb, err := ledger.TrialBalance(ctx, r.pool)
	if err != nil {
		t.Fatal(err)
	}
	for asset, v := range tb {
		if !v.IsZero() {
			t.Fatalf("trial balance %s = %s", asset, v)
		}
	}
}
