package domain

// Amount is a decimal money value. Wire format is decimal-as-string;
// PG storage is NUMERIC(19,4). Held as a string at the gateway
// boundary because gateway-svc passes amounts through to processors
// and back without doing arithmetic — promoting to apd.Decimal would
// be premature complexity here.
//
// If FX or fee math lands in gateway-svc later, this type promotes
// to a wrapper around apd.Decimal at scale -4 (matching ledger-svc's
// LedgerScale convention). The wire format stays the same.
//
// Validation lives at the boundary (gRPC + provider clients), not on
// the type itself — gateway is a pass-through; the authoritative
// validation already happened at the BFF / ledger-svc edge.
type Amount string

func (a Amount) String() string { return string(a) }

// IsZero reports whether the amount is missing or canonically zero.
// Treat any of "" / "0" / "0.0" / "0.0000" as zero — exact decimal
// equality at fixed scale isn't load-bearing for gateway, just
// "is there a non-zero amount to send."
func (a Amount) IsZero() bool {
	switch a {
	case "", "0", "0.0", "0.00", "0.000", "0.0000":
		return true
	}
	return false
}

// Currency is an ISO-4217 three-letter code. v1 lives in USD; the
// type is here to make the boundary explicit and the v2 multi-
// currency expansion mechanical.
type Currency string

func (c Currency) String() string { return string(c) }

// Valid checks the surface shape (length 3). The provider-side
// boundary does the real "is this code recognized by the processor"
// check — there's no point duplicating that table here.
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
