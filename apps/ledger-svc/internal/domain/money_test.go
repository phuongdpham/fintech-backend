package domain_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/domain"
)

// TestNewAmount_Canonicalization is the load-bearing test for the
// fingerprint stability guarantee: "100" and "100.0000" MUST stringify
// identically after parse, otherwise the audit hash chain breaks for
// equal-value-different-format replays.
func TestNewAmount_Canonicalization(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"100", "100.0000"},
		{"100.0", "100.0000"},
		{"100.00", "100.0000"},
		{"100.0000", "100.0000"},
		{"0", "0.0000"},
		{"-100", "-100.0000"},
		{"-100.5", "-100.5000"},
		{"0.0001", "0.0001"},
		{"9999999999999999.9999", "9999999999999999.9999"}, // NUMERIC(19,4) max
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			a, err := domain.NewAmount(tc.in)
			require.NoError(t, err)
			require.Equal(t, tc.want, a.String())
		})
	}
}

func TestNewAmount_RejectsExcessivePrecision(t *testing.T) {
	cases := []string{
		"100.00001",
		"0.12345",
		"-1.99999",
	}
	for _, s := range cases {
		t.Run(s, func(t *testing.T) {
			_, err := domain.NewAmount(s)
			require.Error(t, err)
			require.Contains(t, err.Error(), "fractional digits")
		})
	}
}

func TestNewAmount_RejectsMalformed(t *testing.T) {
	cases := []string{
		"",
		"abc",
		"1.2.3",
		"NaN",
		"Infinity",
		"--5",
	}
	for _, s := range cases {
		t.Run(strings.ReplaceAll(s, "", "<empty>"), func(t *testing.T) {
			_, err := domain.NewAmount(s)
			require.Error(t, err)
		})
	}
}

func TestAmount_SignChecks(t *testing.T) {
	cases := []struct {
		in           string
		isZero       bool
		isPositive   bool
		isNegative   bool
	}{
		{"0", true, false, false},
		{"100", false, true, false},
		{"-100", false, false, true},
		{"0.0001", false, true, false},
		{"-0.0001", false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			a, err := domain.NewAmount(tc.in)
			require.NoError(t, err)
			require.Equal(t, tc.isZero, a.IsZero())
			require.Equal(t, tc.isPositive, a.IsPositive())
			require.Equal(t, tc.isNegative, a.IsNegative())
		})
	}
}

func TestAmount_Add_Neg(t *testing.T) {
	a, _ := domain.NewAmount("100.5000")
	b, _ := domain.NewAmount("-50.5000")
	sum := a.Add(b)
	require.Equal(t, "50.0000", sum.String())

	neg := a.Neg()
	require.Equal(t, "-100.5000", neg.String())

	zero := a.Add(neg)
	require.True(t, zero.IsZero())
}

func TestAmount_FingerprintStable(t *testing.T) {
	// The whole point of canonicalization: same logical value, same
	// String(), same fingerprint downstream.
	for _, equivalent := range []struct{ a, b string }{
		{"100", "100.0000"},
		{"100.0", "100.0000"},
		{"-50.5", "-50.5000"},
		{"0", "0.0000"},
	} {
		t.Run(equivalent.a+"=="+equivalent.b, func(t *testing.T) {
			a, _ := domain.NewAmount(equivalent.a)
			b, _ := domain.NewAmount(equivalent.b)
			require.Equal(t, a.String(), b.String(),
				"Amount canonicalization broken — fingerprints would diverge for the same logical value")
		})
	}
}

// BenchmarkAmount_AddChain exercises the typical SUM-zero verification
// path. SumAmounts is what AssertBalanced calls under the hood.
func BenchmarkAmount_AddChain(b *testing.B) {
	a, _ := domain.NewAmount("100.0000")
	c, _ := domain.NewAmount("-100.0000")
	b.ReportAllocs()
	for b.Loop() {
		_ = domain.SumAmounts(a, c)
	}
}

// BenchmarkAmount_NewFromString exercises parse+quantize, the hottest
// per-request decimal allocation site (one call per Transfer's amount).
func BenchmarkAmount_NewFromString(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_, _ = domain.NewAmount("100.0000")
	}
}

// BenchmarkAmount_Neg measures the cost of building the debit leg
// (Amount.Neg) per Transfer. Cheap — sign-bit toggle.
func BenchmarkAmount_Neg(b *testing.B) {
	a, _ := domain.NewAmount("100.0000")
	b.ReportAllocs()
	for b.Loop() {
		_ = a.Neg()
	}
}
