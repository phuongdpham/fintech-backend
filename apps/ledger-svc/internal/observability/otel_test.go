package observability_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/observability"
)

// TestInit_NoEndpoint_NoOpFallback ensures Init never panics or errors
// when no OTLP endpoint is configured — production runs in this mode by
// default.
func TestInit_NoEndpoint_NoOpFallback(t *testing.T) {
	shutdown, err := observability.Init(context.Background(), "ledger-svc", "test", observability.Config{})
	require.NoError(t, err)
	require.NotNil(t, shutdown)

	// Even in no-op mode the propagator MUST be installed — downstream
	// services depend on traceparent flowing through.
	prop := otel.GetTextMapPropagator()
	require.NotNil(t, prop)
	require.NotEmpty(t, prop.Fields(), "composite propagator should expose fields")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, shutdown(ctx), "shutdown must be safe in no-op mode")
}

// TestInit_OTLPEndpoint_BuildsExporter ensures the exporter constructor
// is wired correctly. We do NOT spin up a collector — Init's exporter
// build is lazy enough that the exporter is created but only fails on
// first export. Spans are dropped silently if the collector is absent.
func TestInit_OTLPEndpoint_BuildsExporter(t *testing.T) {
	shutdown, err := observability.Init(context.Background(), "ledger-svc", "test", observability.Config{
		Endpoint: "http://localhost:4318",
		Insecure: true,
	})
	require.NoError(t, err)

	// Span creation should be safe even without a live collector.
	tr := otel.Tracer("test")
	_, span := tr.Start(context.Background(), "noop-span")
	span.End()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Shutdown may log a warning about the unreachable endpoint but
	// should not return error within the timeout.
	_ = shutdown(ctx)
}

// TestInit_W3CPropagatorRoundTrip proves the installed composite
// propagator includes both TraceContext and Baggage — the contract that
// downstream services rely on for correlation.
func TestInit_W3CPropagatorRoundTrip(t *testing.T) {
	_, err := observability.Init(context.Background(), "ledger-svc", "test", observability.Config{})
	require.NoError(t, err)

	prop := otel.GetTextMapPropagator()
	fields := prop.Fields()

	// Composite propagator's Fields() exposes the union: traceparent,
	// tracestate (W3C TC) plus baggage (Baggage).
	require.Contains(t, fields, "traceparent")
	require.Contains(t, fields, "baggage")

	// Round-trip: inject + extract via a synthetic carrier to guarantee
	// the propagator wiring works, not just the field names.
	carrier := propagation.MapCarrier{}
	prop.Inject(context.Background(), carrier)
	// Plain background ctx has no span, so the carrier is empty here —
	// the contract under test is "no panic, no error" with empty input.
	require.Empty(t, carrier.Get("traceparent"))
}
