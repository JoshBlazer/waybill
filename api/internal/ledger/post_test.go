package ledger_test

// Specification for Post and Reverse (hand-written; docs/design/ledger.md
// §5–§6). Integration tests against real PostgreSQL.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"pgregory.net/rapid"

	"github.com/JoshBlazer/waybill/api/internal/ledger"
	"github.com/JoshBlazer/waybill/api/internal/money"
)

// post runs ledger.Post in its own transaction and commits on success.
func post(t testing.TB, pool *pgxpool.Pool, e ledger.Entry) (ledger.EntryID, error) {
	t.Helper()
	ctx := context.Background()
	var id ledger.EntryID
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		id, err = ledger.Post(ctx, tx, e)
		return err
	})
	return id, err
}

func reverse(t testing.TB, pool *pgxpool.Pool, id ledger.EntryID, key string) (ledger.EntryID, error) {
	t.Helper()
	ctx := context.Background()
	var rid ledger.EntryID
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		rid, err = ledger.Reverse(ctx, tx, id, key, "test reversal")
		return err
	})
	return rid, err
}

func usdc(a ledger.Account, amount int64) ledger.Posting {
	return ledger.Posting{Account: a.ID, Asset: "USDC", Amount: money.FromInt64(amount)}
}

func deposit(key string, f fixture, amount int64) ledger.Entry {
	return ledger.Entry{
		IdempotencyKey: key,
		Kind:           "deposit.confirmed",
		RefType:        "payment",
		RefID:          key,
		Postings:       []ledger.Posting{usdc(f.custody, amount), usdc(f.contractor, -amount)},
	}
}

func TestPost_RecordsEntryAndUpdatesBalances(t *testing.T) {
	f := newFixture(t)
	id, err := post(t, f.pool, deposit("pay-1", f, 125_500_000))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if id <= 0 {
		t.Fatalf("Post returned id %d", id)
	}
	if balance(t, f, f.custody) != "125500000" || balance(t, f, f.contractor) != "-125500000" {
		t.Fatal("balances not updated")
	}
	assertLedgerConsistent(t, f.pool)
}

func TestPost_InvalidEntryWritesNothing(t *testing.T) {
	f := newFixture(t)
	e := deposit("pay-1", f, 100)
	e.Postings[1].Amount = money.FromInt64(-99)
	if _, err := post(t, f.pool, e); !errors.Is(err, ledger.ErrUnbalanced) {
		t.Fatalf("Post = %v, want ErrUnbalanced", err)
	}
	if n := entryCount(t, f.pool); n != 0 {
		t.Fatalf("%d entries written for an invalid entry", n)
	}
}

func TestPost_RetryWithSameKeyReturnsOriginal(t *testing.T) {
	f := newFixture(t)
	first, err := post(t, f.pool, deposit("pay-1", f, 100))
	if err != nil {
		t.Fatal(err)
	}
	// Same entry, postings in a different order: still the same entry.
	e := deposit("pay-1", f, 100)
	e.Postings[0], e.Postings[1] = e.Postings[1], e.Postings[0]
	second, err := post(t, f.pool, e)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if second != first {
		t.Fatalf("retry returned %d, want original %d", second, first)
	}
	if balance(t, f, f.custody) != "100" {
		t.Fatalf("custody = %s after retry; money was posted twice", balance(t, f, f.custody))
	}
	if n := entryCount(t, f.pool); n != 1 {
		t.Fatalf("%d entries after retry, want 1", n)
	}
}

func TestPost_SameKeyDifferentEntryConflicts(t *testing.T) {
	f := newFixture(t)
	if _, err := post(t, f.pool, deposit("pay-1", f, 100)); err != nil {
		t.Fatal(err)
	}
	for name, e := range map[string]ledger.Entry{
		"different amount": deposit("pay-1", f, 101),
		"different kind": func() ledger.Entry {
			e := deposit("pay-1", f, 100)
			e.Kind = "deposit.final"
			return e
		}(),
		"different reference": func() ledger.Entry {
			e := deposit("pay-1", f, 100)
			e.RefID = "other"
			return e
		}(),
	} {
		if _, err := post(t, f.pool, e); !errors.Is(err, ledger.ErrIdempotencyConflict) {
			t.Errorf("%s: Post = %v, want ErrIdempotencyConflict", name, err)
		}
	}
	if balance(t, f, f.custody) != "100" {
		t.Fatal("a conflicting retry changed balances")
	}
}

func TestPost_UnknownAccount(t *testing.T) {
	f := newFixture(t)
	e := deposit("pay-1", f, 100)
	e.Postings[0].Account = 999_999
	if _, err := post(t, f.pool, e); !errors.Is(err, ledger.ErrUnknownAccount) {
		t.Fatalf("Post = %v, want ErrUnknownAccount", err)
	}
}

func TestPost_AssetMismatch(t *testing.T) {
	f := newFixture(t)
	e := ledger.Entry{IdempotencyKey: "x", Kind: "test.mismatch", Postings: []ledger.Posting{
		{Account: f.ngnA.ID, Asset: "USDC", Amount: money.FromInt64(5)}, // NGN account, USDC posting
		{Account: f.wallet.ID, Asset: "USDC", Amount: money.FromInt64(-5)},
	}}
	if _, err := post(t, f.pool, e); !errors.Is(err, ledger.ErrAssetMismatch) {
		t.Fatalf("Post = %v, want ErrAssetMismatch", err)
	}
}

func TestPost_InsufficientFunds(t *testing.T) {
	f := newFixture(t)
	if _, err := post(t, f.pool, deposit("pay-1", f, 100)); err != nil {
		t.Fatal(err)
	}
	// Pay the contractor out 101 when only 100 is owed.
	payout := ledger.Entry{IdempotencyKey: "payout-1", Kind: "payout.sent", Postings: []ledger.Posting{
		usdc(f.contractor, 101), usdc(f.custody, -101),
	}}
	if _, err := post(t, f.pool, payout); !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Fatalf("Post = %v, want ErrInsufficientFunds", err)
	}
	if balance(t, f, f.contractor) != "-100" {
		t.Fatal("a refused payout changed balances")
	}
}

func TestPost_ConcurrentOpposingPostsDoNotDeadlock(t *testing.T) {
	// Goroutines move funds A→B and B→A at the same time. If postings were
	// applied in caller order, balance row locks would be taken in opposite
	// orders and PostgreSQL would abort one side with a deadlock (40P01).
	f := newFixture(t)
	a, b := f.wallet, f.custody
	if _, err := post(t, f.pool, ledger.Entry{IdempotencyKey: "seed", Kind: "test.seed",
		Postings: []ledger.Posting{usdc(b, 1_000_000), usdc(a, -1_000_000)}}); err != nil {
		t.Fatal(err)
	}
	const perSide = 40
	var wg sync.WaitGroup
	errs := make(chan error, 2*perSide)
	for side := 0; side < 2; side++ {
		wg.Add(1)
		go func(side int) {
			defer wg.Done()
			for i := 0; i < perSide; i++ {
				from, to := a, b
				if side == 1 {
					from, to = b, a
				}
				_, err := post(t, f.pool, ledger.Entry{
					IdempotencyKey: fmt.Sprintf("move-%d-%d", side, i),
					Kind:           "test.move",
					Postings:       []ledger.Posting{usdc(to, 1), usdc(from, -1)},
				})
				if err != nil {
					errs <- err
				}
			}
		}(side)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent Post failed: %v", err)
	}
	assertLedgerConsistent(t, f.pool)
}

func TestReverse_NegatesOriginal(t *testing.T) {
	f := newFixture(t)
	orig, err := post(t, f.pool, deposit("pay-1", f, 300))
	if err != nil {
		t.Fatal(err)
	}
	rev, err := reverse(t, f.pool, orig, "pay-1:reorged")
	if err != nil {
		t.Fatalf("Reverse: %v", err)
	}
	if rev == orig {
		t.Fatal("Reverse returned the original id")
	}
	if balance(t, f, f.custody) != "0" || balance(t, f, f.contractor) != "0" {
		t.Fatal("balances not restored")
	}
	var kind string
	var reverses int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT kind, reverses_entry_id FROM journal_entries WHERE id = $1`, int64(rev)).Scan(&kind, &reverses); err != nil {
		t.Fatal(err)
	}
	if kind != "deposit.confirmed.reversal" || reverses != int64(orig) {
		t.Fatalf("reversal kind=%q reverses=%d, want deposit.confirmed.reversal / %d", kind, reverses, orig)
	}
	assertLedgerConsistent(t, f.pool)
}

func TestReverse_RetryWithSameKeyReturnsSameReversal(t *testing.T) {
	f := newFixture(t)
	orig, _ := post(t, f.pool, deposit("pay-1", f, 300))
	first, err := reverse(t, f.pool, orig, "rev-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := reverse(t, f.pool, orig, "rev-1")
	if err != nil || second != first {
		t.Fatalf("retry = %d, %v; want %d, nil", second, err, first)
	}
}

func TestReverse_Errors(t *testing.T) {
	f := newFixture(t)
	orig, err := post(t, f.pool, deposit("pay-1", f, 300))
	if err != nil {
		t.Fatal(err)
	}
	rev, err := reverse(t, f.pool, orig, "rev-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reverse(t, f.pool, orig, "rev-2"); !errors.Is(err, ledger.ErrAlreadyReversed) {
		t.Errorf("second reversal with new key: %v, want ErrAlreadyReversed", err)
	}
	if _, err := reverse(t, f.pool, rev, "rev-of-rev"); !errors.Is(err, ledger.ErrCannotReverseReversal) {
		t.Errorf("reversing a reversal: %v, want ErrCannotReverseReversal", err)
	}
	if _, err := reverse(t, f.pool, 999_999, "rev-x"); !errors.Is(err, ledger.ErrEntryNotFound) {
		t.Errorf("unknown entry: %v, want ErrEntryNotFound", err)
	}
}

func TestReverse_AfterFundsMovedOnIsInsufficientFunds(t *testing.T) {
	// The reorg-after-payout case: a deposit is credited, the contractor is
	// paid, then the deposit disappears. Reversing it would leave custody
	// negative. The ledger must refuse and the caller must escalate.
	f := newFixture(t)
	dep, _ := post(t, f.pool, deposit("pay-1", f, 300))
	if _, err := post(t, f.pool, ledger.Entry{IdempotencyKey: "payout-1", Kind: "payout.sent",
		Postings: []ledger.Posting{usdc(f.contractor, 300), usdc(f.custody, -300)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := reverse(t, f.pool, dep, "pay-1:reorged"); !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Fatalf("Reverse = %v, want ErrInsufficientFunds", err)
	}
}

// TestLedger_TrialBalanceAlwaysZero (invariant I5): any sequence of Posts
// and Reverses, valid or not, leaves every asset's trial balance at zero and
// every cached balance equal to its postings.
func TestLedger_TrialBalanceAlwaysZero(t *testing.T) {
	f := newFixture(t)
	accounts := []ledger.Account{f.wallet, f.custody, f.contractor}
	ngn := []ledger.Account{f.ngnA, f.ngnB}
	var posted []ledger.EntryID
	n := 0
	rapid.Check(t, func(rt *rapid.T) {
		n++
		key := fmt.Sprintf("prop-%d", n)
		if len(posted) > 0 && rapid.IntRange(0, 4).Draw(rt, "reverse?") == 0 {
			target := rapid.SampledFrom(posted).Draw(rt, "target")
			_, _ = reverse(t, f.pool, target, key) // may legitimately fail
		} else {
			from := rapid.SampledFrom(accounts).Draw(rt, "from")
			to := rapid.SampledFrom(accounts).Draw(rt, "to")
			amt := rapid.Int64Range(1, 1_000_000).Draw(rt, "amt")
			ps := []ledger.Posting{usdc(to, amt), usdc(from, -amt)}
			if rapid.Bool().Draw(rt, "withNGN") {
				k := rapid.Int64Range(1, 1_000_000).Draw(rt, "ngn")
				ps = append(ps,
					ledger.Posting{Account: ngn[0].ID, Asset: "NGN", Amount: money.FromInt64(k)},
					ledger.Posting{Account: ngn[1].ID, Asset: "NGN", Amount: money.FromInt64(-k)})
			}
			// from == to is invalid (duplicate account) and must be refused cleanly.
			if id, err := post(t, f.pool, ledger.Entry{IdempotencyKey: key, Kind: "test.prop", Postings: ps}); err == nil {
				posted = append(posted, id)
			}
		}
		tb, err := ledger.TrialBalance(context.Background(), f.pool)
		if err != nil {
			rt.Fatal(err)
		}
		for asset, total := range tb {
			if !total.IsZero() {
				rt.Fatalf("after step %d trial balance for %s = %s", n, asset, total)
			}
		}
		if err := ledger.CheckBalanceCache(context.Background(), f.pool); err != nil {
			rt.Fatal(err)
		}
	})
	if len(posted) == 0 {
		t.Fatal("no entry was ever posted; the property was not exercised")
	}
}
