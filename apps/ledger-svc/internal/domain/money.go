package domain

import "github.com/shopspring/decimal"

// Currency is an ISO-4217 alphabetic code, e.g. "USD", "EUR", "VND".
// We do not maintain a registry of valid codes here — that's a policy
// concern and lives in a higher layer or config. Validation here is
// limited to "looks like a 3-letter ISO code".
type Currency string

func (c Currency) Valid() bool {
	if len(c) != 3 {
		return false
	}
	for i := 0; i < 3; i++ {
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
// Aliasing decimal.Decimal as a named type keeps domain code free of
// shopspring import noise at call sites and gives us a single place to
// hang ledger-specific helpers (e.g. SumZero).
type Amount = decimal.Decimal

// SumAmounts returns the arithmetic sum of the supplied amounts.
// Used by the double-entry verifier to assert SUM == 0 over a tx's legs.
func SumAmounts(amounts ...Amount) Amount {
	total := decimal.Zero
	for _, a := range amounts {
		total = total.Add(a)
	}
	return total
}
