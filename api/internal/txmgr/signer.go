package txmgr

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// Signer signs transactions for one address. Keys stay behind this
// interface (invariant I15), so a remote signer can replace LocalSigner
// without touching the transaction manager.
type Signer interface {
	Address() common.Address
	SignTx(tx *types.Transaction, chainID *big.Int) (*types.Transaction, error)
}

// LocalSigner holds a private key in memory. It never prints the key: String
// and LogValue show only the address.
type LocalSigner struct {
	key  *ecdsa.PrivateKey
	addr common.Address
}

var errBadKey = errors.New("txmgr: signing key is not a 32-byte hex private key")

// ParseKey reads a hex private key (with or without 0x, surrounding
// whitespace ignored). The error never contains the input.
func ParseKey(hexKey string) (*LocalSigner, error) {
	k, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimSpace(hexKey), "0x"))
	if err != nil {
		return nil, errBadKey
	}
	return &LocalSigner{key: k, addr: crypto.PubkeyToAddress(k.PublicKey)}, nil
}

// LoadKeyFile reads a key written by a person or a deploy script. The file
// should be readable by its owner only.
func LoadKeyFile(path string) (*LocalSigner, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path comes from operator configuration
	if err != nil {
		return nil, fmt.Errorf("txmgr: read signing key file: %w", err)
	}
	return ParseKey(string(raw))
}

// Address is the signer's account.
func (s *LocalSigner) Address() common.Address { return s.addr }

// SignTx signs tx for chainID with the London (EIP-1559) signer.
func (s *LocalSigner) SignTx(tx *types.Transaction, chainID *big.Int) (*types.Transaction, error) {
	return types.SignTx(tx, types.NewLondonSigner(chainID), s.key)
}

// String shows the address only.
func (s *LocalSigner) String() string { return "LocalSigner(" + s.addr.Hex() + ")" }

// GoString shows the address only, also under %#v.
func (s *LocalSigner) GoString() string { return s.String() }

// LogValue keeps the key out of structured logs.
func (s *LocalSigner) LogValue() slog.Value { return slog.StringValue(s.String()) }
