package domain

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestOutboundBankTransferState_CanTransitionTo(t *testing.T) {
	// Domain-level state machine. Rail adapters may have richer
	// internal sub-states; the externally-visible transitions are
	// only these. Drift here vs the SQL CHECK constraint or the
	// state diagram in docs/charter.md indicates one of the three
	// has gone out of sync.
	cases := []struct {
		from, to OutboundBankTransferState
		ok       bool
	}{
		// Happy path: REQUESTED → IN_FLIGHT → SETTLED
		{OutboundRequested, OutboundInFlight, true},
		{OutboundInFlight, OutboundSettled, true},

		// Pre-flight failure / cancellation
		{OutboundRequested, OutboundCancelled, true},
		{OutboundRequested, OutboundFailed, true},

		// In-flight failure
		{OutboundInFlight, OutboundFailed, true},

		// Post-settlement reversal (ACH-style; real-time rails just
		// never make this transition)
		{OutboundSettled, OutboundReversed, true},

		// Skipping states is forbidden
		{OutboundRequested, OutboundSettled, false},
		{OutboundRequested, OutboundReversed, false},
		{OutboundInFlight, OutboundReversed, false},
		{OutboundInFlight, OutboundCancelled, false}, // can't cancel once in-flight

		// Terminal states have no outgoing transitions
		{OutboundReversed, OutboundSettled, false},
		{OutboundFailed, OutboundInFlight, false},
		{OutboundCancelled, OutboundRequested, false},

		// Going backwards is forbidden
		{OutboundInFlight, OutboundRequested, false},
		{OutboundSettled, OutboundInFlight, false},
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

func TestOutboundBankTransferState_Terminal(t *testing.T) {
	cases := []struct {
		s        OutboundBankTransferState
		terminal bool
	}{
		{OutboundRequested, false},
		{OutboundInFlight, false},
		{OutboundSettled, false}, // can still transition to REVERSED
		{OutboundReversed, true},
		{OutboundFailed, true},
		{OutboundCancelled, true},
	}
	for _, tc := range cases {
		t.Run(string(tc.s), func(t *testing.T) {
			if got := tc.s.Terminal(); got != tc.terminal {
				t.Fatalf("Terminal: got %v, want %v", got, tc.terminal)
			}
		})
	}
}

func TestOutboundBankTransferState_Cancellable(t *testing.T) {
	cases := []struct {
		s   OutboundBankTransferState
		yes bool
	}{
		{OutboundRequested, true},
		{OutboundInFlight, false}, // rail owns the money; reversal is a separate workflow
		{OutboundSettled, false},
		{OutboundReversed, false},
		{OutboundFailed, false},
		{OutboundCancelled, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.s), func(t *testing.T) {
			if got := tc.s.Cancellable(); got != tc.yes {
				t.Fatalf("Cancellable: got %v, want %v", got, tc.yes)
			}
		})
	}
}

func TestOutboundBankTransferRequest_Validate(t *testing.T) {
	valid := OutboundBankTransferRequest{
		TenantID:         "tenant-001",
		IdempotencyKey:   "idem-out-001",
		UserAccountID:    uuid.Must(uuid.NewV7()),
		LedgerTransferID: uuid.Must(uuid.NewV7()),
		Amount:           Amount("250.0000"),
		Currency:         Currency("SGD"),
		Country:          "SG",
		Destination: OutboundDestination{
			Kind:  "paynow_proxy",
			Token: "+6591234567",
		},
	}

	cases := []struct {
		name   string
		mutate func(*OutboundBankTransferRequest)
		want   error
	}{
		{"happy path", func(*OutboundBankTransferRequest) {}, nil},
		{"missing tenant", func(r *OutboundBankTransferRequest) { r.TenantID = "" }, ErrTenantRequired},
		{"missing idem key", func(r *OutboundBankTransferRequest) { r.IdempotencyKey = "" }, ErrIdempotencyKeyEmpty},
		{"nil user account", func(r *OutboundBankTransferRequest) { r.UserAccountID = uuid.Nil }, ErrAccountRequired},
		{"nil ledger transfer", func(r *OutboundBankTransferRequest) { r.LedgerTransferID = uuid.Nil }, ErrAccountRequired},
		{"bad currency", func(r *OutboundBankTransferRequest) { r.Currency = "sg" }, ErrInvalidCurrency},
		{"zero amount", func(r *OutboundBankTransferRequest) { r.Amount = "0" }, ErrInvalidAmount},
		{"missing country", func(r *OutboundBankTransferRequest) { r.Country = "" }, ErrCountryRequired},
		{"bad country length", func(r *OutboundBankTransferRequest) { r.Country = "SGP" }, ErrCountryRequired},
		{"missing destination kind", func(r *OutboundBankTransferRequest) { r.Destination.Kind = "" }, ErrDestinationEmpty},
		{"missing destination token", func(r *OutboundBankTransferRequest) { r.Destination.Token = "" }, ErrDestinationEmpty},
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
