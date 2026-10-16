package paystack_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JoshBlazer/waybill/api/internal/bankverify"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/paystack"
)

// A made-up test key: the shape of a Paystack test key, not a real one.
const testKey = "sk_test_waybill_fake_key_for_unit_tests_only"

type request struct {
	method, path, auth string
	body               map[string]any
}

// fakePaystack answers each request with the next scripted reply and
// records what it was sent.
type fakePaystack struct {
	mu      sync.Mutex
	replies []reply
	got     []request
	srv     *httptest.Server
}

type reply struct {
	status int
	body   string
	delay  time.Duration
}

func newFake(t *testing.T, replies ...reply) (*fakePaystack, *paystack.Client) {
	t.Helper()
	f := &fakePaystack{replies: replies}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.got = append(f.got, request{r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"), body})
		rep := reply{status: 500, body: "{}"}
		if len(f.replies) > 0 {
			rep, f.replies = f.replies[0], f.replies[1:]
		}
		f.mu.Unlock()
		time.Sleep(rep.delay)
		w.WriteHeader(rep.status)
		_, _ = io.WriteString(w, rep.body)
	}))
	t.Cleanup(f.srv.Close)
	c, err := paystack.New(testKey, f.srv.URL, 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

const ref = "payout-6f1c2a9e0b7d4c3a"

var acct = bankverify.Account{BankCode: "058", AccountNumber: "0123456789"}

func TestNew_RefusesLiveKeys(t *testing.T) {
	for _, k := range []string{"sk_live_abc", "pk_test_abc", ""} {
		if _, err := paystack.New(k, "", time.Second); !errors.Is(err, paystack.ErrLiveKey) {
			t.Errorf("New(%q) = %v, want ErrLiveKey", k, err)
		}
	}
}

func TestResolveAccount(t *testing.T) {
	f, c := newFake(t,
		reply{200, `{"status":true,"message":"Account number resolved","data":{"account_number":"0123456789","account_name":"OKAFOR ADAEZE CHIOMA","bank_id":9}}`, 0},
		reply{422, `{"status":false,"message":"Could not resolve account name. Check parameters or try again."}`, 0},
		reply{503, `upstream down`, 0},
	)
	ctx := context.Background()
	name, err := c.ResolveAccount(ctx, acct)
	if err != nil || name != "OKAFOR ADAEZE CHIOMA" {
		t.Fatalf("resolved: %q, %v", name, err)
	}
	if f.got[0].method != "GET" || f.got[0].path != "/bank/resolve?account_number=0123456789&bank_code=058" || f.got[0].auth != "Bearer "+testKey {
		t.Fatalf("request %+v", f.got[0])
	}
	if _, err := c.ResolveAccount(ctx, acct); !errors.Is(err, bankverify.ErrAccountNotFound) {
		t.Fatalf("refused: %v, want ErrAccountNotFound", err)
	}
	if _, err := c.ResolveAccount(ctx, acct); !errors.Is(err, bankverify.ErrUnavailable) || errors.Is(err, bankverify.ErrAccountNotFound) {
		t.Fatalf("503: %v, want ErrUnavailable only", err)
	}
	// The client satisfies bankverify.Resolver, so verification can use it.
	var _ bankverify.Resolver = c
}

func TestCreateRecipient(t *testing.T) {
	f, c := newFake(t, reply{201, `{"status":true,"message":"Transfer recipient created successfully","data":{"recipient_code":"RCP_t0ya41mp35flk40","type":"nuban"}}`, 0})
	code, err := c.CreateRecipient(context.Background(), "Adaeze Okafor", acct)
	if err != nil || code != "RCP_t0ya41mp35flk40" {
		t.Fatalf("CreateRecipient = %q, %v", code, err)
	}
	b := f.got[0].body
	if f.got[0].path != "/transferrecipient" || b["type"] != "nuban" || b["account_number"] != "0123456789" || b["bank_code"] != "058" || b["currency"] != "NGN" {
		t.Fatalf("request %+v", f.got[0])
	}
}

func TestInitiate_ClassifiesEveryAnswer(t *testing.T) {
	ctx := context.Background()
	tr := paystack.Transfer{Reference: ref, Amount: money.FromInt64(15309400), Recipient: "RCP_t0ya41mp35flk40", Reason: "Invoice WB-7KQ4"}

	f, c := newFake(t, reply{200, `{"status":true,"message":"Transfer has been queued","data":{"reference":"` + ref + `","status":"pending","transfer_code":"TRF_1ptvuv321ahaa7q","amount":15309400}}`, 0})
	got, err := c.Initiate(ctx, tr)
	if err != nil || got.Status != "pending" || got.TransferCode != "TRF_1ptvuv321ahaa7q" || got.Amount.String() != "15309400" {
		t.Fatalf("accepted: %+v, %v", got, err)
	}
	// Amount goes out as an integer number of kobo, never a decimal.
	if raw, _ := json.Marshal(f.got[0].body["amount"]); string(raw) != "15309400" || f.got[0].body["reference"] != ref || f.got[0].body["source"] != "balance" {
		t.Fatalf("request body %v", f.got[0].body)
	}

	cases := []struct {
		name string
		rep  reply
		want error
	}{
		{"refused (insufficient balance)", reply{400, `{"status":false,"message":"Your balance is not enough to fulfil this request"}`, 0}, paystack.ErrRejected},
		{"server error", reply{500, `{"status":false,"message":"oops"}`, 0}, paystack.ErrUnknown},
		{"bad gateway, html body", reply{502, `<html>bad gateway</html>`, 0}, paystack.ErrUnknown},
		{"rate limited", reply{429, `{"status":false,"message":"Too many requests"}`, 0}, paystack.ErrUnknown},
		{"timeout", reply{200, `{"status":true}`, time.Second}, paystack.ErrUnknown},
		{"2xx with unreadable body", reply{200, `not json`, 0}, paystack.ErrUnknown},
		{"answer about another reference", reply{200, `{"status":true,"data":{"reference":"payout-someone-else-000","status":"pending"}}`, 0}, paystack.ErrUnknown},
	}
	for _, cs := range cases {
		_, c := newFake(t, cs.rep)
		_, err := c.Initiate(ctx, tr)
		if !errors.Is(err, cs.want) {
			t.Errorf("%s: err = %v, want %v", cs.name, err, cs.want)
		}
		if errors.Is(err, paystack.ErrUnknown) && errors.Is(err, paystack.ErrRejected) {
			t.Errorf("%s: both unknown and rejected", cs.name)
		}
	}
}

func TestInitiate_RefusesBadInputWithoutCalling(t *testing.T) {
	f, c := newFake(t)
	for _, tr := range []paystack.Transfer{
		{Reference: "Payout-UPPERCASE-0001", Amount: money.FromInt64(1), Recipient: "RCP_x"},
		{Reference: "short", Amount: money.FromInt64(1), Recipient: "RCP_x"},
		{Reference: ref, Amount: money.FromInt64(0), Recipient: "RCP_x"},
	} {
		if _, err := c.Initiate(context.Background(), tr); err == nil {
			t.Errorf("%+v accepted", tr)
		}
	}
	if len(f.got) != 0 {
		t.Fatalf("%d requests sent for invalid transfers", len(f.got))
	}
}

func TestVerify(t *testing.T) {
	ctx := context.Background()
	f, c := newFake(t,
		reply{200, `{"status":true,"message":"Transfer retrieved","data":{"reference":"` + ref + `","status":"success","transfer_code":"TRF_x","amount":15309400}}`, 0},
		reply{400, `{"status":false,"message":"Transfer not found"}`, 0},
		reply{404, `{"status":false,"message":"Transfer not found"}`, 0},
		reply{504, ``, 0},
	)
	got, err := c.Verify(ctx, ref)
	if err != nil || got.Status != "success" || f.got[0].path != "/transfer/verify/"+ref {
		t.Fatalf("found: %+v, %v (path %s)", got, err, f.got[0].path)
	}
	for i := range 2 {
		if _, err := c.Verify(ctx, ref); !errors.Is(err, paystack.ErrNotFound) {
			t.Fatalf("not found (form %d): %v, want ErrNotFound", i, err)
		}
	}
	if _, err := c.Verify(ctx, ref); !errors.Is(err, paystack.ErrUnknown) || errors.Is(err, paystack.ErrNotFound) {
		t.Fatalf("gateway timeout: %v; must be unknown, never not-found", err)
	}
}

func sign(key string, body []byte) string {
	m := hmac.New(sha512.New, []byte(key))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

func TestVerifyWebhook(t *testing.T) {
	_, c := newFake(t)
	body := []byte(`{"event":"transfer.success","data":{"reference":"` + ref + `"}}`)
	if err := c.VerifyWebhook(body, sign(testKey, body)); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if err := c.VerifyWebhook(body, strings.ToUpper(sign(testKey, body))); err != nil {
		t.Fatalf("upper-case hex rejected: %v", err)
	}
	bad := map[string]string{
		"tampered body": sign(testKey, append(bytes.Clone(body), ' ')),
		"other key":     sign("sk_test_someone_else", body),
		"empty":         "",
		"garbage":       "not-a-signature",
	}
	for name, sig := range bad {
		if err := c.VerifyWebhook(body, sig); !errors.Is(err, paystack.ErrBadSignature) {
			t.Errorf("%s: err = %v, want ErrBadSignature", name, err)
		}
	}
}

func TestClient_NeverShowsItsKey(t *testing.T) {
	_, c := newFake(t, reply{500, `{}`, 0})
	_, err := c.Initiate(context.Background(), paystack.Transfer{Reference: ref, Amount: money.FromInt64(1), Recipient: "RCP_x"})
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("client", "c", c, "err", err)
	out := fmt.Sprintf("%v %+v %s", c, c, err) + logs.String()
	if strings.Contains(out, testKey) {
		t.Fatal("the secret key appears in output or logs")
	}
}
