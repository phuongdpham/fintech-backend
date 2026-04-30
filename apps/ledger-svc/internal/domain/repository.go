package domain

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
	TenantID       string
	IdempotencyKey string
	FromAccountID  uuid.UUID
	ToAccountID    uuid.UUID
	Amount         Amount
	Currency       Currency
	OutboxPayload  []byte
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

// IdempotencyStore is the Redis fast-path port. PostgreSQL's UNIQUE
// constraint on transactions.idempotency_key is the source of truth;
// this exists to short-circuit retry-storms before they hit the DB.
type IdempotencyStore interface {
	// Acquire attempts an atomic SETNX. Returns (true, nil) when the caller
	// owns the key (first request); (false, currentState, nil) on duplicate.
	Acquire(ctx context.Context, key string) (acquired bool, current IdempotencyState, err error)
	SetState(ctx context.Context, key string, state IdempotencyState) error
}
