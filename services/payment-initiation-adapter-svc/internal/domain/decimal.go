package domain

import (
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

// ParseAmount parses an exact decimal string. Binary floating point is
// prohibited for monetary values (ZS-DATA-001 D06): amounts are carried as
// decimal text and compared as big.Rat, never float64.
//
// Same pattern as intercompany-accounting-svc's internal/domain/decimal.go.
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

// jsonNumberRe is the JSON number grammar (RFC 8259 §6). big.Rat.SetString
// also accepts fractions ("1/3"), which are not amounts; this rejects them.
var jsonNumberRe = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

// Decimal is an exact monetary amount carried as decimal text.
//
// On the wire it stays a JSON number, as Amount always has been: every
// caller (payment-run-svc, payroll-run-svc, treasury-svc, accounts-receivable-svc)
// sends a bare number today, and a reader decoding the attempt into a number
// type keeps working. What changes is that this service never routes it
// through float64: the digits read are kept exactly, so 10.005 arrives as
// 10.005 and is refused, not rounded to 10.01 first. A quoted string is
// accepted on input as well, so callers can move to the string form
// ZS-DATA-001 D06 prefers.
type Decimal string

// Rat parses d for exact arithmetic.
func (d Decimal) Rat() (*big.Rat, error) { return ParseAmount(string(d)) }

// MarshalJSON writes d as a JSON number literal of exactly its digits.
// An unset Decimal is written as null.
func (d Decimal) MarshalJSON() ([]byte, error) {
	if d == "" {
		return []byte("null"), nil
	}
	r, err := d.Rat()
	if err != nil {
		return nil, fmt.Errorf("marshal amount: %w", err)
	}
	return []byte(CanonicalAmount(r)), nil
}

// UnmarshalJSON accepts a JSON number or a string holding one, and keeps its
// canonical decimal form. null leaves d unset.
func (d *Decimal) UnmarshalJSON(data []byte) error {
	s := strings.TrimSpace(string(data))
	if s == "null" {
		*d = ""
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		s = strings.TrimSpace(s)
	}
	if !jsonNumberRe.MatchString(s) {
		return fmt.Errorf("amount %q is not a decimal number", s)
	}
	r, err := ParseAmount(s)
	if err != nil {
		return err
	}
	*d = Decimal(CanonicalAmount(r))
	return nil
}

// Scan reads a NUMERIC selected as text (the store always selects
// amount::text, so pgx never materialises a float on the way).
func (d *Decimal) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*d = ""
	case string:
		*d = Decimal(v)
	case []byte:
		*d = Decimal(v)
	default:
		return fmt.Errorf("cannot scan %T into Decimal", src)
	}
	return nil
}

// amountPlaces is NUMERIC(18,4)'s scale (migration 000009 widened it from
// NUMERIC(18,2)): the column's own storage CEILING, applied uniformly to
// every currency, same as treasury-svc's MoneyScale — not a per-currency
// rule. No real ISO 4217 currency needs more than 4 decimal places. The
// actual per-currency rule (JPY=0, USD/GBP=2, BHD=3, ...) is
// ValidateCurrencyPrecision below, which needs the currency's real
// minor-unit count from currency-registry-svc and so cannot live here.
const amountPlaces = 4

var (
	// amountScale is NUMERIC(18,4)'s scale: amounts with more fractional
	// digits would be silently rounded by Postgres on insert.
	amountScale = new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(amountPlaces)), nil))
	// amountLimit is NUMERIC(18,4)'s bound: 14 integer digits.
	amountLimit = new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(18-int64(amountPlaces)), nil))
)

// ValidatePositiveAmount checks that d is a positive amount the
// NUMERIC(18,4) amount column stores exactly, with no rounding and no
// overflow. A value that would be rounded is refused rather than quietly
// changed, so the stored amount, the amount sent to the provider and the
// amount the caller authorized are one number (invariant 13).
//
// This is a column-capacity check only — it says nothing about whether a
// SPECIFIC currency actually allows that many decimal places (a 3-decimal
// amount fits this column for every currency, but is only valid for a
// 3-decimal currency like BHD). Callers that know the currency must also
// call ValidateCurrencyPrecision.
func ValidatePositiveAmount(d Decimal) (*big.Rat, error) {
	r, err := d.Rat()
	if err != nil {
		return nil, ErrInvalidAmount
	}
	if r.Sign() <= 0 {
		return nil, ErrInvalidAmount
	}
	if !new(big.Rat).Mul(r, amountScale).IsInt() || r.Cmp(amountLimit) >= 0 {
		return nil, ErrAmountPrecision
	}
	return r, nil
}

// ColumnText renders a validated amount the way the amount column holds it
// and amount::text reads it back ("10.5000"), so a check made before the
// row exists sees the same text the provider-bound path later hashes and
// sends.
func ColumnText(r *big.Rat) string { return r.FloatString(amountPlaces) }

// ErrAmountExceedsCurrencyPrecision is returned when an amount's actual
// decimal places exceed its currency's real ISO 4217 minor-unit count (e.g.
// a JPY amount with any fractional part, or a USD amount with 3+ decimal
// places). Distinct from ErrAmountPrecision above, which only guards this
// column's own storage ceiling (4), identical for every currency.
var ErrAmountExceedsCurrencyPrecision = sentinel("amount has more decimal places than this currency's minor-unit precision allows")

// terminatingScale returns the number of fractional digits needed to write
// denom's reciprocal as a terminating decimal, and whether it terminates.
// Decimal inputs reduce to 2^a * 5^b, so this is exact for every value
// ParseAmount admits and for sums/products of such values.
func terminatingScale(denom *big.Int) (int, bool) {
	n := new(big.Int).Set(denom)
	twos, fives := 0, 0
	two := big.NewInt(2)
	five := big.NewInt(5)
	mod := new(big.Int)
	div := new(big.Int)
	for n.Cmp(big.NewInt(1)) > 0 {
		div.QuoRem(n, two, mod)
		if mod.Sign() == 0 {
			n.Set(div)
			twos++
			continue
		}
		div.QuoRem(n, five, mod)
		if mod.Sign() == 0 {
			n.Set(div)
			fives++
			continue
		}
		break
	}
	if n.Cmp(big.NewInt(1)) != 0 {
		return 0, false
	}
	if twos > fives {
		return twos, true
	}
	return fives, true
}

// DecimalPlaces returns the minimal number of fractional digits needed to
// render r exactly as a terminating decimal (0 for an integer). It is the
// actual precision the caller used, not the column's capacity — "7.50" is
// 2 places by column scale but only needs 1 to be written exactly.
func DecimalPlaces(r *big.Rat) int {
	if r.IsInt() {
		return 0
	}
	scale, exact := terminatingScale(r.Denom())
	if !exact {
		// ParseAmount/jsonNumberRe refuse non-terminating fractions, so this
		// is unreachable for any value this package itself parsed.
		return amountPlaces
	}
	return scale
}

// ValidateCurrencyPrecision refuses r if it uses more decimal places than
// minorUnit allows for its real currency (e.g. minorUnit=0 for JPY, 2 for
// USD/GBP, 3 for BHD). r is assumed already validated by
// ValidatePositiveAmount — this only adds the currency-specific precision
// rule on top.
func ValidateCurrencyPrecision(r *big.Rat, minorUnit int) error {
	if DecimalPlaces(r) > minorUnit {
		return ErrAmountExceedsCurrencyPrecision
	}
	return nil
}
