package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/JoshBlazer/waybill/api/internal/db"
	"github.com/JoshBlazer/waybill/api/internal/evm"
	"github.com/JoshBlazer/waybill/api/internal/httpapi"
	"github.com/JoshBlazer/waybill/api/internal/invoice"
	"github.com/JoshBlazer/waybill/api/internal/outbox"
	"github.com/JoshBlazer/waybill/api/internal/safety"
	"github.com/JoshBlazer/waybill/api/internal/webhook"
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
	chains, err := setupChains(ctx, cfg, pool, log)
	if err != nil {
		return err
	}
	tokens := make(map[safety.Network]string, len(chains))
	for n, c := range chains {
		tokens[n] = evm.NormalizeAddress(c.deployment.Token)
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
	w := &outbox.Worker{
		Pool:  pool,
		Owner: fmt.Sprintf("%s:%d", host, os.Getpid()),
		// A job whose topic has no handler goes dead at once, visibly,
		// rather than waiting forever. Sweeps and payouts arrive later.
		Handlers:    map[string]outbox.Handler{webhook.Topic: deliverer.Handle},
		Policies:    map[string]outbox.Policy{webhook.Topic: webhook.Policy()},
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
