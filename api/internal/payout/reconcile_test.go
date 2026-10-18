package payout_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/paystack"
	"github.com/JoshBlazer/waybill/api/internal/reconcile"
)

// providerView is what the provider says about every transfer and charge
// when reconciliation asks.
type providerView struct {
	status string
	kobo   int64
}

func (p providerView) Verify(_ context.Context, ref string) (paystack.TransferStatus, error) {
	return paystack.TransferStatus{Status: p.status, Reference: ref, Amount: money.FromInt64(p.kobo)}, nil
}

func (p providerView) VerifyTransaction(context.Context, string) (paystack.ChargeStatus, error) {
	return paystack.ChargeStatus{}, paystack.ErrNotFound
}

// sentPayout runs one naira payout of ₦155,000.00 to succeeded.
func sentPayout(t *testing.T) *rig {
	t.Helper()
	r := setup(t)
	r.paidOnChain(t, 100_000_000, time.Now())
	r.ps.verify = []string{"success"}
	r.tick(t)
	r.tick(t)
	if p := r.payout(t); p.State != "succeeded" {
		t.Fatalf("payout %s, want succeeded", p.State)
	}
	return r
}

func providerCheck(t *testing.T, rep reconcile.Report) reconcile.Check {
	t.Helper()
	for _, c := range rep.Checks {
		if c.ID == "R5" {
			return c
		}
	}
	t.Fatal("no provider check in the report")
	return reconcile.Check{}
}

// R5: a succeeded payout the provider confirms for the same amount is
// clean; the ledger checks pass too.
func TestReconcile_ProviderAgreesWithSucceededPayout(t *testing.T) {
	r := sentPayout(t)
	rec := &reconcile.Reconciler{Pool: r.pool, Provider: providerView{status: "success", kobo: 15_500_000}}
	rep, err := rec.Run(context.Background(), reconcile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := providerCheck(t, rep)
	if c.Status != reconcile.OK || c.Checked != 1 || rep.Differences != 0 {
		t.Fatalf("provider check %+v, %d differences; want one record checked, clean", c, rep.Differences)
	}
	if rep.Checks[0].ID != "R1" || rep.Checks[0].Status != reconcile.OK {
		t.Fatalf("ledger check %+v", rep.Checks[0])
	}
}

// R5: the provider reporting a different amount, or a failure, for a payout
// recorded as succeeded is a difference straight away: records do not
// settle with time.
func TestReconcile_ProviderDisagreementIsADifference(t *testing.T) {
	for name, view := range map[string]providerView{
		"different amount": {status: "success", kobo: 15_400_000},
		"failed":           {status: "failed", kobo: 15_500_000},
	} {
		t.Run(name, func(t *testing.T) {
			r := sentPayout(t)
			rec := &reconcile.Reconciler{Pool: r.pool, Provider: view}
			rep, err := rec.Run(context.Background(), reconcile.Options{})
			if err != nil {
				t.Fatal(err)
			}
			c := providerCheck(t, rep)
			if c.Status != reconcile.Difference || rep.Differences != 1 || !strings.Contains(c.Problems[0], "payout ") {
				t.Fatalf("provider check %+v, %d differences; want one difference", c, rep.Differences)
			}
		})
	}
}

// Without a provider, R5 is skipped and says so; it is never reported as
// passed.
func TestReconcile_NoProviderIsSkipped(t *testing.T) {
	r := sentPayout(t)
	rep, err := (&reconcile.Reconciler{Pool: r.pool}).Run(context.Background(), reconcile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if c := providerCheck(t, rep); c.Status != reconcile.Skipped {
		t.Fatalf("provider check %s, want skipped", c.Status)
	}
}
