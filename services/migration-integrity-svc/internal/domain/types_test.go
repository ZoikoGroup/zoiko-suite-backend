package domain

import "testing"

func rec(ref string, fields map[string]string) MigrationRecord {
	return MigrationRecord{Ref: ref, Fields: fields}
}

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

// A record with no ref has no source identity, which is a defect in its own
// right (MIG-INV-02). It used to be checked like any other record, and two
// blank refs came back only as "duplicates" of each other.
func TestIntegrity_EmptyRefIsMissingIdentityNotDuplicate(t *testing.T) {
	req := &ValidateMigrationRequest{LegalEntityID: "le", MigrationName: "m", SourceSystem: "s", TargetService: "t",
		RequiredFields: []string{"amount"}, Records: []MigrationRecord{
			rec("", map[string]string{"amount": "1"}),
			rec("   ", map[string]string{"amount": "2"}),
		}}
	checks, entries, valid, invalid, score := PerformIntegrityValidation(req, "job", "tenant")
	if valid != 0 || invalid != 2 || score != 0 {
		t.Fatalf("got valid=%d invalid=%d score=%.2f, want 0/2/0", valid, invalid, score)
	}

	var identity, dup int
	for _, e := range entries {
		switch e.ViolationType {
		case ViolationMissingSourceIdentity:
			identity++
		case ViolationDuplicate:
			dup++
		}
	}
	if identity != 2 {
		t.Fatalf("expected 2 MISSING_SOURCE_IDENTITY entries, got %d (entries: %+v)", identity, entries)
	}
	if dup != 0 {
		t.Fatalf("blank refs must not be reported as duplicates of each other, got %d DUPLICATE entries", dup)
	}

	for _, c := range checks {
		switch c.CheckType {
		case CheckSourceIdentity:
			if c.RecordsFailed != 2 || c.RecordsPassed != 0 {
				t.Fatalf("source identity check: %+v", c)
			}
		case CheckDuplicateDetection:
			if c.RecordsFailed != 0 {
				t.Fatalf("duplicate check must not count blank refs: %+v", c)
			}
		}
	}
}

// A blank ref must not disturb duplicate detection for the refs that do have
// an identity: the two "A" records are still duplicates, the blank one is a
// missing identity.
func TestIntegrity_EmptyRefDoesNotPolluteDuplicateDetection(t *testing.T) {
	req := &ValidateMigrationRequest{LegalEntityID: "le", MigrationName: "m", SourceSystem: "s", TargetService: "t",
		RequiredFields: []string{"amount"}, Records: []MigrationRecord{
			rec("", map[string]string{"amount": "1"}),
			rec("A", map[string]string{"amount": "2"}),
			rec("A", map[string]string{"amount": "3"}),
		}}
	_, entries, valid, invalid, score := PerformIntegrityValidation(req, "job", "tenant")
	if valid != 0 || invalid != 3 || score != 0 {
		t.Fatalf("got valid=%d invalid=%d score=%.2f, want 0/3/0", valid, invalid, score)
	}

	var identity, dup int
	for _, e := range entries {
		switch e.ViolationType {
		case ViolationMissingSourceIdentity:
			identity++
		case ViolationDuplicate:
			dup++
		}
	}
	if identity != 1 || dup != 1 {
		t.Fatalf("expected 1 identity violation and 1 duplicate-ref entry, got identity=%d dup=%d", identity, dup)
	}
}

// The score is a ratio; acceptance is the threshold rule (SPEC_DEVIATIONS
// A11): below the bar is FAILED, at the bar passes, and a run with every
// record invalid is FAILED at any bar.
func TestEvaluateJobStatus(t *testing.T) {
	for _, tc := range []struct {
		name           string
		score          float64
		valid, invalid int
		threshold      float64
		want           JobStatus
	}{
		{"clean run passes any bar", 100, 10, 0, 95, JobStatusCompleted},
		{"just below the bar fails", 94.99, 19, 1, 95, JobStatusFailed},
		{"at the bar passes", 95, 19, 1, 95, JobStatusCompleted},
		{"one valid record in a thousand fails", 0.1, 1, 999, 95, JobStatusFailed},
		{"below a lowered bar passes", 90, 9, 1, 80, JobStatusCompleted},
		{"all records invalid fails even at a zero bar", 0, 0, 4, 0, JobStatusFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := EvaluateJobStatus(tc.score, tc.valid, tc.invalid, tc.threshold); got != tc.want {
				t.Fatalf("score %.2f valid=%d invalid=%d threshold=%v: got %s, want %s",
					tc.score, tc.valid, tc.invalid, tc.threshold, got, tc.want)
			}
		})
	}
}
