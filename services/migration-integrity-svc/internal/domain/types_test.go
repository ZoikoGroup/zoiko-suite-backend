package domain

import "testing"

func rec(ref string, fields map[string]string) MigrationRecord { return MigrationRecord{Ref: ref, Fields: fields} }

func validate(recs ...MigrationRecord) (valid, invalid int, score float64, checks []IntegrityCheck) {
	req := &ValidateMigrationRequest{LegalEntityID: "le", MigrationName: "m", SourceSystem: "s", TargetService: "t",
		RequiredFields: []string{"amount"}, Records: recs}
	checks, _, valid, invalid, score = PerformIntegrityValidation(req, "job", "tenant")
	return valid, invalid, score, checks
}

// Four records all sharing one ref are four violations, not one: counting by
// distinct ref used to report 3 of 4 valid (75%) when none was.
func TestIntegrity_DuplicatesCountedPerRecord(t *testing.T) {
	r := rec("SAME", map[string]string{"amount": "1"})
	valid, invalid, score, checks := validate(r, r, r, r)
	if valid != 0 || invalid != 4 || score != 0 {
		t.Fatalf("got valid=%d invalid=%d score=%.2f, want 0/4/0", valid, invalid, score)
	}
	for _, c := range checks {
		if c.CheckType == CheckDuplicateDetection && (c.RecordsFailed != 4 || c.RecordsPassed != 0) {
			t.Fatalf("duplicate check: %+v", c)
		}
	}

	valid, invalid, score, _ = validate(r, r, rec("OTHER", map[string]string{"amount": "2"}))
	if valid != 1 || invalid != 2 || score != 33.33 {
		t.Fatalf("got valid=%d invalid=%d score=%.2f, want 1/2/33.33", valid, invalid, score)
	}
}

func TestIntegrity_NumericFormat(t *testing.T) {
	for _, v := range []string{"5", "-5", "5.25", "-0.01", "5.", ".5", " 12 "} {
		if _, invalid, _, _ := validate(rec("R", map[string]string{"amount": v})); invalid != 0 {
			t.Fatalf("%q should be accepted as numeric", v)
		}
	}
	for _, v := range []string{"1.2.3", "-", ".", "1-2", "--5", "N/A", "1,000", "+5", "1e5"} {
		if _, invalid, _, _ := validate(rec("R", map[string]string{"amount": v})); invalid != 1 {
			t.Fatalf("%q should be refused as numeric", v)
		}
	}
}
