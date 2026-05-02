package domain

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestCardChargeState_CanTransitionTo(t *testing.T) {
	// Encodes the state machine in the test as a table; mismatch
	// against validCardChargeTransitions surfaces here. Keep this
	// list synced with the SQL CHECK constraint and the diagram in
	// docs/charter.md.
	cases := []struct {
		from, to CardChargeState
		ok       bool
	}{
		// Happy path
		{CardChargeInitiated, CardChargeAuthorizing, true},
		{CardChargeAuthorizing, CardChargeAuthorized, true},
		{CardChargeAuthorized, CardChargeCaptured, true},
		{CardChargeCaptured, CardChargeSettled, true},

		// Failure / cancellation paths
		{CardChargeAuthorizing, CardChargeFailed, true},
		{CardChargeAuthorized, CardChargeVoided, true},
		{CardChargeAuthorized, CardChargeAbandoned, true},
		{CardChargeCaptured, CardChargeRefunded, true},
		{CardChargeSettled, CardChargeChargeback, true},

		// Skipping states is forbidden
		{CardChargeInitiated, CardChargeAuthorized, false},
		{CardChargeInitiated, CardChargeCaptured, false},
		{CardChargeAuthorizing, CardChargeCaptured, false},
		{CardChargeAuthorized, CardChargeSettled, false},

		// Terminal states have no outgoing transitions
		{CardChargeFailed, CardChargeAuthorized, false},
		{CardChargeVoided, CardChargeRefunded, false},
		{CardChargeRefunded, CardChargeChargeback, false},
		{CardChargeChargeback, CardChargeRefunded, false},
		{CardChargeAbandoned, CardChargeAuthorized, false},

		// Going backwards is forbidden
		{CardChargeAuthorized, CardChargeInitiated, false},
		{CardChargeCaptured, CardChargeAuthorized, false},
		{CardChargeSettled, CardChargeCaptured, false},
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

func TestCardChargeState_Terminal(t *testing.T) {
	cases := []struct {
		s        CardChargeState
		terminal bool
	}{
		{CardChargeInitiated, false},
		{CardChargeAuthorizing, false},
		{CardChargeAuthorized, false},
		{CardChargeCaptured, false},
		{CardChargeSettled, false}, // can still go to CHARGEBACK
		{CardChargeFailed, true},
		{CardChargeVoided, true},
		{CardChargeRefunded, true},
		{CardChargeChargeback, true},
		{CardChargeAbandoned, true},
	}
	for _, tc := range cases {
		t.Run(string(tc.s), func(t *testing.T) {
			if got := tc.s.Terminal(); got != tc.terminal {
				t.Fatalf("Terminal: got %v, want %v", got, tc.terminal)
			}
		})
	}
}

func TestCardChargeState_Cancellable(t *testing.T) {
	cases := []struct {
		s   CardChargeState
		yes bool
	}{
		{CardChargeInitiated, true},
		{CardChargeAuthorizing, true},
		{CardChargeAuthorized, true},
		{CardChargeCaptured, false},  // refund, not cancel
		{CardChargeSettled, false},
		{CardChargeFailed, false},
		{CardChargeVoided, false},
		{CardChargeRefunded, false},
		{CardChargeChargeback, false},
		{CardChargeAbandoned, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.s), func(t *testing.T) {
			if got := tc.s.Cancellable(); got != tc.yes {
				t.Fatalf("Cancellable: got %v, want %v", got, tc.yes)
			}
		})
	}
}

func TestCardChargeRequest_Validate(t *testing.T) {
	valid := CardChargeRequest{
		TenantID:         "tenant-001",
		IdempotencyKey:   "idem-001",
		UserAccountID:    uuid.Must(uuid.NewV7()),
		Amount:           Amount("10.0000"),
		Currency:         Currency("USD"),
		PaymentMethodTok: "pm_test",
	}

	cases := []struct {
		name   string
		mutate func(*CardChargeRequest)
		want   error
	}{
		{"happy path", func(*CardChargeRequest) {}, nil},
		{"missing tenant", func(r *CardChargeRequest) { r.TenantID = "" }, ErrTenantRequired},
		{"missing idem key", func(r *CardChargeRequest) { r.IdempotencyKey = "" }, ErrIdempotencyKeyEmpty},
		{"nil account", func(r *CardChargeRequest) { r.UserAccountID = uuid.Nil }, ErrAccountRequired},
		{"bad currency", func(r *CardChargeRequest) { r.Currency = "us" }, ErrInvalidCurrency},
		{"zero amount", func(r *CardChargeRequest) { r.Amount = "0.0000" }, ErrInvalidAmount},
		{"missing pm token", func(r *CardChargeRequest) { r.PaymentMethodTok = "" }, ErrPaymentMethodEmpty},
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
