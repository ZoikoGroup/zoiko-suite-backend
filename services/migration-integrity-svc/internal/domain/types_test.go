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
			if got := EvaluateJobStatus(tc.score, tc.valid, tc.invalid, 0, tc.threshold); got != tc.want {
				t.Fatalf("score %.2f valid=%d invalid=%d threshold=%v: got %s, want %s",
					tc.score, tc.valid, tc.invalid, tc.threshold, got, tc.want)
			}
		})
	}
}

// Every entry is classified where it is raised (§16) and owned by the
// request's exception owner (MIG-INV-12). Violation type and reason family
// are separate dimensions; this pins the classification each check applies.
func TestIntegrity_EntriesCarrySeverityFamilyAndOwner(t *testing.T) {
	req := &ValidateMigrationRequest{LegalEntityID: "le", MigrationName: "m", SourceSystem: "s", TargetService: "t",
		ExceptionOwnerID: " owner-7 ", RequiredFields: []string{"cost_centre"}, Records: []MigrationRecord{
			rec("", map[string]string{"cost_centre": "CC"}),                   // no identity
			rec("D", map[string]string{"cost_centre": "CC"}),                  // duplicate
			rec("D", map[string]string{"cost_centre": "CC"}),                  // duplicate
			rec("M", map[string]string{}),                                     // missing required
			rec("T", map[string]string{"cost_centre": "CC", "amount": "N/A"}), // bad amount
		}}
	checks, entries, _, _, _ := PerformIntegrityValidation(req, "job", "tenant")

	want := map[ViolationType]struct {
		sev Severity
		fam ReasonFamily
	}{
		ViolationMissingSourceIdentity: {SeverityMQ0, ReasonIdentity},
		ViolationDuplicate:             {SeverityMQ1, ReasonIdentity},
		ViolationTypeMismatch:          {SeverityMQ1, ReasonSchema},
		ViolationMissingRequired:       {SeverityMQ2, ReasonSchema},
	}
	seen := map[ViolationType]bool{}
	for _, e := range entries {
		w, ok := want[e.ViolationType]
		if !ok {
			t.Fatalf("unexpected violation %s", e.ViolationType)
		}
		seen[e.ViolationType] = true
		if e.Severity != w.sev || e.ReasonFamily != w.fam {
			t.Fatalf("%s: got %s/%s, want %s/%s", e.ViolationType, e.Severity, e.ReasonFamily, w.sev, w.fam)
		}
		if e.OwnerID != "owner-7" {
			t.Fatalf("%s: owner %q, want the trimmed exception owner", e.ViolationType, e.OwnerID)
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("expected every classified violation type to be produced, saw %v", seen)
	}
	if n := CountBlockers(entries); n != 1 {
		t.Fatalf("expected 1 MQ0 blocker, got %d", n)
	}
	for _, c := range checks {
		if c.RecordsFailed == 0 {
			t.Fatalf("fixture should fail every check, %s passed", c.CheckName)
		}
	}
}

// A check that finds nothing is MQ4 (informational), whatever class its
// failures would carry.
func TestIntegrity_CleanChecksAreInformational(t *testing.T) {
	_, _, _, checks := validate(rec("A", map[string]string{"amount": "1"}))
	for _, c := range checks {
		if c.Severity != SeverityMQ4 {
			t.Fatalf("%s passed but has severity %s", c.CheckName, c.Severity)
		}
	}
}

// An MQ0 stops the run: the job is FAILED at any score and any bar, a
// perfect score included (SPEC_DEVIATIONS A6).
func TestEvaluateJobStatus_BlockerFailsAtAnyScore(t *testing.T) {
	if got := EvaluateJobStatus(100, 10, 0, 1, 0); got != JobStatusFailed {
		t.Fatalf("a blocker at score 100 / bar 0 must fail, got %s", got)
	}
	if got := EvaluateJobStatus(99, 99, 1, 1, 95); got != JobStatusFailed {
		t.Fatalf("a blocker above the bar must fail, got %s", got)
	}
	if got := EvaluateJobStatus(99, 99, 1, 0, 95); got != JobStatusCompleted {
		t.Fatalf("the same run without a blocker passes, got %s", got)
	}
}

func TestSeverity_OnlyMQ0Blocks(t *testing.T) {
	for _, s := range []Severity{SeverityMQ0, SeverityMQ1, SeverityMQ2, SeverityMQ3, SeverityMQ4} {
		if s.Blocks() != (s == SeverityMQ0) {
			t.Fatalf("%s.Blocks() = %v", s, s.Blocks())
		}
	}
}

func TestValidateRequest_ExceptionOwnerRequired(t *testing.T) {
	r := ValidateMigrationRequest{LegalEntityID: "le", MigrationName: "m", SourceSystem: "s", TargetService: "t",
		Records: []MigrationRecord{rec("A", nil)}, ExceptionOwnerID: "  "}
	if err := r.Validate(); err == nil || err.Error() != "exception_owner_id is required" {
		t.Fatalf("blank owner must be refused, got %v", err)
	}
	r.ExceptionOwnerID = "owner-1"
	if err := r.Validate(); err != nil {
		t.Fatalf("named owner must be accepted, got %v", err)
	}
}
