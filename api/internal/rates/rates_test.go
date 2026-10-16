package rates_test

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/rates"
)

var usdcNGN = rates.Pair{From: "USDC", To: "NGN"}

func TestParseRate(t *testing.T) {
	cases := map[string]string{
		"1530.94":     "153094",
		"1530.9":      "153090",
		"1530":        "153000",
		"1530.949999": "153094", // truncated, never rounded up
		"0.01":        "1",
		"1530.940000": "153094",
	}
	for in, want := range cases {
		got, err := rates.ParseRate(in, money.NGN)
		if err != nil || got.String() != want {
			t.Errorf("ParseRate(%q) = %s, %v; want %s", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "0", "0.00", "-1530", "1e3", "abc", "1,530.94", "0.001"} {
		if _, err := rates.ParseRate(bad, money.NGN); err == nil {
			t.Errorf("ParseRate(%q) succeeded; want an error", bad)
		}
	}
}

func TestConvert(t *testing.T) {
	rate := money.FromInt64(153094) // ₦1,530.94 per USDC
	cases := []struct{ usdc, kobo int64 }{
		{1_000_000, 153094},     // 1 USDC
		{100_000_000, 15309400}, // 100 USDC = ₦153,094.00
		{1, 0},                  // 0.000001 USDC is less than a kobo
		{7, 1},                  // 7 micro-USDC × 153094 / 10^6 = 1.07 kobo, rounded down
		{125_500_000, 19213297}, // 125.50 USDC = 19,213,297 kobo (₦192,132.97), remainder dropped
	}
	for _, c := range cases {
		if got := rates.Convert(money.FromInt64(c.usdc), money.USDC, rate); got.String() != big.NewInt(c.kobo).String() {
			t.Errorf("Convert(%d micro-USDC) = %s kobo, want %d", c.usdc, got, c.kobo)
		}
	}
}

// Converting never pays more than the exact product, by less than one kobo,
// and a larger amount never converts to less.
func TestConvert_RoundsDownByLessThanOneMinorUnit(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		a := rapid.Int64Range(0, 1_000_000_000_000).Draw(t, "microUSDC")
		r := rapid.Int64Range(1, 10_000_000).Draw(t, "koboPerUSDC")
		got := rates.Convert(money.FromInt64(a), money.USDC, money.FromInt64(r)).Big()
		exact := new(big.Int).Mul(big.NewInt(a), big.NewInt(r)) // kobo × 10^6
		lo := new(big.Int).Mul(got, big.NewInt(1_000_000))
		hi := new(big.Int).Add(lo, big.NewInt(1_000_000))
		if lo.Cmp(exact) > 0 || hi.Cmp(exact) <= 0 {
			t.Fatalf("Convert(%d, %d) = %s kobo; exact is %s / 10^6", a, r, got, exact)
		}
		more := rates.Convert(money.FromInt64(a+1), money.USDC, money.FromInt64(r)).Big()
		if more.Cmp(got) < 0 {
			t.Fatalf("a larger amount converted to less: %s < %s", more, got)
		}
	})
}

func TestQuoter_AppliesSpreadAndExpiry(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	q := &rates.Quoter{
		Provider:  &rates.Fake{Rates: map[rates.Pair]money.Amount{usdcNGN: money.FromInt64(153094)}, Now: func() time.Time { return now }},
		SpreadBps: 150, TTL: 48 * time.Hour, MaxAge: 26 * time.Hour, Now: func() time.Time { return now },
	}
	got, err := q.Quote(context.Background(), usdcNGN)
	if err != nil {
		t.Fatal(err)
	}
	// 153094 × (10000 − 150) / 10000 = 150797.59, rounded down.
	if got.Mid.String() != "153094" || got.Rate.String() != "150797" || !got.ExpiresAt.Equal(now.Add(48*time.Hour)) || got.Source != "fake" {
		t.Fatalf("quote %+v", got)
	}
}

func TestQuoter_NoQuoteWhenTheRateCannotBeFound(t *testing.T) {
	now := time.Now()
	q := &rates.Quoter{Provider: &rates.Fake{Err: errors.New("dial tcp: timeout")}, TTL: time.Hour, MaxAge: time.Hour}
	if _, err := q.Quote(context.Background(), usdcNGN); !errors.Is(err, rates.ErrUnavailable) {
		t.Fatalf("provider down: err = %v, want ErrUnavailable", err)
	}
	stale := &rates.Quoter{
		Provider: &rates.Fake{Rates: map[rates.Pair]money.Amount{usdcNGN: money.FromInt64(1)}, Now: func() time.Time { return now.Add(-30 * time.Hour) }},
		TTL:      time.Hour, MaxAge: 26 * time.Hour, Now: func() time.Time { return now },
	}
	if _, err := stale.Quote(context.Background(), usdcNGN); !errors.Is(err, rates.ErrUnavailable) {
		t.Fatalf("30-hour-old rate: err = %v, want ErrUnavailable", err)
	}
	unknown := &rates.Quoter{Provider: &rates.Fake{}, TTL: time.Hour}
	if _, err := unknown.Quote(context.Background(), usdcNGN); !errors.Is(err, rates.ErrUnsupportedPair) {
		t.Fatalf("unknown pair: err = %v, want ErrUnsupportedPair", err)
	}
	for _, bps := range []int64{-1, 10_000} {
		bad := &rates.Quoter{Provider: &rates.Fake{}, SpreadBps: bps}
		if _, err := bad.Quote(context.Background(), usdcNGN); err == nil {
			t.Fatalf("spread %d accepted", bps)
		}
	}
}
