package domain

import (
	"time"

	"github.com/google/uuid"
)

// JournalEntry is one leg of a double-entry transaction.
// Sign convention matches the Amount type: negative = debit, positive = credit.
//
// The composite (AccountID, Currency) tuple maps 1:1 to the DB's composite
// FK against accounts(id, currency); a mismatch is a programming error and
// will fail at write time.
type JournalEntry struct {
	ID            uuid.UUID
	TransactionID uuid.UUID
	AccountID     uuid.UUID
	Amount        Amount
	Currency      Currency
	CreatedAt     time.Time
}

func NewJournalEntry(id, txID, accountID uuid.UUID, amount Amount, currency Currency, createdAt time.Time) (JournalEntry, error) {
	if !currency.Valid() {
		return JournalEntry{}, ErrInvalidCurrency
	}
	return JournalEntry{
		ID:            id,
		TransactionID: txID,
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
