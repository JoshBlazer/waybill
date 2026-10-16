package watcher_test

import (
	"fmt"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/JoshBlazer/waybill/api/internal/testchain"
)

// emitterCode is runtime bytecode that emits an ERC-20 Transfer log from
// the caller to calldata[0:32] for calldata[32:64], and nothing else. It is
// enough for the watcher, which only reads logs, and needs no compiler:
//
//	PUSH1 0x20 CALLDATALOAD PUSH1 0 MSTORE   ; memory[0:32] = amount
//	PUSH1 0 CALLDATALOAD CALLER              ; topic2 = to, topic1 = from
//	PUSH32 keccak("Transfer(address,address,uint256)")
//	PUSH1 0x20 PUSH1 0 LOG3 STOP
var emitterCode = "0x602035600052600035337f" + transferSig.Hex()[2:] + "60206000a300"

// TestWatcher_ReorgReversesConfirmedPayment forces a real reorg on Anvil: a
// payment is confirmed, the chain is reverted to before it and grows past
// the old head without it, and the same signed transaction is then included
// again in a later block. Invariant I8.
func TestWatcher_ReorgReversesConfirmedPayment(t *testing.T) {
	r := setup(t)
	a := testchain.Start(t)
	r.w.Chain = a.Client
	r.w.Cfg.Confirmations, r.w.Cfg.Finality, r.w.Cfg.BatchBlocks = 2, 50, 500

	a.Call(nil, "anvil_setCode", token, emitterCode)
	a.Mine(3)
	r.tick(t) // remembers the chain up to here: the common ancestor
	var snapshot string
	a.Call(&snapshot, "evm_snapshot")

	data := append(common.LeftPadBytes(r.dep.Bytes(), 32), common.LeftPadBytes(big100USDC(), 32)...)
	txHash := a.Send(testchain.Dev0, token, data)
	var raw hexutil.Bytes
	a.Call(&raw, "eth_getRawTransactionByHash", txHash)
	a.Mine(2)
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
	a.Call(&reverted, "evm_revert", snapshot)
	if !reverted {
		t.Fatal("evm_revert returned false")
	}
	a.Mine(6)
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
	a.Call(&again, "eth_sendRawTransaction", raw)
	if again != txHash {
		t.Fatalf("re-sent transaction hash %s, want %s", again, txHash)
	}
	a.Mine(2)
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
