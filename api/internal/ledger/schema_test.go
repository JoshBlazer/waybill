package ledger_test

// These tests talk to the schema with raw SQL, deliberately bypassing the
// Go posting engine: they prove the database enforces the ledger's rules on
// its own (invariants I3 and I4, docs/ARCHITECTURE.md §6).

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshBlazer/waybill/api/internal/ledger"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/store"
	"github.com/JoshBlazer/waybill/api/internal/testdb"
)

type fixture struct {
	pool                        *pgxpool.Pool
	custody, contractor, wallet ledger.Account // USDC
	ngnA, ngnB                  ledger.Account // NGN
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	pool := testdb.New(t)
	ctx := context.Background()
	mk := func(code string, asset money.AssetCode, kind ledger.AccountKind, rule ledger.BalanceRule) ledger.Account {
		a, err := ledger.EnsureAccount(ctx, pool, ledger.Account{Code: code, Asset: asset, Kind: kind, Rule: rule})
		if err != nil {
			t.Fatalf("EnsureAccount(%s): %v", code, err)
		}
		return a
	}
	return fixture{
		pool:       pool,
		custody:    mk("custody:deposit:evm_31337:USDC", "USDC", ledger.KindAsset, ledger.NonNegative),
		contractor: mk("liability:contractor:1:USDC:available", "USDC", ledger.KindLiability, ledger.NonPositive),
		wallet:     mk("asset:test:wallet:USDC", "USDC", ledger.KindAsset, ledger.AnyBalance),
		ngnA:       mk("asset:test:a:NGN", "NGN", ledger.KindAsset, ledger.AnyBalance),
		ngnB:       mk("asset:test:b:NGN", "NGN", ledger.KindAsset, ledger.AnyBalance),
	}
}

type rawPosting struct {
	account ledger.Account
	amount  string // minor units, as SQL text
}

// insertRaw inserts an entry and its postings with plain SQL inside tx.
func insertRaw(ctx context.Context, tx pgx.Tx, key string, reverses *int64, ps ...rawPosting) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx,
		`INSERT INTO journal_entries (idempotency_key, kind, reverses_entry_id) VALUES ($1, 'test.raw', $2) RETURNING id`,
		key, reverses).Scan(&id)
	if err != nil {
		return 0, err
	}
	for _, p := range ps {
		if _, err := tx.Exec(ctx,
			`INSERT INTO postings (entry_id, account_id, asset_code, amount) VALUES ($1, $2, $3, $4::numeric)`,
			id, int64(p.account.ID), string(p.account.Asset), p.amount); err != nil {
			return id, err
		}
	}
	return id, nil
}

// commitRaw runs insertRaw in its own transaction and commits.
func commitRaw(t *testing.T, pool *pgxpool.Pool, key string, reverses *int64, ps ...rawPosting) (int64, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	id, err := insertRaw(ctx, tx, key, reverses, ps...)
	if err != nil {
		return 0, err
	}
	return id, tx.Commit(ctx)
}

func constraintOf(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

func balance(t *testing.T, f fixture, a ledger.Account) string {
	t.Helper()
	b, err := ledger.Balance(context.Background(), f.pool, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func entryCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM journal_entries`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func assertLedgerConsistent(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	tb, err := ledger.TrialBalance(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	for asset, total := range tb {
		if !total.IsZero() {
			t.Errorf("trial balance for %s = %s, want 0", asset, total)
		}
	}
	if err := ledger.CheckBalanceCache(ctx, pool); err != nil {
		t.Error(err)
	}
}

func TestLedger_BalancedEntryCommits(t *testing.T) {
	f := newFixture(t)
	_, err := commitRaw(t, f.pool, "k1", nil,
		rawPosting{f.custody, "125500000"},
		rawPosting{f.contractor, "-125500000"})
	if err != nil {
		t.Fatalf("balanced entry rejected: %v", err)
	}
	if got := balance(t, f, f.custody); got != "125500000" {
		t.Errorf("custody balance = %s", got)
	}
	if got := balance(t, f, f.contractor); got != "-125500000" {
		t.Errorf("contractor balance = %s", got)
	}
	assertLedgerConsistent(t, f.pool)
}

func TestLedger_UnbalancedEntryRejectedByDB(t *testing.T) {
	f := newFixture(t)
	_, err := commitRaw(t, f.pool, "k1", nil,
		rawPosting{f.custody, "100"},
		rawPosting{f.contractor, "-99"})
	if constraintOf(err) != "ledger_entry_balanced" {
		t.Fatalf("commit err = %v, want ledger_entry_balanced violation", err)
	}
	if n := entryCount(t, f.pool); n != 0 {
		t.Fatalf("%d entries persisted after rejected commit", n)
	}
	assertLedgerConsistent(t, f.pool)
}

func TestLedger_BalanceIsPerAsset(t *testing.T) {
	// +100 USDC and -100 NGN sum to zero numerically but not per asset.
	f := newFixture(t)
	_, err := commitRaw(t, f.pool, "k1", nil,
		rawPosting{f.wallet, "100"},
		rawPosting{f.ngnA, "-100"})
	if constraintOf(err) != "ledger_entry_balanced" {
		t.Fatalf("commit err = %v, want ledger_entry_balanced violation", err)
	}
	// A two-asset entry where each asset balances is fine (a conversion).
	_, err = commitRaw(t, f.pool, "k2", nil,
		rawPosting{f.wallet, "100"}, rawPosting{f.custody, "-100"},
		rawPosting{f.ngnA, "-5000"}, rawPosting{f.ngnB, "5000"})
	if err == nil || constraintOf(err) != "ledger_balance_rule" {
		// custody is non_negative and starts at 0, so -100 must be refused.
		t.Fatalf("err = %v, want ledger_balance_rule (custody cannot go negative)", err)
	}
	_, err = commitRaw(t, f.pool, "k3", nil,
		rawPosting{f.wallet, "-100"}, rawPosting{f.custody, "100"},
		rawPosting{f.ngnA, "-5000"}, rawPosting{f.ngnB, "5000"})
	if err != nil {
		t.Fatalf("balanced two-asset entry rejected: %v", err)
	}
	assertLedgerConsistent(t, f.pool)
}

func TestLedger_EntryNeedsTwoPostings(t *testing.T) {
	f := newFixture(t)
	if _, err := commitRaw(t, f.pool, "none", nil); constraintOf(err) != "ledger_entry_min_postings" {
		t.Errorf("entry with no postings: err = %v", err)
	}
	if n := entryCount(t, f.pool); n != 0 {
		t.Fatalf("%d entries persisted", n)
	}
}

func TestLedger_UpdateAndDeleteRejected(t *testing.T) {
	f := newFixture(t)
	if _, err := commitRaw(t, f.pool, "k1", nil,
		rawPosting{f.custody, "10"}, rawPosting{f.contractor, "-10"}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, stmt := range []string{
		`UPDATE postings SET amount = amount * 2`,
		`DELETE FROM postings`,
		`TRUNCATE postings CASCADE`,
		`UPDATE journal_entries SET memo = 'edited'`,
		`DELETE FROM journal_entries`,
		`TRUNCATE journal_entries CASCADE`,
	} {
		if _, err := f.pool.Exec(ctx, stmt); err == nil {
			t.Errorf("%q succeeded; the ledger must be append-only", stmt)
		}
	}
	if got := balance(t, f, f.custody); got != "10" {
		t.Fatalf("custody balance = %s after rejected mutations, want 10", got)
	}
	assertLedgerConsistent(t, f.pool)
}

func TestLedger_BalanceCacheCannotBeWrittenDirectly(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, stmt := range []string{
		`UPDATE ledger_balances SET balance = 1000000`,
		`DELETE FROM ledger_balances`,
	} {
		if _, err := f.pool.Exec(ctx, stmt); err == nil {
			t.Errorf("%q succeeded; balances are trigger-maintained only", stmt)
		}
	}
	assertLedgerConsistent(t, f.pool)
}

func TestLedger_AccountsAreImmutable(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, stmt := range []string{
		`UPDATE ledger_accounts SET balance_rule = 'any'`,
		`UPDATE ledger_accounts SET asset_code = 'NGN'`,
		`DELETE FROM ledger_accounts`,
	} {
		if _, err := f.pool.Exec(ctx, stmt); err == nil {
			t.Errorf("%q succeeded; accounts are immutable", stmt)
		}
	}
}

func TestLedger_FractionalAmountRejected(t *testing.T) {
	f := newFixture(t)
	_, err := commitRaw(t, f.pool, "k1", nil,
		rawPosting{f.wallet, "1.5"},
		rawPosting{f.custody, "-1.5"})
	if err == nil {
		t.Fatal("fractional minor units accepted; they must be rejected, not rounded")
	}
	if n := entryCount(t, f.pool); n != 0 {
		t.Fatalf("%d entries persisted", n)
	}
}

func TestLedger_PostingAssetMustMatchAccount(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id int64
	if err := tx.QueryRow(ctx, `INSERT INTO journal_entries (idempotency_key, kind) VALUES ('k', 'test.raw') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO postings (entry_id, account_id, asset_code, amount) VALUES ($1, $2, 'NGN', 5)`, id, int64(f.custody.ID))
	if constraintOf(err) != "postings_asset_matches_account" {
		t.Fatalf("err = %v, want postings_asset_matches_account", err)
	}
}

func TestLedger_CannotAddPostingsToCommittedEntry(t *testing.T) {
	f := newFixture(t)
	id, err := commitRaw(t, f.pool, "k1", nil,
		rawPosting{f.custody, "10"}, rawPosting{f.wallet, "-10"})
	if err != nil {
		t.Fatal(err)
	}
	// A later transaction appends a balanced pair to the old entry. Even
	// though it balances, history must not change.
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `INSERT INTO postings (entry_id, account_id, asset_code, amount) VALUES ($1, $2, 'USDC', 7), ($1, $3, 'USDC', -7)`,
		id, int64(f.custody.ID), int64(f.wallet.ID))
	if err == nil {
		t.Fatal("postings were added to an entry from an earlier transaction")
	}
}

func TestLedger_OverdraftRejectedByDB(t *testing.T) {
	f := newFixture(t)
	// custody (non_negative) would go to -1.
	_, err := commitRaw(t, f.pool, "k1", nil,
		rawPosting{f.custody, "-1"}, rawPosting{f.wallet, "1"})
	if constraintOf(err) != "ledger_balance_rule" {
		t.Fatalf("err = %v, want ledger_balance_rule", err)
	}
	// contractor liability (non_positive) would go to +1: we cannot owe a
	// contractor a negative amount, i.e. pay out more than they have.
	_, err = commitRaw(t, f.pool, "k2", nil,
		rawPosting{f.contractor, "1"}, rawPosting{f.wallet, "-1"})
	if constraintOf(err) != "ledger_balance_rule" {
		t.Fatalf("err = %v, want ledger_balance_rule", err)
	}
	assertLedgerConsistent(t, f.pool)
}

func TestLedger_ReversalRules(t *testing.T) {
	f := newFixture(t)
	orig, err := commitRaw(t, f.pool, "orig", nil,
		rawPosting{f.custody, "300"}, rawPosting{f.contractor, "-300"})
	if err != nil {
		t.Fatal(err)
	}

	// A "reversal" that does not mirror the original is refused.
	_, err = commitRaw(t, f.pool, "bad-rev", &orig,
		rawPosting{f.custody, "-200"}, rawPosting{f.contractor, "200"})
	if constraintOf(err) != "ledger_reversal_mirrors_original" {
		t.Fatalf("partial reversal: err = %v, want ledger_reversal_mirrors_original", err)
	}

	rev, err := commitRaw(t, f.pool, "rev", &orig,
		rawPosting{f.custody, "-300"}, rawPosting{f.contractor, "300"})
	if err != nil {
		t.Fatalf("exact reversal rejected: %v", err)
	}
	if balance(t, f, f.custody) != "0" || balance(t, f, f.contractor) != "0" {
		t.Fatal("balances not restored by reversal")
	}

	// The same entry cannot be reversed twice.
	_, err = commitRaw(t, f.pool, "rev2", &orig,
		rawPosting{f.custody, "-300"}, rawPosting{f.contractor, "300"})
	if err == nil {
		t.Fatal("entry reversed twice")
	}

	// A reversal cannot itself be reversed.
	_, err = commitRaw(t, f.pool, "rev-of-rev", &rev,
		rawPosting{f.custody, "300"}, rawPosting{f.contractor, "-300"})
	if constraintOf(err) != "ledger_no_reversal_of_reversal" {
		t.Fatalf("reversal of reversal: err = %v", err)
	}
	assertLedgerConsistent(t, f.pool)
}

func TestLedger_AmountParameterEncodesExactly(t *testing.T) {
	// Writes through sqlc with a money.Amount parameter (not SQL text) and
	// reads it back: the domain type must round-trip 78-digit values.
	f := newFixture(t)
	ctx := context.Background()
	big, err := money.ParseMinor("123456789012345678901234567890123456789012345678901234567890123456789012345678")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	e, err := q.InsertJournalEntry(ctx, store.InsertJournalEntryParams{IdempotencyKey: "big", Kind: "test.big"})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []store.InsertPostingParams{
		{EntryID: e.ID, AccountID: int64(f.wallet.ID), AssetCode: "USDC", Amount: big},
		{EntryID: e.ID, AccountID: int64(f.contractor.ID), AssetCode: "USDC", Amount: big.Neg()},
	} {
		if err := q.InsertPosting(ctx, p); err != nil {
			t.Fatalf("InsertPosting: %v", err)
		}
	}
	got, err := ledger.Balance(ctx, tx, f.wallet.ID)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if !got.Equal(big) {
		t.Fatalf("round trip: wrote %s, read %s", big, got)
	}
}

func TestEnsureAccount_IdempotentAndConflict(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	want := ledger.Account{Code: "liability:contractor:9:NGN:available", Asset: "NGN", Kind: ledger.KindLiability, Rule: ledger.NonPositive}
	a, err := ledger.EnsureAccount(ctx, pool, want)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ledger.EnsureAccount(ctx, pool, want)
	if err != nil || b.ID != a.ID {
		t.Fatalf("second EnsureAccount = %+v, %v; want same id %d", b, err, a.ID)
	}
	want.Rule = ledger.AnyBalance
	if _, err := ledger.EnsureAccount(ctx, pool, want); !errors.Is(err, ledger.ErrAccountConflict) {
		t.Fatalf("conflicting EnsureAccount err = %v, want ErrAccountConflict", err)
	}
}
