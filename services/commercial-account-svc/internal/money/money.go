// Package money holds exact decimal amounts for the commercial plane.
//
// Amounts travel as decimal strings ("49.00") and are stored as Postgres
// NUMERIC. Nothing here, or anywhere in the commercial plane, passes money
// through float64 (ZS-SVC-Q-001 negative path #46).
package money

import (
	"errors"
	"math/big"
	"regexp"
	"strings"
)

// MaxScale is the most fractional digits any commercial amount may carry.
// It matches the scale(x) <= 4 CHECKs in the schema.
const MaxScale = 4

// decimalPattern accepts plain non-negative decimals only: no sign, no
// exponent, no leading zeros, at most 15 integer digits and MaxScale
// fractional digits. "1e3", "+5", "049" and ".5" are refused rather than
// reinterpreted.
var decimalPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,14})(\.[0-9]{1,4})?$`)

var (
	ErrInvalidAmount = errors.New("amount must be a non-negative decimal string with at most 4 fractional digits")
)

// Decimal is an exact non-negative amount: unscaled / 10^scale.
type Decimal struct {
	unscaled *big.Int
	scale    int
	text     string
}

// Parse validates s and returns it as an exact Decimal.
func Parse(s string) (Decimal, error) {
	if !decimalPattern.MatchString(s) {
		return Decimal{}, ErrInvalidAmount
	}
	scale := 0
	digits := s
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		scale = len(s) - dot - 1
		digits = s[:dot] + s[dot+1:]
	}
	u, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return Decimal{}, ErrInvalidAmount
	}
	return Decimal{unscaled: u, scale: scale, text: s}, nil
}

// Scale is the number of fractional digits as written.
func (d Decimal) Scale() int { return d.scale }

// String returns the amount exactly as it was written.
func (d Decimal) String() string { return d.text }

// IsZero reports whether the amount is zero.
func (d Decimal) IsZero() bool { return d.unscaled == nil || d.unscaled.Sign() == 0 }

// Cmp compares d and o numerically, independent of how many fractional
// digits each was written with ("1.5" == "1.50").
func (d Decimal) Cmp(o Decimal) int {
	return d.rat().Cmp(o.rat())
}

// Ceil rounds up to a whole number. Amounts are non-negative, so this is
// "away from zero".
func (d Decimal) Ceil() Decimal { return d.whole(true) }

// Floor rounds down to a whole number.
func (d Decimal) Floor() Decimal { return d.whole(false) }

func (d Decimal) whole(up bool) Decimal {
	if d.unscaled == nil || d.scale == 0 {
		return d
	}
	den := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(d.scale)), nil)
	q, r := new(big.Int).QuoRem(d.unscaled, den, new(big.Int))
	if up && r.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	return Decimal{unscaled: q, scale: 0, text: q.String()}
}

// Rat returns the exact value for arithmetic. Every intermediate result in
// rating and proration stays a big.Rat; rounding happens once, at the end,
// through FormatHalfEven.
func (d Decimal) Rat() *big.Rat { return d.rat() }

// FormatHalfEven rounds r to scale fractional digits, ties to even, and
// formats it with exactly that many digits. Unlike Decimal, the result may be
// negative: a proration net can be a credit.
func FormatHalfEven(r *big.Rat, scale int) string {
	pow := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)
	scaled := new(big.Rat).Mul(r, new(big.Rat).SetInt(pow))
	num, den := scaled.Num(), scaled.Denom()
	neg := num.Sign() < 0
	absNum := new(big.Int).Abs(num)
	q, rem := new(big.Int).QuoRem(absNum, den, new(big.Int))
	switch new(big.Int).Mul(rem, big.NewInt(2)).Cmp(den) {
	case 1:
		q.Add(q, big.NewInt(1))
	case 0:
		if q.Bit(0) == 1 {
			q.Add(q, big.NewInt(1))
		}
	}
	s := q.String()
	if scale > 0 {
		if len(s) <= scale {
			s = strings.Repeat("0", scale-len(s)+1) + s
		}
		s = s[:len(s)-scale] + "." + s[len(s)-scale:]
	}
	if neg && q.Sign() != 0 {
		s = "-" + s
	}
	return s
}

func (d Decimal) rat() *big.Rat {
	if d.unscaled == nil {
		return new(big.Rat)
	}
	den := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(d.scale)), nil)
	return new(big.Rat).SetFrac(d.unscaled, den)
}
