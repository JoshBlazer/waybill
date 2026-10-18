package statemachine

import (
	"errors"
	"testing"

	"pgregory.net/rapid"
)

// wantBatch restates docs/design/teams.md §5 independently of the code
// under test. Every pair not listed is illegal.
var wantBatch = map[BatchState]map[BatchEvent]BatchState{
	BatchDraft: {
		EvBatchChecked: BatchReady,
		EvBatchCancel:  BatchCancelled,
	},
	BatchReady: {
		EvBatchNeedsApproval: BatchAwaitingApproval,
		EvBatchApproved:      BatchApproved,
		EvBatchCancel:        BatchCancelled,
	},
	BatchAwaitingApproval: {
		EvBatchApproved: BatchApproved,
		EvBatchRejected: BatchRejected,
		EvBatchCancel:   BatchCancelled,
	},
	BatchApproved: {
		EvBatchSubmit: BatchSending,
		EvBatchCancel: BatchCancelled,
	},
	BatchSending: {
		EvBatchMined:    BatchConfirming,
		EvBatchReverted: BatchFailed,
	},
	BatchConfirming: {
		EvBatchFinal:      BatchPaid,
		EvBatchReverted:   BatchFailed,
		EvBatchReorgedOut: BatchSending,
	},
	BatchPaid:      {},
	BatchFailed:    {},
	BatchRejected:  {},
	BatchCancelled: {},
}

// TestBatch_AllPairs checks every (state, event) pair: 10 × 10 = 100.
func TestBatch_AllPairs(t *testing.T) {
	if len(wantBatch) != len(BatchStates) {
		t.Fatalf("spec covers %d states, machine has %d", len(wantBatch), len(BatchStates))
	}
	pairs := 0
	for _, from := range BatchStates {
		legal, ok := wantBatch[from]
		if !ok {
			t.Fatalf("spec is missing state %s", from)
		}
		for _, ev := range BatchEvents {
			pairs++
			got, err := BatchTransition(from, ev)
			if want, isLegal := legal[ev]; isLegal {
				if err != nil || got != want {
					t.Errorf("%s --%s--> got (%s, %v), want %s", from, ev, got, err, want)
				}
				continue
			}
			if !errors.Is(err, ErrIllegalTransition) {
				t.Errorf("%s --%s--> got (%s, %v), want ErrIllegalTransition", from, ev, got, err)
			}
			if got != from {
				t.Errorf("%s --%s--> an illegal transition changed the state to %s", from, ev, got)
			}
		}
	}
	if pairs != 100 {
		t.Fatalf("checked %d pairs, want 100", pairs)
	}
}

func TestBatch_TerminalStates(t *testing.T) {
	for _, s := range BatchStates {
		terminal := true
		for _, ev := range BatchEvents {
			if _, err := BatchTransition(s, ev); err == nil {
				terminal = false
			}
		}
		want := s == BatchPaid || s == BatchFailed || s == BatchRejected || s == BatchCancelled
		if terminal != want {
			t.Errorf("%s terminal = %v, want %v", s, terminal, want)
		}
	}
}

// TestBatch_RandomWalksNeverSendUnapproved applies random event sequences.
// Whatever happens: money is sent only after an approval; a run is never
// cancelled once it has been submitted; it is paid only by finality after
// being mined; a rejected run never reaches the chain; and the machine
// never leaves its known states.
func TestBatch_RandomWalksNeverSendUnapproved(t *testing.T) {
	known := map[BatchState]bool{}
	for _, s := range BatchStates {
		known[s] = true
	}
	rapid.Check(t, func(t *rapid.T) {
		s := BatchDraft
		approved, submitted, rejected := false, false, false
		events := rapid.SliceOfN(rapid.SampledFrom(BatchEvents), 0, 60).Draw(t, "events")
		for _, ev := range events {
			next, err := BatchTransition(s, ev)
			if err != nil {
				if !errors.Is(err, ErrIllegalTransition) || next != s {
					t.Fatalf("%s --%s--> (%s, %v): illegal events must change nothing", s, ev, next, err)
				}
				continue
			}
			switch ev {
			case EvBatchApproved:
				approved = true
			case EvBatchRejected:
				rejected = true
			case EvBatchSubmit:
				if !approved || rejected {
					t.Fatalf("submitted without an approval: %v", events)
				}
				submitted = true
			case EvBatchCancel:
				if submitted {
					t.Fatalf("cancelled after submission: %v", events)
				}
			}
			if next == BatchPaid && (ev != EvBatchFinal || s != BatchConfirming) {
				t.Fatalf("paid other than by finality of a mined run: %v", events)
			}
			s = next
			if !known[s] {
				t.Fatalf("reached unknown state %q", s)
			}
		}
	})
}
