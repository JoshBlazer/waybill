package auth_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/JoshBlazer/waybill/api/internal/auth"
	"github.com/JoshBlazer/waybill/api/internal/store"
	"github.com/JoshBlazer/waybill/api/internal/testdb"
)

func TestGenerateKey_FormatAndUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		k, err := auth.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(k.Full, "wb_test_") || len(k.Full) != len("wb_test_")+8+1+52 {
			t.Fatalf("bad key format %q", k.Full)
		}
		if seen[k.Full] || seen[k.Prefix] {
			t.Fatalf("duplicate key or prefix after %d keys", i)
		}
		seen[k.Full], seen[k.Prefix] = true, true
	}
}

func TestParseKey_RejectsMalformed(t *testing.T) {
	for _, s := range []string{"", "wb_live_abcdefgh_" + strings.Repeat("a", 52), "wb_test_ABCDEFGH_" + strings.Repeat("a", 52), "wb_test_abcdefgh_short", "sk_test_abc"} {
		if _, err := auth.ParseKey(s); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("ParseKey(%q) = %v", s, err)
		}
	}
}

func TestAuthenticate(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	q := store.New(pool)
	c, err := q.UpsertContractor(ctx, store.UpsertContractorParams{ID: uuid.New(), LegalName: "Test Contractor"})
	if err != nil {
		t.Fatal(err)
	}
	k, _ := auth.GenerateKey()
	if err := q.InsertAPIKey(ctx, store.InsertAPIKeyParams{ContractorID: c.ID, Prefix: k.Prefix, SecretSha256: k.Hash[:]}); err != nil {
		t.Fatal(err)
	}

	got, err := auth.Authenticate(ctx, pool, "Bearer "+k.Full)
	if err != nil || got != c.ID {
		t.Fatalf("Authenticate(valid) = %v, %v", got, err)
	}

	other, _ := auth.GenerateKey()
	wrongSecret := k.Full[:len(k.Full)-52] + other.Full[len(other.Full)-52:] // right prefix, wrong secret
	for name, header := range map[string]string{
		"missing":      "",
		"no bearer":    k.Full,
		"unknown key":  "Bearer " + other.Full,
		"wrong secret": "Bearer " + wrongSecret,
		"malformed":    "Bearer wb_test_nope",
	} {
		if _, err := auth.Authenticate(ctx, pool, header); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("%s: err = %v, want ErrUnauthenticated", name, err)
		}
	}

	if _, err := pool.Exec(ctx, `UPDATE api_keys SET revoked_at = now() WHERE prefix = $1`, k.Prefix); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Authenticate(ctx, pool, "Bearer "+k.Full); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("revoked key accepted: %v", err)
	}
}
