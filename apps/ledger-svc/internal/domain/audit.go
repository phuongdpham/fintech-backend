package domain

import (
	"time"

	"github.com/google/uuid"
)

// AuditEnvelope is the request-level identity/correlation context that
// every audit row carries. Sourced from the gRPC interceptor chain
// (Claims, RequestID, OTel SpanContext) and forwarded down through the
// usecase to the repository as part of the persistence call.
//
// Required fields (TenantID, ActorSubject, RequestID) come from the BFF
// via headers; if any is empty the audit subsystem refuses to write —
// silent anonymization is exactly the failure mode an audit log must
// not allow.
type AuditEnvelope struct {
	TenantID     string
	ActorSubject string
	ActorSession string
	RequestID    string
	TraceID      string // hex; empty when no OTel span is active
	OccurredAt   time.Time
}

// AuditEvent is the canonical record of a state-changing operation.
//
// Operation is a dotted noun.verb the writer chose ("transfer.executed",
// "account.frozen") — stable strings audit consumers can filter on.
//
// BeforeState / AfterState carry the aggregate snapshot bracketing the
// change. Inserts have nil Before; deletes have nil After. Both are
// JSON bytes so the writer doesn't have to reason about Go shapes; the
// caller is responsible for canonicalization (typically json.Marshal of
// a stable struct shape).
type AuditEvent struct {
	AuditEnvelope

	AggregateType string
	AggregateID   uuid.UUID
	Operation     string
	BeforeState   []byte
	AfterState    []byte
}

// Common audit aggregate-type tags. Lowercased to distinguish from the
// outbox AggregateType vocabulary (which uses TitleCase) and to give
// audit consumers stable strings they can filter on.
const (
	AuditAggregateTransaction = "transaction"
	AuditAggregateAccount     = "account"
)

// Common operation tags. Add cases as new state-changing flows land.
const (
	AuditOpTransferExecuted = "transfer.executed"
	AuditOpTransferReversed = "transfer.reversed"
)
