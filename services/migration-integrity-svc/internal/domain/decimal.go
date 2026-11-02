package domain

import (
	"math/big"
	"regexp"
	"strings"
)

// Amounts are handled as exact rationals (math/big.Rat), never float64: a
// control total that drifts by a rounding error is a false mismatch, and one
// that hides a real cent difference is worse.

// MaxAmountDecimals is the scale of the NUMERIC(28,6) columns amounts are
// stored in. A declared amount with more decimals would be silently rounded
// on storage, so it is refused instead.
const MaxAmountDecimals = 6

// decimalRe is a plain signed decimal: "5", "-5", "5.25", "5.", ".5". It refuses
// "1.2.3", "-", ".", "1-2", "--", "1,000" and exponents.
var decimalRe = regexp.MustCompile(`^-?([0-9]+\.?[0-9]*|\.[0-9]+)$`)

// IsDecimal reports whether s (trimmed) is a plain signed decimal.
func IsDecimal(s string) bool {
	return decimalRe.MatchString(strings.TrimSpace(s))
}

// ParseDecimal parses a plain signed decimal into an exact rational.
func ParseDecimal(s string) (*big.Rat, bool) {
	trimmed := strings.TrimSpace(s)
	if !decimalRe.MatchString(trimmed) {
		return nil, false
	}
	value, ok := new(big.Rat).SetString(trimmed)
	return value, ok
}

// decimalPlaces counts the digits after the decimal point of a plain decimal.
func decimalPlaces(s string) int {
	trimmed := strings.TrimSpace(s)
	dot := strings.IndexByte(trimmed, '.')
	if dot < 0 {
		return 0
	}
	return len(trimmed) - dot - 1
}

// FormatDecimal renders an amount canonically: at most MaxAmountDecimals
// decimals, trailing zeros trimmed, but never fewer than two ("3" -> "3.00",
// "300.250000" -> "300.25"). Equal amounts always render identically, which
// the control hash depends on.
func FormatDecimal(value *big.Rat) string {
	text := value.FloatString(MaxAmountDecimals)
	dot := strings.IndexByte(text, '.')
	minimumLength := dot + 3 // keep two decimals
	for len(text) > minimumLength && strings.HasSuffix(text, "0") {
		text = text[:len(text)-1]
	}
	if text == "-0.00" {
		return "0.00"
	}
	return text
}
