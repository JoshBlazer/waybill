package txmgr_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshBlazer/waybill/api/internal/safety"
	"github.com/JoshBlazer/waybill/api/internal/testdb"
	"github.com/JoshBlazer/waybill/api/internal/txmgr"
)

var (
	signer  = common.HexToAddress("0x00000000000000000000000000000000000000a1")
	network = safety.EVMAnvil
)

func acquire(t *testing.T, pool *pgxpool.Pool, owner string, ttl time.Duration) txmgr.Lease {
	t.Helper()
	l, err := txmgr.AcquireLease(context.Background(), pool, signer, network, owner, ttl)
	if err != nil {
		t.Fatalf("AcquireLease(%s): %v", owner, err)
	}
	return l
}

// next runs NextNonce in its own committed transaction.
func next(pool *pgxpool.Pool, l txmgr.Lease, chainPending uint64) (uint64, error) {
	var n uint64
	err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		var err error
		n, err = txmgr.NextNonce(context.Background(), tx, l, chainPending)
		return err
	})
	return n, err
}

func mustNext(t *testing.T, pool *pgxpool.Pool, l txmgr.Lease, chainPending uint64) uint64 {
	t.Helper()
	n, err := next(pool, l, chainPending)
	if err != nil {
		t.Fatalf("NextNonce(chainPending=%d): %v", chainPending, err)
	}
	return n
}

func TestNextNonce_FirstUseStartsAtChainPending(t *testing.T) {
	pool := testdb.New(t)
	l := acquire(t, pool, "worker-a", time.Minute)
	// The key has already sent 7 transactions outside Waybill.
	for i, want := range []uint64{7, 8, 9} {
		if got := mustNext(t, pool, l, 7); got != want {
			t.Fatalf("call %d: nonce %d, want %d", i, got, want)
		}
	}
}

func TestNextNonce_StoredAheadOfNodeIsKept(t *testing.T) {
	pool := testdb.New(t)
	l := acquire(t, pool, "worker-a", time.Minute)
	mustNext(t, pool, l, 0) // 0
	mustNext(t, pool, l, 0) // 1
	// Nonces 0 and 1 are assigned but not yet broadcast, or this node has
	// not seen them: it still reports 0. Going back would reuse a nonce.
	if got := mustNext(t, pool, l, 0); got != 2 {
		t.Fatalf("nonce %d, want 2", got)
	}
}

func TestNextNonce_NodeAheadOfStoredSkipsForward(t *testing.T) {
	pool := testdb.New(t)
	l := acquire(t, pool, "worker-a", time.Minute)
	mustNext(t, pool, l, 0) // 0
	// Something else used nonces 1–9 with this key. Reusing 1 would fail
	// with "nonce too low".
	if got := mustNext(t, pool, l, 10); got != 10 {
		t.Fatalf("nonce %d, want 10", got)
	}
	if got := mustNext(t, pool, l, 3); got != 11 {
		t.Fatalf("nonce %d, want 11", got)
	}
}

func TestNextNonce_RolledBackTransactionDoesNotConsume(t *testing.T) {
	pool := testdb.New(t)
	l := acquire(t, pool, "worker-a", time.Minute)
	mustNext(t, pool, l, 0) // 0
	errRecordFailed := errors.New("recording the outbound transaction failed")
	err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		n, err := txmgr.NextNonce(context.Background(), tx, l, 0)
		if err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("nonce %d inside the rolled-back transaction, want 1", n)
		}
		return errRecordFailed
	})
	if !errors.Is(err, errRecordFailed) {
		t.Fatalf("transaction error = %v", err)
	}
	// Nothing was broadcast with 1, so it must be handed out again; a gap
	// would block every later transaction from this key.
	if got := mustNext(t, pool, l, 0); got != 1 {
		t.Fatalf("nonce after rollback %d, want 1", got)
	}
}

func TestNextNonce_RequiresCurrentLease(t *testing.T) {
	pool := testdb.New(t)

	if _, err := next(pool, txmgr.Lease{Signer: signer, Network: network, Owner: "worker-a"}, 0); !errors.Is(err, txmgr.ErrNoLease) {
		t.Fatalf("without any lease: err = %v, want ErrNoLease", err)
	}

	l := acquire(t, pool, "worker-a", time.Minute)
	impostor := l
	impostor.Owner = "worker-b"
	if _, err := next(pool, impostor, 0); !errors.Is(err, txmgr.ErrNotLeaseHolder) {
		t.Fatalf("another owner: err = %v, want ErrNotLeaseHolder", err)
	}

	short := acquire(t, pool, "worker-a", 50*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	if _, err := next(pool, short, 0); !errors.Is(err, txmgr.ErrLeaseExpired) {
		t.Fatalf("expired lease: err = %v, want ErrLeaseExpired", err)
	}

	// The failed calls handed nothing out.
	renewed := acquire(t, pool, "worker-a", time.Minute)
	if got := mustNext(t, pool, renewed, 0); got != 0 {
		t.Fatalf("first nonce after failed calls %d, want 0", got)
	}
}

// TestTxMgr_ConcurrentSendersNeverShareNonce is invariant I10: many
// goroutines assigning nonces for one key at once get every nonce exactly
// once, with no gaps, and a process without the lease gets none.
func TestTxMgr_ConcurrentSendersNeverShareNonce(t *testing.T) {
	pool := testdb.New(t)
	l := acquire(t, pool, "worker-a", time.Minute)
	impostor := l
	impostor.Owner = "worker-b"

	const senders, each = 16, 10
	var (
		mu     sync.Mutex
		got    []uint64
		wg     sync.WaitGroup
		errs   = make(chan error, senders*each*2)
		stolen = make(chan uint64, senders*each)
	)
	for range senders {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range each {
				n, err := next(pool, l, 0)
				if err != nil {
					errs <- err
					return
				}
				mu.Lock()
				got = append(got, n)
				mu.Unlock()
			}
		}()
		go func() {
			defer wg.Done()
			for range each {
				n, err := next(pool, impostor, 0)
				if err == nil {
					stolen <- n
				} else if !errors.Is(err, txmgr.ErrNotLeaseHolder) {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	close(stolen)
	for err := range errs {
		t.Fatalf("sender failed: %v", err)
	}
	for n := range stolen {
		t.Fatalf("a process without the lease was given nonce %d", n)
	}
	slices.Sort(got)
	for i, n := range got {
		if n != uint64(i) {
			t.Fatalf("nonces %v: want each of 0..%d exactly once", got, senders*each-1)
		}
	}
	if len(got) != senders*each {
		t.Fatalf("%d nonces handed out, want %d", len(got), senders*each)
	}
}
