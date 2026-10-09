package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshBlazer/waybill/api/internal/auth"
	"github.com/JoshBlazer/waybill/api/internal/config"
	"github.com/JoshBlazer/waybill/api/internal/db"
	"github.com/JoshBlazer/waybill/api/internal/deployments"
	"github.com/JoshBlazer/waybill/api/internal/evm"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/safety"
	"github.com/JoshBlazer/waybill/api/internal/store"
	"github.com/JoshBlazer/waybill/api/internal/watcher"
)

// evmChain is one verified EVM network.
type evmChain struct {
	deployment deployments.Deployment
	client     *ethclient.Client
}

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
		client, err := ethclient.DialContext(ctx, cfg.RPCURL(n))
		if err != nil {
			return nil, fmt.Errorf("%s: dial RPC at %s: %w", n, safety.RedactedHost(cfg.RPCURL(n)), err)
		}
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
	cfg := watcher.Config{Network: n, Asset: money.USDC, BatchBlocks: 500}
	switch n {
	case safety.EVMAnvil:
		cfg.Confirmations, cfg.Finality, cfg.PollInterval = 2, 5, time.Second
	default:
		cfg.Confirmations, cfg.FinalizedTag, cfg.PollInterval = 3, true, 3*time.Second
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

// devContractorID is the fixed id of the local demo contractor.
var devContractorID = uuid.MustParse("00000000-0000-4000-8000-00000000d001")

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
		return q.InsertAPIKey(ctx, store.InsertAPIKeyParams{ContractorID: devContractorID, Prefix: key.Prefix, SecretSha256: key.Hash[:]})
	})
	if err != nil {
		return err
	}
	log.Info("seeded demo contractor", "contractor", devContractorID, "keyPrefix", key.Prefix)
	return nil
}
