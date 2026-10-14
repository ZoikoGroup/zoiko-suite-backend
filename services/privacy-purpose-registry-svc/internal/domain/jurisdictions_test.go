package domain

import "testing"

func TestJurisdictionRegistry_IsTheFullISO3166Alpha2List(t *testing.T) {
	// ISO 3166-1 defines exactly 249 officially assigned alpha-2 codes. A typo
	// or an omission in the hand-maintained list changes this count.
	if got := len(isoAlpha2); got != 249 {
		t.Fatalf("ISO 3166-1 alpha-2 registry has %d entries, want 249", got)
	}
	for c := range isoAlpha2 {
		if len(c) != 2 || c[0] < 'A' || c[0] > 'Z' || c[1] < 'A' || c[1] > 'Z' {
			t.Fatalf("malformed registry entry %q", c)
		}
	}
}

func TestIsValidJurisdiction(t *testing.T) {
	valid := []string{"GB", "US", "DE", "FR", "IN", "JP", "BR", "NZ", "ZA", "ZW", "AX", "EU", "EEA", "GLOBAL", " gb ", "eu"}
	// "UK" is not an ISO code (the United Kingdom is GB); "XX"/"ZZ"/"QQ" are
	// user-assigned or unassigned; the rest are free text or malformed.
	invalid := []string{"", "  ", "UK", "XX", "ZZ", "QQ", "AA", "Narnia", "USA", "U", "G B", "US-CA", "WORLD", "EU,US"}
	for _, j := range valid {
		if !IsValidJurisdiction(j) {
			t.Errorf("%q should be a registered jurisdiction", j)
		}
	}
	for _, j := range invalid {
		if IsValidJurisdiction(j) {
			t.Errorf("%q must not be accepted as a jurisdiction", j)
		}
	}
}

func TestNormalizeCodes_PreservesBlanksSoValidationCanReportThem(t *testing.T) {
	got := NormalizeCodes([]string{" gb ", "", "  ", "us"})
	want := []string{"GB", "", "", "US"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("NormalizeCodes = %q, want %q", got, want)
		}
	}
	if NormalizeCodes(nil) != nil || NormalizeRefs(nil) != nil {
		t.Fatalf("nil must stay nil")
	}
	if r := NormalizeRefs([]string{"  Ref-A "}); r[0] != "Ref-A" {
		t.Fatalf("refs are trimmed but case-preserved, got %q", r)
	}
}

func TestSensitiveCategoriesAreRegisteredCategories(t *testing.T) {
	for _, c := range []string{"SPECIAL_CATEGORY", "HEALTH", "BIOMETRIC", "GENETIC", "RACIAL_ETHNIC", "POLITICAL_OPINIONS",
		"RELIGIOUS_PHILOSOPHICAL", "TRADE_UNION", "SEX_LIFE_ORIENTATION", "CRIMINAL_CONVICTIONS", "CHILD_DATA", "HIGH_RISK", "VULNERABLE_SUBJECTS"} {
		if !IsSensitiveCategory(c) || !IsValidDataCategory(c) {
			t.Errorf("%s must be both registered and sensitive", c)
		}
	}
	if IsSensitiveCategory("CONTACT_INFO") {
		t.Errorf("CONTACT_INFO is not sensitive")
	}
}
