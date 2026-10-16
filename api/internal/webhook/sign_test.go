package webhook_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/JoshBlazer/waybill/api/internal/statemachine"
	"github.com/JoshBlazer/waybill/api/internal/webhook"
)

// TestWebhook_SignatureVerifies is invariant I13's signing half: a receiver
// following the documented scheme accepts exactly what Waybill signed.
func TestWebhook_SignatureVerifies(t *testing.T) {
	secret := webhook.NewSecret()
	body := []byte(`{"id":"evt_01J9ZQ7W3X4Y5Z6A7B8C9D0E1F","type":"invoice.settled"}`)
	now := time.Unix(1_760_000_000, 0)
	header := webhook.Sign(secret, now, body)

	if !regexp.MustCompile(`^t=1760000000,v1=[0-9a-f]{64}$`).MatchString(header) {
		t.Fatalf("header %q does not match the documented format", header)
	}
	if err := webhook.Verify(secret, header, body, now.Add(time.Minute)); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	bad := map[string]func() error{
		"tampered body":        func() error { return webhook.Verify(secret, header, append(body, ' '), now) },
		"wrong secret":         func() error { return webhook.Verify(webhook.NewSecret(), header, body, now) },
		"replayed after 6 min": func() error { return webhook.Verify(secret, header, body, now.Add(6*time.Minute)) },
		"from 6 min in future": func() error { return webhook.Verify(secret, header, body, now.Add(-6*time.Minute)) },
		"timestamp changed": func() error {
			return webhook.Verify(secret, strings.Replace(header, "t=1760000000", "t=1760000001", 1), body, now)
		},
		"no signature":            func() error { return webhook.Verify(secret, "t=1760000000", body, now) },
		"garbage":                 func() error { return webhook.Verify(secret, "nonsense", body, now) },
		"signature for elsewhere": func() error { return webhook.Verify(secret, webhook.Sign(secret, now, []byte("{}")), body, now) },
	}
	for name, check := range bad {
		if err := check(); !errors.Is(err, webhook.ErrBadSignature) {
			t.Errorf("%s: err = %v, want ErrBadSignature", name, err)
		}
	}
	// During a secret rotation a sender may include two signatures.
	rotated := header + ",v1=" + strings.Repeat("0", 64)
	if err := webhook.Verify(secret, rotated, body, now); err != nil {
		t.Fatalf("header with an extra v1 rejected: %v", err)
	}
}

func TestNewEventID(t *testing.T) {
	re := regexp.MustCompile(`^evt_[0-9A-HJKMNP-TV-Z]{26}$`)
	a := webhook.NewEventID(time.UnixMilli(1_760_000_000_000))
	b := webhook.NewEventID(time.UnixMilli(1_760_000_000_001))
	if !re.MatchString(a) || !re.MatchString(b) {
		t.Fatalf("ids %q, %q do not match %s", a, b, re)
	}
	if a >= b {
		t.Fatalf("%q was created first but does not sort first", a)
	}
	if c := webhook.NewEventID(time.UnixMilli(1_760_000_000_000)); c == a {
		t.Fatal("two ids in the same millisecond are equal")
	}
}

func TestNewSecret(t *testing.T) {
	re := regexp.MustCompile(`^whsec_test_[0-9A-Za-z]{32}$`)
	seen := map[string]bool{}
	for range 100 {
		s := webhook.NewSecret()
		if !re.MatchString(s) || seen[s] {
			t.Fatalf("secret %q is malformed or repeated", s)
		}
		seen[s] = true
	}
}

func TestCheckURL(t *testing.T) {
	cases := []struct {
		url          string
		allowPrivate bool
		ok           bool
	}{
		{"https://example.com/hooks", false, true},
		{"https://hooks.example.co.uk:8443/x?y=1", false, true},
		{"https://93.184.215.14/hook", false, true},
		{"http://example.com/hooks", false, false},
		{"ftp://example.com/", false, false},
		{"https://user:pass@example.com/", false, false},
		{"https://localhost/hook", false, false},
		{"https://api.localhost/hook", false, false},
		{"https://metadata.google.internal/", false, false},
		{"https://intranet/hook", false, false},
		{"https://127.0.0.1/hook", false, false},
		{"https://10.0.0.5/hook", false, false},
		{"https://172.17.0.1/hook", false, false},
		{"https://192.168.1.1/hook", false, false},
		{"https://169.254.169.254/latest/meta-data", false, false},
		{"https://100.64.0.1/", false, false},
		{"https://[::1]/hook", false, false},
		{"https://[fe80::1]/hook", false, false},
		{"https://[::ffff:127.0.0.1]/hook", false, false},
		{"https://0.0.0.0/", false, false},
		{"/relative", false, false},
		{"not a url", false, false},
		{"http://localhost:9000/hook", true, true},
		{"http://127.0.0.1:9000/hook", true, true},
	}
	for _, c := range cases {
		err := webhook.CheckURL(c.url, c.allowPrivate)
		if (err == nil) != c.ok {
			t.Errorf("CheckURL(%q, allowPrivate=%v) = %v, want ok=%v", c.url, c.allowPrivate, err, c.ok)
		}
		if err != nil && !errors.Is(err, webhook.ErrURLNotAllowed) {
			t.Errorf("CheckURL(%q): %v is not ErrURLNotAllowed", c.url, err)
		}
	}
}

// The registration check sees names; the dialer sees addresses. A name
// that resolves to a private address is refused when connecting, before
// anything is sent.
func TestNewClient_RefusesPrivateAddressesAfterResolution(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer srv.Close()
	url := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1) // a name, resolved by the dialer

	_, err := webhook.NewClient(false, time.Second).Post(url, "application/json", strings.NewReader("{}"))
	if !errors.Is(err, webhook.ErrRefused) || hits != 0 {
		t.Fatalf("err = %v, hits = %d; want ErrRefused and nothing sent", err, hits)
	}
	resp, err := webhook.NewClient(true, time.Second).Post(url, "application/json", strings.NewReader("{}"))
	if err != nil || hits != 1 {
		t.Fatalf("with allowPrivate: err = %v, hits = %d; want one request", err, hits)
	}
	_ = resp.Body.Close()
}

func TestNewClient_DoesNotFollowRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/", http.StatusFound)
	}))
	defer srv.Close()
	resp, err := webhook.NewClient(true, time.Second).Post(srv.URL, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status %d; a redirect must be returned, not followed", resp.StatusCode)
	}
}

func TestEventType(t *testing.T) {
	S := func(s string) statemachine.InvoiceState { return statemachine.InvoiceState(s) }
	cases := []struct {
		from, to string
		ev       statemachine.InvoiceEvent
		want     string
	}{
		{"open", "received", statemachine.EvPaymentDetected, "invoice.received"},
		{"received", "underpaid", statemachine.EvConfirmedShort, "invoice.underpaid"},
		{"received", "overpaid", statemachine.EvConfirmedOver, "invoice.overpaid"},
		{"received", "confirmed", statemachine.EvConfirmedExact, "invoice.confirmed"},
		{"confirmed", "settled", statemachine.EvPaymentFinal, "invoice.settled"},
		{"open", "expired", statemachine.EvExpire, "invoice.expired"},
		{"confirmed", "open", statemachine.EvPaymentReorged, "invoice.reopened"},
		{"received", "received", statemachine.EvPaymentDetected, ""}, // no change, no event
		{"confirmed", "confirmed", statemachine.EvPaymentDetected, ""},
		{"open", "cancelled", statemachine.EvCancel, ""}, // not a published event type
	}
	for _, c := range cases {
		got, ok := webhook.EventType(S(c.from), S(c.to), c.ev)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("%s -> %s on %s = (%q, %v), want %q", c.from, c.to, c.ev, got, ok, c.want)
		}
	}
}

func TestBackoff_RetriesForAboutADay(t *testing.T) {
	var total time.Duration
	for a := 1; a < webhook.MaxAttempts; a++ {
		total += webhook.Backoff(a)
	}
	if total < 23*time.Hour || total > 26*time.Hour {
		t.Fatalf("retries span %v; the API promises about 24 hours", total)
	}
	if webhook.Backoff(1) != 30*time.Second || webhook.Backoff(30) != 4*time.Hour {
		t.Fatalf("Backoff(1) = %v, Backoff(30) = %v", webhook.Backoff(1), webhook.Backoff(30))
	}
}
