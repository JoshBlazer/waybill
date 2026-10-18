package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshBlazer/waybill/api/internal/auth"
	"github.com/JoshBlazer/waybill/api/internal/batch"
	"github.com/JoshBlazer/waybill/api/internal/config"
	"github.com/JoshBlazer/waybill/api/internal/db"
	"github.com/JoshBlazer/waybill/api/internal/deployments"
	"github.com/JoshBlazer/waybill/api/internal/evm"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/rpcpool"
	"github.com/JoshBlazer/waybill/api/internal/safety"
	"github.com/JoshBlazer/waybill/api/internal/store"
	"github.com/JoshBlazer/waybill/api/internal/watcher"
)

// evmChain is one verified EVM network. Its client fails over between the
// configured endpoints (ADR-045).
type evmChain struct {
	deployment deployments.Deployment
	client     *rpcpool.Pool
}

// rpcHealthInterval is how often the head of every endpoint is read.
const rpcHealthInterval = 15 * time.Second

// setupChains loads every EVM deployment, verifies it against its chain and
// records its token in chain_tokens. Any failure refuses startup: a wrong
// contract address would hand out deposit addresses nobody controls.
func setupChains(ctx context.Context, cfg config.Config, pool *pgxpool.Pool, log *slog.Logger) (map[safety.Network]evmChain, error) {
	deps, err := deployments.Load(cfg.DeploymentsDir, cfg.NetworkIDs())
	if err != nil {
		return nil, err
	}
	out := make(map[safety.Network]evmChain, len(deps))
	for n, d := range deps {
		var eps []rpcpool.Endpoint
		for _, u := range cfg.RPCURLs(n) {
			c, err := ethclient.DialContext(ctx, u)
			if err != nil {
				return nil, fmt.Errorf("%s: dial RPC at %s: %w", n, safety.RedactedHost(u), err)
			}
			eps = append(eps, rpcpool.Endpoint{Name: safety.RedactedHost(u), Client: c})
		}
		client, err := rpcpool.New(string(n), eps, rpcpool.Options{Log: log})
		if err != nil {
			return nil, err
		}
		go client.Run(ctx, rpcHealthInterval)
		vctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err = deployments.Verify(vctx, client, d)
		cancel()
		if err != nil {
			return nil, err
		}
		if err := store.New(pool).UpsertChainToken(ctx, store.UpsertChainTokenParams{
			Network: string(n), ContractAddress: evm.NormalizeAddress(d.Token), AssetCode: string(money.USDC.Code),
		}); err != nil {
			return nil, err
		}
		log.Info("deployment verified", "network", n, "factory", d.Factory.Hex(), "token", d.Token.Hex())
		out[n] = evmChain{deployment: d, client: client}
	}
	return out, nil
}

// watchPolicy is how each network is watched. Anvil has no consensus, so
// "final" is a depth; public test networks use their finalized block.
func watchPolicy(n safety.Network) watcher.Config {
	cfg := watcher.Config{Network: n, Asset: money.USDC}
	switch n {
	case safety.EVMAnvil:
		cfg.Confirmations, cfg.Finality, cfg.PollInterval = 2, 5, time.Second
		cfg.BatchBlocks = 500
	default:
		cfg.Confirmations, cfg.FinalizedTag, cfg.PollInterval = 3, true, 3*time.Second
		// Public RPCs cap eth_getLogs ranges; sepolia.base.org allows 200 blocks.
		cfg.BatchBlocks = 200
	}
	return cfg
}

func runWatcher(ctx context.Context, log *slog.Logger) error {
	cfg, err := guard(ctx, log)
	if err != nil {
		return err
	}
	if err := cfg.RequireDatabase(); err != nil {
		return err
	}
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	chains, err := setupChains(ctx, cfg, pool, log)
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	for n, c := range chains {
		wcfg := watchPolicy(n)
		wcfg.Token = c.deployment.Token
		wcfg.StartBlock = c.deployment.StartBlock
		w := &watcher.Watcher{Pool: pool, Chain: c.client, Cfg: wcfg, Log: log.With("network", n)}
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Info("watching", "network", n, "confirmations", wcfg.Confirmations, "finalizedTag", wcfg.FinalizedTag)
			_ = w.Run(ctx)
		}()
	}
	wg.Wait()
	return nil
}

// Fixed ids of the local demo contractor and the demo organisation.
var (
	devContractorID = uuid.MustParse("00000000-0000-4000-8000-00000000d001")
	devOrgID        = uuid.MustParse("00000000-0000-4000-8000-00000000e001")
	devAdminID      = uuid.MustParse("00000000-0000-4000-8000-00000000e101")
	devApproverID   = uuid.MustParse("00000000-0000-4000-8000-00000000e102")
)

// devContractorWallet is Anvil dev account #9 (#1 and #2 pay in the e2e tests).
const devContractorWallet = "0xa0ee7a142d267c1f36714e4a8f75612f20a79720"

// devOrgThreshold is 500 USDC: runs above it need a second person.
var devOrgThreshold = money.FromInt64(500_000_000)

// runSeedDev installs the local demo contractor and its API key. It refuses
// to run unless every configured chain is local Anvil.
func runSeedDev(ctx context.Context, log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	for _, n := range cfg.NetworkIDs() {
		if n != safety.EVMAnvil {
			return fmt.Errorf("seed-dev only runs against local Anvil; %s is configured", n)
		}
	}
	if err := cfg.RequireDatabase(); err != nil {
		return err
	}
	key, err := auth.ParseKey(strings.TrimSpace(cfg.DevContractorKey))
	if err != nil {
		return fmt.Errorf("WAYBILL_DEV_CONTRACTOR_KEY is missing or malformed (want wb_test_<8>_<52>)")
	}
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	now := time.Now()
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := q.UpsertContractor(ctx, store.UpsertContractorParams{
			ID: devContractorID, LegalName: "Adaeze Okafor", VerifiedAt: &now,
		}); err != nil {
			return err
		}
		if err := q.InsertAPIKey(ctx, store.InsertAPIKeyParams{ContractorID: devContractorID, Prefix: key.Prefix, SecretSha256: key.Hash[:]}); err != nil {
			return err
		}
		// A payout wallet already past its 24-hour wait, so batch runs can
		// pay the demo contractor at once. Anvil dev account #9: its
		// key is public, which is fine for a local test chain only.
		if _, err := q.UpsertContractorWallet(ctx, store.UpsertContractorWalletParams{
			ContractorID: devContractorID, Network: string(safety.EVMAnvil), Address: devContractorWallet,
			Now: now.Add(-25 * time.Hour), PayoutsAllowedFrom: now.Add(-time.Hour),
		}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		return seedDevOrg(ctx, q, cfg.DevAdminKey, cfg.DevApproverKey)
	})
	if err != nil {
		return err
	}
	log.Info("seeded demo contractor", "contractor", devContractorID, "keyPrefix", key.Prefix)
	return nil
}

// seedDevOrg installs the demo organisation with an admin (who also builds
// runs) and an approver, so dual approval can be shown. Skipped when the
// member keys are not configured.
func seedDevOrg(ctx context.Context, q *store.Queries, adminKey, approverKey string) error {
	if adminKey == "" && approverKey == "" {
		return nil
	}
	if _, err := q.UpsertOrg(ctx, store.UpsertOrgParams{ID: devOrgID, Name: "Lagoon Labs", ApprovalThreshold: devOrgThreshold}); err != nil {
		return err
	}
	members := []struct {
		id    uuid.UUID
		name  string
		roles []string
		key   string
		env   string
	}{
		{devAdminID, "Tolu Bakare", []string{"admin", "maker"}, adminKey, "WAYBILL_DEV_ADMIN_KEY"},
		{devApproverID, "Chidi Eze", []string{"approver"}, approverKey, "WAYBILL_DEV_APPROVER_KEY"},
	}
	for _, m := range members {
		k, err := auth.ParseKey(strings.TrimSpace(m.key))
		if err != nil {
			return fmt.Errorf("%s is missing or malformed (want wb_test_<8>_<52>)", m.env)
		}
		if _, err := q.UpsertMember(ctx, store.UpsertMemberParams{ID: m.id, OrgID: devOrgID, Name: m.name, Roles: m.roles}); err != nil {
			return err
		}
		if err := q.InsertMemberAPIKey(ctx, store.InsertMemberAPIKeyParams{MemberID: m.id, Prefix: k.Prefix, SecretSha256: k.Hash[:], Name: "demo"}); err != nil {
			return err
		}
	}
	return nil
}

// batchService builds the batch-run service, reading each network's vault
// limits from its own chain.
func batchService(pool *pgxpool.Pool, chains map[safety.Network]evmChain, log *slog.Logger) *batch.Service {
	svc := &batch.Service{
		Pool: pool, Log: log,
		Deployments: map[safety.Network]deployments.Deployment{},
		Limits:      map[safety.Network]batch.VaultLimits{},
	}
	for n, c := range chains {
		svc.Deployments[n] = c.deployment
		svc.Limits[n] = batch.ChainVault{Client: c.client, Vault: c.deployment.Vault, Token: c.deployment.Token}
	}
	return svc
}

// pools lists the networks' RPC pools, in a stable order, for /v1/health.
func pools(chains map[safety.Network]evmChain) []*rpcpool.Pool {
	out := make([]*rpcpool.Pool, 0, len(chains))
	for _, n := range sortedNetworks(chains) {
		out = append(out, chains[n].client)
	}
	return out
}

func sortedNetworks(chains map[safety.Network]evmChain) []safety.Network {
	out := make([]safety.Network, 0, len(chains))
	for n := range chains {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}
