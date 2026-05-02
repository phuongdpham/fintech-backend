package domain

import "testing"

func TestAmount_IsZero(t *testing.T) {
	cases := []struct {
		in  Amount
		out bool
	}{
		{"", true},
		{"0", true},
		{"0.0", true},
		{"0.00", true},
		{"0.000", true},
		{"0.0000", true},
		{"1", false},
		{"0.0001", false},
		{"100.0000", false},
	}
	for _, tc := range cases {
		t.Run(string(tc.in), func(t *testing.T) {
			if got := tc.in.IsZero(); got != tc.out {
				t.Fatalf("IsZero: got %v, want %v", got, tc.out)
			}
		})
	}
}

func TestCurrency_Valid(t *testing.T) {
	cases := []struct {
		in  Currency
		out bool
	}{
		{"USD", true},
		{"EUR", true},
		{"GBP", true},
		{"", false},
		{"US", false},
		{"USDD", false},
		{"usd", false},
		{"123", false},
		{"U$D", false},
	}
	for _, tc := range cases {
		t.Run(string(tc.in), func(t *testing.T) {
			if got := tc.in.Valid(); got != tc.out {
				t.Fatalf("Valid: got %v, want %v", got, tc.out)
			}
		})
	}
}
