package domain

import "errors"

// Sentinel errors. Adapters and usecases compare with errors.Is.
//
// Boundary: domain errors describe domain-level conditions. Driver
// errors (network, SQL constraint violations, provider HTTP errors)
// stay in the adapter layer and only cross the boundary as one of
// these sentinels — keeps the usecase layer free of pgx / Stripe SDK
// types.
var (
	// ErrWorkflowNotFound — Query / Signal / Cancel against a
	// workflow id that doesn't exist in the store.
	ErrWorkflowNotFound = errors.New("gateway: workflow not found")

	// ErrIdempotencyConflict — Start called with an idempotency_key
	// that already exists for the tenant under a *different* request
	// body. The matching-body case returns the existing WorkflowID,
	// not this error.
	ErrIdempotencyConflict = errors.New("gateway: workflow already exists for idempotency key")

	// ErrCannotCancel — Cancel against a workflow whose current
	// state has no transition to a cancelled / voided terminal.
	// Card already captured, ACH already submitted, etc.
	ErrCannotCancel = errors.New("gateway: workflow cannot be cancelled in current state")

	// ErrInvalidTransition — adapter attempted a state change the
	// state machine forbids. Should be unreachable in correct code;
	// surfaces as a paging-grade signal that something violated the
	// state-machine contract.
	ErrInvalidTransition = errors.New("gateway: invalid state transition")

	// ErrInvalidEvent — Signal received an event whose Kind is not
	// applicable to the workflow's current state. Webhook for a
	// charge that was already voided, manual refund on a workflow
	// that hasn't been captured, etc.
	ErrInvalidEvent = errors.New("gateway: event does not apply to current state")

	// Request-validation sentinels — surface to the gRPC layer as
	// InvalidArgument; never leak adapter detail.
	ErrTenantRequired      = errors.New("gateway: tenant id is required")
	ErrIdempotencyKeyEmpty = errors.New("gateway: idempotency key is required")
	ErrInvalidCurrency     = errors.New("gateway: currency must be ISO-4217 three-letter code")
	ErrInvalidAmount       = errors.New("gateway: amount must be positive")
	ErrAccountRequired     = errors.New("gateway: user account id is required")
	ErrPaymentMethodEmpty  = errors.New("gateway: payment method token is required")
	ErrDestinationEmpty    = errors.New("gateway: destination token is required")
	ErrCountryRequired     = errors.New("gateway: country must be ISO-3166 alpha-2 code")
)
