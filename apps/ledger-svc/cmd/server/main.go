// Command server is the ledger-svc gRPC entrypoint.
//
// It composes the application graph from a strongly-typed Config (loaded
// via internal/config), runs the gRPC server (LedgerService + standard
// Health) and the in-process Outbox Relay Worker, and orchestrates a
// two-phase graceful shutdown on SIGINT/SIGTERM.
//
// The server speaks gRPC only. A separate BFF will eventually expose
// GraphQL / HTTP to clients; this service stays a private mesh peer.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/config"
	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/infrastructure"
	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/observability"
	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/repository"
	transportgrpc "github.com/phuongdpham/fintech/apps/ledger-svc/internal/transport/grpc"
	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/transport/grpc/interceptors"
	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/usecase"
)

// Version is set by the build via -ldflags; defaults to "dev" for
// uninstrumented `go build` invocations.
var Version = "dev"

// flagOverrides hosts the small set of CLI overrides we still honor.
// Env (via internal/config) is the authoritative source; flags exist
// for one reason — `--addr=:0` in integration tests, where we need
// the kernel to assign a port. Anything beyond that should go through
// env, not creep back into flag plumbing.
type flagOverrides struct {
	Addr string
}

func parseFlags() flagOverrides {
	var f flagOverrides
	flag.StringVar(&f.Addr, "addr", "", "override GRPC_ADDR (test-only — leave unset in deployments)")
	flag.Parse()
	return f
}

func main() {
	overrides := parseFlags()

	// Service-scoped .env layered on top of root .env. In production
	// (APP_ENV=production) both files are skipped and we read pure os.Environ.
	cfg, err := config.Load(config.LoadOptions{
		ServiceEnvFile: "apps/ledger-svc/.env",
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}

	if overrides.Addr != "" {
		cfg.GRPC.Addr = overrides.Addr
	}

	log := newLogger(cfg.LogLevel)
	slog.SetDefault(log)

	// Apply runtime limits before anything allocates significantly.
	// GOMEMLIMIT from cgroup smooths GC behavior under load; GOGC=200
	// trades heap for fewer GC cycles. Both overridable via env.
	observability.ApplyRuntimeLimits(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, log); err != nil {
		log.Error("server exited with error", slog.Any("err", err))
		os.Exit(1)
	}
}

// run wires the dependency graph and blocks until ctx is done. Split out
// from main so we can return errors instead of os.Exiting from deep
// within boot — easier to test (when we add an integration test) and
// produces consistent error handling.
func run(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	// OTel first — propagator is installed even when no exporter is
	// configured, so trace context flows downstream regardless. Spans
	// from later boot operations (pool init, etc.) get captured if
	// caller passes an upstream traceparent.
	otelShutdown, err := observability.Init(ctx, "ledger-svc", Version, observability.Config{
		Endpoint:    cfg.OTel.Endpoint,
		Insecure:    cfg.OTel.Insecure,
		ServiceName: cfg.OTel.ServiceName,
		Version:     cfg.OTel.Version,
		Sampler:     cfg.OTel.Sampler,
		SamplerArg:  cfg.OTel.SamplerArg,
		StdoutDebug: cfg.OTel.StdoutDebug,
	})
	if err != nil {
		return fmt.Errorf("otel init: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := otelShutdown(shutdownCtx); err != nil {
			log.Error("otel shutdown", slog.Any("err", err))
		}
	}()
	log.Info("otel initialized",
		slog.String("endpoint", cfg.OTel.Endpoint),
		slog.Bool("stdout_debug", cfg.OTel.StdoutDebug))

	// Prometheus registry + named metric handles. /metrics HTTP server
	// is started below — needs to be running before pgxpool init so the
	// scraper can see acquire-wait observations from boot onward.
	metrics := observability.NewMetrics()
	acquireMetrics := repository.PromAcquireMetrics{Wait: metrics.PoolAcquireWait}

	metricsServer, metricsShutdown, err := startMetricsServer(cfg.Metrics.Addr, metrics.Handler(), log)
	if err != nil {
		return fmt.Errorf("metrics server: %w", err)
	}
	defer metricsShutdown()
	if metricsServer != "" {
		log.Info("metrics endpoint serving", slog.String("addr", metricsServer))
	}

	// Two pgxpools, one purpose each.
	//
	//   requestPool: short-deadline (500ms acquire) for the gRPC hot
	//   path. When saturated, new requests fail fast with
	//   ResourceExhausted instead of stretching p99 latency.
	//
	//   workerPool: long-deadline (30s acquire) for outbox drain +
	//   reconciler. Background work; willing to wait for a slot.
	//   Splitting prevents the request path from starving workers
	//   under load and vice versa.
	//
	// PG max_connections must accommodate the SUM of both pool ceilings
	// across all replicas plus a small admin reserve. With defaults:
	// 250 (request) + 16 (worker) = 266 per replica.
	requestPoolCfg := repository.PoolConfig{
		DSN:             cfg.DB.URL,
		MaxConns:        cfg.DB.MaxConns,
		MinConns:        cfg.DB.MinConns,
		MaxConnLifetime: cfg.DB.ConnMaxLifetime,
		MaxConnIdleTime: cfg.DB.ConnMaxIdleTime,
		AcquireTimeout:  cfg.DB.AcquireTimeout,
	}
	requestPool, err := repository.NewPool(ctx, requestPoolCfg)
	if err != nil {
		return fmt.Errorf("postgres request pool: %w", err)
	}
	defer requestPool.Close()
	stopReqStats := repository.ExportPoolStats(ctx, requestPool, metrics.PoolAcquired, metrics.PoolIdle)
	defer stopReqStats()

	workerPoolCfg := repository.PoolConfig{
		DSN:             cfg.DB.URL,
		MaxConns:        cfg.DB.WorkerMaxConns,
		MinConns:        2,
		MaxConnLifetime: cfg.DB.ConnMaxLifetime,
		MaxConnIdleTime: cfg.DB.ConnMaxIdleTime,
		AcquireTimeout:  cfg.DB.WorkerAcquireTimeout,
	}
	workerPool, err := repository.NewPool(ctx, workerPoolCfg)
	if err != nil {
		return fmt.Errorf("postgres worker pool: %w", err)
	}
	defer workerPool.Close()

	log.Info("postgres pools ready",
		slog.Int64("request_max", int64(requestPoolCfg.MaxConns)),
		slog.Duration("request_acquire", requestPoolCfg.AcquireTimeout),
		slog.Int64("worker_max", int64(workerPoolCfg.MaxConns)),
		slog.Duration("worker_acquire", workerPoolCfg.AcquireTimeout),
	)
	// Pool stats heartbeat — debug-only so prod stays quiet. At INFO this
	// gets spammy under load; at DEBUG it's the right granularity for
	// diffing request-pool vs worker-pool during a load run. Worker-pool
	// gauges aren't first-class Prometheus metrics yet (request pool is,
	// via PoolAcquired/PoolIdle); promote when the second consumer asks.
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				rs := requestPool.Stat()
				ws := workerPool.Stat()
				log.Debug("pool stats",
					slog.Int64("req_acquired", int64(rs.AcquiredConns())),
					slog.Int64("req_idle", int64(rs.IdleConns())),
					slog.Int64("req_total", int64(rs.TotalConns())),
					slog.Int64("wkr_acquired", int64(ws.AcquiredConns())),
					slog.Int64("wkr_idle", int64(ws.IdleConns())),
					slog.Int64("wkr_total", int64(ws.TotalConns())),
				)
			}
		}
	}()

	// Redis (idempotency fast-path).
	redisOpts, err := redis.ParseURL(cfg.Redis.URL)
	if err != nil {
		return fmt.Errorf("parse REDIS_URL: %w", err)
	}
	redisClient := redis.NewClient(redisOpts)
	defer redisClient.Close()
	if err := redisClient.Ping(ctx).Err(); err != nil {
		// Fail boot if Redis is down; the usecase tolerates Redis faults
		// at runtime via fail-open, but at boot a hard signal is better
		// than silent reduced-mode startup.
		return fmt.Errorf("redis ping: %w", err)
	}
	log.Info("redis ready")

	// Kafka producer (idempotent, acks=all).
	producer, err := infrastructure.NewConfluentProducer(infrastructure.ConfluentProducerConfig{
		Brokers:  cfg.Kafka.BootstrapServers(),
		ClientID: cfg.Kafka.ClientID,
	})
	if err != nil {
		return fmt.Errorf("kafka producer: %w", err)
	}
	// Producer Close blocks for in-flight delivery — defer means it runs
	// AFTER worker.Wait so we don't truncate pending publishes.
	defer producer.Close()
	log.Info("kafka producer ready")

	// Repository layer (Postgres).
	breakerCfg := repository.DefaultBreakerConfig()
	breakerCfg.MetricsOutcomes = metrics.DBTxOutcomes
	breakerCfg.MetricsState = metrics.DBCircuitState
	breaker := repository.NewBreaker(breakerCfg)
	ledgerRepo := repository.NewLedgerRepo(requestPool, requestPoolCfg, acquireMetrics).WithBreaker(breaker)
	outboxRepo := repository.NewOutboxRepo(workerPool, workerPoolCfg, acquireMetrics)
	auditDrainRepo := repository.NewAuditDrainRepo(workerPool, workerPoolCfg, acquireMetrics)

	// Infrastructure adapters.
	idemStore := infrastructure.NewRedisIdempotencyStore(redisClient)

	// Usecase.
	transferUC := usecase.NewTransferUsecase(ledgerRepo, idemStore, log)

	// Outbox worker — runs in-process for simpler ops (one binary, one
	// pod). Operationally fine because FOR UPDATE SKIP LOCKED makes the
	// worker safely concurrent across replicas of this same binary.
	worker := infrastructure.NewOutboxWorker(outboxRepo, producer, infrastructure.OutboxWorkerConfig{
		Topic:          cfg.Outbox.Topic,
		MaxConcurrency: cfg.Outbox.Workers,
	}, log)

	// Worker uses its own ctx so we can stop it independently if we ever
	// need to (e.g. drain the gRPC server first, then stop the worker).
	workerCtx, stopWorker := context.WithCancel(ctx)
	defer stopWorker()
	worker.Run(workerCtx)
	log.Info("outbox worker started", slog.Int("concurrency", cfg.Outbox.Workers))

	// Audit drain worker — single writer that turns audit_pending rows
	// into chained audit_log rows. The request path enqueues into
	// audit_pending atomic with the ledger write; this worker is the
	// only thing that touches audit_log. Single-writer is by design:
	// per-tenant chain integrity needs a single owner of the chain head.
	auditWorker := infrastructure.NewAuditWorker(auditDrainRepo, infrastructure.AuditWorkerConfig{
		BatchSize: cfg.Audit.BatchSize,
	}, log).
		WithLagGauge(metrics.AuditPendingLagSeconds).
		WithDrainCounter(metrics.AuditDrainedTotal)
	auditWorker.Run(workerCtx)
	log.Info("audit worker started", slog.Int("batch_size", cfg.Audit.BatchSize))

	// gRPC transport. ledger-svc is internal-only; the BFF validates the
	// caller's JWT and forwards the resolved actor identity as headers
	// (x-tenant-id, x-actor-subject, x-actor-session). The interceptor
	// just trusts and parses those — service-mesh / network policy is
	// what keeps non-BFF callers out, not anything in this process.
	handler := transportgrpc.NewLedgerHandler(transferUC, ledgerRepo, log)
	serverCfg := transportgrpc.ServerConfig{
		Addr: cfg.GRPC.Addr,
		EdgeIdentity: interceptors.EdgeIdentityConfig{
			PublicMethods: interceptors.DefaultPublicMethods(),
		},
	}
	if cfg.Admission.Enabled {
		max := cfg.Admission.MaxInFlight
		if max <= 0 {
			// Default: 2× the effective pool ceiling. Lets bursts absorb
			// without queueing past the pgxpool wait depth.
			max = int64(requestPoolCfg.MaxConns) * 2
		}
		admCfg := interceptors.AdmissionConfig{
			MaxInFlight:     max,
			MetricsRejected: metrics.AdmissionRejected,
			MetricsInFlight: metrics.AdmissionInFlight,
		}
		serverCfg.Admission = &admCfg
		log.Info("admission cap enabled", slog.Int64("max_inflight", max))
	}
	if cfg.RateLimit.Enabled {
		rlCfg := interceptors.DefaultRateLimitConfig()
		rlCfg.TierByTenant = interceptors.ParseTierMap(cfg.RateLimit.TenantTierMap)
		rlCfg.MetricsRejected = metrics.RateLimitRejected
		serverCfg.RateLimit = &rlCfg
		log.Info("rate limit enabled",
			slog.Int("tier_map_entries", len(rlCfg.TierByTenant)),
		)
	}
	srv, err := transportgrpc.New(serverCfg, handler, log)
	if err != nil {
		return fmt.Errorf("grpc server: %w", err)
	}

	serveErr := make(chan error, 1)
	go func() {
		if err := srv.Serve(); err != nil {
			serveErr <- err
		}
		close(serveErr)
	}()

	// Block until ctx done OR the gRPC server fails on its own.
	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-serveErr:
		if err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("grpc serve: %w", err)
		}
	}

	// Two-phase graceful shutdown.
	//
	//   1. Flip health-check to NOT_SERVING — load balancer drains us
	//      from rotation while we still accept in-flight RPCs.
	//   2. GracefulStop with a deadline — finishes in-flight, refuses
	//      new. Hard-kill if the deadline passes.
	//   3. Stop outbox worker (cancel its ctx, Wait).
	//   4. Deferred resources (producer.Flush+Close, redis.Close,
	//      pool.Close) tear down in the reverse-of-init order.
	srv.SetNotServing()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.GRPC.ShutdownTimeout)
	defer cancel()
	srv.GracefulStop(shutdownCtx)
	log.Info("grpc server stopped")

	stopWorker()
	worker.Wait()
	log.Info("outbox worker stopped")

	auditWorker.Wait()
	log.Info("audit worker stopped")

	return nil
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}

// startMetricsServer starts an HTTP listener that serves Prometheus
// exposition at /metrics. Returns the bound address (helpful when addr
// is `:0` in tests), a shutdown closure, or an error if the listener
// can't bind. Empty addr disables the server (returns no-op shutdown).
func startMetricsServer(addr string, h http.Handler, log *slog.Logger) (string, func(), error) {
	if addr == "" {
		return "", func() {}, nil
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", h)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			log.Error("metrics server exited", slog.Any("err", err))
		}
		close(errCh)
	}()
	shutdown := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
	return addr, shutdown, nil
}

