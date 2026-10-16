package rates_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/rates"
)

func serve(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

const sample = `{"result":"success","provider":"https://www.exchangerate-api.com","time_last_update_unix":1760572951,"base_code":"USD","rates":{"USD":1,"EUR":0.8611,"NGN":1530.947312}}`

func TestOpenERAPI_ReadsNGNExactlyAndCaches(t *testing.T) {
	srv, hits := serve(t, 200, sample)
	p := &rates.OpenERAPI{URL: srv.URL, Client: srv.Client(), CacheFor: time.Hour}
	ctx := context.Background()
	obs, err := p.Mid(ctx, rates.Pair{From: "USDC", To: "NGN"})
	if err != nil {
		t.Fatal(err)
	}
	// 1530.947312 naira → 153094 kobo: truncated, never rounded up.
	if obs.Rate.String() != "153094" || obs.Source != "ExchangeRate-API" || !obs.ObservedAt.Equal(time.Unix(1760572951, 0)) {
		t.Fatalf("observation %+v", obs)
	}
	if _, err := p.Mid(ctx, rates.Pair{From: "USDT", To: "NGN"}); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatalf("%d requests; the second quote should come from the cache", hits.Load())
	}
}

func TestOpenERAPI_FailuresAreNotRates(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
	}{
		"server error": {500, `{}`},
		"error result": {200, `{"result":"error","error-type":"quota-reached"}`},
		"other base":   {200, `{"result":"success","base_code":"EUR","time_last_update_unix":1,"rates":{"NGN":1700}}`},
		"no NGN":       {200, `{"result":"success","base_code":"USD","time_last_update_unix":1,"rates":{"EUR":0.9}}`},
		"zero NGN":     {200, `{"result":"success","base_code":"USD","time_last_update_unix":1,"rates":{"NGN":0}}`},
		"not JSON":     {200, `<html>`},
		"no timestamp": {200, `{"result":"success","base_code":"USD","rates":{"NGN":1530}}`},
	}
	for name, c := range cases {
		srv, _ := serve(t, c.status, c.body)
		p := &rates.OpenERAPI{URL: srv.URL, Client: srv.Client(), CacheFor: time.Hour}
		// Through the Quoter, every failure is ErrUnavailable: no quote.
		q := &rates.Quoter{Provider: p, TTL: time.Hour}
		if _, err := q.Quote(context.Background(), rates.Pair{From: "USDC", To: "NGN"}); !errors.Is(err, rates.ErrUnavailable) {
			t.Errorf("%s: err = %v, want ErrUnavailable", name, err)
		}
	}
	p := &rates.OpenERAPI{}
	if _, err := p.Mid(context.Background(), rates.Pair{From: "BTC", To: "NGN"}); !errors.Is(err, rates.ErrUnsupportedPair) {
		t.Fatalf("BTC/NGN: %v, want ErrUnsupportedPair", err)
	}
}

// TestLive_OpenERAPI asks the real endpoint. It runs only with
// WAYBILL_LIVE_RATES=1, so CI never depends on it.
func TestLive_OpenERAPI(t *testing.T) {
	if os.Getenv("WAYBILL_LIVE_RATES") != "1" {
		t.Skip("set WAYBILL_LIVE_RATES=1 to ask ExchangeRate-API")
	}
	p := &rates.OpenERAPI{Client: &http.Client{Timeout: 10 * time.Second}, CacheFor: time.Hour}
	obs, err := p.Mid(context.Background(), rates.Pair{From: "USDC", To: "NGN"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("USDC/NGN %s kobo, observed %s", obs.Rate, obs.ObservedAt)
	if obs.Rate.Cmp(money.FromInt64(10_000)) < 0 || time.Since(obs.ObservedAt) > 48*time.Hour {
		t.Fatalf("implausible: %s kobo per USDC observed %s", obs.Rate, obs.ObservedAt)
	}
}
