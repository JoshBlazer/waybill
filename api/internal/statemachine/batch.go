package statemachine

import "fmt"

// BatchState is the lifecycle state of a batch run: an organisation paying
// several contractors' wallets in one BatchPayout transaction. See
// docs/design/teams.md §5.
type BatchState string

// Batch states.
const (
	BatchDraft            BatchState = "draft"             // built; lines not yet all passing their checks
	BatchReady            BatchState = "ready"             // every line passed its checks
	BatchAwaitingApproval BatchState = "awaiting_approval" // above the threshold: waits for another member
	BatchApproved         BatchState = "approved"          // may be sent
	BatchSending          BatchState = "sending"           // recorded before broadcast; the transaction is out or going out
	BatchConfirming       BatchState = "confirming"        // mined; waiting for finality
	BatchPaid             BatchState = "paid"              // final on-chain
	BatchFailed           BatchState = "failed"            // the transaction reverted; nothing was paid
	BatchRejected         BatchState = "rejected"          // an approver said no
	BatchCancelled        BatchState = "cancelled"         // withdrawn before it was sent
)

// BatchStates lists every state, for exhaustive tests and validation.
var BatchStates = []BatchState{
	BatchDraft, BatchReady, BatchAwaitingApproval, BatchApproved, BatchSending,
	BatchConfirming, BatchPaid, BatchFailed, BatchRejected, BatchCancelled,
}

// BatchEvent is something that happened to a batch run.
type BatchEvent string

// Batch events.
const (
	EvBatchChecked       BatchEvent = "checked"        // every line passed its checks
	EvBatchNeedsApproval BatchEvent = "needs_approval" // the total is above the organisation's threshold
	EvBatchApproved      BatchEvent = "approved"       // approved (by another member when above the threshold)
	EvBatchRejected      BatchEvent = "rejected"       // an approver refused it
	EvBatchCancel        BatchEvent = "cancel"         // withdrawn by a maker or admin
	EvBatchSubmit        BatchEvent = "submit"         // about to hand the transaction to the transaction manager
	EvBatchMined         BatchEvent = "mined"          // the transaction is in a block and succeeded
	EvBatchReverted      BatchEvent = "reverted"       // the transaction reverted
	EvBatchFinal         BatchEvent = "final"          // the block is final
	EvBatchReorgedOut    BatchEvent = "reorged_out"    // a reorg removed the block; the same transaction goes out again
)

// BatchEvents lists every event, for exhaustive tests.
var BatchEvents = []BatchEvent{
	EvBatchChecked, EvBatchNeedsApproval, EvBatchApproved, EvBatchRejected, EvBatchCancel,
	EvBatchSubmit, EvBatchMined, EvBatchReverted, EvBatchFinal, EvBatchReorgedOut,
}

type batchKey struct {
	from BatchState
	ev   BatchEvent
}

// batchTransitions is the complete table from docs/design/teams.md §5.
// Anything absent is illegal.
//
// Money leaves only through approved → sending. Once submitted, a run can
// no longer be cancelled: it ends paid or, if the transaction reverted,
// failed. Who may approve (approver ≠ creator, I17) is checked by the
// caller and in SQL; the machine only knows states.
var batchTransitions = map[batchKey]BatchState{
	{BatchDraft, EvBatchChecked}: BatchReady,
	{BatchDraft, EvBatchCancel}:  BatchCancelled,

	{BatchReady, EvBatchNeedsApproval}: BatchAwaitingApproval,
	{BatchReady, EvBatchApproved}:      BatchApproved, // at or below the threshold
	{BatchReady, EvBatchCancel}:        BatchCancelled,

	{BatchAwaitingApproval, EvBatchApproved}: BatchApproved,
	{BatchAwaitingApproval, EvBatchRejected}: BatchRejected,
	{BatchAwaitingApproval, EvBatchCancel}:   BatchCancelled,

	{BatchApproved, EvBatchSubmit}: BatchSending,
	{BatchApproved, EvBatchCancel}: BatchCancelled,

	{BatchSending, EvBatchMined}:    BatchConfirming,
	{BatchSending, EvBatchReverted}: BatchFailed,

	{BatchConfirming, EvBatchFinal}:      BatchPaid,
	{BatchConfirming, EvBatchReverted}:   BatchFailed,
	{BatchConfirming, EvBatchReorgedOut}: BatchSending,
}

// BatchTransition returns the state that follows from on event ev, or
// ErrIllegalTransition (wrapped, matched with errors.Is) with from
// unchanged for every pair not in the table. Pure: no I/O, no clock.
func BatchTransition(from BatchState, ev BatchEvent) (BatchState, error) {
	if to, ok := batchTransitions[batchKey{from, ev}]; ok {
		return to, nil
	}
	return from, fmt.Errorf("%w: batch %s on %s", ErrIllegalTransition, from, ev)
}
