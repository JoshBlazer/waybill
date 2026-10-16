package txmgr_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/JoshBlazer/waybill/api/internal/testdb"
	"github.com/JoshBlazer/waybill/api/internal/txmgr"
)

func TestAcquireLease_OneHolderAtATime(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	a := acquire(t, pool, "worker-a", 300*time.Millisecond)
	if _, err := txmgr.AcquireLease(ctx, pool, signer, network, "worker-b", time.Minute); !errors.Is(err, txmgr.ErrLeaseHeld) {
		t.Fatalf("second owner while the lease is live: err = %v, want ErrLeaseHeld", err)
	}
	renewed := acquire(t, pool, "worker-a", 300*time.Millisecond)
	if !renewed.ExpiresAt.After(a.ExpiresAt) {
		t.Fatalf("renewal did not extend the lease: %v then %v", a.ExpiresAt, renewed.ExpiresAt)
	}

	time.Sleep(500 * time.Millisecond) // worker-a stopped renewing: crashed
	b := acquire(t, pool, "worker-b", time.Minute)
	if b.Owner != "worker-b" {
		t.Fatalf("lease owner after takeover = %q", b.Owner)
	}
	if _, err := txmgr.AcquireLease(ctx, pool, signer, network, "worker-a", time.Minute); !errors.Is(err, txmgr.ErrLeaseHeld) {
		t.Fatalf("the old owner after takeover: err = %v, want ErrLeaseHeld", err)
	}
}

func TestSignerNonces_NextNonceNeverDecreases(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	acquire(t, pool, "worker-a", time.Minute)
	if _, err := pool.Exec(ctx, `UPDATE signer_nonces SET next_nonce = 5`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE signer_nonces SET next_nonce = 4`); err == nil {
		t.Fatal("the database let next_nonce go down")
	}
}
