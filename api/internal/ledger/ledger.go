// Package ledger is Waybill's double-entry, append-only, multi-asset ledger.
// Design: docs/design/ledger.md.
//
// The posting engine (Entry.Validate, Post, Reverse in post.go) is written
// by hand by the project owner; this file holds the types and the
// supporting functions around it.
package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/store"
)

// AccountID identifies a ledger account.
type AccountID int64

// EntryID identifies a journal entry.
type EntryID int64

// AccountKind is the accounting class of an account.
type AccountKind string

// Account kinds.
const (
	KindAsset     AccountKind = "asset"
	KindLiability AccountKind = "liability"
	KindEquity    AccountKind = "equity"
	KindRevenue   AccountKind = "revenue"
	KindExpense   AccountKind = "expense"
)

// BalanceRule limits the sign of an account's balance. The database enforces
// it on every posting (constraint ledger_balance_rule).
type BalanceRule string

// Balance rules. With debit-positive signs, an asset account holding funds
// is non-negative and a liability we owe is non-positive.
const (
	NonNegative BalanceRule = "non_negative"
	NonPositive BalanceRule = "non_positive"
	AnyBalance  BalanceRule = "any"
)

// Account is a ledger account. Accounts are immutable once created.
type Account struct {
	ID    AccountID
	Code  string // for example "liability:contractor:42:USDC:available"
	Asset money.AssetCode
	Kind  AccountKind
	Rule  BalanceRule
}

// Posting is one line of an entry. Amount is signed: debit positive, credit
// negative, in minor units of Asset.
type Posting struct {
	Account AccountID
	Asset   money.AssetCode
	Amount  money.Amount
}

// Entry is a journal entry to be posted. Every field except Memo and the
// references takes part in idempotency: see Post.
type Entry struct {
	// IdempotencyKey identifies this entry for retries, for example
	// "payment:7f3c…:confirmed". Required.
	IdempotencyKey string
	// Kind classifies the entry, dot-separated lower case, for example
	// "deposit.confirmed". Required.
	Kind string
	// RefType and RefID point at the domain object that caused the entry.
	RefType string
	RefID   string
	// Memo is free text for humans.
	Memo string
	// Postings must sum to zero per asset. At least two.
	Postings []Posting
}

// Errors returned by the ledger. Callers match them with errors.Is.
var (
	ErrEmptyIdempotencyKey   = errors.New("ledger: idempotency key is required")
	ErrEmptyKind             = errors.New("ledger: entry kind is required")
	ErrTooFewPostings        = errors.New("ledger: an entry needs at least two postings")
	ErrZeroAmount            = errors.New("ledger: posting amount is zero")
	ErrDuplicateAccount      = errors.New("ledger: an account appears more than once in an entry")
	ErrUnbalanced            = errors.New("ledger: entry does not sum to zero for an asset")
	ErrUnknownAccount        = errors.New("ledger: account does not exist")
	ErrAssetMismatch         = errors.New("ledger: posting asset differs from its account's asset")
	ErrInsufficientFunds     = errors.New("ledger: posting would break an account's balance rule")
	ErrIdempotencyConflict   = errors.New("ledger: idempotency key already used for a different entry")
	ErrEntryNotFound         = errors.New("ledger: entry not found")
	ErrAlreadyReversed       = errors.New("ledger: entry has already been reversed")
	ErrCannotReverseReversal = errors.New("ledger: a reversal cannot itself be reversed")
	ErrAccountConflict       = errors.New("ledger: account code exists with different attributes")
	// ErrNotImplemented marks the hand-written engine before it is written.
	ErrNotImplemented = errors.New("ledger: not implemented")
)

// EnsureAccount returns the account with this code, creating it if needed.
// It fails with ErrAccountConflict if the code exists with a different
// asset, kind or rule: accounts are immutable, so that is a bug.
func EnsureAccount(ctx context.Context, db store.DBTX, want Account) (Account, error) {
	q := store.New(db)
	row, err := q.InsertAccount(ctx, store.InsertAccountParams{
		Code:        want.Code,
		AssetCode:   string(want.Asset),
		Kind:        string(want.Kind),
		BalanceRule: string(want.Rule),
	})
	if errors.Is(err, pgx.ErrNoRows) { // ON CONFLICT DO NOTHING: it already exists
		existing, gerr := q.GetAccountByCode(ctx, want.Code)
		if gerr != nil {
			return Account{}, fmt.Errorf("ledger: load account %q: %w", want.Code, gerr)
		}
		got := accountFromRow(existing.ID, existing.Code, existing.AssetCode, existing.Kind, existing.BalanceRule)
		if got.Asset != want.Asset || got.Kind != want.Kind || got.Rule != want.Rule {
			return got, fmt.Errorf("%w: %q", ErrAccountConflict, want.Code)
		}
		return got, nil
	}
	if err != nil {
		return Account{}, fmt.Errorf("ledger: create account %q: %w", want.Code, err)
	}
	return accountFromRow(row.ID, row.Code, row.AssetCode, row.Kind, row.BalanceRule), nil
}

func accountFromRow(id int64, code, asset, kind, rule string) Account {
	return Account{
		ID:    AccountID(id),
		Code:  code,
		Asset: money.AssetCode(asset),
		Kind:  AccountKind(kind),
		Rule:  BalanceRule(rule),
	}
}

// Balance returns an account's balance in minor units (debit positive).
func Balance(ctx context.Context, db store.DBTX, id AccountID) (money.Amount, error) {
	b, err := store.New(db).GetBalance(ctx, int64(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return money.Zero, fmt.Errorf("%w: %d", ErrUnknownAccount, id)
	}
	return b, err
}

// TrialBalance returns the sum of all postings per asset. In a correct
// ledger every value is zero.
func TrialBalance(ctx context.Context, db store.DBTX) (map[money.AssetCode]money.Amount, error) {
	rows, err := store.New(db).TrialBalance(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[money.AssetCode]money.Amount, len(rows))
	for _, r := range rows {
		out[money.AssetCode(r.AssetCode)] = r.Total
	}
	return out, nil
}

// CheckBalanceCache returns an error if any cached balance differs from the
// sum of that account's postings. It should never fail; reconciliation and
// tests call it to prove that.
func CheckBalanceCache(ctx context.Context, db store.DBTX) error {
	rows, err := store.New(db).BalanceCacheMismatches(ctx)
	if err != nil {
		return err
	}
	if len(rows) > 0 {
		r := rows[0]
		return fmt.Errorf("ledger: %d account(s) have a cached balance that differs from their postings; first: account %d cached %s, posted %s",
			len(rows), r.AccountID, r.Balance, r.Posted)
	}
	return nil
}
