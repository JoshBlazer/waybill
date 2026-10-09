// Package statemachine holds Waybill's state machines as pure functions:
// no I/O, no clock, no database. Callers load the current state, call
// Transition, and persist the result in the same transaction as the event
// that caused it. TestStatemachine_NoIOImports enforces the purity.
package statemachine

import (
	"errors"
	"fmt"

	"github.com/JoshBlazer/waybill/api/internal/money"
)

// ErrIllegalTransition is returned for every (state, event) pair that is not
// in a machine's transition table. Callers treat it as a bug or an alert,
// never as something to retry.
var ErrIllegalTransition = errors.New("statemachine: illegal transition")

// InvoiceState is the lifecycle state of an invoice. See
// docs/ARCHITECTURE.md §5.1.
type InvoiceState string

// Invoice states.
const (
	InvoiceOpen      InvoiceState = "open"      // waiting for payment
	InvoiceReceived  InvoiceState = "received"  // a payment is seen, not yet confirmed
	InvoiceUnderpaid InvoiceState = "underpaid" // confirmed total < amount due
	InvoiceOverpaid  InvoiceState = "overpaid"  // confirmed total > amount due
	InvoiceConfirmed InvoiceState = "confirmed" // confirmed total = amount due (or shortfall accepted)
	InvoiceSettled   InvoiceState = "settled"   // funds final and credited to the contractor
	InvoiceExpired   InvoiceState = "expired"   // no payment before expires_at
	InvoiceCancelled InvoiceState = "cancelled" // withdrawn by the contractor before payment
)

// InvoiceStates lists every state, for exhaustive tests and validation.
var InvoiceStates = []InvoiceState{
	InvoiceOpen, InvoiceReceived, InvoiceUnderpaid, InvoiceOverpaid,
	InvoiceConfirmed, InvoiceSettled, InvoiceExpired, InvoiceCancelled,
}

// InvoiceEvent is something that happened to an invoice.
type InvoiceEvent string

// Invoice events. Confirmation is split three ways because the next state
// depends on how the confirmed total compares with the amount due; use
// ClassifyConfirmation to pick one.
const (
	EvPaymentDetected      InvoiceEvent = "payment_detected"      // a payment appeared on-chain or at a provider
	EvConfirmedExact       InvoiceEvent = "confirmed_exact"       // confirmed total = due
	EvConfirmedShort       InvoiceEvent = "confirmed_short"       // confirmed total < due
	EvConfirmedOver        InvoiceEvent = "confirmed_over"        // confirmed total > due
	EvPaymentReorged       InvoiceEvent = "payment_reorged"       // a seen or confirmed payment left the canonical chain
	EvPaymentFinal         InvoiceEvent = "payment_final"         // the confirmed payment(s) reached finality
	EvUnderpaymentAccepted InvoiceEvent = "underpayment_accepted" // contractor accepts the shortfall
	EvExpire               InvoiceEvent = "expire"                // expires_at passed with no payment
	EvCancel               InvoiceEvent = "cancel"                // contractor withdraws the invoice
)

// InvoiceEvents lists every event, for exhaustive tests.
var InvoiceEvents = []InvoiceEvent{
	EvPaymentDetected, EvConfirmedExact, EvConfirmedShort, EvConfirmedOver,
	EvPaymentReorged, EvPaymentFinal, EvUnderpaymentAccepted, EvExpire, EvCancel,
}

type invoiceKey struct {
	from InvoiceState
	ev   InvoiceEvent
}

// invoiceTransitions is the complete table. Anything absent is illegal.
//
// On a reorg the invoice returns to open, whatever it was before: the
// watcher then rescans and replays detection and confirmation events for
// the payments that survived, so the invoice re-derives its state from what
// is actually on the canonical chain. A reorg after settled means finality
// was violated; it is deliberately illegal so it surfaces as an alert.
var invoiceTransitions = map[invoiceKey]InvoiceState{
	{InvoiceOpen, EvPaymentDetected}: InvoiceReceived,
	{InvoiceOpen, EvExpire}:          InvoiceExpired,
	{InvoiceOpen, EvCancel}:          InvoiceCancelled,

	{InvoiceReceived, EvPaymentDetected}: InvoiceReceived, // a second payment while the first is unconfirmed
	{InvoiceReceived, EvConfirmedExact}:  InvoiceConfirmed,
	{InvoiceReceived, EvConfirmedShort}:  InvoiceUnderpaid,
	{InvoiceReceived, EvConfirmedOver}:   InvoiceOverpaid,
	{InvoiceReceived, EvPaymentReorged}:  InvoiceOpen,

	{InvoiceUnderpaid, EvPaymentDetected}:      InvoiceReceived, // a top-up arrives
	{InvoiceUnderpaid, EvUnderpaymentAccepted}: InvoiceConfirmed,
	{InvoiceUnderpaid, EvPaymentReorged}:       InvoiceOpen,

	{InvoiceOverpaid, EvPaymentFinal}:   InvoiceSettled,
	{InvoiceOverpaid, EvPaymentReorged}: InvoiceOpen,

	{InvoiceConfirmed, EvPaymentFinal}:   InvoiceSettled,
	{InvoiceConfirmed, EvPaymentReorged}: InvoiceOpen,

	{InvoiceExpired, EvPaymentDetected}: InvoiceReceived, // late payment: accepted, rate re-quoted
}

// InvoiceTransition returns the state that follows `from` on event `ev`, or
// ErrIllegalTransition.
func InvoiceTransition(from InvoiceState, ev InvoiceEvent) (InvoiceState, error) {
	if to, ok := invoiceTransitions[invoiceKey{from, ev}]; ok {
		return to, nil
	}
	return from, fmt.Errorf("%w: invoice %s on %s", ErrIllegalTransition, from, ev)
}

// IsTerminal reports whether no event can move the invoice out of s.
func (s InvoiceState) IsTerminal() bool {
	for _, ev := range InvoiceEvents {
		if _, ok := invoiceTransitions[invoiceKey{s, ev}]; ok {
			return false
		}
	}
	return true
}

// ClassifyConfirmation picks the confirmation event from the total confirmed
// so far and the amount due. Both are in minor units of the invoice asset.
func ClassifyConfirmation(confirmedTotal, due money.Amount) InvoiceEvent {
	switch confirmedTotal.Cmp(due) {
	case 0:
		return EvConfirmedExact
	case -1:
		return EvConfirmedShort
	default:
		return EvConfirmedOver
	}
}
