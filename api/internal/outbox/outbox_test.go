package outbox_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshBlazer/waybill/api/internal/outbox"
	"github.com/JoshBlazer/waybill/api/internal/testdb"
)

func worker(pool *pgxpool.Pool, owner string, handlers map[string]outbox.Handler) *outbox.Worker {
	return &outbox.Worker{
		Pool: pool, Owner: owner, Handlers: handlers, Log: slog.New(slog.DiscardHandler),
		Lease: time.Minute, Batch: 10, MaxAttempts: 3,
		Backoff: func(int) time.Duration { return 0 },
	}
}

func enqueue(t *testing.T, pool *pgxpool.Pool, topic string, payload any, opts outbox.Options) bool {
	t.Helper()
	var added bool
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		var err error
		added, err = outbox.Enqueue(context.Background(), tx, topic, payload, opts)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return added
}

type row struct {
	attempts     int
	done, dead   bool
	lastError    string
	leaseOwner   string
	availableNow bool
}

func job(t *testing.T, pool *pgxpool.Pool, id int64) row {
	t.Helper()
	var r row
	var lastErr, owner *string
	if err := pool.QueryRow(context.Background(), `
		SELECT attempts, done_at IS NOT NULL, dead_at IS NOT NULL, last_error, lease_owner, available_at <= now()
		FROM outbox WHERE id = $1`, id).Scan(&r.attempts, &r.done, &r.dead, &lastErr, &owner, &r.availableNow); err != nil {
		t.Fatal(err)
	}
	if lastErr != nil {
		r.lastError = *lastErr
	}
	if owner != nil {
		r.leaseOwner = *owner
	}
	return r
}

func TestOutbox_RunsJobOnceAndMarksDone(t *testing.T) {
	pool := testdb.New(t)
	enqueue(t, pool, "test.echo", map[string]string{"hello": "world"}, outbox.Options{})
	var got []string
	w := worker(pool, "w1", map[string]outbox.Handler{"test.echo": func(_ context.Context, j outbox.Job) error {
		got = append(got, string(j.Payload))
		return nil
	}})
	for range 3 {
		if _, err := w.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 1 || got[0] != `{"hello": "world"}` {
		t.Fatalf("handler saw %q; want the payload exactly once", got)
	}
	if r := job(t, pool, 1); !r.done || r.attempts != 1 || r.leaseOwner != "" {
		t.Fatalf("job %+v; want done after one attempt with no lease", r)
	}
}

func TestOutbox_EnqueueRollsBackWithItsTransaction(t *testing.T) {
	pool := testdb.New(t)
	errStateChangeFailed := errors.New("state change failed")
	err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		if _, err := outbox.Enqueue(context.Background(), tx, "test.echo", 1, outbox.Options{}); err != nil {
			return err
		}
		return errStateChangeFailed
	})
	if !errors.Is(err, errStateChangeFailed) {
		t.Fatal(err)
	}
	var n int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d jobs after rollback; a job must not outlive the change that caused it", n)
	}
}

func TestOutbox_DedupeKeyEnqueuesOnce(t *testing.T) {
	pool := testdb.New(t)
	if !enqueue(t, pool, "test.echo", 1, outbox.Options{DedupeKey: "sweep:abc"}) {
		t.Fatal("first enqueue reported a duplicate")
	}
	if enqueue(t, pool, "test.echo", 2, outbox.Options{DedupeKey: "sweep:abc"}) {
		t.Fatal("second enqueue with the same key was added")
	}
	var n int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d jobs; want 1", n)
	}
}

func TestOutbox_NotBeforeDelaysTheJob(t *testing.T) {
	pool := testdb.New(t)
	enqueue(t, pool, "test.echo", 1, outbox.Options{NotBefore: time.Now().Add(time.Hour)})
	ran := 0
	w := worker(pool, "w1", map[string]outbox.Handler{"test.echo": func(context.Context, outbox.Job) error { ran++; return nil }})
	if n, err := w.RunOnce(context.Background()); err != nil || n != 0 || ran != 0 {
		t.Fatalf("RunOnce = %d, %v (ran %d); a job due in an hour must wait", n, err, ran)
	}
}

func TestOutbox_FailuresRetryThenGoDead(t *testing.T) {
	pool := testdb.New(t)
	enqueue(t, pool, "test.flaky", 1, outbox.Options{})
	calls := 0
	w := worker(pool, "w1", map[string]outbox.Handler{"test.flaky": func(context.Context, outbox.Job) error {
		calls++
		return fmt.Errorf("provider timeout %d", calls)
	}})
	ctx := context.Background()
	for range 5 {
		if _, err := w.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 3 {
		t.Fatalf("handler ran %d times; want MaxAttempts = 3", calls)
	}
	if r := job(t, pool, 1); !r.dead || r.done || r.lastError != "provider timeout 3" {
		t.Fatalf("job %+v; want dead with the last error kept", r)
	}
}

func TestOutbox_RetryWaitsForBackoff(t *testing.T) {
	pool := testdb.New(t)
	enqueue(t, pool, "test.flaky", 1, outbox.Options{})
	w := worker(pool, "w1", map[string]outbox.Handler{"test.flaky": func(context.Context, outbox.Job) error { return errors.New("try later") }})
	w.Backoff = outbox.DefaultBackoff
	ctx := context.Background()
	if n, _ := w.RunOnce(ctx); n != 1 {
		t.Fatalf("first round ran %d jobs; want 1", n)
	}
	if n, _ := w.RunOnce(ctx); n != 0 {
		t.Fatalf("second round ran %d jobs; the retry must wait for its backoff", n)
	}
	if r := job(t, pool, 1); r.availableNow || r.dead || r.leaseOwner != "" {
		t.Fatalf("job %+v; want scheduled for later and unleased", r)
	}
}

func TestOutbox_PermanentErrorAndUnknownTopicGoDeadAtOnce(t *testing.T) {
	pool := testdb.New(t)
	enqueue(t, pool, "test.bad", 1, outbox.Options{})
	enqueue(t, pool, "test.nobody", 1, outbox.Options{})
	w := worker(pool, "w1", map[string]outbox.Handler{"test.bad": func(context.Context, outbox.Job) error {
		return fmt.Errorf("payload does not parse: %w", outbox.ErrPermanent)
	}})
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	for id := int64(1); id <= 2; id++ {
		if r := job(t, pool, id); !r.dead || r.attempts != 1 {
			t.Fatalf("job %d: %+v; want dead after one attempt", id, r)
		}
	}
}

func TestOutbox_HandlerPanicIsAFailedAttempt(t *testing.T) {
	pool := testdb.New(t)
	enqueue(t, pool, "test.panic", 1, outbox.Options{})
	w := worker(pool, "w1", map[string]outbox.Handler{"test.panic": func(context.Context, outbox.Job) error { panic("boom") }})
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := job(t, pool, 1); r.done || r.dead || r.lastError != "handler panicked: boom" {
		t.Fatalf("job %+v; want a retryable failure", r)
	}
}

// A worker leases a job and crashes. Nobody else may take the job while the
// lease lasts; once it expires, another worker runs it.
func TestOutbox_CrashedWorkersJobIsTakenOverAfterLease(t *testing.T) {
	pool := testdb.New(t)
	enqueue(t, pool, "test.echo", 1, outbox.Options{})
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		UPDATE outbox SET lease_owner = 'crashed', lease_expires_at = now() + interval '300 milliseconds', attempts = 1`); err != nil {
		t.Fatal(err)
	}
	ran := 0
	w := worker(pool, "w2", map[string]outbox.Handler{"test.echo": func(context.Context, outbox.Job) error { ran++; return nil }})
	if n, _ := w.RunOnce(ctx); n != 0 {
		t.Fatal("took a job whose lease is still live")
	}
	time.Sleep(500 * time.Millisecond)
	if n, err := w.RunOnce(ctx); err != nil || n != 1 || ran != 1 {
		t.Fatalf("RunOnce = %d, %v (ran %d); want the job taken over", n, err, ran)
	}
	if r := job(t, pool, 1); !r.done || r.attempts != 2 {
		t.Fatalf("job %+v; want done on attempt 2", r)
	}
}

// A slow worker whose lease ran out must not mark done a job that another
// worker now owns.
func TestOutbox_LostLeaseCannotRecordOutcome(t *testing.T) {
	pool := testdb.New(t)
	enqueue(t, pool, "test.slow", 1, outbox.Options{})
	ctx := context.Background()
	w := worker(pool, "slow", map[string]outbox.Handler{"test.slow": func(ctx context.Context, _ outbox.Job) error {
		// Another worker takes over meanwhile.
		_, err := pool.Exec(ctx, `UPDATE outbox SET lease_owner = 'fast', lease_expires_at = now() + interval '1 minute'`)
		return err
	}})
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if r := job(t, pool, 1); r.done || r.leaseOwner != "fast" {
		t.Fatalf("job %+v; the slow worker overwrote the new owner's job", r)
	}
}

// Many workers at once: every job runs, and no job runs twice.
func TestOutbox_ConcurrentWorkersRunEachJobOnce(t *testing.T) {
	pool := testdb.New(t)
	const jobs, workers = 200, 8
	for i := range jobs {
		enqueue(t, pool, "test.count", i, outbox.Options{})
	}
	var mu sync.Mutex
	seen := map[int64]int{}
	var total atomic.Int64
	h := func(_ context.Context, j outbox.Job) error {
		mu.Lock()
		seen[j.ID]++
		mu.Unlock()
		total.Add(1)
		return nil
	}
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := worker(pool, fmt.Sprintf("w%d", i), map[string]outbox.Handler{"test.count": h})
			w.Batch = 7
			for {
				n, err := w.RunOnce(context.Background())
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()
	if total.Load() != jobs || len(seen) != jobs {
		t.Fatalf("%d runs over %d distinct jobs; want %d and %d", total.Load(), len(seen), jobs, jobs)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("job %d ran %d times", id, n)
		}
	}
}

func TestDefaultBackoff(t *testing.T) {
	want := map[int]time.Duration{1: 2 * time.Second, 2: 4 * time.Second, 5: 32 * time.Second, 20: 10 * time.Minute}
	for attempt, d := range want {
		if got := outbox.DefaultBackoff(attempt); got != d {
			t.Errorf("DefaultBackoff(%d) = %v, want %v", attempt, got, d)
		}
	}
}

func TestOutbox_TopicPolicyOverridesDefaults(t *testing.T) {
	pool := testdb.New(t)
	enqueue(t, pool, "test.patient", 1, outbox.Options{})
	calls := 0
	w := worker(pool, "w1", map[string]outbox.Handler{"test.patient": func(context.Context, outbox.Job) error {
		calls++
		return errors.New("not yet")
	}})
	w.Policies = map[string]outbox.Policy{"test.patient": {MaxAttempts: 5, Backoff: func(int) time.Duration { return 0 }}}
	for range 8 {
		if _, err := w.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 5 {
		t.Fatalf("handler ran %d times; want the topic's 5, not the default 3", calls)
	}
}
