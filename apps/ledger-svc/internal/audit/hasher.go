package audit

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strconv"

	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/domain"
)

// GenesisHash is the per-tenant chain anchor: a deterministic 32-byte
// sentinel used as prev_hash for the first audit row of every tenant.
// All-zeros is the convention for tamper-evident logs (CT, Sigstore Rekor)
// — auditors verify the chain by walking forward from this known value.
var GenesisHash = make([]byte, sha256.Size)

// CanonicalBytes serializes an AuditEvent into a stable byte sequence
// for hashing. The format is hand-rolled, not JSON, because:
//
//  1. JSON field-order isn't guaranteed across runtimes — two valid
//     marshalers can produce different bytes for the same data.
//  2. The byte sequence becomes part of the chain integrity proof; we
//     want a format that's reproducible from raw fields by any external
//     auditor running their own implementation.
//
// Field encoding: each variable-length value is prefixed with a 4-byte
// big-endian length, then the raw bytes. Order is locked below — never
// reorder, never insert, only append (with a version byte if the schema
// ever changes meaningfully). Primitive numbers go in fixed-width form.
func CanonicalBytes(e domain.AuditEvent, prevHash []byte) []byte {
	buf := make([]byte, 0, 512)
	buf = appendBytes(buf, prevHash)
	buf = appendString(buf, e.TenantID)
	buf = appendString(buf, e.ActorSubject)
	buf = appendString(buf, e.ActorSession)
	buf = appendString(buf, e.RequestID)
	buf = appendString(buf, e.TraceID)
	buf = appendString(buf, strconv.FormatInt(e.OccurredAt.UnixNano(), 10))
	buf = appendString(buf, e.AggregateType)
	buf = appendBytes(buf, e.AggregateID[:])
	buf = appendString(buf, e.Operation)
	buf = appendBytes(buf, e.BeforeState)
	buf = appendBytes(buf, e.AfterState)
	return buf
}

// EntryHash computes the SHA-256 chain hash for e, using prevHash as
// the previous link. SHA-256 has hardware acceleration (SHA-NI) on
// every modern x86_64 / arm64 server CPU; even at 5K TPS this is well
// under 1% of the per-request budget.
func EntryHash(e domain.AuditEvent, prevHash []byte) [sha256.Size]byte {
	return sha256.Sum256(CanonicalBytes(e, prevHash))
}

// EntryHashHex is a convenience for places that want the hex form.
func EntryHashHex(e domain.AuditEvent, prevHash []byte) string {
	h := EntryHash(e, prevHash)
	return hex.EncodeToString(h[:])
}

func appendBytes(buf, b []byte) []byte {
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(b)))
	return append(buf, b...)
}

func appendString(buf []byte, s string) []byte {
	return appendBytes(buf, []byte(s))
}
