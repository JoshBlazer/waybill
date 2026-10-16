// Package outbox runs background work written in the same transaction as
// the state change that causes it (ADR-006, docs/ARCHITECTURE.md §7).
//
// Enqueue inside the caller's transaction: if the transaction rolls back,
// the job never existed. Workers lease ready jobs with FOR UPDATE SKIP
// LOCKED, run the topic's handler outside any transaction, and record the
// outcome. Delivery is at least once (a worker can crash after the handler
// and before recording success), so every handler must be idempotent.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshBlazer/waybill/api/internal/store"
)

// Job is one leased unit of work.
type Job struct {
	ID      int64
	Topic   string
	Payload json.RawMessage
	Attempt int // 1 on the first run
}

// Handler does a job's work. It must be idempotent: check the target's
// current state before acting, and pass our ids as references to anything
// external. Returning an error schedules a retry with backoff; wrapping
// ErrPermanent gives up at once.
type Handler func(ctx context.Context, j Job) error

// ErrPermanent marks a failure that retrying cannot fix (for example, a
// payload that does not parse). The job goes straight to dead.
var ErrPermanent = errors.New("outbox: permanent failure")

// Options for Enqueue.
type Options struct {
	// DedupeKey makes enqueueing the same logical job twice a no-op.
	DedupeKey string
	// NotBefore delays the first run.
	NotBefore time.Time
}

// Enqueue adds a job inside tx. It returns false when a job with the same
// DedupeKey already exists.
func Enqueue(ctx context.Context, tx pgx.Tx, topic string, payload any, opts Options) (bool, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return false, fmt.Errorf("outbox: payload for %s: %w", topic, err)
	}
	p := store.EnqueueOutboxParams{Topic: topic, Payload: raw}
	if opts.DedupeKey != "" {
		p.DedupeKey = &opts.DedupeKey
	}
	if !opts.NotBefore.IsZero() {
		p.AvailableAt = &opts.NotBefore
	}
	_, err = store.New(tx).EnqueueOutbox(ctx, p)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("outbox: enqueue %s: %w", topic, err)
	}
	return true, nil
}

// Worker leases and runs jobs.
type Worker struct {
	Pool     *pgxpool.Pool
	Owner    string // unique per process, e.g. hostname and pid
	Handlers map[string]Handler
	Log      *slog.Logger

	Lease       time.Duration // how long a job is ours; a handler gets a little less
	Batch       int           // jobs leased per round
	MaxAttempts int           // after this many failures the job is dead
	// Backoff returns the wait before attempt+1 after a failed attempt.
	Backoff func(attempt int) time.Duration
	// Policies override MaxAttempts and Backoff per topic.
	Policies map[string]Policy
}

// Policy is a topic's retry schedule.
type Policy struct {
	MaxAttempts int
	Backoff     func(attempt int) time.Duration
}

func (w *Worker) policy(topic string) Policy {
	if p, ok := w.Policies[topic]; ok {
		return p
	}
	return Policy{MaxAttempts: w.MaxAttempts, Backoff: w.Backoff}
}

// DefaultBackoff doubles from 2 s up to 10 minutes.
func DefaultBackoff(attempt int) time.Duration {
	d := 2 * time.Second
	for i := 1; i < attempt && d < 10*time.Minute; i++ {
		d *= 2
	}
	return min(d, 10*time.Minute)
}

// RunOnce leases up to Batch ready jobs, runs each, and records the
// outcome. It returns how many jobs it ran.
func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	owner := w.Owner
	jobs, err := store.New(w.Pool).LeaseOutbox(ctx, store.LeaseOutboxParams{
		Owner: &owner, LeaseMs: w.Lease.Milliseconds(), Max: int32(w.Batch), //nolint:gosec // small configured batch
	})
	if err != nil {
		return 0, fmt.Errorf("outbox: lease: %w", err)
	}
	for _, row := range jobs {
		w.run(ctx, Job{ID: row.ID, Topic: row.Topic, Payload: row.Payload, Attempt: int(row.Attempts)})
	}
	return len(jobs), nil
}

func (w *Worker) run(ctx context.Context, j Job) {
	log := w.Log.With("job", j.ID, "topic", j.Topic, "attempt", j.Attempt)
	h, ok := w.Handlers[j.Topic]
	if !ok {
		w.record(ctx, log, j, fmt.Errorf("%w: no handler for topic %q", ErrPermanent, j.Topic))
		return
	}
	// Finish well inside the lease, so another worker does not start the
	// same job while this one is still running it.
	hctx, cancel := context.WithTimeout(ctx, w.Lease*9/10)
	defer cancel()
	w.record(ctx, log, j, runHandler(hctx, h, j))
}

// runHandler turns a handler panic into an error, so one bad job cannot
// take the worker down.
func runHandler(ctx context.Context, h Handler, j Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panicked: %v", r)
		}
	}()
	return h(ctx, j)
}

func (w *Worker) record(ctx context.Context, log *slog.Logger, j Job, herr error) {
	q := store.New(w.Pool)
	owner := w.Owner
	var (
		n   int64
		err error
	)
	switch {
	case herr == nil:
		n, err = q.CompleteOutbox(ctx, store.CompleteOutboxParams{ID: j.ID, Owner: &owner})
	case errors.Is(herr, ErrPermanent) || j.Attempt >= w.policy(j.Topic).MaxAttempts:
		msg := herr.Error()
		n, err = q.KillOutbox(ctx, store.KillOutboxParams{ID: j.ID, Owner: &owner, LastError: &msg})
		log.Error("outbox: job is dead; needs a person", "err", herr)
	default:
		msg := herr.Error()
		delay := w.policy(j.Topic).Backoff(j.Attempt)
		n, err = q.RetryOutbox(ctx, store.RetryOutboxParams{ID: j.ID, Owner: &owner, LastError: &msg, DelayMs: delay.Milliseconds()})
		log.Warn("outbox: job failed; will retry", "err", herr, "in", delay)
	}
	switch {
	case err != nil:
		// The lease expires and the job runs again: at least once.
		log.Error("outbox: could not record outcome", "err", err)
	case n == 0:
		log.Warn("outbox: lease lost before the outcome was recorded; another worker owns the job")
	}
}

// Run calls RunOnce until ctx is done, sleeping poll between empty rounds.
func (w *Worker) Run(ctx context.Context, poll time.Duration) {
	for {
		n, err := w.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			w.Log.Warn("outbox: round failed; will retry", "err", err)
		}
		if n > 0 && err == nil {
			continue // more may be ready
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(poll):
		}
	}
}
