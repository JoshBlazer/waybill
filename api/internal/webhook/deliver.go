package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshBlazer/waybill/api/internal/outbox"
	"github.com/JoshBlazer/waybill/api/internal/store"
)

// Timeout is how long an endpoint has to answer.
const Timeout = 10 * time.Second

// MaxAttempts and Backoff give about a day of retries: 30 s doubling to a
// 4 h ceiling, 15 attempts in all (about 24.5 h from the first).
const MaxAttempts = 15

// Backoff returns the wait after a failed attempt.
func Backoff(attempt int) time.Duration {
	d := 30 * time.Second
	for i := 1; i < attempt && d < 4*time.Hour; i++ {
		d *= 2
	}
	return min(d, 4*time.Hour)
}

// Policy is the outbox retry schedule for Topic.
func Policy() outbox.Policy { return outbox.Policy{MaxAttempts: MaxAttempts, Backoff: Backoff} }

// Renderer returns the invoice as the API shows it, with the given state
// (the state when the event was created).
type Renderer func(ctx context.Context, invoiceID uuid.UUID, state string) (any, error)

// Event is the body POSTed to an endpoint: the WebhookEvent schema in
// openapi/waybill.yaml.
type Event struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"createdAt"`
	Data      EventData `json:"data"`
}

// EventData holds the invoice.
type EventData struct {
	Invoice any `json:"invoice"`
}

// Deliverer is the outbox handler for Topic.
type Deliverer struct {
	Pool   *pgxpool.Pool
	Render Renderer
	Client *http.Client
	Log    *slog.Logger
	Now    func() time.Time // nil means time.Now
}

func (d *Deliverer) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// Handle delivers one event to one endpoint and logs the attempt. An error
// return makes the outbox retry; ErrPermanent (wrapped) stops retries.
func (d *Deliverer) Handle(ctx context.Context, j outbox.Job) error {
	var job Job
	if err := json.Unmarshal(j.Payload, &job); err != nil {
		return fmt.Errorf("%w: payload: %w", outbox.ErrPermanent, err)
	}
	q := store.New(d.Pool)
	ep, err := q.GetWebhookEndpoint(ctx, job.EndpointID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: endpoint %s not found", outbox.ErrPermanent, job.EndpointID)
	}
	if err != nil {
		return err
	}
	if ep.DisabledAt != nil {
		return nil // disabled since the event: nothing to send, nothing to retry
	}
	ev, err := q.GetWebhookEvent(ctx, job.EventID)
	if err != nil {
		return fmt.Errorf("event %s: %w", job.EventID, err)
	}
	inv, err := d.Render(ctx, ev.InvoiceID, ev.InvoiceState)
	if err != nil {
		return fmt.Errorf("render invoice %s: %w", ev.InvoiceID, err)
	}
	body, err := json.Marshal(Event{ID: ev.ID, Type: ev.Type, CreatedAt: ev.CreatedAt.UTC(), Data: EventData{Invoice: inv}})
	if err != nil {
		return fmt.Errorf("%w: body: %w", outbox.ErrPermanent, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.Url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: request: %w", outbox.ErrPermanent, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Waybill-Webhooks/1")
	req.Header.Set(SignatureHeader, Sign(ep.Secret, d.now(), body))

	start := time.Now()
	resp, herr := d.Client.Do(req)
	latency := int32(min(time.Since(start).Milliseconds(), 1<<31-1)) //nolint:gosec // clamped
	a := store.InsertWebhookDeliveryParams{
		EndpointID: ep.ID, EventID: ev.ID, Attempt: int32(j.Attempt), LatencyMs: &latency, //nolint:gosec // attempts are small
	}
	var result error
	switch {
	case errors.Is(herr, ErrRefused):
		a.Outcome = "refused"
		a.LatencyMs = nil
		result = fmt.Errorf("%w: %w", outbox.ErrPermanent, herr)
	case herr != nil:
		a.Outcome = "failed"
		result = fmt.Errorf("deliver: %w", herr)
	default:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		status := int32(resp.StatusCode) //nolint:gosec // HTTP status codes are small
		a.ResponseStatus = &status
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			a.Outcome = "succeeded"
		} else {
			a.Outcome = "failed"
			result = fmt.Errorf("deliver: endpoint answered %d", resp.StatusCode)
		}
	}
	if result != nil {
		msg := result.Error()
		a.Error = &msg
		if a.Outcome == "failed" && j.Attempt < MaxAttempts {
			next := d.now().Add(Backoff(j.Attempt))
			a.NextAttemptAt = &next
		}
	}
	if err := q.InsertWebhookDelivery(ctx, a); err != nil {
		d.Log.Error("webhook: could not log delivery attempt", "event", ev.ID, "endpoint", ep.ID, "err", err)
	}
	return result
}
