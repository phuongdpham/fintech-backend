package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/audit"
	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/domain"
)

// AuditDrainRepo is the worker-side audit adapter. Where LedgerRepo
// enqueues audit_pending in the request tx, this drains it: per-tenant
// SHA-256 chain compute, INSERT into audit_log, DELETE from
// audit_pending — one Postgres tx per batch.
//
// Single-writer by design. Per-tenant chain integrity needs a single
// owner of the chain head; running multiple drainers concurrently would
// fork the chain (two writers reading the same prev_hash, both
// inserting). FOR UPDATE SKIP LOCKED on the SELECT is belt-and-braces
// for a future replica race on startup — only one wins, the other goes
// idle.
//
// READ COMMITTED isolation: the drain tx claims rows under FOR UPDATE
// SKIP LOCKED, doesn't read-modify-write any other table. SERIALIZABLE
// would buy nothing and add 40001 retries on the chain-head read.
type AuditDrainRepo struct {
	pool       *pgxpool.Pool
	acquireCfg PoolConfig
	metrics    AcquireMetrics
	now        func() time.Time
}

func NewAuditDrainRepo(pool *pgxpool.Pool, acquireCfg PoolConfig, metrics AcquireMetrics) *AuditDrainRepo {
	if metrics == nil {
		metrics = nopAcquireMetrics{}
	}
	return &AuditDrainRepo{
		pool:       pool,
		acquireCfg: acquireCfg,
		metrics:    metrics,
		now:        func() time.Time { return time.Now().UTC() },
	}
}

// Drain claims up to batchSize rows, computes the per-tenant hash chain,
// inserts into audit_log, and deletes the claimed rows from
// audit_pending — all in one Postgres tx. Returns the count drained;
// zero with nil error means the queue is empty.
func (r *AuditDrainRepo) Drain(ctx context.Context, batchSize int) (int, error) {
	if batchSize <= 0 {
		return 0, fmt.Errorf("audit: drain batch size must be positive (got %d)", batchSize)
	}
	var drained int
	err := r.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		pending, err := claimPendingBatch(ctx, tx, batchSize)
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			return nil
		}
		if err := chainAndForward(ctx, tx, pending); err != nil {
			return err
		}
		drained = len(pending)
		return nil
	})
	return drained, err
}

// OldestPendingAge returns the age of the oldest audit_pending row,
// or zero when the queue is empty. Reconciler polls this to detect
// worker lag — if it crosses the SLO bound, page oncall.
func (r *AuditDrainRepo) OldestPendingAge(ctx context.Context) (time.Duration, error) {
	conn, err := AcquireConn(ctx, r.pool, r.acquireCfg, r.metrics)
	if err != nil {
		return 0, err
	}
	defer conn.Release()

	var oldest *time.Time
	if err := conn.QueryRow(ctx, selectOldestAuditPendingSQL).Scan(&oldest); err != nil {
		return 0, fmt.Errorf("audit: oldest pending: %w", err)
	}
	if oldest == nil {
		return 0, nil
	}
	age := r.now().Sub(*oldest)
	if age < 0 {
		// Clock skew between app and DB — clamp at zero so the metric
		// stays sane. Reconciler doesn't care about negatives.
		return 0, nil
	}
	return age, nil
}

// claimPendingBatch reads + locks the next batch and scans it into
// PendingAuditRow values. Rows come pre-sorted by (tenant_id, id) so
// the caller can group runs per tenant without re-sorting.
func claimPendingBatch(ctx context.Context, tx pgx.Tx, batchSize int) ([]PendingAuditRow, error) {
	rows, err := tx.Query(ctx, selectAuditPendingBatchSQL, batchSize)
	if err != nil {
		return nil, fmt.Errorf("audit: select pending: %w", err)
	}
	defer rows.Close()

	out := make([]PendingAuditRow, 0, batchSize)
	for rows.Next() {
		var p PendingAuditRow
		var e domain.AuditEvent
		if err := rows.Scan(
			&p.ID, &p.EnqueuedAt, &e.OccurredAt, &e.TenantID,
			&e.ActorSubject, &e.ActorSession, &e.RequestID, &e.TraceID,
			&e.AggregateType, &e.AggregateID, &e.Operation,
			&e.BeforeState, &e.AfterState,
		); err != nil {
			return nil, fmt.Errorf("audit: scan pending: %w", err)
		}
		p.Event = e
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("audit: iterate pending: %w", err)
	}
	return out, nil
}

// chainAndForward groups pending rows by tenant (input is already sorted
// by tenant_id, id), loads each tenant's current chain head from
// audit_log, computes per-row entry_hash, INSERTs into audit_log, then
// DELETEs the claimed rows from audit_pending.
func chainAndForward(ctx context.Context, tx pgx.Tx, pending []PendingAuditRow) error {
	// Group by tenant, preserving id order within tenant. Single linear
	// pass — input is already sorted by (tenant_id, id) from the SELECT.
	start := 0
	for start < len(pending) {
		tenantID := pending[start].Event.TenantID
		end := start + 1
		for end < len(pending) && pending[end].Event.TenantID == tenantID {
			end++
		}
		if err := chainTenantRun(ctx, tx, tenantID, pending[start:end]); err != nil {
			return err
		}
		start = end
	}
	return nil
}

// chainTenantRun forwards one tenant's contiguous run of pending rows
// into audit_log, advancing the chain head row by row, then deletes
// those rows from audit_pending.
func chainTenantRun(ctx context.Context, tx pgx.Tx, tenantID string, rows []PendingAuditRow) error {
	prev, err := latestAuditHash(ctx, tx, tenantID)
	if err != nil {
		return fmt.Errorf("audit: load chain head for tenant %q: %w", tenantID, err)
	}

	ids := make([]uuid.UUID, len(rows))
	for i, p := range rows {
		entry := audit.EntryHash(p.Event, prev)
		if _, err := tx.Exec(ctx, insertAuditEventSQL,
			p.ID, p.Event.TenantID, p.Event.OccurredAt,
			p.Event.ActorSubject, p.Event.ActorSession,
			p.Event.RequestID, p.Event.TraceID,
			p.Event.AggregateType, p.Event.AggregateID, p.Event.Operation,
			p.Event.BeforeState, p.Event.AfterState,
			prev, entry[:],
		); err != nil {
			return fmt.Errorf("audit: insert audit_log: %w", err)
		}
		prev = entry[:]
		ids[i] = p.ID
	}
	if _, err := tx.Exec(ctx, deleteAuditPendingByIDSQL, tenantID, ids); err != nil {
		return fmt.Errorf("audit: delete pending: %w", err)
	}
	return nil
}

func (r *AuditDrainRepo) inTx(ctx context.Context, fn func(context.Context, pgx.Tx) error) (returnedErr error) {
	tx, err := BeginTxWithAcquire(ctx, r.pool, r.acquireCfg, r.metrics,
		pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("audit: begin tx: %w", err)
	}
	defer func() {
		if returnedErr != nil {
			// Best-effort rollback; ignore ErrTxClosed (already committed
			// or rolled back).
			_ = tx.Rollback(ctx)
		}
	}()
	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("audit: commit: %w", err)
	}
	return nil
}
