package domain

import (
	"fmt"
	"math/big"
)

// EvaluateTolerance decides Pass, Fail or Indeterminate for one rule outcome against the rule's explicit
// tolerance (ZS-SVC-Z-001 section 5: "explicit numeric or percentage tolerance, scale/rounding semantics and
// population definition").
//
//   - no records evaluated            -> Indeterminate (the absence of evidence is not a pass)
//   - no failures                     -> Pass
//   - failures and no tolerance       -> Fail (strict)
//   - ABSOLUTE: the failure magnitude sum, rounded to the tolerance's scale by its rounding mode, is compared
//     with the tolerance value
//   - PERCENT:  failures as a percentage of the records evaluated, rounded the same way, is compared with it
//
// The measured value is rounded to Scale decimal places (HALF_UP, HALF_EVEN, or DOWN) BEFORE it is compared,
// and the tolerance value is compared exactly as the contract states it. A measured value at or below the
// tolerance passes. Everything is exact rational arithmetic: no floating point is involved.
func EvaluateTolerance(t *Tolerance, rule string, pass, fail int64, magnitudeSum string) (DQResultStatus, error) {
	if pass == 0 && fail == 0 {
		return ResultIndeterminate, nil
	}
	if fail == 0 {
		return ResultPass, nil
	}
	if t == nil {
		return ResultFail, nil
	}
	limit, ok := new(big.Rat).SetString(t.Value)
	if !ok {
		return "", fmt.Errorf("rule %s: tolerance value %q is not a decimal", rule, t.Value)
	}
	var measured *big.Rat
	switch t.Kind {
	case "ABSOLUTE":
		sum, ok := new(big.Rat).SetString(magnitudeSum)
		if !ok {
			return "", fmt.Errorf("rule %s: failure_magnitude_sum %q is not a decimal", rule, magnitudeSum)
		}
		measured = sum
	case "PERCENT":
		total := new(big.Rat).SetInt64(pass + fail)
		measured = new(big.Rat).Quo(new(big.Rat).Mul(new(big.Rat).SetInt64(fail), big.NewRat(100, 1)), total)
	default:
		return "", fmt.Errorf("rule %s: unknown tolerance kind %q", rule, t.Kind)
	}
	rounded, err := roundTo(measured, t.Scale, t.Rounding)
	if err != nil {
		return "", fmt.Errorf("rule %s: %w", rule, err)
	}
	if rounded.Cmp(limit) > 0 {
		return ResultFail, nil
	}
	return ResultPass, nil
}

// roundTo rounds a non-negative rational to scale decimal places. Measured values are never negative (a
// failure count and a magnitude sum), so HALF_UP and "away from zero" coincide.
func roundTo(v *big.Rat, scale int, mode string) (*big.Rat, error) {
	if v.Sign() < 0 {
		return nil, fmt.Errorf("a measured value cannot be negative")
	}
	if scale < 0 || scale > 18 {
		return nil, fmt.Errorf("tolerance scale %d is outside 0..18", scale)
	}
	pow := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)
	scaled := new(big.Rat).Mul(v, new(big.Rat).SetInt(pow))
	floor := new(big.Int).Quo(scaled.Num(), scaled.Denom()) // non-negative, so truncation is floor
	frac := new(big.Rat).Sub(scaled, new(big.Rat).SetInt(floor))
	half := big.NewRat(1, 2)
	up := false
	switch mode {
	case "DOWN":
	case "HALF_UP":
		up = frac.Cmp(half) >= 0
	case "HALF_EVEN":
		switch frac.Cmp(half) {
		case 1:
			up = true
		case 0:
			up = floor.Bit(0) == 1 // exactly half: round to the even neighbour
		}
	default:
		return nil, fmt.Errorf("unknown rounding mode %q", mode)
	}
	if up {
		floor.Add(floor, big.NewInt(1))
	}
	return new(big.Rat).SetFrac(floor, pow), nil
}
