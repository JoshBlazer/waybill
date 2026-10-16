package main

import (
	"context"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshBlazer/waybill/api/internal/config"
	"github.com/JoshBlazer/waybill/api/internal/db"
	"github.com/JoshBlazer/waybill/api/internal/deployments"
	"github.com/JoshBlazer/waybill/api/internal/evm"
	"github.com/JoshBlazer/waybill/api/internal/httpapi"
	"github.com/JoshBlazer/waybill/api/internal/invoice"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/outbox"
	"github.com/JoshBlazer/waybill/api/internal/safety"
	"github.com/JoshBlazer/waybill/api/internal/sweep"
	"github.com/JoshBlazer/waybill/api/internal/txmgr"
	"github.com/JoshBlazer/waybill/api/internal/webhook"
)

// maxFeeCap is the most the worker pays per gas on any network. Above it,
// stuck transactions wait for a person instead of overpaying.
var maxFeeCap = big.NewInt(500_000_000_000) // 500 gwei

// runWorker runs outbox jobs (webhook deliveries and sweeps), the
// transaction manager for each network and invoice expiry, until stopped.
// Several workers can run at once: outbox leases keep them on separate jobs,
// the signing-key lease keeps one writer per key, and expiry is safe to
// repeat.
func runWorker(ctx context.Context, log *slog.Logger) error {
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
	tokens := make(map[safety.Network]string, len(chains))
	deps := make(map[safety.Network]deployments.Deployment, len(chains))
	for n, c := range chains {
		tokens[n] = evm.NormalizeAddress(c.deployment.Token)
		deps[n] = c.deployment
	}
	deliverer := &webhook.Deliverer{
		Pool:   pool,
		Client: webhook.NewClient(cfg.WebhookAllowPrivate, webhook.Timeout),
		Log:    log,
		Render: func(ctx context.Context, id uuid.UUID, state string) (any, error) {
			inv, err := invoice.LoadByID(ctx, pool, tokens, id)
			if err != nil {
				return nil, err
			}
			out := httpapi.InvoiceJSON(cfg.WebURL, inv)
			out.State = httpapi.InvoiceState(state) // as it was when the event happened
			return out, nil
		},
	}

	host, _ := os.Hostname()
	owner := fmt.Sprintf("%s:%d", host, os.Getpid())
	w := &outbox.Worker{
		Pool:  pool,
		Owner: owner,
		// A job whose topic has no handler goes dead at once, visibly,
		// rather than waiting forever.
		Handlers:    map[string]outbox.Handler{webhook.Topic: deliverer.Handle},
		Policies:    map[string]outbox.Policy{webhook.Topic: webhook.Policy()},
		Log:         log,
		Lease:       5 * time.Minute,
		Batch:       10,
		MaxAttempts: 10,
		Backoff:     outbox.DefaultBackoff,
	}

	var wg sync.WaitGroup
	run := func(f func()) {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}

	signer, err := loadSigner(cfg)
	switch {
	case err != nil:
		return err
	case signer == nil:
		log.Warn("worker: no signing key configured; sweeps are off")
	default:
		sweeper := &sweep.Sweeper{Pool: pool, Deployments: deps, Asset: money.USDC, Log: log, Managers: map[safety.Network]*txmgr.Manager{}}
		for n, c := range chains {
			policy := watchPolicy(n)
			stuckAfter := time.Minute
			if n == safety.EVMAnvil {
				stuckAfter = 15 * time.Second
			}
			m := &txmgr.Manager{
				Pool: pool, Chain: c.client, Signer: signer, Network: n, ChainID: big.NewInt(c.deployment.ChainID),
				Owner: owner, Log: log.With("network", n), Hooks: map[string]txmgr.Hook{sweep.Purpose: sweeper.Hook(n)},
				LeaseTTL: time.Minute, MaxFeeCap: maxFeeCap, BumpPercent: 10,
				StuckAfter: stuckAfter, Confirmations: policy.Confirmations,
			}
			sweeper.Managers[n] = m
			run(func() { m.Run(ctx, policy.PollInterval) })
			run(func() { enqueueSweeps(ctx, pool, n, log) })
		}
		w.Handlers[sweep.Topic] = sweeper.Handle
		log.Info("worker: signing as", "signer", signer)
	}
	log.Info("worker started", "owner", owner, "topics", len(w.Handlers))

	nr, err := setupNaira(cfg, log)
	if err != nil {
		return err
	}
	if nr.charger != nil {
		co := nr.checkout(pool, tokens, cfg.WebURL, log)
		run(func() { co.Run(ctx, 15*time.Second) })
	}
	run(func() { w.Run(ctx, time.Second) })
	run(func() { invoice.RunExpiry(ctx, pool, 30*time.Second, log) })
	wg.Wait()
	return nil
}

func loadSigner(cfg config.Config) (*txmgr.LocalSigner, error) {
	switch {
	case cfg.SignerKeyFile != "":
		return txmgr.LoadKeyFile(cfg.SignerKeyFile)
	case cfg.SignerKey != "":
		return txmgr.ParseKey(string(cfg.SignerKey))
	}
	return nil, nil
}

// enqueueSweeps adds a sweep job for each newly final payment every 15 s.
func enqueueSweeps(ctx context.Context, pool *pgxpool.Pool, n safety.Network, log *slog.Logger) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		if added, err := sweep.EnqueueDue(ctx, pool, n); err != nil && ctx.Err() == nil {
			log.Warn("sweep: enqueue failed; will retry", "network", n, "err", err)
		} else if added > 0 {
			log.Info("sweep: jobs added", "network", n, "count", added)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
