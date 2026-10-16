package txmgr

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// ─────────────────────────────────────────────────────────────────────────
// HAND-WRITTEN MODULE. The project owner implements this file.
// Design: docs/design/txmgr.md §4. The tests in nonce_test.go define
// "done"; they fail until then.
// ─────────────────────────────────────────────────────────────────────────

// NextNonce assigns the next nonce for l.Signer on l.Network inside the
// caller's transaction, the same transaction that records the outbound
// transaction using it. If tx rolls back, the nonce was never handed out.
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
// gaps: each waits for the previous caller's transaction to end.
// Queries available: LockSignerNonce, SetSignerNextNonce (db/queries/txmgr.sql).
func NextNonce(ctx context.Context, tx pgx.Tx, l Lease, chainPending uint64) (uint64, error) {
	return 0, ErrNotImplemented
}
