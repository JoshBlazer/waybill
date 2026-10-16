package txmgr

import (
	"fmt"
	"math/big"
)

// Pure functions: no I/O, no clock. Design: docs/design/txmgr.md §5.

var (
	two     = big.NewInt(2)
	hundred = big.NewInt(100)
)

// InitialFees returns the fees for a transaction's first broadcast.
//
//   - Tip is suggestedTip.
//   - MaxFee is 2 × baseFee + Tip, so the transaction stays valid while the
//     base fee rises for about six consecutive full blocks.
//
// Errors: ErrInvalidFees if an input is nil or negative; ErrFeeCapExceeded
// if MaxFee would be above maxFeeCap.
func InitialFees(baseFee, suggestedTip, maxFeeCap *big.Int) (Fees, error) {
	if err := checkWei("base fee", baseFee); err != nil {
		return Fees{}, err
	}
	if err := checkWei("tip", suggestedTip); err != nil {
		return Fees{}, err
	}
	if err := checkWei("max fee cap", maxFeeCap); err != nil {
		return Fees{}, err
	}
	maxFee := new(big.Int).Mul(baseFee, two)
	maxFee.Add(maxFee, suggestedTip)
	if maxFee.Cmp(maxFeeCap) > 0 {
		return Fees{}, fmt.Errorf("%w: max fee %s wei above cap %s", ErrFeeCapExceeded, maxFee, maxFeeCap)
	}
	return Fees{MaxFee: maxFee, Tip: new(big.Int).Set(suggestedTip)}, nil
}

// Replacement returns the fees for a transaction that replaces prev at the
// same nonce. A node accepts a replacement only if both fees rise by at
// least bumpPercent and both are strictly higher than before; the result is
// the smallest fees that meet that rule, raised to market where market is
// higher, with MaxFee never below Tip.
//
// Errors: ErrInvalidFees if a value is nil or negative, prev.MaxFee <
// prev.Tip, or bumpPercent is 0; ErrFeeCapExceeded if MaxFee would be above
// maxFeeCap.
func Replacement(prev, market Fees, bumpPercent uint64, maxFeeCap *big.Int) (Fees, error) {
	for _, f := range []struct {
		name string
		v    *big.Int
	}{
		{"previous max fee", prev.MaxFee}, {"previous tip", prev.Tip},
		{"market max fee", market.MaxFee}, {"market tip", market.Tip},
		{"max fee cap", maxFeeCap},
	} {
		if err := checkWei(f.name, f.v); err != nil {
			return Fees{}, err
		}
	}
	if bumpPercent == 0 {
		return Fees{}, fmt.Errorf("%w: bump percentage is 0", ErrInvalidFees)
	}
	if prev.MaxFee.Cmp(prev.Tip) < 0 {
		return Fees{}, fmt.Errorf("%w: previous max fee %s below its tip %s", ErrInvalidFees, prev.MaxFee, prev.Tip)
	}

	tip := maxOf(bumped(prev.Tip, bumpPercent), market.Tip)
	maxFee := maxOf(bumped(prev.MaxFee, bumpPercent), market.MaxFee, tip)
	if maxFee.Cmp(maxFeeCap) > 0 {
		return Fees{}, fmt.Errorf("%w: replacement max fee %s wei above cap %s", ErrFeeCapExceeded, maxFee, maxFeeCap)
	}
	return Fees{MaxFee: maxFee, Tip: tip}, nil
}

// bumped is the smallest fee a node accepts in place of x:
// max(⌈x × (100 + bump) / 100⌉, x + 1). Rounding up matters because
// integer division can land one wei short; x + 1 matters because a zero
// tip bumped by any percentage is still zero, and an equal fee is not a
// replacement. The result is a new value.
func bumped(x *big.Int, bumpPercent uint64) *big.Int {
	n := new(big.Int).Mul(x, new(big.Int).SetUint64(100+bumpPercent))
	n.Add(n, big.NewInt(99))
	n.Quo(n, hundred)
	return maxOf(n, new(big.Int).Add(x, big.NewInt(1)))
}

// maxOf returns a new value equal to the largest argument, never one of the
// arguments themselves, so results do not alias inputs.
func maxOf(xs ...*big.Int) *big.Int {
	m := new(big.Int).Set(xs[0])
	for _, x := range xs[1:] {
		if x.Cmp(m) > 0 {
			m.Set(x)
		}
	}
	return m
}

func checkWei(name string, v *big.Int) error {
	if v == nil {
		return fmt.Errorf("%w: %s is missing", ErrInvalidFees, name)
	}
	if v.Sign() < 0 {
		return fmt.Errorf("%w: %s is negative", ErrInvalidFees, name)
	}
	return nil
}
