package interceptors

import (
	"context"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	gogrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// AdmissionConfig caps the number of concurrent in-flight RPCs the
// server will accept. Saturation past this cap returns
// RESOURCE_EXHAUSTED *immediately* — queueing past the SLO yields
// worse end-user latency than rejecting outright. The cap is a
// proxy for the downstream pool ceiling: sized at 2× DB pool size,
// it lets a small burst absorb without dropping but bounds the
// pgxpool wait queue depth.
//
// MetricsRejected, MetricsInflight: optional Prometheus handles.
// Both are safe to leave nil for tests.
type AdmissionConfig struct {
	MaxInFlight     int64
	MetricsRejected *prometheus.CounterVec
	MetricsInFlight prometheus.Gauge
}

// DefaultAdmission returns a config sized at 2× the supplied pool max.
// Use as: DefaultAdmission(int64(cfg.DB.MaxConns) * 2).
func DefaultAdmission(max int64) AdmissionConfig {
	return AdmissionConfig{MaxInFlight: max}
}

// Admission returns a server-side unary interceptor that admits up to
// MaxInFlight concurrent calls and rejects everything past the cap.
// MUST slot BEFORE RateLimit in the chain — global capacity check
// is cheaper to fail than a per-tenant rate-limit check.
func Admission(cfg AdmissionConfig) gogrpc.UnaryServerInterceptor {
	if cfg.MaxInFlight <= 0 {
		// 0 or negative max disables admission (useful for tests
		// that don't want this layer in the way). The interceptor
		// still installs but is a pass-through.
		return func(ctx context.Context, req any, _ *gogrpc.UnaryServerInfo, handler gogrpc.UnaryHandler) (any, error) {
			return handler(ctx, req)
		}
	}
	var current atomic.Int64

	return func(ctx context.Context, req any, info *gogrpc.UnaryServerInfo, handler gogrpc.UnaryHandler) (any, error) {
		// Pre-flight deadline check: if the caller's remaining budget
		// is already gone, don't even take a slot.
		if dl, ok := ctx.Deadline(); ok && dl.IsZero() {
			return nil, status.Error(codes.DeadlineExceeded, "admission: caller deadline already exceeded")
		}

		// TryAcquire-style: increment, check, decrement on overflow.
		// No queueing past the cap — that's the whole point.
		next := current.Add(1)
		if next > cfg.MaxInFlight {
			current.Add(-1)
			if cfg.MetricsRejected != nil {
				cfg.MetricsRejected.WithLabelValues(info.FullMethod).Inc()
			}
			return nil, status.Errorf(codes.ResourceExhausted,
				"server at capacity (in-flight=%d, max=%d) for %s",
				next-1, cfg.MaxInFlight, info.FullMethod)
		}
		if cfg.MetricsInFlight != nil {
			cfg.MetricsInFlight.Set(float64(next))
		}
		defer func() {
			now := current.Add(-1)
			if cfg.MetricsInFlight != nil {
				cfg.MetricsInFlight.Set(float64(now))
			}
		}()
		return handler(ctx, req)
	}
}
