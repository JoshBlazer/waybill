package batch

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/JoshBlazer/waybill/api/internal/evm"
	"github.com/JoshBlazer/waybill/api/internal/safety"
	"github.com/JoshBlazer/waybill/api/internal/store"
)

// WalletCoolingOff is how long a new or changed payout wallet waits before
// it can be paid, so a stolen key cannot redirect a run at once (RISKS §2).
const WalletCoolingOff = 24 * time.Hour

// ErrBadWallet means the network is not configured or the address is not a
// usable EVM address.
var ErrBadWallet = errors.New("batch: not a usable wallet on a configured network")

// SetWallet saves a contractor's payout wallet on a network. Saving the
// same address again changes nothing; a different one waits
// WalletCoolingOff before runs can pay it.
func (s *Service) SetWallet(ctx context.Context, contractorID uuid.UUID, network, address string) (store.ContractorWallet, error) {
	if _, ok := s.Deployments[safety.Network(network)]; !ok {
		return store.ContractorWallet{}, fmt.Errorf("%w: network %q is not configured", ErrBadWallet, network)
	}
	if !common.IsHexAddress(address) || common.HexToAddress(address) == (common.Address{}) {
		return store.ContractorWallet{}, fmt.Errorf("%w: %q is not an address", ErrBadWallet, address)
	}
	addr := evm.NormalizeAddress(common.HexToAddress(address))
	for _, d := range s.Deployments {
		if slices.Contains([]string{evm.NormalizeAddress(d.Vault), evm.NormalizeAddress(d.BatchPayout), evm.NormalizeAddress(d.Factory), evm.NormalizeAddress(d.Token)}, addr) {
			return store.ContractorWallet{}, fmt.Errorf("%w: that is one of Waybill's own contracts", ErrBadWallet)
		}
	}
	now := s.now()
	q := store.New(s.Pool)
	w, err := q.UpsertContractorWallet(ctx, store.UpsertContractorWalletParams{
		ContractorID: contractorID, Network: network, Address: addr, Now: now, PayoutsAllowedFrom: now.Add(WalletCoolingOff),
	})
	if errors.Is(err, pgx.ErrNoRows) { // unchanged
		return q.GetContractorWallet(ctx, store.GetContractorWalletParams{ContractorID: contractorID, Network: network})
	}
	return w, err
}

// Wallets returns a contractor's payout wallets, one per configured network
// that has one.
func (s *Service) Wallets(ctx context.Context, contractorID uuid.UUID) ([]store.ContractorWallet, error) {
	var out []store.ContractorWallet
	networks := make([]string, 0, len(s.Deployments))
	for n := range s.Deployments {
		networks = append(networks, string(n))
	}
	slices.Sort(networks)
	q := store.New(s.Pool)
	for _, n := range networks {
		w, err := q.GetContractorWallet(ctx, store.GetContractorWalletParams{ContractorID: contractorID, Network: n})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, nil
}
