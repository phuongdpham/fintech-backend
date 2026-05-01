package infrastructure

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// AuditStore is the worker's data port. Defined here (consumer side) so
// the worker stays decoupled from the concrete repository.AuditDrainRepo.
type AuditStore interface {
	Drain(ctx context.Context, batchSize int) (int, error)
	OldestPendingAge(ctx context.Context) (time.Duration, error)
}

// AuditLagGauge is the optional observability port: a Prometheus gauge
// (or any setter) the worker pushes the oldest-pending-age into on each
// idle tick. Reconciler / alert lives on top of this.
type AuditLagGauge interface {
	Set(seconds float64)
}

// AuditDrainCounter records the count of rows successfully forwarded
// from audit_pending to audit_log. Optional; if nil the worker doesn't
// publish a counter.
type AuditDrainCounter interface {
	Add(delta float64)
}

// AuditWorkerConfig tunes the drain loop. Defaults are sized for the
// 10K TPS write path: a single worker doing 10× 1000-row batches/s,
// with sub-100ms steady-state lag.
type AuditWorkerConfig struct {
	BatchSize      int           // rows per drain tx
	PollInterval   time.Duration // wait between drains when last batch was non-empty
	IdleInterval   time.Duration // wait when last batch was empty (back off harder)
	BackoffOnError time.Duration // wait after a drain error
}

func defaultAuditWorkerConfig() AuditWorkerConfig {
	return AuditWorkerConfig{
		BatchSize:      1000,
		PollInterval:   50 * time.Millisecond,
		IdleInterval:   500 * time.Millisecond,
		BackoffOnError: 1 * time.Second,
	}
}

// AuditWorker is the single-writer drain loop that turns audit_pending
// rows into chained audit_log rows. See repository.AuditDrainRepo for
// the rationale behind single-writer (per-tenant chain head ownership).
//
// Lifecycle mirrors OutboxWorker:
//
//	w := NewAuditWorker(...)
//	go w.Run(ctx)
//	<-ctx.Done()
//	w.Wait()
type AuditWorker struct {
	store    AuditStore
	cfg      AuditWorkerConfig
	log      *slog.Logger
	lag      AuditLagGauge
	drained  AuditDrainCounter
	wg       sync.WaitGroup
}

func NewAuditWorker(store AuditStore, cfg AuditWorkerConfig, log *slog.Logger) *AuditWorker {
	def := defaultAuditWorkerConfig()
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = def.BatchSize
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = def.PollInterval
	}
	if cfg.IdleInterval <= 0 {
		cfg.IdleInterval = def.IdleInterval
	}
	if cfg.BackoffOnError <= 0 {
		cfg.BackoffOnError = def.BackoffOnError
	}
	if log == nil {
		log = slog.Default()
	}
	return &AuditWorker{store: store, cfg: cfg, log: log}
}

// WithLagGauge installs a Prometheus gauge the worker updates on each
// idle tick with the age (seconds) of the oldest audit_pending row.
// Returns w for chaining at boot.
func (w *AuditWorker) WithLagGauge(g AuditLagGauge) *AuditWorker {
	w.lag = g
	return w
}

// WithDrainCounter installs a counter incremented by the number of rows
// drained on each successful tick.
func (w *AuditWorker) WithDrainCounter(c AuditDrainCounter) *AuditWorker {
	w.drained = c
	return w
}

// Run launches the single drain goroutine. Returns immediately; call
// Wait to join after ctx cancellation.
func (w *AuditWorker) Run(ctx context.Context) {
	w.wg.Add(1)
	go w.runLoop(ctx)
}

func (w *AuditWorker) Wait() { w.wg.Wait() }

func (w *AuditWorker) runLoop(ctx context.Context) {
	defer w.wg.Done()
	w.log.InfoContext(ctx, "audit worker started",
		slog.Int("batch_size", w.cfg.BatchSize),
		slog.Duration("poll_interval", w.cfg.PollInterval),
	)

	for {
		if ctx.Err() != nil {
			w.log.InfoContext(ctx, "audit worker stopping", slog.Any("err", ctx.Err()))
			return
		}

		drained, err := w.store.Drain(ctx, w.cfg.BatchSize)
		switch {
		case err != nil:
			if errors.Is(err, context.Canceled) {
				return
			}
			w.log.WarnContext(ctx, "audit drain failed", slog.Any("err", err))
			if !sleepCtx(ctx, w.cfg.BackoffOnError) {
				return
			}
		case drained == 0:
			w.publishLag(ctx)
			if !sleepCtx(ctx, w.cfg.IdleInterval) {
				return
			}
		default:
			if w.drained != nil {
				w.drained.Add(float64(drained))
			}
			w.log.DebugContext(ctx, "audit drained", slog.Int("count", drained))
			if !sleepCtx(ctx, w.cfg.PollInterval) {
				return
			}
		}
	}
}

// publishLag refreshes the lag gauge. Best-effort: a probe failure here
// shouldn't take the drain loop down — worst case the gauge goes stale,
// which the alert rule treats as a problem on its own ("no fresh data").
func (w *AuditWorker) publishLag(ctx context.Context) {
	if w.lag == nil {
		return
	}
	age, err := w.store.OldestPendingAge(ctx)
	if err != nil {
		// Don't spam — DEBUG. The reconciler queries the same metric
		// out-of-band; if the drain is sustained, lag isn't the SLO.
		w.log.DebugContext(ctx, "audit oldest-pending probe failed", slog.Any("err", err))
		return
	}
	w.lag.Set(age.Seconds())
}

// sleepCtx returns true if the wait completed, false if ctx was cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
