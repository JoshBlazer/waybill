package httpapi

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// notifyChannel matches the trigger in migration 00003.
const notifyChannel = "invoice_events"

// Broker fans PostgreSQL notifications out to SSE subscribers. PostgreSQL is
// the only message bus: there is no broker process (ADR-006, ADR-015).
type Broker struct {
	pool *pgxpool.Pool
	log  *slog.Logger

	mu   sync.Mutex
	subs map[uuid.UUID]map[chan struct{}]struct{}
}

// NewBroker returns a broker; call Run to start listening.
func NewBroker(pool *pgxpool.Pool, log *slog.Logger) *Broker {
	return &Broker{pool: pool, log: log, subs: make(map[uuid.UUID]map[chan struct{}]struct{})}
}

// Subscribe returns a channel that receives a signal whenever the invoice
// has a new event, and a function to unsubscribe. Signals coalesce: a slow
// reader sees one pending signal, then reloads the current state.
func (b *Broker) Subscribe(invoice uuid.UUID) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	if b.subs[invoice] == nil {
		b.subs[invoice] = make(map[chan struct{}]struct{})
	}
	b.subs[invoice][ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs[invoice], ch)
		if len(b.subs[invoice]) == 0 {
			delete(b.subs, invoice)
		}
		b.mu.Unlock()
	}
}

func (b *Broker) publish(invoice uuid.UUID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs[invoice] {
		select {
		case ch <- struct{}{}:
		default: // already signalled
		}
	}
}

// broadcastAll wakes every subscriber. Used after reconnecting, because
// notifications sent while disconnected are lost.
func (b *Broker) broadcastAll() {
	b.mu.Lock()
	ids := make([]uuid.UUID, 0, len(b.subs))
	for id := range b.subs {
		ids = append(ids, id)
	}
	b.mu.Unlock()
	for _, id := range ids {
		b.publish(id)
	}
}

// Run listens until ctx is done, reconnecting with backoff on failure.
func (b *Broker) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := b.listen(ctx)
		if ctx.Err() != nil {
			return
		}
		b.log.Warn("broker: listen failed; reconnecting", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (b *Broker) listen(ctx context.Context) error {
	conn, err := b.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+notifyChannel); err != nil {
		return err
	}
	b.broadcastAll() // anything missed while disconnected
	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		id, err := uuid.Parse(n.Payload)
		if err != nil {
			b.log.Warn("broker: bad payload", "payload", n.Payload)
			continue
		}
		b.publish(id)
	}
}
