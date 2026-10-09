package ledger

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/store"
)

// The posting engine. Design: docs/design/ledger.md §4–§6. The tests in
// validate_test.go and post_test.go are its specification.

// Validate checks an entry without touching the database. It returns the
// first violated rule, wrapped so errors.Is matches one of:
// ErrEmptyIdempotencyKey, ErrEmptyKind, ErrTooFewPostings, ErrZeroAmount,
// ErrDuplicateAccount, ErrUnbalanced.
//
// For ErrUnbalanced the message names the asset and its non-zero sum.
func (e Entry) Validate() error {
	if e.IdempotencyKey == "" {
		return ErrEmptyIdempotencyKey
	}
	if e.Kind == "" {
		return ErrEmptyKind
	}
	if len(e.Postings) < 2 {
		return fmt.Errorf("%w: got %d", ErrTooFewPostings, len(e.Postings))
	}
	for i, p := range e.Postings {
		if p.Amount.IsZero() {
			return fmt.Errorf("%w: posting %d (account %d)", ErrZeroAmount, i, p.Account)
		}
	}
	seen := make(map[AccountID]bool, len(e.Postings))
	for _, p := range e.Postings {
		if seen[p.Account] {
			return fmt.Errorf("%w: account %d", ErrDuplicateAccount, p.Account)
		}
		seen[p.Account] = true
	}

	sums := make(map[money.AssetCode]money.Amount)
	for _, p := range e.Postings {
		sums[p.Asset] = sums[p.Asset].Add(p.Amount)
	}
	var off []string
	for asset, s := range sums {
		if !s.IsZero() {
			off = append(off, fmt.Sprintf("%s sums to %s", asset, s))
		}
	}
	if len(off) > 0 {
		slices.Sort(off) // each item starts with its asset code
		return fmt.Errorf("%w: %s", ErrUnbalanced, strings.Join(off, ", "))
	}
	return nil
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
//
// Two transactions racing on one new key both miss the lookup; the loser
// fails on the key's unique constraint and its transaction is aborted.
// Retrying the whole transaction finds the winner's entry.
func Post(ctx context.Context, tx pgx.Tx, e Entry) (EntryID, error) {
	return post(ctx, tx, e, nil)
}

// post is Post with an optional link to the entry being reversed.
func post(ctx context.Context, tx pgx.Tx, e Entry, reverses *int64) (EntryID, error) {
	if err := e.Validate(); err != nil {
		return 0, err
	}
	q := store.New(tx)

	existing, err := q.GetJournalEntryByIdempotencyKey(ctx, e.IdempotencyKey)
	switch {
	case err == nil:
		return sameEntry(ctx, q, existing, e, reverses)
	case !errors.Is(err, pgx.ErrNoRows):
		return 0, fmt.Errorf("ledger: look up idempotency key: %w", err)
	}

	if err := checkAccounts(ctx, q, e.Postings); err != nil {
		return 0, err
	}

	row, err := q.InsertJournalEntry(ctx, store.InsertJournalEntryParams{
		IdempotencyKey:  e.IdempotencyKey,
		Kind:            e.Kind,
		RefType:         e.RefType,
		RefID:           e.RefID,
		Memo:            e.Memo,
		ReversesEntryID: reverses,
	})
	if err != nil {
		return 0, mapError("insert entry", err)
	}

	// Ascending account id: every transaction takes balance row locks in
	// the same order, so opposing Posts cannot deadlock (design note §5).
	ps := slices.Clone(e.Postings)
	slices.SortFunc(ps, func(a, b Posting) int { return cmp.Compare(a.Account, b.Account) })
	for _, p := range ps {
		if err := q.InsertPosting(ctx, store.InsertPostingParams{
			EntryID:   row.ID,
			AccountID: int64(p.Account),
			AssetCode: string(p.Asset),
			Amount:    p.Amount,
		}); err != nil {
			return 0, mapError(fmt.Sprintf("post to account %d", p.Account), err)
		}
	}
	return EntryID(row.ID), nil
}

// sameEntry decides whether a stored entry with e's idempotency key is a
// retry of e: same kind, references, reversal link and postings as a
// multiset. Memo is free text and does not count.
func sameEntry(ctx context.Context, q *store.Queries, got store.GetJournalEntryByIdempotencyKeyRow, e Entry, reverses *int64) (EntryID, error) {
	conflict := func(what string) (EntryID, error) {
		return 0, fmt.Errorf("%w: key %q, %s differs from entry %d", ErrIdempotencyConflict, e.IdempotencyKey, what, got.ID)
	}
	if got.Kind != e.Kind {
		return conflict("kind")
	}
	if got.RefType != e.RefType || got.RefID != e.RefID {
		return conflict("reference")
	}
	if !equalPtr(got.ReversesEntryID, reverses) {
		return conflict("reversed entry")
	}
	stored, err := q.ListPostingsByEntry(ctx, got.ID)
	if err != nil {
		return 0, fmt.Errorf("ledger: load postings of entry %d: %w", got.ID, err)
	}
	have := make([]string, len(stored))
	for i, p := range stored {
		have[i] = postingKey(AccountID(p.AccountID), money.AssetCode(p.AssetCode), p.Amount)
	}
	want := make([]string, len(e.Postings))
	for i, p := range e.Postings {
		want[i] = postingKey(p.Account, p.Asset, p.Amount)
	}
	slices.Sort(have)
	slices.Sort(want)
	if !slices.Equal(have, want) {
		return conflict("postings")
	}
	return EntryID(got.ID), nil
}

func postingKey(a AccountID, asset money.AssetCode, amount money.Amount) string {
	return fmt.Sprintf("%d|%s|%s", a, asset, amount)
}

// checkAccounts reports a missing account or an asset mismatch before any
// insert, so the caller gets a named error instead of a foreign-key failure.
func checkAccounts(ctx context.Context, q *store.Queries, ps []Posting) error {
	ids := make([]int64, len(ps))
	for i, p := range ps {
		ids[i] = int64(p.Account)
	}
	rows, err := q.GetAccountsByIDs(ctx, ids)
	if err != nil {
		return fmt.Errorf("ledger: load accounts: %w", err)
	}
	assets := make(map[AccountID]money.AssetCode, len(rows))
	for _, r := range rows {
		assets[AccountID(r.ID)] = money.AssetCode(r.AssetCode)
	}
	for _, p := range ps {
		asset, ok := assets[p.Account]
		if !ok {
			return fmt.Errorf("%w: %d", ErrUnknownAccount, p.Account)
		}
		if asset != p.Asset {
			return fmt.Errorf("%w: account %d holds %s, posting is %s", ErrAssetMismatch, p.Account, asset, p.Asset)
		}
	}
	return nil
}

// mapError turns the balance-rule CHECK violation into ErrInsufficientFunds
// and wraps everything else.
func mapError(op string, err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.ConstraintName == "ledger_balance_rule" {
		return fmt.Errorf("%w: %s", ErrInsufficientFunds, op)
	}
	return fmt.Errorf("ledger: %s: %w", op, err)
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
	q := store.New(tx)
	orig, err := q.GetJournalEntry(ctx, int64(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: %d", ErrEntryNotFound, id)
	}
	if err != nil {
		return 0, fmt.Errorf("ledger: load entry %d: %w", id, err)
	}
	if orig.ReversesEntryID != nil {
		return 0, fmt.Errorf("%w: entry %d reverses entry %d", ErrCannotReverseReversal, id, *orig.ReversesEntryID)
	}

	origID := int64(id)
	prior, err := q.GetReversalOf(ctx, &origID)
	switch {
	case err == nil:
		// A retry if the earlier reversal used this key.
		e, gerr := q.GetJournalEntry(ctx, prior)
		if gerr != nil {
			return 0, fmt.Errorf("ledger: load reversal %d: %w", prior, gerr)
		}
		if e.IdempotencyKey == idempotencyKey {
			return EntryID(prior), nil
		}
		return 0, fmt.Errorf("%w: entry %d by entry %d", ErrAlreadyReversed, id, prior)
	case !errors.Is(err, pgx.ErrNoRows):
		return 0, fmt.Errorf("ledger: look up reversal of %d: %w", id, err)
	}

	stored, err := q.ListPostingsByEntry(ctx, origID)
	if err != nil {
		return 0, fmt.Errorf("ledger: load postings of entry %d: %w", id, err)
	}
	ps := make([]Posting, len(stored))
	for i, p := range stored {
		ps[i] = Posting{Account: AccountID(p.AccountID), Asset: money.AssetCode(p.AssetCode), Amount: p.Amount.Neg()}
	}
	return post(ctx, tx, Entry{
		IdempotencyKey: idempotencyKey,
		Kind:           orig.Kind + ".reversal",
		RefType:        orig.RefType,
		RefID:          orig.RefID,
		Memo:           memo,
		Postings:       ps,
	}, &origID)
}

func equalPtr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
