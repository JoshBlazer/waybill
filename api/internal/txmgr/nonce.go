package txmgr

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/JoshBlazer/waybill/api/internal/evm"
	"github.com/JoshBlazer/waybill/api/internal/store"
)

// NextNonce assigns the next nonce for l.Signer on l.Network inside the
// caller's transaction, the same transaction that records the outbound
// transaction using it. If tx rolls back, the nonce was never handed out.
// Design: docs/design/txmgr.md §4.
//
// chainPending is the node's pending nonce for the signer
// (eth_getTransactionCount at "pending").
//
// Behaviour:
//   - No signer_nonces row: ErrNoLease.
//   - Row leased to another owner: ErrNotLeaseHolder.
//   - Lease expired by the database clock: ErrLeaseExpired.
//   - Otherwise the nonce is the larger of the stored next nonce and
//     chainPending, and the stored next nonce becomes that plus one.
//
// Concurrent callers with the same lease receive distinct nonces with no
// gaps: each waits on the row lock until the previous caller's transaction
// ends.
func NextNonce(ctx context.Context, tx pgx.Tx, l Lease, chainPending uint64) (uint64, error) {
	q := store.New(tx)
	signer := evm.NormalizeAddress(l.Signer)
	network := string(l.Network)

	// The lock is what makes I10 hold: a second caller for this key blocks
	// here until our transaction commits or rolls back, then reads the
	// value we wrote.
	row, err := q.LockSignerNonce(ctx, store.LockSignerNonceParams{Signer: signer, Network: network})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: %s on %s", ErrNoLease, signer, network)
	}
	if err != nil {
		return 0, fmt.Errorf("txmgr: lock nonce: %w", err)
	}
	if row.LeaseOwner != l.Owner {
		return 0, fmt.Errorf("%w: %s on %s is leased to %s", ErrNotLeaseHolder, signer, network, row.LeaseOwner)
	}
	if !row.LeaseExpiresAt.After(row.DbNow) {
		return 0, fmt.Errorf("%w: %s on %s expired at %s", ErrLeaseExpired, signer, network, row.LeaseExpiresAt)
	}

	// Stored ahead of the node: assigned but not yet broadcast or not yet
	// seen by this node; going back would reuse a nonce. Node ahead of
	// stored: the key was used outside Waybill; staying behind would fail
	// with "nonce too low".
	stored := uint64(row.NextNonce) //nolint:gosec // CHECK (next_nonce >= 0)
	nonce := max(stored, chainPending)
	if nonce >= 1<<63-1 {
		return 0, fmt.Errorf("txmgr: nonce %d out of range", nonce)
	}
	if err := q.SetSignerNextNonce(ctx, store.SetSignerNextNonceParams{
		Signer: signer, Network: network, NextNonce: int64(nonce) + 1,
	}); err != nil {
		return 0, fmt.Errorf("txmgr: store next nonce: %w", err)
	}
	return nonce, nil
}
