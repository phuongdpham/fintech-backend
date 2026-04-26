// Package finance houses double-entry accounting primitives:
// money, accounts, postings, and balanced transactions.
//
// Invariant: every transaction's debit and credit legs sum to zero
// in their settlement currency. Persistence layers should reject
// any transaction that fails this invariant before commit.
package finance

// Unit returns the smallest currency subdivision used internally.
// All monetary values are stored as integer minor units to avoid
// floating-point rounding loss.
func Unit() string { return "minor-unit (integer)" }
