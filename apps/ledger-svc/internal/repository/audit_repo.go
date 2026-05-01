package repository

import (
	"context"
	"errors"
	"fmt"

	json "github.com/goccy/go-json"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/audit"
	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/domain"
)

const (
	selectLatestAuditHashSQL = `
		SELECT entry_hash
		FROM audit_log
		WHERE tenant_id = $1
		ORDER BY occurred_at DESC, id DESC
		LIMIT 1`

	insertAuditEventSQL = `
		INSERT INTO audit_log (
			tenant_id, occurred_at,
			actor_subject, actor_session,
			request_id, trace_id,
			aggregate_type, aggregate_id, operation,
			before_state, after_state,
			prev_hash, entry_hash
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`
)

// writeAuditEvent appends one audit row to the chain inside the supplied
// pgx.Tx — atomic with whatever ledger write the caller is performing.
//
// The chain is per-tenant: prev_hash comes from the most recent row for
// req.TenantID, or the GenesisHash sentinel if none exists. Genesis lookup
// happens inside the same SERIALIZABLE tx so concurrent first-writes for
// the same tenant serialize correctly (one wins, the other 40001-retries
// and sees the now-existing row).
//
// Empty required fields fail fast — silent anonymization is the failure
// the audit log exists to prevent.
func writeAuditEvent(ctx context.Context, tx pgx.Tx, e domain.AuditEvent) error {
	if e.TenantID == "" || e.ActorSubject == "" || e.RequestID == "" {
		return fmt.Errorf("audit: envelope incomplete (tenant=%q subject=%q req=%q)",
			e.TenantID, e.ActorSubject, e.RequestID)
	}
	if e.AggregateType == "" || e.Operation == "" {
		return fmt.Errorf("audit: aggregate_type and operation are required")
	}

	prevHash, err := latestAuditHash(ctx, tx, e.TenantID)
	if err != nil {
		return fmt.Errorf("audit: load prev_hash: %w", err)
	}
	entry := audit.EntryHash(e, prevHash)

	if _, err := tx.Exec(ctx, insertAuditEventSQL,
		e.TenantID, e.OccurredAt,
		e.ActorSubject, e.ActorSession,
		e.RequestID, e.TraceID,
		e.AggregateType, e.AggregateID, e.Operation,
		e.BeforeState, e.AfterState,
		prevHash, entry[:],
	); err != nil {
		return fmt.Errorf("audit: insert: %w", err)
	}
	return nil
}

// latestAuditHash returns the most recent entry_hash for the tenant, or
// audit.GenesisHash when no row exists yet (first-ever event for that
// tenant). The query runs inside the caller's tx so concurrent first-
// writes serialize correctly under SERIALIZABLE isolation.
func latestAuditHash(ctx context.Context, tx pgx.Tx, tenantID string) ([]byte, error) {
	var prev []byte
	err := tx.QueryRow(ctx, selectLatestAuditHashSQL, tenantID).Scan(&prev)
	if errors.Is(err, pgx.ErrNoRows) {
		// Slice copy keeps GenesisHash untouched if the caller mutates.
		return append([]byte(nil), audit.GenesisHash...), nil
	}
	if err != nil {
		return nil, err
	}
	return prev, nil
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
