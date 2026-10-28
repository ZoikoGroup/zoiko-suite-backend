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

const amountPlaces = 2

var (
	// amountScale is NUMERIC(18,2)'s scale: amounts with more fractional
	// digits would be silently rounded by Postgres on insert.
	amountScale = big.NewRat(100, 1)
	// amountLimit is NUMERIC(18,2)'s bound: 16 integer digits.
	amountLimit = new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(16), nil))
)

// ValidatePositiveAmount checks that d is a positive amount the
// NUMERIC(18,2) amount column stores exactly, with no rounding and no
// overflow. A value that would be rounded is refused rather than quietly
// changed, so the stored amount, the amount sent to the provider and the
// amount the caller authorized are one number (invariant 13).
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
// and amount::text reads it back ("10.50"), so a check made before the row
// exists sees the same text the provider-bound path later hashes and sends.
func ColumnText(r *big.Rat) string { return r.FloatString(amountPlaces) }
