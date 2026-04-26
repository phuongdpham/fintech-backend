package infrastructure

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/domain"
	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/repository"
)

// OutboxStore is the worker's persistence port. Defined here (consumer
// side) so the worker stays decoupled from the concrete repository type.
type OutboxStore interface {
	Drain(ctx context.Context, batchSize int, publish repository.DrainPublisher) (int, error)
}

// OutboxWorkerConfig is the per-worker tuning surface. Defaults match
// the plan's "every 500ms" cadence; under bursty load you'll want to
// shorten the idle interval and grow the batch.
type OutboxWorkerConfig struct {
	Topic           string
	BatchSize       int
	PollInterval    time.Duration // wait between drains when the previous batch was non-empty
	IdleInterval    time.Duration // wait when the previous batch was empty (back off harder)
	BackoffOnError  time.Duration // wait after a drain error
	MaxConcurrency  int           // number of drainer goroutines this worker spawns
}

func defaultWorkerConfig() OutboxWorkerConfig {
	return OutboxWorkerConfig{
		Topic:          "fintech.ledger.transactions",
		BatchSize:      100,
		PollInterval:   100 * time.Millisecond,
		IdleInterval:   500 * time.Millisecond,
		BackoffOnError: 1 * time.Second,
		MaxConcurrency: 4,
	}
}

// OutboxWorker is the Phase-5 transactional-outbox relay. It pulls PENDING
// rows from the outbox, publishes them to Kafka, and marks the successful
// subset as PUBLISHED — atomically per batch.
//
// Concurrency model: Run() spawns MaxConcurrency goroutines, each running
// an independent drain loop. PostgreSQL's FOR UPDATE SKIP LOCKED ensures
// they never compete for the same row.
//
// Lifecycle:
//
//	w := NewOutboxWorker(...)
//	go w.Run(ctx)
//	<-ctx.Done()
//	w.Wait()       // joins all goroutines
type OutboxWorker struct {
	store    OutboxStore
	producer KafkaProducer
	cfg      OutboxWorkerConfig
	log      *slog.Logger
	wg       sync.WaitGroup
}

func NewOutboxWorker(store OutboxStore, producer KafkaProducer, cfg OutboxWorkerConfig, log *slog.Logger) *OutboxWorker {
	def := defaultWorkerConfig()
	if cfg.Topic == "" {
		cfg.Topic = def.Topic
	}
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
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = def.MaxConcurrency
	}
	if log == nil {
		log = slog.Default()
	}
	return &OutboxWorker{store: store, producer: producer, cfg: cfg, log: log}
}

// Run spawns the configured number of drain goroutines and returns. They
// stop on ctx cancellation; call Wait to join them.
func (w *OutboxWorker) Run(ctx context.Context) {
	for i := 0; i < w.cfg.MaxConcurrency; i++ {
		w.wg.Add(1)
		go w.runOne(ctx, i)
	}
}

// Wait blocks until all spawned drain goroutines have exited. Safe to
// call any time after Run.
func (w *OutboxWorker) Wait() { w.wg.Wait() }

func (w *OutboxWorker) runOne(ctx context.Context, id int) {
	defer w.wg.Done()
	log := w.log.With(slog.Int("worker_id", id))
	log.InfoContext(ctx, "outbox worker started")

	for {
		if ctx.Err() != nil {
			log.InfoContext(ctx, "outbox worker stopping", slog.Any("err", ctx.Err()))
			return
		}

		drained, err := w.tick(ctx)
		switch {
		case err != nil:
			log.WarnContext(ctx, "outbox drain failed", slog.Any("err", err))
			if !sleep(ctx, w.cfg.BackoffOnError) {
				return
			}
		case drained == 0:
			if !sleep(ctx, w.cfg.IdleInterval) {
				return
			}
		default:
			log.DebugContext(ctx, "outbox drained", slog.Int("count", drained))
			if !sleep(ctx, w.cfg.PollInterval) {
				return
			}
		}
	}
}

// tick runs one drain. Public-ish (lowercase but documented) so tests can
// invoke a single tick without spinning up the goroutine loop.
func (w *OutboxWorker) tick(ctx context.Context) (int, error) {
	return w.store.Drain(ctx, w.cfg.BatchSize, w.publishBatch)
}

// publishBatch is the DrainPublisher handed into the repo. It runs INSIDE
// the Postgres tx; an error here rolls the entire batch back to PENDING.
func (w *OutboxWorker) publishBatch(ctx context.Context, events []*domain.OutboxEvent) ([]uuid.UUID, error) {
	if len(events) == 0 {
		return nil, nil
	}

	msgs := make([]KafkaMessage, len(events))
	for i, e := range events {
		msgs[i] = KafkaMessage{
			Topic: w.cfg.Topic,
			// Key by aggregate_id so all events for the same transaction
			// land on the same Kafka partition (preserves per-aggregate
			// ordering for downstream consumers).
			Key:   []byte(e.AggregateID.String()),
			Value: e.Payload,
			Headers: []KafkaHeader{
				{Key: "aggregate_type", Value: []byte(e.AggregateType)},
				{Key: "outbox_event_id", Value: []byte(e.ID.String())},
			},
		}
	}

	successIdx, err := w.producer.ProduceBatch(ctx, msgs)
	if err != nil && !errors.Is(err, context.Canceled) {
		// A producer-level fault (queue full, transport down). Roll the
		// whole batch back; reconciler will pick up the stragglers.
		// Note: even on a fault the producer may have partially
		// delivered — successIdx contains those, but we discard them and
		// trade one round of duplicates for simpler operational reasoning.
		return nil, err
	}

	succeededIDs := make([]uuid.UUID, 0, len(successIdx))
	for _, idx := range successIdx {
		if idx >= 0 && idx < len(events) {
			succeededIDs = append(succeededIDs, events[idx].ID)
		}
	}
	return succeededIDs, nil
}

// sleep returns true if the wait completed, false if ctx was cancelled.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
