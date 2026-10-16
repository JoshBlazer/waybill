package txmgr_test

import (
	"errors"
	"math/big"
	"testing"

	"pgregory.net/rapid"

	"github.com/JoshBlazer/waybill/api/internal/txmgr"
)

const gwei = 1_000_000_000

func w(n int64) *big.Int { return big.NewInt(n) }

func fees(maxFee, tip int64) txmgr.Fees { return txmgr.Fees{MaxFee: w(maxFee), Tip: w(tip)} }

func eq(a, b txmgr.Fees) bool { return a.MaxFee.Cmp(b.MaxFee) == 0 && a.Tip.Cmp(b.Tip) == 0 }

func show(f txmgr.Fees) string { return "{max " + f.MaxFee.String() + ", tip " + f.Tip.String() + "}" }

var noCap = new(big.Int).Lsh(big.NewInt(1), 256)

func TestInitialFees(t *testing.T) {
	cases := []struct {
		name           string
		base, tip, cap *big.Int
		want           txmgr.Fees
		wantErr        error
	}{
		{"twice base plus tip", w(10 * gwei), w(1 * gwei), noCap, fees(21*gwei, 1*gwei), nil},
		{"zero base fee", w(0), w(2), noCap, fees(2, 2), nil},
		{"zero tip", w(7), w(0), noCap, fees(14, 0), nil},
		{"exactly at the cap", w(10), w(1), w(21), fees(21, 1), nil},
		{"one wei over the cap", w(10), w(1), w(20), txmgr.Fees{}, txmgr.ErrFeeCapExceeded},
		{"nil base fee", nil, w(1), noCap, txmgr.Fees{}, txmgr.ErrInvalidFees},
		{"nil tip", w(1), nil, noCap, txmgr.Fees{}, txmgr.ErrInvalidFees},
		{"nil cap", w(1), w(1), nil, txmgr.Fees{}, txmgr.ErrInvalidFees},
		{"negative base fee", w(-1), w(1), noCap, txmgr.Fees{}, txmgr.ErrInvalidFees},
		{"negative tip", w(1), w(-1), noCap, txmgr.Fees{}, txmgr.ErrInvalidFees},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := txmgr.InitialFees(c.base, c.tip, c.cap)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err = %v, want %v", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !eq(got, c.want) {
				t.Fatalf("got %s, want %s", show(got), show(c.want))
			}
		})
	}
}

func TestReplacement(t *testing.T) {
	cases := []struct {
		name      string
		prev, mkt txmgr.Fees
		bump      uint64
		cap       *big.Int
		want      txmgr.Fees
		wantErr   error
	}{
		{"both bumped by exactly 10%", fees(100, 10), fees(50, 5), 10, noCap, fees(110, 11), nil},
		{"rounds up, never down", fees(101, 1), fees(0, 0), 10, noCap, fees(112, 2), nil},
		{"a zero tip still rises", fees(100, 0), fees(0, 0), 10, noCap, fees(110, 1), nil},
		{"market above the bump wins", fees(100, 10), fees(500, 50), 10, noCap, fees(500, 50), nil},
		{"market tip above the bumped max fee lifts the max fee", fees(100, 10), fees(5, 200), 10, noCap, fees(200, 200), nil},
		{"larger bump percentage", fees(100, 10), fees(0, 0), 25, noCap, fees(125, 13), nil},
		{"exactly at the cap", fees(100, 10), fees(0, 0), 10, w(110), fees(110, 11), nil},
		{"over the cap", fees(100, 10), fees(0, 0), 10, w(109), txmgr.Fees{}, txmgr.ErrFeeCapExceeded},
		{"zero bump", fees(100, 10), fees(0, 0), 0, noCap, txmgr.Fees{}, txmgr.ErrInvalidFees},
		{"previous max below its tip", fees(10, 100), fees(0, 0), 10, noCap, txmgr.Fees{}, txmgr.ErrInvalidFees},
		{"negative market fee", fees(100, 10), fees(-1, 0), 10, noCap, txmgr.Fees{}, txmgr.ErrInvalidFees},
		{"nil previous tip", txmgr.Fees{MaxFee: w(100)}, fees(0, 0), 10, noCap, txmgr.Fees{}, txmgr.ErrInvalidFees},
		{"nil market", fees(100, 10), txmgr.Fees{}, 10, noCap, txmgr.Fees{}, txmgr.ErrInvalidFees},
		{"nil cap", fees(100, 10), fees(0, 0), 10, nil, txmgr.Fees{}, txmgr.ErrInvalidFees},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := txmgr.Replacement(c.prev, c.mkt, c.bump, c.cap)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err = %v, want %v", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !eq(got, c.want) {
				t.Fatalf("got %s, want %s", show(got), show(c.want))
			}
		})
	}
}

// accepts is the node's replacement rule for one fee: strictly higher, and
// at least bump percent higher (new × 100 ≥ old × (100 + bump), which is
// stricter than geth's rounded-down threshold).
func accepts(prev, next *big.Int, bump uint64) bool {
	if next.Cmp(prev) <= 0 {
		return false
	}
	lhs := new(big.Int).Mul(next, big.NewInt(100))
	rhs := new(big.Int).Mul(prev, new(big.Int).SetUint64(100+bump))
	return lhs.Cmp(rhs) >= 0
}

func genWei(max *big.Int) *rapid.Generator[*big.Int] {
	return rapid.Custom(func(t *rapid.T) *big.Int {
		// Mostly small numbers, where rounding matters, and some up to 2^96.
		var n *big.Int
		if rapid.Bool().Draw(t, "small") {
			n = big.NewInt(rapid.Int64Range(0, 1000).Draw(t, "n"))
		} else {
			n = new(big.Int).SetBytes(rapid.SliceOfN(rapid.Byte(), 1, 12).Draw(t, "bytes"))
		}
		if max != nil && n.Cmp(max) > 0 {
			n.Set(max)
		}
		return n
	})
}

func genFees(t *rapid.T, label string) txmgr.Fees {
	maxFee := genWei(nil).Draw(t, label+".max")
	tip := genWei(maxFee).Draw(t, label+".tip")
	return txmgr.Fees{MaxFee: maxFee, Tip: tip}
}

func clone(f txmgr.Fees) txmgr.Fees {
	return txmgr.Fees{MaxFee: new(big.Int).Set(f.MaxFee), Tip: new(big.Int).Set(f.Tip)}
}

// TestTxMgr_ReplacementBumpsBothFees is invariant I11: every replacement
// is one the node accepts, at or above market, valid (max fee ≥ tip), as
// small as those rules allow, and leaves its inputs untouched.
func TestTxMgr_ReplacementBumpsBothFees(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		prev := genFees(t, "prev")
		market := genFees(t, "market")
		bump := rapid.Uint64Range(1, 100).Draw(t, "bump")
		prevCopy, marketCopy := clone(prev), clone(market)

		got, err := txmgr.Replacement(prev, market, bump, noCap)
		if err != nil {
			t.Fatalf("Replacement(%s, %s, %d%%): %v", show(prev), show(market), bump, err)
		}
		if !eq(prev, prevCopy) || !eq(market, marketCopy) {
			t.Fatalf("inputs were modified")
		}
		if got.MaxFee == prev.MaxFee || got.Tip == prev.Tip || got.MaxFee == market.MaxFee || got.Tip == market.Tip {
			t.Fatalf("result aliases an input")
		}
		if !accepts(prev.Tip, got.Tip, bump) || !accepts(prev.MaxFee, got.MaxFee, bump) {
			t.Fatalf("node would reject %s replacing %s at %d%%", show(got), show(prev), bump)
		}
		if got.Tip.Cmp(market.Tip) < 0 || got.MaxFee.Cmp(market.MaxFee) < 0 {
			t.Fatalf("%s is below market %s", show(got), show(market))
		}
		if got.MaxFee.Cmp(got.Tip) < 0 {
			t.Fatalf("%s: max fee below tip", show(got))
		}
		// Minimal: one wei less tip would break a rule, unless the tip is
		// held up by market.
		lessTip := new(big.Int).Sub(got.Tip, big.NewInt(1))
		if got.Tip.Cmp(market.Tip) != 0 && accepts(prev.Tip, lessTip, bump) {
			t.Fatalf("tip %s is higher than needed (prev %s, market %s, %d%%)", got.Tip, prev.Tip, market.Tip, bump)
		}
		lessMax := new(big.Int).Sub(got.MaxFee, big.NewInt(1))
		if got.MaxFee.Cmp(market.MaxFee) != 0 && got.MaxFee.Cmp(got.Tip) != 0 && accepts(prev.MaxFee, lessMax, bump) {
			t.Fatalf("max fee %s is higher than needed (prev %s, market %s, %d%%)", got.MaxFee, prev.MaxFee, market.MaxFee, bump)
		}
	})
}

// TestReplacement_RepeatedBumpsKeepRising: replacing a replacement again
// and again is accepted every time and never stalls, even from zero.
func TestReplacement_RepeatedBumpsKeepRising(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		cur := genFees(t, "start")
		bump := rapid.Uint64Range(1, 100).Draw(t, "bump")
		zero := fees(0, 0)
		for i := range 20 {
			next, err := txmgr.Replacement(cur, zero, bump, noCap)
			if err != nil {
				t.Fatalf("round %d: %v", i, err)
			}
			if !accepts(cur.Tip, next.Tip, bump) || !accepts(cur.MaxFee, next.MaxFee, bump) {
				t.Fatalf("round %d: %s does not replace %s", i, show(next), show(cur))
			}
			cur = next
		}
	})
}

func TestReplacement_CapIsAHardCeiling(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		prev := genFees(t, "prev")
		market := genFees(t, "market")
		bump := rapid.Uint64Range(1, 100).Draw(t, "bump")
		capWei := genWei(nil).Draw(t, "cap")
		got, err := txmgr.Replacement(prev, market, bump, capWei)
		switch {
		case errors.Is(err, txmgr.ErrFeeCapExceeded):
			uncapped, err := txmgr.Replacement(prev, market, bump, noCap)
			if err != nil {
				t.Fatal(err)
			}
			if uncapped.MaxFee.Cmp(capWei) <= 0 {
				t.Fatalf("ErrFeeCapExceeded, but %s fits under cap %s", show(uncapped), capWei)
			}
		case err != nil:
			t.Fatal(err)
		case got.MaxFee.Cmp(capWei) > 0:
			t.Fatalf("%s is above cap %s", show(got), capWei)
		}
	})
}
