package money

import (
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"pgregory.net/rapid"
)

// genAmount draws amounts of up to 78 digits, both signs, biased toward
// small values and boundaries by rapid.
func genAmount() *rapid.Generator[Amount] {
	return rapid.Custom(func(t *rapid.T) Amount {
		digits := rapid.StringMatching(`-?(0|[1-9][0-9]{0,77})`).Draw(t, "digits")
		a, err := ParseMinor(digits)
		if err != nil {
			t.Fatalf("generator produced invalid amount %q: %v", digits, err)
		}
		return a
	})
}

func TestAmount_ArithmeticRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		a, b := genAmount().Draw(t, "a"), genAmount().Draw(t, "b")
		if !a.Add(b).Sub(b).Equal(a) {
			t.Fatalf("(a + b) - b != a for a=%s b=%s", a, b)
		}
		if !a.Add(b).Equal(b.Add(a)) {
			t.Fatalf("addition not commutative for a=%s b=%s", a, b)
		}
		if !a.Add(a.Neg()).IsZero() {
			t.Fatalf("a + (-a) != 0 for a=%s", a)
		}
	})
}

func TestAmount_IsImmutable(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		a, b := genAmount().Draw(t, "a"), genAmount().Draw(t, "b")
		as, bs := a.String(), b.String()
		_ = a.Add(b)
		_ = a.Sub(b)
		_ = a.Neg()
		a.Big().SetInt64(42) // mutating the copy must not leak back
		if a.String() != as || b.String() != bs {
			t.Fatalf("operands changed: a %s→%s, b %s→%s", as, a, bs, b)
		}
	})
}

func TestDecimal_FormatParseRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		a := genAmount().Draw(t, "a")
		scale := uint8(rapid.IntRange(0, 18).Draw(t, "scale"))
		s := a.Format(scale)
		back, err := ParseDecimal(s, scale)
		if err != nil {
			t.Fatalf("ParseDecimal(%q, %d): %v", s, scale, err)
		}
		if !back.Equal(a) {
			t.Fatalf("round trip %s → %q → %s", a, s, back)
		}
	})
}

func TestParseDecimal_Examples(t *testing.T) {
	cases := []struct {
		in    string
		scale uint8
		want  string
		err   error
	}{
		{"125.50", 6, "125500000", nil},
		{"125", 6, "125000000", nil},
		{"0.000001", 6, "1", nil},
		{"-3.2", 2, "-320", nil},
		{"1540.25", 2, "154025", nil},
		{"0.1234567", 6, "", ErrTooPrecise}, // never rounded
		{"1.5", 0, "", ErrTooPrecise},
		{"", 6, "", ErrInvalidAmount},
		{"1.", 6, "", ErrInvalidAmount},
		{".5", 6, "", ErrInvalidAmount},
		{"1e6", 6, "", ErrInvalidAmount},
		{"1,000", 6, "", ErrInvalidAmount},
		{" 1", 6, "", ErrInvalidAmount},
		{"+1", 6, "", ErrInvalidAmount},
		{"--1", 6, "", ErrInvalidAmount},
		{"NaN", 6, "", ErrInvalidAmount},
		{"1" + strings.Repeat("0", 78), 0, "", ErrOutOfRange},
	}
	for _, tc := range cases {
		got, err := ParseDecimal(tc.in, tc.scale)
		if tc.err != nil {
			if !errors.Is(err, tc.err) {
				t.Errorf("ParseDecimal(%q, %d) err = %v, want %v", tc.in, tc.scale, err, tc.err)
			}
			continue
		}
		if err != nil || got.String() != tc.want {
			t.Errorf("ParseDecimal(%q, %d) = %s, %v; want %s", tc.in, tc.scale, got, err, tc.want)
		}
	}
}

func TestFormat_Examples(t *testing.T) {
	cases := []struct {
		minor int64
		scale uint8
		want  string
	}{
		{125500000, 6, "125.500000"},
		{1, 6, "0.000001"},
		{-1, 2, "-0.01"},
		{0, 2, "0.00"},
		{154025, 2, "1540.25"},
		{7, 0, "7"},
	}
	for _, tc := range cases {
		if got := FromInt64(tc.minor).Format(tc.scale); got != tc.want {
			t.Errorf("Format(%d, %d) = %q, want %q", tc.minor, tc.scale, got, tc.want)
		}
	}
}

func TestParseMinor_Rejects(t *testing.T) {
	for _, s := range []string{"", "1.0", "0x10", " 1", "+1", "1_000", "abc"} {
		if _, err := ParseMinor(s); err == nil {
			t.Errorf("ParseMinor(%q) accepted", s)
		}
	}
}

func TestNumeric_RoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		a := genAmount().Draw(t, "a")
		n, err := a.NumericValue()
		if err != nil {
			t.Fatal(err)
		}
		var back Amount
		if err := back.ScanNumeric(n); err != nil {
			t.Fatal(err)
		}
		if !back.Equal(a) {
			t.Fatalf("numeric round trip %s → %s", a, back)
		}
	})
}

func TestScanNumeric_RejectsFractionsAndSpecials(t *testing.T) {
	cases := map[string]pgtype.Numeric{
		"fraction":  {Int: big.NewInt(15), Exp: -1, Valid: true}, // 1.5
		"null":      {Valid: false},
		"nan":       {NaN: true, Valid: true},
		"+infinity": {InfinityModifier: pgtype.Infinity, Valid: true},
	}
	for name, n := range cases {
		var a Amount
		if err := a.ScanNumeric(n); err == nil {
			t.Errorf("%s: ScanNumeric accepted %+v", name, n)
		}
	}
	// Trailing zeros in the exponent are whole numbers and are accepted.
	var a Amount
	if err := a.ScanNumeric(pgtype.Numeric{Int: big.NewInt(150), Exp: -1, Valid: true}); err != nil || a.String() != "15" {
		t.Fatalf("ScanNumeric(15.0) = %s, %v", a, err)
	}
	if err := a.ScanNumeric(pgtype.Numeric{Int: big.NewInt(15), Exp: 2, Valid: true}); err != nil || a.String() != "1500" {
		t.Fatalf("ScanNumeric(15e2) = %s, %v", a, err)
	}
}

func TestZeroValueIsZero(t *testing.T) {
	var a Amount
	if !a.IsZero() || a.String() != "0" || !a.Add(FromInt64(5)).Equal(FromInt64(5)) {
		t.Fatal("zero value Amount is not usable as 0")
	}
}
