package infrastructure

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/domain"
	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/repository"
)

// ReconcilerOutboxStore is the subset of OutboxRepo the reconciler needs.
// Defined consumer-side so tests don't need a real Postgres pool.
type ReconcilerOutboxStore interface {
	DrainStale(ctx context.Context, staleAfter time.Duration, batchSize int, publish repository.DrainPublisher) (int, error)
	CountStalePending(ctx context.Context, staleAfter time.Duration) (int64, error)
}

// ReconcilerInvariantChecker is the subset of OutboxRepo's invariant
// helpers the reconciler needs. Same package, kept as a separate port
// so the call sites read clearly.
type ReconcilerInvariantChecker interface {
	LedgerSumZeroCheck(ctx context.Context) (sum string, ok bool, err error)
	UnbalancedTransactionIDs(ctx context.Context, limit int) ([]uuid.UUID, error)
}

// ReconcilerConfig tunes the two periodic tasks.
//
// StaleAfter is the threshold past which a still-PENDING outbox row is
// considered "stuck" and worth re-publishing. 5min matches the plan. The
// hot-path worker should drain rows in tens of ms in steady state, so
// 5min is a strong "something went wrong" signal.
type ReconcilerConfig struct {
	StaleAfter     time.Duration
	BatchSize      int
	DrilldownLimit int // how many unbalanced txs to surface in a single alert
}

func defaultReconcilerConfig() ReconcilerConfig {
	return ReconcilerConfig{
		StaleAfter:     5 * time.Minute,
		BatchSize:      500,
		DrilldownLimit: 50,
	}
}

// ReconcilerResult is the per-cycle outcome the cron loop logs and
// (eventually) emits as Prometheus gauges.
type ReconcilerResult struct {
	OutboxDrained        int
	StalePendingObserved int64
	LedgerSum            string
	LedgerInvariantOK    bool
	UnbalancedTxIDs      []uuid.UUID
}

// Reconciler is the Phase-6 anti-entropy job. Runs RunOnce per tick;
// the cmd/reconciler binary loops it.
type Reconciler struct {
	outbox    ReconcilerOutboxStore
	invariant ReconcilerInvariantChecker
	producer  KafkaProducer
	topic     string
	cfg       ReconcilerConfig
	log       *slog.Logger
}

func NewReconciler(
	outbox ReconcilerOutboxStore,
	invariant ReconcilerInvariantChecker,
	producer KafkaProducer,
	topic string,
	cfg ReconcilerConfig,
	log *slog.Logger,
) *Reconciler {
	def := defaultReconcilerConfig()
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = def.StaleAfter
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = def.BatchSize
	}
	if cfg.DrilldownLimit <= 0 {
		cfg.DrilldownLimit = def.DrilldownLimit
	}
	if topic == "" {
		topic = "fintech.ledger.transactions"
	}
	if log == nil {
		log = slog.Default()
	}
	return &Reconciler{
		outbox:    outbox,
		invariant: invariant,
		producer:  producer,
		topic:     topic,
		cfg:       cfg,
		log:       log,
	}
}

// RunOnce executes both tasks and returns a structured result. Task
// errors are logged but do not abort sibling tasks — partial signal beats
// no signal in a reconciler.
func (r *Reconciler) RunOnce(ctx context.Context) ReconcilerResult {
	result := ReconcilerResult{}

	// Task 1: stale outbox sweep.
	staleCount, err := r.outbox.CountStalePending(ctx, r.cfg.StaleAfter)
	if err != nil {
		r.log.ErrorContext(ctx, "reconciler: count stale pending", slog.Any("err", err))
	} else {
		result.StalePendingObserved = staleCount
		if staleCount > 0 {
			r.log.WarnContext(ctx, "reconciler: stale pending events observed",
				slog.Int64("count", staleCount),
				slog.String("stale_after", r.cfg.StaleAfter.String()))
		}
	}

	drained, err := r.outbox.DrainStale(ctx, r.cfg.StaleAfter, r.cfg.BatchSize, r.publishStaleBatch)
	if err != nil {
		r.log.ErrorContext(ctx, "reconciler: stale drain failed", slog.Any("err", err))
	}
	result.OutboxDrained = drained
	if drained > 0 {
		r.log.InfoContext(ctx, "reconciler: republished stale outbox events",
			slog.Int("count", drained))
	}

	// Task 2: ledger invariant check.
	sum, ok, err := r.invariant.LedgerSumZeroCheck(ctx)
	if err != nil {
		r.log.ErrorContext(ctx, "reconciler: invariant check failed", slog.Any("err", err))
		return result
	}
	result.LedgerSum = sum
	result.LedgerInvariantOK = ok

	if !ok {
		// THIS IS THE PAGE. The whole point of double-entry bookkeeping
		// is that this sum is zero. If it isn't, something is broken
		// at a level that user-facing code cannot recover from.
		ids, idsErr := r.invariant.UnbalancedTransactionIDs(ctx, r.cfg.DrilldownLimit)
		if idsErr != nil {
			r.log.ErrorContext(ctx, "reconciler: unbalanced tx drilldown failed", slog.Any("err", idsErr))
		}
		result.UnbalancedTxIDs = ids
		r.log.ErrorContext(ctx, "LEDGER_INVARIANT_VIOLATION sum != 0",
			slog.String("sum", sum),
			slog.Int("unbalanced_count", len(ids)),
			slog.Any("unbalanced_tx_ids", ids))
	}

	return result
}

// publishStaleBatch is the DrainPublisher handed to DrainStale. Same
// shape as the hot-path worker's publish; consumers are expected to be
// idempotent on the outbox event ID (header) since stale events may
// have already been delivered once.
func (r *Reconciler) publishStaleBatch(ctx context.Context, events []*domain.OutboxEvent) ([]uuid.UUID, error) {
	if len(events) == 0 {
		return nil, nil
	}
	msgs := make([]KafkaMessage, len(events))
	for i, e := range events {
		msgs[i] = KafkaMessage{
			Topic: r.topic,
			Key:   []byte(e.AggregateID.String()),
			Value: e.Payload,
			Headers: []KafkaHeader{
				{Key: "aggregate_type", Value: []byte(e.AggregateType)},
				{Key: "outbox_event_id", Value: []byte(e.ID.String())},
				// Distinguishes reconciler-driven re-publishes from the hot
				// path so consumers can route to a slower path if desired.
				{Key: "x-reconciler", Value: []byte("true")},
			},
		}
	}
	successIdx, err := r.producer.ProduceBatch(ctx, msgs)
	if err != nil {
		return nil, fmt.Errorf("reconciler: produce stale batch: %w", err)
	}
	out := make([]uuid.UUID, 0, len(successIdx))
	for _, idx := range successIdx {
		if idx >= 0 && idx < len(events) {
			out = append(out, events[idx].ID)
		}
	}
	return out, nil
}
