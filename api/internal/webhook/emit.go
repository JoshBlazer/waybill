package webhook

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/JoshBlazer/waybill/api/internal/outbox"
	"github.com/JoshBlazer/waybill/api/internal/statemachine"
	"github.com/JoshBlazer/waybill/api/internal/store"
)

// Topic is the outbox topic for one delivery of one event to one endpoint.
const Topic = "webhook.deliver"

// Job is the outbox payload for Topic.
type Job struct {
	EventID    string    `json:"eventId"`
	EndpointID uuid.UUID `json:"endpointId"`
}

// EventType names the event for an invoice transition, after the state
// reached. A transition that leaves the state unchanged (a second payment
// while received) is not an event. ok is false when there is no event.
func EventType(from, to statemachine.InvoiceState, ev statemachine.InvoiceEvent) (string, bool) {
	if from == to {
		return "", false
	}
	switch to {
	case statemachine.InvoiceReceived, statemachine.InvoiceUnderpaid, statemachine.InvoiceOverpaid,
		statemachine.InvoiceConfirmed, statemachine.InvoiceSettled, statemachine.InvoiceExpired:
		return "invoice." + string(to), true
	case statemachine.InvoiceOpen:
		if ev == statemachine.EvPaymentReorged {
			return "invoice.reopened", true
		}
	}
	return "", false
}

// Emit records an event inside tx, the transaction that changed the
// invoice, and enqueues one delivery per enabled endpoint subscribed to its
// type. With no subscriber nothing is written.
func Emit(ctx context.Context, tx pgx.Tx, contractor, invoiceID uuid.UUID, typ string, state statemachine.InvoiceState) error {
	q := store.New(tx)
	endpoints, err := q.ListSubscribedEndpoints(ctx, store.ListSubscribedEndpointsParams{ContractorID: contractor, Type: typ})
	if err != nil {
		return fmt.Errorf("webhook: subscribers: %w", err)
	}
	if len(endpoints) == 0 {
		return nil
	}
	id := NewEventID(time.Now())
	if err := q.InsertWebhookEvent(ctx, store.InsertWebhookEventParams{
		ID: id, ContractorID: contractor, Type: typ, InvoiceID: invoiceID, InvoiceState: string(state),
	}); err != nil {
		return fmt.Errorf("webhook: event: %w", err)
	}
	for _, ep := range endpoints {
		if _, err := outbox.Enqueue(ctx, tx, Topic, Job{EventID: id, EndpointID: ep}, outbox.Options{
			DedupeKey: "webhook:" + id + ":" + ep.String(),
		}); err != nil {
			return err
		}
	}
	return nil
}
