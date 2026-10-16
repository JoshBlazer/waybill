package txmgr_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshBlazer/waybill/api/internal/store"
	"github.com/JoshBlazer/waybill/api/internal/testchain"
	"github.com/JoshBlazer/waybill/api/internal/testdb"
	"github.com/JoshBlazer/waybill/api/internal/txmgr"
)

type mrig struct {
	pool  *pgxpool.Pool
	anvil *testchain.Anvil
	m     *txmgr.Manager
	hooks *[]common.Hash
}

// newSigner makes a fresh key, funded on the test chain. Tests never use a
// fixed private key.
func newSigner(t *testing.T, a *testchain.Anvil) *txmgr.LocalSigner {
	t.Helper()
	k, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	s, err := txmgr.ParseKey(hex.EncodeToString(crypto.FromECDSA(k)))
	if err != nil {
		t.Fatal(err)
	}
	a.Fund(s.Address(), "0x56BC75E2D63100000") // 100 ETH
	return s
}

func newManager(t *testing.T, args ...string) mrig {
	t.Helper()
	pool := testdb.New(t)
	a := testchain.Start(t, args...)
	var hooked []common.Hash
	m := &txmgr.Manager{
		Pool: pool, Chain: a.Client, Signer: newSigner(t, a), Network: network, ChainID: big.NewInt(31337),
		Owner: "worker-a", Log: slog.New(slog.DiscardHandler),
		LeaseTTL: time.Minute, MaxFeeCap: big.NewInt(1_000_000_000_000), BumpPercent: 10,
		StuckAfter: time.Hour, Confirmations: 2,
		Hooks: map[string]txmgr.Hook{"test": func(_ context.Context, _ pgx.Tx, _ store.OutboundTx, r *types.Receipt) error {
			hooked = append(hooked, r.TxHash)
			return nil
		}},
	}
	return mrig{pool: pool, anvil: a, m: m, hooks: &hooked}
}

func (r mrig) row(t *testing.T, ref string) store.OutboundTx {
	t.Helper()
	row, err := store.New(r.pool).GetOutboundTxByRef(context.Background(), store.GetOutboundTxByRefParams{Purpose: "test", RefID: ref})
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func (r mrig) attempts(t *testing.T, id store.OutboundTx) []store.OutboundTxAttempt {
	t.Helper()
	as, err := store.New(r.pool).ListOutboundAttempts(context.Background(), id.ID)
	if err != nil {
		t.Fatal(err)
	}
	return as
}

func (r mrig) tick(t *testing.T) {
	t.Helper()
	if err := r.m.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}

var recipient = common.HexToAddress("0x00000000000000000000000000000000000000b2")

func payment(ref string) txmgr.Request {
	return txmgr.Request{Purpose: "test", RefID: ref, To: recipient, Value: big.NewInt(12345)}
}

func TestTxMgr_SubmitSendsAndHookRunsOnceWhenMined(t *testing.T) {
	r := newManager(t)
	ctx := context.Background()
	if _, err := r.m.Submit(ctx, payment("p1")); err != nil {
		t.Fatal(err)
	}
	row := r.row(t, "p1")
	if row.State != "pending" || row.Nonce == nil || *row.Nonce != 0 || len(r.attempts(t, row)) != 1 {
		t.Fatalf("after submit: %+v with %d attempts; want pending, nonce 0, one attempt", row, len(r.attempts(t, row)))
	}

	r.tick(t) // mined (automine) but 1 block deep < 2 confirmations
	if r.row(t, "p1").State != "pending" || len(*r.hooks) != 0 {
		t.Fatal("finished before reaching the required confirmations")
	}
	r.anvil.Mine(1)
	r.tick(t)
	r.tick(t)
	row = r.row(t, "p1")
	if row.State != "mined" || row.MinedHash == nil || len(*r.hooks) != 1 || strings.ToLower((*r.hooks)[0].Hex()) != *row.MinedHash {
		t.Fatalf("after confirmations: %+v, hooks %v; want mined once", row, *r.hooks)
	}
	bal, _ := r.anvil.Client.BalanceAt(ctx, recipient, nil)
	if bal.Cmp(big.NewInt(12345)) != 0 {
		t.Fatalf("recipient balance %s, want 12345", bal)
	}
}

func TestTxMgr_SubmitIsIdempotentPerRef(t *testing.T) {
	r := newManager(t)
	ctx := context.Background()
	a, err := r.m.Submit(ctx, payment("same"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.m.Submit(ctx, payment("same"))
	if err != nil {
		t.Fatal(err)
	}
	n, _ := r.anvil.Client.PendingNonceAt(ctx, r.m.Signer.Address())
	if a.ID != b.ID || len(r.attempts(t, a)) != 1 || n != 1 {
		t.Fatalf("rows %s/%s, %d attempts, chain nonce %d; want one transaction sent once", a.ID, b.ID, len(r.attempts(t, a)), n)
	}
}

// TestTxMgr_StuckTransactionIsReplaced is invariant I11 on a real node: a
// transaction priced under the base fee sits in the mempool; Tick replaces
// it at the same nonce with both fees raised, the replacement is mined, and
// both hashes stay on record (TestTxMgr_AllAttemptHashesRecorded).
func TestTxMgr_StuckTransactionIsReplaced(t *testing.T) {
	r := newManager(t, "--no-mining")
	ctx := context.Background()
	r.m.StuckAfter = 0
	if _, err := r.m.Submit(ctx, payment("stuck")); err != nil {
		t.Fatal(err)
	}
	row := r.row(t, "stuck")
	first := r.attempts(t, row)[0]

	// The base fee jumps far above what the first attempt offered, so the
	// next block leaves it out.
	spike := new(big.Int).Mul(first.MaxFee.Big(), big.NewInt(50))
	r.anvil.Call(nil, "anvil_setNextBlockBaseFeePerGas", "0x"+spike.Text(16))
	r.anvil.Mine(1)
	if rc, _ := r.anvil.Client.TransactionReceipt(ctx, common.HexToHash(first.Hash)); rc != nil {
		t.Fatal("the underpriced attempt was mined; the test did not make it stuck")
	}

	r.tick(t) // stuck: replaced
	attempts := r.attempts(t, row)
	if len(attempts) != 2 {
		t.Fatalf("%d attempts after a stuck tick, want 2", len(attempts))
	}
	second := attempts[1]
	if second.MaxFee.Big().Cmp(first.MaxFee.Big()) <= 0 || second.Tip.Big().Cmp(first.Tip.Big()) <= 0 {
		t.Fatalf("replacement fees max %s tip %s do not both exceed max %s tip %s", second.MaxFee, second.Tip, first.MaxFee, first.Tip)
	}
	r.anvil.Mine(2)
	r.tick(t)
	row = r.row(t, "stuck")
	if row.State != "mined" || *row.MinedHash != second.Hash {
		t.Fatalf("row %+v; want mined by the replacement %s", row, second.Hash)
	}
	if rc, _ := r.anvil.Client.TransactionReceipt(ctx, common.HexToHash(first.Hash)); rc != nil {
		t.Fatal("both attempts were mined; they must share a nonce")
	}
	// Both hashes are still on record, and the record cannot be edited.
	if got := r.attempts(t, row); len(got) != 2 || got[0].Hash != first.Hash || got[1].Hash != second.Hash {
		t.Fatalf("attempts %+v; want both hashes kept", got)
	}
	if _, err := r.pool.Exec(ctx, `DELETE FROM outbound_tx_attempts`); err == nil {
		t.Fatal("attempt history could be deleted")
	}
}

// revertingCode reverts with Error(reason) on every call.
func revertingCode(reason string) string {
	var data bytes.Buffer
	data.Write(crypto.Keccak256([]byte("Error(string)"))[:4])
	data.Write(common.LeftPadBytes([]byte{0x20}, 32))
	data.Write(common.LeftPadBytes([]byte{byte(len(reason))}, 32))
	data.Write(common.RightPadBytes([]byte(reason), 32))
	size := data.Len()
	buf := common.RightPadBytes(data.Bytes(), 128)
	var code bytes.Buffer
	for i := 0; i < 4; i++ {
		code.WriteByte(0x7f) // PUSH32
		code.Write(buf[i*32 : i*32+32])
		code.Write([]byte{0x60, byte(i * 32), 0x52}) // PUSH1 offset MSTORE
	}
	code.Write([]byte{0x60, byte(size), 0x60, 0x00, 0xfd}) // REVERT(0, size)
	return "0x" + hex.EncodeToString(code.Bytes())
}

// Gas estimation fails because the call would revert: the reason is
// recorded, no nonce is used, nothing is broadcast, and asking again gives
// the same answer without another estimate.
func TestTxMgr_RevertingCallIsRecordedNotSent(t *testing.T) {
	r := newManager(t)
	ctx := context.Background()
	target := common.HexToAddress("0x00000000000000000000000000000000000000c3")
	r.anvil.Call(nil, "anvil_setCode", target, revertingCode("SweepMismatch"))

	req := txmgr.Request{Purpose: "test", RefID: "bad", To: target, Data: []byte{1, 2, 3, 4}}
	_, err := r.m.Submit(ctx, req)
	if !errors.Is(err, txmgr.ErrEstimateFailed) || !strings.Contains(err.Error(), "SweepMismatch") {
		t.Fatalf("err = %v; want ErrEstimateFailed with the revert reason", err)
	}
	row := r.row(t, "bad")
	if row.State != "failed" || row.Nonce != nil || len(r.attempts(t, row)) != 0 || !strings.Contains(*row.Failure, "SweepMismatch") {
		t.Fatalf("row %+v; want failed, no nonce, no attempts, reason kept", row)
	}
	n, _ := r.anvil.Client.PendingNonceAt(ctx, r.m.Signer.Address())
	var stored int
	_ = r.pool.QueryRow(ctx, `SELECT count(*) FROM signer_nonces WHERE next_nonce > 0`).Scan(&stored)
	if n != 0 || stored != 0 {
		t.Fatalf("chain nonce %d, stored nonces %d; a failed estimate must not use a nonce", n, stored)
	}
	if _, err := r.m.Submit(ctx, req); !errors.Is(err, txmgr.ErrEstimateFailed) {
		t.Fatalf("second submit: %v; want the recorded failure", err)
	}
	// The next good transaction gets nonce 0: no gap.
	if _, err := r.m.Submit(ctx, payment("after")); err != nil {
		t.Fatal(err)
	}
	if got := r.row(t, "after"); *got.Nonce != 0 {
		t.Fatalf("next transaction got nonce %d, want 0", *got.Nonce)
	}
}

type unreachable struct{ txmgr.Chain }

func (unreachable) EstimateGas(context.Context, ethereum.CallMsg) (uint64, error) {
	return 0, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
}

// A node that cannot be reached says nothing about the transaction: no row
// is recorded, so a later retry estimates again (I12).
func TestTxMgr_UnreachableNodeIsNotAFailure(t *testing.T) {
	r := newManager(t)
	r.m.Chain = unreachable{r.m.Chain}
	_, err := r.m.Submit(context.Background(), payment("p"))
	if err == nil || errors.Is(err, txmgr.ErrEstimateFailed) {
		t.Fatalf("err = %v; want a retryable error, not ErrEstimateFailed", err)
	}
	var n int
	_ = r.pool.QueryRow(context.Background(), `SELECT count(*) FROM outbound_txs`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d rows recorded for an estimate that never happened", n)
	}
}

// Fees above the cap: the nonce is assigned but nothing is broadcast until
// fees fit, then Tick sends it.
func TestTxMgr_FeeCapHoldsBroadcastUntilFeesFit(t *testing.T) {
	r := newManager(t)
	r.m.MaxFeeCap = big.NewInt(1)
	if _, err := r.m.Submit(context.Background(), payment("capped")); err != nil {
		t.Fatal(err)
	}
	r.tick(t)
	row := r.row(t, "capped")
	if row.State != "pending" || len(r.attempts(t, row)) != 0 {
		t.Fatalf("row %+v with %d attempts; want pending and nothing sent", row, len(r.attempts(t, row)))
	}
	r.m.MaxFeeCap = big.NewInt(1_000_000_000_000)
	r.tick(t)
	r.anvil.Mine(2)
	r.tick(t)
	if row := r.row(t, "capped"); row.State != "mined" || len(r.attempts(t, row)) != 1 {
		t.Fatalf("row %+v; want mined after fees fit", row)
	}
}

// Another process holding the key's lease does the work; this one ticks
// without touching anything.
func TestTxMgr_OnlyLeaseHolderTicks(t *testing.T) {
	r := newManager(t, "--no-mining")
	r.m.StuckAfter = 0
	if _, err := r.m.Submit(context.Background(), payment("p")); err != nil {
		t.Fatal(err)
	}
	other := &txmgr.Manager{
		Pool: r.m.Pool, Chain: r.m.Chain, Signer: r.m.Signer, Network: r.m.Network, ChainID: r.m.ChainID,
		Owner: "worker-b", Log: r.m.Log, LeaseTTL: r.m.LeaseTTL, MaxFeeCap: r.m.MaxFeeCap,
		BumpPercent: r.m.BumpPercent, StuckAfter: r.m.StuckAfter, Confirmations: r.m.Confirmations,
	}
	if err := other.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := r.attempts(t, r.row(t, "p")); len(got) != 1 {
		t.Fatalf("%d attempts; a process without the lease replaced the transaction", len(got))
	}
}

// TestSecret_NeverAppearsInLogs is invariant I15 for signing keys.
func TestSecret_NeverAppearsInLogs(t *testing.T) {
	k, _ := crypto.GenerateKey()
	keyHex := hex.EncodeToString(crypto.FromECDSA(k))
	s, err := txmgr.ParseKey("0x" + keyHex)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("signer", "s", s)
	out := fmt.Sprintf("%v %+v %#v %s", s, s, s, s) + logs.String()
	if strings.Contains(strings.ToLower(out), keyHex) {
		t.Fatal("the private key appears in formatted output or logs")
	}
	if !strings.Contains(out, s.Address().Hex()) {
		t.Fatalf("output %q does not identify the signer by address", out)
	}
	if _, err := txmgr.ParseKey("0x" + keyHex[:10] + "zz"); err == nil || strings.Contains(err.Error(), keyHex[:10]) {
		t.Fatalf("bad key error %v; must fail without echoing the input", err)
	}
}

// driftingTips makes every fee quote different and slow, so a broadcast by
// Tick that interleaves with Submit's would show up as a second attempt.
type driftingTips struct {
	txmgr.Chain
	mu sync.Mutex
	n  int64
}

func (c *driftingTips) SuggestGasTipCap(ctx context.Context) (*big.Int, error) {
	c.mu.Lock()
	c.n++
	n := c.n
	c.mu.Unlock()
	time.Sleep(30 * time.Millisecond)
	tip, err := c.Chain.SuggestGasTipCap(ctx)
	if err != nil {
		return nil, err
	}
	return tip.Add(tip, big.NewInt(n)), nil
}

// Submit and the Tick loop in the same process never both broadcast a new
// transaction: each gets exactly one attempt.
func TestTxMgr_SubmitAndTickDoNotDoubleBroadcast(t *testing.T) {
	r := newManager(t, "--no-mining")
	r.m.Chain = &driftingTips{Chain: r.m.Chain}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			_ = r.m.Tick(ctx)
		}
	}()
	for i := range 5 {
		if _, err := r.m.Submit(context.Background(), payment(fmt.Sprintf("p%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	<-done
	for i := range 5 {
		if got := r.attempts(t, r.row(t, fmt.Sprintf("p%d", i))); len(got) != 1 {
			t.Fatalf("transaction p%d has %d attempts; Submit and Tick both broadcast it", i, len(got))
		}
	}
}
