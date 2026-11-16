package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/policy-svc/internal/domain"
	"zoiko.io/policy-svc/internal/store"
)

// V-001 PDC-05 on a real database, as the RLS-bound application role.

// gated is the env's store with the certification gate enforced.
func (e *testEnv) gated() *store.PgStore {
	s := store.New(e.app, zap.NewNop()).WithCertificationGate(domain.GateEnforce)
	s.SetClock(func() time.Time { return e.clock })
	return s
}

func (e *testEnv) testCase(t *testing.T, p *domain.Policy, code string, amount int, expected string) *domain.TestCase {
	t.Helper()
	ctx, _ := json.Marshal(map[string]int{"amount": amount})
	c, _, err := e.s.CreateTestCase(as(tenantA), domain.CreateTestCaseParams{
		PolicyID: p.PolicyID, TestCode: code, Layer: domain.TestLayerBoundary, Mandatory: true,
		Description: code, ActionContext: ctx, ExpectedResult: expected, ActorID: "tester",
	})
	if err != nil {
		t.Fatalf("test case %s: %v", code, err)
	}
	return c
}

// approved takes a draft through review and approval (not activation).
func (e *testEnv) reviewedAndApproved(t *testing.T, v *domain.PolicyVersion) *domain.PolicyVersion {
	t.Helper()
	for _, step := range []struct{ cmd, actor string }{{domain.CommandSubmitReview, "author"}, {domain.CommandApprove, "approver"}} {
		res, err := e.cmd(t, tenantA, v, step.cmd, step.actor)
		if err != nil {
			t.Fatalf("%s: %v", step.cmd, err)
		}
		v = res.Version
	}
	return v
}

func activate(s *store.PgStore, v *domain.PolicyVersion) error {
	_, err := s.Transition(as(tenantA), domain.TransitionCommand{PolicyVersionID: v.PolicyVersionID, Command: domain.CommandActivate, ActorID: "activator"})
	return err
}

// NP-41: a failing mandatory suite blocks activation; a passing one lets it
// through. No suite at all is not a pass.
func TestPDC05_NP41_FailingSuiteBlocksActivation(t *testing.T) {
	e := newEnv(t)
	gs := e.gated()
	p := e.family(t, tenantA, sp(tenantA), "PDC-41")
	v := e.reviewedAndApproved(t, e.draft(t, tenantA, p, sp(tenantA), nil, e.clock.Add(-time.Hour), `{"threshold_amount": 1000}`))

	if err := activate(gs, v); !errors.Is(err, domain.ErrCertificationRequired) {
		t.Fatalf("an uncertified version must not go live, got %v", err)
	}
	run, err := e.s.RunTests(as(tenantA), v.PolicyVersionID, "tester", domain.EventMeta{})
	if err != nil || run.RunStatus != domain.TestRunFail || len(run.StaticFindings) == 0 {
		t.Fatalf("a run with no mandatory case must fail static validation: %+v %v", run, err)
	}

	e.testCase(t, p, "above", 2000, "APPROVAL_REQUIRED")
	e.testCase(t, p, "at", 1000, "WITHIN_THRESHOLD")
	// A negative test: a negative amount is refused, never "within".
	e.testCase(t, p, "negative", -1, "INPUT_ERROR")
	run, err = e.s.RunTests(as(tenantA), v.PolicyVersionID, "tester", domain.EventMeta{})
	if err != nil || run.RunStatus != domain.TestRunPass || run.CaseCount != 3 || run.FailedCount != 0 {
		t.Fatalf("the suite must pass: %+v %v", run, err)
	}
	if c, _ := e.s.Certification(as(tenantA), v.PolicyVersionID); c.Status != domain.CertificationCertified {
		t.Fatalf("certification: %+v", c)
	}
	if err := activate(gs, v); err != nil {
		t.Fatalf("a certified version activates: %v", err)
	}

	// A successor whose threshold breaks the "above" case cannot go live.
	v2 := e.reviewedAndApproved(t, e.draft(t, tenantA, p, sp(tenantA), nil, e.clock.Add(-time.Minute), `{"threshold_amount": 5000}`))
	run, _ = e.s.RunTests(as(tenantA), v2.PolicyVersionID, "tester", domain.EventMeta{})
	if run.RunStatus != domain.TestRunFail || run.FailedCount != 1 {
		t.Fatalf("the successor must fail one case: %+v", run)
	}
	if err := activate(gs, v2); !errors.Is(err, domain.ErrCertificationRequired) {
		t.Fatalf("NP-41: a failing suite blocks activation, got %v", err)
	}
	// Runs are immutable evidence.
	if _, err := e.admin.Exec(context.Background(), `UPDATE policy_test_runs SET run_status = 'PASS'`); err == nil {
		t.Fatal("NP-42: a run cannot be marked passed by hand")
	}
	if n := e.count(t, `SELECT COUNT(*) FROM outbox WHERE event_type = 'policy.version.certified'`); n != 1 {
		t.Fatalf("one certified event (the passing run), got %d", n)
	}
}

// NP-42/NP-43: a failed case is excused only by an approved, scoped,
// unexpired waiver from someone other than the requester; the run stays FAIL.
func TestPDC05_WaiversAreScopedTimeBoundAndIndependent(t *testing.T) {
	e := newEnv(t)
	gs := e.gated()
	p := e.family(t, tenantA, sp(tenantA), "PDC-W")
	failing := e.testCase(t, p, "above", 2000, "APPROVAL_REQUIRED")
	v := e.reviewedAndApproved(t, e.draft(t, tenantA, p, sp(tenantA), nil, e.clock.Add(-time.Hour), `{"threshold_amount": 5000}`))
	if run, _ := e.s.RunTests(as(tenantA), v.PolicyVersionID, "tester", domain.EventMeta{}); run.RunStatus != domain.TestRunFail {
		t.Fatal("setup: the run must fail")
	}

	req := domain.RequestWaiverParams{PolicyVersionID: v.PolicyVersionID, TestCaseID: failing.TestCaseID, ReasonCode: "KNOWN_GAP",
		Justification: "threshold raised deliberately", ActorID: "requester"}
	if _, err := e.s.RequestWaiver(as(tenantA), req); !errors.Is(err, domain.ErrInvalidWaiver) {
		t.Fatalf("NP-43: a waiver without an expiry is refused, got %v", err)
	}
	req.ExpiresAt = e.clock.Add(400 * 24 * time.Hour)
	if _, err := e.s.RequestWaiver(as(tenantA), req); !errors.Is(err, domain.ErrInvalidWaiver) {
		t.Fatalf("a waiver beyond the maximum life is refused, got %v", err)
	}
	req.ExpiresAt = e.clock.Add(7 * 24 * time.Hour)
	w, err := e.s.RequestWaiver(as(tenantA), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := activate(gs, v); !errors.Is(err, domain.ErrCertificationRequired) {
		t.Fatalf("a requested (unapproved) waiver excuses nothing, got %v", err)
	}
	if _, err := e.s.ApproveWaiver(as(tenantA), domain.WaiverDecisionParams{WaiverID: w.WaiverID, ActorID: "requester"}); !errors.Is(err, domain.ErrSoDConflict) {
		t.Fatalf("the requester cannot approve, got %v", err)
	}
	if _, err := e.s.ApproveWaiver(as(tenantB), domain.WaiverDecisionParams{WaiverID: w.WaiverID, ActorID: "checker"}); !errors.Is(err, domain.ErrWaiverNotFound) {
		t.Fatalf("another tenant cannot see the waiver, got %v", err)
	}
	if _, err := e.s.ApproveWaiver(as(tenantA), domain.WaiverDecisionParams{WaiverID: w.WaiverID, ActorID: "checker"}); err != nil {
		t.Fatal(err)
	}
	c, _ := e.s.Certification(as(tenantA), v.PolicyVersionID)
	if c.Status != domain.CertificationWaived {
		t.Fatalf("an approved waiver makes the version WAIVED (never CERTIFIED): %+v", c)
	}

	// An expired waiver stops counting.
	e.advance(8 * 24 * time.Hour)
	if c, _ := e.s.Certification(as(tenantA), v.PolicyVersionID); c.Status != domain.CertificationFailed {
		t.Fatalf("an expired waiver must stop counting: %+v", c)
	}
	if err := activate(gs, v); !errors.Is(err, domain.ErrCertificationRequired) {
		t.Fatalf("activation under an expired waiver must be refused, got %v", err)
	}
}

// Adding a case changes the suite: the old certification is STALE.
func TestPDC05_SuiteChangeMakesCertificationStale(t *testing.T) {
	e := newEnv(t)
	p := e.family(t, tenantA, sp(tenantA), "PDC-S")
	e.testCase(t, p, "above", 2000, "APPROVAL_REQUIRED")
	v := e.draft(t, tenantA, p, sp(tenantA), nil, e.clock.Add(-time.Hour), `{"threshold_amount": 1000}`)
	if run, _ := e.s.RunTests(as(tenantA), v.PolicyVersionID, "tester", domain.EventMeta{}); run.RunStatus != domain.TestRunPass {
		t.Fatal("setup: pass")
	}
	e.testCase(t, p, "below", 10, "WITHIN_THRESHOLD")
	if c, _ := e.s.Certification(as(tenantA), v.PolicyVersionID); c.Status != domain.CertificationStale {
		t.Fatalf("a new case must make the certification STALE: %+v", c)
	}
	if _, _, err := e.s.CreateTestCase(as(tenantA), domain.CreateTestCaseParams{PolicyID: p.PolicyID, TestCode: "below",
		Layer: domain.TestLayerUnit, Mandatory: true, Description: "x", ActionContext: []byte(`{"amount": 11}`), ExpectedResult: "WITHIN_THRESHOLD", ActorID: "t"}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a test code reused with other content is a conflict, got %v", err)
	}
}

// NP-44/NP-45: a simulation reads only the caller tenant's decisions and
// writes nothing but itself and its event.
func TestPDC05_SimulationIsTenantBoundAndSideEffectFree(t *testing.T) {
	e := newEnv(t)
	p := e.family(t, tenantA, sp(tenantA), "PDC-SIM")
	e.testCase(t, p, "above", 2000, "APPROVAL_REQUIRED")
	v1 := e.draft(t, tenantA, p, sp(tenantA), nil, e.clock.Add(-time.Hour), `{"threshold_amount": 1000}`)
	if run, _ := e.s.RunTests(as(tenantA), v1.PolicyVersionID, "tester", domain.EventMeta{}); run.RunStatus != domain.TestRunPass {
		t.Fatal("setup")
	}
	e.release(t, tenantA, v1)
	candidate := e.draft(t, tenantA, p, sp(tenantA), nil, e.clock.Add(time.Hour), `{"threshold_amount": 100}`)

	for i, amount := range []int{50, 500, 5000} { // 500 changes: WITHIN under 1000, APPROVAL_REQUIRED under 100
		ev := evaluation(tenantA, "sim-"+string(rune('a'+i)))
		ev.ActionContext, _ = json.Marshal(map[string]int{"amount": amount})
		if _, _, err := e.s.SaveEvaluation(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := e.s.SaveEvaluation(context.Background(), evaluation(tenantB, "other-tenant")); err != nil {
		t.Fatal(err)
	}
	before := map[string]int{}
	for _, tbl := range []string{"policy_evaluations", "policy_evidence_outbox", "policy_versions", "policy_version_transitions"} {
		before[tbl] = e.count(t, `SELECT COUNT(*) FROM `+tbl)
	}
	outboxBefore := e.count(t, `SELECT COUNT(*) FROM outbox`)

	run, err := e.s.Simulate(context.Background(), domain.SimulationParams{
		TenantID: tenantA, CandidateVersionID: candidate.PolicyVersionID,
		CorpusFrom: time.Now().Add(-time.Hour), CorpusTo: time.Now().Add(time.Hour), ActorID: "analyst",
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.CorpusSize != 3 || run.ChangedCount != 1 || run.SimStatus != domain.SimulationComplete || run.BaselineVersionID == nil {
		t.Fatalf("NP-44: only tenant A's three decisions, one changes: %+v", run)
	}
	diffs, _ := e.s.ListSimulationDiffs(as(tenantA), run.SimulationID, domain.ListQuery{Limit: 10})
	if len(diffs) != 1 || *diffs[0].BaselineResult != "WITHIN_THRESHOLD" || *diffs[0].CandidateResult != "APPROVAL_REQUIRED" {
		t.Fatalf("diffs: %+v", diffs)
	}
	for tbl, n := range before {
		if got := e.count(t, `SELECT COUNT(*) FROM `+tbl); got != n {
			t.Errorf("NP-45: simulation changed %s (%d -> %d)", tbl, n, got)
		}
	}
	if got := e.count(t, `SELECT COUNT(*) FROM outbox`); got != outboxBefore+1 {
		t.Errorf("only policy.simulation.completed may be written, outbox %d -> %d", outboxBefore, got)
	}
	if _, err := e.s.GetSimulation(as(tenantB), run.SimulationID); !errors.Is(err, domain.ErrSimulationNotFound) {
		t.Fatalf("tenant B must not read tenant A's simulation, got %v", err)
	}
}

// NP-48: a rollback is a new draft of an earlier governing version's payload,
// never backdated and never a mutation.
func TestPDC05_NP48_RollbackNeverBackdates(t *testing.T) {
	e := newEnv(t)
	p := e.family(t, tenantA, sp(tenantA), "PDC-RB")
	v1 := e.release(t, tenantA, e.draft(t, tenantA, p, sp(tenantA), nil, e.clock.Add(-2*time.Hour), `{"threshold_amount": 1000}`))
	v2 := e.release(t, tenantA, e.draft(t, tenantA, p, sp(tenantA), nil, e.clock.Add(-time.Hour), `{"threshold_amount": 2000}`))

	if _, _, err := e.s.RollbackDraft(as(tenantA), v2.PolicyVersionID, e.clock, "author2", "back", "k1", domain.EventMeta{}); !errors.Is(err, domain.ErrNotRollbackEligible) {
		t.Fatalf("the governing version is not a rollback target, got %v", err)
	}
	if _, _, err := e.s.RollbackDraft(as(tenantA), v1.PolicyVersionID, e.clock.Add(-24*time.Hour), "author2", "back", "k2", domain.EventMeta{}); !errors.Is(err, domain.ErrNotRollbackEligible) {
		t.Fatalf("NP-48: a backdated rollback is refused, got %v", err)
	}
	d, created, err := e.s.RollbackDraft(as(tenantA), v1.PolicyVersionID, e.clock, "author2", "threshold 2000 too loose", "k3", domain.EventMeta{})
	if err != nil || !created || d.VersionStatus != domain.StatusDraft || d.Source != "ROLLBACK:"+v1.PolicyVersionID || !jsonSame(d.RulePayload, v1.RulePayload) {
		t.Fatalf("rollback draft: %+v created=%v err=%v", d, created, err)
	}
	if got, _ := e.s.FindPolicyVersionByID(as(tenantA), v1.PolicyVersionID); got.VersionStatus != domain.StatusSuperseded {
		t.Fatalf("the target is never mutated: %s", got.VersionStatus)
	}
}

func jsonSame(a, b []byte) bool {
	var x, y any
	_ = json.Unmarshal(a, &x)
	_ = json.Unmarshal(b, &y)
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return string(xa) == string(ya)
}
