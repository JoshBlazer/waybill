// Package rates turns a market rate into a quote a contractor can rely on,
// and converts amounts at it exactly.
//
// A rate is an integer: minor units of the target asset per one whole unit
// of the source asset. For USDC to NGN that is kobo per 1 USDC, so a market
// rate of ₦1,530.94 is 153094. Providers report decimals; ParseRate turns the
// decimal text into that integer without ever passing through a float (I2).
//
// Providers sit behind an interface. Fake serves tests and the local stack;
// the sandbox provider is chosen in a decision record (ADR-039).
package rates

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"

	"github.com/JoshBlazer/waybill/api/internal/money"
)

// ErrUnavailable means the provider could not be asked or did not answer
// usefully. It is "could not find out" (I12): no quote is made, and the
// caller says so rather than guessing a rate.
var ErrUnavailable = errors.New("rates: rate unavailable")

// ErrUnsupportedPair means there is no rate for this pair of assets.
var ErrUnsupportedPair = errors.New("rates: unsupported pair")

// Pair is a conversion direction.
type Pair struct {
	From, To money.AssetCode
}

func (p Pair) String() string { return string(p.From) + "/" + string(p.To) }

// Observation is a market (mid) rate as a provider reported it.
type Observation struct {
	Pair       Pair
	Rate       money.Amount // minor units of To per one whole From
	Source     string       // for attribution, e.g. "ExchangeRate-API"
	ObservedAt time.Time    // when the provider says the rate applied
}

// Provider reports market rates.
type Provider interface {
	Mid(ctx context.Context, p Pair) (Observation, error)
}

// ParseRate converts a provider's decimal rate (for example "1530.94")
// into minor units of to per one whole from, exactly. More decimals than
// to's scale are truncated, never rounded up: a quote never promises more
// than the market rate.
func ParseRate(decimal string, to money.Asset) (money.Amount, error) {
	whole, frac, _ := cutPoint(decimal)
	if len(frac) > int(to.Scale) {
		frac = frac[:to.Scale]
	}
	s := whole
	if frac != "" {
		s += "." + frac
	}
	r, err := money.ParseDecimal(s, to.Scale)
	if err != nil {
		return money.Zero, fmt.Errorf("rates: parse %q: %w", decimal, err)
	}
	if r.Sign() <= 0 {
		return money.Zero, fmt.Errorf("rates: rate %q is not positive", decimal)
	}
	return r, nil
}

func cutPoint(s string) (whole, frac string, ok bool) {
	for i := range len(s) {
		if s[i] == '.' {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

// Convert returns amount of from converted at rate (minor units of to per
// one whole from), rounded down to the minor unit of to. Rounding down means
// a conversion never pays out more than the quote covers; the remainder is
// below one kobo.
func Convert(amount money.Amount, from money.Asset, rate money.Amount) money.Amount {
	n := new(big.Int).Mul(amount.Big(), rate.Big())
	d := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(from.Scale)), nil)
	q, m := new(big.Int).QuoRem(n, d, new(big.Int))
	if m.Sign() < 0 { // QuoRem truncates toward zero; round negatives down too
		q.Sub(q, big.NewInt(1))
	}
	out, err := money.FromBig(q)
	if err != nil {
		panic(err) // the product of two in-range amounts divided by 10^scale stays in range for real inputs
	}
	return out
}

// Quote is a rate Waybill commits to until ExpiresAt.
type Quote struct {
	ID         uuid.UUID
	Pair       Pair
	Mid        money.Amount // the market rate observed
	Rate       money.Amount // what the contractor gets: Mid less the spread, rounded down
	SpreadBps  int64        // basis points taken from the mid rate
	Source     string
	ObservedAt time.Time
	QuotedAt   time.Time
	ExpiresAt  time.Time
}

// Quoter makes quotes from a provider's mid rate.
type Quoter struct {
	Provider  Provider
	SpreadBps int64         // 0 to 10000; e.g. 150 is 1.5 %
	TTL       time.Duration // how long a quote holds
	MaxAge    time.Duration // refuse observations older than this (daily providers: about 26 h)
	Now       func() time.Time
}

func (q *Quoter) now() time.Time {
	if q.Now != nil {
		return q.Now()
	}
	return time.Now()
}

// Quote asks the provider and applies the spread. A provider error, or an
// observation older than MaxAge, is ErrUnavailable: no quote rather than a
// stale one.
func (q *Quoter) Quote(ctx context.Context, p Pair) (Quote, error) {
	if q.SpreadBps < 0 || q.SpreadBps >= 10_000 {
		return Quote{}, fmt.Errorf("rates: spread %d bps out of range", q.SpreadBps)
	}
	obs, err := q.Provider.Mid(ctx, p)
	if err != nil {
		if errors.Is(err, ErrUnsupportedPair) {
			return Quote{}, err
		}
		return Quote{}, fmt.Errorf("%w: %s: %w", ErrUnavailable, p, err)
	}
	now := q.now()
	if q.MaxAge > 0 && now.Sub(obs.ObservedAt) > q.MaxAge {
		return Quote{}, fmt.Errorf("%w: %s rate from %s is older than %s", ErrUnavailable, p, obs.ObservedAt.Format(time.RFC3339), q.MaxAge)
	}
	rate := new(big.Int).Mul(obs.Rate.Big(), big.NewInt(10_000-q.SpreadBps))
	rate.Quo(rate, big.NewInt(10_000))
	r, err := money.FromBig(rate)
	if err != nil {
		return Quote{}, err
	}
	return Quote{
		ID: uuid.New(), Pair: p, Mid: obs.Rate, Rate: r, SpreadBps: q.SpreadBps, Source: obs.Source,
		ObservedAt: obs.ObservedAt, QuotedAt: now, ExpiresAt: now.Add(q.TTL),
	}, nil
}

// Fake is a Provider with fixed rates, for tests and the local stack.
type Fake struct {
	Rates map[Pair]money.Amount
	Err   error            // returned by every call when set
	Now   func() time.Time // ObservedAt; nil means time.Now
}

// Mid returns the fixed rate for p.
func (f *Fake) Mid(_ context.Context, p Pair) (Observation, error) {
	if f.Err != nil {
		return Observation{}, f.Err
	}
	r, ok := f.Rates[p]
	if !ok {
		return Observation{}, fmt.Errorf("%w: %s", ErrUnsupportedPair, p)
	}
	at := time.Now()
	if f.Now != nil {
		at = f.Now()
	}
	return Observation{Pair: p, Rate: r, Source: "fake", ObservedAt: at}, nil
}
