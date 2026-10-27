// Package kit is the shared foundation for the AR-01..AR-10 modules that live
// inside accounts-receivable-svc: exact money, stable error classes, the
// tenant/entity/authorization guard, command idempotency, the transactional
// outbox/audit helpers, the durable ACC-04 posting queue and the cross-module
// ports.
//
// Nothing here owns a business fact. Each AR module (internal/arNN) owns its own
// tables and rules and uses kit for the plumbing the spec §14/§20 requires of
// every command.
package kit

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Amount is an exact decimal money value held as an integer count of 1/10000
// units. Spec invariant: "exact arithmetic" — no float64 anywhere on a new AR
// money path. It marshals as a decimal string ("1234.5000") and scans from a
// Postgres NUMERIC, so a value never passes through a binary float.
type Amount int64

const (
	amountScale     = 10000
	amountScaleDigs = 4
)

// ErrAmountFormat is returned for anything that is not a plain decimal.
var ErrAmountFormat = errors.New("amount must be a decimal number with at most 4 fractional digits")

// ParseAmount reads "123", "123.4", "-0.0001". Exponents, thousands separators
// and more than four fractional digits are refused rather than rounded.
func ParseAmount(s string) (Amount, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, ErrAmountFormat
	}
	neg := false
	switch s[0] {
	case '-':
		neg, s = true, s[1:]
	case '+':
		s = s[1:]
	}
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" && frac == "" {
		return 0, ErrAmountFormat
	}
	if len(frac) > amountScaleDigs {
		return 0, ErrAmountFormat
	}
	var w int64
	for _, c := range whole {
		if c < '0' || c > '9' {
			return 0, ErrAmountFormat
		}
		if w > (1<<62)/10 {
			return 0, ErrAmountFormat
		}
		w = w*10 + int64(c-'0')
	}
	frac += strings.Repeat("0", amountScaleDigs-len(frac))
	var f int64
	for _, c := range frac {
		if c < '0' || c > '9' {
			return 0, ErrAmountFormat
		}
		f = f*10 + int64(c-'0')
	}
	if w > (1<<62)/amountScale {
		return 0, ErrAmountFormat
	}
	v := w*amountScale + f
	if neg {
		v = -v
	}
	return Amount(v), nil
}

// MustAmount is for constants and tests.
func MustAmount(s string) Amount {
	a, err := ParseAmount(s)
	if err != nil {
		panic(err)
	}
	return a
}

// String renders the canonical four-decimal form.
func (a Amount) String() string {
	v := int64(a)
	sign := ""
	if v < 0 {
		sign, v = "-", -v
	}
	return fmt.Sprintf("%s%d.%04d", sign, v/amountScale, v%amountScale)
}

func (a Amount) IsZero() bool     { return a == 0 }
func (a Amount) IsPositive() bool { return a > 0 }
func (a Amount) IsNegative() bool { return a < 0 }

// Add and Sub are exact; overflow is not a realistic concern at 4 dp inside an
// int64, and ParseAmount bounds inputs well below it.
func (a Amount) Add(b Amount) Amount { return a + b }
func (a Amount) Sub(b Amount) Amount { return a - b }
func (a Amount) Neg() Amount         { return -a }

func MinAmount(a, b Amount) Amount {
	if a < b {
		return a
	}
	return b
}

// Float is for the legacy float64 wire contracts only (general-ledger-svc and
// tax-determination-svc still take JSON numbers). It is never used to compute.
func (a Amount) Float() float64 { return float64(a) / amountScale }

func (a Amount) MarshalJSON() ([]byte, error) { return json.Marshal(a.String()) }

// UnmarshalJSON accepts a decimal string or a bare JSON number, but a number is
// taken from its literal text so 0.1 never becomes a binary float on the way in.
func (a *Amount) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		*a = 0
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return ErrAmountFormat
		}
		s = str
	}
	v, err := ParseAmount(s)
	if err != nil {
		return err
	}
	*a = v
	return nil
}

// Scan implements sql.Scanner for NUMERIC columns.
func (a *Amount) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*a = 0
		return nil
	case string:
		return a.scanText(v)
	case []byte:
		return a.scanText(string(v))
	case int64:
		*a = Amount(v * amountScale)
		return nil
	default:
		return fmt.Errorf("cannot scan %T into Amount", src)
	}
}

func (a *Amount) scanText(s string) error {
	// Postgres may return more than four fractional digits for wider scales;
	// the columns used for Amount are NUMERIC(20,4) so this is exact.
	v, err := ParseAmount(s)
	if err != nil {
		return err
	}
	*a = v
	return nil
}

// Value implements driver.Valuer; pgx sends it as text and Postgres casts it to
// NUMERIC.
func (a Amount) Value() (driver.Value, error) { return a.String(), nil }

// minorUnitDigits is the ISO 4217 exponent for the currencies that are not 2.
// The platform's authority is currency-registry-svc; this table is the offline
// guard that stops an obviously mis-scaled amount (e.g. 12.345 JPY) at the
// boundary. Unknown codes default to 2.
var minorUnitDigits = map[string]int{
	"JPY": 0, "KRW": 0, "VND": 0, "CLP": 0, "ISK": 0, "UGX": 0, "XOF": 0, "XAF": 0, "PYG": 0,
	"BHD": 3, "KWD": 3, "OMR": 3, "JOD": 3, "TND": 3, "IQD": 3, "LYD": 3,
}

// MinorUnitDigits returns the number of fractional digits the currency allows.
func MinorUnitDigits(currency string) int {
	if d, ok := minorUnitDigits[strings.ToUpper(currency)]; ok {
		return d
	}
	return 2
}

// FitsCurrency reports whether the amount has no precision beyond the
// currency's minor unit.
func (a Amount) FitsCurrency(currency string) bool {
	digits := MinorUnitDigits(currency)
	step := int64(1)
	for i := 0; i < amountScaleDigs-digits; i++ {
		step *= 10
	}
	return int64(a)%step == 0
}

// RoundHalfUp rounds to the currency's minor unit, away from zero on ties, for
// the places the spec allows computed values (proportional tax, FX); stored
// document amounts are validated with FitsCurrency instead.
func (a Amount) RoundHalfUp(currency string) Amount {
	digits := MinorUnitDigits(currency)
	step := int64(1)
	for i := 0; i < amountScaleDigs-digits; i++ {
		step *= 10
	}
	v := int64(a)
	neg := v < 0
	if neg {
		v = -v
	}
	v = (v + step/2) / step * step
	if neg {
		v = -v
	}
	return Amount(v)
}
