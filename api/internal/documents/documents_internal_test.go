package documents

import (
	"strings"
	"testing"

	"github.com/JoshBlazer/waybill/api/internal/money"
)

// Every leading character a spreadsheet treats as the start of a formula
// is neutralised; anything else is left alone.
func TestCell_NeutralisesFormulas(t *testing.T) {
	for _, c := range []string{"=SUM(A1)", "+1", "-1", "@cmd", "\tx", "\rx"} {
		if got := cell(c); got != "'"+c {
			t.Errorf("cell(%q) = %q", c, got)
		}
	}
	for _, c := range []string{"", "WB-ABCD", "155000.00", "0xabc", "Payment run"} {
		if got := cell(c); got != c {
			t.Errorf("cell(%q) = %q, want unchanged", c, got)
		}
	}
}

func TestDisplay(t *testing.T) {
	for _, tc := range []struct {
		minor int64
		asset money.Asset
		want  string
	}{
		{125_500_000, money.USDC, "USDC 125.50"},
		{250_500_001, money.USDC, "USDC 250.500001"},
		{1_234_567_890_000, money.USDC, "USDC 1,234,567.89"},
		{15_500_000, money.NGN, "NGN 155,000.00"},
		{5, money.NGN, "NGN 0.05"},
		{-100, money.NGN, "-NGN 1.00"},
	} {
		if got := Display(money.FromInt64(tc.minor), tc.asset); got != tc.want {
			t.Errorf("Display(%d %s) = %q, want %q", tc.minor, tc.asset.Code, got, tc.want)
		}
	}
}

func TestWrap_BreaksLongWordsAndKeepsShortLines(t *testing.T) {
	hash := "0x" + strings.Repeat("ab", 32)
	lines := wrap("Transaction "+hash, 40)
	for _, l := range lines {
		if len(l) > 40 {
			t.Fatalf("line %q is longer than 40", l)
		}
	}
	if strings.Join(lines, "") != "Transaction"+hash {
		t.Fatalf("wrapping lost text: %q", lines)
	}
	if got := wrap("", 10); len(got) != 1 || got[0] != "" {
		t.Fatalf("wrap of nothing: %q", got)
	}
}

func TestDescribe(t *testing.T) {
	for kind, want := range map[string]string{
		"deposit.final":           "Payment received",
		"payout.sent":             "Sent to bank",
		"batch.reserved":          "Payment run",
		"batch.reserved.reversal": "Payment run failed; money returned",
		"checkout.paid.reversal":  "Reversed: Card or bank transfer received",
	} {
		if got := describe(kind, money.USDC); got != want {
			t.Errorf("describe(%s) = %q, want %q", kind, got, want)
		}
	}
	if describe("payout.converted", money.NGN) != "Converted from USDC" || describe("payout.converted", money.USDC) != "Converted to naira" {
		t.Error("a conversion reads differently on each side")
	}
}
