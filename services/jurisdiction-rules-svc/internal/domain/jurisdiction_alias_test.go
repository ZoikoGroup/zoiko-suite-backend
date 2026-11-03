package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func goodAlias() AddAliasParams {
	return AddAliasParams{
		JurisdictionID: "j-1", AliasSystem: SystemISO3166Alpha2, AliasCode: "GB",
		SourceStandard: "ISO 3166-1", SourceVersion: "2024-06", MappingRationale: "assigned by the standard",
		ValidFrom: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func TestValidateCode_ISOFormatsFollowTheStandard(t *testing.T) {
	good := map[string][]string{
		SystemISO3166Alpha2:  {"GB", "gb", " us "},
		SystemISO3166Alpha3:  {"GBR", "usa"},
		SystemISO3166Numeric: {"826", "004"},
		SystemISO3166Sub:     {"GB-ENG", "US-CA", "ca-on", "DE-BY"},
		SystemUNM49:          {"001", "150"},
	}
	for system, codes := range good {
		for _, c := range codes {
			if err := ValidateCode(system, c); err != nil {
				t.Errorf("%s %q refused: %v", system, c, err)
			}
		}
	}
	bad := map[string][]string{
		SystemISO3166Alpha2:  {"G", "GBR", "G1", "", "G B"},
		SystemISO3166Alpha3:  {"GB", "GB1", "GBRX"},
		SystemISO3166Numeric: {"82", "8260", "82A"},
		SystemISO3166Sub:     {"GB", "GB-", "GB-ENGL", "G-EN"},
		SystemUNM49:          {"1", "ABC"},
	}
	for system, codes := range bad {
		for _, c := range codes {
			if err := ValidateCode(system, c); !errors.Is(err, ErrInvalidAlias) {
				t.Errorf("%s %q: %v, want ErrInvalidAlias", system, c, err)
			}
		}
	}
}

// An external system's code list is its own business, but it must be safe to
// log and to put in a URL.
func TestValidateCode_ExternalCodesAreSafeAndPrintable(t *testing.T) {
	for _, c := range []string{"X", "UK-ENG", "ERP_9:7", "a/b", "North West"} {
		if err := ValidateCode("SOME-ERP", c); err != nil {
			t.Errorf("%q refused: %v", c, err)
		}
	}
	for _, c := range []string{"", "  ", "x';--", "a\nb", "<script>", "-lead", "trail-", strings.Repeat("A", 65), "é"} {
		if err := ValidateCode("SOME-ERP", c); !errors.Is(err, ErrInvalidAlias) {
			t.Errorf("%q: %v, want ErrInvalidAlias", c, err)
		}
	}
}

func TestNormalizeCode_IsCaseAndSpacingAndNothingElse(t *testing.T) {
	for in, want := range map[string]string{"gb": "GB", "  Gb ": "GB", "GB-eng": "GB-ENG", "a b": "A B"} {
		if got := NormalizeCode(in); got != want {
			t.Errorf("NormalizeCode(%q) = %q, want %q", in, got, want)
		}
	}
	// No similarity matching of any kind: these stay different.
	if NormalizeCode("GB") == NormalizeCode("GBR") || NormalizeCode("G-B") == NormalizeCode("GB") {
		t.Error("normalisation merged distinct codes")
	}
}

func TestAddAliasParams_Validate(t *testing.T) {
	if err := goodAlias().Validate(); err != nil {
		t.Fatalf("a good alias refused: %v", err)
	}
	before := goodAlias().ValidFrom.Add(-time.Hour)
	for name, mutate := range map[string]func(*AddAliasParams){
		"no jurisdiction":   func(p *AddAliasParams) { p.JurisdictionID = "" },
		"no system":         func(p *AddAliasParams) { p.AliasSystem = "" },
		"reserved system":   func(p *AddAliasParams) { p.AliasSystem = SystemInternal },
		"padded system":     func(p *AddAliasParams) { p.AliasSystem = " SOME-ERP " },
		"lower-case system": func(p *AddAliasParams) { p.AliasSystem = "some-erp" },
		"short system":      func(p *AddAliasParams) { p.AliasSystem = "X" },
		"bad ISO code":      func(p *AddAliasParams) { p.AliasCode = "G1" },
		"no standard":       func(p *AddAliasParams) { p.SourceStandard = " " },
		"no version":        func(p *AddAliasParams) { p.SourceVersion = "" },
		"no rationale":      func(p *AddAliasParams) { p.MappingRationale = "" },
		"no valid_from":     func(p *AddAliasParams) { p.ValidFrom = time.Time{} },
		"valid_to before":   func(p *AddAliasParams) { p.ValidTo = &before },
		"over-long source":  func(p *AddAliasParams) { p.SourceStandard = strings.Repeat("x", 129) },
	} {
		p := goodAlias()
		mutate(&p)
		if err := p.Validate(); !errors.Is(err, ErrInvalidAlias) {
			t.Errorf("%s: %v, want ErrInvalidAlias", name, err)
		}
	}
}

func TestEndAliasParams_Validate(t *testing.T) {
	ok := EndAliasParams{ValidTo: time.Now(), Reason: "withdrawn"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (EndAliasParams{Reason: "x"}).Validate(); !errors.Is(err, ErrInvalidAlias) {
		t.Errorf("no valid_to: %v", err)
	}
	if err := (EndAliasParams{ValidTo: time.Now(), Reason: "  "}).Validate(); !errors.Is(err, ErrInvalidAlias) {
		t.Errorf("no reason: %v", err)
	}
}

func TestResolutionErrors_MatchTheirSentinelsAndStayDistinct(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	retired := &RetiredError{JurisdictionID: "j", Code: "OLD", RetiredAt: &at}
	ambiguous := &AmbiguousError{Code: "GB", Candidates: []AmbiguityCandidate{{JurisdictionID: "a"}, {JurisdictionID: "b"}}}
	unmapped := &AliasNotEffectiveError{System: "S", Code: "C", AsOf: at.Add(-time.Hour), From: at}

	checks := []struct {
		err    error
		target error
	}{{retired, ErrJurisdictionRetired}, {ambiguous, ErrAmbiguousJurisdictionCode}, {unmapped, ErrAliasNotEffective}}
	all := []error{ErrUnknownJurisdictionCode, ErrJurisdictionRetired, ErrAmbiguousJurisdictionCode, ErrAliasNotEffective, ErrJurisdictionNotEffective}
	for _, c := range checks {
		for _, other := range all {
			if want := other == c.target; errors.Is(c.err, other) != want {
				t.Errorf("%v: errors.Is(%v) = %v, want %v", c.err, other, !want, want)
			}
		}
	}
	if !strings.Contains(retired.Error(), "2026-01-01") || !strings.Contains(ambiguous.Error(), "2 jurisdictions") {
		t.Errorf("messages: %q / %q", retired.Error(), ambiguous.Error())
	}
	if !strings.Contains(unmapped.Error(), "not yet mapped") {
		t.Errorf("unmapped = %q", unmapped.Error())
	}
	late := &AliasNotEffectiveError{System: "S", Code: "C", AsOf: at.Add(time.Hour), From: at.AddDate(-1, 0, 0)}
	if !strings.Contains(late.Error(), "no longer mapped") {
		t.Errorf("late = %q", late.Error())
	}
}
