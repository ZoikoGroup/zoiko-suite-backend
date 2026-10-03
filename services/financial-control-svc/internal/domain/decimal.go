package domain

import (
	"fmt"
	"math/big"
	"strings"
)

// parseNonNegDecimal parses an exact decimal string. Binary floating point is
// prohibited for monetary values (ZS-DATA-001 D06), so amounts cross this
// service as strings and are compared as big.Rat.
func parseNonNegDecimal(s string) (*big.Rat, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return new(big.Rat), nil
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("%q is not a decimal number", s)
	}
	if r.Sign() < 0 {
		return nil, fmt.Errorf("must not be negative")
	}
	// NUMERIC(38,12): refuse more than 12 fractional digits rather than round silently.
	if i := strings.IndexByte(s, '.'); i >= 0 && len(s)-i-1 > 12 {
		return nil, fmt.Errorf("more than 12 fractional digits")
	}
	return r, nil
}

// NormalizeDecimalText renders a decimal string as the canonical form used in
// hashes and APIs ("7.000000000000" -> "7"). Values that are not decimals are
// returned unchanged so a read never fails on display.
func NormalizeDecimalText(s string) string {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	if !ok {
		return s
	}
	return CanonicalAmount(r)
}
