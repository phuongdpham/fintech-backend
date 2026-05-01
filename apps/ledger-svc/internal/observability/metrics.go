// Package observability also owns the service's Prometheus metrics
// surface. We use prometheus/client_golang directly rather than the OTel
// meter SDK + Prometheus exporter — the OTel meter ceremony adds setup
// code without a benefit at our current scope (we don't ship metrics to
// any non-Prom backend). Switch later if/when an OTLP-only metric
// pipeline becomes the norm.
//
// Conventions:
//   - Metric names follow the `<subsystem>_<thing>_<unit>` pattern,
//     all lowercase, snake_case.
//   - Histograms expose latency in seconds (Prometheus convention),
//     not milliseconds — even when the underlying measurement is in
//     microseconds, convert at recording time.
//   - Labels are bounded — never include unbounded values (request id,
//     trace id) directly. tenant id is the most cardinality-risky we
//     allow; bound it to known tiers via interceptor logic.
package observability

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics is the registry-bound set of metric handles ledger-svc
// exposes. Construct once at boot via NewMetrics, pass into the
// subsystems that record. Subsystems take only the handles they need
// (Pool takes PoolAcquireWait, etc.) — keeps coupling visible.
type Metrics struct {
	// Registry is the per-process registry. Exposed so tests can
	// instantiate isolated registries (NewRegistry()) instead of
	// polluting the global default.
	Registry *prometheus.Registry

	// PoolAcquireWait — histogram of pgxpool.Acquire wait time. Tail
	// here is the canary for pool starvation: p99 climbing past a few
	// ms means add capacity (vertically) or fix a leak.
	PoolAcquireWait *prometheus.HistogramVec

	// PoolAcquired — gauge of currently checked-out connections,
	// scraped from pgxpool.Stat() on each Prometheus scrape.
	PoolAcquired prometheus.Gauge

	// PoolIdle — gauge of idle connections. PoolAcquired+PoolIdle ≤
	// MaxConns; the gap is "connections being created."
	PoolIdle prometheus.Gauge

	// RateLimitRejected — counter of per-tenant rate-limit rejections.
	// Labels bounded: tenant cardinality matches the active tenant set
	// (configurable via TIER_MAP), tier is from a closed set, method
	// is closed by the proto.
	RateLimitRejected *prometheus.CounterVec

	// AdmissionRejected — counter of in-flight-cap rejections. Method
	// label is closed by the proto.
	AdmissionRejected *prometheus.CounterVec
	// AdmissionInFlight — gauge of currently admitted RPCs. Useful
	// for "are we sustained at capacity" dashboards.
	AdmissionInFlight prometheus.Gauge
}

// NewMetrics constructs the metric handles and registers them on a
// fresh Registry. Returns the registry so the caller can wire it to
// the /metrics handler.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	// Process + Go runtime collectors — free observability for
	// "is the GC pause time exploding under load."
	reg.MustRegister(
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewGoCollector(),
	)

	m := &Metrics{
		Registry: reg,
		PoolAcquireWait: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "pgxpool_acquire_wait_seconds",
				Help: "Time spent waiting on pgxpool.Acquire (seconds).",
				// Buckets sized for a 10K TPS write path: most acquires
				// resolve in < 1 ms (warm pool), pool exhaustion drives
				// the tail past 100 ms. 12 buckets, exponential.
				Buckets: prometheus.ExponentialBuckets(0.0001, 2, 12),
			},
			[]string{"outcome"}, // ok | timeout | err
		),
		PoolAcquired: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "pgxpool_acquired_conns",
			Help: "Current number of acquired (in-use) Postgres connections.",
		}),
		PoolIdle: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "pgxpool_idle_conns",
			Help: "Current number of idle Postgres connections in the pool.",
		}),
		RateLimitRejected: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "grpc_ratelimit_rejected_total",
				Help: "Per-tenant rate-limit rejections.",
			},
			[]string{"tenant", "method", "tier"},
		),
		AdmissionRejected: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "grpc_admission_rejected_total",
				Help: "RPCs rejected by the global in-flight admission cap.",
			},
			[]string{"method"},
		),
		AdmissionInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "grpc_admission_inflight",
			Help: "Current number of admitted (in-flight) RPCs.",
		}),
	}
	reg.MustRegister(
		m.PoolAcquireWait, m.PoolAcquired, m.PoolIdle,
		m.RateLimitRejected, m.AdmissionRejected, m.AdmissionInFlight,
	)
	return m
}

// Handler returns an http.Handler serving Prometheus exposition format
// against this Metrics' Registry. Mount at /metrics.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{
		// EnableOpenMetrics makes the response richer when scraped by
		// recent Prometheus servers (exemplars, etc.) without breaking
		// older scrapers.
		EnableOpenMetrics: true,
	})
}
