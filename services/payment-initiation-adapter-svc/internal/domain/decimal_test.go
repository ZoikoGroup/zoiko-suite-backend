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

// The NUMERIC(18,2) column stores exactly what passes, and ColumnText is what
// amount::text reads back.
func TestValidatePositiveAmount(t *testing.T) {
	for d, want := range map[domain.Decimal]error{
		"0":                   domain.ErrInvalidAmount,
		"-1":                  domain.ErrInvalidAmount,
		"":                    domain.ErrInvalidAmount,
		"10.005":              domain.ErrAmountPrecision,
		"0.001":               domain.ErrAmountPrecision,
		"10000000000000000":   domain.ErrAmountPrecision,
		"10000000000000000.5": domain.ErrAmountPrecision,
	} {
		if _, err := domain.ValidatePositiveAmount(d); !errors.Is(err, want) {
			t.Errorf("%q: %v, want %v", d, err, want)
		}
	}
	for d, text := range map[domain.Decimal]string{
		"10.5":                "10.50",
		"500":                 "500.00",
		"0.01":                "0.01",
		"9999999999999999.99": "9999999999999999.99",
	} {
		r, err := domain.ValidatePositiveAmount(d)
		if err != nil || domain.ColumnText(r) != text {
			t.Errorf("%q: %v, column text %q, want %q", d, err, domain.ColumnText(r), text)
		}
	}
}
