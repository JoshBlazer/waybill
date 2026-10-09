package tracking

import (
	"testing"
	"time"

	"github.com/JoshBlazer/waybill/api/internal/statemachine"
)

var t0 = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

func ev(typ string, minutes int) Event {
	return Event{Type: typ, At: t0.Add(time.Duration(minutes) * time.Minute)}
}

func statuses(steps []Step) [4]Status {
	return [4]Status{steps[0].Status, steps[1].Status, steps[2].Status, steps[3].Status}
}

func TestBuild_EveryInvoiceState(t *testing.T) {
	cases := []struct {
		state statemachine.InvoiceState
		mode  string
		want  [4]Status
	}{
		{statemachine.InvoiceOpen, "hold", [4]Status{Current, Waiting, Waiting, Waiting}},
		{statemachine.InvoiceReceived, "hold", [4]Status{Done, Current, Waiting, Waiting}},
		{statemachine.InvoiceUnderpaid, "hold", [4]Status{Done, Current, Waiting, Waiting}},
		{statemachine.InvoiceOverpaid, "hold", [4]Status{Done, Current, Waiting, Waiting}},
		// N confirmations are not "safe from being undone": still current.
		{statemachine.InvoiceConfirmed, "hold", [4]Status{Done, Current, Waiting, Waiting}},
		{statemachine.InvoiceSettled, "hold", [4]Status{Done, Done, Skipped, Skipped}},
		{statemachine.InvoiceSettled, "naira", [4]Status{Done, Done, Current, Waiting}},
		{statemachine.InvoiceExpired, "hold", [4]Status{Waiting, Waiting, Waiting, Waiting}},
		{statemachine.InvoiceCancelled, "hold", [4]Status{Waiting, Waiting, Waiting, Waiting}},
	}
	covered := map[statemachine.InvoiceState]bool{}
	for _, tc := range cases {
		covered[tc.state] = true
		if got := statuses(Build(tc.state, tc.mode, nil)); got != tc.want {
			t.Errorf("%s/%s = %v, want %v", tc.state, tc.mode, got, tc.want)
		}
	}
	for _, s := range statemachine.InvoiceStates {
		if !covered[s] {
			t.Errorf("state %s has no tracking case", s)
		}
	}
}

func TestBuild_StepsInOrderAlwaysFour(t *testing.T) {
	want := []StepName{Received, Confirmed, Converted, SentToBank}
	for _, s := range statemachine.InvoiceStates {
		steps := Build(s, "hold", nil)
		if len(steps) != 4 {
			t.Fatalf("%s: %d steps", s, len(steps))
		}
		for i := range want {
			if steps[i].Name != want[i] {
				t.Fatalf("%s: step %d is %s, want %s", s, i, steps[i].Name, want[i])
			}
		}
	}
}

func TestBuild_Times(t *testing.T) {
	events := []Event{ev(EventCreated, 0), ev(EventPaymentDetected, 3), ev(EventConfirmed, 4), ev(EventPaymentFinal, 6)}
	steps := Build(statemachine.InvoiceSettled, "hold", events)
	if steps[0].At == nil || !steps[0].At.Equal(t0.Add(3*time.Minute)) {
		t.Errorf("received at = %v", steps[0].At)
	}
	if steps[1].At == nil || !steps[1].At.Equal(t0.Add(6*time.Minute)) {
		t.Errorf("confirmed at = %v (must be the final time, not the first confirmation)", steps[1].At)
	}
}

func TestBuild_ReorgShowsChecking(t *testing.T) {
	events := []Event{ev(EventCreated, 0), ev(EventPaymentDetected, 1), ev(EventPaymentReorged, 2)}
	if got := Build(statemachine.InvoiceOpen, "hold", events)[0].Status; got != Checking {
		t.Fatalf("received after reorg = %s, want checking", got)
	}
}
