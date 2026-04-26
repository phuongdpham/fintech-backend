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
	pool   *pgxpool.Pool
	retry  RetryConfig
	now    func() time.Time
	newID  func() uuid.UUID
}

func NewLedgerRepo(pool *pgxpool.Pool) *LedgerRepo {
	return &LedgerRepo{
		pool:  pool,
		retry: DefaultRetryConfig(),
		now:   func() time.Time { return time.Now().UTC() },
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
		INSERT INTO transactions (id, idempotency_key, status, created_at)
		VALUES ($1, $2, $3, $4)`

	insertJournalEntrySQL = `
		INSERT INTO journal_entries (id, transaction_id, account_id, amount, currency, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)`

	insertOutboxEventSQL = `
		INSERT INTO outbox_events (id, aggregate_type, aggregate_id, payload, status, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)`

	selectTransactionSQL = `
		SELECT id, idempotency_key, status, created_at
		FROM transactions
		WHERE id = $1`

	selectTransactionByIdemKeySQL = `
		SELECT id, idempotency_key, status, created_at
		FROM transactions
		WHERE idempotency_key = $1`

	selectJournalEntriesSQL = `
		SELECT id, transaction_id, account_id, amount, currency, created_at
		FROM journal_entries
		WHERE transaction_id = $1
		ORDER BY created_at, id`
)

// ExecuteTransfer atomically writes one Transaction, two JournalEntry legs
// (debit on FromAccountID, credit on ToAccountID), and one OutboxEvent.
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
//	  INSERT transactions
//	  INSERT journal_entries (debit)
//	  INSERT journal_entries (credit)
//	  INSERT outbox_events
//	COMMIT
//
// On SQLSTATE 40001 the wrapper retries the entire block. The outbox
// insert participates in the SAME transaction, which is the whole point
// of the Transactional Outbox pattern: ledger durability ⟺ event durability.
func (r *LedgerRepo) ExecuteTransfer(ctx context.Context, req domain.TransferRequest) (*domain.Transaction, error) {
	if err := r.validateTransferRequest(req); err != nil {
		return nil, err
	}

	now := r.now()
	txID := r.newID()
	debitLeg := domain.JournalEntry{
		ID:            r.newID(),
		TransactionID: txID,
		AccountID:     req.FromAccountID,
		Amount:        req.Amount.Neg(),
		Currency:      req.Currency,
		CreatedAt:     now,
	}
	creditLeg := domain.JournalEntry{
		ID:            r.newID(),
		TransactionID: txID,
		AccountID:     req.ToAccountID,
		Amount:        req.Amount,
		Currency:      req.Currency,
		CreatedAt:     now,
	}
	transaction := &domain.Transaction{
		ID:             txID,
		IdempotencyKey: req.IdempotencyKey,
		Status:         domain.TransactionStatusCommitted,
		Entries:        []domain.JournalEntry{debitLeg, creditLeg},
		CreatedAt:      now,
	}

	// Defense-in-depth: balance check BEFORE going to the DB. Costs one
	// decimal sum; saves a network round-trip on malformed inputs.
	if err := transaction.AssertBalanced(); err != nil {
		return nil, err
	}

	outboxID := r.newID()

	err := InTx(ctx, r.pool, r.retry, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, insertTransactionSQL,
			transaction.ID, transaction.IdempotencyKey,
			string(transaction.Status), transaction.CreatedAt,
		); err != nil {
			if IsUniqueViolation(err) {
				return domain.ErrDuplicateIdempotencyKey
			}
			return fmt.Errorf("insert transaction: %w", err)
		}

		for i := range transaction.Entries {
			leg := transaction.Entries[i]
			if _, err := tx.Exec(ctx, insertJournalEntrySQL,
				leg.ID, leg.TransactionID, leg.AccountID,
				leg.Amount, string(leg.Currency), leg.CreatedAt,
			); err != nil {
				return fmt.Errorf("insert journal entry: %w", err)
			}
		}

		if _, err := tx.Exec(ctx, insertOutboxEventSQL,
			outboxID, string(domain.AggregateTypeTransaction),
			transaction.ID, req.OutboxPayload,
			string(domain.OutboxStatusPending), now,
		); err != nil {
			return fmt.Errorf("insert outbox event: %w", err)
		}

		return nil
	})
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

// GetTransactionByIdempotencyKey is the replay-path read. Returns
// ErrTransactionNotFound if the key has never been written.
func (r *LedgerRepo) GetTransactionByIdempotencyKey(ctx context.Context, key string) (*domain.Transaction, error) {
	if key == "" {
		return nil, fmt.Errorf("repository: idempotency key is required")
	}
	return r.loadTransaction(ctx, selectTransactionByIdemKeySQL, key)
}

// loadTransaction fetches the transactions row using the supplied SQL +
// argument, then attaches journal entries. Two pool queries on purpose:
// keeping legs as a separate query lets us paginate them later without
// changing the header query shape.
func (r *LedgerRepo) loadTransaction(ctx context.Context, headerSQL string, arg any) (*domain.Transaction, error) {
	var t domain.Transaction
	var statusStr string
	err := r.pool.QueryRow(ctx, headerSQL, arg).
		Scan(&t.ID, &t.IdempotencyKey, &statusStr, &t.CreatedAt)
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
		if err := rows.Scan(&e.ID, &e.TransactionID, &e.AccountID, &e.Amount, &currStr, &e.CreatedAt); err != nil {
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

func (r *LedgerRepo) validateTransferRequest(req domain.TransferRequest) error {
	if req.IdempotencyKey == "" {
		return domain.ErrDuplicateIdempotencyKey
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
