package domain

import (
	"errors"
	"testing"
)

func spec(side string) PopulationSpec {
	return PopulationSpec{Side: side, System: "gl", Population: "account-postings"}
}

func defineReq(pops ...PopulationSpec) DefineReconciliationRequest {
	scale := 2
	return DefineReconciliationRequest{LegalEntityID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", Name: "n", RulesInput: RulesInput{
		Populations: pops, MatchKeyFields: []string{"ref_id"}, AmountScale: &scale, ExceptionPolicy: testExceptionPolicy()}}
}

func testExceptionPolicy() *ExceptionPolicy {
	return &ExceptionPolicy{OwnerRole: "recon-owner",
		SLADays: map[Severity]int{SeverityBlocker: 1, SeverityError: 3, SeverityWarning: 10}}
}

// The matcher compares two sides only; a third population would be frozen but
// never reconciled, so a definition naming anything but two is refused.
func TestDefineRequest_RequiresExactlyTwoDistinctPopulations(t *testing.T) {
	for name, pops := range map[string][]PopulationSpec{
		"one":       {spec("A")},
		"three":     {spec("A"), spec("B"), spec("C")},
		"same side": {spec("A"), spec("A")},
		"bad name":  {spec("A"), {Side: "B", System: "gl", Population: "../etc"}},
		"reserved":  {spec("A"), {Side: "B", System: "gl", Population: "p", Params: map[string]string{"legal_entity_id": "x"}}},
	} {
		if err := defineReq(pops...).Validate(); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("%s: got %v, want ErrInvalidArgument", name, err)
		}
	}
	if err := defineReq(spec("A"), spec("B")).Validate(); err != nil {
		t.Fatalf("two populations should be accepted: %v", err)
	}
}

// The scale and the entity are part of what "balanced" means; neither has a default.
func TestDefineRequest_RequiresEntityAndScale(t *testing.T) {
	r := defineReq(spec("A"), spec("B"))
	r.AmountScale = nil
	if err := r.Validate(); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("missing amount_scale: %v", err)
	}
	r = defineReq(spec("A"), spec("B"))
	r.LegalEntityID = "tenant-not-an-entity"
	if err := r.Validate(); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("non-UUID legal_entity_id: %v", err)
	}
	big := MaxAmountScale + 1
	r = defineReq(spec("A"), spec("B"))
	r.AmountScale = &big
	if err := r.Validate(); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("scale above max: %v", err)
	}
}

// Scenario 06: a manual match with an amount difference beyond the pinned
// tolerance stays an exception; the reason text cannot force it through. The
// same holds for a currency difference.
func TestValidateManualMatch(t *testing.T) {
	usd := func(side string, amt int64) MatchItem { return MatchItem{Side: side, Amount: amt, Currency: "USD"} }
	eur := func(side string, amt int64) MatchItem { return MatchItem{Side: side, Amount: amt, Currency: "EUR"} }
	cases := []struct {
		name string
		a, b MatchItem
		tol  int64
		want error
	}{
		{"equal amounts", usd("Bank", 10000), usd("Ledger", 10000), 0, nil},
		{"within tolerance", usd("Bank", 10000), usd("Ledger", 10050), 50, nil},
		{"negative difference within tolerance", usd("Bank", 10050), usd("Ledger", 10000), 50, nil},
		{"outside tolerance", usd("Bank", 10000), usd("Ledger", 10051), 50, ErrManualMatchOutsideTolerance},
		{"outside zero tolerance", usd("Bank", 10000), usd("Ledger", 9999), 0, ErrManualMatchOutsideTolerance},
		{"same side", usd("Bank", 10000), usd("Bank", 10000), 0, ErrManualMatchSameSide},
		{"different currencies", usd("Bank", 10000), eur("Ledger", 10000), 50, ErrManualMatchOutsideTolerance},
	}
	for _, c := range cases {
		tol := c.tol
		p := TolerancePolicy{Currencies: map[string]CurrencyTolerance{"USD": {AbsoluteMinorUnits: &tol}}}
		err := ValidateManualMatch(c.a, c.b, p)
		if c.want == nil && err != nil {
			t.Fatalf("%s: unexpected error %v", c.name, err)
		}
		if c.want != nil && !errors.Is(err, c.want) {
			t.Fatalf("%s: got %v, want %v", c.name, err, c.want)
		}
	}
}
