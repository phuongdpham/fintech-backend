package domain

import "github.com/google/uuid"

// ACHTransferRequest is the input shape for an ACH-out flow. Always
// has a LedgerTransferID — by the time gateway-svc starts an ACH-out
// workflow, ledger-svc has already committed the internal debit
// (user_account → external_pending) and we're driving the external
// settlement against that prior commit.
//
// DestinationToken is a tokenized bank account, not raw routing /
// account numbers — same PCI/scope rationale as cards.
type ACHTransferRequest struct {
	TenantID         string
	IdempotencyKey   string
	UserAccountID    uuid.UUID
	Amount           Amount
	Currency         Currency
	DestinationToken string
	LedgerTransferID uuid.UUID
}

// Validate runs the cheap surface checks every Start call needs.
func (r ACHTransferRequest) Validate() error {
	if r.TenantID == "" {
		return ErrTenantRequired
	}
	if r.IdempotencyKey == "" {
		return ErrIdempotencyKeyEmpty
	}
	if r.UserAccountID == uuid.Nil {
		return ErrAccountRequired
	}
	if r.LedgerTransferID == uuid.Nil {
		// ACH-out always has a prior internal debit — without the
		// ledger reference the workflow can't reconcile or compensate.
		return ErrAccountRequired
	}
	if !r.Currency.Valid() {
		return ErrInvalidCurrency
	}
	if r.Amount.IsZero() {
		return ErrInvalidAmount
	}
	if r.DestinationToken == "" {
		return ErrDestinationEmpty
	}
	return nil
}

// ACHTransferState is the per-row state for an ACH-out workflow.
// Keep this enum, the SQL CHECK constraint in
// migrations/001_init_gateway.up.sql, and the diagram in
// docs/charter.md mutually consistent.
type ACHTransferState string

const (
	ACHInitiated ACHTransferState = "INITIATED"
	ACHSubmitted ACHTransferState = "SUBMITTED"
	ACHPending   ACHTransferState = "PENDING"
	ACHSettled   ACHTransferState = "SETTLED"
	ACHReturned  ACHTransferState = "RETURNED"
	ACHFailed    ACHTransferState = "FAILED"
	ACHCancelled ACHTransferState = "CANCELLED"
)

// validACHTransitions encodes the state machine. States with no
// entry are terminal. Don't widen without updating the SQL CHECK
// constraint and the diagram in docs/charter.md.
var validACHTransitions = map[ACHTransferState]map[ACHTransferState]struct{}{
	ACHInitiated: {ACHSubmitted: {}, ACHCancelled: {}},
	ACHSubmitted: {ACHPending: {}, ACHFailed: {}},
	ACHPending:   {ACHSettled: {}, ACHReturned: {}},
	// Terminal: SETTLED, RETURNED, FAILED, CANCELLED.
}

// CanTransitionTo reports whether moving from s to next is allowed.
func (s ACHTransferState) CanTransitionTo(next ACHTransferState) bool {
	allowed, ok := validACHTransitions[s]
	if !ok {
		return false
	}
	_, ok = allowed[next]
	return ok
}

// Terminal reports whether s is an end state.
func (s ACHTransferState) Terminal() bool {
	_, hasNext := validACHTransitions[s]
	return !hasNext
}

// Cancellable reports whether Cancel is currently legal for this
// state. ACH is cancellable only before submission to the processor;
// once submitted, the only path to undoing a transfer is an ACH
// reversal — a separate workflow, not a Cancel call.
func (s ACHTransferState) Cancellable() bool {
	return s == ACHInitiated
}

// ACHTransferWorkflow is the workflow port specialized to the ACH-out
// flow. Usecases depend on this alias.
type ACHTransferWorkflow = PaymentWorkflow[ACHTransferRequest, ACHTransferState]
