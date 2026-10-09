package ledger

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// ─────────────────────────────────────────────────────────────────────────
// HAND-WRITTEN MODULE. The project owner implements the three functions in
// this file. Design: docs/design/ledger.md §4–§6. The tests in
// validate_test.go and post_test.go define "done"; they fail until then.
// ─────────────────────────────────────────────────────────────────────────

// Validate checks an entry without touching the database. It returns the
// first violated rule, wrapped so errors.Is matches one of:
// ErrEmptyIdempotencyKey, ErrEmptyKind, ErrTooFewPostings, ErrZeroAmount,
// ErrDuplicateAccount, ErrUnbalanced.
//
// For ErrUnbalanced the message names the asset and its non-zero sum.
func (e Entry) Validate() error {
	return ErrNotImplemented
}

// Post records e inside the caller's transaction and returns its id. The
// caller commits; the database's deferred checks run then, so Post's own
// checks exist to give precise errors early, not to be the last line.
//
// Behaviour:
//   - Invalid entry: the Validate error, and nothing is written.
//   - Idempotency key already used for an equivalent entry (same kind,
//     references and postings, in any order): the existing id, nil.
//   - Key used for a different entry: ErrIdempotencyConflict.
//   - Account missing: ErrUnknownAccount. Asset differs from the account's:
//     ErrAssetMismatch. Both detected before inserting.
//   - A balance rule would break: ErrInsufficientFunds (mapped from the
//     database constraint ledger_balance_rule).
//   - Postings are inserted in ascending account id order, so concurrent
//     Posts lock balances in the same order and cannot deadlock each other.
func Post(ctx context.Context, tx pgx.Tx, e Entry) (EntryID, error) {
	return 0, ErrNotImplemented
}

// Reverse posts an entry that exactly negates entry id, linked to it by
// reverses_entry_id, and returns the new entry's id.
//
// Behaviour:
//   - Unknown id: ErrEntryNotFound.
//   - id is itself a reversal: ErrCannotReverseReversal.
//   - id already reversed under a different key: ErrAlreadyReversed.
//   - Same idempotencyKey as an earlier Reverse of the same id: the earlier
//     reversal's id, nil.
//   - Reversal would break a balance rule (funds already moved on):
//     ErrInsufficientFunds. The caller escalates; it is not retried.
//
// The new entry's kind is the original's kind with ".reversal" appended.
func Reverse(ctx context.Context, tx pgx.Tx, id EntryID, idempotencyKey, memo string) (EntryID, error) {
	return 0, ErrNotImplemented
}
