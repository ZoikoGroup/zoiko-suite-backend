package domain_test

import (
	"encoding/json"
	"errors"
	"testing"

	"zoiko.io/payment-initiation-adapter-svc/internal/domain"
)

// A6: Amount is read as decimal text, a JSON number or a quoted string, and
// keeps every digit the caller sent. A float64 would have turned 10.005
// into 10.00499999999999989 before any check could see it.
func TestDecimal_UnmarshalKeepsTheDigits(t *testing.T) {
	for in, want := range map[string]domain.Decimal{
		`10.005`:                "10.005",
		`"10.005"`:              "10.005",
		`" 7.50 "`:              "7.5",
		`1e2`:                   "100",
		`0.1`:                   "0.1",
		`9999999999999999.99`:   "9999999999999999.99",
		`123456789.123456789`:   "123456789.123456789",
		`null`:                  "",
		`"-0.01"`:               "-0.01",
		`12345678901234567890`:  "12345678901234567890",
		`"12345678901234567.8"`: "12345678901234567.8",
	} {
		var d domain.Decimal
		if err := json.Unmarshal([]byte(in), &d); err != nil || d != want {
			t.Errorf("%s: %q %v, want %q", in, d, err, want)
		}
	}
	for _, in := range []string{`"ten"`, `"1/3"`, `""`, `"0x10"`, `true`, `"1,000.00"`, `".5"`} {
		var d domain.Decimal
		if err := json.Unmarshal([]byte(in), &d); err == nil {
			t.Errorf("%s: accepted as %q", in, d)
		}
	}
}

// On the way out it is a bare JSON number of exactly its digits, so a reader
// decoding into float64 keeps working.
func TestDecimal_MarshalIsABareNumber(t *testing.T) {
	out, err := json.Marshal(struct{ Amount domain.Decimal }{"1234.50"})
	if err != nil || string(out) != `{"Amount":1234.5}` {
		t.Fatalf("%s %v", out, err)
	}
	var f struct{ Amount float64 }
	if err := json.Unmarshal(out, &f); err != nil || f.Amount != 1234.5 {
		t.Fatalf("float64 reader: %v %v", f.Amount, err)
	}
	if out, _ := json.Marshal(struct{ Amount domain.Decimal }{}); string(out) != `{"Amount":null}` {
		t.Fatalf("unset: %s", out)
	}
}

// The NUMERIC(18,4) column (migration 000009 widened it from NUMERIC(18,2))
// stores exactly what passes, and ColumnText is what amount::text reads
// back. This is a column-capacity check only — "10.005" and "0.001" (3
// decimal places) now fit the column; whether a SPECIFIC currency actually
// allows 3 decimal places is TestValidateCurrencyPrecision's concern, not
// this one's.
func TestValidatePositiveAmount(t *testing.T) {
	for d, want := range map[domain.Decimal]error{
		"0":                 domain.ErrInvalidAmount,
		"-1":                domain.ErrInvalidAmount,
		"":                  domain.ErrInvalidAmount,
		"10.00001":          domain.ErrAmountPrecision, // 5th decimal place: beyond the column's own scale
		"100000000000000":   domain.ErrAmountPrecision, // 15 integer digits: beyond the column's own 14-digit capacity
		"100000000000000.5": domain.ErrAmountPrecision,
	} {
		if _, err := domain.ValidatePositiveAmount(d); !errors.Is(err, want) {
			t.Errorf("%q: %v, want %v", d, err, want)
		}
	}
	for d, text := range map[domain.Decimal]string{
		"10.005":              "10.0050",
		"0.001":               "0.0010",
		"10.5":                "10.5000",
		"500":                 "500.0000",
		"0.01":                "0.0100",
		"99999999999999.9999": "99999999999999.9999",
	} {
		r, err := domain.ValidatePositiveAmount(d)
		if err != nil {
			t.Errorf("%q: %v, want no error", d, err)
			continue
		}
		if domain.ColumnText(r) != text {
			t.Errorf("%q: column text %q, want %q", d, domain.ColumnText(r), text)
		}
	}
}

// A real currency's decimal places come from its own ISO 4217 minor-unit
// count — a value that fits the column (NUMERIC(18,4)) is not necessarily
// valid for every currency.
func TestValidateCurrencyPrecision(t *testing.T) {
	cases := []struct {
		amount    domain.Decimal
		minorUnit int
		wantErr   bool
	}{
		{"100", 0, false},     // JPY: an integer amount is fine
		{"100.5", 0, true},    // JPY: any fractional part is refused
		{"10.01", 2, false},   // USD/GBP: 2 decimal places is the common case
		{"10.005", 2, true},   // USD/GBP: a 3rd decimal place is refused
		{"100.123", 3, false}, // BHD: 3 decimal places is its real precision
		{"100.1234", 3, true}, // BHD: a 4th decimal place is still refused
	}
	for _, c := range cases {
		r, err := domain.ValidatePositiveAmount(c.amount)
		if err != nil {
			t.Fatalf("%q: ValidatePositiveAmount failed: %v", c.amount, err)
		}
		err = domain.ValidateCurrencyPrecision(r, c.minorUnit)
		if c.wantErr && !errors.Is(err, domain.ErrAmountExceedsCurrencyPrecision) {
			t.Errorf("%q at minor_unit=%d: %v, want ErrAmountExceedsCurrencyPrecision", c.amount, c.minorUnit, err)
		}
		if !c.wantErr && err != nil {
			t.Errorf("%q at minor_unit=%d: %v, want nil", c.amount, c.minorUnit, err)
		}
	}
}
