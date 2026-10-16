package invoice

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/JoshBlazer/waybill/api/internal/statemachine"
	"github.com/JoshBlazer/waybill/api/internal/store"
	"github.com/JoshBlazer/waybill/api/internal/webhook"
)

// Apply moves an invoice through the state machine inside tx: it locks the
// row, computes the next state, stores it, records the event (which
// notifies live listeners at commit) and queues webhook deliveries for it,
// all in the caller's transaction. It returns the new state, or
// statemachine.ErrIllegalTransition with nothing written.
func Apply(ctx context.Context, tx pgx.Tx, id uuid.UUID, ev statemachine.InvoiceEvent, data map[string]any) (statemachine.InvoiceState, error) {
	q := store.New(tx)
	row, err := q.GetInvoiceForUpdate(ctx, id)
	if err != nil {
		return "", fmt.Errorf("invoice: lock %s: %w", id, err)
	}
	from := statemachine.InvoiceState(row.State)
	to, err := statemachine.InvoiceTransition(from, ev)
	if err != nil {
		return from, err
	}
	if data == nil {
		data = map[string]any{}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return from, err
	}
	if err := q.UpdateInvoiceState(ctx, store.UpdateInvoiceStateParams{ID: id, State: string(to)}); err != nil {
		return from, err
	}
	fromStr := string(from)
	if _, err := q.InsertInvoiceEvent(ctx, store.InsertInvoiceEventParams{
		InvoiceID: id, Type: string(ev), FromState: &fromStr, ToState: string(to), Data: raw,
	}); err != nil {
		return from, err
	}
	if typ, ok := webhook.EventType(from, to, ev); ok {
		if err := webhook.Emit(ctx, tx, row.ContractorID, id, typ, to); err != nil {
			return from, err
		}
	}
	return to, nil
}
