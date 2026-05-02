package domain

import "github.com/google/uuid"

// OutboundBankTransferRequest is the rail-agnostic input shape for
// "user wants money sent to their bank account." The infrastructure
// adapter routes the request to the right concrete rail (Singapore
// PayNow, Vietnam NAPAS, US ACH if ever, …) based on Country and
// the deployment's rail catalog.
//
// The domain expresses the BUSINESS concept; rail mechanics
// (submission windows, return-code handling, processor SDK calls)
// live in the infra adapter, not here. Adding a new rail does NOT
// change this struct — only adds a new adapter that implements
// OutboundBankTransferWorkflow.
type OutboundBankTransferRequest struct {
	TenantID         string
	IdempotencyKey   string
	UserAccountID    uuid.UUID
	LedgerTransferID uuid.UUID

	Amount   Amount
	Currency Currency

	// Country is ISO-3166 alpha-2 ('SG', 'VN', 'US', …). Drives
	// rail selection in the adapter layer. Required even in v1
	// single-country deployments — keeps the contract stable as
	// markets are added.
	Country string

	// Destination carries the recipient identifier. Format is
	// rail-specific and the domain treats it as opaque; the adapter
	// validates per-rail. See OutboundDestination for the kinds.
	Destination OutboundDestination
}

// OutboundDestination is the recipient identifier in a rail-agnostic
// envelope. Token is opaque from the domain's POV — the adapter
// parses based on Kind.
type OutboundDestination struct {
	// Kind discriminates the Token format. Examples:
	//   'paynow_proxy'    — Singapore PayNow proxy (NRIC/UEN/mobile/VPA)
	//   'bank_account'    — generic account-number tokenized reference
	//   'napas_account'   — Vietnam NAPAS bank account
	//   'iban'            — SEPA / international
	// Adapters reject Kinds they don't know how to route.
	Kind string

	// Token is the recipient identifier value. Always tokenized at
	// the boundary — the gateway never accepts raw account/routing
	// numbers from clients; tokenization happens at the BFF or
	// processor layer (e.g., Plaid Link, Stripe Financial Connections).
	Token string
}

// Validate runs the cheap surface checks every Start call needs.
// Rail-specific validation (proxy format check, country/Currency
// compatibility) is the adapter's job — kept out of the domain so
// the rail catalog can grow without touching this file.
func (r OutboundBankTransferRequest) Validate() error {
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
		// Outbound transfers always have a prior internal debit —
		// without the ledger reference the workflow can't reconcile
		// or compensate.
		return ErrAccountRequired
	}
	if !r.Currency.Valid() {
		return ErrInvalidCurrency
	}
	if r.Amount.IsZero() {
		return ErrInvalidAmount
	}
	if r.Country == "" || len(r.Country) != 2 {
		return ErrCountryRequired
	}
	if r.Destination.Kind == "" || r.Destination.Token == "" {
		return ErrDestinationEmpty
	}
	return nil
}

// OutboundBankTransferState is the BUSINESS-LEVEL state of a transfer.
// Adapters map their rail-specific internal states (PayNow's
// SUBMITTED, ACH's PENDING, NAPAS's processing windows) to one of
// these — the domain doesn't care whether the rail uses 3, 5, or 7
// internal sub-states.
//
// Keep this enum, the SQL CHECK constraint in the per-rail tables,
// and the diagram in docs/charter.md mutually consistent. Each
// rail's adapter is free to add internal sub-states for its own
// tracking; the visible (snapshot) state stays in this enum.
type OutboundBankTransferState string

const (
	// OutboundRequested — workflow created, validated, accepted by
	// the adapter. No money has left the gateway yet.
	OutboundRequested OutboundBankTransferState = "REQUESTED"

	// OutboundInFlight — the rail is moving the money. Could mean
	// "submitted to PayNow, awaiting ack" (real-time, seconds) or
	// "in NACHA settlement window" (batched, days). Adapter knows
	// the timing; domain just knows "money is en route."
	OutboundInFlight OutboundBankTransferState = "IN_FLIGHT"

	// OutboundSettled — recipient bank confirmed receipt. Terminal
	// for most rails; can transition to REVERSED on ACH return.
	OutboundSettled OutboundBankTransferState = "SETTLED"

	// OutboundReversed — post-settlement reversal (e.g., ACH return,
	// SEPA recall). Real-time rails (PayNow, NAPAS) generally don't
	// reach this state — once they SETTLE, reversal requires a
	// separate inverse transfer, which is its own workflow.
	OutboundReversed OutboundBankTransferState = "REVERSED"

	// OutboundFailed — rail rejected at submission OR before settlement.
	// Money never left the gateway; ledger compensation is straightforward.
	OutboundFailed OutboundBankTransferState = "FAILED"

	// OutboundCancelled — operator or system cancelled the workflow
	// before it went IN_FLIGHT. Only legal pre-IN_FLIGHT.
	OutboundCancelled OutboundBankTransferState = "CANCELLED"
)

// validOutboundTransitions encodes the abstract state machine.
// Rail-specific adapters may have richer internal transitions
// (PayNow's INITIATED → SUBMITTED → SETTLED) but the externally-
// visible transitions are these.
//
// States with no entry in this map are terminal. SETTLED is
// non-terminal because slow rails (ACH) can transition to REVERSED
// post-settlement; real-time rails simply never make that transition.
var validOutboundTransitions = map[OutboundBankTransferState]map[OutboundBankTransferState]struct{}{
	OutboundRequested: {OutboundInFlight: {}, OutboundCancelled: {}, OutboundFailed: {}},
	OutboundInFlight:  {OutboundSettled: {}, OutboundFailed: {}},
	OutboundSettled:   {OutboundReversed: {}},
	// Terminal: REVERSED, FAILED, CANCELLED.
}

// CanTransitionTo reports whether moving from s to next is allowed.
// Adapters call this before applying a state change.
func (s OutboundBankTransferState) CanTransitionTo(next OutboundBankTransferState) bool {
	allowed, ok := validOutboundTransitions[s]
	if !ok {
		return false
	}
	_, ok = allowed[next]
	return ok
}

// Terminal reports whether s is an end state (no outgoing transitions).
func (s OutboundBankTransferState) Terminal() bool {
	_, hasNext := validOutboundTransitions[s]
	return !hasNext
}

// Cancellable reports whether Cancel is currently legal. Only
// REQUESTED is cancellable — once IN_FLIGHT, the rail owns the
// money and reversal becomes a separate workflow (per-rail).
func (s OutboundBankTransferState) Cancellable() bool {
	return s == OutboundRequested
}

// OutboundBankTransferWorkflow is the workflow port specialized to
// outbound bank transfers. v1: implemented by a Singapore PayNow
// adapter in internal/infrastructure. v2+: additional adapters per
// rail (NAPAS, e-wallets, etc.) can implement the same interface.
type OutboundBankTransferWorkflow = PaymentWorkflow[OutboundBankTransferRequest, OutboundBankTransferState]
