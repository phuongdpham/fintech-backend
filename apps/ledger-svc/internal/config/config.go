// Package config is the single source of truth for ledger-svc env-driven
// configuration.
//
// Design rules:
//   - Every binary in this service (cmd/server, cmd/reconciler, cmd/migrate)
//     loads through Load(). Nothing else in the codebase calls os.Getenv —
//     that keeps the env contract grep-able to one struct.
//   - .env loading is dev-only: when APP_ENV != "production" we best-effort
//     source the root .env (shared infra) followed by the service .env
//     (binary-specific knobs). Missing files are not an error. Production
//     images set APP_ENV=production and never touch godotenv.
//   - Required fields fail boot loud and early via env tag `,required`.
//     Cross-field invariants live in Validate() — anything env tags can't
//     express belongs there.
//   - Nested structs group concerns (DB, Redis, Kafka, ...). Subsystems
//     receive their own substruct rather than the whole Config — keeps
//     coupling visible at the call site.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
)

// Config is the service-wide env contract. Substructs intentionally
// group related knobs so subsystems can take what they need without
// importing the whole struct.
type Config struct {
	AppEnv   string `env:"APP_ENV"   envDefault:"development"`
	LogLevel string `env:"LOG_LEVEL" envDefault:"info"`

	GRPC      GRPCConfig
	Metrics   MetricsConfig
	Admission AdmissionConfig
	RateLimit RateLimitConfig
	DB        DBConfig
	Redis     RedisConfig
	Kafka     KafkaConfig
	Outbox    OutboxConfig
	OTel      OTelConfig
}

// GRPCConfig — gRPC server transport knobs.
type GRPCConfig struct {
	Addr            string        `env:"GRPC_ADDR"             envDefault:":9090"`
	ShutdownTimeout time.Duration `env:"GRPC_SHUTDOWN_TIMEOUT" envDefault:"30s"`
}

// MetricsConfig — Prometheus exposition (`/metrics`) listener. Empty Addr
// disables the listener; useful in tests or environments that scrape
// out-of-band.
type MetricsConfig struct {
	Addr string `env:"METRICS_ADDR" envDefault:":9100"`
}

// AdmissionConfig — global in-flight cap. Zero / negative MaxInFlight
// disables the interceptor. Default sized at 2× pgxpool MaxConns
// (computed at boot in main.go).
type AdmissionConfig struct {
	Enabled     bool  `env:"GRPC_ADMISSION_ENABLED"      envDefault:"true"`
	MaxInFlight int64 `env:"GRPC_ADMISSION_MAX_INFLIGHT" envDefault:"0"`
}

// RateLimitConfig — per-tenant token-bucket throttle.
//
// TenantTierMap is the env-format string "tenant-a:premium,tenant-b:standard"
// mapping individual tenant ids to tier labels. The tiers themselves
// (default/premium/internal) are hardcoded in the interceptor; only the
// per-tenant assignment is operator-tunable here.
type RateLimitConfig struct {
	Enabled       bool   `env:"GRPC_RATELIMIT_ENABLED"  envDefault:"true"`
	TenantTierMap string `env:"GRPC_RATELIMIT_TIER_MAP" envDefault:""`
}

// DBConfig — Postgres pool sizing.
//
// Sizing rule of thumb for the OLTP write path:
//
//	MaxConns ≥ TPS × p99_tx_seconds × safety_factor(2)
//
// At 10K TPS with 5ms p99 tx, that's 100 active connections; default 120
// leaves headroom. PG's max_connections must accommodate the sum across
// replicas + outbox workers.
//
// AcquireTimeout is enforced per call by repository.AcquireConn /
// BeginTxWithAcquire. Set strictly less than the typical client deadline:
// failing fast on pool starvation is preferable to consuming the request's
// remaining budget in the wait queue.
type DBConfig struct {
	URL             string        `env:"DATABASE_URL,required"`
	MaxConns        int32         `env:"DATABASE_MAX_CONNS"        envDefault:"120"`
	MinConns        int32         `env:"DATABASE_MIN_CONNS"        envDefault:"20"`
	ConnMaxLifetime time.Duration `env:"DATABASE_CONN_MAX_LIFE"    envDefault:"1h"`
	ConnMaxIdleTime time.Duration `env:"DATABASE_CONN_MAX_IDLE"    envDefault:"30m"`
	AcquireTimeout  time.Duration `env:"DATABASE_ACQUIRE_TIMEOUT"  envDefault:"500ms"`
}

// RedisConfig — idempotency-store coordinates. Required: the SETNX
// fast-path is load-bearing for the at-most-once guarantee.
type RedisConfig struct {
	URL string `env:"REDIS_URL,required"`
}

// KafkaConfig — producer bootstrap. Brokers as a slice so we can pass it
// straight to confluent-kafka-go's bootstrap.servers (comma-joined).
type KafkaConfig struct {
	Brokers  []string `env:"KAFKA_BROKERS,required" envSeparator:","`
	ClientID string   `env:"KAFKA_CLIENT_ID"        envDefault:"ledger-svc"`
}

// BootstrapServers returns the comma-joined broker list expected by
// librdkafka's bootstrap.servers config key.
func (k KafkaConfig) BootstrapServers() string {
	return strings.Join(k.Brokers, ",")
}

// OutboxConfig — worker fleet + topic.
type OutboxConfig struct {
	Topic   string `env:"OUTBOX_TOPIC"   envDefault:"fintech.ledger.transactions"`
	Workers int    `env:"OUTBOX_WORKERS" envDefault:"4"`
}

// OTelConfig — OpenTelemetry knobs. Zero-valued Endpoint installs the
// no-op tracer (zero overhead); see internal/observability.
type OTelConfig struct {
	Endpoint     string  `env:"OTEL_EXPORTER_OTLP_ENDPOINT"`
	Insecure     bool    `env:"OTEL_EXPORTER_OTLP_INSECURE" envDefault:"false"`
	ServiceName  string  `env:"OTEL_SERVICE_NAME"           envDefault:"ledger-svc"`
	Version      string  `env:"OTEL_SERVICE_VERSION"`
	Sampler      string  `env:"OTEL_TRACES_SAMPLER"         envDefault:"always_on"`
	SamplerArg   float64 `env:"OTEL_TRACES_SAMPLER_ARG"     envDefault:"0.1"`
	StdoutDebug  bool    `env:"OBS_TRACE_DEBUG"             envDefault:"false"`
}

// LoadOptions tweaks Load behavior. Zero-valued is the right default for
// production; tests use SkipDotenv to assert against a fully-controlled
// env. ServiceEnvFile lets each binary opt into its own dotenv layer
// (the per-service .env.example).
type LoadOptions struct {
	SkipDotenv     bool   // when true, never call godotenv.Load (test mode)
	ServiceEnvFile string // optional service-scoped .env, e.g. "apps/ledger-svc/.env"
}

// Load reads env vars (optionally backed by .env files in non-prod)
// into a Config and validates cross-field invariants.
//
// Lookup order for .env files when APP_ENV != "production":
//  1. ./.env                — repo root (shared infra coordinates)
//  2. opts.ServiceEnvFile   — per-binary knobs (when set)
//
// godotenv.Load is non-overriding — already-set env wins, so a shell
// export beats the file. That matches twelve-factor expectations.
func Load(opts LoadOptions) (*Config, error) {
	if !opts.SkipDotenv && os.Getenv("APP_ENV") != "production" {
		loadDotenv(opts.ServiceEnvFile)
	}

	var c Config
	if err := env.Parse(&c); err != nil {
		return nil, fmt.Errorf("config: parse env: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &c, nil
}

// Validate enforces cross-field invariants that env tags can't express.
// Add a case here only when the rule spans more than one field; single-
// field invariants belong on the field's tag.
func (c *Config) Validate() error {
	var errs []error

	if c.Outbox.Workers < 1 {
		errs = append(errs, fmt.Errorf("OUTBOX_WORKERS must be >= 1 (got %d)", c.Outbox.Workers))
	}
	if c.DB.MaxConns < c.DB.MinConns {
		errs = append(errs, fmt.Errorf(
			"DATABASE_MAX_CONNS (%d) must be >= DATABASE_MIN_CONNS (%d)",
			c.DB.MaxConns, c.DB.MinConns))
	}
	// AcquireTimeout sanity bounds. Below ~50ms is almost always a typo
	// (request-path tx commits routinely take that long under load); above
	// ~30s defeats the point of the timeout (the request's own deadline
	// would fire first). Either signals a misconfigured deployment.
	const (
		minAcquireTimeout = 50 * time.Millisecond
		maxAcquireTimeout = 30 * time.Second
	)
	if c.DB.AcquireTimeout <= 0 ||
		c.DB.AcquireTimeout < minAcquireTimeout ||
		c.DB.AcquireTimeout > maxAcquireTimeout {
		errs = append(errs, fmt.Errorf(
			"DATABASE_ACQUIRE_TIMEOUT must be in [%s, %s] (got %s)",
			minAcquireTimeout, maxAcquireTimeout, c.DB.AcquireTimeout))
	}
	if c.OTel.Sampler == "traceidratio" || c.OTel.Sampler == "parentbased_traceidratio" {
		if c.OTel.SamplerArg < 0 || c.OTel.SamplerArg > 1 {
			errs = append(errs, fmt.Errorf(
				"OTEL_TRACES_SAMPLER_ARG must be in [0,1] for ratio samplers (got %v)",
				c.OTel.SamplerArg))
		}
	}

	return errors.Join(errs...)
}

// LoadDotenv best-effort sources root .env then the service-specific
// file. Both are optional; missing files are silently ignored (the
// production case). godotenv is non-overriding: shell exports beat the
// file, which matches twelve-factor expectations.
//
// Exported so tools that don't need the full Config (e.g. cmd/migrate)
// can still benefit from the same .env layering. Skipped automatically
// when APP_ENV=production.
func LoadDotenv(serviceFile string) {
	if os.Getenv("APP_ENV") == "production" {
		return
	}
	for _, p := range candidateDotenvPaths(serviceFile) {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		_ = godotenv.Load(p)
	}
}

// loadDotenv is the unguarded internal variant — Load already gates on
// APP_ENV / SkipDotenv before calling, so this stays lean.
func loadDotenv(serviceFile string) {
	for _, p := range candidateDotenvPaths(serviceFile) {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		_ = godotenv.Load(p)
	}
}

// candidateDotenvPaths returns the dotenv files Load tries, in order.
// Root .env is searched relative to the working directory (repo root in
// the common Makefile-driven case).
func candidateDotenvPaths(serviceFile string) []string {
	out := []string{".env"}
	if serviceFile != "" {
		// Allow callers to pass either an absolute path or one relative
		// to the working dir; filepath.Clean normalizes both.
		out = append(out, filepath.Clean(serviceFile))
	}
	return out
}
