package evm

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

// Vectors read from the real contracts: contracts/script/Deploy.s.sol on a
// fresh Anvil with sender 0xf39F…2266, then `cast call <factory>
// "predict(bytes32)(address)" <salt>`. If the contracts or OpenZeppelin's
// Clones change, these must be regenerated the same way.
var (
	vecFactory = common.HexToAddress("0x9fE46736679d2D9a65F0992F2272dE9f3c7fa6e0")
	vecImpl    = common.HexToAddress("0x75537828f2ce51be7289709686A69CbFDbB714F1")
)

func TestPredictDepositAddress(t *testing.T) {
	cases := []struct {
		salt string
		want string
	}{
		{"0x0000000000000000000000000000000000000000000000000000000000000000", "0x28a18359bCD1FFFA625A54A5A6945c9B5a7f6AA0"},
		{"0x73ff5e116f7045a125854606e39364b69ad1340192ef9c48584444296624ae28", "0x697526fdFbbbE88824905924bC745362aca4a565"},
	}
	for _, tc := range cases {
		got := PredictDepositAddress(vecFactory, vecImpl, common.HexToHash(tc.salt))
		if got != common.HexToAddress(tc.want) {
			t.Errorf("salt %s: got %s, want %s", tc.salt, got.Hex(), tc.want)
		}
	}
}

func TestSaltForInvoice_MatchesFactoryVector(t *testing.T) {
	// The second vector's salt is keccak256 of these 16 UUID bytes.
	id := uuid.MustParse("01234567-89ab-cdef-0123-456789abcdef")
	got := common.Hash(SaltForInvoice(id)).Hex()
	if got != "0x73ff5e116f7045a125854606e39364b69ad1340192ef9c48584444296624ae28" {
		t.Fatalf("SaltForInvoice = %s", got)
	}
	if addr := PredictDepositAddress(vecFactory, vecImpl, SaltForInvoice(id)); addr.Hex() != "0x697526fdFbbbE88824905924bC745362aca4a565" {
		t.Fatalf("invoice deposit address = %s", addr.Hex())
	}
}

func TestDepositAddress_DistinctPerInvoice(t *testing.T) {
	seen := map[common.Address]bool{}
	for i := 0; i < 1000; i++ {
		a := PredictDepositAddress(vecFactory, vecImpl, SaltForInvoice(uuid.New()))
		if seen[a] {
			t.Fatalf("two invoices share deposit address %s", a.Hex())
		}
		seen[a] = true
	}
}

func TestNormalizeAddress(t *testing.T) {
	if got := NormalizeAddress(common.HexToAddress("0x697526fdFbbbE88824905924bC745362aca4a565")); got != "0x697526fdfbbbe88824905924bc745362aca4a565" {
		t.Fatalf("NormalizeAddress = %s", got)
	}
}
