// Command reconciler is the Phase-6 anti-entropy job for the ledger.
//
// Two operating modes:
//
//	(default)   long-running daemon, ticks once per --interval (default 1h)
//	--once      runs both tasks once and exits — for k8s CronJob /
//	            systemd-timer style scheduling, where the scheduler owns cadence
//
// Tasks per tick:
//  1. Stale outbox sweep — re-publish PENDING rows older than --stale-after.
//  2. Ledger invariant check — assert SUM(amount)=0 across journal_entries;
//     log a paging-grade error if it fails.
//
// Config: env-driven via internal/config. The remaining flags are
// reconciler-policy knobs (cadence, batch size, what counts as stale)
// rather than infra coordinates — those belong in env, not flags.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/config"
	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/infrastructure"
	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/repository"
)

func main() {
	var (
		staleAfter = flag.Duration("stale-after", 5*time.Minute, "PENDING rows older than this are eligible for re-publish")
		batchSize  = flag.Int("batch-size", 500, "max rows per stale-sweep tick")
		interval   = flag.Duration("interval", time.Hour, "cron interval (ignored with --once)")
		once       = flag.Bool("once", false, "run a single cycle then exit")
	)
	flag.Parse()

	cfg, err := config.Load(config.LoadOptions{
		ServiceEnvFile: "apps/ledger-svc/.env",
	})
	if err != nil {
		fail("config: " + err.Error())
	}

	log := newLogger(cfg.LogLevel)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := repository.NewPool(ctx, repository.PoolConfig{
		DSN:      cfg.DB.URL,
		MaxConns: 8, // reconciler is low-traffic; small pool is enough
	})
	if err != nil {
		fail("pool init: " + err.Error())
	}
	defer pool.Close()

	producer, err := infrastructure.NewConfluentProducer(infrastructure.ConfluentProducerConfig{
		Brokers:  cfg.Kafka.BootstrapServers(),
		ClientID: "ledger-reconciler",
	})
	if err != nil {
		fail("kafka producer init: " + err.Error())
	}
	defer producer.Close()

	outboxRepo := repository.NewOutboxRepo(pool)
	rec := infrastructure.NewReconciler(
		outboxRepo,
		outboxRepo,
		producer,
		cfg.Outbox.Topic,
		infrastructure.ReconcilerConfig{
			StaleAfter: *staleAfter,
			BatchSize:  *batchSize,
		},
		log,
	)

	if *once {
		runCycle(ctx, log, rec)
		return
	}

	log.InfoContext(ctx, "reconciler started",
		slog.String("interval", interval.String()),
		slog.String("stale_after", staleAfter.String()))

	// Tick once immediately so a freshly-deployed reconciler doesn't sit
	// idle for a full interval while a problem is brewing.
	runCycle(ctx, log, rec)

	t := time.NewTicker(*interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.InfoContext(ctx, "reconciler shutting down")
			return
		case <-t.C:
			runCycle(ctx, log, rec)
		}
	}
}

func runCycle(ctx context.Context, log *slog.Logger, rec *infrastructure.Reconciler) {
	start := time.Now()
	res := rec.RunOnce(ctx)
	log.InfoContext(ctx, "reconciler cycle complete",
		slog.Duration("elapsed", time.Since(start)),
		slog.Int("outbox_drained", res.OutboxDrained),
		slog.Int64("stale_observed", res.StalePendingObserved),
		slog.String("ledger_sum", res.LedgerSum),
		slog.Bool("invariant_ok", res.LedgerInvariantOK),
		slog.Int("unbalanced_count", len(res.UnbalancedTxIDs)),
	)
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "reconciler:", msg)
	os.Exit(1)
}
