package audit_test

import (
	"crypto/sha256"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/audit"
	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/domain"
)

func sampleEvent() domain.AuditEvent {
	return domain.AuditEvent{
		AuditEnvelope: domain.AuditEnvelope{
			TenantID:     "tenant-a",
			ActorSubject: "alice",
			ActorSession: "sess-1",
			RequestID:    "req-001",
			TraceID:      "0123456789abcdef0123456789abcdef",
			OccurredAt:   time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		},
		AggregateType: domain.AuditAggregateTransaction,
		AggregateID:   uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Operation:     domain.AuditOpTransferExecuted,
		BeforeState:   nil,
		AfterState:    []byte(`{"id":"11111111-1111-1111-1111-111111111111","status":"COMMITTED"}`),
	}
}

func TestEntryHash_Determinism(t *testing.T) {
	e := sampleEvent()
	prev := audit.GenesisHash

	h1 := audit.EntryHash(e, prev)
	h2 := audit.EntryHash(e, prev)
	require.Equal(t, h1, h2, "EntryHash must be deterministic for identical inputs")
}

func TestEntryHash_PrevHashAffectsResult(t *testing.T) {
	e := sampleEvent()

	h1 := audit.EntryHash(e, audit.GenesisHash)
	otherPrev := make([]byte, sha256.Size)
	otherPrev[0] = 1
	h2 := audit.EntryHash(e, otherPrev)

	require.NotEqual(t, h1, h2, "different prev_hash MUST produce different entry_hash — chain breaks otherwise")
}

func TestEntryHash_FieldsAffectResult(t *testing.T) {
	base := sampleEvent()
	cases := []struct {
		name   string
		mutate func(e *domain.AuditEvent)
	}{
		{"tenant", func(e *domain.AuditEvent) { e.TenantID = "tenant-b" }},
		{"actor_subject", func(e *domain.AuditEvent) { e.ActorSubject = "bob" }},
		{"actor_session", func(e *domain.AuditEvent) { e.ActorSession = "sess-2" }},
		{"request_id", func(e *domain.AuditEvent) { e.RequestID = "req-002" }},
		{"trace_id", func(e *domain.AuditEvent) { e.TraceID = "deadbeefdeadbeefdeadbeefdeadbeef" }},
		{"occurred_at", func(e *domain.AuditEvent) { e.OccurredAt = e.OccurredAt.Add(time.Second) }},
		{"aggregate_type", func(e *domain.AuditEvent) { e.AggregateType = "account" }},
		{"aggregate_id", func(e *domain.AuditEvent) { e.AggregateID = uuid.New() }},
		{"operation", func(e *domain.AuditEvent) { e.Operation = "transfer.reversed" }},
		{"before_state", func(e *domain.AuditEvent) { e.BeforeState = []byte(`{"x":1}`) }},
		{"after_state", func(e *domain.AuditEvent) { e.AfterState = []byte(`{"y":2}`) }},
	}
	baseHash := audit.EntryHash(base, audit.GenesisHash)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := base
			tc.mutate(&e)
			h := audit.EntryHash(e, audit.GenesisHash)
			require.NotEqual(t, baseHash, h, "mutating %s must change entry_hash", tc.name)
		})
	}
}

func TestEntryHash_ChainIntegrity(t *testing.T) {
	// Walk a 5-event chain forward, then verify every link by recomputing.
	chain := []domain.AuditEvent{
		sampleEvent(),
		mutated(sampleEvent(), func(e *domain.AuditEvent) { e.RequestID = "req-002" }),
		mutated(sampleEvent(), func(e *domain.AuditEvent) { e.RequestID = "req-003" }),
		mutated(sampleEvent(), func(e *domain.AuditEvent) { e.RequestID = "req-004" }),
		mutated(sampleEvent(), func(e *domain.AuditEvent) { e.RequestID = "req-005" }),
	}
	hashes := make([][]byte, len(chain))
	prev := audit.GenesisHash
	for i, e := range chain {
		h := audit.EntryHash(e, prev)
		hashes[i] = h[:]
		prev = h[:]
	}

	// Forward verification: recomputing each hash from its predecessor
	// must match the recorded hash. A tampered row anywhere in the chain
	// makes everything downstream verify-fail.
	verifyPrev := audit.GenesisHash
	for i, e := range chain {
		got := audit.EntryHash(e, verifyPrev)
		require.Equal(t, hashes[i], got[:],
			"chain link %d: recomputed hash diverges — tamper or encoding bug", i)
		verifyPrev = got[:]
	}

	// Tamper detection: change one byte of any earlier row's after_state,
	// recomputing forward must produce a different final hash.
	tampered := chain
	tampered[2].AfterState = []byte(`{"tampered":true}`)
	tamperedPrev := audit.GenesisHash
	tamperedHashes := make([][]byte, len(tampered))
	for i, e := range tampered {
		h := audit.EntryHash(e, tamperedPrev)
		tamperedHashes[i] = h[:]
		tamperedPrev = h[:]
	}
	require.NotEqual(t, hashes[len(hashes)-1], tamperedHashes[len(tamperedHashes)-1],
		"tampering an earlier row MUST change all downstream hashes")
}

func mutated(e domain.AuditEvent, fn func(*domain.AuditEvent)) domain.AuditEvent {
	fn(&e)
	return e
}

func TestGenesisHash_Shape(t *testing.T) {
	require.Len(t, audit.GenesisHash, sha256.Size, "genesis must be SHA-256-shaped")
	zero := make([]byte, sha256.Size)
	require.Equal(t, zero, audit.GenesisHash, "genesis must be all-zeros sentinel")
}
