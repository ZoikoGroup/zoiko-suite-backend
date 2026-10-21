package domain

import (
	"encoding/json"
	"errors"
	"testing"
)

func ruleInput() RulesInput {
	scale := 2
	tol := int64(5)
	return RulesInput{
		Populations:    []PopulationSpec{{Side: "Bank", System: "BANK", Population: "p"}, {Side: "Ledger", System: "GL", Population: "q"}},
		MatchKeyFields: []string{"ref_id"}, AmountScale: &scale,
		Tolerance:       TolerancePolicy{Currencies: map[string]CurrencyTolerance{"USD": {AbsoluteMinorUnits: &tol}}},
		Materiality:     MaterialityPolicy{Currencies: map[string]int64{"USD": 1000, "EUR": 900}},
		ExceptionPolicy: testExceptionPolicy(),
	}
}

// The digest is canonical: equal rules hash equal however they were written,
// including after a JSON round trip (the store re-verifies it on every load),
// and any rule change changes it.
func TestRuleSet_DigestIsCanonical(t *testing.T) {
	a, err := ruleInput().RuleSet()
	if err != nil {
		t.Fatal(err)
	}
	d1, _ := a.Digest()

	in := ruleInput()
	in.BalanceDimensions = []string{} // empty and absent are the same rule
	in.Materiality = MaterialityPolicy{Currencies: map[string]int64{"EUR": 900, "USD": 1000}}
	b, _ := in.RuleSet()
	if d2, _ := b.Digest(); d2 != d1 {
		t.Fatalf("equal rules hash differently: %s / %s", d1, d2)
	}

	raw, _ := json.Marshal(a)
	var back RuleSet
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if d3, _ := back.Digest(); d3 != d1 {
		t.Fatalf("round trip changed the digest: %s / %s", d1, d3)
	}

	c := a
	c.CertificationProfile.PermitAcceptedRisk = true
	if d4, _ := c.Digest(); d4 == d1 {
		t.Fatal("a rule change did not change the digest")
	}
}

func TestCreateRuleVersionRequest_Validate(t *testing.T) {
	zero := int64(0)
	ok := CreateRuleVersionRequest{BasedOnVersion: &zero, RulesInput: ruleInput(), Reason: "widen nothing", EffectiveFrom: "2026-11-01"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*CreateRuleVersionRequest){
		"no base":       func(r *CreateRuleVersionRequest) { r.BasedOnVersion = nil },
		"no reason":     func(r *CreateRuleVersionRequest) { r.Reason = " " },
		"bad date":      func(r *CreateRuleVersionRequest) { r.EffectiveFrom = "1/11/2026" },
		"no policy":     func(r *CreateRuleVersionRequest) { r.ExceptionPolicy = nil },
		"no scale":      func(r *CreateRuleVersionRequest) { r.AmountScale = nil },
		"bad match key": func(r *CreateRuleVersionRequest) { r.MatchKeyFields = []string{"reference"} },
	} {
		r := ok
		r.RulesInput = ruleInput()
		mutate(&r)
		if err := r.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDefinition_Runnable(t *testing.T) {
	if err := (ReconciliationDefinition{}).Runnable(); !errors.Is(err, ErrLegacyRecord) {
		t.Fatalf("no entity: %v", err)
	}
	draftOnly := ReconciliationDefinition{LegalEntityID: "e", LatestRuleVersion: 1}
	if err := draftOnly.Runnable(); !errors.Is(err, ErrNoEffectiveRules) {
		t.Fatalf("no approved version: %v", err)
	}
	rs, _ := ruleInput().RuleSet()
	ok := ReconciliationDefinition{LegalEntityID: "e", Version: 1, Materiality: &rs.Materiality, ExceptionPolicy: &rs.ExceptionPolicy,
		CertificationProfile: &rs.CertificationProfile}
	if err := ok.Runnable(); err != nil {
		t.Fatal(err)
	}
}
