package domain

import (
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

type TransactionStatus string

const (
	TransactionStatusCommitted TransactionStatus = "COMMITTED"
	TransactionStatusReversed  TransactionStatus = "REVERSED"
)

func (s TransactionStatus) Valid() bool {
	switch s {
	case TransactionStatusCommitted, TransactionStatusReversed:
		return true
	}
	return false
}

// Transaction is the aggregate that owns a balanced set of JournalEntry legs.
// IdempotencyKey is the user-supplied key the system uses to guarantee
// at-most-once semantics even under retry storms.
type Transaction struct {
	ID             uuid.UUID
	TenantID       string
	IdempotencyKey string
	Status         TransactionStatus
	Entries        []JournalEntry
	CreatedAt      time.Time
}

func NewTransaction(id uuid.UUID, tenantID, idemKey string, status TransactionStatus, entries []JournalEntry, createdAt time.Time) (*Transaction, error) {
	if tenantID == "" {
		return nil, ErrTenantRequired
	}
	if idemKey == "" {
		return nil, ErrDuplicateIdempotencyKey
	}
	if !status.Valid() {
		return nil, ErrInvalidTxStatus
	}
	return &Transaction{
		ID:             id,
		TenantID:       tenantID,
		IdempotencyKey: idemKey,
		Status:         status,
		Entries:        entries,
		CreatedAt:      createdAt,
	}, nil
}

// AssertBalanced enforces the double-entry invariant: SUM(amount) over all
// legs MUST equal zero. Caller MUST treat a non-nil return as fatal and
// rollback the surrounding DB transaction.
func (t *Transaction) AssertBalanced() error {
	if len(t.Entries) < 2 {
		return ErrUnbalancedTransaction
	}
	sum := decimal.Zero
	for i := range t.Entries {
		sum = sum.Add(t.Entries[i].Amount)
	}
	if !sum.IsZero() {
		return ErrUnbalancedTransaction
	}
	return nil
}
