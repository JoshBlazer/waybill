package statemachine

import (
	"errors"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/JoshBlazer/waybill/api/internal/money"
)

// wantInvoice is written out independently of invoiceTransitions on purpose:
// the test restates the specification rather than reading it back from the
// code under test.
var wantInvoice = map[InvoiceState]map[InvoiceEvent]InvoiceState{
	InvoiceOpen: {
		EvPaymentDetected: InvoiceReceived,
		EvExpire:          InvoiceExpired,
		EvCancel:          InvoiceCancelled,
	},
	InvoiceReceived: {
		EvPaymentDetected: InvoiceReceived,
		EvConfirmedExact:  InvoiceConfirmed,
		EvConfirmedShort:  InvoiceUnderpaid,
		EvConfirmedOver:   InvoiceOverpaid,
		EvPaymentReorged:  InvoiceOpen,
	},
	InvoiceUnderpaid: {
		EvPaymentDetected:      InvoiceReceived,
		EvUnderpaymentAccepted: InvoiceConfirmed,
		EvPaymentReorged:       InvoiceOpen,
	},
	InvoiceOverpaid: {
		EvPaymentFinal:   InvoiceSettled,
		EvPaymentReorged: InvoiceOpen,
	},
	InvoiceConfirmed: {
		EvPaymentFinal:   InvoiceSettled,
		EvPaymentReorged: InvoiceOpen,
	},
	InvoiceExpired: {
		EvPaymentDetected: InvoiceReceived,
	},
	InvoiceSettled:   {},
	InvoiceCancelled: {},
}

// TestInvoice_AllPairs checks every (state, event) pair: 8 × 9 = 72 cases.
func TestInvoice_AllPairs(t *testing.T) {
	if len(wantInvoice) != len(InvoiceStates) {
		t.Fatalf("spec covers %d states, machine has %d", len(wantInvoice), len(InvoiceStates))
	}
	pairs := 0
	for _, from := range InvoiceStates {
		legal, ok := wantInvoice[from]
		if !ok {
			t.Fatalf("spec is missing state %s", from)
		}
		for _, ev := range InvoiceEvents {
			pairs++
			got, err := InvoiceTransition(from, ev)
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
				t.Errorf("%s --%s--> illegal transition changed state to %s", from, ev, got)
			}
		}
	}
	if pairs != 72 {
		t.Fatalf("checked %d pairs, want 72", pairs)
	}
}

func TestInvoice_TerminalStates(t *testing.T) {
	for _, s := range InvoiceStates {
		want := s == InvoiceSettled || s == InvoiceCancelled
		if s.IsTerminal() != want {
			t.Errorf("%s.IsTerminal() = %v, want %v", s, s.IsTerminal(), want)
		}
	}
}

func TestInvoice_ReorgAfterSettledIsIllegal(t *testing.T) {
	// Finality was violated. This must surface, not silently reopen.
	if _, err := InvoiceTransition(InvoiceSettled, EvPaymentReorged); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("settled + reorg = %v, want ErrIllegalTransition", err)
	}
}

// TestInvoice_RandomWalksStayInSpec applies random event sequences and
// checks that the machine never leaves the set of known states, and that
// reaching settled requires a confirmation and finality along the way.
func TestInvoice_RandomWalksStayInSpec(t *testing.T) {
	known := map[InvoiceState]bool{}
	for _, s := range InvoiceStates {
		known[s] = true
	}
	rapid.Check(t, func(t *rapid.T) {
		s := InvoiceOpen
		sawConfirm, sawFinal := false, false
		events := rapid.SliceOfN(rapid.SampledFrom(InvoiceEvents), 0, 40).Draw(t, "events")
		for _, ev := range events {
			next, err := InvoiceTransition(s, ev)
			if err != nil {
				continue // illegal events are rejected and change nothing
			}
			switch ev {
			case EvConfirmedExact, EvConfirmedOver, EvUnderpaymentAccepted:
				sawConfirm = true
			case EvPaymentReorged:
				sawConfirm = false
			case EvPaymentFinal:
				sawFinal = true
			}
			s = next
			if !known[s] {
				t.Fatalf("reached unknown state %q", s)
			}
		}
		if s == InvoiceSettled && (!sawConfirm || !sawFinal) {
			t.Fatalf("settled without a surviving confirmation and finality: %v", events)
		}
	})
}

func TestClassifyConfirmation(t *testing.T) {
	due := money.FromInt64(100_000_000)
	cases := []struct {
		paid int64
		want InvoiceEvent
	}{
		{100_000_000, EvConfirmedExact},
		{99_999_999, EvConfirmedShort},
		{100_000_001, EvConfirmedOver},
		{0, EvConfirmedShort},
	}
	for _, tc := range cases {
		if got := ClassifyConfirmation(money.FromInt64(tc.paid), due); got != tc.want {
			t.Errorf("ClassifyConfirmation(%d, %s) = %s, want %s", tc.paid, due, got, tc.want)
		}
	}
}

func TestInvoiceTrackingStep(t *testing.T) {
	want := map[InvoiceState]TrackingStep{
		InvoiceOpen: StepNone, InvoiceExpired: StepNone, InvoiceCancelled: StepNone,
		InvoiceReceived: StepReceived, InvoiceUnderpaid: StepReceived,
		InvoiceConfirmed: StepConfirmed, InvoiceOverpaid: StepConfirmed, InvoiceSettled: StepConfirmed,
	}
	for _, s := range InvoiceStates {
		if got := InvoiceTrackingStep(s); got != want[s] {
			t.Errorf("InvoiceTrackingStep(%s) = %d, want %d", s, got, want[s])
		}
	}
}

// allowedImports are the only packages a state machine may import. Anything
// that can perform I/O, read the clock or touch randomness is excluded.
var allowedImports = map[string]bool{
	"errors": true,
	"fmt":    true,
	"github.com/JoshBlazer/waybill/api/internal/money": true,
}

func TestStatemachine_NoIOImports(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if !allowedImports[path] {
				t.Errorf("%s imports %q; state machines must stay pure", name, path)
			}
		}
	}
}
