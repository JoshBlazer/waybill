package testchain

import (
	"context"
	_ "embed"
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/JoshBlazer/waybill/api/internal/deployments"
	"github.com/JoshBlazer/waybill/api/internal/safety"
)

//go:embed contracts.json
var fixtureJSON []byte

func bytecode(t *testing.T, name string) []byte {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal(fixtureJSON, &m); err != nil {
		t.Fatal(err)
	}
	b, err := hexutil.Decode(m[name])
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return b
}

func encode(t *testing.T, types string, args ...any) []byte {
	t.Helper()
	if types == "" {
		return nil
	}
	var as abi.Arguments
	for _, ty := range strings.Split(types, ",") {
		typ, err := abi.NewType(ty, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		as = append(as, abi.Argument{Type: typ})
	}
	out, err := as.Pack(args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Calldata builds a call: the selector of sig, then args ABI-encoded as
// the comma-separated types (for example "address,uint256").
func Calldata(t *testing.T, sig, types string, args ...any) []byte {
	t.Helper()
	return append(crypto.Keccak256([]byte(sig))[:4:4], encode(t, types, args...)...)
}

// Deploy creates a contract from the fixture with ABI-encoded constructor
// arguments, sent from Dev0, and returns its address.
func (a *Anvil) Deploy(name, ctorTypes string, args ...any) common.Address {
	a.t.Helper()
	data := append(bytecode(a.t, name), encode(a.t, ctorTypes, args...)...)
	var h common.Hash
	a.Call(&h, "eth_sendTransaction", map[string]any{"from": Dev0, "data": hexutil.Bytes(data), "gas": "0x1000000"})
	r := a.Receipt(h)
	if r.Status != 1 {
		a.t.Fatalf("deploy %s reverted", name)
	}
	return r.ContractAddress
}

// Role returns keccak256(name), the AccessControl role id.
func Role(name string) [32]byte { return crypto.Keccak256Hash([]byte(name)) }

// DeployWaybill deploys what Deploy.s.sol deploys, from Dev0, and gives
// operator the sweeper and batcher roles (as the deploy script does for
// the service signer). Vault limits: 10,000 per transaction, 50,000 a day.
func (a *Anvil) DeployWaybill(operator common.Address) deployments.Deployment {
	a.t.Helper()
	t := a.t
	token := a.Deploy("MockStablecoin", "string,string", "Waybill Mock USD Coin", "mUSDC")
	vault := a.Deploy("Vault", "address", Dev0)
	factory := a.Deploy("ForwarderFactory", "address,address", vault, Dev0)
	batch := a.Deploy("BatchPayout", "address,address", vault, Dev0)

	grant := "grantRole(bytes32,address)"
	a.Send(Dev0, vault, Calldata(t, grant, "bytes32,address", Role("PAYOUT_ROLE"), batch))
	a.Send(Dev0, vault, Calldata(t, "setLimits(address,uint128,uint128)", "address,uint128,uint128",
		token, big.NewInt(10_000e6), big.NewInt(50_000e6)))
	a.Send(Dev0, factory, Calldata(t, grant, "bytes32,address", Role("SWEEPER_ROLE"), operator))
	a.Send(Dev0, batch, Calldata(t, grant, "bytes32,address", Role("BATCHER_ROLE"), operator))

	impl, err := a.Client.CallContract(context.Background(), callMsg(factory, Calldata(t, "implementation()", "")), nil)
	if err != nil {
		t.Fatal(err)
	}
	return deployments.Deployment{
		Network: safety.EVMAnvil, ChainID: 31337, Token: token, Vault: vault, Factory: factory,
		Implementation: common.BytesToAddress(impl), BatchPayout: batch,
	}
}

// Mint mints amount of the mock token to to.
func (a *Anvil) Mint(token, to common.Address, amount int64) {
	a.t.Helper()
	a.Send(Dev0, token, Calldata(a.t, "mint(address,uint256)", "address,uint256", to, big.NewInt(amount)))
}

// TokenBalance reads an ERC-20 balance.
func (a *Anvil) TokenBalance(token, who common.Address) *big.Int {
	a.t.Helper()
	out, err := a.Client.CallContract(context.Background(), callMsg(token, Calldata(a.t, "balanceOf(address)", "address", who)), nil)
	if err != nil {
		a.t.Fatal(err)
	}
	return new(big.Int).SetBytes(out)
}
