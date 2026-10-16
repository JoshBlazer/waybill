package watcher_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Same image and version as deploy/compose.yaml.
const anvilImage = "ghcr.io/foundry-rs/foundry:v1.8.3"

// emitterCode is runtime bytecode that emits an ERC-20 Transfer log from
// the caller to calldata[0:32] for calldata[32:64], and nothing else. It is
// enough for the watcher, which only reads logs, and needs no compiler:
//
//	PUSH1 0x20 CALLDATALOAD PUSH1 0 MSTORE   ; memory[0:32] = amount
//	PUSH1 0 CALLDATALOAD CALLER              ; topic2 = to, topic1 = from
//	PUSH32 keccak("Transfer(address,address,uint256)")
//	PUSH1 0x20 PUSH1 0 LOG3 STOP
var emitterCode = "0x602035600052600035337f" + transferSig.Hex()[2:] + "60206000a300"

// anvilDev0 is Anvil's first dev account, unlocked by default.
var anvilDev0 = common.HexToAddress("0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266")

func startAnvil(t *testing.T) *rpc.Client {
	t.Helper()
	if testing.Short() {
		t.Skip("starts Anvil in Docker; skipped with -short")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        anvilImage,
			Entrypoint:   []string{"anvil"},
			Cmd:          []string{"--host", "0.0.0.0", "--chain-id", "31337"},
			ExposedPorts: []string{"8545/tcp"},
			WaitingFor:   wait.ForListeningPort("8545/tcp"),
		},
		Started: true,
	})
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start anvil: %v", err)
	}
	endpoint, err := ctr.PortEndpoint(ctx, "8545/tcp", "http")
	if err != nil {
		t.Fatal(err)
	}
	c, err := rpc.DialContext(context.Background(), endpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func call(t *testing.T, c *rpc.Client, result any, method string, args ...any) {
	t.Helper()
	if err := c.CallContext(context.Background(), result, method, args...); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
}

func mine(t *testing.T, c *rpc.Client, n int) {
	t.Helper()
	call(t, c, nil, "anvil_mine", hexutil.EncodeUint64(uint64(n)))
}

// TestWatcher_ReorgReversesConfirmedPayment forces a real reorg on Anvil: a
// payment is confirmed, the chain is reverted to before it and grows past
// the old head without it, and the same signed transaction is then included
// again in a later block. Invariant I8.
func TestWatcher_ReorgReversesConfirmedPayment(t *testing.T) {
	r := setup(t)
	c := startAnvil(t)
	r.w.Chain = ethclient.NewClient(c)
	r.w.Cfg.Confirmations, r.w.Cfg.Finality, r.w.Cfg.BatchBlocks = 2, 50, 500

	call(t, c, nil, "anvil_setCode", token, emitterCode)
	mine(t, c, 3)
	r.tick(t) // remembers the chain up to here: the common ancestor
	var snapshot string
	call(t, c, &snapshot, "evm_snapshot")

	data := append(common.LeftPadBytes(r.dep.Bytes(), 32), common.LeftPadBytes(big100USDC(), 32)...)
	var txHash common.Hash
	call(t, c, &txHash, "eth_sendTransaction", map[string]any{
		"from": anvilDev0, "to": token, "data": hexutil.Bytes(data), "gas": "0x30000",
	})
	var raw hexutil.Bytes
	call(t, c, &raw, "eth_getRawTransactionByHash", txHash)
	mine(t, c, 2)
	r.tick(t)
	if st, ps := r.state(t); st != "confirmed" || ps["confirmed"] != 1 {
		t.Fatalf("before the reorg: invoice %s, payments %v; want confirmed", st, ps)
	}
	if p := r.balance(t, pendingLike); p != "-100000000" {
		t.Fatalf("pending %s before the reorg; want -100000000", p)
	}

	// The reorg: back to the snapshot, then a longer chain without the
	// payment.
	var reverted bool
	call(t, c, &reverted, "evm_revert", snapshot)
	if !reverted {
		t.Fatal("evm_revert returned false")
	}
	mine(t, c, 6)
	r.tick(t)
	if st, ps := r.state(t); st != "open" || ps["reorged"] != 1 || len(ps) != 1 {
		t.Fatalf("after the reorg: invoice %s, payments %v; want open with the payment reorged", st, ps)
	}
	if got := r.entryKinds(t); fmt.Sprint(got) != "[deposit.confirmed deposit.confirmed.reversal]" {
		t.Fatalf("journal = %v; want the confirmation and its reversal", got)
	}
	if p, cu := r.balance(t, pendingLike), r.balance(t, custodyLike); p != "0" || cu != "0" {
		t.Fatalf("pending %s, custody %s after the reorg; want 0", p, cu)
	}
	r.assertTrialBalanceZero(t)

	// The payer's transaction comes back in a later block: same hash, same
	// log index, new payment.
	var again common.Hash
	call(t, c, &again, "eth_sendRawTransaction", raw)
	if again != txHash {
		t.Fatalf("re-sent transaction hash %s, want %s", again, txHash)
	}
	mine(t, c, 2)
	r.tick(t)
	if st, ps := r.state(t); st != "confirmed" || ps["reorged"] != 1 || ps["confirmed"] != 1 {
		t.Fatalf("after re-inclusion: invoice %s, payments %v; want confirmed once", st, ps)
	}
	if p := r.balance(t, pendingLike); p != "-100000000" {
		t.Fatalf("pending %s after re-inclusion; the payment must count exactly once", p)
	}
	r.assertTrialBalanceZero(t)
}

func big100USDC() []byte { return hexutil.MustDecodeBig("0x5f5e100").Bytes() } // 100_000_000
