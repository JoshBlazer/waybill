// Package money represents amounts as exact integers in minor units of a
// named asset. There is no floating point anywhere in this package, and
// TestNoFloatsInMoneyPackages keeps it that way.
//
// An Amount is a whole number of minor units (micro-USDC, kobo, satoshi).
// Converting to and from human decimals needs the asset's scale and is exact:
// ParseDecimal rejects input with more precision than the asset has, rather
// than rounding it.
package money

import (
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
)

// AssetCode names a ledger asset, for example "USDC" or "NGN".
type AssetCode string

// Asset is a code and its scale: the number of decimal places in one major
// unit. USDC has scale 6, NGN has scale 2.
type Asset struct {
	Code  AssetCode
	Scale uint8
}

// Known assets. The database table `assets` holds the same rows.
var (
	USDC = Asset{Code: "USDC", Scale: 6}
	USDT = Asset{Code: "USDT", Scale: 6}
	NGN  = Asset{Code: "NGN", Scale: 2}
	BTC  = Asset{Code: "BTC", Scale: 8}
	ETH  = Asset{Code: "ETH", Scale: 18}
)

// MaxDigits bounds amounts to what NUMERIC columns and uint256 hold.
const MaxDigits = 78

// Errors returned by parsing and scanning.
var (
	ErrInvalidAmount  = errors.New("money: invalid amount")
	ErrTooPrecise     = errors.New("money: more decimal places than the asset allows")
	ErrOutOfRange     = errors.New("money: amount exceeds 78 digits")
	ErrNotWholeNumber = errors.New("money: database value is not a whole number of minor units")
)

// Amount is an exact, immutable integer number of minor units. The zero
// value is 0. Methods never modify their receiver or arguments.
type Amount struct {
	v *big.Int // nil means 0
}

// Zero is the zero amount.
var Zero = Amount{}

// FromInt64 returns an amount of n minor units.
func FromInt64(n int64) Amount { return Amount{v: big.NewInt(n)} }

// FromBig returns an amount equal to n, copying it.
func FromBig(n *big.Int) (Amount, error) {
	if n == nil {
		return Zero, nil
	}
	a := Amount{v: new(big.Int).Set(n)}
	if !a.inRange() {
		return Zero, ErrOutOfRange
	}
	return a, nil
}

// ParseMinor parses a base-10 integer string of minor units, such as
// "125500000". Signs are allowed; anything else is rejected.
func ParseMinor(s string) (Amount, error) {
	n, ok := new(big.Int).SetString(s, 10)
	if !ok || strings.TrimSpace(s) != s || s == "" || strings.HasPrefix(s, "+") {
		return Zero, fmt.Errorf("%w: %q", ErrInvalidAmount, s)
	}
	return FromBig(n)
}

// ParseDecimal parses a human decimal in major units, such as "125.50", for
// an asset with the given scale. It is exact: "0.1234567" with scale 6 is an
// error, not 0.123457.
func ParseDecimal(s string, scale uint8) (Amount, error) {
	bad := fmt.Errorf("%w: %q", ErrInvalidAmount, s)
	if s == "" {
		return Zero, bad
	}
	neg := false
	body := s
	if body[0] == '-' {
		neg, body = true, body[1:]
	}
	whole, frac, hasPoint := strings.Cut(body, ".")
	if whole == "" || (hasPoint && frac == "") || !allDigits(whole) || !allDigits(frac) {
		return Zero, bad
	}
	if len(frac) > int(scale) {
		return Zero, fmt.Errorf("%w: %q has %d decimal places, asset allows %d", ErrTooPrecise, s, len(frac), scale)
	}
	digits := whole + frac + strings.Repeat("0", int(scale)-len(frac))
	n, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return Zero, bad
	}
	if neg {
		n.Neg(n)
	}
	return FromBig(n)
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (a Amount) big() *big.Int {
	if a.v == nil {
		return new(big.Int)
	}
	return a.v
}

// Big returns a copy of the amount as a *big.Int.
func (a Amount) Big() *big.Int { return new(big.Int).Set(a.big()) }

// Add returns a + b.
func (a Amount) Add(b Amount) Amount { return Amount{v: new(big.Int).Add(a.big(), b.big())} }

// Sub returns a - b.
func (a Amount) Sub(b Amount) Amount { return Amount{v: new(big.Int).Sub(a.big(), b.big())} }

// Neg returns -a.
func (a Amount) Neg() Amount { return Amount{v: new(big.Int).Neg(a.big())} }

// Cmp compares a and b: -1, 0 or +1.
func (a Amount) Cmp(b Amount) int { return a.big().Cmp(b.big()) }

// Equal reports whether a == b.
func (a Amount) Equal(b Amount) bool { return a.Cmp(b) == 0 }

// Sign returns -1, 0 or +1.
func (a Amount) Sign() int { return a.big().Sign() }

// IsZero reports whether a == 0.
func (a Amount) IsZero() bool { return a.Sign() == 0 }

// String returns the minor-unit integer, for example "125500000".
func (a Amount) String() string { return a.big().String() }

// Format renders the amount in major units with exactly `scale` decimal
// places, for example Format(6) of 125500000 is "125.500000".
func (a Amount) Format(scale uint8) string {
	n := a.big()
	neg := n.Sign() < 0
	digits := new(big.Int).Abs(n).String()
	if scale > 0 {
		if pad := int(scale) + 1 - len(digits); pad > 0 {
			digits = strings.Repeat("0", pad) + digits
		}
		cut := len(digits) - int(scale)
		digits = digits[:cut] + "." + digits[cut:]
	}
	if neg {
		return "-" + digits
	}
	return digits
}

func (a Amount) inRange() bool {
	return len(new(big.Int).Abs(a.big()).String()) <= MaxDigits
}

// NumericValue implements pgtype.NumericValuer so an Amount can be written
// to a NUMERIC column. The exponent is always 0: whole minor units.
func (a Amount) NumericValue() (pgtype.Numeric, error) {
	if !a.inRange() {
		return pgtype.Numeric{}, ErrOutOfRange
	}
	return pgtype.Numeric{Int: a.Big(), Exp: 0, Valid: true}, nil
}

// ScanNumeric implements pgtype.NumericScanner. It rejects NULL, NaN,
// infinities and any value with a fractional part rather than rounding.
func (a *Amount) ScanNumeric(n pgtype.Numeric) error {
	if !n.Valid || n.NaN || n.InfinityModifier != pgtype.Finite || n.Int == nil {
		return fmt.Errorf("%w: NULL, NaN or infinite", ErrInvalidAmount)
	}
	v := new(big.Int).Set(n.Int)
	switch {
	case n.Exp > 0:
		v.Mul(v, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n.Exp)), nil))
	case n.Exp < 0:
		div := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(-n.Exp)), nil)
		q, r := new(big.Int).QuoRem(v, div, new(big.Int))
		if r.Sign() != 0 {
			return ErrNotWholeNumber
		}
		v = q
	}
	out, err := FromBig(v)
	if err != nil {
		return err
	}
	*a = out
	return nil
}
