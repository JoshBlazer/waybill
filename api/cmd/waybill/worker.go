package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/JoshBlazer/waybill/api/internal/db"
	"github.com/JoshBlazer/waybill/api/internal/invoice"
	"github.com/JoshBlazer/waybill/api/internal/outbox"
)

// runWorker runs outbox jobs and invoice expiry until stopped. Several
// workers can run at once: outbox leases keep them on separate jobs, and
// expiry is safe to repeat.
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

	host, _ := os.Hostname()
	w := &outbox.Worker{
		Pool:  pool,
		Owner: fmt.Sprintf("%s:%d", host, os.Getpid()),
		// Handlers are registered as background work arrives: sweeps,
		// payouts and webhook deliveries. A job whose topic has no handler
		// goes dead at once, visibly, rather than waiting forever.
		Handlers:    map[string]outbox.Handler{},
		Log:         log,
		Lease:       5 * time.Minute,
		Batch:       10,
		MaxAttempts: 10,
		Backoff:     outbox.DefaultBackoff,
	}
	log.Info("worker started", "owner", w.Owner, "topics", len(w.Handlers))

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		w.Run(ctx, time.Second)
	}()
	go func() {
		defer wg.Done()
		invoice.RunExpiry(ctx, pool, 30*time.Second, log)
	}()
	wg.Wait()
	return nil
}
