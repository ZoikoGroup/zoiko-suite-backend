package store

import "testing"

func TestAmountDirection(t *testing.T) {
	cases := map[string]string{
		"500.0000":   "CREDIT",
		"0.0001":     "CREDIT",
		"-200.0000":  "DEBIT",
		"-0.0001":    "DEBIT",
		"0.0000":     "ZERO",
		"0":          "ZERO",
		"-0.0000":    "ZERO",
		"1000000.50": "CREDIT",
		"-10.0000":   "DEBIT",
	}
	for in, want := range cases {
		if got := amountDirection(in); got != want {
			t.Errorf("amountDirection(%q) = %q, want %q", in, got, want)
		}
	}
}