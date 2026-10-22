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
// Same pattern as financial-statements-svc's internal/domain/decimal.go.
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

// jsonNumberRe is the JSON number grammar (RFC 8259 §6). big.Rat.SetString
// also accepts fractions ("1/3"), which are not amounts; this rejects them.
var jsonNumberRe = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

// Decimal is an exact monetary amount carried as decimal text, the same
// type intercompany-accounting-svc uses for the amounts it serves.
//
// It decodes a JSON number as well as a quoted string: intercompany-
// accounting-svc writes amount as a JSON number literal of the exact stored
// digits (so float-decoding consumers keep working), and a plain Go string
// field would refuse that. The digits are kept exactly; nothing passes
// through float64.
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
// canonical decimal form. null leaves d unset. Anything else is an error, so
// a malformed upstream amount fails the read instead of becoming zero.
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
