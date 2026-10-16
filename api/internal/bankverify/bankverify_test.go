package bankverify_test

import (
	"context"
	"errors"
	"testing"

	"github.com/JoshBlazer/waybill/api/internal/bankverify"
)

func TestNameMatches(t *testing.T) {
	cases := []struct {
		legal, account string
		want           bool
	}{
		{"Adaeze Okafor", "ADAEZE OKAFOR", true},
		{"Adaeze Okafor", "OKAFOR ADAEZE", true},        // surname first
		{"Adaeze Okafor", "OKAFOR ADAEZE CHIOMA", true}, // extra middle name at the bank
		{"Adaeze  Okafor ", "okafor, adaeze", true},     // case, spacing, punctuation
		{"Adébáyọ̀ Ògúnlésì", "OGUNLESI ADEBAYO", true}, // marks dropped by the bank
		{"Chinedu O'Neil", "ONEIL CHINEDU", true},
		{"Foláké Adẹ", "ADE FOLAKE", true},            // combining marks mid-word
		{"Adaeze Okafor", "ADAEZE OKAFO", false},         // near miss is a mismatch
		{"Adaeze Okafor", "CHIOMA OKAFOR", false},        // different first name
		{"Adaeze Chioma Okafor", "ADAEZE OKAFOR", false}, // a legal part missing at the bank
		{"Okafor", "OKAFOR ADAEZE", false},               // one part is not enough
		{"Ada Ada", "ADA OKAFOR", false},                 // each legal part needs its own match
		{"", "ADAEZE OKAFOR", false},
		{"Adaeze Okafor", "", false},
	}
	for _, c := range cases {
		if got := bankverify.NameMatches(c.legal, c.account); got != c.want {
			t.Errorf("NameMatches(%q, %q) = %v, want %v", c.legal, c.account, got, c.want)
		}
	}
}

var acct = bankverify.Account{BankCode: "058", AccountNumber: "0123456789"}

func TestVerify(t *testing.T) {
	ctx := context.Background()
	fake := &bankverify.Fake{Accounts: map[bankverify.Account]string{acct: "OKAFOR ADAEZE CHIOMA"}}

	got, err := bankverify.Verify(ctx, fake, "Adaeze Okafor", acct)
	if err != nil || !got.Matches || got.AccountName != "OKAFOR ADAEZE CHIOMA" {
		t.Fatalf("matching account: %+v, %v", got, err)
	}
	if got, err := bankverify.Verify(ctx, fake, "Bola Ahmed", acct); err != nil || got.Matches {
		t.Fatalf("someone else's account: %+v, %v; want resolved but not matching", got, err)
	}
	other := bankverify.Account{BankCode: "058", AccountNumber: "9999999999"}
	if _, err := bankverify.Verify(ctx, fake, "Adaeze Okafor", other); !errors.Is(err, bankverify.ErrAccountNotFound) {
		t.Fatalf("unknown account: %v, want ErrAccountNotFound", err)
	}
	down := &bankverify.Fake{Err: bankverify.ErrUnavailable}
	if _, err := bankverify.Verify(ctx, down, "Adaeze Okafor", acct); !errors.Is(err, bankverify.ErrUnavailable) || errors.Is(err, bankverify.ErrAccountNotFound) {
		t.Fatalf("bank unreachable: %v; want ErrUnavailable, not ErrAccountNotFound", err)
	}
}

func TestVerify_RefusesMalformedAccountsWithoutAsking(t *testing.T) {
	asked := &bankverify.Fake{Err: errors.New("must not be called")}
	for _, a := range []bankverify.Account{
		{BankCode: "058", AccountNumber: "012345678"},   // 9 digits
		{BankCode: "058", AccountNumber: "01234567890"}, // 11 digits
		{BankCode: "058", AccountNumber: "01234567a9"},
		{BankCode: "58", AccountNumber: "0123456789"},
		{BankCode: "", AccountNumber: "0123456789"},
	} {
		if _, err := bankverify.Verify(context.Background(), asked, "Adaeze Okafor", a); !errors.Is(err, bankverify.ErrInvalidAccount) {
			t.Errorf("%+v: err = %v, want ErrInvalidAccount", a, err)
		}
	}
}
