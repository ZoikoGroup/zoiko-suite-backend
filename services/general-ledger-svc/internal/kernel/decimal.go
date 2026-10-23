package kernel

import (
	"math/big"
	"regexp"
	"strings"
)

// Authoritative accounting arithmetic is exact (AK-INV-007): decimals are big.Rat parsed from decimal strings, never
// float64, and rounding happens only at the stages the rule names.

var decimalRE = regexp.MustCompile(`^-?[0-9]{1,30}(\.[0-9]{1,18})?$`)

// ParseDecimal parses a plain decimal string ("12.50", "-3", "0.0001"). Exponents, fractions and separators are refused.
func ParseDecimal(s string) (*big.Rat, bool) {
	s = strings.TrimSpace(s)
	if !decimalRE.MatchString(s) {
		return nil, false
	}
	r, ok := new(big.Rat).SetString(s)
	return r, ok
}

// Rounding modes a rounding policy may name.
const (
	HalfEven = "HALF_EVEN"
	HalfUp   = "HALF_UP" // half away from zero
	Down     = "DOWN"    // toward zero
	Up       = "UP"      // away from zero
)

// RoundingPolicy is the controlled rounding of a rule set: the mode, and the account and tolerance a residual may be
// posted to (standard §35). Policies are named, versioned reference data; the built-in set below is the minimum every
// deployment has.
type RoundingPolicy struct {
	ID   string `json:"id"`
	Mode string `json:"mode"`
	// ResidualTolerance is the largest residual, in minor units of the journal currency, that may be posted through
	// the rounding account instead of failing the journal. Zero means no residual is ever posted.
	ResidualTolerance int `json:"residual_tolerance_minor_units"`
}

// BuiltinRoundingPolicies are always available.
var BuiltinRoundingPolicies = map[string]RoundingPolicy{
	"HALF_EVEN":          {ID: "HALF_EVEN", Mode: HalfEven},
	"HALF_UP":            {ID: "HALF_UP", Mode: HalfUp},
	"HALF_EVEN_RESIDUAL": {ID: "HALF_EVEN_RESIDUAL", Mode: HalfEven, ResidualTolerance: 2},
	"HALF_UP_RESIDUAL":   {ID: "HALF_UP_RESIDUAL", Mode: HalfUp, ResidualTolerance: 2},
}

var ten = big.NewInt(10)

func pow10(n int) *big.Int { return new(big.Int).Exp(ten, big.NewInt(int64(n)), nil) }

// RoundMinor rounds x to a whole number of minor units at the given scale and returns that integer (units of 10^-scale).
func RoundMinor(x *big.Rat, scale int, mode string) *big.Int {
	scaled := new(big.Rat).Mul(x, new(big.Rat).SetInt(pow10(scale)))
	num, den := scaled.Num(), scaled.Denom()
	neg := num.Sign() < 0
	n := new(big.Int).Abs(num)
	q, rem := new(big.Int).QuoRem(n, den, new(big.Int))
	if rem.Sign() != 0 {
		twice := new(big.Int).Mul(rem, big.NewInt(2))
		cmp := twice.Cmp(den)
		switch mode {
		case Down:
		case Up:
			q.Add(q, big.NewInt(1))
		case HalfUp:
			if cmp >= 0 {
				q.Add(q, big.NewInt(1))
			}
		default: // HALF_EVEN
			if cmp > 0 || (cmp == 0 && q.Bit(0) == 1) {
				q.Add(q, big.NewInt(1))
			}
		}
	}
	if neg {
		q.Neg(q)
	}
	return q
}

// FormatMinor renders minor units as a plain decimal string at the given scale.
func FormatMinor(minor *big.Int, scale int) string {
	neg := minor.Sign() < 0
	s := new(big.Int).Abs(minor).String()
	if scale > 0 {
		for len(s) <= scale {
			s = "0" + s
		}
		s = s[:len(s)-scale] + "." + s[len(s)-scale:]
	}
	if neg {
		s = "-" + s
	}
	return s
}

// ratString renders an exact rate or intermediate to at most 18 decimals, trimming trailing zeros.
func ratString(x *big.Rat) string {
	s := x.FloatString(18)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	if s == "" || s == "-" {
		return "0"
	}
	return s
}
