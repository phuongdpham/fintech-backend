// Package domain holds gateway-svc's entities, value objects, and the
// PaymentWorkflow port. Per the charter (apps/gateway-svc/docs/charter.md)
// this package has zero infrastructure dependencies — it imports
// uuid for the ID type and time for timestamps; nothing else.
//
// The PaymentWorkflow interface is the architectural seam between the
// usecase layer and whichever workflow engine is in use. v1 will
// implement it with a PG-backed adapter (state column + polling
// worker); v2 may swap to Temporal or Restate. Usecases depend only
// on the interface here.
package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// WorkflowKind enumerates the BUSINESS-LEVEL workflow categories the
// gateway drives — not the underlying rails. Card charging is one
// kind regardless of whether it goes through Stripe or Adyen; an
// outbound bank transfer is one kind regardless of whether the
// concrete rail is Singapore PayNow, Vietnam NAPAS, or US ACH.
//
// Rail-specific mechanics (PayNow's 5 states, ACH's 7 states with
// PENDING/RETURNED) live in the infrastructure adapter that
// implements the corresponding PaymentWorkflow[Req,State]. Adapters
// translate between the rail's internal sub-states and the
// domain-level state enum exposed on the workflow.
//
// Add a new constant here only when a genuinely new business
// category lands (e.g. e-wallet charges, QR payments) — not for a
// new rail under an existing category.
type WorkflowKind string

const (
	WorkflowKindCardCharge           WorkflowKind = "card_charge"
	WorkflowKindOutboundBankTransfer WorkflowKind = "outbound_bank_transfer"
)

// Valid returns true for kinds the gateway knows how to drive.
// Used by adapters to fail fast on bad input.
func (k WorkflowKind) Valid() bool {
	switch k {
	case WorkflowKindCardCharge, WorkflowKindOutboundBankTransfer:
		return true
	}
	return false
}

// WorkflowID is the durable handle to one in-flight workflow. The
// Kind+ID pair is what an adapter needs to route to the right table
// (v1) or workflow type (v2). Carrying the Kind alongside the UUID
// keeps callers from accidentally passing a random uuid.UUID where a
// workflow id is expected.
type WorkflowID struct {
	Kind WorkflowKind
	ID   uuid.UUID
}

// PaymentWorkflow is the implementation seam between usecase code and
// the workflow engine. v1 fulfilled by a PG-backed adapter; v2 by a
// Temporal or Restate adapter. The interface stays constant; only the
// implementation swaps.
//
// Type parameters carry the request and state shapes per rail so the
// interface stays type-safe without leaking adapter concepts (no
// "activity", no "decision task") into callers.
//
// Idempotency: Start MUST be idempotent on (TenantID, IdempotencyKey)
// inside Req — calling Start twice with the same key returns the same
// WorkflowID, never a duplicate workflow. Signal MUST be idempotent
// on Event.ID — duplicate webhook deliveries become no-ops.
type PaymentWorkflow[Req, State any] interface {
	// Start registers a new workflow and schedules its first action.
	// Returns ErrIdempotencyConflict if a different request body
	// already exists under the same (tenant, idempotency_key).
	Start(ctx context.Context, req Req) (WorkflowID, error)

	// Signal delivers an external event into a running workflow.
	// Webhook arrivals, manual approvals, refund commands, reconciler
	// nudges all flow through here. Returns ErrInvalidEvent if the
	// event does not apply to the workflow's current state.
	Signal(ctx context.Context, id WorkflowID, ev Event) error

	// Query returns the current snapshot. Read path; doesn't change
	// state. Used by BFF for status pages and by the reconciler for
	// stuck-workflow sweeps.
	Query(ctx context.Context, id WorkflowID) (Snapshot[State], error)

	// Cancel terminates the workflow if its current state allows it.
	// Returns ErrCannotCancel for workflows past the point of no
	// return (card already captured, ACH already submitted).
	Cancel(ctx context.Context, id WorkflowID, reason string) error
}

// Event is the signal envelope. Kind discriminates the payload; the
// adapter routes to the appropriate state-transition handler. ID is
// the dedup key — webhook redelivery and double-clicks on a manual
// op produce one applied state change, not many.
type Event struct {
	// ID is the dedup key. Adapters reject duplicates idempotently.
	// For provider webhooks this is the provider's event id (e.g.
	// Stripe's evt_*). For manual ops, generate a fresh uuidv7.
	ID uuid.UUID

	// Kind is a dotted name describing what happened. The adapter
	// matches on (workflow.state, event.kind) to pick a handler.
	// Examples:
	//   webhook.charge.succeeded
	//   webhook.charge.dispute.created
	//   webhook.ach.return
	//   manual.refund
	//   reconciler.timeout
	Kind string

	// OccurredAt is the upstream-asserted time of the underlying
	// event (e.g. Stripe's `created` timestamp), not when we
	// received it. Use the latter only when no upstream time exists.
	OccurredAt time.Time

	// Source labels who emitted the event. Keeps the audit trail
	// readable: 'stripe', 'plaid', 'support-tool', 'reconciler', ...
	Source string

	// Payload is canonical bytes (proto / JSON / raw webhook body
	// depending on Kind). Adapters know the schema by Kind.
	Payload []byte
}

// Snapshot is the read-side projection of a workflow at a point in
// time. The State type parameter is the per-rail enum
// (CardChargeState, ACHTransferState).
type Snapshot[State any] struct {
	ID           WorkflowID
	State        State
	CreatedAt    time.Time
	UpdatedAt    time.Time
	NextActionAt *time.Time
	NextAction   string
	History      []HistoryRecord

	// LastError carries the last provider error message for FAILED
	// workflows. Empty for happy-path states. Useful for support
	// tooling without forcing callers to walk History.
	LastError string
}

// HistoryRecord is one entry in a workflow's audit log. Backed by
// the workflow_events table in v1. Kept in chronological order
// inside Snapshot.History.
type HistoryRecord struct {
	EventType  string
	OccurredAt time.Time
	Details    []byte
}
