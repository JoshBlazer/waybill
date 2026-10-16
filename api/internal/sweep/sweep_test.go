package sweep_test

import (
	"context"
	"encoding/hex"
	"log/slog"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshBlazer/waybill/api/internal/deployments"
	"github.com/JoshBlazer/waybill/api/internal/invoice"
	"github.com/JoshBlazer/waybill/api/internal/ledger"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/outbox"
	"github.com/JoshBlazer/waybill/api/internal/safety"
	"github.com/JoshBlazer/waybill/api/internal/store"
	"github.com/JoshBlazer/waybill/api/internal/sweep"
	"github.com/JoshBlazer/waybill/api/internal/testchain"
	"github.com/JoshBlazer/waybill/api/internal/testdb"
	"github.com/JoshBlazer/waybill/api/internal/txmgr"
	"github.com/JoshBlazer/waybill/api/internal/watcher"
)

const anvil = safety.EVMAnvil

type rig struct {
	pool    *pgxpool.Pool
	a       *testchain.Anvil
	d       deployments.Deployment
	inv     invoice.Invoice
	dep     common.Address
	w       *watcher.Watcher
	m       *txmgr.Manager
	worker  *outbox.Worker
	sweeper *sweep.Sweeper
}

func signer(t *testing.T, a *testchain.Anvil) *txmgr.LocalSigner {
	t.Helper()
	k, _ := crypto.GenerateKey()
	s, err := txmgr.ParseKey(hex.EncodeToString(crypto.FromECDSA(k)))
	if err != nil {
		t.Fatal(err)
	}
	a.Fund(s.Address(), "0x56BC75E2D63100000")
	return s
}

// setup deploys Waybill's contracts on Anvil, creates a 100 USDC invoice and
// wires the watcher, transaction manager, sweeper and outbox worker as the
// services do. roleHolder gets the sweeper role; nil means the signer.
func setup(t *testing.T, roleHolder *common.Address) rig {
	t.Helper()
	pool := testdb.New(t)
	a := testchain.Start(t)
	s := signer(t, a)
	holder := s.Address()
	if roleHolder != nil {
		holder = *roleHolder
	}
	d := a.DeployWaybill(holder)
	ctx := context.Background()

	c, err := store.New(pool).UpsertContractor(ctx, store.UpsertContractorParams{ID: uuid.New(), LegalName: "Adaeze Okafor"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.New(pool).UpsertChainToken(ctx, store.UpsertChainTokenParams{
		Network: string(anvil), ContractAddress: strings.ToLower(d.Token.Hex()), AssetCode: "USDC",
	}); err != nil {
		t.Fatal(err)
	}
	svc := &invoice.Service{Deployments: map[safety.Network]deployments.Deployment{anvil: d}, Now: time.Now}
	var inv invoice.Invoice
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		inv, err = svc.Create(ctx, tx, c.ID, invoice.Input{Description: "Work", Amount: "100", Asset: "USDC", Payout: "hold"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	m := &txmgr.Manager{
		Pool: pool, Chain: a.Client, Signer: s, Network: anvil, ChainID: big.NewInt(31337), Owner: "worker-a", Log: log,
		LeaseTTL: time.Minute, MaxFeeCap: big.NewInt(1_000_000_000_000), BumpPercent: 10, StuckAfter: time.Hour, Confirmations: 2,
	}
	sw := &sweep.Sweeper{
		Pool: pool, Managers: map[safety.Network]*txmgr.Manager{anvil: m},
		Deployments: map[safety.Network]deployments.Deployment{anvil: d}, Asset: money.USDC, Log: log,
	}
	m.Hooks = map[string]txmgr.Hook{sweep.Purpose: sw.Hook(anvil)}
	return rig{
		pool: pool, a: a, d: d, inv: inv, dep: common.HexToAddress(inv.DepositAddresses[0].Address), m: m, sweeper: sw,
		w: &watcher.Watcher{Pool: pool, Chain: a.Client, Log: log, Cfg: watcher.Config{
			Network: anvil, Token: d.Token, Asset: money.USDC, Confirmations: 2, Finality: 3, BatchBlocks: 500,
		}},
		worker: &outbox.Worker{
			Pool: pool, Owner: "worker-a", Log: log, Lease: time.Minute, Batch: 10, MaxAttempts: 3,
			Backoff:  func(int) time.Duration { return 0 },
			Handlers: map[string]outbox.Handler{sweep.Topic: sw.Handle},
		},
	}
}

func (r rig) watch(t *testing.T, rounds int) {
	t.Helper()
	for range rounds {
		r.a.Mine(1)
		if err := r.w.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func (r rig) invoiceState(t *testing.T) string {
	t.Helper()
	var s string
	if err := r.pool.QueryRow(context.Background(), `SELECT state FROM invoices WHERE id = $1`, r.inv.ID).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (r rig) balance(t *testing.T, code string) string {
	t.Helper()
	var b string
	if err := r.pool.QueryRow(context.Background(), `
		SELECT coalesce(sum(b.balance), 0)::text FROM ledger_balances b JOIN ledger_accounts a ON a.id = b.account_id
		WHERE a.code = $1`, code).Scan(&b); err != nil {
		t.Fatal(err)
	}
	return b
}

func (r rig) runJobs(t *testing.T) {
	t.Helper()
	if _, err := r.worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSweep_FinalPaymentIsSweptIntoVault(t *testing.T) {
	r := setup(t, nil)
	ctx := context.Background()
	r.a.Mint(r.d.Token, r.dep, 100_000_000) // the payer pays 100 USDC
	r.watch(t, 6)
	if st := r.invoiceState(t); st != "settled" {
		t.Fatalf("invoice %s; want settled before sweeping", st)
	}

	if n, err := sweep.EnqueueDue(ctx, r.pool, anvil); err != nil || n != 1 {
		t.Fatalf("EnqueueDue = %d, %v; want 1 job", n, err)
	}
	if n, _ := sweep.EnqueueDue(ctx, r.pool, anvil); n != 0 {
		t.Fatalf("EnqueueDue again = %d; want 0 (one sweep per payment)", n)
	}
	r.runJobs(t) // submits deployAndSweep; Anvil mines it
	// Mine and tick until the sweep is deep enough, as the worker would.
	var state string
	for range 10 {
		r.a.Mine(1)
		if err := r.m.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		_ = r.pool.QueryRow(ctx, `SELECT state FROM outbound_txs WHERE purpose = 'sweep'`).Scan(&state)
		if state != "pending" {
			break
		}
	}

	if got := r.a.TokenBalance(r.d.Token, r.d.Vault); got.Cmp(big.NewInt(100_000_000)) != 0 {
		t.Fatalf("vault holds %s, want 100000000", got)
	}
	if got := r.a.TokenBalance(r.d.Token, r.dep); got.Sign() != 0 {
		t.Fatalf("deposit address still holds %s", got)
	}
	if state != "mined" {
		t.Fatalf("sweep transaction %q, want mined", state)
	}
	if v, dp := r.balance(t, "custody:vault:evm_31337:USDC"), r.balance(t, "custody:deposit:evm_31337:USDC"); v != "100000000" || dp != "0" {
		t.Fatalf("ledger: vault %s, deposit %s; want 100000000 and 0", v, dp)
	}
	if a := r.balance(t, "liability:contractor:"+r.inv.ContractorID.String()+":USDC:available"); a != "-100000000" {
		t.Fatalf("contractor available %s; sweeping must not change what is owed", a)
	}
	tb, err := ledger.TrialBalance(ctx, r.pool)
	if err != nil {
		t.Fatal(err)
	}
	for asset, v := range tb {
		if !v.IsZero() {
			t.Fatalf("trial balance %s = %s", asset, v)
		}
	}
	// Ticking again records nothing more.
	r.a.Mine(1)
	_ = r.m.Tick(ctx)
	var entries int
	_ = r.pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries WHERE kind = 'sweep.confirmed'`).Scan(&entries)
	if entries != 1 {
		t.Fatalf("%d sweep entries, want 1", entries)
	}
}

// A second payment is still unconfirmed: sweeping now would move money the
// ledger has not counted, so the job waits.
func TestSweep_WaitsWhileAPaymentIsUnconfirmed(t *testing.T) {
	r := setup(t, nil)
	ctx := context.Background()
	r.a.Mint(r.d.Token, r.dep, 100_000_000)
	r.watch(t, 6)
	if _, err := sweep.EnqueueDue(ctx, r.pool, anvil); err != nil {
		t.Fatal(err)
	}
	r.a.Mint(r.d.Token, r.dep, 5_000_000) // arrives before the sweep runs
	if err := r.w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	r.runJobs(t)
	var txs int
	var lastErr *string
	_ = r.pool.QueryRow(ctx, `SELECT count(*) FROM outbound_txs`).Scan(&txs)
	_ = r.pool.QueryRow(ctx, `SELECT last_error FROM outbox WHERE topic = 'sweep.deposit'`).Scan(&lastErr)
	if txs != 0 || lastErr == nil || !strings.Contains(*lastErr, "unconfirmed") {
		t.Fatalf("%d transactions sent, last error %v; want none sent and a retry", txs, lastErr)
	}
}

// The signer lacks the factory's sweeper role, so the sweep would revert:
// gas estimation fails, the reason is recorded, nothing is broadcast, and
// the job goes dead for a person to look at.
func TestSweep_WithoutSweeperRoleIsRecordedNotSent(t *testing.T) {
	someoneElse := common.HexToAddress("0x00000000000000000000000000000000000000e5")
	r := setup(t, &someoneElse)
	ctx := context.Background()
	r.a.Mint(r.d.Token, r.dep, 100_000_000)
	r.watch(t, 6)
	if _, err := sweep.EnqueueDue(ctx, r.pool, anvil); err != nil {
		t.Fatal(err)
	}
	r.runJobs(t)

	var state string
	var nonce *int64
	var failure *string
	_ = r.pool.QueryRow(ctx, `SELECT state, nonce, failure FROM outbound_txs WHERE purpose = 'sweep'`).Scan(&state, &nonce, &failure)
	var dead bool
	_ = r.pool.QueryRow(ctx, `SELECT dead_at IS NOT NULL FROM outbox WHERE topic = 'sweep.deposit'`).Scan(&dead)
	sent, _ := r.a.Client.PendingNonceAt(ctx, r.m.Signer.Address())
	// AccessControlUnauthorizedAccount(address,bytes32) is 0xe2517d3f.
	if state != "failed" || nonce != nil || failure == nil || !strings.Contains(*failure, "e2517d3f") || !dead || sent != 0 {
		t.Fatalf("state %q nonce %v failure %v dead %v sent %d; want a recorded revert, nothing sent", state, nonce, failure, dead, sent)
	}
	if got := r.a.TokenBalance(r.d.Token, r.dep); got.Cmp(big.NewInt(100_000_000)) != 0 {
		t.Fatalf("deposit holds %s; nothing should have moved", got)
	}
}
