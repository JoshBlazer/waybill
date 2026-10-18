// Package telemetry sets up OpenTelemetry traces and metrics
// (docs/design/stage5.md §4, ADR-045).
//
// It is off unless OTEL_EXPORTER_OTLP_ENDPOINT is set: then traces and
// metrics are exported over OTLP/HTTP, configured by the standard OTEL_*
// variables. Off, the global providers are OpenTelemetry's no-ops, so
// instrumented code costs next to nothing and needs no checks.
//
// Attributes never carry secrets: an RPC endpoint is named by its host, and
// no request body, key or URL query is recorded.
package telemetry

import (
	"context"
	"errors"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Name is the instrumentation scope for Waybill's own spans and metrics.
const Name = "github.com/JoshBlazer/waybill"

// Enabled reports whether an exporter is configured.
func Enabled() bool { return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" }

// Setup installs the global providers for one process (role is "api",
// "worker", "watcher" …) and returns a function that flushes and stops
// them. With no endpoint configured it does nothing.
func Setup(ctx context.Context, role, version string) (func(context.Context) error, error) {
	if !Enabled() {
		return func(context.Context) error { return nil }, nil
	}
	// Schemaless: the SDK default resource carries its own semconv schema
	// URL, and merging two different ones is an error.
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		semconv.ServiceName("waybill-"+role), semconv.ServiceVersion(version), semconv.ServiceNamespace("waybill")))
	if err != nil {
		return nil, err
	}
	te, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	me, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(te), sdktrace.WithResource(res))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(me, sdkmetric.WithInterval(15*time.Second))))
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	return func(ctx context.Context) error {
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
	}, nil
}

// Tracer is Waybill's tracer.
func Tracer() trace.Tracer { return otel.Tracer(Name) }

// Meter is Waybill's meter.
func Meter() metric.Meter { return otel.Meter(Name) }
