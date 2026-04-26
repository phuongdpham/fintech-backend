// Package domain contains the ledger's business entities, value objects,
// invariants, sentinel errors, and the port interfaces consumed by usecases.
//
// Dependency rule: this package may NOT import any I/O, framework, or
// driver code (no pgx, no Redis, no Kafka, no HTTP). It is allowed to
// depend on pure value-type libraries — currently:
//
//   - github.com/google/uuid       (RFC-4122 UUIDs)
//   - github.com/shopspring/decimal (arbitrary-precision decimals)
//
// Both are header-only value types with no runtime I/O. Keeping them here
// avoids reinventing primitives that every adapter would otherwise have
// to convert at the boundary.
package domain
