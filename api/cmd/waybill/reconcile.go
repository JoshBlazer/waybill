package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JoshBlazer/waybill/api/internal/btc"
	"github.com/JoshBlazer/waybill/api/internal/db"
	"github.com/JoshBlazer/waybill/api/internal/reconcile"
	"github.com/JoshBlazer/waybill/api/internal/safety"
)

// reconcileInterval is how often the worker reconciles.
const reconcileInterval = 15 * time.Minute

func newReconciler(pool *pgxpool.Pool, chains map[safety.Network]evmChain, bnets map[safety.Network]btcNetwork, nr naira, log *slog.Logger) *reconcile.Reconciler {
	rc := &reconcile.Reconciler{Pool: pool, Networks: map[safety.Network]reconcile.Network{}, BTC: map[safety.Network]btc.Chain{}, Log: log}
	for n, b := range bnets {
		rc.BTC[n] = b.chain
	}
	for n, c := range chains {
		rc.Networks[n] = reconcile.Network{Chain: c.client, Deployment: c.deployment}
	}
	if nr.paystack != nil {
		rc.Provider = nr.paystack
	}
	return rc
}

// runReconcile is `waybill reconcile`: one run, printed; exit 1 on any
// difference (docs/design/reconciliation.md §3).
func runReconcile(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	settle := fs.Bool("settle", false, "if a balance is unsettled, wait and run once more to decide it")
	wait := fs.Duration("wait", time.Minute, "how long --settle waits: at least one finality period")
	full := fs.Bool("full", false, "check every record, not only changes since the last clean run")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
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
	nr, err := setupNaira(cfg, log)
	if err != nil {
		return err
	}
	bnets, err := btcNetworks(cfg)
	if err != nil {
		return err
	}
	rc := newReconciler(pool, chains, bnets, nr, log)
	opts := reconcile.Options{Full: *full}
	var rep reconcile.Report
	if *settle {
		rep, err = rc.RunSettled(ctx, opts, func(ctx context.Context) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(*wait):
				return nil
			}
		})
	} else {
		rep, err = rc.Run(ctx, opts)
	}
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return err
		}
	} else if err := rep.WriteText(os.Stdout); err != nil {
		return err
	}
	if rep.Differences > 0 {
		return fmt.Errorf("reconciliation found %d differences (report %s)", rep.Differences, rep.ID)
	}
	return nil
}
