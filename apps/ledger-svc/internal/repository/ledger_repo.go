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

// LedgerRepo is the Postgres implementation of domain.LedgerRepository.
// All writes go through InTx (Serializable + 40001 retry).
type LedgerRepo struct {
	pool       *pgxpool.Pool
	acquireCfg PoolConfig // honored by BeginTxWithAcquire (timeout + metric)
	metrics    AcquireMetrics
	retry      RetryConfig
	breaker    *Breaker // optional; nil disables circuit-break
	now        func() time.Time
	newID      func() uuid.UUID
}

// WithBreaker returns a copy of r with the circuit breaker installed.
// Call once at boot. Production wires this; tests typically pass nil.
func (r *LedgerRepo) WithBreaker(b *Breaker) *LedgerRepo {
	cp := *r
	cp.breaker = b
	return &cp
}

// NewLedgerRepo wires the repo against a pool. acquireCfg.AcquireTimeout
// caps each tx's connection-acquire wait; metrics may be nil for tests
// (replaced with a no-op).
func NewLedgerRepo(pool *pgxpool.Pool, acquireCfg PoolConfig, metrics AcquireMetrics) *LedgerRepo {
	if metrics == nil {
		metrics = nopAcquireMetrics{}
	}
	return &LedgerRepo{
		pool:       pool,
		acquireCfg: acquireCfg,
		metrics:    metrics,
		retry:      DefaultRetryConfig(),
		now:        func() time.Time { return time.Now().UTC() },
		// UUIDv7: time-ordered prefix → INSERTs append to the right edge
		// of the B-tree on transactions(id), journal_entries(id), and
		// outbox_events(id), avoiding the leaf-thrash v4 causes under load.
		// uuid.NewV7 only errors on crypto/rand failure (catastrophic);
		// Must is the right ergonomics here.
		newID: func() uuid.UUID { return uuid.Must(uuid.NewV7()) },
	}
}

// WithRetryConfig returns a copy of r with retry overridden. Used by tests
// and tuning code paths to inject deterministic backoff.
func (r *LedgerRepo) WithRetryConfig(cfg RetryConfig) *LedgerRepo {
	cp := *r
	cp.retry = cfg
	return &cp
}

const (
	insertTransactionSQL = `
		INSERT INTO transactions (id, tenant_id, idempotency_key, request_fingerprint, status, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)`

	insertJournalEntrySQL = `
		INSERT INTO journal_entries (id, transaction_id, tenant_id, account_id, amount, currency, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`

	insertOutboxEventSQL = `
		INSERT INTO outbox_events (id, aggregate_type, aggregate_id, event_schema, payload, status, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`

	selectTransactionSQL = `
		SELECT id, tenant_id, idempotency_key, request_fingerprint, status, created_at
		FROM transactions
		WHERE id = $1`

	selectTransactionByTenantIdemKeySQL = `
		SELECT id, tenant_id, idempotency_key, request_fingerprint, status, created_at
		FROM transactions
		WHERE tenant_id = $1 AND idempotency_key = $2`

	selectJournalEntriesSQL = `
		SELECT id, transaction_id, tenant_id, account_id, amount, currency, created_at
		FROM journal_entries
		WHERE transaction_id = $1
		ORDER BY created_at, id`
)

// ExecuteTransfer atomically writes one Transaction, two JournalEntry legs
// (debit on FromAccountID, credit on ToAccountID), one OutboxEvent, and
// one audit_pending row.
//
// Invariants enforced:
//  1. Currency is a valid ISO-4217-shaped code.
//  2. The two-leg transaction sums to zero (domain.AssertBalanced).
//  3. idempotency_key is unique — UNIQUE-violation is mapped to
//     domain.ErrDuplicateIdempotencyKey for the usecase to handle.
//
// Atomicity model:
//
//	BEGIN ISOLATION LEVEL SERIALIZABLE
//	  -- single round-trip via pgx batched protocol (SendBatch):
//	  INSERT transactions
//	  INSERT journal_entries (debit)
//	  INSERT journal_entries (credit)
//	  INSERT outbox_events
//	  INSERT audit_pending
//	COMMIT
//
// All five inserts pipeline into one network round-trip — ~5 RTTs of
// per-tx latency saved over the unbatched path, which both lifts the
// pool-saturation TPS ceiling and shrinks the SERIALIZABLE snapshot
// window (so hot-account 40001 conflicts get a smaller hit-rate bonus).
//
// On SQLSTATE 40001 the wrapper retries the entire block. The outbox
// insert participates in the SAME transaction — the whole point of the
// Transactional Outbox pattern: ledger durability ⟺ event durability.
// audit_pending is written in the same tx for the same reason: a
// committed ledger row never exists without its audit row enqueued.
func (r *LedgerRepo) ExecuteTransfer(ctx context.Context, req domain.TransferRequest) (*domain.Transaction, error) {
	if err := r.validateTransferRequest(req); err != nil {
		return nil, err
	}

	now := r.now()
	txID := r.newID()
	debitLeg := domain.JournalEntry{
		ID:            r.newID(),
		TransactionID: txID,
		TenantID:      req.TenantID,
		AccountID:     req.FromAccountID,
		Amount:        req.Amount.Neg(),
		Currency:      req.Currency,
		CreatedAt:     now,
	}
	creditLeg := domain.JournalEntry{
		ID:            r.newID(),
		TransactionID: txID,
		TenantID:      req.TenantID,
		AccountID:     req.ToAccountID,
		Amount:        req.Amount,
		Currency:      req.Currency,
		CreatedAt:     now,
	}
	transaction := &domain.Transaction{
		ID:                 txID,
		TenantID:           req.TenantID,
		IdempotencyKey:     req.IdempotencyKey,
		RequestFingerprint: req.RequestFingerprint,
		Status:             domain.TransactionStatusCommitted,
		Entries:            []domain.JournalEntry{debitLeg, creditLeg},
		CreatedAt:          now,
	}

	// Defense-in-depth: balance check BEFORE going to the DB. Costs one
	// decimal sum; saves a network round-trip on malformed inputs.
	if err := transaction.AssertBalanced(); err != nil {
		return nil, err
	}

	outboxID := r.newID()

	// Per-(tenant, op) circuit breaker: when correlated 40001 storms
	// dominate (hot account contention), failing fast for 1s prevents
	// retry-amplification meltdown while letting unrelated tenants keep
	// flowing.
	breakerKey := BreakerKey(req.TenantID, "ExecuteTransfer")
	if r.breaker != nil && !r.breaker.Allow(breakerKey) {
		return nil, ErrCircuitOpen
	}

	// Build the audit event up front so envelope validation fails the
	// request before we open a tx (saves one pool acquire on bad input).
	// canonicalTransactionAudit is pure CPU — no DB round-trip.
	auditBody, err := canonicalTransactionAudit(transaction)
	if err != nil {
		return nil, fmt.Errorf("audit: canonicalize transaction: %w", err)
	}
	auditEvt := domain.AuditEvent{
		AuditEnvelope: req.AuditEnvelope,
		AggregateType: domain.AuditAggregateTransaction,
		AggregateID:   transaction.ID,
		Operation:     domain.AuditOpTransferExecuted,
		BeforeState:   nil, // INSERT — no prior state
		AfterState:    auditBody,
	}
	if err := validateAuditEnvelope(auditEvt); err != nil {
		return nil, err
	}

	err = InTx(ctx, r.pool, r.acquireCfg, r.retry, r.metrics, func(ctx context.Context, tx pgx.Tx) error {
		// All five inserts go in one round-trip via pgx's batched
		// protocol. Order is load-bearing: classifyBatchErr maps the
		// failing-statement index back to the right domain error
		// (UNIQUE on transactions → ErrDuplicateIdempotencyKey,
		// composite FK on journal_entries → ErrAccountTenantMismatch).
		batch := &pgx.Batch{}
		batch.Queue(insertTransactionSQL,
			transaction.ID, transaction.TenantID, transaction.IdempotencyKey,
			transaction.RequestFingerprint,
			string(transaction.Status), transaction.CreatedAt,
		)
		for i := range transaction.Entries {
			leg := transaction.Entries[i]
			batch.Queue(insertJournalEntrySQL,
				leg.ID, leg.TransactionID, leg.TenantID, leg.AccountID,
				leg.Amount, string(leg.Currency), leg.CreatedAt,
			)
		}
		batch.Queue(insertOutboxEventSQL,
			outboxID, string(domain.AggregateTypeTransaction),
			transaction.ID, req.OutboxSchema, req.OutboxPayload,
			string(domain.OutboxStatusPending), now,
		)
		batch.Queue(insertAuditPendingSQL, auditPendingArgs(auditEvt)...)

		// SendBatch pipelines the queued statements; results come back
		// in queue order. Each Exec() must be drained — skipping one
		// blocks the next. defer Close releases pgx's batch state.
		br := tx.SendBatch(ctx, batch)
		defer br.Close()
		for i := 0; i < batch.Len(); i++ {
			if _, err := br.Exec(); err != nil {
				return classifyBatchErr(err, i)
			}
		}
		return nil
	})
	// Feed breaker outcome before returning. capReached=true is the
	// signal that we exhausted the retry budget on 40001 — a strong
	// indicator of correlated contention.
	if r.breaker != nil {
		r.breaker.RecordOutcome(breakerKey, errors.Is(err, ErrRetryCapReached))
	}
	if err != nil {
		return nil, err
	}
	return transaction, nil
}

// GetTransaction loads a transaction with all its legs. Read path —
// not wrapped in InTx since READ COMMITTED is sufficient for this query
// and avoids unnecessary 40001 retries on a pure read.
func (r *LedgerRepo) GetTransaction(ctx context.Context, id uuid.UUID) (*domain.Transaction, error) {
	return r.loadTransaction(ctx, selectTransactionSQL, id)
}

// GetTransactionByIdempotencyKey is the replay-path read. Tenant scoping
// is mandatory: composite UNIQUE (tenant_id, idempotency_key) means the
// same key can legally exist across tenants, and an unscoped query could
// return another tenant's row. Returns ErrTransactionNotFound if no row.
func (r *LedgerRepo) GetTransactionByIdempotencyKey(ctx context.Context, tenantID, key string) (*domain.Transaction, error) {
	if tenantID == "" {
		return nil, domain.ErrTenantRequired
	}
	if key == "" {
		return nil, fmt.Errorf("repository: idempotency key is required")
	}
	return r.loadTransaction(ctx, selectTransactionByTenantIdemKeySQL, tenantID, key)
}

// loadTransaction fetches the transactions row using the supplied SQL +
// arguments, then attaches journal entries. Two pool queries on purpose:
// keeping legs as a separate query lets us paginate them later without
// changing the header query shape.
func (r *LedgerRepo) loadTransaction(ctx context.Context, headerSQL string, args ...any) (*domain.Transaction, error) {
	var t domain.Transaction
	var statusStr string
	err := r.pool.QueryRow(ctx, headerSQL, args...).
		Scan(&t.ID, &t.TenantID, &t.IdempotencyKey, &t.RequestFingerprint, &statusStr, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrTransactionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("repository: load transaction: %w", err)
	}
	t.Status = domain.TransactionStatus(statusStr)

	rows, err := r.pool.Query(ctx, selectJournalEntriesSQL, t.ID)
	if err != nil {
		return nil, fmt.Errorf("repository: query journal entries: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var e domain.JournalEntry
		var currStr string
		if err := rows.Scan(&e.ID, &e.TransactionID, &e.TenantID, &e.AccountID, &e.Amount, &currStr, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("repository: scan journal entry: %w", err)
		}
		e.Currency = domain.Currency(currStr)
		t.Entries = append(t.Entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repository: iterate journal entries: %w", err)
	}
	return &t, nil
}

// classifyBatchErr maps an error from a queued INSERT in
// ExecuteTransfer's pgx.Batch back to the right domain error based on
// which statement failed. The index is the position in the batch; the
// order is locked by the Queue() calls in ExecuteTransfer:
//
//	0: INSERT transactions       — UNIQUE → ErrDuplicateIdempotencyKey
//	1: INSERT journal_entries#1  — FK     → ErrAccountTenantMismatch
//	2: INSERT journal_entries#2  — FK     → ErrAccountTenantMismatch
//	3: INSERT outbox_events
//	4: INSERT audit_pending
//
// Mirroring the unbatched path's per-statement error mapping keeps
// callers (the usecase) blind to whether the repo pipelines or not.
func classifyBatchErr(err error, idx int) error {
	switch idx {
	case 0:
		if IsUniqueViolation(err) {
			return domain.ErrDuplicateIdempotencyKey
		}
		return fmt.Errorf("insert transaction: %w", err)
	case 1, 2:
		if IsForeignKeyViolation(err) {
			return domain.ErrAccountTenantMismatch
		}
		return fmt.Errorf("insert journal entry: %w", err)
	case 3:
		return fmt.Errorf("insert outbox event: %w", err)
	case 4:
		return fmt.Errorf("audit: enqueue pending: %w", err)
	default:
		return fmt.Errorf("batch insert idx=%d: %w", idx, err)
	}
}

func (r *LedgerRepo) validateTransferRequest(req domain.TransferRequest) error {
	if req.TenantID == "" {
		return domain.ErrTenantRequired
	}
	if req.IdempotencyKey == "" {
		return domain.ErrDuplicateIdempotencyKey
	}
	if req.RequestFingerprint == "" {
		return fmt.Errorf("repository: request_fingerprint is required")
	}
	if !req.Currency.Valid() {
		return domain.ErrInvalidCurrency
	}
	if req.FromAccountID == uuid.Nil || req.ToAccountID == uuid.Nil {
		return domain.ErrAccountNotFound
	}
	if req.FromAccountID == req.ToAccountID {
		// Self-transfer would balance trivially but is almost certainly a
		// caller bug — reject early. Loosen if a real use case appears.
		return fmt.Errorf("repository: from and to account IDs must differ")
	}
	if req.Amount.IsZero() || req.Amount.IsNegative() {
		return fmt.Errorf("repository: transfer amount must be strictly positive")
	}
	if len(req.OutboxPayload) == 0 {
		return fmt.Errorf("repository: outbox payload is required")
	}
	return nil
}
