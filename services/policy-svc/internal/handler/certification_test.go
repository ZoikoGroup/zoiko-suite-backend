package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"zoiko.io/policy-svc/internal/authz"
	"zoiko.io/policy-svc/internal/domain"
)

// ── stubStore: certification (handler.certificationStore) ───────────────────

var certStub = struct {
	err       error
	calls     int
	simParams domain.SimulationParams
	rbFrom    time.Time
	waiver    domain.RequestWaiverParams
}{}

func resetCertStub() {
	certStub.err, certStub.calls = nil, 0
	certStub.simParams, certStub.rbFrom, certStub.waiver = domain.SimulationParams{}, time.Time{}, domain.RequestWaiverParams{}
}

func (s *stubStore) CreateTestCase(_ context.Context, p domain.CreateTestCaseParams) (*domain.TestCase, bool, error) {
	certStub.calls++
	return &domain.TestCase{TestCaseID: "c", PolicyID: p.PolicyID, TestCode: p.TestCode, Mandatory: p.Mandatory}, true, certStub.err
}
func (s *stubStore) ListTestCases(context.Context, string) ([]*domain.TestCase, error) {
	certStub.calls++
	return nil, certStub.err
}
func (s *stubStore) RetireTestCase(context.Context, string, string, string, domain.EventMeta) (*domain.TestCase, error) {
	certStub.calls++
	return &domain.TestCase{}, certStub.err
}
func (s *stubStore) RunTests(_ context.Context, v, _ string, _ domain.EventMeta) (*domain.TestRun, error) {
	certStub.calls++
	return &domain.TestRun{PolicyVersionID: v, RunStatus: domain.TestRunPass}, certStub.err
}
func (s *stubStore) ListTestRuns(context.Context, string) ([]*domain.TestRun, error) {
	certStub.calls++
	return nil, certStub.err
}
func (s *stubStore) GetTestRun(context.Context, string) (*domain.TestRun, error) {
	certStub.calls++
	return &domain.TestRun{}, certStub.err
}
func (s *stubStore) Certification(_ context.Context, v string) (*domain.Certification, error) {
	certStub.calls++
	return &domain.Certification{PolicyVersionID: v, Status: domain.CertificationNotRun}, certStub.err
}
func (s *stubStore) RequestWaiver(_ context.Context, p domain.RequestWaiverParams) (*domain.Waiver, error) {
	certStub.calls++
	certStub.waiver = p
	return &domain.Waiver{WaiverID: "w"}, certStub.err
}
func (s *stubStore) ApproveWaiver(context.Context, domain.WaiverDecisionParams) (*domain.Waiver, error) {
	certStub.calls++
	return &domain.Waiver{}, certStub.err
}
func (s *stubStore) RevokeWaiver(context.Context, domain.WaiverDecisionParams) (*domain.Waiver, error) {
	certStub.calls++
	return &domain.Waiver{}, certStub.err
}
func (s *stubStore) ListWaivers(context.Context, string) ([]*domain.Waiver, error) {
	certStub.calls++
	return nil, certStub.err
}
func (s *stubStore) Simulate(_ context.Context, p domain.SimulationParams) (*domain.SimulationRun, error) {
	certStub.calls++
	certStub.simParams = p
	return &domain.SimulationRun{SimulationID: "s"}, certStub.err
}
func (s *stubStore) GetSimulation(context.Context, string) (*domain.SimulationRun, error) {
	certStub.calls++
	return &domain.SimulationRun{}, certStub.err
}
func (s *stubStore) ListSimulationDiffs(context.Context, string, domain.ListQuery) ([]*domain.SimulationDiff, error) {
	certStub.calls++
	return nil, certStub.err
}
func (s *stubStore) RollbackDraft(_ context.Context, _ string, from time.Time, _, _, _ string, _ domain.EventMeta) (*domain.PolicyVersion, bool, error) {
	certStub.calls++
	certStub.rbFrom = from
	return &domain.PolicyVersion{PolicyVersionID: "rb", RowVersion: 1}, true, certStub.err
}

const (
	testCasesPath = "/v1/policies/" + policyID + "/test-cases"
	testRunsPath  = "/v1/policy-versions/" + versionID + "/test-runs"
	waiversPath   = "/v1/policy-versions/" + versionID + "/waivers"
	rollbackPath  = "/v1/policy-versions/" + versionID + "/rollback-draft"
)

// Each PDC-05 command asks for its own permission: maintaining the suite,
// running it, requesting and approving waivers and simulating are separate.
func TestPDC05_CommandsAskForTheirOwnPermission(t *testing.T) {
	future := fixedNow.Add(24 * time.Hour).Format(time.RFC3339)
	for _, c := range []struct{ path, body, action string }{
		{testCasesPath, `{"test_code":"a","layer":"unit","description":"d","action_context":{"amount":1},"expected_result":"WITHIN_THRESHOLD"}`, authz.ActionPolicyTestManage},
		{testCasesPath + "/" + versionID + "/retire", ``, authz.ActionPolicyTestManage},
		{testRunsPath, ``, authz.ActionPolicyTestRun},
		{waiversPath, `{"test_case_id":"` + versionID + `","reason_code":"R","justification":"j","expires_at":"` + future + `"}`, authz.ActionPolicyWaiverRequest},
		{"/v1/policy-waivers/" + versionID + "/approve", ``, authz.ActionPolicyWaiverApprove},
		{"/v1/policy-waivers/" + versionID + "/revoke", `{"reason_code":"R"}`, authz.ActionPolicyWaiverApprove},
		{"/v1/simulations", `{"candidate_version_id":"` + versionID + `"}`, authz.ActionPolicySimulate},
	} {
		resetCertStub()
		g := newRig(nil, nil, &stubAuthz{err: domain.ErrAuthorizationDenied})
		expect(t, g.do(http.MethodPost, c.path, c.body), http.StatusForbidden, "AUTHORIZATION_DENIED")
		if g.az.actionType != c.action || certStub.calls != 0 {
			t.Errorf("%s: want %s and no store call, got %s (%d calls)", c.path, c.action, g.az.actionType, certStub.calls)
		}
		resetCertStub()
		g = newRig(nil, nil, nil)
		if w := g.do(http.MethodPost, c.path, c.body); w.Code >= 300 {
			t.Errorf("%s: granted command answered %d: %s", c.path, w.Code, w.Body.String())
		}
	}
}

func TestPDC05_InputRefusals(t *testing.T) {
	resetCertStub()
	g := newRig(nil, nil, nil)
	// NP-43: no expiry, no waiver.
	expect(t, g.do(http.MethodPost, waiversPath, `{"test_case_id":"`+versionID+`","reason_code":"R","justification":"j"}`), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, g.do(http.MethodPost, testCasesPath, `{"test_code":"a","layer":"SMOKE","description":"d","action_context":{},"expected_result":"X"}`), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, g.do(http.MethodPost, testCasesPath, `{"test_code":"a","layer":"UNIT","description":"d","action_context":[1],"expected_result":"X"}`), http.StatusBadRequest, "INVALID_REQUEST")
	// NP-44: a simulation never reads another tenant's decisions.
	expect(t, g.do(http.MethodPost, "/v1/simulations", `{"candidate_version_id":"`+versionID+`","tenant_id":"`+otherTenant+`"}`), http.StatusForbidden, "CONTEXT_UNRESOLVED")
	expect(t, g.do(http.MethodPost, "/v1/simulations", `{"candidate_version_id":"`+versionID+`","corpus_from":"2026-11-02T00:00:00Z","corpus_to":"2026-11-01T00:00:00Z"}`), http.StatusBadRequest, "INVALID_REQUEST")
	// NP-48: a rollback is never backdated.
	expect(t, g.do(http.MethodPost, rollbackPath, `{"rationale":"r","effective_from":"2020-01-01T00:00:00Z"}`), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, g.do(http.MethodPost, rollbackPath, `{}`), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, g.do(http.MethodPost, "/v1/policy-waivers/"+versionID+"/revoke", `{}`), http.StatusBadRequest, "INVALID_REQUEST")
	if certStub.calls != 0 {
		t.Fatalf("a refused PDC-05 request reached the store (%d calls)", certStub.calls)
	}
}

func TestPDC05_SimulationDefaultsToTheLast90DaysOfTheCallersTenant(t *testing.T) {
	resetCertStub()
	g := newRig(nil, nil, nil)
	expect(t, g.do(http.MethodPost, "/v1/simulations", `{"candidate_version_id":"`+versionID+`"}`), http.StatusCreated, "")
	p := certStub.simParams
	if p.TenantID != testTenant || !p.CorpusTo.Equal(fixedNow) || !p.CorpusFrom.Equal(fixedNow.Add(-90*24*time.Hour)) {
		t.Fatalf("simulation params: %+v", p)
	}
}

func TestPDC05_StoreRefusalsAreMapped(t *testing.T) {
	for err, code := range map[error]string{
		domain.ErrNoEvaluator:                                 "NO_EVALUATOR",
		domain.ErrSoDConflict:                                 "SOD_CONFLICT",
		domain.ErrWaiverNotFound:                              "NOT_FOUND",
		fmt.Errorf("%w: x", domain.ErrInvalidWaiver):          "INVALID_REQUEST",
		fmt.Errorf("%w: past", domain.ErrNotRollbackEligible): "INVALID_REQUEST",
	} {
		resetCertStub()
		certStub.err = err
		g := newRig(nil, nil, nil)
		w := g.do(http.MethodPost, "/v1/policy-waivers/"+versionID+"/approve", ``)
		body := decode(t, w)
		if body["code"] != code {
			t.Errorf("%v: want %s, got %d %v", err, code, w.Code, body)
		}
	}
}

// The gate's refusal reaches the caller as 409 CERTIFICATION_REQUIRED.
func TestPDC05_ActivationRefusedUncertifiedIs409(t *testing.T) {
	st := lifecycleStore(nil)
	st.transitionErr = fmt.Errorf("%w: NOT_RUN", domain.ErrCertificationRequired)
	g := newRig(st, nil, nil)
	expect(t, g.do(http.MethodPost, lifecyclePath("activate"), "", ifm), http.StatusConflict, "CERTIFICATION_REQUIRED")
}
