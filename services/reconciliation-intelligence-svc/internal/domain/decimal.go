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

// Amount columns are NUMERIC(18,4). Postgres rounds a fifth fractional digit
// away without error, so anything that would not survive storage exactly is
// rejected up front instead of being silently changed.
const (
	maxAmountScale         = 4
	maxAmountIntegerDigits = 18 - maxAmountScale
)

var (
	amountScaleUnit = new(big.Int).Exp(big.NewInt(10), big.NewInt(maxAmountScale), nil)
	amountMagnitude = new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(maxAmountIntegerDigits), nil))
)

// ParseStorableAmount parses s and checks it is stored exactly by a
// NUMERIC(18,4) column: at most 4 fractional digits and |value| < 10^14.
func ParseStorableAmount(s string) (*big.Rat, error) {
	r, err := ParseAmount(s)
	if err != nil {
		return nil, err
	}
	if new(big.Int).Rem(amountScaleUnit, r.Denom()).Sign() != 0 {
		return nil, fmt.Errorf("%q has more than %d decimal places", s, maxAmountScale)
	}
	if new(big.Rat).Abs(r).Cmp(amountMagnitude) >= 0 {
		return nil, fmt.Errorf("%q exceeds %d integer digits", s, maxAmountIntegerDigits)
	}
	return r, nil
}
