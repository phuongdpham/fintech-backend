package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

// clearAll wipes every env key the Config struct touches and registers
// cleanup to restore the original values after the test. t.Setenv is
// load-bearing: it captures the pre-test value internally, so calling
// os.Unsetenv afterwards still restores cleanly via t.Cleanup.
func clearAll(t *testing.T) {
	t.Helper()
	keys := []string{
		"APP_ENV", "LOG_LEVEL",
		"GRPC_ADDR", "GRPC_SHUTDOWN_TIMEOUT",
		"DATABASE_URL", "DATABASE_MAX_CONNS", "DATABASE_MIN_CONNS",
		"DATABASE_CONN_MAX_LIFE", "DATABASE_CONN_MAX_IDLE",
		"DATABASE_ACQUIRE_TIMEOUT", "DATABASE_WORKER_ACQUIRE_TIMEOUT",
		"DATABASE_WORKER_MAX_CONNS",
		"REDIS_URL",
		"KAFKA_BROKERS", "KAFKA_CLIENT_ID",
		"OUTBOX_TOPIC", "OUTBOX_WORKERS",
		"AUDIT_BATCH_SIZE",
		"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_INSECURE",
		"OTEL_SERVICE_NAME", "OTEL_SERVICE_VERSION",
		"OTEL_TRACES_SAMPLER", "OTEL_TRACES_SAMPLER_ARG",
		"OBS_TRACE_DEBUG",
	}
	for _, k := range keys {
		t.Setenv(k, "") // register restore-on-cleanup
		_ = os.Unsetenv(k)
	}
}

func TestLoad_Required(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{
			name: "missing DATABASE_URL",
			env: map[string]string{
				"REDIS_URL":     "redis://localhost:6379/0",
				"KAFKA_BROKERS": "localhost:9092",
			},
			wantErr: "DATABASE_URL",
		},
		{
			name: "missing REDIS_URL",
			env: map[string]string{
				"DATABASE_URL":  "postgres://x/y",
				"KAFKA_BROKERS": "localhost:9092",
			},
			wantErr: "REDIS_URL",
		},
		{
			name: "missing KAFKA_BROKERS",
			env: map[string]string{
				"DATABASE_URL": "postgres://x/y",
				"REDIS_URL":    "redis://localhost:6379/0",
			},
			wantErr: "KAFKA_BROKERS",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearAll(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			_, err := Load(LoadOptions{SkipDotenv: true})
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error to mention %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestLoad_Defaults(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://x/y")
	t.Setenv("REDIS_URL", "redis://localhost:6379/0")
	t.Setenv("KAFKA_BROKERS", "localhost:9092")

	cfg, err := Load(LoadOptions{SkipDotenv: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"AppEnv", cfg.AppEnv, "development"},
		{"LogLevel", cfg.LogLevel, "info"},
		{"GRPC.Addr", cfg.GRPC.Addr, ":9090"},
		{"DB.MaxConns", cfg.DB.MaxConns, int32(120)},
		{"DB.MinConns", cfg.DB.MinConns, int32(20)},
		{"DB.AcquireTimeout", cfg.DB.AcquireTimeout, 500 * time.Millisecond},
		{"DB.WorkerAcquireTimeout", cfg.DB.WorkerAcquireTimeout, 30 * time.Second},
		{"DB.WorkerMaxConns", cfg.DB.WorkerMaxConns, int32(16)},
		{"Kafka.ClientID", cfg.Kafka.ClientID, "ledger-svc"},
		{"Kafka.Brokers", cfg.Kafka.BootstrapServers(), "localhost:9092"},
		{"Outbox.Topic", cfg.Outbox.Topic, "fintech.ledger.transactions"},
		{"Outbox.Workers", cfg.Outbox.Workers, 4},
		{"Audit.BatchSize", cfg.Audit.BatchSize, 1000},
		{"OTel.ServiceName", cfg.OTel.ServiceName, "ledger-svc"},
		{"OTel.Sampler", cfg.OTel.Sampler, "always_on"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestLoad_KafkaBrokerList(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://x/y")
	t.Setenv("REDIS_URL", "redis://localhost:6379/0")
	t.Setenv("KAFKA_BROKERS", "broker-1:9092,broker-2:9092,broker-3:9092")

	cfg, err := Load(LoadOptions{SkipDotenv: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := len(cfg.Kafka.Brokers), 3; got != want {
		t.Fatalf("brokers count: got %d, want %d", got, want)
	}
	if got, want := cfg.Kafka.BootstrapServers(), "broker-1:9092,broker-2:9092,broker-3:9092"; got != want {
		t.Errorf("bootstrap servers: got %q, want %q", got, want)
	}
}

func TestLoad_TypeErrors(t *testing.T) {
	// env/v11 reports type errors as: parse error on field "Workers" of
	// type "int". We assert on the field name + type so the test stays
	// stable across env/v11 patch versions while still proving the
	// error is the one we expect.
	cases := []struct {
		name  string
		key   string
		bad   string
		match string
	}{
		{"OUTBOX_WORKERS not int", "OUTBOX_WORKERS", "many", `"Workers"`},
		{"GRPC_SHUTDOWN_TIMEOUT not duration", "GRPC_SHUTDOWN_TIMEOUT", "soon", `"ShutdownTimeout"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearAll(t)
			t.Setenv("DATABASE_URL", "postgres://x/y")
			t.Setenv("REDIS_URL", "redis://localhost:6379/0")
			t.Setenv("KAFKA_BROKERS", "localhost:9092")
			t.Setenv(tc.key, tc.bad)

			_, err := Load(LoadOptions{SkipDotenv: true})
			if err == nil {
				t.Fatalf("expected error mentioning %q, got nil", tc.match)
			}
			if !strings.Contains(err.Error(), tc.match) {
				t.Fatalf("expected error to mention %q, got %v", tc.match, err)
			}
		})
	}
}

func TestValidate_CrossField(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name:    "outbox workers zero",
			mutate:  func(c *Config) { c.Outbox.Workers = 0 },
			wantErr: "OUTBOX_WORKERS",
		},
		{
			name:    "max < min db conns",
			mutate:  func(c *Config) { c.DB.MaxConns = 2; c.DB.MinConns = 8 },
			wantErr: "DATABASE_MAX_CONNS",
		},
		{
			name: "ratio sampler out of range",
			mutate: func(c *Config) {
				c.OTel.Sampler = "traceidratio"
				c.OTel.SamplerArg = 2.5
			},
			wantErr: "OTEL_TRACES_SAMPLER_ARG",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{
				Outbox: OutboxConfig{Workers: 4},
				DB:     DBConfig{MaxConns: 20, MinConns: 4, AcquireTimeout: 500 * time.Millisecond, WorkerAcquireTimeout: 30 * time.Second, WorkerMaxConns: 8},
				Audit:  AuditConfig{BatchSize: 1000},
				OTel:   OTelConfig{Sampler: "always_on", SamplerArg: 0.1},
			}
			tc.mutate(c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("expected error mentioning %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error to mention %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestValidate_OK(t *testing.T) {
	c := &Config{
		Outbox: OutboxConfig{Workers: 4},
		DB:     DBConfig{MaxConns: 20, MinConns: 4, AcquireTimeout: 500 * time.Millisecond, WorkerAcquireTimeout: 30 * time.Second, WorkerMaxConns: 8},
		Audit:  AuditConfig{BatchSize: 1000},
		OTel:   OTelConfig{Sampler: "always_on"},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestValidate_AcquireTimeoutBounds(t *testing.T) {
	cases := []struct {
		name string
		val  time.Duration
		ok   bool
	}{
		{"zero rejected", 0, false},
		{"too low", 1 * time.Millisecond, false},
		{"min boundary", 50 * time.Millisecond, true},
		{"typical", 500 * time.Millisecond, true},
		{"max boundary", 30 * time.Second, true},
		{"too high", 31 * time.Second, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{
				Outbox: OutboxConfig{Workers: 4},
				DB:     DBConfig{MaxConns: 20, MinConns: 4, AcquireTimeout: tc.val, WorkerAcquireTimeout: 30 * time.Second, WorkerMaxConns: 8},
				Audit:  AuditConfig{BatchSize: 1000},
				OTel:   OTelConfig{Sampler: "always_on"},
			}
			err := c.Validate()
			if tc.ok && err != nil {
				t.Fatalf("expected no error for %s, got %v", tc.val, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("expected error for %s, got nil", tc.val)
			}
			if !tc.ok && !strings.Contains(err.Error(), "DATABASE_ACQUIRE_TIMEOUT") {
				t.Fatalf("expected error to mention DATABASE_ACQUIRE_TIMEOUT, got %v", err)
			}
		})
	}
}
