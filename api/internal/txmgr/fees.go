package txmgr

import "math/big"

// ─────────────────────────────────────────────────────────────────────────
// HAND-WRITTEN MODULE. The project owner implements this file.
// Design: docs/design/txmgr.md §5. The tests in fees_test.go define
// "done"; they fail until then. Pure functions: no I/O, no clock.
// ─────────────────────────────────────────────────────────────────────────

// InitialFees returns the fees for a transaction's first broadcast.
//
//   - Tip is suggestedTip.
//   - MaxFee is 2 × baseFee + Tip, so the transaction stays valid while the
//     base fee rises for about six consecutive full blocks.
//
// Errors: ErrInvalidFees if an input is nil or negative; ErrFeeCapExceeded
// if MaxFee would be above maxFeeCap.
func InitialFees(baseFee, suggestedTip, maxFeeCap *big.Int) (Fees, error) {
	return Fees{}, ErrNotImplemented
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
	return Fees{}, ErrNotImplemented
}
