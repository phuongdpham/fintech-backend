// Package observability bootstraps the OpenTelemetry SDK for ledger-svc.
//
// Init registers a global TracerProvider + propagator and returns a
// shutdown function the caller MUST defer at process exit so in-flight
// spans are flushed.
//
// Configuration is passed in as a Config value (typically derived from
// internal/config.OTelConfig). This package never reads os.Getenv —
// keeping the env contract owned by internal/config means there's one
// place to grep when a knob's behavior is in question.
//
// The W3C Trace-Context + Baggage propagator is installed unconditionally
// so trace ids flow through gRPC metadata, Kafka headers (when we wire
// it), and HTTP — even when no exporter is configured.
package observability

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Config is the observability subsystem's view of OTel knobs. Mirrors
// internal/config.OTelConfig — duplicated intentionally so this package
// stays a leaf with no dependency on the broader config package (and is
// thus reusable from any future service that wants the same wiring).
type Config struct {
	// Endpoint is the OTLP/HTTP collector URL (e.g. http://localhost:4318).
	// Empty + StdoutDebug=false => install the no-op tracer.
	Endpoint string

	// Insecure forces http (no TLS). http:// endpoints imply this regardless.
	Insecure bool

	// ServiceName / Version land on the resource as service.* attrs.
	// ServiceName falls back to the Init(serviceName, ...) arg if empty.
	// Version falls back to the Init(..., version) arg if empty.
	ServiceName string
	Version     string

	// Sampler ∈ {"always_on", "always_off", "traceidratio",
	// "parentbased_traceidratio"}. Empty = always_on (the right default
	// for a write-path service where every request matters for debug).
	Sampler    string
	SamplerArg float64

	// StdoutDebug duplicates spans to stdout via SimpleSpanProcessor.
	// Useful in dev; never enable in prod (per-span fsync to stdout).
	StdoutDebug bool
}

// ShutdownFunc flushes pending spans and tears down the TracerProvider.
// Safe to call multiple times — internal guards make subsequent calls a
// no-op. Pass a context with a finite deadline (typically 5–15s).
type ShutdownFunc func(ctx context.Context) error

// noopShutdown is returned when there's nothing to tear down (no-op
// provider). Keeps callers' defer expressions uniform.
func noopShutdown(context.Context) error { return nil }

// Init wires up the global TracerProvider + propagator. When no OTLP
// endpoint is configured and StdoutDebug is off, it leaves the SDK's
// default no-op tracer in place — callers' otel.Tracer(...).Start(...)
// calls remain valid but do no work.
//
// Returns the shutdown closure even when there's nothing to flush, so
// `defer shutdown(ctx)` is always safe.
func Init(ctx context.Context, serviceName, version string, cfg Config) (ShutdownFunc, error) {
	// Always install the propagator. Trace context flows even when we
	// don't sample locally — important for downstream services that DO.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	if cfg.Endpoint == "" && !cfg.StdoutDebug {
		// Honest no-op: leave the global TracerProvider as the SDK default
		// (which is also no-op). Callers can still call otel.Tracer(...).
		return noopShutdown, nil
	}

	res, err := buildResource(ctx, serviceName, version, cfg)
	if err != nil {
		return noopShutdown, fmt.Errorf("otel: build resource: %w", err)
	}

	processors := make([]sdktrace.SpanProcessor, 0, 2)

	if cfg.Endpoint != "" {
		exp, err := newOTLPExporter(ctx, cfg)
		if err != nil {
			return noopShutdown, fmt.Errorf("otel: otlp exporter: %w", err)
		}
		processors = append(processors, sdktrace.NewBatchSpanProcessor(exp))
	}
	if cfg.StdoutDebug {
		exp, err := stdouttrace.New(stdouttrace.WithPrettyPrint())
		if err != nil {
			return noopShutdown, fmt.Errorf("otel: stdout exporter: %w", err)
		}
		// SimpleSpanProcessor for stdout: we want each span to print
		// as it ends, not after a batching window.
		processors = append(processors, sdktrace.NewSimpleSpanProcessor(exp))
	}

	tp := sdktrace.NewTracerProvider(
		append([]sdktrace.TracerProviderOption{
			sdktrace.WithResource(res),
			sdktrace.WithSampler(buildSampler(cfg)),
		}, mapProcessors(processors)...)...,
	)
	otel.SetTracerProvider(tp)

	return func(ctx context.Context) error {
		// Bound shutdown so a misbehaving collector can't hang process exit.
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return tp.Shutdown(ctx)
	}, nil
}

func newOTLPExporter(ctx context.Context, cfg Config) (*otlptrace.Exporter, error) {
	opts := []otlptracehttp.Option{
		otlptracehttp.WithEndpointURL(cfg.Endpoint),
	}
	// Plain http:// or explicit Insecure → drop TLS.
	if cfg.Insecure || strings.HasPrefix(cfg.Endpoint, "http://") {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	return otlptracehttp.New(ctx, opts...)
}

func buildResource(ctx context.Context, serviceName, version string, cfg Config) (*resource.Resource, error) {
	if cfg.ServiceName != "" {
		serviceName = cfg.ServiceName
	}
	if cfg.Version != "" {
		version = cfg.Version
	}

	// Merge with the SDK's environmental detector so attrs like
	// host.name, process.pid, OTEL_RESOURCE_ATTRIBUTES are auto-included.
	// WithFromEnv stays — it reads the SDK-standard OTEL_RESOURCE_ATTRIBUTES
	// which is a free-form k=v list, not a knob we model in config.
	base, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithProcessPID(),
		resource.WithHost(),
		resource.WithOS(),
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(version),
		),
	)
	if err != nil && !errors.Is(err, resource.ErrPartialResource) {
		return nil, err
	}
	return base, nil
}

// buildSampler honors cfg.Sampler / cfg.SamplerArg. Defaults to AlwaysOn
// because (a) we're a write-path service where every request matters
// for debug, (b) the no-op fallback above means production runs without
// an exporter incur zero cost regardless of sampler.
func buildSampler(cfg Config) sdktrace.Sampler {
	switch strings.ToLower(cfg.Sampler) {
	case "always_off":
		return sdktrace.NeverSample()
	case "parentbased_traceidratio":
		return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SamplerArg))
	case "traceidratio":
		return sdktrace.TraceIDRatioBased(cfg.SamplerArg)
	default:
		return sdktrace.AlwaysSample()
	}
}

// mapProcessors converts SpanProcessors into provider options. Avoids
// repeating the WithSpanProcessor wrapper at every call site.
func mapProcessors(ps []sdktrace.SpanProcessor) []sdktrace.TracerProviderOption {
	out := make([]sdktrace.TracerProviderOption, 0, len(ps))
	for _, p := range ps {
		out = append(out, sdktrace.WithSpanProcessor(p))
	}
	return out
}
