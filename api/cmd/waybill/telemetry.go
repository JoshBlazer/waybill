package main

import (
	"context"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/JoshBlazer/waybill/api/internal/telemetry"
)

// startTelemetry sets up OpenTelemetry for one subcommand (ADR-045). It is
// a no-op unless OTEL_EXPORTER_OTLP_ENDPOINT is set.
func startTelemetry(ctx context.Context, role string) (func(context.Context) error, error) {
	version := os.Getenv("WAYBILL_VERSION")
	if version == "" {
		version = "0.1.0-dev"
	}
	return telemetry.Setup(ctx, role, version)
}

// observeQueues reports what is waiting in the database, read at each
// export: outbox jobs due or retrying, dead jobs needing a person, and
// transactions not yet mined, per network.
func observeQueues(pool *pgxpool.Pool) error {
	m := telemetry.Meter()
	depth, err := m.Int64ObservableGauge("waybill.outbox.depth", metric.WithDescription("Outbox jobs not done and not dead, by topic."))
	if err != nil {
		return err
	}
	dead, err := m.Int64ObservableGauge("waybill.outbox.dead", metric.WithDescription("Dead outbox jobs, by topic. Each needs a person."))
	if err != nil {
		return err
	}
	pending, err := m.Int64ObservableGauge("waybill.txmgr.pending", metric.WithDescription("Outbound transactions not yet mined, by network."))
	if err != nil {
		return err
	}
	_, err = m.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		// Every topic and network ever seen, so an empty queue reports 0
		// rather than no data.
		rows, err := pool.Query(ctx, `SELECT topic,
			count(*) FILTER (WHERE done_at IS NULL AND dead_at IS NULL),
			count(*) FILTER (WHERE dead_at IS NOT NULL)
			FROM outbox GROUP BY topic`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var topic string
			var d, x int64
			if err := rows.Scan(&topic, &d, &x); err != nil {
				rows.Close()
				return err
			}
			o.ObserveInt64(depth, d, metric.WithAttributes(attribute.String("topic", topic)))
			o.ObserveInt64(dead, x, metric.WithAttributes(attribute.String("topic", topic)))
		}
		rows.Close()
		rows, err = pool.Query(ctx, `SELECT network, count(*) FILTER (WHERE state = 'pending') FROM outbound_txs GROUP BY network`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var network string
			var n int64
			if err := rows.Scan(&network, &n); err != nil {
				return err
			}
			o.ObserveInt64(pending, n, metric.WithAttributes(attribute.String("network", network)))
		}
		return rows.Err()
	}, depth, dead, pending)
	return err
}
