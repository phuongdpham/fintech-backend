package domain

import "github.com/google/uuid"

// CardChargeRequest is the input shape for a Stripe-style card-in
// flow. Carries everything the workflow needs to drive the charge
// from INITIATED through SETTLED without re-fetching context.
//
// PaymentMethodTok is a tokenized payment method (Stripe's pm_*),
// never a raw PAN — gateway-svc deliberately stays out of PCI scope.
type CardChargeRequest struct {
	TenantID         string
	IdempotencyKey   string
	UserAccountID    uuid.UUID
	Amount           Amount
	Currency         Currency
	PaymentMethodTok string

	// LedgerTransferID is the back-reference into ledger-svc's
	// transactions table when the inbound flow pre-allocates a
	// PENDING_EXTERNAL transfer. Optional in v1 — when absent, the
	// gateway-svc → ledger-svc settled event creates the ledger
	// transfer at confirmation time.
	LedgerTransferID *uuid.UUID
}

// Validate runs the cheap surface checks every Start call needs.
// The workflow adapter calls this before opening a tx so a malformed
// request doesn't cost a pool acquire.
func (r CardChargeRequest) Validate() error {
	if r.TenantID == "" {
		return ErrTenantRequired
	}
	if r.IdempotencyKey == "" {
		return ErrIdempotencyKeyEmpty
	}
	if r.UserAccountID == uuid.Nil {
		return ErrAccountRequired
	}
	if !r.Currency.Valid() {
		return ErrInvalidCurrency
	}
	if r.Amount.IsZero() {
		return ErrInvalidAmount
	}
	if r.PaymentMethodTok == "" {
		return ErrPaymentMethodEmpty
	}
	return nil
}

// CardChargeState is the per-row state for a card-in workflow. Keep
// this enum, the SQL CHECK constraint in
// migrations/001_init_gateway.up.sql, and the diagram in
// docs/charter.md mutually consistent — all three are the same
// state machine, expressed in different formats.
type CardChargeState string

const (
	CardChargeInitiated   CardChargeState = "INITIATED"
	CardChargeAuthorizing CardChargeState = "AUTHORIZING"
	CardChargeAuthorized  CardChargeState = "AUTHORIZED"
	CardChargeCaptured    CardChargeState = "CAPTURED"
	CardChargeSettled     CardChargeState = "SETTLED"
	CardChargeFailed      CardChargeState = "FAILED"
	CardChargeVoided      CardChargeState = "VOIDED"
	CardChargeRefunded    CardChargeState = "REFUNDED"
	CardChargeChargeback  CardChargeState = "CHARGEBACK"
	CardChargeAbandoned   CardChargeState = "ABANDONED"
)

// validCardChargeTransitions encodes the state machine. States with
// no entry in this map are terminal. Don't widen without updating
// the SQL CHECK constraint and the state diagram in
// docs/charter.md — the three must agree.
var validCardChargeTransitions = map[CardChargeState]map[CardChargeState]struct{}{
	CardChargeInitiated:   {CardChargeAuthorizing: {}},
	CardChargeAuthorizing: {CardChargeAuthorized: {}, CardChargeFailed: {}},
	CardChargeAuthorized:  {CardChargeCaptured: {}, CardChargeVoided: {}, CardChargeAbandoned: {}},
	CardChargeCaptured:    {CardChargeSettled: {}, CardChargeRefunded: {}},
	CardChargeSettled:     {CardChargeChargeback: {}},
	// Terminal: FAILED, VOIDED, REFUNDED, CHARGEBACK, ABANDONED.
}

// CanTransitionTo reports whether moving from s to next is allowed.
// Adapters call this before applying a state change; the contract is
// that ErrInvalidTransition is returned (not surfaced as a generic
// SQL error) when the predicate is false.
func (s CardChargeState) CanTransitionTo(next CardChargeState) bool {
	allowed, ok := validCardChargeTransitions[s]
	if !ok {
		return false
	}
	_, ok = allowed[next]
	return ok
}

// Terminal reports whether s is an end state (no outgoing transitions).
// Used by sweepers / reconcilers to skip workflows that don't need
// further driving.
func (s CardChargeState) Terminal() bool {
	_, hasNext := validCardChargeTransitions[s]
	return !hasNext
}

// Cancellable reports whether Cancel is currently legal for this
// state. Cards stay cancellable through AUTHORIZED (we void the
// auth); past CAPTURED the path is "issue a refund," not "cancel."
func (s CardChargeState) Cancellable() bool {
	switch s {
	case CardChargeInitiated, CardChargeAuthorizing, CardChargeAuthorized:
		return true
	}
	return false
}

// CardChargeWorkflow is the workflow port specialized to the card-in
// flow. Usecases depend on this alias; v1 fulfilled by a PG-backed
// adapter, v2 by Temporal/Restate.
type CardChargeWorkflow = PaymentWorkflow[CardChargeRequest, CardChargeState]
