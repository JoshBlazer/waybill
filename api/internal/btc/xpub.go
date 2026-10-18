// Package btc is Waybill's Bitcoin receive side (docs/design/stage5.md §2,
// ADR-045): addresses derived from an extended public key, and a small
// interface to follow the chain through bitcoind or an Esplora API.
//
// Waybill holds only a public key (I15). It can derive addresses and watch
// them; it can never spend from them.
package btc

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/base58"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"

	"github.com/JoshBlazer/waybill/api/internal/safety"
)

// Errors.
var (
	ErrPrivateKey     = errors.New("btc: a private key was configured; Waybill holds only an extended public key (tpub or vpub)")
	ErrNotTestNetwork = errors.New("btc: the extended key is for Bitcoin mainnet; only test networks are allowed")
	ErrBadKey         = errors.New("btc: not a valid extended public key")
)

// Params returns the chain parameters for a Bitcoin test network.
func Params(n safety.Network) (*chaincfg.Params, bool) {
	switch n {
	case safety.BTCRegtest:
		return &chaincfg.RegressionNetParams, true
	case safety.BTCSignet:
		return &chaincfg.SigNetParams, true
	}
	return nil, false
}

// Version bytes of BIP32 and SLIP-132 extended keys.
var (
	tpub = []byte{0x04, 0x35, 0x87, 0xcf}
	vpub = []byte{0x04, 0x5f, 0x1c, 0xf6} // BIP84 testnet public
	// Private keys of every kind are refused before parsing.
	privatePrefixes = []string{"tprv", "vprv", "uprv", "xprv", "yprv", "zprv"}
	mainnetPublic   = []string{"xpub", "ypub", "zpub"}
)

// Deriver turns an account-level BIP84 extended public key into receive
// addresses: index i is the P2WPKH address of key/0/i.
type Deriver struct {
	network safety.Network
	params  *chaincfg.Params
	receive *hdkeychain.ExtendedKey // key/0
}

// ParseXPub checks and parses an extended public key for network n. It
// refuses any private key, any mainnet key, and anything that is not an
// extended key, without echoing the input.
func ParseXPub(n safety.Network, s string) (*Deriver, error) {
	params, ok := Params(n)
	if !ok {
		return nil, fmt.Errorf("btc: %s is not a Bitcoin test network", n)
	}
	s = strings.TrimSpace(s)
	for _, p := range privatePrefixes {
		if strings.HasPrefix(s, p) {
			return nil, ErrPrivateKey
		}
	}
	for _, p := range mainnetPublic {
		if strings.HasPrefix(s, p) {
			return nil, ErrNotTestNetwork
		}
	}
	if strings.HasPrefix(s, "vpub") {
		var err error
		if s, err = withVersion(s, tpub); err != nil {
			return nil, err
		}
	}
	key, err := hdkeychain.NewKeyFromString(s)
	if err != nil {
		return nil, ErrBadKey
	}
	if key.IsPrivate() {
		return nil, ErrPrivateKey
	}
	if !key.IsForNet(params) {
		return nil, ErrNotTestNetwork
	}
	receive, err := key.Derive(0)
	if err != nil {
		return nil, fmt.Errorf("btc: derive the receive chain: %w", err)
	}
	return &Deriver{network: n, params: params, receive: receive}, nil
}

// withVersion re-encodes a base58check extended key with other version
// bytes (SLIP-132 vpub to BIP32 tpub: same key, different label).
func withVersion(s string, version []byte) (string, error) {
	raw := base58.Decode(s)
	if len(raw) != 82 {
		return "", ErrBadKey
	}
	payload, sum := raw[:78], raw[78:]
	if !bytes.Equal(checksum(payload), sum) {
		return "", ErrBadKey
	}
	out := append(append([]byte{}, version...), payload[4:]...)
	return base58.Encode(append(out, checksum(out)...)), nil
}

func checksum(b []byte) []byte {
	h := sha256.Sum256(b)
	h = sha256.Sum256(h[:])
	return h[:4]
}

// Network is the network the key's addresses are for.
func (d *Deriver) Network() safety.Network { return d.network }

// Address returns the receive address at index i.
func (d *Deriver) Address(i uint32) (string, error) {
	return address(d.receive, d.params, i)
}

func address(receive *hdkeychain.ExtendedKey, params *chaincfg.Params, i uint32) (string, error) {
	if i >= hdkeychain.HardenedKeyStart {
		return "", fmt.Errorf("btc: index %d is out of range", i)
	}
	child, err := receive.Derive(i)
	if err != nil {
		return "", err
	}
	pub, err := child.ECPubKey()
	if err != nil {
		return "", err
	}
	a, err := btcutil.NewAddressWitnessPubKeyHash(btcutil.Hash160(pub.SerializeCompressed()), params)
	if err != nil {
		return "", err
	}
	return a.EncodeAddress(), nil
}
