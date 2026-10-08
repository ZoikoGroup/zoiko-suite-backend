package domain

import (
	"errors"
	"strings"
	"testing"
)

func contract() map[string]VariableSpec {
	return NormalizeContract(map[string]VariableSpec{
		"first_name": {Type: "STRING", Required: true, Sensitivity: "S1"},
		"period":     {Type: "STRING", Required: true, Sensitivity: "S0", MaxLength: 30},
		"net_pay":    {Type: "NUMBER", Required: true, Sensitivity: "S3"},
		"due":        {Type: "DATE", Required: false, Sensitivity: "S1"},
		"case_ref":   {Type: "ID", Required: false, Sensitivity: "S1"},
		"link":       {Type: "URL", Required: false, Sensitivity: "S1"},
	})
}

func TestValidateContract_NothingIsDefaultedOrGuessed(t *testing.T) {
	if err := ValidateContract(contract()); err != nil {
		t.Fatalf("a well-formed contract: %v", err)
	}
	cases := map[string]map[string]VariableSpec{
		"no sensitivity": {"x": {Type: "STRING"}},
		"no type":        {"x": {Sensitivity: "S1"}},
		"unknown type":   {"x": {Type: "BLOB", Sensitivity: "S1"}},
		"unknown sens.":  {"x": {Type: "STRING", Sensitivity: "S9"}},
		"bad name":       {"X-1": {Type: "STRING", Sensitivity: "S1"}},
		"dotted name":    {"a.b": {Type: "STRING", Sensitivity: "S1"}},
		"length on date": {"x": {Type: "DATE", Sensitivity: "S1", MaxLength: 4}},
		"length too big": {"x": {Type: "STRING", Sensitivity: "S1", MaxLength: 5000}},
	}
	for name, c := range cases {
		if err := ValidateContract(c); !errors.Is(err, ErrIntentInvalid) {
			t.Errorf("%s: want ErrIntentInvalid, got %v", name, err)
		}
	}
	many := map[string]VariableSpec{}
	for i := 0; i < 51; i++ {
		many["v"+strings.Repeat("a", i%10)+string(rune('a'+i%26))+string(rune('a'+i/26))] = VariableSpec{Type: "STRING", Sensitivity: "S1"}
	}
	if err := ValidateContract(many); !errors.Is(err, ErrIntentInvalid) {
		t.Errorf("more than the limit: %v", err)
	}
}

func TestCheckVariables_TypesRequiredAndUndeclared(t *testing.T) {
	good := map[string]string{"first_name": "Asha", "period": "October 2026", "net_pay": "4210.55", "due": "2026-11-05", "case_ref": "CASE-9", "link": "https://app.example.com/p/1"}
	if err := CheckVariables(contract(), good); err != nil {
		t.Fatalf("valid values refused: %v", err)
	}
	// Optional ones may be left out.
	if err := CheckVariables(contract(), map[string]string{"first_name": "A", "period": "P", "net_pay": "1"}); err != nil {
		t.Fatalf("optional variables omitted: %v", err)
	}
	bad := map[string]map[string]string{
		"missing required": {"first_name": "A", "net_pay": "1"},
		"blank required":   {"first_name": "", "period": "P", "net_pay": "1"},
		"undeclared":       {"first_name": "A", "period": "P", "net_pay": "1", "extra": "x"},
		"number as text":   {"first_name": "A", "period": "P", "net_pay": "4,210.55"},
		"number exponent":  {"first_name": "A", "period": "P", "net_pay": "1e3"},
		"bad date":         {"first_name": "A", "period": "P", "net_pay": "1", "due": "05/11/2026"},
		"bad id":           {"first_name": "A", "period": "P", "net_pay": "1", "case_ref": "has space"},
		"http url":         {"first_name": "A", "period": "P", "net_pay": "1", "link": "http://app.example.com"},
		"url credentials":  {"first_name": "A", "period": "P", "net_pay": "1", "link": "https://user:pw@app.example.com"},
		"javascript url":   {"first_name": "A", "period": "P", "net_pay": "1", "link": "javascript:alert(1)"},
		"too long":         {"first_name": "A", "period": strings.Repeat("x", 31), "net_pay": "1"},
		"control char":     {"first_name": "A\x00", "period": "P", "net_pay": "1"},
	}
	for name, v := range bad {
		if err := CheckVariables(contract(), v); !errors.Is(err, ErrTemplateVariableInvalid) {
			t.Errorf("%s: want ErrTemplateVariableInvalid, got %v", name, err)
		}
	}
	// Every problem is reported at once.
	err := CheckVariables(contract(), map[string]string{"extra": "x"})
	var vp VariableProblem
	if !errors.As(err, &vp) || len(vp.Problems) != 4 { // first_name, period, net_pay required + extra undeclared
		t.Errorf("want 4 problems reported together, got %v", err)
	}
}

// INV-17: the contract, not the author, decides what may reach a subject.
func TestCheckSubjectAgainstContract(t *testing.T) {
	c := contract()
	if err := CheckSubjectAgainstContract([]string{"period", "first_name"}, c); err != nil {
		t.Fatalf("S0 and S1 variables are subject-safe: %v", err)
	}
	if err := CheckSubjectAgainstContract([]string{"net_pay"}, c); !errors.Is(err, ErrSubjectInvalid) {
		t.Errorf("an S3 variable must never reach a subject: %v", err)
	}
	if err := CheckSubjectAgainstContract([]string{"ghost"}, c); !errors.Is(err, ErrSubjectInvalid) {
		t.Errorf("an undeclared variable cannot be in a subject: %v", err)
	}
}

func TestCheckTemplateVariables_SubsetOfTheContract(t *testing.T) {
	c := contract()
	if err := CheckTemplateVariables([]string{"first_name", "period"}, c); err != nil {
		t.Fatalf("a subset is fine: %v", err)
	}
	if err := CheckTemplateVariables([]string{"first_name", "ssn"}, c); !errors.Is(err, ErrTemplateVariableInvalid) {
		t.Errorf("a template cannot use a variable its intent does not govern: %v", err)
	}
}

func TestValidateIntentVersion_MarketingIsConfinedToLifecycleAndMarketingPurposes(t *testing.T) {
	base := CreateIntentVersionParams{PurposeClass: "T0", EvidenceClass: "E1", AllowedChannels: []string{"EMAIL"}, VariableContract: map[string]VariableSpec{}}
	if err := ValidateIntentVersion(base); err != nil {
		t.Fatal(err)
	}
	for _, class := range []string{"S0", "T0", "A1"} {
		p := base
		p.PurposeClass, p.MarketingAllowed = class, true
		if err := ValidateIntentVersion(p); !errors.Is(err, ErrIntentInvalid) {
			t.Errorf("%s with marketing_allowed must be refused: %v", class, err)
		}
	}
	for _, class := range []string{"L1", "M1"} {
		p := base
		p.PurposeClass, p.MarketingAllowed = class, true
		if err := ValidateIntentVersion(p); err != nil {
			t.Errorf("%s may allow marketing: %v", class, err)
		}
	}
}

func TestValidateIntentKey(t *testing.T) {
	for _, k := range []string{"payroll.payslip_available", "a", "hr.review.invitation_v2"} {
		if err := ValidateIntentKey(k); err != nil {
			t.Errorf("%q: %v", k, err)
		}
	}
	for _, k := range []string{"", "Payroll.x", "payroll..x", ".payroll", "payroll.", "pay roll", "1abc", strings.Repeat("a", 121)} {
		if err := ValidateIntentKey(k); !errors.Is(err, ErrIntentInvalid) {
			t.Errorf("%q should be refused: %v", k, err)
		}
	}
}
