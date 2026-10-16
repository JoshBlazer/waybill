// Package txmgr is the only writer for each signing key. It hands out
// nonces under a single-writer lease (invariant I10) and computes EIP-1559
// fees, including the bumped fees that replace a stuck transaction
// (invariant I11). Design: docs/design/txmgr.md.
//
// nonce.go and fees.go are hand-written by the project owner; lease.go and
// this file are not.
package txmgr

import (
	"errors"
	"math/big"
)

// Errors. Callers match them with errors.Is.
var (
	// ErrNotImplemented marks the hand-written functions before they are written.
	ErrNotImplemented = errors.New("txmgr: not implemented")

	// ErrLeaseHeld: another process holds a live lease on the key.
	ErrLeaseHeld = errors.New("txmgr: signing key is leased by another process")
	// ErrNoLease: nobody has ever leased the key on this network.
	ErrNoLease = errors.New("txmgr: signing key has no lease")
	// ErrNotLeaseHolder: the lease belongs to another process.
	ErrNotLeaseHolder = errors.New("txmgr: lease is held by another process")
	// ErrLeaseExpired: the caller's lease has run out; renew it first.
	ErrLeaseExpired = errors.New("txmgr: lease has expired")

	// ErrInvalidFees: an input is missing or negative, a max fee is below
	// its tip, or the bump percentage is zero.
	ErrInvalidFees = errors.New("txmgr: invalid fees")
	// ErrFeeCapExceeded: the fees needed are above the configured ceiling.
	// The transaction is not sent; a person decides.
	ErrFeeCapExceeded = errors.New("txmgr: fee above cap")
)

// Fees are EIP-1559 fee caps in wei per gas. Functions in this package
// never modify the *big.Int values they are given and never return values
// that alias their inputs.
type Fees struct {
	MaxFee *big.Int // maxFeePerGas
	Tip    *big.Int // maxPriorityFeePerGas
}
