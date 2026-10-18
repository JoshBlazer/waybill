// Command waybill is the single Waybill server binary. Each role runs as its
// own process via a subcommand.
//
//	waybill api          serve the REST API
//	waybill worker       run background jobs from the outbox
//	waybill watcher      follow configured chains
//	waybill migrate      apply database migrations
//	waybill seed-dev     install the local demo contractor (Anvil only)
//	waybill healthcheck  probe the local API (for container health checks)
//	waybill reconcile    check the ledger against the chains and the provider
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/JoshBlazer/waybill/api/internal/config"
	"github.com/JoshBlazer/waybill/api/internal/db"
	"github.com/JoshBlazer/waybill/api/internal/deployments"
	"github.com/JoshBlazer/waybill/api/internal/evm"
	"github.com/JoshBlazer/waybill/api/internal/httpapi"
	"github.com/JoshBlazer/waybill/api/internal/invoice"
	"github.com/JoshBlazer/waybill/api/internal/org"
	"github.com/JoshBlazer/waybill/api/internal/safety"
	"github.com/JoshBlazer/waybill/api/internal/store"
)

const usage = `usage: waybill <api|worker|watcher|migrate|seed-dev|healthcheck|reconcile>`

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := os.Args[1]
	log = log.With("cmd", cmd)
	var err error
	switch cmd {
	case "api":
		err = runAPI(ctx, log)
	case "worker":
		err = runWorker(ctx, log)
	case "watcher":
		err = runWatcher(ctx, log)
	case "seed-dev":
		err = runSeedDev(ctx, log)
	case "migrate":
		err = runMigrate(ctx, log)
	case "healthcheck":
		err = runHealthcheck(ctx)
	case "reconcile":
		err = runReconcile(ctx, log, os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		log.Error("exiting", "err", err)
		os.Exit(1)
	}
}

// guard loads configuration and runs the test-money guard. Every subcommand
// that could touch a chain or a provider calls it before doing anything else.
func guard(ctx context.Context, log *slog.Logger) (config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return cfg, err
	}
	if err := safety.Check(ctx, cfg.Safety(), safety.EVMProber{}); err != nil {
		return cfg, fmt.Errorf("test-money guard refused to start: %w", err)
	}
	log.Info("test-money guard passed", "networks", cfg.Networks())
	return cfg, nil
}

func runAPI(ctx context.Context, log *slog.Logger) error {
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
	deps := make(map[safety.Network]deployments.Deployment, len(chains))
	tokens := make(map[safety.Network]string, len(chains))
	for n, c := range chains {
		deps[n] = c.deployment
		tokens[n] = evm.NormalizeAddress(c.deployment.Token)
	}
	nr, err := setupNaira(cfg, log)
	if err != nil {
		return err
	}
	broker := httpapi.NewBroker(pool, log)
	go broker.Run(ctx)

	srv := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: httpapi.NewHandler(&httpapi.Server{
			DB:       store.New(pool),
			Pool:     pool,
			Invoices: &invoice.Service{Deployments: deps, Now: time.Now, Quoter: nr.quoter},
			Banks:    nr.banks,
			Resolver: nr.resolver,
			Paystack: nr.paystack,
			Checkout: nr.checkout(pool, tokens, cfg.WebURL, log),
			Org:      &org.Service{Pool: pool, Deployments: deps},
			Batches:  batchService(pool, chains, log),
			Tokens:   tokens,
			WebURL:   cfg.WebURL,

			WebhookAllowPrivate: cfg.WebhookAllowPrivate,
			Broker:              broker,
			Version:             cfg.Version,
			Networks:            cfg.Networks(),
			Log:                 log,
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.HTTPAddr)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func runMigrate(ctx context.Context, log *slog.Logger) error {
	cfg, err := config.Load()
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
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}
	v, err := db.Version(ctx, pool)
	if err != nil {
		return err
	}
	log.Info("migrations applied", "version", v)
	return nil
}

// healthcheckURL is fixed: the probe only ever targets the API in its own
// container, so it takes no URL from configuration.
const healthcheckURL = "http://127.0.0.1:8080/v1/health"

func runHealthcheck(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthcheckURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health: HTTP %d", resp.StatusCode)
	}
	return nil
}
