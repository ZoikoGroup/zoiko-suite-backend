package domain_test

import (
	"errors"
	"strings"
	"testing"

	"zoiko.io/data-quality-svc/internal/domain"
)

func goodBody() domain.ContractBody {
	return domain.ContractBody{
		Schema: []domain.SchemaField{
			{Name: "amount", Type: "decimal"},
			{Name: "invoice_no", Type: "string"},
			{Name: "memo", Type: "string", Nullable: true},
		},
		Predicates: []domain.SemanticPredicate{{Name: "positive", Expression: "amount > 0", Fields: []string{"amount"}}},
		Rules: []domain.ContractRule{
			{RuleKey: "amount-positive", Dimension: "validity", Severity: domain.SeverityBlocker, Predicate: "positive"},
			{RuleKey: "memo-present", Dimension: "completeness", Severity: domain.SeverityInfo},
		},
		Compatibility: domain.CompatBackward,
	}
}

func normalized(b domain.ContractBody) domain.ContractBody { b.Normalize(); return b }

func TestValidate_AcceptsAGoodContract(t *testing.T) {
	if err := normalized(goodBody()).Validate(); err != nil {
		t.Fatalf("a good contract was refused: %v", err)
	}
}

func TestValidate_RefusesEachDefect(t *testing.T) {
	cases := map[string]func(*domain.ContractBody){
		"no schema":            func(b *domain.ContractBody) { b.Schema = nil },
		"duplicate field":      func(b *domain.ContractBody) { b.Schema = append(b.Schema, b.Schema[0]) },
		"unknown type":         func(b *domain.ContractBody) { b.Schema[0].Type = "varchar" },
		"unknown compat":       func(b *domain.ContractBody) { b.Compatibility = "SOMETIMES" },
		"no rules":             func(b *domain.ContractBody) { b.Rules = nil },
		"duplicate rule":       func(b *domain.ContractBody) { b.Rules = append(b.Rules, b.Rules[0]) },
		"unknown severity":     func(b *domain.ContractBody) { b.Rules[0].Severity = "FATAL" },
		"unknown dimension":    func(b *domain.ContractBody) { b.Rules[0].Dimension = "vibes" },
		"undeclared predicate": func(b *domain.ContractBody) { b.Rules[0].Predicate = "ghost" },
		"predicate reads a field outside the schema": func(b *domain.ContractBody) { b.Predicates[0].Fields = []string{"nope"} },
		"predicate with no fields":                   func(b *domain.ContractBody) { b.Predicates[0].Fields = nil },
		"profile names an unknown rule": func(b *domain.ContractBody) {
			b.CertificationProfile.MandatoryRules = []string{"amount-positive", "ghost"}
		},
		"profile leaves out a BLOCKER": func(b *domain.ContractBody) { b.CertificationProfile.MandatoryRules = []string{"memo-present"} },
		"tolerance with no rounding": func(b *domain.ContractBody) {
			b.Rules[1].Tolerance = &domain.Tolerance{Kind: "ABSOLUTE", Value: "1", Scale: 2, Population: "all"}
		},
		"tolerance with no population": func(b *domain.ContractBody) {
			b.Rules[1].Tolerance = &domain.Tolerance{Kind: "ABSOLUTE", Value: "1", Scale: 2, Rounding: "HALF_UP"}
		},
		"percent over 100": func(b *domain.ContractBody) {
			b.Rules[1].Tolerance = &domain.Tolerance{Kind: "PERCENT", Value: "101", Scale: 2, Rounding: "HALF_UP", Population: "all"}
		},
		"negative tolerance": func(b *domain.ContractBody) {
			b.Rules[1].Tolerance = &domain.Tolerance{Kind: "ABSOLUTE", Value: "-1", Scale: 2, Rounding: "HALF_UP", Population: "all"}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b := goodBody()
			mutate(&b)
			b.Normalize()
			err := b.Validate()
			if !errors.Is(err, domain.ErrContractInvalid) {
				t.Fatalf("want ErrContractInvalid, got %v", err)
			}
		})
	}
}

// The proof that the BLOCKER rule above is what refused the profile, not something else: the same
// profile is accepted once it lists the BLOCKER.
func TestValidate_ProfileIsAcceptedOnceItListsTheBlocker(t *testing.T) {
	b := goodBody()
	b.CertificationProfile.MandatoryRules = []string{"amount-positive"}
	b.Normalize()
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}
	ok := goodBody()
	ok.Rules[1].Tolerance = &domain.Tolerance{Kind: "PERCENT", Value: "0.5", Scale: 2, Rounding: "HALF_EVEN", Population: "all posted invoices"}
	ok.Normalize()
	if err := ok.Validate(); err != nil {
		t.Fatalf("a fully specified tolerance was refused: %v", err)
	}
}

func TestNormalize_StoresTheDefaultCertificationProfile(t *testing.T) {
	b := normalized(goodBody())
	if got := strings.Join(b.CertificationProfile.MandatoryRules, ","); got != "amount-positive" {
		t.Fatalf("default mandatory rules = %q, want only the BLOCKER (INFO is not mandatory)", got)
	}
}

func TestContentHash_IsOrderIndependentAndContentSensitive(t *testing.T) {
	a := goodBody()
	b := goodBody()
	b.Schema[0], b.Schema[2] = b.Schema[2], b.Schema[0]
	b.Rules[0], b.Rules[1] = b.Rules[1], b.Rules[0]
	if normalized(a).ContentHash() != normalized(b).ContentHash() {
		t.Fatal("the same contract written in a different order hashed differently")
	}
	c := goodBody()
	c.Rules[0].Severity = domain.SeverityError
	if normalized(a).ContentHash() == normalized(c).ContentHash() {
		t.Fatal("changing a severity did not change the hash")
	}
}

func old() domain.ContractBody { return normalized(goodBody()) }

func with(mode domain.Compatibility, mutate func(*domain.ContractBody)) domain.ContractBody {
	b := goodBody()
	b.Compatibility = mode
	mutate(&b)
	return normalized(b)
}

func kinds(r domain.CompatibilityReport) string {
	var k []string
	for _, b := range r.Breaking {
		k = append(k, b.Field+":"+b.Kind)
	}
	return strings.Join(k, ",")
}

func TestCompatibility_Matrix(t *testing.T) {
	type tc struct {
		name   string
		mode   domain.Compatibility
		mutate func(*domain.ContractBody)
		want   string // "" = compatible
	}
	cases := []tc{
		{"identical", domain.CompatFull, func(*domain.ContractBody) {}, ""},
		{"nullable field added is fine", domain.CompatFull, func(b *domain.ContractBody) {
			b.Schema = append(b.Schema, domain.SchemaField{Name: "note", Type: "string", Nullable: true})
		}, ""},
		{"required field added breaks backward", domain.CompatBackward, func(b *domain.ContractBody) {
			b.Schema = append(b.Schema, domain.SchemaField{Name: "branch", Type: "string"})
		}, "branch:REQUIRED_FIELD_ADDED"},
		{"required field added is fine for forward", domain.CompatForward, func(b *domain.ContractBody) {
			b.Schema = append(b.Schema, domain.SchemaField{Name: "branch", Type: "string"})
		}, ""},
		{"required field removed breaks every enforced mode (NP-03)", domain.CompatForward, func(b *domain.ContractBody) {
			b.Schema = b.Schema[1:] // drops "amount"
		}, "amount:REQUIRED_FIELD_REMOVED"},
		{"required field removed breaks backward too", domain.CompatBackward, func(b *domain.ContractBody) {
			b.Schema = b.Schema[1:]
		}, "amount:REQUIRED_FIELD_REMOVED"},
		{"nullable field removed is fine", domain.CompatFull, func(b *domain.ContractBody) {
			b.Schema = b.Schema[:2] // drops "memo"
		}, ""},
		{"type changed", domain.CompatFull, func(b *domain.ContractBody) { b.Schema[0].Type = "integer" }, "amount:TYPE_CHANGED"},
		{"format changed", domain.CompatBackward, func(b *domain.ContractBody) { b.Schema[0].Format = "0.00" }, "amount:FORMAT_CHANGED"},
		{"nullable tightened breaks backward", domain.CompatBackward, func(b *domain.ContractBody) { b.Schema[2].Nullable = false }, "memo:NULLABLE_TIGHTENED"},
		{"nullable tightened is fine for forward", domain.CompatForward, func(b *domain.ContractBody) { b.Schema[2].Nullable = false }, ""},
		{"nullable loosened breaks forward", domain.CompatForward, func(b *domain.ContractBody) { b.Schema[0].Nullable = true }, "amount:NULLABLE_LOOSENED"},
		{"nullable loosened is fine for backward", domain.CompatBackward, func(b *domain.ContractBody) { b.Schema[0].Nullable = true }, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rep := domain.CheckCompatibility(old(), with(c.mode, c.mutate), 1)
			if got := kinds(rep); got != c.want {
				t.Fatalf("breaking = %q, want %q", got, c.want)
			}
			if rep.Compatible != (c.want == "") {
				t.Fatalf("compatible = %v for breaking %q", rep.Compatible, got(rep))
			}
		})
	}
}

func got(r domain.CompatibilityReport) string { return kinds(r) }

// NONE claims no guarantee, so a breaking change does not make the report incompatible, but the
// evidence is still produced: the caller can see exactly what it is acknowledging.
func TestCompatibility_NoneStillReportsTheEvidence(t *testing.T) {
	rep := domain.CheckCompatibility(old(), with(domain.CompatNone, func(b *domain.ContractBody) { b.Schema[0].Type = "integer" }), 3)
	if !rep.Compatible || !rep.IsBreaking() || len(rep.Breaking) != 1 || rep.AgainstVersion != 3 {
		t.Fatalf("report = %+v", rep)
	}
}

func TestCompatibility_ReportsSeverityChanges(t *testing.T) {
	rep := domain.CheckCompatibility(old(), with(domain.CompatBackward, func(b *domain.ContractBody) {
		b.Rules[0].Severity = domain.SeverityWarning // BLOCKER -> WARNING
		b.Rules = b.Rules[:1]                        // memo-present removed
	}), 1)
	byKey := map[string]string{}
	for _, c := range rep.SeverityChanges {
		byKey[c.RuleKey] = c.Kind
	}
	if byKey["amount-positive"] != "WEAKENED" || byKey["memo-present"] != "RULE_REMOVED" {
		t.Fatalf("severity changes = %+v", rep.SeverityChanges)
	}
}
