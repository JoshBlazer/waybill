package batch_test

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshBlazer/waybill/api/internal/auth"
	"github.com/JoshBlazer/waybill/api/internal/batch"
	"github.com/JoshBlazer/waybill/api/internal/deployments"
	"github.com/JoshBlazer/waybill/api/internal/ledger"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/org"
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

// rig is the stage 4 stack on a real Anvil: Waybill's contracts, the
// watcher, the transaction manager with the sweep and batch hooks, the
// outbox worker, and an organisation with an admin (maker) and an approver.
type rig struct {
	pool            *pgxpool.Pool
	a               *testchain.Anvil
	d               deployments.Deployment
	w               *watcher.Watcher
	m               *txmgr.Manager
	worker          *outbox.Worker
	svc             *batch.Service
	sender          *batch.Sender
	orgs            *org.Service
	orgID           uuid.UUID
	maker, approver auth.Principal
	wallets         []common.Address
	contractors     []uuid.UUID
}

func setup(t *testing.T) rig {
	t.Helper()
	ctx := context.Background()
	pool := testdb.New(t)
	a := testchain.Start(t)
	k, _ := crypto.GenerateKey()
	signer, err := txmgr.ParseKey(hex.EncodeToString(crypto.FromECDSA(k)))
	if err != nil {
		t.Fatal(err)
	}
	a.Fund(signer.Address(), "0x56BC75E2D63100000")
	d := a.DeployWaybill(signer.Address())
	q := store.New(pool)
	if err := q.UpsertChainToken(ctx, store.UpsertChainTokenParams{Network: string(anvil), ContractAddress: strings.ToLower(d.Token.Hex()), AssetCode: "USDC"}); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	deps := map[safety.Network]deployments.Deployment{anvil: d}
	svc := &batch.Service{Pool: pool, Deployments: deps, Log: log,
		Limits: map[safety.Network]batch.VaultLimits{anvil: batch.ChainVault{Client: a.Client, Vault: d.Vault, Token: d.Token}}}
	m := &txmgr.Manager{
		Pool: pool, Chain: a.Client, Signer: signer, Network: anvil, ChainID: big.NewInt(31337), Owner: "worker-a", Log: log,
		LeaseTTL: time.Minute, MaxFeeCap: big.NewInt(1_000_000_000_000), BumpPercent: 10, StuckAfter: time.Hour, Confirmations: 2,
	}
	sw := &sweep.Sweeper{Pool: pool, Managers: map[safety.Network]*txmgr.Manager{anvil: m}, Deployments: deps, Asset: money.USDC, Log: log}
	m.Hooks = map[string]txmgr.Hook{sweep.Purpose: sw.Hook(anvil), batch.Purpose: svc.Hook(anvil)}
	sender := &batch.Sender{Service: svc, Managers: map[safety.Network]*txmgr.Manager{anvil: m},
		Finality: map[safety.Network]uint64{anvil: 3}, Chains: map[safety.Network]batch.ChainReader{anvil: a.Client}}

	o, err := q.UpsertOrg(ctx, store.UpsertOrgParams{ID: uuid.New(), Name: "Lagoon Labs", ApprovalThreshold: money.FromInt64(500_000_000)})
	if err != nil {
		t.Fatal(err)
	}
	member := func(name string, roles ...string) auth.Principal {
		mm, err := q.UpsertMember(ctx, store.UpsertMemberParams{ID: uuid.New(), OrgID: o.ID, Name: name, Roles: roles})
		if err != nil {
			t.Fatal(err)
		}
		return auth.Principal{MemberID: mm.ID, OrgID: o.ID}
	}
	r := rig{
		pool: pool, a: a, d: d, m: m, svc: svc, sender: sender, orgID: o.ID,
		orgs:     &org.Service{Pool: pool, Deployments: deps},
		maker:    member("Tolu Bakare", "admin", "maker"),
		approver: member("Chidi Eze", "approver"),
		w: &watcher.Watcher{Pool: pool, Chain: a.Client, Log: log, Cfg: watcher.Config{
			Network: anvil, Token: d.Token, Asset: money.USDC, Confirmations: 2, Finality: 3, BatchBlocks: 500,
		}},
		worker: &outbox.Worker{
			Pool: pool, Owner: "worker-a", Log: log, Lease: time.Minute, Batch: 10, MaxAttempts: 3,
			Backoff:  func(int) time.Duration { return 0 },
			Handlers: map[string]outbox.Handler{sweep.Topic: sw.Handle, batch.Topic: sender.Handle},
			// As in production (batch.Policy), without the 15 s wait.
			Policies: map[string]outbox.Policy{batch.Topic: {MaxAttempts: batch.Policy().MaxAttempts, Backoff: func(int) time.Duration { return 0 }}},
		},
	}
	now := time.Now()
	for i, name := range []string{"Adaeze Okafor", "Bello Tunde"} {
		c, err := q.UpsertContractor(ctx, store.UpsertContractorParams{ID: uuid.New(), LegalName: name, VerifiedAt: &now})
		if err != nil {
			t.Fatal(err)
		}
		w := common.BigToAddress(big.NewInt(int64(0xA11CE + i)))
		if _, err := pool.Exec(ctx, `INSERT INTO contractor_wallets (contractor_id, network, address, payouts_allowed_from)
			VALUES ($1, $2, $3, now() - interval '1 hour')`, c.ID, string(anvil), strings.ToLower(w.Hex())); err != nil {
			t.Fatal(err)
		}
		r.contractors = append(r.contractors, c.ID)
		r.wallets = append(r.wallets, w)
	}
	return r
}

// fundAndSweep sends usdc to the organisation's funding address and runs
// the watcher and the sweeper until it is available and in the vault.
func (r rig) fundAndSweep(t *testing.T, usdc int64) {
	t.Helper()
	ctx := context.Background()
	f, err := r.orgs.Funding(ctx, r.orgID)
	if err != nil {
		t.Fatal(err)
	}
	r.a.Mint(r.d.Token, common.HexToAddress(f.Addresses[0].Address), usdc*1_000_000)
	for range 6 {
		r.a.Mine(1)
		if err := r.w.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sweep.EnqueueDue(ctx, r.pool, anvil); err != nil {
		t.Fatal(err)
	}
	r.drive(t, func() bool {
		var n int
		_ = r.pool.QueryRow(ctx, `SELECT count(*) FROM outbound_txs WHERE purpose = 'sweep' AND state = 'mined'`).Scan(&n)
		return n > 0
	})
}

// drive runs jobs, mines and ticks the transaction manager and the batch
// sender until done reports true.
func (r rig) drive(t *testing.T, done func() bool) {
	t.Helper()
	ctx := context.Background()
	for range 20 {
		if _, err := r.worker.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		r.a.Mine(1)
		if err := r.m.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if err := r.sender.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if done() {
			return
		}
	}
	t.Fatal("did not finish in 20 rounds")
}

func (r rig) create(t *testing.T, lines ...batch.LineInput) batch.Run {
	t.Helper()
	var run batch.Run
	err := pgx.BeginFunc(context.Background(), r.pool, func(tx pgx.Tx) error {
		var err error
		run, err = r.svc.Create(context.Background(), tx, r.maker, "", lines)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func (r rig) state(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := r.pool.QueryRow(context.Background(), `SELECT state FROM batches WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func trialBalanceZero(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	tb, err := ledger.TrialBalance(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	for asset, v := range tb {
		if !v.IsZero() {
			t.Fatalf("trial balance %s = %s", asset, v)
		}
	}
}

// TestExitCheck_BatchWithDualApprovalCompletes is the stage 4 exit check
// (ROADMAP): a run above the threshold is refused to its creator, approved
// by a second member, and paid to two wallets in one BatchPayout
// transaction; the books balance and the vault matches the ledger.
func TestExitCheck_BatchWithDualApprovalCompletes(t *testing.T) {
	r := setup(t)
	ctx := context.Background()
	r.fundAndSweep(t, 1000)

	run := r.create(t,
		batch.LineInput{ContractorID: r.contractors[0], Amount: "400", Reference: "October retainer"},
		batch.LineInput{ContractorID: r.contractors[1], Amount: "250.5", Reference: "October retainer"},
	)
	if run.State != "awaiting_approval" {
		t.Fatalf("run of 650.5 above a 500 threshold is %s; want awaiting_approval (problem %v)", run.State, run.Failure)
	}
	if _, err := r.svc.Approve(ctx, r.maker, run.ID); !errors.Is(err, batch.ErrForbidden) && !errors.Is(err, batch.ErrSelfApproval) {
		t.Fatalf("creator approving: %v; want refused", err)
	}
	if _, err := r.svc.Send(ctx, r.maker, run.ID); !errors.Is(err, batch.ErrWrongState) {
		t.Fatalf("sending before approval: %v; want ErrWrongState", err)
	}
	if _, err := r.svc.Approve(ctx, r.approver, run.ID); err != nil {
		t.Fatalf("second member approving: %v", err)
	}
	if _, err := r.svc.Send(ctx, r.maker, run.ID); err != nil {
		t.Fatalf("send: %v", err)
	}
	r.drive(t, func() bool { return r.state(t, run.ID) == "paid" })

	// One transaction paid both wallets.
	var txs int
	_ = r.pool.QueryRow(ctx, `SELECT count(*) FROM outbound_txs WHERE purpose = 'batch_payout'`).Scan(&txs)
	got0, got1 := r.a.TokenBalance(r.d.Token, r.wallets[0]), r.a.TokenBalance(r.d.Token, r.wallets[1])
	if txs != 1 || got0.Int64() != 400_000_000 || got1.Int64() != 250_500_000 {
		t.Fatalf("%d transactions; wallets hold %s and %s; want 1 transaction paying 400 and 250.5", txs, got0, got1)
	}
	f, _ := r.orgs.Funding(ctx, r.orgID)
	if f.Available.String() != "349500000" {
		t.Fatalf("organisation available %s; want 1000 - 650.5 = 349.5", f.Available)
	}
	trialBalanceZero(t, r.pool)
	// Reconciliation: the vault on-chain holds exactly what the ledger says.
	var custody string
	_ = r.pool.QueryRow(ctx, `SELECT b.balance::text FROM ledger_balances b JOIN ledger_accounts a ON a.id = b.account_id
		WHERE a.code = 'custody:vault:evm_31337:USDC'`).Scan(&custody)
	if onChain := r.a.TokenBalance(r.d.Token, r.d.Vault); onChain.String() != custody || custody != "349500000" {
		t.Fatalf("vault holds %s on-chain, ledger says %s; want both 349500000", onChain, custody)
	}
	var inflight string
	_ = r.pool.QueryRow(ctx, `SELECT coalesce(sum(b.balance),0)::text FROM ledger_balances b JOIN ledger_accounts a ON a.id = b.account_id
		WHERE a.code LIKE 'liability:batch:%'`).Scan(&inflight)
	if inflight != "0" {
		t.Fatalf("in-flight %s after payment; want 0", inflight)
	}
}

// TestBatch_WaitsForFundingToReachTheVault: a run sent while the
// organisation's funding is final but not yet swept waits, sends nothing,
// and is paid once the sweep has moved the money into the vault.
func TestBatch_WaitsForFundingToReachTheVault(t *testing.T) {
	r := setup(t)
	ctx := context.Background()
	f, _ := r.orgs.Funding(ctx, r.orgID)
	r.a.Mint(r.d.Token, common.HexToAddress(f.Addresses[0].Address), 100_000_000)
	for range 6 {
		r.a.Mine(1)
		if err := r.w.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	run := r.create(t, batch.LineInput{ContractorID: r.contractors[0], Amount: "40", Reference: "x"})
	if _, err := r.svc.Send(ctx, r.maker, run.ID); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := r.worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var txs int
	_ = r.pool.QueryRow(ctx, `SELECT count(*) FROM outbound_txs WHERE purpose = 'batch_payout'`).Scan(&txs)
	if st := r.state(t, run.ID); st != "sending" || txs != 0 {
		t.Fatalf("before the sweep: run %s, %d transactions; want sending and none sent", st, txs)
	}
	if _, err := sweep.EnqueueDue(ctx, r.pool, anvil); err != nil {
		t.Fatal(err)
	}
	r.drive(t, func() bool { return r.state(t, run.ID) == "paid" })
	if got := r.a.TokenBalance(r.d.Token, r.wallets[0]); got.Int64() != 40_000_000 {
		t.Fatalf("wallet holds %s, want 40", got)
	}
	trialBalanceZero(t, r.pool)
}

// TestBatch_RevertedRunReturnsFunds: the vault is paused after the run is
// approved, so the node refuses the transaction. Nothing is sent, the run
// fails, and the reservation returns to the organisation.
func TestBatch_RevertedRunReturnsFunds(t *testing.T) {
	r := setup(t)
	ctx := context.Background()
	r.fundAndSweep(t, 100)
	run := r.create(t, batch.LineInput{ContractorID: r.contractors[0], Amount: "40", Reference: "x"})
	if run.State != "ready" {
		t.Fatalf("run %s (%v), want ready", run.State, run.Failure)
	}
	// Dev0 is the vault admin; give it the pauser role and pause.
	r.a.Send(testchain.Dev0, r.d.Vault, testchain.Calldata(t, "grantRole(bytes32,address)", "bytes32,address", testchain.Role("PAUSER_ROLE"), testchain.Dev0))
	r.a.Send(testchain.Dev0, r.d.Vault, testchain.Calldata(t, "pause()", ""))
	if _, err := r.svc.Send(ctx, r.maker, run.ID); err != nil {
		t.Fatalf("send: %v", err)
	}
	r.drive(t, func() bool { return r.state(t, run.ID) == "failed" })
	f, _ := r.orgs.Funding(ctx, r.orgID)
	if f.Available.String() != "100000000" {
		t.Fatalf("available %s after the failed run; want all 100 back", f.Available)
	}
	if got := r.a.TokenBalance(r.d.Token, r.wallets[0]); got.Sign() != 0 {
		t.Fatalf("wallet received %s from a failed run", got)
	}
	trialBalanceZero(t, r.pool)
}

// TestBatch_ConcurrentSendPaysOnce: eight makers press send at once. One
// send wins; the rest are refused; one transaction pays.
func TestBatch_ConcurrentSendPaysOnce(t *testing.T) {
	r := setup(t)
	ctx := context.Background()
	r.fundAndSweep(t, 100)
	run := r.create(t, batch.LineInput{ContractorID: r.contractors[0], Amount: "40", Reference: "x"})
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.svc.Send(ctx, r.maker, run.ID); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			} else if !errors.Is(err, batch.ErrWrongState) {
				t.Errorf("send: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("%d sends succeeded, want 1", ok)
	}
	r.drive(t, func() bool { return r.state(t, run.ID) == "paid" })
	var txs int
	_ = r.pool.QueryRow(ctx, `SELECT count(*) FROM outbound_txs WHERE purpose = 'batch_payout'`).Scan(&txs)
	if got := r.a.TokenBalance(r.d.Token, r.wallets[0]); txs != 1 || got.Int64() != 40_000_000 {
		t.Fatalf("%d transactions, wallet holds %s; want one payment of 40", txs, got)
	}
	trialBalanceZero(t, r.pool)
}
