package domain

//go:generate go tool mockgen -source=repository.go -destination=mocks/mock_repository.go -package=mocks

import (
	"context"

	"github.com/google/uuid"
)

// Port interfaces consumed by usecases. Implementations live under
// internal/repository (Postgres) and internal/infrastructure (Redis,
// Kafka, etc.). Keeping them here lets usecases depend on abstractions
// while the domain stays I/O-free.

// AccountRepository is the persistence port for Account aggregates.
type AccountRepository interface {
	Create(ctx context.Context, a *Account) error
	GetByID(ctx context.Context, id uuid.UUID) (*Account, error)
}

// TransferRequest is the input the Transfer usecase hands to the repo.
// The repo is expected to: open a SERIALIZABLE tx, insert the Transaction
// row, insert both JournalEntry rows, assert balance, write the outbox
// event, and commit — all atomically. SQLSTATE 40001 retries are the
// caller's concern (see Phase 4).
type TransferRequest struct {
	TenantID           string
	IdempotencyKey     string
	RequestFingerprint string
	FromAccountID      uuid.UUID
	ToAccountID        uuid.UUID
	Amount             Amount
	Currency           Currency
	OutboxPayload      []byte
	// AuditEnvelope is sourced from request context (Claims, RequestID,
	// trace_id) by the usecase. The repo writes the audit row in the
	// same Postgres tx as the ledger write — committed-or-not-at-all.
	// Required: empty TenantID/ActorSubject/RequestID hard-fails the call.
	AuditEnvelope AuditEnvelope
}

// LedgerRepository owns the atomic write path for balanced transactions.
// One method, one responsibility — the engine.
//
// GetTransactionByIdempotencyKey exists for the replay path: when a duplicate
// idempotency key is detected (Redis says COMPLETED, or PG UNIQUE caught a
// late duplicate), the usecase loads the original transaction and returns
// it to the caller verbatim. Tenant scoping is mandatory — the composite
// UNIQUE (tenant_id, idempotency_key) means the same key string can legally
// exist across tenants, and the replay must filter by tenant or risk
// returning another tenant's row. Returns ErrTransactionNotFound if no row.
type LedgerRepository interface {
	ExecuteTransfer(ctx context.Context, req TransferRequest) (*Transaction, error)
	GetTransaction(ctx context.Context, id uuid.UUID) (*Transaction, error)
	GetTransactionByIdempotencyKey(ctx context.Context, tenantID, key string) (*Transaction, error)
}

// OutboxRepository is the persistence port driven by the Outbox Relay.
// SelectPending uses FOR UPDATE SKIP LOCKED in the implementation so
// horizontally-scaled workers don't double-deliver.
type OutboxRepository interface {
	SelectPending(ctx context.Context, limit int) ([]*OutboxEvent, error)
	MarkPublished(ctx context.Context, ids []uuid.UUID) error
}

type IdempotencyState string

const (
	IdempotencyStarted   IdempotencyState = "STARTED"
	IdempotencyCompleted IdempotencyState = "COMPLETED"
	IdempotencyFailed    IdempotencyState = "FAILED"
)

// IdempotencyRecord is the per-key state stored in the fast-path cache.
// Fingerprint is the SHA-256 of the originating request body (hex), set
// at Acquire time and preserved across SetState calls. A subsequent
// Acquire with a non-matching fingerprint is the caller's signal to
// reject the request as ErrRequestFingerprintMismatch.
type IdempotencyRecord struct {
	State       IdempotencyState
	Fingerprint string
}

// IdempotencyStore is the Redis fast-path port. PostgreSQL's composite
// UNIQUE on (tenant_id, idempotency_key) plus the request_fingerprint
// column is the durable source of truth; this exists to short-circuit
// retry-storms before they hit the DB.
type IdempotencyStore interface {
	// Acquire attempts an atomic SETNX with the given fingerprint.
	// Returns (true, zero, nil) when the caller owns the key;
	// (false, existing, nil) on duplicate. Fingerprint comparison is
	// the caller's responsibility — store stays oblivious so it remains
	// reusable for non-fingerprinted callers if any ever appear.
	Acquire(ctx context.Context, key, fingerprint string) (acquired bool, current IdempotencyRecord, err error)
	SetState(ctx context.Context, key, fingerprint string, state IdempotencyState) error
}
