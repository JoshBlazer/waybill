package watcher_test

import (
	"context"
	"testing"
	"time"

	"github.com/JoshBlazer/waybill/api/internal/ledger"
)

// balance returns an account's balance by code pattern, "0" if it has none.
func (r rig) balance(t *testing.T, codeLike string) string {
	t.Helper()
	var b string
	if err := r.pool.QueryRow(context.Background(), `
		SELECT coalesce(sum(b.balance), 0)::text FROM ledger_balances b JOIN ledger_accounts a ON a.id = b.account_id
		WHERE a.code LIKE $1`, codeLike).Scan(&b); err != nil {
		t.Fatal(err)
	}
	return b
}

func (r rig) entryKinds(t *testing.T) []string {
	t.Helper()
	rows, err := r.pool.Query(context.Background(), `SELECT kind FROM journal_entries ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		out = append(out, k)
	}
	return out
}

func (r rig) assertTrialBalanceZero(t *testing.T) {
	t.Helper()
	tb, err := ledger.TrialBalance(context.Background(), r.pool)
	if err != nil {
		t.Fatal(err)
	}
	for a, v := range tb {
		if !v.IsZero() {
			t.Fatalf("trial balance %s = %s", a, v)
		}
	}
	if err := ledger.CheckBalanceCache(context.Background(), r.pool); err != nil {
		t.Fatal(err)
	}
}

func (r rig) tick(t *testing.T) {
	t.Helper()
	if err := r.w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}

const (
	pendingLike = "liability:contractor:%:USDC:pending"
	custodyLike = "custody:deposit:%"
)

func TestWatcher_ReorgUndoesConfirmedPayment(t *testing.T) {
	r := setup(t)
	r.chain.transfer(5, token, r.dep, 100_000_000, 0)
	r.chain.head = 7
	r.tick(t)
	if st, ps := r.state(t); st != "confirmed" || ps["confirmed"] != 1 {
		t.Fatalf("before the reorg: invoice %s, payments %v; want confirmed", st, ps)
	}

	r.chain.reorg(5, dropAll) // the new chain has no transfer
	r.chain.head = 9
	r.tick(t)

	if st, ps := r.state(t); st != "open" || ps["reorged"] != 1 || len(ps) != 1 {
		t.Fatalf("after the reorg: invoice %s, payments %v; want open with the payment reorged", st, ps)
	}
	if got := r.entryKinds(t); len(got) != 2 || got[0] != "deposit.confirmed" || got[1] != "deposit.confirmed.reversal" {
		t.Fatalf("journal = %v; want the confirmation and its reversal", got)
	}
	if p, c := r.balance(t, pendingLike), r.balance(t, custodyLike); p != "0" || c != "0" {
		t.Fatalf("pending %s, custody %s; want both back to 0", p, c)
	}
	r.assertTrialBalanceZero(t)

	// Later ticks change nothing: the reversal happens once.
	r.chain.head = 20
	r.tick(t)
	if got := r.entryKinds(t); len(got) != 2 {
		t.Fatalf("journal after another tick = %v; want unchanged", got)
	}
	if st, _ := r.state(t); st != "open" {
		t.Fatalf("invoice %s after another tick; want open", st)
	}
}

func TestWatcher_ReorgReincludedPaymentCountsOnce(t *testing.T) {
	r := setup(t)
	r.chain.transfer(5, token, r.dep, 100_000_000, 0)
	r.chain.head = 7
	r.tick(t)

	r.chain.reorg(5, keepAll) // same transaction, new block
	r.chain.head = 9
	r.tick(t)

	if st, ps := r.state(t); st != "confirmed" || ps["reorged"] != 1 || ps["confirmed"] != 1 {
		t.Fatalf("invoice %s, payments %v; want confirmed, one reorged sighting and one confirmed", st, ps)
	}
	if p, c := r.balance(t, pendingLike), r.balance(t, custodyLike); p != "-100000000" || c != "100000000" {
		t.Fatalf("pending %s, custody %s; the 100 USDC must count exactly once", p, c)
	}
	r.assertTrialBalanceZero(t)

	r.chain.head = 20
	r.tick(t)
	if st, ps := r.state(t); st != "settled" || ps["final"] != 1 {
		t.Fatalf("invoice %s, payments %v; want settled once the new sighting is final", st, ps)
	}
}

func TestWatcher_ReorgKeepsPaymentsOutsideReplacedBlocks(t *testing.T) {
	r := setup(t)
	r.w.Cfg.Finality = 50
	r.chain.transfer(5, token, r.dep, 60_000_000, 0)
	r.chain.head = 7
	r.tick(t) // 60 of 100 confirmed: underpaid
	r.chain.transfer(9, token, r.dep, 40_000_000, 0)
	r.chain.head = 11
	r.tick(t) // the top-up confirmed: confirmed
	if st, ps := r.state(t); st != "confirmed" || ps["confirmed"] != 2 {
		t.Fatalf("before the reorg: invoice %s, payments %v; want confirmed", st, ps)
	}

	r.chain.reorg(9, dropAll)
	r.chain.head = 12
	r.tick(t)

	if st, ps := r.state(t); st != "underpaid" || ps["confirmed"] != 1 || ps["reorged"] != 1 {
		t.Fatalf("invoice %s, payments %v; want underpaid with the 60 USDC payment still confirmed", st, ps)
	}
	if p := r.balance(t, pendingLike); p != "-60000000" {
		t.Fatalf("pending %s; want -60000000", p)
	}
	r.assertTrialBalanceZero(t)
}

func TestWatcher_ReorgOfOnlyPaymentAfterExpiryExpiresAgain(t *testing.T) {
	r := setup(t)
	r.chain.transfer(5, token, r.dep, 100_000_000, 0)
	r.chain.head = 5
	r.tick(t)

	r.w.Now = func() time.Time { return time.Now().Add(365 * 24 * time.Hour) }
	r.chain.reorg(5, dropAll)
	r.chain.head = 6
	r.tick(t)

	if st, ps := r.state(t); st != "expired" || ps["reorged"] != 1 {
		t.Fatalf("invoice %s, payments %v; want expired again", st, ps)
	}
}

func TestWatcher_ReorgRPCErrorChangesNothing(t *testing.T) {
	r := setup(t)
	r.chain.transfer(5, token, r.dep, 100_000_000, 0)
	r.chain.head = 7
	r.tick(t)
	cursor := r.cursor(t)

	r.chain.reorg(5, dropAll)
	r.chain.head = 9
	r.chain.failHeaders = true
	if err := r.w.Tick(context.Background()); err == nil {
		t.Fatal("Tick succeeded while block headers could not be read")
	}
	if st, ps := r.state(t); st != "confirmed" || ps["confirmed"] != 1 || r.cursor(t) != cursor {
		t.Fatalf("invoice %s, payments %v, cursor %d (was %d); an RPC error must change nothing", st, ps, r.cursor(t), cursor)
	}

	r.chain.failHeaders = false
	r.tick(t)
	if st, ps := r.state(t); st != "open" || ps["reorged"] != 1 {
		t.Fatalf("after recovery: invoice %s, payments %v; want the reorg handled", st, ps)
	}
}

func TestWatcher_NodeBehindScannedBlocksChangesNothing(t *testing.T) {
	r := setup(t)
	r.chain.transfer(5, token, r.dep, 100_000_000, 0)
	r.chain.head = 7
	r.tick(t)

	r.chain.head = 4 // a lagging node behind the payment's block
	if err := r.w.Tick(context.Background()); err == nil {
		t.Fatal("Tick succeeded on a node behind blocks already scanned")
	}
	if st, ps := r.state(t); st != "confirmed" || ps["confirmed"] != 1 || r.cursor(t) != 8 {
		t.Fatalf("invoice %s, payments %v, cursor %d; want nothing changed", st, ps, r.cursor(t))
	}
}
