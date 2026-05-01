package domain

import (
	"database/sql/driver"
	"fmt"

	"github.com/cockroachdb/apd/v3"
)

// Currency is an ISO-4217 alphabetic code, e.g. "USD", "EUR", "VND".
// We do not maintain a registry of valid codes here — that's a policy
// concern and lives in a higher layer or config. Validation here is
// limited to "looks like a 3-letter ISO code".
type Currency string

func (c Currency) Valid() bool {
	if len(c) != 3 {
		return false
	}
	for i := range 3 {
		ch := c[i]
		if ch < 'A' || ch > 'Z' {
			return false
		}
	}
	return true
}

func (c Currency) String() string { return string(c) }

// Amount is a signed decimal stored at the precision of the DB column
// (NUMERIC(19,4)). Sign convention: negative = debit, positive = credit.
//
// Backed by cockroachdb/apd/v3 — chosen over shopspring/decimal for its
// pointer-based arithmetic API. shopspring allocates on every op; apd
// allows in-place mutation through a target pointer, which matters at
// 10K TPS where decimal allocs account for a measurable slice of
// per-request GC pressure.
//
// We expose a value-semantic wrapper Amount{} so callers don't have
// to manage apd.Context or pass target pointers explicitly. Each
// method allocates a new Amount; the win is that apd's underlying
// Decimal allocates less than shopspring's per arithmetic op,
// especially for values that fit in inline int64 storage (which our
// NUMERIC(19,4) values do).
type Amount struct {
	d apd.Decimal
}

// ledgerCtx is the canonical apd.Context for all ledger arithmetic.
//
// Precision=22: covers NUMERIC(19,4) (19 significant digits) with
// 3 digits of headroom for intermediate sums.
// Rounding=HalfEven: banker's rounding, the convention financial
// systems use to avoid a systematic bias under repeated rounding.
//
// Trap behavior: leave default (no traps) — we want apd to set
// Condition flags, not panic. Callers that care about loss of
// precision check the returned condition explicitly.
var ledgerCtx = &apd.Context{
	Precision:   22,
	Rounding:    apd.RoundHalfEven,
	MaxExponent: 1000,
	MinExponent: -1000,
}

// ZeroAmount returns Amount(0). Cheap — the zero value of apd.Decimal
// is already 0 with no heap allocation for the coefficient.
func ZeroAmount() Amount {
	return Amount{}
}

// LedgerScale is the fractional precision the ledger maintains —
// NUMERIC(19,4) on the DB side, exponent -4 on the apd side. Every
// Amount that enters the system is quantized to this scale so:
//
//  1. Fingerprints over Amount.String() are stable regardless of
//     input format ("100" and "100.0000" canonicalize identically).
//  2. The on-wire response shape stays consistent.
//  3. Inputs with more than 4 fractional digits are rejected loudly
//     instead of silently rounded — silent precision loss in money
//     is exactly the bug that destroys trust.
const LedgerScale int32 = -4

// NewAmount parses a decimal-as-string into Amount, quantized to
// LedgerScale. Returns an error for malformed input, NaN/Infinity,
// or values with more than 4 fractional digits.
func NewAmount(s string) (Amount, error) {
	a := Amount{}
	if _, _, err := a.d.SetString(s); err != nil {
		return Amount{}, fmt.Errorf("amount: parse %q: %w", s, err)
	}
	if a.d.Form != apd.Finite {
		return Amount{}, fmt.Errorf("amount: %q is not a finite number", s)
	}
	if a.d.Exponent < LedgerScale {
		return Amount{}, fmt.Errorf("amount: %q has more than 4 fractional digits", s)
	}
	if _, err := ledgerCtx.Quantize(&a.d, &a.d, LedgerScale); err != nil {
		return Amount{}, fmt.Errorf("amount: quantize %q: %w", s, err)
	}
	return a, nil
}

// NewAmountFromInt is the convenience constructor for tests and
// internal computations. The result is quantized to LedgerScale —
// `100` and `100.0000` are byte-identical strings.
func NewAmountFromInt(n int64) Amount {
	a := Amount{}
	a.d.SetInt64(n)
	_, _ = ledgerCtx.Quantize(&a.d, &a.d, LedgerScale)
	return a
}

// IsZero reports whether a == 0. Sign-agnostic.
func (a Amount) IsZero() bool { return a.d.IsZero() }

// IsNegative reports whether a < 0.
func (a Amount) IsNegative() bool { return a.d.Sign() < 0 }

// IsPositive reports whether a > 0.
func (a Amount) IsPositive() bool { return a.d.Sign() > 0 }

// String renders the canonical decimal-as-string form. Stable across
// process restarts and across apd patch versions — used as input to
// the request fingerprint (audit hash chain) so deterministic shape
// is load-bearing.
func (a Amount) String() string { return a.d.String() }

// Neg returns -a. Cheap — apd's Neg toggles the sign bit on the
// existing coefficient, no big.Int allocation when coefficient stays
// the same magnitude. Result stays at LedgerScale (negation doesn't
// change exponent).
func (a Amount) Neg() Amount {
	out := Amount{}
	out.d.Neg(&a.d)
	return out
}

// Add returns a + b. Uses ledgerCtx for precision and rounding;
// result is requantized to LedgerScale so all Amount values in the
// system share one canonical exponent.
func (a Amount) Add(b Amount) Amount {
	out := Amount{}
	_, _ = ledgerCtx.Add(&out.d, &a.d, &b.d)
	_, _ = ledgerCtx.Quantize(&out.d, &out.d, LedgerScale)
	return out
}

// Value implements database/sql/driver.Valuer for pgx. Sends as
// canonical string; matches the NUMERIC column representation
// exactly.
func (a Amount) Value() (driver.Value, error) {
	return a.d.String(), nil
}

// Scan implements database/sql.Scanner for pgx. Accepts string
// (PG NUMERIC scan path) and []byte (binary protocol fallback).
func (a *Amount) Scan(src any) error {
	if src == nil {
		*a = Amount{}
		return nil
	}
	var s string
	switch v := src.(type) {
	case string:
		s = v
	case []byte:
		s = string(v)
	default:
		return fmt.Errorf("amount: cannot scan from %T", src)
	}
	if _, _, err := a.d.SetString(s); err != nil {
		return fmt.Errorf("amount: scan %q: %w", s, err)
	}
	return nil
}

// SumAmounts returns the arithmetic sum of the supplied amounts.
// Used by the double-entry verifier to assert SUM == 0 over a tx's
// legs. O(n) Adds; allocations are bounded by n+1 Amount values.
func SumAmounts(amounts ...Amount) Amount {
	total := Amount{}
	for i := range amounts {
		_, _ = ledgerCtx.Add(&total.d, &total.d, &amounts[i].d)
	}
	return total
}
