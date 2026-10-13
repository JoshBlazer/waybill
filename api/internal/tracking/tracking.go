// Package tracking turns an invoice's state and history into the four plain
// steps on the public tracking page: received, confirmed, converted, sent to
// bank (docs/PRODUCT.md §3). Pure: no I/O.
package tracking

import (
	"time"

	"github.com/JoshBlazer/waybill/api/internal/statemachine"
)

// StepName names a tracking step. The four steps, in display order, follow.
type StepName string

// The four steps.
const (
	Received   StepName = "received"
	Confirmed  StepName = "confirmed"
	Converted  StepName = "converted"
	SentToBank StepName = "sent_to_bank"
)

// Status of one step.
type Status string

// Step statuses. Checking means "we could not find out yet" and is never
// presented as a failure.
const (
	Done     Status = "done"
	Current  Status = "current"
	Waiting  Status = "waiting"
	Checking Status = "checking"
	Skipped  Status = "skipped"
)

// Step is one row of the tracking line.
type Step struct {
	Name   StepName
	Status Status
	At     *time.Time
}

// Event is the part of an invoice event the tracking view needs.
type Event struct {
	Type string // the invoice event that caused the change
	At   time.Time
}

// Invoice event types written by the API and the watcher.
const (
	EventCreated         = "created"
	EventPaymentDetected = string(statemachine.EvPaymentDetected)
	EventConfirmed       = "confirmed" // any of the three confirmation events
	EventPaymentFinal    = string(statemachine.EvPaymentFinal)
	EventPaymentReorged  = string(statemachine.EvPaymentReorged)
)

// Build returns the four steps for an invoice in state s with payout mode
// mode ("hold" or "naira"), given its events in order.
//
// "Confirmed" means safe from being undone, which is finality (settled),
// not the first N confirmations; until then the step is current.
func Build(s statemachine.InvoiceState, mode string, events []Event) []Step {
	received := Step{Name: Received, Status: Waiting}
	confirmed := Step{Name: Confirmed, Status: Waiting}
	converted := Step{Name: Converted, Status: Waiting}
	sent := Step{Name: SentToBank, Status: Waiting}

	switch s {
	case statemachine.InvoiceOpen:
		received.Status = Current
		if last(events) == EventPaymentReorged {
			received.Status = Checking // we saw it; the network undid it
		}
	case statemachine.InvoiceReceived, statemachine.InvoiceUnderpaid,
		statemachine.InvoiceConfirmed, statemachine.InvoiceOverpaid:
		received.Status, received.At = Done, firstAt(events, EventPaymentDetected)
		confirmed.Status = Current
	case statemachine.InvoiceSettled:
		received.Status, received.At = Done, firstAt(events, EventPaymentDetected)
		confirmed.Status, confirmed.At = Done, lastAt(events, EventPaymentFinal)
		if mode == "hold" {
			converted.Status, sent.Status = Skipped, Skipped
		} else {
			converted.Status = Current // naira settlement arrives in stage 3
		}
	case statemachine.InvoiceExpired, statemachine.InvoiceCancelled:
		// Nothing is in progress; the invoice state explains why.
	}
	return []Step{received, confirmed, converted, sent}
}

func last(events []Event) string {
	if len(events) == 0 {
		return ""
	}
	return events[len(events)-1].Type
}

func firstAt(events []Event, typ string) *time.Time {
	for i := range events {
		if events[i].Type == typ {
			return &events[i].At
		}
	}
	return nil
}

func lastAt(events []Event, typ string) *time.Time {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == typ {
			return &events[i].At
		}
	}
	return nil
}
