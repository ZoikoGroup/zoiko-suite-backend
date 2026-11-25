package store_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"zoiko.io/data-quality-svc/internal/domain"
)

// ZS-SVC-Z-001 INV-04 / INV-05 / INV-15 (docs/architecture/DQC_GAP_MATRIX.md, step A).
//
// Before this change RunDQ only demanded at least ONE outcome, so a rule set of
// N rules could be "certified" on evidence for a single rule, and a rule with
// no evidence had no representation at all. These run against real Postgres as
// the NOSUPERUSER NOBYPASSRLS role like the rest of the suite.

func twoRules() []domain.RuleDefinition {
	return []domain.RuleDefinition{
		{RuleKey: "r-a", Description: "first"},
		{RuleKey: "r-b", Description: "second"},
	}
}

func (f *fixture) run(version *domain.DQRuleSetVersion, ref string, outcomes ...domain.RuleOutcome) (*domain.DQRun, error) {
	f.t.Helper()
	return f.s.RunDQ(f.ctx, orgA, domain.RunDQRequest{
		RuleSetVersionID: version.VersionID, PopulationRef: ref, PopulationRowCount: 1000,
		PopulationContentHash: hashLike(ref), RuleOutcomes: outcomes,
	}, "test-operator", f.claim("RunDQ", ref))
}

func resultFor(t *testing.T, f *fixture, runID, rule string) domain.DQResult {
	t.Helper()
	rs, err := f.s.GetResults(f.ctx, orgA, runID)
	if err != nil {
		t.Fatalf("get results: %v", err)
	}
	for _, r := range rs {
		if r.RuleKey == rule {
			return r
		}
	}
	t.Fatalf("no result stored for rule %q (have %d results)", rule, len(rs))
	return domain.DQResult{}
}

// The headline case: 1 of 2 rules reported. The missing rule is recorded as
// Indeterminate, the run opens exceptions, and certification is refused.
func TestDQ_OneOfNRulesReported_IsIndeterminate_AndCannotCertify(t *testing.T) {
	f := newFixture(t)
	v := f.publishRuleSet(orgA, "completeness-1-of-n", twoRules())

	run, err := f.run(v, "pop-1of2", passingOutcome("r-a"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if run.Status != domain.RunExceptionsOpen {
		t.Fatalf("status = %s, want ExceptionsOpen — a run missing a rule's evidence must not be clean", run.Status)
	}
	if got := resultFor(t, f, run.RunID, "r-a").Status; got != domain.ResultPass {
		t.Fatalf("reported rule = %s, want Pass", got)
	}
	missing := resultFor(t, f, run.RunID, "r-b")
	if missing.Status != domain.ResultIndeterminate {
		t.Fatalf("unreported rule = %s, want Indeterminate (never dropped, never Pass)", missing.Status)
	}
	if missing.PassCount != 0 || missing.FailCount != 0 {
		t.Fatalf("an Indeterminate result must carry no counts: %+v", missing)
	}

	if _, err := f.s.CertifyDQ(f.ctx, orgA, run.RunID, "test-operator", domain.CertifyDQRequest{}, f.claim("CertifyDQ", run.RunID)); err == nil {
		t.Fatal("certified a run with a rule that has no evidence")
	}
}

// "0 failures out of 0 records" is the absence of evidence, not a pass.
func TestDQ_RuleThatEvaluatedNothing_IsIndeterminate(t *testing.T) {
	f := newFixture(t)
	v := f.publishRuleSet(orgA, "completeness-zero-evaluated", []domain.RuleDefinition{{RuleKey: "r-a", Description: "x"}})

	run, err := f.run(v, "pop-zero", domain.RuleOutcome{RuleKey: "r-a", PassCount: 0, FailCount: 0, FailureMagnitudeSum: "0"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := resultFor(t, f, run.RunID, "r-a").Status; got != domain.ResultIndeterminate {
		t.Fatalf("zero-record rule = %s, want Indeterminate", got)
	}
	if run.Status != domain.RunExceptionsOpen {
		t.Fatalf("status = %s, want ExceptionsOpen", run.Status)
	}
}

// INV-15: evidence for a rule the contract does not contain is not guessed at.
func TestDQ_OutcomeForUnknownRule_IsRefused(t *testing.T) {
	f := newFixture(t)
	v := f.publishRuleSet(orgA, "completeness-unknown", []domain.RuleDefinition{{RuleKey: "r-a", Description: "x"}})

	_, err := f.run(v, "pop-unknown", passingOutcome("r-a"), passingOutcome("r-not-in-version"))
	if !errors.Is(err, domain.ErrUnknownRule) {
		t.Fatalf("err = %v, want ErrUnknownRule", err)
	}
}

func TestDQ_TwoOutcomesForTheSameRule_IsRefused(t *testing.T) {
	f := newFixture(t)
	v := f.publishRuleSet(orgA, "completeness-duplicate", []domain.RuleDefinition{{RuleKey: "r-a", Description: "x"}})

	_, err := f.run(v, "pop-dup", passingOutcome("r-a"), passingOutcome("r-a"))
	if !errors.Is(err, domain.ErrDuplicateRuleOutcome) {
		t.Fatalf("err = %v, want ErrDuplicateRuleOutcome", err)
	}
}

// A refused run must leave nothing behind: no run, and no half-written results.
func TestDQ_RefusedRun_WritesNothing(t *testing.T) {
	f := newFixture(t)
	v := f.publishRuleSet(orgA, "completeness-atomic", []domain.RuleDefinition{{RuleKey: "r-a", Description: "x"}})
	before := countRows(t, f, "dq_runs")

	if _, err := f.run(v, "pop-atomic", passingOutcome("r-a"), passingOutcome("nope")); err == nil {
		t.Fatal("expected refusal")
	}
	if after := countRows(t, f, "dq_runs"); after != before {
		t.Fatalf("dq_runs went from %d to %d on a refused run", before, after)
	}
}

func countRows(t *testing.T, f *fixture, table string) int {
	t.Helper()
	var n int
	if err := f.admin.QueryRow(f.ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// The only way out of Indeterminate is evidence: a Reperform that reports every
// rule. It certifies; the superseded run keeps its Indeterminate result.
func TestDQ_ReperformWithCompleteEvidence_ClearsIndeterminateAndCertifies(t *testing.T) {
	f := newFixture(t)
	v := f.publishRuleSet(orgA, "completeness-reperform", twoRules())
	first, err := f.run(v, "pop-rp-1", passingOutcome("r-a"))
	if err != nil {
		t.Fatalf("first run: %v", err)
	}

	second, err := f.s.Reperform(f.ctx, orgA, first.RunID, domain.RunDQRequest{
		RuleSetVersionID: v.VersionID, PopulationRef: "pop-rp-2", PopulationRowCount: 1000,
		PopulationContentHash: hashLike("pop-rp-2"),
		RuleOutcomes:          []domain.RuleOutcome{passingOutcome("r-a"), passingOutcome("r-b")},
	}, "test-operator", f.claim("Reperform", first.RunID))
	if err != nil {
		t.Fatalf("reperform: %v", err)
	}
	if second.Status != domain.RunRunning {
		t.Fatalf("complete reperform status = %s, want Running", second.Status)
	}
	if _, err := f.s.CertifyDQ(f.ctx, orgA, second.RunID, "test-operator", domain.CertifyDQRequest{}, f.claim("CertifyDQ", second.RunID)); err != nil {
		t.Fatalf("certify complete run: %v", err)
	}
	if got := resultFor(t, f, first.RunID, "r-b").Status; got != domain.ResultIndeterminate {
		t.Fatalf("the superseded run's evidence changed: r-b = %s", got)
	}
}

// An Indeterminate result is triaged like a failure: an issue can be raised on it.
func TestDQ_IssueCanBeRaisedOnIndeterminateResult(t *testing.T) {
	f := newFixture(t)
	v := f.publishRuleSet(orgA, "completeness-issue", twoRules())
	run, err := f.run(v, "pop-issue", passingOutcome("r-a"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	missing := resultFor(t, f, run.RunID, "r-b")
	issue, err := f.s.RaiseIssue(f.ctx, orgA, domain.RaiseIssueRequest{
		RunID: run.RunID, ResultID: missing.ResultID, Description: "no evidence for r-b",
	}, "test-operator", f.claim("RaiseIssue", missing.ResultID))
	if err != nil {
		t.Fatalf("raise issue on indeterminate result: %v", err)
	}
	if issue.Status != domain.IssueOpen {
		t.Fatalf("issue status = %s", issue.Status)
	}
	// ...but a Pass result is still not issue-able.
	pass := resultFor(t, f, run.RunID, "r-a")
	if _, err := f.s.RaiseIssue(f.ctx, orgA, domain.RaiseIssueRequest{
		RunID: run.RunID, ResultID: pass.ResultID, Description: "x",
	}, "test-operator", f.claim("RaiseIssue", pass.ResultID)); err == nil {
		t.Fatal("raised an issue on a Pass result")
	}
}

// Defence in depth. A run recorded BEFORE this change can be Running with only
// some of the version's rules reported. CertifyDQ must not trust the run's
// status: it re-derives completeness from the pinned version.
func TestDQ_Certify_RechecksCompleteness_ForARunRecordedBeforeTheFix(t *testing.T) {
	f := newFixture(t)
	v := f.publishRuleSet(orgA, "completeness-legacy", twoRules())

	runID := "dqn_" + uuid.NewString()
	if _, err := f.admin.Exec(f.ctx, `
		INSERT INTO dq_runs (run_id, tenant_id, ruleset_version_id, population_ref, population_row_count,
			population_content_hash, status, created_by)
		VALUES ($1, $2, $3, 'legacy', 10, $4, 'Running', 'legacy-writer')`,
		runID, orgA, v.VersionID, hashLike("legacy")); err != nil {
		t.Fatalf("seed legacy run: %v", err)
	}
	if _, err := f.admin.Exec(f.ctx, `
		INSERT INTO dq_results (result_id, tenant_id, run_id, rule_key, status, pass_count, fail_count, failure_magnitude_sum)
		VALUES ($1, $2, $3, 'r-a', 'Pass', 10, 0, 0)`,
		"dqr_"+uuid.NewString(), orgA, runID); err != nil {
		t.Fatalf("seed legacy result: %v", err)
	}

	_, err := f.s.CertifyDQ(f.ctx, orgA, runID, "test-operator", domain.CertifyDQRequest{}, f.claim("CertifyDQ", runID))
	if !errors.Is(err, domain.ErrEvidenceIncomplete) {
		t.Fatalf("err = %v, want ErrEvidenceIncomplete — a Running run with a missing rule must not certify", err)
	}
}

// The database, not just the application, refuses an Indeterminate result that
// claims counts: it would hide a real outcome under the "no evidence" label.
func TestDQ_Database_RejectsIndeterminateResultWithCounts(t *testing.T) {
	f := newFixture(t)
	v := f.publishRuleSet(orgA, "completeness-check-constraint", []domain.RuleDefinition{{RuleKey: "r-a", Description: "x"}})
	run, err := f.run(v, "pop-check", passingOutcome("r-a"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// First prove the database ACCEPTS a well-formed Indeterminate row. Without
	// this, the rejections below would also pass on a database that has never
	// heard of 'Indeterminate' (the old CHECK rejects them too) — a test that
	// passes for the wrong reason.
	if _, err = f.admin.Exec(f.ctx, `
		INSERT INTO dq_results (result_id, tenant_id, run_id, rule_key, status, pass_count, fail_count, failure_magnitude_sum)
		VALUES ($1, $2, $3, 'r-ok', 'Indeterminate', 0, 0, 0)`,
		"dqr_"+uuid.NewString(), orgA, run.RunID); err != nil {
		t.Fatalf("database refused a well-formed Indeterminate result: %v", err)
	}
	_, err = f.admin.Exec(f.ctx, `
		INSERT INTO dq_results (result_id, tenant_id, run_id, rule_key, status, pass_count, fail_count, failure_magnitude_sum)
		VALUES ($1, $2, $3, 'r-other', 'Indeterminate', 5, 0, 0)`,
		"dqr_"+uuid.NewString(), orgA, run.RunID)
	if err == nil {
		t.Fatal("database accepted an Indeterminate result that carries pass counts")
	}
	_, err = f.admin.Exec(f.ctx, `
		INSERT INTO dq_results (result_id, tenant_id, run_id, rule_key, status, pass_count, fail_count, failure_magnitude_sum)
		VALUES ($1, $2, $3, 'r-bogus', 'Maybe', 0, 0, 0)`,
		"dqr_"+uuid.NewString(), orgA, run.RunID)
	if err == nil {
		t.Fatal("database accepted a result status outside Pass/Fail/Indeterminate")
	}
}
