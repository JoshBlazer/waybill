package paystack_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JoshBlazer/waybill/api/internal/bankverify"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/paystack"
)

// TestLive_TransferRoundTrip talks to Paystack in test mode. It runs only
// when PAYSTACK_SECRET_KEY holds a test key (sk_test_…); otherwise it skips.
// Test-mode transfers move no money: Paystack documents that they always
// succeed. The key comes from the environment and is never printed.
//
// Optional: PAYSTACK_TEST_BANK_CODE and PAYSTACK_TEST_ACCOUNT pick the
// account (default 058 / 0000000000).
func TestLive_TransferRoundTrip(t *testing.T) {
	key := os.Getenv("PAYSTACK_SECRET_KEY")
	if key == "" {
		t.Skip("PAYSTACK_SECRET_KEY not set; skipping the live Paystack test-mode check")
	}
	c, err := paystack.New(key, "", 30*time.Second)
	if err != nil {
		t.Fatal(err) // ErrLiveKey for anything that is not sk_test_
	}
	ctx := context.Background()
	acct := bankverify.Account{BankCode: envOr("PAYSTACK_TEST_BANK_CODE", "058"), AccountNumber: envOr("PAYSTACK_TEST_ACCOUNT", "0000000000")}

	name, err := c.ResolveAccount(ctx, acct)
	if err != nil {
		t.Fatalf("resolve %s at %s: %v", acct.AccountNumber, acct.BankCode, err)
	}
	t.Logf("resolved account name: %q", name)

	rcp, err := c.CreateRecipient(ctx, name, acct)
	if err != nil {
		t.Fatalf("create recipient: %v", err)
	}

	ref := "waybill-live-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := c.Verify(ctx, ref); !errors.Is(err, paystack.ErrNotFound) {
		t.Fatalf("verify before sending: %v; want ErrNotFound (Paystack's not-found answer has changed)", err)
	}
	sent, err := c.Initiate(ctx, paystack.Transfer{Reference: ref, Amount: money.FromInt64(10_000), Recipient: rcp, Reason: "Waybill live test"})
	if err != nil {
		t.Fatalf("initiate: %v", err)
	}
	t.Logf("initiated: status %s, code %s", sent.Status, sent.TransferCode)

	got, err := c.Verify(ctx, ref)
	if err != nil {
		t.Fatalf("verify after sending: %v", err)
	}
	if got.Reference != ref || got.Amount.String() != "10000" {
		t.Fatalf("verified %+v; want reference %s and 10000 kobo", got, ref)
	}
	t.Logf("verified: status %s", got.Status)

	// The same reference again is the same transfer, never a second one.
	again, err := c.Initiate(ctx, paystack.Transfer{Reference: ref, Amount: money.FromInt64(10_000), Recipient: rcp, Reason: "Waybill live test"})
	t.Logf("resend with the same reference: %+v, %v", again, err)
	if err == nil && again.TransferCode != "" && sent.TransferCode != "" && again.TransferCode != sent.TransferCode {
		t.Fatalf("a second transfer was created for one reference: %s and %s", sent.TransferCode, again.TransferCode)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
