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

// ExactDecimal renders r as a Decimal without losing anything: a value
// CanonicalAmount's 12-place rendering would round is refused instead of
// returned as a different number. Every amount a NUMERIC(18,4) column can
// hold, and any sum or difference of such amounts, renders exactly; the
// guard exists so no reader can quietly introduce a rounding path on the way
// out (SPEC_DEVIATIONS A1/B10).
func ExactDecimal(r *big.Rat) (Decimal, error) {
	d := Decimal(CanonicalAmount(r))
	back, err := d.Rat()
	if err != nil || back.Cmp(r) != 0 {
		return "", fmt.Errorf("amount %s has no exact decimal rendering", r.RatString())
	}
	return d, nil
}

// jsonNumberRe is the JSON number grammar (RFC 8259 §6). big.Rat.SetString
// also accepts fractions ("1/3"), which are not amounts; this rejects them.
var jsonNumberRe = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

// Decimal is an exact monetary amount carried as decimal text.
//
// On the wire it stays a JSON number, as amount always has been: consumers
// decode it into number types today (consolidation-svc's
// clients.IntercompanyEntry, treasury-svc's create request), and switching
// to a JSON string would break their decoding. What changes is that this
// service never routes it through float64: the digits written are the
// canonical form of the stored NUMERIC, and the digits read are kept
// exactly. A quoted string is accepted on input as well, so callers can move
// to the string form ZS-DATA-001 D06 prefers.
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

var (
	// amountScale is NUMERIC(18,4)'s scale: amounts with more fractional
	// digits would be silently rounded by Postgres on insert.
	amountScale = big.NewRat(10000, 1)
	// amountLimit is NUMERIC(18,4)'s bound: 14 integer digits.
	amountLimit = new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(14), nil))
)

// ValidatePositiveAmount checks that d is a positive amount the
// NUMERIC(18,4) amount columns store exactly, with no rounding and no
// overflow. A value that would be rounded is refused rather than quietly
// changed, so the stored amount is always the amount the caller sent.
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

// currencyCodeRe is the ISO 4217 alphabetic code shape, the same check
// general-ledger-svc applies to a journal's currency_code.
var currencyCodeRe = regexp.MustCompile(`^[A-Z]{3}$`)

// NoCurrency is ISO 4217's "no currency" code. general-ledger-svc backfilled
// it onto every journal that predates its currency_code column (its
// migration 000006), so a journal carrying it has no known currency.
const NoCurrency = "XXX"

// ValidCurrencyCode reports whether c is a three-letter ISO 4217 code that
// names a real currency.
func ValidCurrencyCode(c string) bool {
	return currencyCodeRe.MatchString(c) && c != NoCurrency
}
