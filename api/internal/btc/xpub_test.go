package btc

import (
	"errors"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"

	"github.com/JoshBlazer/waybill/api/internal/safety"
)

// The BIP84 test vector: account m/84'/0'/0' of the BIP39 mnemonic "abandon
// abandon … about", and its first two receive addresses.
const (
	bip84Zpub  = "zpub6rFR7y4Q2AijBEqTUquhVz398htDFrtymD9xYYfG1m4wAcvPhXNfE3EfH1r1ADqtfSdVCToUG868RvUUkgDKf31mGDtKsAYz2oz2AGutZYs"
	bip84Addr0 = "bc1qcr8te4kr609gcawutmrza0j4xv80jy8z306fyu"
	bip84Addr1 = "bc1qnjg0jd8228aq7egyzacy8cys3knf9xvrerkf9g"
)

// The derivation itself, checked against the BIP84 vector on mainnet
// parameters (the only published vector); test networks differ only in
// the address prefix.
func TestDerivation_MatchesBIP84Vector(t *testing.T) {
	xpub, err := withVersion(bip84Zpub, []byte{0x04, 0x88, 0xb2, 0x1e})
	if err != nil {
		t.Fatal(err)
	}
	key, err := hdkeychain.NewKeyFromString(xpub)
	if err != nil {
		t.Fatal(err)
	}
	receive, err := key.Derive(0)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{bip84Addr0, bip84Addr1} {
		got, err := address(receive, &chaincfg.MainNetParams, uint32(i))
		if err != nil || got != want {
			t.Fatalf("address %d = %q, %v; want %q", i, got, err, want)
		}
	}
}

// The same key as a test-network vpub gives the same key hashes with the
// test prefixes: tb1 on signet, bcrt1 on regtest.
func TestParseXPub_TestNetworks(t *testing.T) {
	vp, err := withVersion(bip84Zpub, vpub)
	if err != nil {
		t.Fatal(err)
	}
	for n, prefix := range map[safety.Network]string{safety.BTCSignet: "tb1q", safety.BTCRegtest: "bcrt1q"} {
		d, err := ParseXPub(n, vp)
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		a, err := d.Address(0)
		if err != nil || !strings.HasPrefix(a, prefix) {
			t.Fatalf("%s address %q, %v", n, a, err)
		}
		// Same 20-byte program as the mainnet vector: the witness program
		// is the bech32 data after the prefix and before the checksum.
		if a[len(prefix):len(a)-6] != bip84Addr0[len("bc1q"):len(bip84Addr0)-6] {
			t.Fatalf("%s: %s does not carry the vector's key hash", n, a)
		}
		b, _ := d.Address(1)
		if a == b {
			t.Fatal("two indexes gave the same address")
		}
	}
}

// TestBTCConfig_RejectsPrivateKey (I15): any private key is refused, and
// the error never contains the key.
func TestBTCConfig_RejectsPrivateKey(t *testing.T) {
	// Shaped like private keys, but not keys: the prefix is what is
	// refused, before anything is parsed. No real key belongs in the repo.
	fake := func(prefix string) string { return prefix + strings.Repeat("X", 107) }
	secret := fake("tprv")
	for _, s := range []string{secret, fake("vprv"), fake("xprv"), fake("zprv"), "c" + strings.Repeat("Y", 51)} {
		_, err := ParseXPub(safety.BTCRegtest, s)
		if err == nil {
			t.Fatalf("%.6s… accepted", s)
		}
		if strings.Contains(err.Error(), s[:8]) {
			t.Fatalf("error echoes the key: %v", err)
		}
	}
	if _, err := ParseXPub(safety.BTCRegtest, secret); !errors.Is(err, ErrPrivateKey) {
		t.Fatalf("tprv: %v, want ErrPrivateKey", err)
	}
}

func TestParseXPub_RejectsMainnetAndJunk(t *testing.T) {
	if _, err := ParseXPub(safety.BTCSignet, bip84Zpub); !errors.Is(err, ErrNotTestNetwork) {
		t.Fatalf("zpub: %v", err)
	}
	xpub, _ := withVersion(bip84Zpub, []byte{0x04, 0x88, 0xb2, 0x1e})
	if _, err := ParseXPub(safety.BTCSignet, xpub); !errors.Is(err, ErrNotTestNetwork) {
		t.Fatalf("xpub: %v", err)
	}
	if _, err := ParseXPub(safety.BTCSignet, "tpubnotakey"); !errors.Is(err, ErrBadKey) {
		t.Fatalf("junk: %v", err)
	}
	if _, err := ParseXPub(safety.EVMAnvil, "tpub"); err == nil {
		t.Fatal("an EVM network accepted")
	}
}
