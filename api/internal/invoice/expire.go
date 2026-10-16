package invoice

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshBlazer/waybill/api/internal/statemachine"
	"github.com/JoshBlazer/waybill/api/internal/store"
)

// expireBatch bounds one ExpireDue call; the next call picks up the rest.
const expireBatch = 100

// ExpireDue moves open invoices whose expires_at is at or before now to
// expired, each in its own transaction, and returns how many it moved. An
// invoice that a payment moved on in the meantime is left alone: Apply
// locks the row and the state machine rejects Expire on anything but open.
// Expired invoices are still watched, so a late payment is accepted
// (expired → received), never dropped.
func ExpireDue(ctx context.Context, pool *pgxpool.Pool, now time.Time) (int, error) {
	ids, err := store.New(pool).ListDueInvoices(ctx, store.ListDueInvoicesParams{Now: now, Max: expireBatch})
	if err != nil {
		return 0, fmt.Errorf("invoice: list due: %w", err)
	}
	n := 0
	for _, id := range ids {
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			_, err := Apply(ctx, tx, id, statemachine.EvExpire, map[string]any{})
			return err
		})
		switch {
		case errors.Is(err, statemachine.ErrIllegalTransition):
			// Paid between the list and the lock.
		case err != nil:
			return n, fmt.Errorf("invoice: expire %s: %w", id, err)
		default:
			n++
		}
	}
	return n, nil
}

// RunExpiry calls ExpireDue every interval until ctx is done. Failures are
// logged and retried on the next tick.
func RunExpiry(ctx context.Context, pool *pgxpool.Pool, interval time.Duration, log *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		n, err := ExpireDue(ctx, pool, time.Now())
		switch {
		case err != nil && ctx.Err() == nil:
			log.Warn("expiry: failed; will retry", "err", err)
		case n > 0:
			log.Info("expiry: invoices expired", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
