package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/JoshBlazer/waybill/api/internal/ledger"
	"github.com/JoshBlazer/waybill/api/internal/money"
)

// credit gives the test contractor available USDC, as a final payment would.
func (e env) credit(t *testing.T, micro int64) {
	t.Helper()
	ctx := context.Background()
	err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		custody, err := ledger.EnsureAccount(ctx, tx, ledger.Account{Code: "custody:vault:evm_31337:USDC", Asset: "USDC", Kind: ledger.KindAsset, Rule: ledger.NonNegative})
		if err != nil {
			return err
		}
		owed, err := ledger.EnsureAccount(ctx, tx, ledger.Account{Code: "liability:contractor:" + e.contractor.String() + ":USDC:available", Asset: "USDC", Kind: ledger.KindLiability, Rule: ledger.NonPositive})
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

func minor(v any) string { return v.(map[string]any)["minor"].(string) }

func TestConversion_QuoteAcceptAndBalance(t *testing.T) {
	e := setup(t)
	e.credit(t, 50_000_000)

	if code, body := e.call(t, e.key, http.MethodPost, "/v1/me/conversions", "", `{"amount":"10"}`); code != 422 || body["type"] != "https://waybill.dev/problems/no-bank-account" {
		t.Fatalf("without a bank account: %d %v", code, body)
	}
	setAccount(t, e, "058", "0123456789")
	for _, bad := range []string{`{"amount":"0.5"}`, `{"amount":"ten"}`, `{"amount":"50.000001"}`} {
		if code, _ := e.call(t, e.key, http.MethodPost, "/v1/me/conversions", "", bad); code != 422 {
			t.Fatalf("quote %s: %d, want 422", bad, code)
		}
	}
	code, q := e.call(t, e.key, http.MethodPost, "/v1/me/conversions", "", `{"amount":"10"}`)
	if code != http.StatusCreated || q["state"] != "quoted" || minor(q["naira"]) == "" || q["expiresAt"] == nil {
		t.Fatalf("quote: %d %v", code, q)
	}
	id := q["id"].(string)
	accept := "/v1/me/conversions/" + id + "/accept"

	if code, _ := e.call(t, e.otherContractorKey(t), http.MethodPost, accept, "x1", ""); code != http.StatusNotFound {
		t.Fatalf("another contractor accepting: %d, want 404", code)
	}
	code, got := e.call(t, e.key, http.MethodPost, accept, "a1", "")
	if code != http.StatusOK || got["state"] != "accepted" || got["payout"].(map[string]any)["state"] != "converted" {
		t.Fatalf("accept: %d %v", code, got)
	}
	if code, again := e.call(t, e.key, http.MethodPost, accept, "a1", ""); code != http.StatusOK || again["payout"].(map[string]any)["reference"] != got["payout"].(map[string]any)["reference"] {
		t.Fatalf("same key again: %d %v", code, again)
	}

	code, bal := e.call(t, e.key, http.MethodGet, "/v1/me/balance", "", "")
	if code != http.StatusOK || minor(bal["usdcAvailable"]) != "40000000" || minor(bal["nairaOnItsWay"]) != minor(q["naira"]) {
		t.Fatalf("balance: %d %v", code, bal)
	}
	if list := bal["conversions"].([]any); len(list) != 1 {
		t.Fatalf("conversions on the balance: %v", list)
	}
	if code, one := e.call(t, e.key, http.MethodGet, "/v1/me/conversions/"+id, "", ""); code != http.StatusOK || one["state"] != "accepted" {
		t.Fatalf("get: %d %v", code, one)
	}
}

func TestConversion_NoRateIs503(t *testing.T) {
	e := setup(t)
	e.credit(t, 5_000_000)
	setAccount(t, e, "058", "0123456789")
	e.rates.Err = errors.New("dial tcp: i/o timeout")
	if code, body := e.call(t, e.key, http.MethodPost, "/v1/me/conversions", "", `{"amount":"2"}`); code != http.StatusServiceUnavailable || body["type"] != "https://waybill.dev/problems/rate-unavailable" {
		t.Fatalf("rate down: %d %v", code, body)
	}
}
