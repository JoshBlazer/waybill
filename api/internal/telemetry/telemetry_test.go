package telemetry_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"

	"github.com/JoshBlazer/waybill/api/internal/telemetry"
)

// Without an endpoint nothing is installed and nothing is sent.
func TestSetup_OffWithoutEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	before := otel.GetTracerProvider()
	shutdown, err := telemetry.Setup(context.Background(), "api", "test")
	if err != nil {
		t.Fatal(err)
	}
	if otel.GetTracerProvider() != before || telemetry.Enabled() {
		t.Fatal("a tracer provider was installed with no endpoint")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// With an endpoint, spans and metrics go to it over OTLP/HTTP, tagged with
// the service name.
func TestSetup_ExportsOverOTLPHTTP(t *testing.T) {
	var mu sync.Mutex
	got := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got[r.URL.Path]++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")

	ctx := context.Background()
	shutdown, err := telemetry.Setup(ctx, "worker", "test")
	if err != nil {
		t.Fatal(err)
	}
	_, span := telemetry.Tracer().Start(ctx, "test span")
	span.End()
	c, err := telemetry.Meter().Int64Counter("waybill.test.counter")
	if err != nil {
		t.Fatal(err)
	}
	c.Add(ctx, 1)
	if err := shutdown(ctx); err != nil { // flushes both
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got["/v1/traces"] == 0 || got["/v1/metrics"] == 0 {
		t.Fatalf("requests by path: %v; want traces and metrics", got)
	}
}
