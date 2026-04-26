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

	// Postgres pool. MaxConns sized to host transactional load + the
	// outbox worker fleet — each worker holds one conn for the duration
	// of a drain (SELECT FOR UPDATE SKIP LOCKED + Kafka produce + UPDATE).
	// The +OutboxWorkers headroom reflects the fact that workers are
	// in-process; in a separate-deployment topology this would not apply.
	pool, err := repository.NewPool(ctx, repository.PoolConfig{
		DSN:             cfg.DB.URL,
		MaxConns:        cfg.DB.MaxConns + int32(cfg.Outbox.Workers),
		MinConns:        cfg.DB.MinConns,
		MaxConnLifetime: cfg.DB.ConnMaxLifetime,
		MaxConnIdleTime: cfg.DB.ConnMaxIdleTime,
	})
	if err != nil {
		return fmt.Errorf("postgres pool: %w", err)
	}
	defer pool.Close()
	log.Info("postgres pool ready")

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
	ledgerRepo := repository.NewLedgerRepo(pool)
	outboxRepo := repository.NewOutboxRepo(pool)

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

	// gRPC transport.
	handler := transportgrpc.NewLedgerHandler(transferUC, ledgerRepo, log)
	authCfg := buildAuthConfig(cfg.Auth, log)
	srv, err := transportgrpc.New(
		transportgrpc.ServerConfig{Addr: cfg.GRPC.Addr, Auth: authCfg},
		handler,
		log,
	)
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

	return nil
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}

// buildAuthConfig translates the typed AuthConfig into the interceptor's
// AuthConfig. Policy:
//
//	Required=true  + DevToken=<x>  → Required + DevTokenVerifier
//	                                 (CI / dev only — token is a plain
//	                                  string, no signature).
//	Required=true  + no token      → Required + RejectAllVerifier
//	                                 (deliberate fail-closed: every RPC
//	                                  returns Unauthenticated until a
//	                                  real verifier is wired). Note:
//	                                  config.Validate() actually rejects
//	                                  this combo at boot — RejectAll is
//	                                  defense-in-depth in case Validate
//	                                  ever loosens.
//	Required=false (default)       → best-effort; if a token is present
//	                                 and a verifier is wired, claims
//	                                 populated; otherwise request proceeds
//	                                 with nil claims.
//
// Production wiring will swap DevTokenVerifier for a JWKS-backed JWT
// verifier or an OAuth2 introspection client — a one-line change.
func buildAuthConfig(auth config.AuthConfig, log *slog.Logger) interceptors.AuthConfig {
	out := interceptors.AuthConfig{
		Required:      auth.Required,
		PublicMethods: interceptors.DefaultPublicMethods(),
	}
	switch {
	case auth.DevToken != "":
		log.Warn("using DevTokenVerifier — DO NOT enable in production",
			slog.Bool("auth_required", auth.Required))
		out.Verifier = &interceptors.DevTokenVerifier{
			Token: auth.DevToken,
			Claims: interceptors.Claims{
				Subject: "dev",
				Tenant:  "default",
				Scopes:  []string{"ledger.transfer", "ledger.read"},
			},
		}
	case auth.Required:
		log.Warn("AUTH_REQUIRED=true but no verifier wired — RejectAllVerifier installed (every RPC will Unauthenticated)")
		out.Verifier = interceptors.RejectAllVerifier()
	}
	return out
}
