package domain_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"zoiko.io/treasury-svc/internal/domain"
)

// A34: amounts keep every digit the caller sent, whether the caller uses a
// JSON number (the old float64 wire shape) or a quoted string (D06's
// preferred form). A float64 would have turned 10.005 into
// 10.00499999999999989 before any check could see it.
func TestDecimal_UnmarshalKeepsDigitsAndAcceptsBothWireForms(t *testing.T) {
	for in, want := range map[string]domain.Decimal{
		`10.005`:       "10.005",
		`"10.005"`:     "10.005",
		`" 7.50 "`:     "7.5",
		`1e2`:          "100",
		`0.1`:          "0.1",
		`-0.01`:        "-0.01",
		`null`:         "",
		`"0.00000001"`: "0.00000001",
	} {
		var d domain.Decimal
		if err := json.Unmarshal([]byte(in), &d); err != nil || d != want {
			t.Errorf("Unmarshal(%s) = %q, %v; want %q", in, d, err, want)
		}
	}
}

// A Decimal still marshals as a bare JSON number, not a quoted string, so
// existing callers that decode `amount` into a number keep working. This is
// what treasury sends to BNK-06, the ledger and intercompany.
func TestDecimal_MarshalStaysAJSONNumber(t *testing.T) {
	b, err := json.Marshal(domain.Decimal("10.5000"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "10.5" {
		t.Fatalf("Marshal = %s, want bare number 10.5", b)
	}
}

func TestDecimal_RejectsNonDecimalText(t *testing.T) {
	for _, in := range []string{`"1/3"`, `"abc"`, `""`, `"1,000"`, `1e999999`} {
		var d domain.Decimal
		if err := json.Unmarshal([]byte(in), &d); err == nil {
			t.Errorf("Unmarshal(%s) accepted %q, want an error", in, d)
		}
	}
	for _, bad := range []string{"1/3", "1e999999", strings.Repeat("1", 129), strings.Repeat("1", 100) + "e30"} {
		if _, err := domain.ParseAmount(bad); err == nil {
			t.Errorf("ParseAmount accepted %q; a Decimal must be a bounded terminating decimal", bad)
		}
	}
	// The exponent bound must leave room for the canonical plain-decimal
	// form to be parsed again by Rat/MarshalJSON.
	r, err := domain.ParseAmount("1e-30")
	if err != nil {
		t.Fatalf("ParseAmount(1e-30): %v", err)
	}
	if _, err := domain.Decimal(domain.CanonicalAmount(r)).Rat(); err != nil {
		t.Fatalf("canonical form of 1e-30 is not re-parseable: %v", err)
	}
}

// CanonicalAmount must not round away digits before validation can refuse
// them. A 13-decimal input is visible as 13 decimals, not silently rounded
// to 12 or to zero.
func TestCanonicalAmount_KeepsDigitsBeyondTwelvePlaces(t *testing.T) {
	r, err := domain.ParseAmount("0.0000000000001")
	if err != nil {
		t.Fatal(err)
	}
	if got := domain.CanonicalAmount(r); got != "0.0000000000001" {
		t.Fatalf("CanonicalAmount = %q, want 0.0000000000001", got)
	}
}

func TestDecimal_ArithmeticIsExact(t *testing.T) {
	sum, err := domain.Decimal("0.1").Add("0.2")
	if err != nil || sum != "0.3" {
		t.Fatalf("0.1 + 0.2 = %q, %v; want 0.3", sum, err)
	}
	product, err := domain.Decimal("10.005").Mul("1.1")
	if err != nil || product != "11.0055" {
		t.Fatalf("10.005 * 1.1 = %q, %v; want 11.0055", product, err)
	}
	cmp, err := domain.Decimal("10.5000").Cmp("10.5")
	if err != nil || cmp != 0 {
		t.Fatalf("10.5000 cmp 10.5 = %d, %v; want 0", cmp, err)
	}
}

// NUMERIC(20,4) stores 4 fractional digits and 16 integer digits. An input
// the column would round is refused, not accepted and silently changed.
func TestValidateMoney_RefusesWhatTheColumnWouldRoundOrOverflow(t *testing.T) {
	for _, ok := range []domain.Decimal{"0", "-1.5", "9999999999999999.9999", "0.0001"} {
		if _, err := domain.ValidateMoney(ok); err != nil {
			t.Errorf("ValidateMoney(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []domain.Decimal{"0.00001", "10000000000000000", "abc", ""} {
		if _, err := domain.ValidateMoney(bad); err == nil {
			t.Errorf("ValidateMoney(%q) accepted a value the column cannot store exactly", bad)
		}
	}
	if _, err := domain.ValidatePositiveMoney("0"); !errors.Is(err, domain.ErrInvalidAmount) {
		t.Errorf("ValidatePositiveMoney(0) = %v, want ErrInvalidAmount", err)
	}
}

// NUMERIC(20,8) is a different scale and bound. The same Decimal type
// serves it, but the money validators must not be reused for rates.
func TestValidateRate_UsesTheRateScale(t *testing.T) {
	for _, ok := range []domain.Decimal{"1", "1.12345678", "0.00000001", "999999999999.99999999"} {
		if _, err := domain.ValidateRate(ok); err != nil {
			t.Errorf("ValidateRate(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []domain.Decimal{"1.123456789", "1000000000000", "0", "-1", "abc"} {
		if _, err := domain.ValidateRate(bad); err == nil {
			t.Errorf("ValidateRate(%q) accepted a value the rate column cannot store", bad)
		}
	}
}

// New transfer hashes and fingerprints use the column's fixed four-place
// exact text. For ordinary amounts this is also the old %.4f shape; the
// store separately accepts legacy hashes where float64 rounded differently.
func TestMoneyText_PreservesTheLegacyFourPlaceShape(t *testing.T) {
	for in, want := range map[domain.Decimal]string{
		"10.5":    "10.5000",
		"10.005":  "10.0050",
		"0.1":     "0.1000",
		"250.5":   "250.5000",
		"0":       "0.0000",
		"-1.2345": "-1.2345",
	} {
		if got := in.MoneyText(); got != want {
			t.Errorf("%q.MoneyText() = %q, want %q", in, got, want)
		}
	}
}
