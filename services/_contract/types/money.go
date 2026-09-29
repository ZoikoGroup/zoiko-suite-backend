package types

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// MoneyPrecision defines the maximum fractional scale for monetary amounts in ZoikoSuite (NUMERIC(38,12)).
const MoneyPrecision = 12

// MoneyDecimal represents an exact monetary amount with up to 12 decimal places.
// In accordance with ZS-DATA-001 Invariant D06 ("Exact monetary arithmetic"),
// binary floating point (float32/float64) is strictly prohibited for monetary calculations.
type MoneyDecimal struct {
	rat *big.Rat
}

// ZeroMoney returns a zero monetary amount.
func ZeroMoney() MoneyDecimal {
	return MoneyDecimal{rat: new(big.Rat)}
}

// ParseMoney creates a MoneyDecimal from a string representation.
// Returns an error if the string is not a valid decimal number.
func ParseMoney(s string) (MoneyDecimal, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return ZeroMoney(), nil
	}
	r := new(big.Rat)
	if _, ok := r.SetString(s); !ok {
		return ZeroMoney(), fmt.Errorf("invalid monetary decimal format: %q", s)
	}
	return MoneyDecimal{rat: r}, nil
}

// MustParseMoney creates a MoneyDecimal or panics if invalid (for constants and tests).
func MustParseMoney(s string) MoneyDecimal {
	m, err := ParseMoney(s)
	if err != nil {
		panic(err)
	}
	return m
}

// FromInt64 creates a MoneyDecimal from an integer amount and scale.
// Example: FromInt64(1050, 2) represents 10.50.
func FromInt64(val int64, scale uint) MoneyDecimal {
	num := big.NewInt(val)
	denom := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)
	return MoneyDecimal{rat: new(big.Rat).SetFrac(num, denom)}
}

// String returns the decimal string representation with exact formatting.
func (m MoneyDecimal) String() string {
	if m.rat == nil {
		return "0"
	}
	return m.rat.FloatString(MoneyPrecision)
}

// StringTrimmed returns the decimal string without trailing zeros past scale 2, or up to actual precision.
func (m MoneyDecimal) StringTrimmed() string {
	s := m.String()
	if !strings.Contains(s, ".") {
		return s
	}
	s = strings.TrimRight(s, "0")
	if strings.HasSuffix(s, ".") {
		s = s + "00"
	} else {
		parts := strings.Split(s, ".")
		if len(parts) == 2 && len(parts[1]) < 2 {
			s = s + strings.Repeat("0", 2-len(parts[1]))
		}
	}
	return s
}

// IsZero returns true if the monetary amount is zero.
func (m MoneyDecimal) IsZero() bool {
	if m.rat == nil {
		return true
	}
	return m.rat.Sign() == 0
}

// Sign returns -1 if m < 0, 0 if m == 0, +1 if m > 0.
func (m MoneyDecimal) Sign() int {
	if m.rat == nil {
		return 0
	}
	return m.rat.Sign()
}

// Add returns m + other exactly.
func (m MoneyDecimal) Add(other MoneyDecimal) MoneyDecimal {
	r := new(big.Rat)
	mRat := m.rat
	if mRat == nil {
		mRat = new(big.Rat)
	}
	oRat := other.rat
	if oRat == nil {
		oRat = new(big.Rat)
	}
	r.Add(mRat, oRat)
	return MoneyDecimal{rat: r}
}

// Sub returns m - other exactly.
func (m MoneyDecimal) Sub(other MoneyDecimal) MoneyDecimal {
	r := new(big.Rat)
	mRat := m.rat
	if mRat == nil {
		mRat = new(big.Rat)
	}
	oRat := other.rat
	if oRat == nil {
		oRat = new(big.Rat)
	}
	r.Sub(mRat, oRat)
	return MoneyDecimal{rat: r}
}

// Mul returns m * other exactly.
func (m MoneyDecimal) Mul(other MoneyDecimal) MoneyDecimal {
	r := new(big.Rat)
	mRat := m.rat
	if mRat == nil {
		mRat = new(big.Rat)
	}
	oRat := other.rat
	if oRat == nil {
		oRat = new(big.Rat)
	}
	r.Mul(mRat, oRat)
	return MoneyDecimal{rat: r}
}

// MulRate multiplies a monetary amount by a rate decimal.
func (m MoneyDecimal) MulRate(rate RateDecimal) MoneyDecimal {
	r := new(big.Rat)
	mRat := m.rat
	if mRat == nil {
		mRat = new(big.Rat)
	}
	rRat := rate.rat
	if rRat == nil {
		rRat = new(big.Rat)
	}
	r.Mul(mRat, rRat)
	return MoneyDecimal{rat: r}
}

// Cmp compares m and other and returns:
// -1 if m < other
//  0 if m == other
// +1 if m > other
func (m MoneyDecimal) Cmp(other MoneyDecimal) int {
	mRat := m.rat
	if mRat == nil {
		mRat = new(big.Rat)
	}
	oRat := other.rat
	if oRat == nil {
		oRat = new(big.Rat)
	}
	return mRat.Cmp(oRat)
}

// RoundToCurrencyMinorUnits rounds the monetary amount to the given minor unit digits (e.g. 2 for USD/EUR, 0 for JPY).
// Uses Half-Even (Banker's Rounding) for audit-compliant accounting arithmetic.
func (m MoneyDecimal) RoundToCurrencyMinorUnits(minorUnits int) MoneyDecimal {
	if m.rat == nil || m.rat.Sign() == 0 {
		return ZeroMoney()
	}

	scaleFactor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(minorUnits)), nil)
	
	// Multiply numerator by scaleFactor
	num := new(big.Int).Mul(m.rat.Num(), scaleFactor)
	denom := m.rat.Denom()

	// Perform division with remainder: num = q * denom + r
	q := new(big.Int)
	r := new(big.Int)
	q.QuoRem(num, denom, r)

	// Half-even banker's rounding rule
	if r.Sign() != 0 {
		// 2 * |r| vs denom
		absR := new(big.Int).Abs(r)
		twoR := new(big.Int).Mul(absR, big.NewInt(2))
		cmp := twoR.Cmp(denom)
		if cmp > 0 {
			if num.Sign() > 0 {
				q.Add(q, big.NewInt(1))
			} else {
				q.Sub(q, big.NewInt(1))
			}
		} else if cmp == 0 {
			// Exactly half: round to nearest even integer
			if new(big.Int).And(q, big.NewInt(1)).Sign() != 0 {
				if num.Sign() > 0 {
					q.Add(q, big.NewInt(1))
				} else {
					q.Sub(q, big.NewInt(1))
				}
			}
		}
	}

	resRat := new(big.Rat).SetFrac(q, scaleFactor)
	return MoneyDecimal{rat: resRat}
}

// MarshalJSON implements json.Marshaler. Outputs as a string or number depending on options.
// To avoid JavaScript float truncation in web frontends, monetary values are serialized as JSON strings.
func (m MoneyDecimal) MarshalJSON() ([]byte, error) {
	return json.Marshal(m.StringTrimmed())
}

// UnmarshalJSON implements json.Unmarshaler. Accepts either JSON number or string.
func (m *MoneyDecimal) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		parsed, parseErr := ParseMoney(s)
		if parseErr != nil {
			return parseErr
		}
		*m = parsed
		return nil
	}

	// Try unmarshaling as raw numeric string without float intermediate
	var num json.Number
	if err := json.Unmarshal(data, &num); err != nil {
		return fmt.Errorf("cannot unmarshal %s into MoneyDecimal", string(data))
	}

	parsed, parseErr := ParseMoney(num.String())
	if parseErr != nil {
		return parseErr
	}
	*m = parsed
	return nil
}

// Value implements driver.Valuer for database persistence into PostgreSQL NUMERIC.
func (m MoneyDecimal) Value() (driver.Value, error) {
	if m.rat == nil {
		return "0.000000000000", nil
	}
	return m.String(), nil
}

// Scan implements sql.Scanner for database reading from PostgreSQL NUMERIC.
func (m *MoneyDecimal) Scan(src any) error {
	if src == nil {
		*m = ZeroMoney()
		return nil
	}

	switch v := src.(type) {
	case string:
		parsed, err := ParseMoney(v)
		if err != nil {
			return err
		}
		*m = parsed
		return nil
	case []byte:
		parsed, err := ParseMoney(string(v))
		if err != nil {
			return err
		}
		*m = parsed
		return nil
	case int64:
		*m = FromInt64(v, 0)
		return nil
	default:
		return errors.New("unsupported database driver type for MoneyDecimal scan")
	}
}
