// Package audit holds the audit-log primitives: envelope extraction
// from request context, and the canonical-form / hash-chain logic that
// the repository layer wraps with PG persistence.
//
// No infra deps here — this package is pure logic and trivially
// testable. The actual INSERT lives in internal/repository/audit_repo.go
// because it must share a pgx.Tx with the ledger write.
package audit

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/domain"
	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/transport/grpc/interceptors"
)

// ErrEnvelopeIncomplete is returned when EnvelopeFromContext can't find
// the required identity / correlation fields. Audit writes must NOT
// proceed silently with anonymized data — that would defeat the audit
// log's purpose. The caller maps this to a server-side failure (5xx /
// Internal); the request itself was already authenticated by the time
// it reached this point, so this is a wiring bug, not a client problem.
var ErrEnvelopeIncomplete = errors.New("audit: envelope missing required fields")

// EnvelopeFromContext assembles a domain.AuditEnvelope from the request
// context. It pulls:
//
//   - TenantID, ActorSubject, ActorSession from EdgeIdentity claims
//   - RequestID from the RequestID interceptor
//   - TraceID from the active OTel span (empty if no tracer is wired)
//
// OccurredAt is set to time.Now().UTC() at envelope-build time, so all
// audit rows from the same request share a clock-stable timestamp even
// if multiple aggregates change in one tx.
func EnvelopeFromContext(ctx context.Context) (domain.AuditEnvelope, error) {
	claims := interceptors.ClaimsFromContext(ctx)
	if claims == nil || claims.Tenant == "" || claims.Subject == "" {
		return domain.AuditEnvelope{}, ErrEnvelopeIncomplete
	}
	reqID := interceptors.RequestIDFromContext(ctx)
	if reqID == "" {
		return domain.AuditEnvelope{}, ErrEnvelopeIncomplete
	}

	traceID := ""
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		traceID = sc.TraceID().String()
	}

	return domain.AuditEnvelope{
		TenantID:     claims.Tenant,
		ActorSubject: claims.Subject,
		ActorSession: claims.Session,
		RequestID:    reqID,
		TraceID:      traceID,
		OccurredAt:   time.Now().UTC(),
	}, nil
}
