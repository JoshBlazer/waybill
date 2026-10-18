package rpcpool_test

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/JoshBlazer/waybill/api/internal/rpcpool"
)

// With a real meter provider installed after start-up (as main does), a
// pool registers and reports its endpoint states. A gauge created at
// package init, from the earlier no-op meter, made this fail with "invalid
// observable: from different implementation" and stopped the API starting.
func TestPool_ReportsEndpointStateMetric(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	c := &clock{t: time.Unix(1_000_000, 0)}
	p := newPool(t, c, &fake{head: 10}, &fake{head: 10})
	p.Check(context.Background())

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "waybill.rpc.endpoint.state" {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[int64])
			if !ok || len(g.DataPoints) < 2 {
				t.Fatalf("data %+v", m.Data)
			}
			for _, dp := range g.DataPoints {
				if dp.Value != 2 {
					t.Fatalf("endpoint state %d, want 2 (up)", dp.Value)
				}
			}
			return
		}
	}
	t.Fatal("waybill.rpc.endpoint.state was not reported")
}

var _ = rpcpool.Up
