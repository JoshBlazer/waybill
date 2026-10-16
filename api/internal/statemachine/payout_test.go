package statemachine

import (
	"errors"
	"testing"

	"pgregory.net/rapid"
)

// wantPayout restates docs/ARCHITECTURE.md §5.2 independently of the code
// under test. Every pair not listed is illegal.
var wantPayout = map[PayoutState]map[PayoutEvent]PayoutState{
	PayoutQueued: {
		EvConverted:     PayoutConverted,
		EvHoldRequested: PayoutHeld,
		EvPayoutCancel:  PayoutCancelled,
	},
	PayoutConverted: {
		EvSubmit:       PayoutSubmitting,
		EvPayoutCancel: PayoutCancelled,
	},
	PayoutSubmitting: {
		EvProviderAccepted: PayoutSubmitted,
		EvProviderRejected: PayoutFailed,
		EvProviderUnknown:  PayoutUnknown,
	},
	PayoutUnknown: {
		EvReconciledAccepted: PayoutSubmitted,
		EvReconciledNotFound: PayoutConverted,
	},
	PayoutSubmitted: {
		EvProviderSucceeded: PayoutSucceeded,
		EvProviderFailed:    PayoutFailed,
	},
	PayoutSucceeded: {
		EvProviderReversed: PayoutReversed,
	},
	PayoutFailed: {
		EvRetryApproved: PayoutConverted,
	},
	PayoutHeld:      {},
	PayoutReversed:  {},
	PayoutCancelled: {},
}

// TestPayout_AllPairs checks every (state, event) pair: 10 × 13 = 130.
// Invariant I7.
func TestPayout_AllPairs(t *testing.T) {
	if len(wantPayout) != len(PayoutStates) {
		t.Fatalf("spec covers %d states, machine has %d", len(wantPayout), len(PayoutStates))
	}
	pairs := 0
	for _, from := range PayoutStates {
		legal, ok := wantPayout[from]
		if !ok {
			t.Fatalf("spec is missing state %s", from)
		}
		for _, ev := range PayoutEvents {
			pairs++
			got, err := PayoutTransition(from, ev)
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
	if pairs != 130 {
		t.Fatalf("checked %d pairs, want 130", pairs)
	}
}

// TestPayout_ProviderTimeoutGoesUnknownNotFailed is invariant I12 for
// payouts: no answer from the provider is never "failed" (which would
// invite a resubmission that could pay twice) and never "submitted".
func TestPayout_ProviderTimeoutGoesUnknownNotFailed(t *testing.T) {
	got, err := PayoutTransition(PayoutSubmitting, EvProviderUnknown)
	if err != nil || got != PayoutUnknown {
		t.Fatalf("submitting + provider_unknown = (%s, %v), want unknown", got, err)
	}
	// From unknown the only ways out are what reconciliation found.
	for _, ev := range PayoutEvents {
		next, err := PayoutTransition(PayoutUnknown, ev)
		if err != nil {
			continue
		}
		if ev != EvReconciledAccepted && ev != EvReconciledNotFound {
			t.Errorf("unknown --%s--> %s: only reconciliation may move an unknown payout", ev, next)
		}
		if next == PayoutFailed || next == PayoutSubmitting {
			t.Errorf("unknown --%s--> %s: unknown must never become failed or be resubmitted directly", ev, next)
		}
	}
}

func TestPayout_TerminalStates(t *testing.T) {
	for _, s := range PayoutStates {
		terminal := true
		for _, ev := range PayoutEvents {
			if _, err := PayoutTransition(s, ev); err == nil {
				terminal = false
			}
		}
		want := s == PayoutHeld || s == PayoutReversed || s == PayoutCancelled
		if terminal != want {
			t.Errorf("%s terminal = %v, want %v", s, terminal, want)
		}
	}
}

// TestPayout_RandomWalksNeverPayWithoutTheProvider applies random event
// sequences. Whatever happens, money reaches succeeded only through a
// provider acceptance (direct or found by reconciliation) followed by a
// provider success; a payout is never cancelled while the provider holds an
// accepted transfer; and the machine never leaves its known states.
// (Cancelling after a submission is fine once reconciliation found nothing
// or the transfer failed: in both cases nothing was sent.)
func TestPayout_RandomWalksNeverPayWithoutTheProvider(t *testing.T) {
	known := map[PayoutState]bool{}
	for _, s := range PayoutStates {
		known[s] = true
	}
	rapid.Check(t, func(t *rapid.T) {
		s := PayoutQueued
		accepted := false
		events := rapid.SliceOfN(rapid.SampledFrom(PayoutEvents), 0, 60).Draw(t, "events")
		for _, ev := range events {
			next, err := PayoutTransition(s, ev)
			if err != nil {
				if !errors.Is(err, ErrIllegalTransition) || next != s {
					t.Fatalf("%s --%s--> (%s, %v): illegal events must change nothing", s, ev, next, err)
				}
				continue
			}
			switch ev {
			case EvProviderAccepted, EvReconciledAccepted:
				accepted = true
			case EvReconciledNotFound, EvProviderRejected, EvProviderFailed:
				accepted = false
			case EvPayoutCancel:
				if accepted {
					t.Fatalf("cancelled while the provider holds an accepted transfer: %v", events)
				}
			}
			if next == PayoutSucceeded && (!accepted || ev != EvProviderSucceeded) {
				t.Fatalf("succeeded without a provider acceptance and success: %v", events)
			}
			s = next
			if !known[s] {
				t.Fatalf("reached unknown state %q", s)
			}
		}
	})
}
