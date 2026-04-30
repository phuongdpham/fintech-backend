package domain

import (
	"time"

	"github.com/google/uuid"
)

// JournalEntry is one leg of a double-entry transaction.
// Sign convention matches the Amount type: negative = debit, positive = credit.
//
// Two composite tuples map 1:1 to DB-level composite FKs:
//   (AccountID, Currency)  -> accounts(id, currency)         — currency lock
//   (AccountID, TenantID)  -> accounts(id, tenant_id)        — tenant lock
//   (TransactionID, TenantID) -> transactions(id, tenant_id) — tx lock
// A mismatch is a programming error and fails at write time.
type JournalEntry struct {
	ID            uuid.UUID
	TransactionID uuid.UUID
	TenantID      string
	AccountID     uuid.UUID
	Amount        Amount
	Currency      Currency
	CreatedAt     time.Time
}

func NewJournalEntry(id, txID uuid.UUID, tenantID string, accountID uuid.UUID, amount Amount, currency Currency, createdAt time.Time) (JournalEntry, error) {
	if tenantID == "" {
		return JournalEntry{}, ErrTenantRequired
	}
	if !currency.Valid() {
		return JournalEntry{}, ErrInvalidCurrency
	}
	return JournalEntry{
		ID:            id,
		TransactionID: txID,
		TenantID:      tenantID,
		AccountID:     accountID,
		Amount:        amount,
		Currency:      currency,
		CreatedAt:     createdAt,
	}, nil
}

// IsDebit / IsCredit are convenience predicates; zero-amount legs are
// neither and should not normally exist.
func (e JournalEntry) IsDebit() bool  { return e.Amount.IsNegative() }
func (e JournalEntry) IsCredit() bool { return e.Amount.IsPositive() }
