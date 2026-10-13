package domain

import (
	"fmt"
	"math/big"
	"strings"
)

// ParseAmount parses an exact decimal string. Binary floating point is
// prohibited for monetary values (ZS-DATA-001 D06): amounts cross this
// service as strings and are compared as big.Rat, never float64.
func ParseAmount(s string) (*big.Rat, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("amount is empty")
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("%q is not a decimal number", s)
	}
	return r, nil
}

// SumAmounts adds a list of exact-decimal strings, failing on the first
// unparseable value (callers are expected to have validated already).
func SumAmounts(amounts []string) (*big.Rat, error) {
	total := new(big.Rat)
	for _, a := range amounts {
		r, err := ParseAmount(a)
		if err != nil {
			return nil, err
		}
		total.Add(total, r)
	}
	return total, nil
}

// CanonicalAmount renders r as a plain decimal string (never scientific
// notation), trimming trailing fractional zeros so "7.00" and "7" compare
// and print identically.
func CanonicalAmount(r *big.Rat) string {
	if r.IsInt() {
		return r.RatString()
	}
	s := r.FloatString(12)
	s = strings.TrimRight(s, "0")
	s = strings.TrimSuffix(s, ".")
	return s
}
