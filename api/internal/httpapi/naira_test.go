package httpapi_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/JoshBlazer/waybill/api/internal/bankverify"
)

func setAccount(t *testing.T, e env, bank, number string) (int, map[string]any) {
	t.Helper()
	return e.call(t, e.key, http.MethodPut, "/v1/me/bank-account", "", `{"bankCode":"`+bank+`","accountNumber":"`+number+`"}`)
}

func parseTime(t *testing.T, v any) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, v.(string))
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func TestBanks_List(t *testing.T) {
	e := setup(t)
	code, body := e.call(t, e.key, http.MethodGet, "/v1/banks", "", "")
	if code != http.StatusOK || len(items(body)) != len(bankverify.FakeBanks) {
		t.Fatalf("banks: %d %v", code, body)
	}
}

func TestBankAccount_VerifiedSaveAndCoolingOff(t *testing.T) {
	e := setup(t)
	if code, _ := e.call(t, e.key, http.MethodGet, "/v1/me/bank-account", "", ""); code != http.StatusNotFound {
		t.Fatalf("before saving: %d, want 404", code)
	}
	before := time.Now()
	code, first := setAccount(t, e, "058", "0123456789")
	if code != http.StatusOK || first["accountNumberLast4"] != "6789" || first["accountName"] != "OKAFOR ADAEZE CHIOMA" || first["bankName"] != "Guaranty Trust Bank" {
		t.Fatalf("first account: %d %v", code, first)
	}
	if allowed := parseTime(t, first["payoutsAllowedFrom"]); allowed.After(time.Now()) || allowed.Before(before.Add(-time.Second)) {
		t.Fatalf("the first account must receive payouts at once; allowed from %v", allowed)
	}
	if _, ok := first["accountNumber"]; ok {
		t.Fatal("the full account number was returned")
	}

	// Changing it: payouts wait 24 hours.
	code, changed := setAccount(t, e, "057", "1111111111")
	if code != http.StatusOK {
		t.Fatalf("change: %d %v", code, changed)
	}
	if wait := time.Until(parseTime(t, changed["payoutsAllowedFrom"])); wait < 23*time.Hour || wait > 25*time.Hour {
		t.Fatalf("a changed account must wait about 24 h; waits %v", wait)
	}
	// Saving the same account again does not restart or end the wait.
	_, again := setAccount(t, e, "057", "1111111111")
	if again["payoutsAllowedFrom"] != changed["payoutsAllowedFrom"] {
		t.Fatalf("re-saving moved payoutsAllowedFrom from %v to %v", changed["payoutsAllowedFrom"], again["payoutsAllowedFrom"])
	}
	if code, got := e.call(t, e.key, http.MethodGet, "/v1/me/bank-account", "", ""); code != http.StatusOK || got["accountNumberLast4"] != "1111" {
		t.Fatalf("get: %d %v", code, got)
	}
}

func TestBankAccount_RefusalsSaveNothing(t *testing.T) {
	e := setup(t)
	cases := []struct {
		name, bank, number string
		want               int
		problem            string
	}{
		{"someone else's account", "057", "2222222222", http.StatusUnprocessableEntity, "name-mismatch"},
		{"no such account", "058", "9999999999", http.StatusUnprocessableEntity, "account-not-found"},
		{"unknown bank", "999", "0123456789", http.StatusUnprocessableEntity, "unknown-bank"},
		{"nine digits", "058", "012345678", http.StatusBadRequest, "bad-request"},
	}
	for _, c := range cases {
		code, body := setAccount(t, e, c.bank, c.number)
		if code != c.want || !strings.HasSuffix(body["type"].(string), "/"+c.problem) {
			t.Errorf("%s: %d %v; want %d %s", c.name, code, body, c.want, c.problem)
		}
	}
	// The bank cannot be reached: "could not verify" is never "verified".
	e.resolver.Err = bankverify.ErrUnavailable
	if code, body := setAccount(t, e, "058", "0123456789"); code != http.StatusServiceUnavailable {
		t.Fatalf("bank unreachable: %d %v, want 503", code, body)
	}
	var n int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM contractor_bank_accounts`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d accounts saved by refused requests", n)
	}
	// The mismatch message shows the bank's name, so the contractor can fix it.
	e.resolver.Err = nil
	_, body := setAccount(t, e, "057", "2222222222")
	if !strings.Contains(body["detail"].(string), "BELLO TUNDE") {
		t.Fatalf("mismatch detail %v does not show the bank's name", body["detail"])
	}
}

const nairaBody = `{"description":"Design work, September","amount":"125.50","asset":"USDC","payout":"naira"}`

func TestCreateInvoice_NairaLocksRate(t *testing.T) {
	e := setup(t)
	code, body := e.create(t, "n0", nairaBody)
	if code != http.StatusUnprocessableEntity || !strings.HasSuffix(body["type"].(string), "/no-bank-account") {
		t.Fatalf("without a bank account: %d %v; want 422 no-bank-account", code, body)
	}
	setAccount(t, e, "058", "0123456789")

	code, body = e.create(t, "n1", nairaBody)
	if code != http.StatusCreated {
		t.Fatalf("naira invoice: %d %v", code, body)
	}
	q := body["quote"].(map[string]any)
	rate := q["rate"].(map[string]any)
	naira := q["nairaAmount"].(map[string]any)
	// Mid ₦1,550.00 less 1.5 % = ₦1,526.75 per USDC; 125.50 × 1,526.75 =
	// ₦191,607.125, rounded down to the kobo.
	if rate["minor"] != "152675" || rate["asset"] != "NGN" || rate["scale"].(float64) != 2 ||
		naira["minor"] != "19160712" || q["spreadBps"].(float64) != 150 || q["source"] != "fake" {
		t.Fatalf("quote %v", q)
	}
	if hold := parseTime(t, q["expiresAt"]).Sub(parseTime(t, q["quotedAt"])); hold != 48*time.Hour {
		t.Fatalf("rate held for %v, want 48h", hold)
	}

	// The same invoice, read back, carries the same quote.
	code, got := e.call(t, e.key, http.MethodGet, "/v1/invoices/"+body["id"].(string), "", "")
	if code != http.StatusOK || got["quote"].(map[string]any)["nairaAmount"].(map[string]any)["minor"] != "19160712" {
		t.Fatalf("get invoice: %d %v", code, got)
	}
	// A hold invoice has no quote.
	_, hold := e.create(t, "h1", validBody)
	if _, ok := hold["quote"]; ok {
		t.Fatalf("hold invoice has a quote: %v", hold["quote"])
	}
}

// No rate, no naira invoice, and the request can be retried with the same
// key once rates are back.
func TestCreateInvoice_NairaWithoutARateIs503(t *testing.T) {
	e := setup(t)
	setAccount(t, e, "058", "0123456789")
	e.rates.Err = errors.New("dial tcp: i/o timeout")
	code, body := e.create(t, "n1", nairaBody)
	if code != http.StatusServiceUnavailable || !strings.HasSuffix(body["type"].(string), "/rate-unavailable") {
		t.Fatalf("rate down: %d %v; want 503 rate-unavailable", code, body)
	}
	if n := countInvoices(t, e.pool); n != 0 {
		t.Fatalf("%d invoices created without a rate", n)
	}
	e.rates.Err = nil
	if code, body := e.create(t, "n1", nairaBody); code != http.StatusCreated {
		t.Fatalf("retry with the same key after rates recovered: %d %v", code, body)
	}
}

func TestGetInvoice_OnlyTheContractors(t *testing.T) {
	e := setup(t)
	_, inv := e.create(t, "k1", validBody)
	other := e.otherContractorKey(t)
	if code, _ := e.call(t, other, http.MethodGet, "/v1/invoices/"+inv["id"].(string), "", ""); code != http.StatusNotFound {
		t.Fatalf("another contractor's invoice: %d, want 404", code)
	}
	if code, _ := e.call(t, e.key, http.MethodGet, "/v1/invoices/00000000-0000-4000-8000-000000000000", "", ""); code != http.StatusNotFound {
		t.Fatalf("no such invoice: %d, want 404", code)
	}
}

func (e env) underpay(t *testing.T, id string, paymentState string) {
	t.Helper()
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `UPDATE invoices SET state = 'underpaid' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO payments (network, tx_hash, log_index, block_number, block_hash, token_address, from_address,
		                      to_address, invoice_id, asset_code, amount, state)
		SELECT network, '0x' || repeat('a', 64), 0, 10, '0x' || repeat('b', 64), '0x' || repeat('c', 40),
		       '0x' || repeat('d', 40), address, invoice_id, 'USDC', 60000000, $2
		FROM deposit_addresses WHERE invoice_id = $1`, id, paymentState); err != nil {
		t.Fatal(err)
	}
}

func TestAcceptShortfall_SettlesAFinalUnderpayment(t *testing.T) {
	e := setup(t)
	_, inv := e.create(t, "k1", validBody)
	id := inv["id"].(string)
	path := "/v1/invoices/" + id + "/accept-shortfall"

	if code, _ := e.call(t, e.key, http.MethodPost, path, "a0", ""); code != http.StatusConflict {
		t.Fatalf("an open invoice: %d, want 409", code)
	}
	e.underpay(t, id, "final")
	code, got := e.call(t, e.key, http.MethodPost, path, "a1", "")
	if code != http.StatusOK || got["state"] != "settled" {
		t.Fatalf("accept: %d %v; want settled (the payment is already final)", code, got)
	}
	// A retry with the same key returns the same answer; a new key is a 409.
	if code, again := e.call(t, e.key, http.MethodPost, path, "a1", ""); code != http.StatusOK || again["state"] != "settled" {
		t.Fatalf("retry: %d %v", code, again)
	}
	if code, _ := e.call(t, e.key, http.MethodPost, path, "a2", ""); code != http.StatusConflict {
		t.Fatalf("accepting a settled invoice: %d, want 409", code)
	}
	var types []string
	rows, _ := e.pool.Query(context.Background(), `SELECT type FROM invoice_events WHERE invoice_id = $1 ORDER BY id`, id)
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		types = append(types, s)
	}
	rows.Close()
	if strings.Join(types, ",") != "created,underpayment_accepted,payment_final" {
		t.Fatalf("events %v", types)
	}
	if code, _ := e.call(t, e.otherContractorKey(t), http.MethodPost, path, "a3", ""); code != http.StatusNotFound {
		t.Fatalf("another contractor: %d, want 404", code)
	}
}

// Accepted while the payment is only confirmed: the invoice becomes
// confirmed and the watcher settles it at finality.
func TestAcceptShortfall_WaitsForFinality(t *testing.T) {
	e := setup(t)
	_, inv := e.create(t, "k1", validBody)
	id := inv["id"].(string)
	e.underpay(t, id, "confirmed")
	code, got := e.call(t, e.key, http.MethodPost, "/v1/invoices/"+id+"/accept-shortfall", "a1", "")
	if code != http.StatusOK || got["state"] != "confirmed" {
		t.Fatalf("accept: %d %v; want confirmed until the payment is final", code, got)
	}
}

func paystackSign(body []byte) string {
	m := hmac.New(sha512.New, []byte(testPaystackKey))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

func (e env) paystackPost(t *testing.T, body []byte, sig string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/v1/providers/paystack/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-paystack-signature", sig)
	code, _ := do(t, req)
	return code
}

// TestPaystackWebhook_ReplayProcessedOnce is the storage half of I14: a
// signed event is stored raw once, however often it is delivered, and a
// forged one is refused and not stored.
func TestPaystackWebhook_ReplayProcessedOnce(t *testing.T) {
	e := setup(t)
	// Key order and spacing as Paystack sends them: the signature covers
	// these exact bytes.
	body := []byte(`{"event":"transfer.success","data":{"amount":15309400, "id":52107,"reference":"payout-6f1c2a9e0b7d4c3a","status":"success"}}`)
	for range 3 {
		if code := e.paystackPost(t, body, paystackSign(body)); code != http.StatusOK {
			t.Fatalf("signed event: %d, want 200", code)
		}
	}
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM provider_events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	var id string
	var raw []byte
	if err := e.pool.QueryRow(context.Background(), `SELECT event_id, raw FROM provider_events`).Scan(&id, &raw); err != nil {
		t.Fatal(err)
	}
	if n != 1 || id != "transfer.success:52107" || !bytes.Equal(raw, body) {
		t.Fatalf("%d stored, id %q, raw intact %v; want one, exactly as received", n, id, bytes.Equal(raw, body))
	}
}

func TestPaystackWebhook_BadSignatureRejected(t *testing.T) {
	e := setup(t)
	body := []byte(`{"event":"transfer.success","data":{"id":1}}`)
	forged := []string{"deadbeef", paystackSign([]byte(`{"event":"transfer.success","data":{"id":2}}`))}
	for _, sig := range forged {
		if code := e.paystackPost(t, body, sig); code != http.StatusUnauthorized {
			t.Errorf("signature %q: %d, want 401", sig, code)
		}
	}
	// No signature header at all fails the required-parameter check.
	if code := e.paystackPost(t, body, ""); code != http.StatusBadRequest {
		t.Errorf("no signature: %d, want 400", code)
	}
	e.server.Paystack = nil // no key configured: nothing can be verified
	if code := e.paystackPost(t, body, paystackSign(body)); code != http.StatusUnauthorized {
		t.Fatalf("without a key: %d, want 401", code)
	}
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM provider_events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d events stored from forged requests", n)
	}
}
