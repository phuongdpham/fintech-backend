package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/domain"
)

// DrainPublisher is invoked by Drain inside the Postgres transaction with
// the locked batch. It returns the IDs of events whose downstream
// publication succeeded; events whose IDs are NOT in the returned slice
// stay PENDING for the next sweep.
//
// Returning a non-nil error rolls back the entire batch — all rows go
// back to PENDING for retry. Use the (succeededIDs, nil) form for
// partial success.
type DrainPublisher func(ctx context.Context, events []*domain.OutboxEvent) (succeededIDs []uuid.UUID, err error)

// OutboxRepo persists outbox-event lifecycle transitions.
//
// Two read-paths:
//
//	Drain      — hot path; picks any PENDING row (FOR UPDATE SKIP LOCKED).
//	DrainStale — reconciler; only rows older than staleAfter still PENDING.
type OutboxRepo struct {
	pool       *pgxpool.Pool
	acquireCfg PoolConfig
	metrics    AcquireMetrics
}

// NewOutboxRepo wires the repo against a pool. acquireCfg.AcquireTimeout
// caps each Drain's connection-acquire wait; metrics may be nil.
func NewOutboxRepo(pool *pgxpool.Pool, acquireCfg PoolConfig, metrics AcquireMetrics) *OutboxRepo {
	if metrics == nil {
		metrics = nopAcquireMetrics{}
	}
	return &OutboxRepo{pool: pool, acquireCfg: acquireCfg, metrics: metrics}
}

const (
	selectPendingForUpdateSQL = `
		SELECT id, aggregate_type, aggregate_id, event_schema, payload, status, created_at
		FROM outbox_events
		WHERE status = 'PENDING'
		ORDER BY created_at
		LIMIT $1
		FOR UPDATE SKIP LOCKED`

	selectStalePendingForUpdateSQL = `
		SELECT id, aggregate_type, aggregate_id, event_schema, payload, status, created_at
		FROM outbox_events
		WHERE status = 'PENDING' AND created_at < $1
		ORDER BY created_at
		LIMIT $2
		FOR UPDATE SKIP LOCKED`

	markPublishedSQL = `
		UPDATE outbox_events SET status = 'PUBLISHED'
		WHERE id = ANY($1::uuid[])`

	countStalePendingSQL = `
		SELECT COUNT(*) FROM outbox_events
		WHERE status = 'PENDING' AND created_at < $1`
)

// Drain pulls up to batchSize PENDING rows with row-level locks, calls
// publish with them, and marks succeededIDs as PUBLISHED — all in one
// Postgres transaction. Returns the number of rows marked PUBLISHED.
//
// Concurrency: FOR UPDATE SKIP LOCKED makes parallel drain workers safe.
// Each worker (goroutine, process, pod) sees a disjoint row subset; no
// coordination needed.
//
// Tx span trade-off: the Postgres tx is open for the duration of publish
// (which includes Kafka I/O — single-digit ms in steady state, seconds
// under degradation). Connection-pool sizing MUST account for this.
func (r *OutboxRepo) Drain(ctx context.Context, batchSize int, publish DrainPublisher) (int, error) {
	return r.drain(ctx, selectPendingForUpdateSQL, []any{batchSize}, publish)
}

// DrainStale is the reconciler's hook. Only events older than staleAfter
// are eligible — avoids racing the hot-path worker that's about to pick
// up the freshest rows on its next tick.
func (r *OutboxRepo) DrainStale(ctx context.Context, staleAfter time.Duration, batchSize int, publish DrainPublisher) (int, error) {
	cutoff := time.Now().Add(-staleAfter).UTC()
	return r.drain(ctx, selectStalePendingForUpdateSQL, []any{cutoff, batchSize}, publish)
}

// CountStalePending is a cheap observability hook for the reconciler's
// "is the outbox backed up?" alert.
func (r *OutboxRepo) CountStalePending(ctx context.Context, staleAfter time.Duration) (int64, error) {
	cutoff := time.Now().Add(-staleAfter).UTC()
	var n int64
	if err := r.pool.QueryRow(ctx, countStalePendingSQL, cutoff).Scan(&n); err != nil {
		return 0, fmt.Errorf("repository: count stale pending: %w", err)
	}
	return n, nil
}

func (r *OutboxRepo) drain(ctx context.Context, selectSQL string, args []any, publish DrainPublisher) (int, error) {
	if publish == nil {
		return 0, fmt.Errorf("repository: drain publisher is required")
	}

	tx, err := BeginTxWithAcquire(ctx, r.pool, r.acquireCfg, r.metrics,
		pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("repository: begin outbox tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	rows, err := tx.Query(ctx, selectSQL, args...)
	if err != nil {
		return 0, fmt.Errorf("repository: select pending: %w", err)
	}
	events := make([]*domain.OutboxEvent, 0)
	for rows.Next() {
		e := &domain.OutboxEvent{}
		var aggType, statusStr string
		if err := rows.Scan(&e.ID, &aggType, &e.AggregateID, &e.EventSchema, &e.Payload, &statusStr, &e.CreatedAt); err != nil {
			rows.Close()
			return 0, fmt.Errorf("repository: scan pending: %w", err)
		}
		e.AggregateType = domain.AggregateType(aggType)
		e.Status = domain.OutboxStatus(statusStr)
		events = append(events, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("repository: iterate pending: %w", err)
	}

	if len(events) == 0 {
		// Empty batch — commit the (no-op) tx promptly to release the snapshot.
		if err := tx.Commit(ctx); err != nil {
			return 0, fmt.Errorf("repository: commit empty drain: %w", err)
		}
		committed = true
		return 0, nil
	}

	succeeded, err := publish(ctx, events)
	if err != nil {
		// Rollback (deferred) → all rows return to the available pool.
		return 0, fmt.Errorf("repository: drain publisher: %w", err)
	}

	if len(succeeded) == 0 {
		// Producer returned nothing successful (e.g. all msgs hit per-message
		// errors but the producer itself didn't fail). Rollback so the rows
		// are immediately retryable rather than holding their locks.
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			return 0, fmt.Errorf("repository: rollback empty success: %w", err)
		}
		committed = true // suppress deferred rollback
		return 0, nil
	}

	tag, err := tx.Exec(ctx, markPublishedSQL, succeeded)
	if err != nil {
		return 0, fmt.Errorf("repository: mark published: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("repository: commit drain: %w", err)
	}
	committed = true
	return int(tag.RowsAffected()), nil
}

// LedgerSumZeroCheck verifies the global double-entry invariant: across
// all journal entries, SUM(amount) MUST equal zero. A non-zero result
// means at least one transaction is unbalanced — paging-territory.
//
// Returns the actual sum (for the alert payload) and a boolean indicating
// whether the invariant holds. Used by the reconciler.
func (r *OutboxRepo) LedgerSumZeroCheck(ctx context.Context) (string, bool, error) {
	var sum string
	err := r.pool.QueryRow(ctx, `SELECT COALESCE(SUM(amount), 0)::text FROM journal_entries`).Scan(&sum)
	if err != nil {
		return "", false, fmt.Errorf("repository: sum-zero check: %w", err)
	}
	return sum, sum == "0" || sum == "0.0000", nil
}

// UnbalancedTransactionIDs returns up to limit transaction IDs whose
// journal-entry sum is non-zero. Run only when LedgerSumZeroCheck has
// already failed — this is the more expensive drilldown query.
func (r *OutboxRepo) UnbalancedTransactionIDs(ctx context.Context, limit int) ([]uuid.UUID, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT transaction_id
		FROM journal_entries
		GROUP BY transaction_id
		HAVING SUM(amount) != 0
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("repository: unbalanced tx scan: %w", err)
	}
	defer rows.Close()
	out := make([]uuid.UUID, 0)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("repository: scan unbalanced tx id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
