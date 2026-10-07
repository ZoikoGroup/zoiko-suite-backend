package store_test

import (
	"errors"
	"testing"

	"zoiko.io/data-quality-svc/internal/domain"
)

// DATA-02 Data Quality, against real Postgres as the NOSUPERUSER
// NOBYPASSRLS role.

func passingOutcome(ruleKey string) domain.RuleOutcome {
	return domain.RuleOutcome{RuleKey: ruleKey, PassCount: 1000, FailCount: 0, FailureMagnitudeSum: "0"}
}

// Happy path: publish rule set -> RunDQ (all pass) -> CertifyDQ.
func TestDQ_HappyPath_RunAndCertify(t *testing.T) {
	f := newFixture(t)
	version := f.publishRuleSet(orgA, "invoice-completeness", []domain.RuleDefinition{
		{RuleKey: "completeness-01", Description: "no null invoice totals"},
	})

	run, err := f.s.RunDQ(f.ctx, orgA, domain.RunDQRequest{
		RuleSetVersionID: version.VersionID, PopulationRef: "pop-1", PopulationRowCount: 1000,
		PopulationContentHash: hashLike("pop-1"),
		RuleOutcomes:          []domain.RuleOutcome{passingOutcome("completeness-01")},
	}, "test-operator", f.claim("RunDQ", "pop-1"))
	if err != nil {
		t.Fatalf("run dq: %v", err)
	}
	if run.Status != domain.RunRunning {
		t.Fatalf("run status = %s, want Running (clean)", run.Status)
	}

	cert, err := f.s.CertifyDQ(f.ctx, orgA, run.RunID, "test-operator", f.claim("CertifyDQ", run.RunID))
	if err != nil {
		t.Fatalf("certify dq: %v", err)
	}
	if cert.SummarySHA256 == "" || len(cert.SummarySHA256) != 64 {
		t.Fatalf("summary_sha256 = %q", cert.SummarySHA256)
	}
	if len(cert.Summary.Results) != 1 {
		t.Fatalf("certified summary results = %d, want 1", len(cert.Summary.Results))
	}

	after, err := f.s.GetRun(f.ctx, orgA, run.RunID)
	if err != nil || after.Status != domain.RunCertified {
		t.Fatalf("run after certify: %+v (err=%v)", after, err)
	}
}

// Negative path #1 (doc-named): DQ rule-set change creates a new
// run/version — an existing published version is never edited in place.
func TestDQ_RuleSetChangeCreatesNewVersion(t *testing.T) {
	f := newFixture(t)
	v1 := f.publishRuleSet(orgA, "reference-integrity", []domain.RuleDefinition{
		{RuleKey: "ref-01", Description: "supplier_id exists"},
	})
	if v1.VersionNumber != 1 {
		t.Fatalf("first version number = %d, want 1", v1.VersionNumber)
	}

	v2, err := f.s.PublishRuleSetVersion(f.ctx, orgA, "reference-integrity",
		[]domain.RuleDefinition{
			{RuleKey: "ref-01", Description: "supplier_id exists"},
			{RuleKey: "ref-02", Description: "customer_id exists"},
		}, "test-operator", f.claim("PublishRuleSetVersion", "reference-integrity-v2"))
	if err != nil {
		t.Fatalf("publish v2: %v", err)
	}
	if v2.VersionNumber != 2 {
		t.Fatalf("second version number = %d, want 2", v2.VersionNumber)
	}
	if v2.VersionID == v1.VersionID {
		t.Fatal("publishing a rule-set change reused the old version's ID instead of creating a new one")
	}

	// v1 is still independently queryable, unchanged.
	gotV1, err := f.s.GetRuleSetVersion(f.ctx, orgA, v1.VersionID)
	if err != nil {
		t.Fatalf("get v1 after v2 published: %v", err)
	}
	if len(gotV1.Rules) != 1 {
		t.Fatalf("v1 rules = %d, want 1 (unchanged by v2's publish)", len(gotV1.Rules))
	}
}

// Negative path #2 (doc-named): operator cannot mark a source defect
// fixed without source-domain evidence — there is no direct "resolve"
// command; the only path to clearing a failing run's issues is a
// Reperform whose new run passes.
func TestDQ_NoDirectResolvePath_OnlyReperformClearsIssues(t *testing.T) {
	f := newFixture(t)
	version := f.publishRuleSet(orgA, "amount-reasonableness", []domain.RuleDefinition{
		{RuleKey: "reasonableness-01", Description: "amount within expected range"},
	})

	failingRun, err := f.s.RunDQ(f.ctx, orgA, domain.RunDQRequest{
		RuleSetVersionID: version.VersionID, PopulationRef: "pop-2", PopulationRowCount: 500,
		PopulationContentHash: hashLike("pop-2"),
		RuleOutcomes: []domain.RuleOutcome{
			{RuleKey: "reasonableness-01", PassCount: 490, FailCount: 10, FailureMagnitudeSum: "500.00"},
		},
	}, "test-operator", f.claim("RunDQ", "pop-2"))
	if err != nil {
		t.Fatalf("run dq: %v", err)
	}
	if failingRun.Status != domain.RunExceptionsOpen {
		t.Fatalf("run status = %s, want ExceptionsOpen", failingRun.Status)
	}

	results, err := f.s.GetResults(f.ctx, orgA, failingRun.RunID)
	if err != nil || len(results) != 1 {
		t.Fatalf("results: %+v (err=%v)", results, err)
	}
	issue, err := f.s.RaiseIssue(f.ctx, orgA, domain.RaiseIssueRequest{
		RunID: failingRun.RunID, ResultID: results[0].ResultID, Description: "amounts look wrong",
	}, "test-operator", f.claim("RaiseIssue", results[0].ResultID))
	if err != nil {
		t.Fatalf("raise issue: %v", err)
	}

	assigned, err := f.s.AssignIssue(f.ctx, orgA, domain.AssignIssueRequest{IssueID: issue.IssueID, Assignee: "ops-alice"},
		f.claim("AssignIssue", issue.IssueID))
	if err != nil {
		t.Fatalf("assign issue: %v", err)
	}
	if assigned.Status != domain.IssueAssigned {
		t.Fatalf("issue status = %s, want Assigned", assigned.Status)
	}
	// AssignIssue is the only mutation this issue can ever undergo — a
	// second call (no "resolve" exists) is refused.
	if _, err := f.s.AssignIssue(f.ctx, orgA, domain.AssignIssueRequest{IssueID: issue.IssueID, Assignee: "ops-bob"},
		f.claim("AssignIssue-again", issue.IssueID)); !errors.Is(err, domain.ErrIssueAlreadyAssigned) {
		t.Fatalf("re-assigned/re-mutated an already-assigned issue: %v", err)
	}
	// CertifyDQ on the still-failing run is refused — an assigned issue is
	// not a resolved one.
	if _, err := f.s.CertifyDQ(f.ctx, orgA, failingRun.RunID, "test-operator", f.claim("CertifyDQ-too-early", failingRun.RunID)); err == nil {
		t.Fatal("certified a run with an unresolved (merely assigned) issue")
	}

	// The ONLY path forward: Reperform with fresh evidence showing the
	// source domain actually fixed the underlying data.
	reperformed, err := f.s.Reperform(f.ctx, orgA, failingRun.RunID, domain.RunDQRequest{
		RuleSetVersionID: version.VersionID, PopulationRef: "pop-2-corrected", PopulationRowCount: 500,
		PopulationContentHash: hashLike("pop-2-corrected"),
		RuleOutcomes:          []domain.RuleOutcome{passingOutcome("reasonableness-01")},
	}, "test-operator", f.claim("Reperform", failingRun.RunID))
	if err != nil {
		t.Fatalf("reperform: %v", err)
	}
	if reperformed.Status != domain.RunRunning {
		t.Fatalf("reperformed run status = %s, want Running (clean)", reperformed.Status)
	}
	if reperformed.SupersedesRunID == nil || *reperformed.SupersedesRunID != failingRun.RunID {
		t.Fatalf("reperformed run supersedes_run_id: %+v", reperformed.SupersedesRunID)
	}

	oldRun, err := f.s.GetRun(f.ctx, orgA, failingRun.RunID)
	if err != nil || oldRun.Status != domain.RunReperformed {
		t.Fatalf("old run after reperform: %+v (err=%v)", oldRun, err)
	}

	if _, err := f.s.CertifyDQ(f.ctx, orgA, reperformed.RunID, "test-operator", f.claim("CertifyDQ", reperformed.RunID)); err != nil {
		t.Fatalf("certify the clean reperformed run: %v", err)
	}
}

// Negative path #3 (doc-named): aggregate materiality catches many
// individually small failures — 100 failures of $1 each (sum $100)
// against a $50 aggregate threshold still fails, even though no single
// failure is large.
func TestDQ_AggregateMaterialityCatchesManySmallFailures(t *testing.T) {
	f := newFixture(t)
	version := f.publishRuleSet(orgA, "rounding-tolerance", []domain.RuleDefinition{
		{RuleKey: "rounding-01", Description: "penny rounding tolerance", AggregateMaterialityThreshold: strp("50.00")},
	})

	run, err := f.s.RunDQ(f.ctx, orgA, domain.RunDQRequest{
		RuleSetVersionID: version.VersionID, PopulationRef: "pop-3", PopulationRowCount: 10000,
		PopulationContentHash: hashLike("pop-3"),
		RuleOutcomes: []domain.RuleOutcome{
			{RuleKey: "rounding-01", PassCount: 9900, FailCount: 100, FailureMagnitudeSum: "100.00"},
		},
	}, "test-operator", f.claim("RunDQ", "pop-3"))
	if err != nil {
		t.Fatalf("run dq: %v", err)
	}
	if run.Status != domain.RunExceptionsOpen {
		t.Fatalf("run status = %s, want ExceptionsOpen (aggregate $100 > $50 threshold)", run.Status)
	}
	results, err := f.s.GetResults(f.ctx, orgA, run.RunID)
	if err != nil || len(results) != 1 || results[0].Status != domain.ResultFail {
		t.Fatalf("results: %+v (err=%v)", results, err)
	}

	// Same shape, but under the threshold: tolerated, passes despite
	// FailCount > 0.
	runUnder, err := f.s.RunDQ(f.ctx, orgA, domain.RunDQRequest{
		RuleSetVersionID: version.VersionID, PopulationRef: "pop-4", PopulationRowCount: 10000,
		PopulationContentHash: hashLike("pop-4"),
		RuleOutcomes: []domain.RuleOutcome{
			{RuleKey: "rounding-01", PassCount: 9970, FailCount: 30, FailureMagnitudeSum: "30.00"},
		},
	}, "test-operator", f.claim("RunDQ", "pop-4"))
	if err != nil {
		t.Fatalf("run dq (under threshold): %v", err)
	}
	if runUnder.Status != domain.RunRunning {
		t.Fatalf("run status = %s, want Running ($30 aggregate is under the $50 threshold, tolerated)", runUnder.Status)
	}
}

// Idempotent replay: retrying RunDQ with the same claim key must not
// create a second run.
func TestDQ_RunDQ_IdempotentReplay(t *testing.T) {
	f := newFixture(t)
	version := f.publishRuleSet(orgA, "replay-check", []domain.RuleDefinition{
		{RuleKey: "r1", Description: "x"},
	})
	req := domain.RunDQRequest{
		RuleSetVersionID: version.VersionID, PopulationRef: "pop-5", PopulationRowCount: 100,
		PopulationContentHash: hashLike("pop-5"),
		RuleOutcomes:          []domain.RuleOutcome{passingOutcome("r1")},
	}
	claim := f.claim("RunDQ", "pop-5")
	first, err := f.s.RunDQ(f.ctx, orgA, req, "test-operator", claim)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	_, err = f.s.RunDQ(f.ctx, orgA, req, "test-operator", claim)
	var replay *domain.IdempotentReplayError
	if !errors.As(err, &replay) || replay.ResourceID != first.RunID {
		t.Fatalf("replay of RunDQ: %v", err)
	}
}

// DB-level negative control: a sealed DQ certification is immutable at
// the database.
func TestDQ_CertificationIsImmutableAtTheDatabase(t *testing.T) {
	f := newFixture(t)
	version := f.publishRuleSet(orgA, "immutability-check", []domain.RuleDefinition{
		{RuleKey: "r1", Description: "x"},
	})
	run, err := f.s.RunDQ(f.ctx, orgA, domain.RunDQRequest{
		RuleSetVersionID: version.VersionID, PopulationRef: "pop-6", PopulationRowCount: 100,
		PopulationContentHash: hashLike("pop-6"),
		RuleOutcomes:          []domain.RuleOutcome{passingOutcome("r1")},
	}, "test-operator", f.claim("RunDQ", "pop-6"))
	if err != nil {
		t.Fatalf("run dq: %v", err)
	}
	cert, err := f.s.CertifyDQ(f.ctx, orgA, run.RunID, "test-operator", f.claim("CertifyDQ", run.RunID))
	if err != nil {
		t.Fatalf("certify: %v", err)
	}

	tx, err := f.app.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(f.ctx) //nolint:errcheck
	if _, err := tx.Exec(f.ctx, "SELECT set_config('app.tenant_id', $1, true)", orgA); err != nil {
		t.Fatalf("declare tenant: %v", err)
	}
	if _, err := tx.Exec(f.ctx, `UPDATE dq_certifications SET summary_sha256 = repeat('0', 64) WHERE certification_id = $1`,
		cert.CertificationID); err == nil {
		t.Fatal("a raw UPDATE against a sealed dq certification was not rejected")
	}
}

// DB-level negative control: a terminal run (Certified) cannot be
// mutated further, even to touch an unrelated column.
func TestDQ_CertifiedRunIsImmutableAtTheDatabase(t *testing.T) {
	f := newFixture(t)
	version := f.publishRuleSet(orgA, "terminal-check", []domain.RuleDefinition{
		{RuleKey: "r1", Description: "x"},
	})
	run, err := f.s.RunDQ(f.ctx, orgA, domain.RunDQRequest{
		RuleSetVersionID: version.VersionID, PopulationRef: "pop-7", PopulationRowCount: 100,
		PopulationContentHash: hashLike("pop-7"),
		RuleOutcomes:          []domain.RuleOutcome{passingOutcome("r1")},
	}, "test-operator", f.claim("RunDQ", "pop-7"))
	if err != nil {
		t.Fatalf("run dq: %v", err)
	}
	if _, err := f.s.CertifyDQ(f.ctx, orgA, run.RunID, "test-operator", f.claim("CertifyDQ", run.RunID)); err != nil {
		t.Fatalf("certify: %v", err)
	}

	tx, err := f.app.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(f.ctx) //nolint:errcheck
	if _, err := tx.Exec(f.ctx, "SELECT set_config('app.tenant_id', $1, true)", orgA); err != nil {
		t.Fatalf("declare tenant: %v", err)
	}
	if _, err := tx.Exec(f.ctx, `UPDATE dq_runs SET population_ref = 'tampered' WHERE run_id = $1`, run.RunID); err == nil {
		t.Fatal("a raw UPDATE touching an unrelated column on a certified run was not rejected")
	}
}

// Tenant isolation: cross-tenant reads are denied by RLS.
func TestDQ_TenantIsolation(t *testing.T) {
	f := newFixture(t)
	version := f.publishRuleSet(orgA, "isolation-check", []domain.RuleDefinition{
		{RuleKey: "r1", Description: "x"},
	})
	run, err := f.s.RunDQ(f.ctx, orgA, domain.RunDQRequest{
		RuleSetVersionID: version.VersionID, PopulationRef: "pop-8", PopulationRowCount: 100,
		PopulationContentHash: hashLike("pop-8"),
		RuleOutcomes:          []domain.RuleOutcome{passingOutcome("r1")},
	}, "test-operator", f.claim("RunDQ", "pop-8"))
	if err != nil {
		t.Fatalf("run dq: %v", err)
	}
	if _, err := f.s.GetRun(f.ctx, orgB, run.RunID); !errors.Is(err, domain.ErrRunNotFound) {
		t.Fatalf("cross-tenant run read: %v", err)
	}
}

func hashLike(seed string) string {
	// A syntactically valid 64-hex-char stand-in content hash for tests —
	// this service trusts caller-supplied population evidence, it never
	// recomputes it, so tests don't need a real sha256 here.
	base := "0123456789abcdef"
	out := make([]byte, 64)
	for i := range out {
		out[i] = base[(int(seed[i%len(seed)])+i)%16]
	}
	return string(out)
}
