package domain

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestACHTransferState_CanTransitionTo(t *testing.T) {
	cases := []struct {
		from, to ACHTransferState
		ok       bool
	}{
		// Happy path
		{ACHInitiated, ACHSubmitted, true},
		{ACHSubmitted, ACHPending, true},
		{ACHPending, ACHSettled, true},

		// Cancel before submission only
		{ACHInitiated, ACHCancelled, true},
		{ACHSubmitted, ACHCancelled, false},
		{ACHPending, ACHCancelled, false},

		// Failure paths
		{ACHSubmitted, ACHFailed, true},
		{ACHPending, ACHReturned, true},

		// Skipping states is forbidden
		{ACHInitiated, ACHPending, false},
		{ACHInitiated, ACHSettled, false},
		{ACHSubmitted, ACHSettled, false},

		// Terminal states have no outgoing transitions
		{ACHSettled, ACHReturned, false},
		{ACHReturned, ACHSettled, false},
		{ACHFailed, ACHSubmitted, false},
		{ACHCancelled, ACHSubmitted, false},

		// Going backwards is forbidden
		{ACHSubmitted, ACHInitiated, false},
		{ACHPending, ACHSubmitted, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.from)+"→"+string(tc.to), func(t *testing.T) {
			got := tc.from.CanTransitionTo(tc.to)
			if got != tc.ok {
				t.Fatalf("CanTransitionTo: got %v, want %v", got, tc.ok)
			}
		})
	}
}

func TestACHTransferState_Terminal(t *testing.T) {
	cases := []struct {
		s        ACHTransferState
		terminal bool
	}{
		{ACHInitiated, false},
		{ACHSubmitted, false},
		{ACHPending, false},
		{ACHSettled, true},
		{ACHReturned, true},
		{ACHFailed, true},
		{ACHCancelled, true},
	}
	for _, tc := range cases {
		t.Run(string(tc.s), func(t *testing.T) {
			if got := tc.s.Terminal(); got != tc.terminal {
				t.Fatalf("Terminal: got %v, want %v", got, tc.terminal)
			}
		})
	}
}

func TestACHTransferState_Cancellable(t *testing.T) {
	cases := []struct {
		s   ACHTransferState
		yes bool
	}{
		{ACHInitiated, true},
		{ACHSubmitted, false}, // ACH reversal, not cancel
		{ACHPending, false},
		{ACHSettled, false},
		{ACHReturned, false},
		{ACHFailed, false},
		{ACHCancelled, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.s), func(t *testing.T) {
			if got := tc.s.Cancellable(); got != tc.yes {
				t.Fatalf("Cancellable: got %v, want %v", got, tc.yes)
			}
		})
	}
}

func TestACHTransferRequest_Validate(t *testing.T) {
	valid := ACHTransferRequest{
		TenantID:         "tenant-001",
		IdempotencyKey:   "idem-ach-001",
		UserAccountID:    uuid.Must(uuid.NewV7()),
		LedgerTransferID: uuid.Must(uuid.NewV7()),
		Amount:           Amount("250.0000"),
		Currency:         Currency("USD"),
		DestinationToken: "ba_test",
	}

	cases := []struct {
		name   string
		mutate func(*ACHTransferRequest)
		want   error
	}{
		{"happy path", func(*ACHTransferRequest) {}, nil},
		{"missing tenant", func(r *ACHTransferRequest) { r.TenantID = "" }, ErrTenantRequired},
		{"missing idem key", func(r *ACHTransferRequest) { r.IdempotencyKey = "" }, ErrIdempotencyKeyEmpty},
		{"nil user account", func(r *ACHTransferRequest) { r.UserAccountID = uuid.Nil }, ErrAccountRequired},
		{"nil ledger transfer", func(r *ACHTransferRequest) { r.LedgerTransferID = uuid.Nil }, ErrAccountRequired},
		{"bad currency", func(r *ACHTransferRequest) { r.Currency = "u$d" }, ErrInvalidCurrency},
		{"zero amount", func(r *ACHTransferRequest) { r.Amount = "0" }, ErrInvalidAmount},
		{"missing destination", func(r *ACHTransferRequest) { r.DestinationToken = "" }, ErrDestinationEmpty},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := valid
			tc.mutate(&req)
			err := req.Validate()
			if !errors.Is(err, tc.want) {
				t.Fatalf("Validate: got %v, want %v", err, tc.want)
			}
		})
	}
}
