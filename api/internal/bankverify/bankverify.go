// Package bankverify checks that a Nigerian bank account belongs to the
// contractor before Waybill will pay into it (docs/RISKS.md §2).
//
// A Resolver asks the bank (through a provider) for the account's name;
// NameMatches compares it with the contractor's legal name. The sandbox
// resolver is chosen in a decision record (ADR-040); Fake serves tests and
// the local stack.
package bankverify

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// Account is a NUBAN account at a bank.
type Account struct {
	BankCode      string // the provider's bank code, e.g. "058"
	AccountNumber string // 10 digits
}

var (
	nuban    = regexp.MustCompile(`^[0-9]{10}$`)
	bankCode = regexp.MustCompile(`^[0-9]{3,6}$`)
)

// Errors. ErrUnavailable is "could not find out" (I12), distinct from
// ErrAccountNotFound, which is the bank's answer.
var (
	ErrInvalidAccount  = errors.New("bankverify: account number must be 10 digits and the bank code 3 to 6 digits")
	ErrAccountNotFound = errors.New("bankverify: the bank has no such account")
	ErrUnavailable     = errors.New("bankverify: could not reach the bank")
)

// Validate checks the account's format before anything is sent anywhere.
func (a Account) Validate() error {
	if !nuban.MatchString(a.AccountNumber) || !bankCode.MatchString(a.BankCode) {
		return ErrInvalidAccount
	}
	return nil
}

// Resolver asks the bank for an account's name.
type Resolver interface {
	ResolveAccount(ctx context.Context, a Account) (name string, err error)
}

// Result is the outcome of a verification.
type Result struct {
	AccountName string // as the bank holds it
	Matches     bool   // the legal name matches it (NameMatches)
}

// Verify resolves the account and compares names. An invalid account is
// refused without asking; ErrAccountNotFound and ErrUnavailable pass through
// unchanged so callers can tell "no" from "could not find out".
func Verify(ctx context.Context, r Resolver, legalName string, a Account) (Result, error) {
	if err := a.Validate(); err != nil {
		return Result{}, err
	}
	name, err := r.ResolveAccount(ctx, a)
	if err != nil {
		return Result{}, err
	}
	return Result{AccountName: name, Matches: NameMatches(legalName, name)}, nil
}

// NameMatches reports whether every part of the legal name appears in the
// bank's account name, in any order. Banks often store names surname first
// and with middle names ("OKAFOR ADAEZE CHIOMA" for "Adaeze Okafor"), so
// order and extra names are allowed; a missing or different part is not. A
// legal name needs at least two parts. Case, accents, punctuation and
// spacing are ignored. No fuzzy matching: a near-miss is a mismatch that a
// person resolves.
func NameMatches(legal, account string) bool {
	want := nameParts(legal)
	if len(want) < 2 {
		return false
	}
	have := map[string]int{}
	for _, p := range nameParts(account) {
		have[p]++
	}
	for _, p := range want {
		if have[p] == 0 {
			return false
		}
		have[p]--
	}
	return true
}

func nameParts(s string) []string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		switch {
		case unicode.Is(unicode.Mn, r):
			// a combining accent typed separately: drop it, keep the word whole
		case unicode.IsLetter(r):
			b.WriteRune(fold(r))
		case r == '\'' || r == '’':
			// O'Brien and OBRIEN are the same name
		default:
			b.WriteRune(' ')
		}
	}
	return strings.Fields(b.String())
}

// fold maps common accented capitals to their plain letter (Yoruba and
// Igbo names in bank records are often written without marks).
func fold(r rune) rune {
	switch r {
	case 'À', 'Á', 'Â', 'Ã', 'Ä':
		return 'A'
	case 'È', 'É', 'Ê', 'Ë', 'Ẹ':
		return 'E'
	case 'Ì', 'Í', 'Î', 'Ï', 'Ị':
		return 'I'
	case 'Ò', 'Ó', 'Ô', 'Õ', 'Ö', 'Ọ':
		return 'O'
	case 'Ù', 'Ú', 'Û', 'Ü', 'Ụ':
		return 'U'
	case 'Ṣ':
		return 'S'
	case 'Ń', 'Ǹ':
		return 'N'
	}
	return r
}

// Fake is a Resolver with fixed accounts, for tests and the local stack.
type Fake struct {
	Accounts map[Account]string
	Err      error // returned by every call when set
}

// ResolveAccount returns the fixed name, or ErrAccountNotFound.
func (f *Fake) ResolveAccount(_ context.Context, a Account) (string, error) {
	if f.Err != nil {
		return "", f.Err
	}
	name, ok := f.Accounts[a]
	if !ok {
		return "", fmt.Errorf("%w: %s at bank %s", ErrAccountNotFound, a.AccountNumber, a.BankCode)
	}
	return name, nil
}
