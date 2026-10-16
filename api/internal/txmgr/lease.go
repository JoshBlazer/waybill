package txmgr

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"

	"github.com/JoshBlazer/waybill/api/internal/evm"
	"github.com/JoshBlazer/waybill/api/internal/safety"
	"github.com/JoshBlazer/waybill/api/internal/store"
)

// Lease is the right to assign nonces for one key on one network until
// ExpiresAt (database time).
type Lease struct {
	Signer    common.Address
	Network   safety.Network
	Owner     string
	ExpiresAt time.Time
}

// AcquireLease takes or renews the lease on signer for owner, valid for ttl
// from now by the database clock. It returns ErrLeaseHeld while another
// owner's lease is live. Holders renew by calling it again well before
// ExpiresAt.
func AcquireLease(ctx context.Context, db store.DBTX, signer common.Address, network safety.Network, owner string, ttl time.Duration) (Lease, error) {
	row, err := store.New(db).AcquireSignerLease(ctx, store.AcquireSignerLeaseParams{
		Signer:  evm.NormalizeAddress(signer),
		Network: string(network),
		Owner:   owner,
		TtlMs:   ttl.Milliseconds(),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, fmt.Errorf("%w: %s on %s", ErrLeaseHeld, evm.NormalizeAddress(signer), network)
	}
	if err != nil {
		return Lease{}, fmt.Errorf("txmgr: acquire lease: %w", err)
	}
	return Lease{Signer: signer, Network: network, Owner: row.LeaseOwner, ExpiresAt: row.LeaseExpiresAt}, nil
}
