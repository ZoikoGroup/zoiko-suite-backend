package domain_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
)

// A JSON number keeps every digit it was sent. Through float64 the first
// value would come back as 1e+14.
func TestDecimal_UnmarshalKeepsExactDigits(t *testing.T) {
	cases := map[string]string{
		`99999999999999.9999`:   "99999999999999.9999",
		`"99999999999999.9999"`: "99999999999999.9999",
		`0.3`:                   "0.3",
		`5000.00`:               "5000",
		`"5000.10"`:             "5000.1",
		`5e3`:                   "5000",
	}
	for in, want := range cases {
		var d domain.Decimal
		if err := json.Unmarshal([]byte(in), &d); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if string(d) != want {
			t.Errorf("%s: got %q, want %q", in, d, want)
		}
	}
}

// big.Rat accepts fractions; an amount must not.
func TestDecimal_UnmarshalRejectsNonDecimals(t *testing.T) {
	for _, in := range []string{`"1/3"`, `"abc"`, `""`, `true`, `"1,000"`, `".5"`} {
		var d domain.Decimal
		if err := json.Unmarshal([]byte(in), &d); err == nil {
			t.Errorf("%s: expected an error, got %q", in, d)
		}
	}
}

// The wire form stays a JSON number, so consumers decoding amount into a
// number type keep working.
func TestDecimal_MarshalsAsNumberLiteral(t *testing.T) {
	out, err := json.Marshal(struct {
		A domain.Decimal `json:"a"`
		B domain.Decimal `json:"b"`
	}{A: "5000.0000", B: "99999999999999.9999"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(out), `{"a":5000,"b":99999999999999.9999}`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	var asFloat struct {
		A float64 `json:"a"`
	}
	if err := json.Unmarshal(out, &asFloat); err != nil || asFloat.A != 5000 {
		t.Fatalf("a float64 consumer can no longer decode amount: %v %v", asFloat.A, err)
	}
}

// NUMERIC(18,4) would round a fifth decimal place, or overflow past 14
// integer digits; both are refused rather than stored as something else.
func TestValidatePositiveAmount(t *testing.T) {
	ok := []domain.Decimal{"0.0001", "1", "5000.25", "99999999999999.9999"}
	for _, d := range ok {
		if _, err := domain.ValidatePositiveAmount(d); err != nil {
			t.Errorf("%s: unexpected %v", d, err)
		}
	}
	cases := map[domain.Decimal]error{
		"0":                  domain.ErrInvalidAmount,
		"-1":                 domain.ErrInvalidAmount,
		"":                   domain.ErrInvalidAmount,
		"1.00001":            domain.ErrAmountPrecision,
		"100000000000000":    domain.ErrAmountPrecision,
		"123456789012345.12": domain.ErrAmountPrecision,
	}
	for d, want := range cases {
		if _, err := domain.ValidatePositiveAmount(d); !errors.Is(err, want) {
			t.Errorf("%q: got %v, want %v", d, err, want)
		}
	}
}

func TestValidCurrencyCode(t *testing.T) {
	for c, want := range map[string]bool{"USD": true, "GBP": true, "XXX": false, "usd": false, "US": false, "USDT": false, "": false} {
		if got := domain.ValidCurrencyCode(c); got != want {
			t.Errorf("%q: got %v, want %v", c, got, want)
		}
	}
}

func TestDimensionsValidate(t *testing.T) {
	if err := (domain.Dimensions{"cost_centre": "CC-100", "project": ""}).Validate(); err != nil {
		t.Fatalf("unexpected %v", err)
	}
	big := domain.Dimensions{}
	for i := 0; i < 33; i++ {
		big[string(rune('a'+i%26))+string(rune('a'+i/26))] = "x"
	}
	for name, d := range map[string]domain.Dimensions{
		"empty key":    {"": "x"},
		"control char": {"k": "a\nb"},
		"too many":     big,
		"long key":     {strings.Repeat("k", 65): "x"},
	} {
		if err := d.Validate(); !errors.Is(err, domain.ErrInvalidDimensions) {
			t.Errorf("%s: got %v", name, err)
		}
	}
}
