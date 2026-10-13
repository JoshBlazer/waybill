// Package deployments loads the contract addresses written by
// contracts/script/Deploy.s.sol and verifies them against the live chain.
//
// A wrong factory or implementation address would make every deposit address
// Waybill hands out unreachable, so services refuse to start unless the
// chain itself confirms the configuration (Verify).
package deployments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/JoshBlazer/waybill/api/internal/evm"
	"github.com/JoshBlazer/waybill/api/internal/safety"
)

// Deployment is one network's contract set.
type Deployment struct {
	Network        safety.Network `json:"-"`
	ChainID        int64          `json:"chainId"`
	Token          common.Address `json:"token"`
	Vault          common.Address `json:"vault"`
	Factory        common.Address `json:"factory"`
	Implementation common.Address `json:"implementation"`
}

// PredictDepositAddress returns the deposit address for salt on this network.
func (d Deployment) PredictDepositAddress(salt [32]byte) common.Address {
	return evm.PredictDepositAddress(d.Factory, d.Implementation, salt)
}

// Load reads <dir>/<chainId>.json for every EVM network in networks.
func Load(dir string, networks []safety.Network) (map[safety.Network]Deployment, error) {
	out := make(map[safety.Network]Deployment)
	for _, n := range networks {
		family, id, _ := strings.Cut(string(n), ":")
		if family != "evm" {
			continue
		}
		path := filepath.Join(dir, id+".json")
		raw, err := os.ReadFile(path) //nolint:gosec // path is built from an allowlisted network id
		if err != nil {
			return nil, fmt.Errorf("deployments: %s: %w", n, err)
		}
		var d Deployment
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, fmt.Errorf("deployments: %s: %w", path, err)
		}
		if strconv.FormatInt(d.ChainID, 10) != id {
			return nil, fmt.Errorf("deployments: %s records chain id %d, want %s", path, d.ChainID, id)
		}
		var zero common.Address
		if d.Token == zero || d.Factory == zero || d.Implementation == zero || d.Vault == zero {
			return nil, fmt.Errorf("deployments: %s is missing an address", path)
		}
		d.Network = n
		out[n] = d
	}
	return out, nil
}

// Caller performs eth_call. *ethclient.Client implements it.
type Caller interface {
	CallContract(ctx context.Context, msg ethereum.CallMsg, blockNumber *big.Int) ([]byte, error)
}

var errMismatch = errors.New("deployments: chain does not match configuration")

// Verify checks d against the chain: the factory reports the configured
// implementation, the factory's predict() agrees with Waybill's own CREATE2
// computation, and the token has six decimals.
func Verify(ctx context.Context, c Caller, d Deployment) error {
	impl, err := call(ctx, c, d.Factory, "implementation()", nil)
	if err != nil {
		return fmt.Errorf("deployments: %s factory.implementation(): %w", d.Network, err)
	}
	if common.BytesToAddress(impl) != d.Implementation {
		return fmt.Errorf("%w: %s factory reports implementation %s, configured %s",
			errMismatch, d.Network, common.BytesToAddress(impl).Hex(), d.Implementation.Hex())
	}

	probe := crypto.Keccak256Hash([]byte("waybill deployment self-check"))
	predicted, err := call(ctx, c, d.Factory, "predict(bytes32)", probe.Bytes())
	if err != nil {
		return fmt.Errorf("deployments: %s factory.predict(): %w", d.Network, err)
	}
	if got, want := common.BytesToAddress(predicted), d.PredictDepositAddress(probe); got != want {
		return fmt.Errorf("%w: %s factory predicts %s, Waybill computes %s",
			errMismatch, d.Network, got.Hex(), want.Hex())
	}

	dec, err := call(ctx, c, d.Token, "decimals()", nil)
	if err != nil {
		return fmt.Errorf("deployments: %s token.decimals(): %w", d.Network, err)
	}
	if new(big.Int).SetBytes(dec).Cmp(big.NewInt(6)) != 0 {
		return fmt.Errorf("%w: %s token has %s decimals, want 6", errMismatch, d.Network, new(big.Int).SetBytes(dec))
	}
	return nil
}

func call(ctx context.Context, c Caller, to common.Address, sig string, arg []byte) ([]byte, error) {
	data := append(crypto.Keccak256([]byte(sig))[:4:4], arg...)
	out, err := c.CallContract(ctx, ethereum.CallMsg{To: &to, Data: data}, nil)
	if err != nil {
		return nil, err
	}
	// A call to an address without code returns no data.
	if len(out) != 32 {
		return nil, fmt.Errorf("got %d bytes of return data; is a contract deployed at %s?", len(out), to.Hex())
	}
	return out, nil
}
