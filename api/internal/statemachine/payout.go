package statemachine

import "fmt"

// PayoutState is the lifecycle state of a payout: the conversion of a
// settled invoice's stablecoin to naira and the bank transfer that follows.
// See docs/ARCHITECTURE.md §5.2 and docs/design/payout.md.
type PayoutState string

// Payout states.
const (
	PayoutQueued     PayoutState = "queued"     // created when the invoice settles; nothing done yet
	PayoutConverted  PayoutState = "converted"  // stablecoin converted to naira at the locked rate (ledger)
	PayoutHeld       PayoutState = "held"       // the contractor keeps the stablecoin; nothing to send
	PayoutSubmitting PayoutState = "submitting" // a transfer request to the provider is in flight
	PayoutSubmitted  PayoutState = "submitted"  // the provider accepted the transfer; waiting for the bank
	PayoutUnknown    PayoutState = "unknown"    // the provider did not answer: we do not know if it was accepted
	PayoutSucceeded  PayoutState = "succeeded"  // the bank received the naira
	PayoutFailed     PayoutState = "failed"     // the provider or bank refused; nothing was sent
	PayoutReversed   PayoutState = "reversed"   // a succeeded transfer was reversed by the provider
	PayoutCancelled  PayoutState = "cancelled"  // withdrawn before anything was sent
)

// PayoutStates lists every state, for exhaustive tests and validation.
var PayoutStates = []PayoutState{
	PayoutQueued, PayoutConverted, PayoutHeld, PayoutSubmitting, PayoutSubmitted,
	PayoutUnknown, PayoutSucceeded, PayoutFailed, PayoutReversed, PayoutCancelled,
}

// PayoutEvent is something that happened to a payout.
type PayoutEvent string

// Payout events.
const (
	EvConverted          PayoutEvent = "converted"            // the conversion was posted to the ledger
	EvHoldRequested      PayoutEvent = "hold_requested"       // the contractor chose to keep the stablecoin
	EvSubmit             PayoutEvent = "submit"               // the worker is about to call the provider
	EvProviderAccepted   PayoutEvent = "provider_accepted"    // the provider accepted the transfer request
	EvProviderRejected   PayoutEvent = "provider_rejected"    // the provider refused the request (bad details, no balance)
	EvProviderUnknown    PayoutEvent = "provider_unknown"     // timeout, 5xx or connection error: no answer
	EvReconciledAccepted PayoutEvent = "reconciled_accepted"  // asked by reference: the provider has the transfer
	EvReconciledNotFound PayoutEvent = "reconciled_not_found" // asked by reference: the provider has no such transfer
	EvProviderSucceeded  PayoutEvent = "provider_succeeded"   // the bank credited the account
	EvProviderFailed     PayoutEvent = "provider_failed"      // the bank or provider failed the accepted transfer
	EvProviderReversed   PayoutEvent = "provider_reversed"    // a succeeded transfer was reversed
	EvRetryApproved      PayoutEvent = "retry_approved"       // a person approved retrying a failed payout
	EvPayoutCancel       PayoutEvent = "cancel"               // withdrawn before submission
)

// PayoutEvents lists every event, for exhaustive tests.
var PayoutEvents = []PayoutEvent{
	EvConverted, EvHoldRequested, EvSubmit, EvProviderAccepted, EvProviderRejected,
	EvProviderUnknown, EvReconciledAccepted, EvReconciledNotFound, EvProviderSucceeded,
	EvProviderFailed, EvProviderReversed, EvRetryApproved, EvPayoutCancel,
}

type payoutKey struct {
	from PayoutState
	ev   PayoutEvent
}

// payoutTransitions is the complete table from docs/ARCHITECTURE.md §5.2
// and docs/design/payout.md. Anything absent is illegal.
//
// A provider timeout is unknown, never failed (I12), and only
// reconciliation by reference leaves unknown. Cancelling is possible only
// while nothing is with the provider.
var payoutTransitions = map[payoutKey]PayoutState{
	{PayoutQueued, EvConverted}:     PayoutConverted,
	{PayoutQueued, EvHoldRequested}: PayoutHeld,
	{PayoutQueued, EvPayoutCancel}:  PayoutCancelled,

	{PayoutConverted, EvSubmit}:       PayoutSubmitting,
	{PayoutConverted, EvPayoutCancel}: PayoutCancelled,

	{PayoutSubmitting, EvProviderAccepted}: PayoutSubmitted,
	{PayoutSubmitting, EvProviderRejected}: PayoutFailed,
	{PayoutSubmitting, EvProviderUnknown}:  PayoutUnknown,

	{PayoutUnknown, EvReconciledAccepted}: PayoutSubmitted,
	{PayoutUnknown, EvReconciledNotFound}: PayoutConverted, // resubmitted with the same reference

	{PayoutSubmitted, EvProviderSucceeded}: PayoutSucceeded,
	{PayoutSubmitted, EvProviderFailed}:    PayoutFailed,

	{PayoutSucceeded, EvProviderReversed}: PayoutReversed,

	{PayoutFailed, EvRetryApproved}: PayoutConverted,
}

// PayoutTransition returns the state that follows from on event ev, or
// ErrIllegalTransition (wrapped, matched with errors.Is) with from
// unchanged for every pair not in the table. Pure: no I/O, no clock.
func PayoutTransition(from PayoutState, ev PayoutEvent) (PayoutState, error) {
	if to, ok := payoutTransitions[payoutKey{from, ev}]; ok {
		return to, nil
	}
	return from, fmt.Errorf("%w: payout %s on %s", ErrIllegalTransition, from, ev)
}
