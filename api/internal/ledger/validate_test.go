package ledger_test

// Specification for Entry.Validate (hand-written; docs/design/ledger.md §4).
// Pure: no database.

import (
	"errors"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/JoshBlazer/waybill/api/internal/ledger"
	"github.com/JoshBlazer/waybill/api/internal/money"
)

func p(account int64, asset string, amount int64) ledger.Posting {
	return ledger.Posting{Account: ledger.AccountID(account), Asset: money.AssetCode(asset), Amount: money.FromInt64(amount)}
}

func entry(ps ...ledger.Posting) ledger.Entry {
	return ledger.Entry{IdempotencyKey: "k", Kind: "test.entry", Postings: ps}
}

func TestValidate_AcceptsBalancedEntries(t *testing.T) {
	cases := map[string]ledger.Entry{
		"two postings":       entry(p(1, "USDC", 100), p(2, "USDC", -100)),
		"three postings":     entry(p(1, "USDC", 100), p(2, "USDC", -60), p(3, "USDC", -40)),
		"conversion":         entry(p(1, "USDC", -100), p(2, "USDC", 100), p(3, "NGN", 154025), p(4, "NGN", -154025)),
		"postings unordered": entry(p(9, "USDC", -1), p(3, "USDC", 1)),
	}
	for name, e := range cases {
		if err := e.Validate(); err != nil {
			t.Errorf("%s: Validate = %v, want nil", name, err)
		}
	}
}

func TestValidate_RejectsEachRule(t *testing.T) {
	cases := []struct {
		name string
		e    ledger.Entry
		want error
	}{
		{"empty key", ledger.Entry{Kind: "k.k", Postings: []ledger.Posting{p(1, "USDC", 1), p(2, "USDC", -1)}}, ledger.ErrEmptyIdempotencyKey},
		{"empty kind", ledger.Entry{IdempotencyKey: "k", Postings: []ledger.Posting{p(1, "USDC", 1), p(2, "USDC", -1)}}, ledger.ErrEmptyKind},
		{"no postings", entry(), ledger.ErrTooFewPostings},
		{"one posting", entry(p(1, "USDC", 0)), ledger.ErrTooFewPostings},
		{"zero amount", entry(p(1, "USDC", 0), p(2, "USDC", 0)), ledger.ErrZeroAmount},
		{"zero amount among others", entry(p(1, "USDC", 5), p(2, "USDC", -5), p(3, "USDC", 0)), ledger.ErrZeroAmount},
		{"duplicate account", entry(p(1, "USDC", 5), p(1, "USDC", -5)), ledger.ErrDuplicateAccount},
		{"unbalanced", entry(p(1, "USDC", 100), p(2, "USDC", -99)), ledger.ErrUnbalanced},
		{"balanced in total, not per asset", entry(p(1, "USDC", 100), p(2, "NGN", -100)), ledger.ErrUnbalanced},
	}
	for _, tc := range cases {
		err := tc.e.Validate()
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: Validate = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestValidate_UnbalancedErrorNamesAssetAndSum(t *testing.T) {
	err := entry(p(1, "USDC", 100), p(2, "USDC", -100), p(3, "NGN", 7), p(4, "NGN", -2)).Validate()
	if !errors.Is(err, ledger.ErrUnbalanced) {
		t.Fatalf("Validate = %v, want ErrUnbalanced", err)
	}
	if !strings.Contains(err.Error(), "NGN") || !strings.Contains(err.Error(), "5") {
		t.Fatalf("error %q should name the asset (NGN) and its sum (5)", err)
	}
}

// genBalancedEntry draws an entry with 1–3 assets, each balanced, over
// distinct accounts with non-zero amounts.
func genBalancedEntry() *rapid.Generator[ledger.Entry] {
	return rapid.Custom(func(t *rapid.T) ledger.Entry {
		assets := rapid.SliceOfNDistinct(rapid.SampledFrom([]string{"USDC", "USDT", "NGN", "BTC"}), 1, 3, rapid.ID[string]).Draw(t, "assets")
		var ps []ledger.Posting
		next := int64(1)
		for _, a := range assets {
			n := rapid.IntRange(1, 4).Draw(t, "n_"+a)
			sum := money.Zero
			for i := 0; i < n; i++ {
				amt := rapid.Int64Range(1, 1<<50).Draw(t, "amt")
				if rapid.Bool().Draw(t, "neg") {
					amt = -amt
				}
				ps = append(ps, ledger.Posting{Account: ledger.AccountID(next), Asset: money.AssetCode(a), Amount: money.FromInt64(amt)})
				sum = sum.Add(money.FromInt64(amt))
				next++
			}
			if sum.IsZero() { // need a non-zero balancing posting; add a pair instead
				ps = append(ps,
					ledger.Posting{Account: ledger.AccountID(next), Asset: money.AssetCode(a), Amount: money.FromInt64(1)},
					ledger.Posting{Account: ledger.AccountID(next + 1), Asset: money.AssetCode(a), Amount: money.FromInt64(-1)})
				next += 2
				continue
			}
			ps = append(ps, ledger.Posting{Account: ledger.AccountID(next), Asset: money.AssetCode(a), Amount: sum.Neg()})
			next++
		}
		ps = rapid.Permutation(ps).Draw(t, "order")
		return ledger.Entry{IdempotencyKey: "k", Kind: "test.prop", Postings: ps}
	})
}

func TestValidate_PropertyBalancedAccepted(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		e := genBalancedEntry().Draw(t, "entry")
		if err := e.Validate(); err != nil {
			t.Fatalf("balanced entry rejected: %v", err)
		}
	})
}

func TestValidate_PropertyAnyPerturbationRejected(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		e := genBalancedEntry().Draw(t, "entry")
		i := rapid.IntRange(0, len(e.Postings)-1).Draw(t, "which")
		delta := rapid.Int64Range(1, 1000).Draw(t, "delta")
		ps := append([]ledger.Posting(nil), e.Postings...)
		ps[i].Amount = ps[i].Amount.Add(money.FromInt64(delta))
		e.Postings = ps
		err := e.Validate()
		if err == nil {
			t.Fatalf("perturbed entry accepted: posting %d changed by %d", i, delta)
		}
		if !errors.Is(err, ledger.ErrUnbalanced) && !errors.Is(err, ledger.ErrZeroAmount) {
			t.Fatalf("perturbed entry: err = %v, want ErrUnbalanced (or ErrZeroAmount if it hit 0)", err)
		}
	})
}
