package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/domain"
)

// AssertBalanced is the most important invariant in the engine. Test it
// table-driven so future contributors add edge cases here, not in scattered
// integration tests.
func TestTransaction_AssertBalanced(t *testing.T) {
	now := time.Now().UTC()
	amt := func(s string) domain.Amount {
		a, err := domain.NewAmount(s)
		require.NoError(t, err)
		return a
	}
	leg := func(amount string) domain.JournalEntry {
		return domain.JournalEntry{
			ID:            uuid.New(),
			TransactionID: uuid.New(),
			AccountID:     uuid.New(),
			Amount:        amt(amount),
			Currency:      "USD",
			CreatedAt:     now,
		}
	}

	cases := []struct {
		name    string
		entries []domain.JournalEntry
		wantErr error
	}{
		{
			name:    "empty legs is unbalanced",
			entries: nil,
			wantErr: domain.ErrUnbalancedTransaction,
		},
		{
			name:    "single leg is unbalanced",
			entries: []domain.JournalEntry{leg("100.0000")},
			wantErr: domain.ErrUnbalancedTransaction,
		},
		{
			name:    "balanced two-leg transfer",
			entries: []domain.JournalEntry{leg("-100.0000"), leg("100.0000")},
			wantErr: nil,
		},
		{
			name:    "balanced three-leg fee split",
			entries: []domain.JournalEntry{leg("-100.0000"), leg("99.5000"), leg("0.5000")},
			wantErr: nil,
		},
		{
			name:    "off-by-one-minor-unit is rejected",
			entries: []domain.JournalEntry{leg("-100.0000"), leg("99.9999")},
			wantErr: domain.ErrUnbalancedTransaction,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx := &domain.Transaction{
				ID:             uuid.New(),
				IdempotencyKey: "k",
				Status:         domain.TransactionStatusCommitted,
				Entries:        tc.entries,
				CreatedAt:      now,
			}
			err := tx.AssertBalanced()
			if tc.wantErr == nil {
				require.NoError(t, err)
			} else {
				require.True(t, errors.Is(err, tc.wantErr), "want %v, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestCurrency_Valid(t *testing.T) {
	cases := []struct {
		in   domain.Currency
		want bool
	}{
		{"USD", true},
		{"EUR", true},
		{"VND", true},
		{"usd", false}, // lowercase rejected
		{"US", false},  // too short
		{"USDT", false}, // too long
		{"US1", false}, // digit rejected
	}
	for _, tc := range cases {
		t.Run(string(tc.in), func(t *testing.T) {
			require.Equal(t, tc.want, tc.in.Valid())
		})
	}
}
