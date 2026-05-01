package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	json "github.com/goccy/go-json"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/audit"
	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/domain"
)

const (
	// Async-audit path: the request tx writes here instead of audit_log.
	// No SELECT-latest-hash, no per-tenant chain conflict — that work
	// moves to AuditWorker, which drains this queue into audit_log.
	insertAuditPendingSQL = `
		INSERT INTO audit_pending (
			tenant_id, occurred_at,
			actor_subject, actor_session,
			request_id, trace_id,
			aggregate_type, aggregate_id, operation,
			before_state, after_state
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`

	// Worker-side: claim a batch of pending rows under FOR UPDATE
	// SKIP LOCKED. Single-writer drain by design (per-tenant ordering
	// requires a single owner of the chain head); SKIP LOCKED still
	// helps if a future replica races on startup — only one wins.
	//
	// ORDER BY tenant_id, id: groups rows by tenant for efficient
	// per-tenant chain compute, and sorts within tenant by uuidv7 id
	// (time-ordered) so the worker writes audit_log in the same order
	// the request path enqueued.
	selectAuditPendingBatchSQL = `
		SELECT id, enqueued_at, occurred_at, tenant_id,
		       actor_subject, actor_session, request_id, trace_id,
		       aggregate_type, aggregate_id, operation,
		       before_state, after_state
		FROM audit_pending
		ORDER BY tenant_id, id
		LIMIT $1
		FOR UPDATE SKIP LOCKED`

	// Per-tenant chain head lookup. Runs ONCE per tenant per batch on
	// the worker — no longer in the request path.
	selectLatestAuditHashSQL = `
		SELECT entry_hash
		FROM audit_log
		WHERE tenant_id = $1
		ORDER BY occurred_at DESC, id DESC
		LIMIT 1`

	// Worker-side: forward the row from audit_pending into audit_log.
	// id is reused (not regenerated) so the audit_pending row and its
	// audit_log destination share an identity — useful for trace and
	// for at-least-once delete semantics on the worker side.
	insertAuditEventSQL = `
		INSERT INTO audit_log (
			id, tenant_id, occurred_at,
			actor_subject, actor_session,
			request_id, trace_id,
			aggregate_type, aggregate_id, operation,
			before_state, after_state,
			prev_hash, entry_hash
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`

	deleteAuditPendingByIDSQL = `
		DELETE FROM audit_pending WHERE tenant_id = $1 AND id = ANY($2)`

	// Lag-alert query, served by idx_audit_pending_enqueued. The
	// reconciler polls this — a NULL result means the queue is drained.
	selectOldestAuditPendingSQL = `
		SELECT MIN(enqueued_at) FROM audit_pending`
)

// writeAuditEvent enqueues one audit event into audit_pending inside the
// caller's tx — atomic with the ledger write. The hash chain is computed
// later by AuditWorker, NOT here. This keeps the SERIALIZABLE block free
// of the per-tenant chain head SELECT that used to dominate p99.
//
// Empty required fields fail fast — silent anonymization is the failure
// mode an audit log must not allow.
func writeAuditEvent(ctx context.Context, tx pgx.Tx, e domain.AuditEvent) error {
	if e.TenantID == "" || e.ActorSubject == "" || e.RequestID == "" {
		return fmt.Errorf("audit: envelope incomplete (tenant=%q subject=%q req=%q)",
			e.TenantID, e.ActorSubject, e.RequestID)
	}
	if e.AggregateType == "" || e.Operation == "" {
		return fmt.Errorf("audit: aggregate_type and operation are required")
	}

	if _, err := tx.Exec(ctx, insertAuditPendingSQL,
		e.TenantID, e.OccurredAt,
		e.ActorSubject, e.ActorSession,
		e.RequestID, e.TraceID,
		e.AggregateType, e.AggregateID, e.Operation,
		e.BeforeState, e.AfterState,
	); err != nil {
		return fmt.Errorf("audit: enqueue pending: %w", err)
	}
	return nil
}

// PendingAuditRow is one claimed row from audit_pending, ready for the
// worker to chain-hash and forward into audit_log. Public so the worker
// (different package) can consume it without crossing repository
// internals.
type PendingAuditRow struct {
	ID            uuid.UUID
	EnqueuedAt    time.Time
	Event         domain.AuditEvent
}

// latestAuditHash returns the most recent entry_hash for the tenant, or
// audit.GenesisHash when no row exists yet. Worker-side helper — the
// request path no longer touches the chain head.
func latestAuditHash(ctx context.Context, q pgxQuerier, tenantID string) ([]byte, error) {
	var prev []byte
	err := q.QueryRow(ctx, selectLatestAuditHashSQL, tenantID).Scan(&prev)
	if errors.Is(err, pgx.ErrNoRows) {
		// Slice copy keeps GenesisHash untouched if the caller mutates.
		return append([]byte(nil), audit.GenesisHash...), nil
	}
	if err != nil {
		return nil, err
	}
	return prev, nil
}

// pgxQuerier abstracts over pgx.Tx and *pgxpool.Conn for the few
// helpers that don't care which they're given (read-only chain head
// lookup). Avoids dragging tx-vs-conn polymorphism into call sites.
type pgxQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// transactionAuditBody is the canonical JSON shape for a transaction in
// before_state/after_state. Field order is locked here (Go's json package
// emits in struct-declaration order), so two implementations of this
// struct produce byte-identical output. Adding a field is safe; reorder
// or rename breaks chain reproducibility for past rows.
type transactionAuditBody struct {
	ID                 uuid.UUID                 `json:"id"`
	TenantID           string                    `json:"tenant_id"`
	IdempotencyKey     string                    `json:"idempotency_key"`
	RequestFingerprint string                    `json:"request_fingerprint"`
	Status             string                    `json:"status"`
	CreatedAt          string                    `json:"created_at"` // RFC3339Nano
	Entries            []journalEntryAuditBody   `json:"entries"`
}

type journalEntryAuditBody struct {
	ID        uuid.UUID `json:"id"`
	AccountID uuid.UUID `json:"account_id"`
	Amount    string    `json:"amount"` // decimal-as-string, NUMERIC(19,4)
	Currency  string    `json:"currency"`
}

func canonicalTransactionAudit(tx *domain.Transaction) ([]byte, error) {
	body := transactionAuditBody{
		ID:                 tx.ID,
		TenantID:           tx.TenantID,
		IdempotencyKey:     tx.IdempotencyKey,
		RequestFingerprint: tx.RequestFingerprint,
		Status:             string(tx.Status),
		CreatedAt:          tx.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
		Entries:            make([]journalEntryAuditBody, len(tx.Entries)),
	}
	for i, e := range tx.Entries {
		body.Entries[i] = journalEntryAuditBody{
			ID:        e.ID,
			AccountID: e.AccountID,
			Amount:    e.Amount.String(),
			Currency:  string(e.Currency),
		}
	}
	out, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("audit: marshal transaction: %w", err)
	}
	return out, nil
}
