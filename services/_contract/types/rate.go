package types

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// RatePrecision defines the maximum fractional scale for rates/percentages in ZoikoSuite (NUMERIC(38,18)).
const RatePrecision = 18

// RateDecimal represents an exact rate or percentage with up to 18 decimal places.
// In accordance with ZS-DATA-001 Section 18 ("Canonical Field Types & Precision"),
// tax rates, exchange rates, interest rates and allocation percentages require exact NUMERIC(38,18).
type RateDecimal struct {
	rat *big.Rat
}

// ZeroRate returns a zero rate.
func ZeroRate() RateDecimal {
	return RateDecimal{rat: new(big.Rat)}
}

// ParseRate parses a string into a RateDecimal.
func ParseRate(s string) (RateDecimal, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return ZeroRate(), nil
	}
	r := new(big.Rat)
	if _, ok := r.SetString(s); !ok {
		return ZeroRate(), fmt.Errorf("invalid rate decimal format: %q", s)
	}
	return RateDecimal{rat: r}, nil
}

// MustParseRate parses a rate or panics if invalid.
func MustParseRate(s string) RateDecimal {
	r, err := ParseRate(s)
	if err != nil {
		panic(err)
	}
	return r
}

// FromBasisPoints creates a RateDecimal from integer basis points (1 bp = 0.0001 = 0.01%).
func FromBasisPoints(bps int64) RateDecimal {
	num := big.NewInt(bps)
	denom := big.NewInt(10000)
	return RateDecimal{rat: new(big.Rat).SetFrac(num, denom)}
}

// FromPercentage creates a RateDecimal from a standard percentage (e.g. 20.0 for 20% VAT).
func FromPercentage(pct string) (RateDecimal, error) {
	r, err := ParseRate(pct)
	if err != nil {
		return ZeroRate(), err
	}
	oneHundred := new(big.Rat).SetInt64(100)
	r.rat.Quo(r.rat, oneHundred)
	return r, nil
}

// String returns the decimal string with full rate precision.
func (r RateDecimal) String() string {
	if r.rat == nil {
		return "0"
	}
	return r.rat.FloatString(RatePrecision)
}

// StringTrimmed returns the decimal string with trailing zeros trimmed.
func (r RateDecimal) StringTrimmed() string {
	s := r.String()
	if !strings.Contains(s, ".") {
		return s
	}
	s = strings.TrimRight(s, "0")
	if strings.HasSuffix(s, ".") {
		s = s + "0"
	}
	return s
}

// IsZero returns true if rate is zero.
func (r RateDecimal) IsZero() bool {
	if r.rat == nil {
		return true
	}
	return r.rat.Sign() == 0
}

// Sign returns -1, 0, or 1.
func (r RateDecimal) Sign() int {
	if r.rat == nil {
		return 0
	}
	return r.rat.Sign()
}

// Add returns r + other.
func (r RateDecimal) Add(other RateDecimal) RateDecimal {
	res := new(big.Rat)
	rRat := r.rat
	if rRat == nil {
		rRat = new(big.Rat)
	}
	oRat := other.rat
	if oRat == nil {
		oRat = new(big.Rat)
	}
	res.Add(rRat, oRat)
	return RateDecimal{rat: res}
}

// Mul returns r * other.
func (r RateDecimal) Mul(other RateDecimal) RateDecimal {
	res := new(big.Rat)
	rRat := r.rat
	if rRat == nil {
		rRat = new(big.Rat)
	}
	oRat := other.rat
	if oRat == nil {
		oRat = new(big.Rat)
	}
	res.Mul(rRat, oRat)
	return RateDecimal{rat: res}
}

// Cmp compares r and other.
func (r RateDecimal) Cmp(other RateDecimal) int {
	rRat := r.rat
	if rRat == nil {
		rRat = new(big.Rat)
	}
	oRat := other.rat
	if oRat == nil {
		oRat = new(big.Rat)
	}
	return rRat.Cmp(oRat)
}

// MarshalJSON implements json.Marshaler.
func (r RateDecimal) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.StringTrimmed())
}

// UnmarshalJSON implements json.Unmarshaler.
func (r *RateDecimal) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		parsed, parseErr := ParseRate(s)
		if parseErr != nil {
			return parseErr
		}
		*r = parsed
		return nil
	}

	var num json.Number
	if err := json.Unmarshal(data, &num); err != nil {
		return fmt.Errorf("cannot unmarshal %s into RateDecimal", string(data))
	}

	parsed, parseErr := ParseRate(num.String())
	if parseErr != nil {
		return parseErr
	}
	*r = parsed
	return nil
}

// Value implements driver.Valuer for PostgreSQL NUMERIC.
func (r RateDecimal) Value() (driver.Value, error) {
	if r.rat == nil {
		return "0.000000000000000000", nil
	}
	return r.String(), nil
}

// Scan implements sql.Scanner for PostgreSQL NUMERIC.
func (r *RateDecimal) Scan(src any) error {
	if src == nil {
		*r = ZeroRate()
		return nil
	}

	switch v := src.(type) {
	case string:
		parsed, err := ParseRate(v)
		if err != nil {
			return err
		}
		*r = parsed
		return nil
	case []byte:
		parsed, err := ParseRate(string(v))
		if err != nil {
			return err
		}
		*r = parsed
		return nil
	default:
		return errors.New("unsupported database driver type for RateDecimal scan")
	}
}
