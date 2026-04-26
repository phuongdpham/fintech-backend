package domain

import "errors"

// Sentinel domain errors. Adapters wrap these to add infrastructure context;
// usecases compare with errors.Is to drive control flow without leaking
// driver-specific error types upward.
var (
	ErrInvalidCurrency       = errors.New("domain: invalid ISO-4217 currency code")
	ErrInvalidAccountStatus  = errors.New("domain: invalid account status")
	ErrInvalidTxStatus       = errors.New("domain: invalid transaction status")
	ErrInvalidOutboxStatus   = errors.New("domain: invalid outbox status")
	ErrAccountClosed         = errors.New("domain: account is closed")
	ErrAccountFrozen         = errors.New("domain: account is frozen")
	ErrCurrencyMismatch      = errors.New("domain: journal entry currency does not match account currency")

	// ErrUnbalancedTransaction is the most important invariant the engine
	// enforces: SUM(amount) over a transaction's legs MUST equal zero.
	// Returning this error MUST trigger a transaction rollback.
	ErrUnbalancedTransaction = errors.New("domain: transaction legs do not sum to zero")

	ErrDuplicateIdempotencyKey = errors.New("domain: idempotency key already used")
	ErrAccountNotFound         = errors.New("domain: account not found")
	ErrTransactionNotFound     = errors.New("domain: transaction not found")

	// ErrTransferInFlight indicates an existing request under the same
	// idempotency key is still STARTED. Caller should retry after a short
	// delay; the response will eventually settle to a stored transaction
	// (replayed) or a terminal failure.
	ErrTransferInFlight = errors.New("domain: transfer already in flight under this idempotency key")

	// ErrIdempotencyKeyFailed indicates a previous request under the same
	// idempotency key terminated in FAILED. Per the plan's at-most-once
	// contract, the same key cannot be reused — caller must mint a new key.
	ErrIdempotencyKeyFailed = errors.New("domain: idempotency key previously failed; use a new key")
)
