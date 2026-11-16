package handler_test

import (
	"context"
	"net/http"
	"testing"

	"zoiko.io/policy-svc/internal/authz"
	"zoiko.io/policy-svc/internal/domain"
)

var opsStub = struct {
	retired      *domain.Policy
	retireErr    error
	retireParams domain.RetirePolicyParams
	retireCalls  int
	redeliverErr error
	redeliver    domain.EvidenceRedeliveryParams
	redeliveries int
}{}

func resetOpsStub() {
	opsStub.retired, opsStub.retireErr, opsStub.retireParams, opsStub.retireCalls = nil, nil, domain.RetirePolicyParams{}, 0
	opsStub.redeliverErr, opsStub.redeliver, opsStub.redeliveries = nil, domain.EvidenceRedeliveryParams{}, 0
}

func (s *stubStore) RetirePolicy(_ context.Context, p domain.RetirePolicyParams) (*domain.Policy, bool, error) {
	opsStub.retireParams, opsStub.retireCalls = p, opsStub.retireCalls+1
	return opsStub.retired, opsStub.retireErr == nil, opsStub.retireErr
}

func (s *stubStore) RedeliverEvidence(_ context.Context, p domain.EvidenceRedeliveryParams) (bool, error) {
	opsStub.redeliver, opsStub.redeliveries = p, opsStub.redeliveries+1
	return opsStub.redeliverErr == nil, opsStub.redeliverErr
}

func retireStore(tenant *string) *stubStore {
	return &stubStore{policy: &domain.Policy{PolicyID: policyID, PolicyType: "APPROVAL_THRESHOLD", TenantID: tenant, PolicyStatus: "ACTIVE", RecordVersion: 2}}
}

const retirePath = "/v1/policies/" + policyID + "/retire"

func TestRetirePolicy_Contract(t *testing.T) {
	resetOpsStub()
	opsStub.retired = &domain.Policy{PolicyID: policyID, PolicyStatus: domain.PolicyStatusRetired, RecordVersion: 3}
	g := newRig(retireStore(strPtr(testTenant)), nil, nil)
	w := g.do(http.MethodPost, retirePath, `{"reason_code":"OBSOLETE","reason":"replaced by P-2"}`, header("If-Match", `"v2"`))
	expect(t, w, http.StatusOK, "")
	if w.Header().Get("ETag") != `"v3"` || opsStub.retireParams.Reason != "OBSOLETE: replaced by P-2" ||
		opsStub.retireParams.ActorID != testPrincipal || opsStub.retireParams.ExpectedVersion != 2 {
		t.Fatalf("retire: ETag %q params %+v", w.Header().Get("ETag"), opsStub.retireParams)
	}
	if g.az.actionType != authz.ActionPolicyVersionRetire {
		t.Fatalf("a tenant family is retired under POLICY_VERSION_RETIRE, got %s", g.az.actionType)
	}

	g = newRig(retireStore(nil), nil, nil)
	expect(t, g.do(http.MethodPost, retirePath, `{"reason_code":"X"}`, header("If-Match", `"v2"`)), http.StatusOK, "")
	if g.az.actionType != authz.ActionPolicyVersionRetireGlobal {
		t.Fatalf("a platform family needs the global grant, got %s", g.az.actionType)
	}
}

func TestRetirePolicy_Refusals(t *testing.T) {
	resetOpsStub()
	g := newRig(retireStore(strPtr(testTenant)), nil, nil)
	expect(t, g.do(http.MethodPost, retirePath, `{"reason_code":"X"}`), http.StatusPreconditionRequired, "precondition_required")
	expect(t, g.do(http.MethodPost, retirePath, `{}`, header("If-Match", `"v2"`)), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, g.do(http.MethodPost, retirePath, `{"reason_code":"X"}`, header("If-Match", `"v1"`)), http.StatusPreconditionFailed, "stale_version")
	expect(t, g.do(http.MethodPost, "/v1/policies/nope/retire", `{"reason_code":"X"}`, header("If-Match", `"v2"`)), http.StatusBadRequest, "INVALID_REQUEST")
	if opsStub.retireCalls != 0 {
		t.Fatal("a refused retirement reached the store")
	}
	denied := newRig(retireStore(strPtr(testTenant)), nil, &stubAuthz{err: domain.ErrAuthorizationDenied})
	expect(t, denied.do(http.MethodPost, retirePath, `{"reason_code":"X"}`, header("If-Match", `"v2"`)), http.StatusForbidden, "AUTHORIZATION_DENIED")

	opsStub.retireErr = domain.ErrPolicyInUse
	expect(t, g.do(http.MethodPost, retirePath, `{"reason_code":"X"}`, header("If-Match", `"v2"`)), http.StatusConflict, "POLICY_IN_USE")
}

func TestCreateVersion_RetiredFamilyIs409(t *testing.T) {
	st := &stubStore{policy: &domain.Policy{PolicyID: policyID, PolicyType: "APPROVAL_THRESHOLD", TenantID: strPtr(testTenant)}, versionErr: domain.ErrPolicyRetired}
	g := newRig(st, nil, nil)
	body := `{"rule_payload":{"threshold_amount":1},"effective_from":"2026-11-01T00:00:00Z"}`
	expect(t, g.do(http.MethodPost, "/v1/policies/"+policyID+"/versions", body), http.StatusConflict, "POLICY_RETIRED")
}

const redeliverPath = "/v1/policy-evaluations/" + evidenceID + "/redeliver"

func TestRedeliverEvidence_Contract(t *testing.T) {
	resetOpsStub()
	st := &stubStore{evaluation: &domain.Evaluation{EvaluationID: evidenceID, LedgerStatus: "PENDING"}}
	g := newRig(st, nil, nil)
	body := expect(t, g.do(http.MethodPost, redeliverPath, `{"reason_code":"LEDGER_FIXED"}`), http.StatusOK, "")
	if body["ledger_status"] != "PENDING" || opsStub.redeliver.TenantID != testTenant || opsStub.redeliver.ActorID != testPrincipal {
		t.Fatalf("redeliver: body %v params %+v", body, opsStub.redeliver)
	}
	if g.az.actionType != authz.ActionPolicyEvidenceRedeliver {
		t.Fatalf("want POLICY_EVIDENCE_REDELIVER, got %s", g.az.actionType)
	}

	expect(t, g.do(http.MethodPost, redeliverPath, `{}`), http.StatusBadRequest, "INVALID_REQUEST")
	opsStub.redeliverErr = domain.ErrEvidenceAlreadyRecorded
	expect(t, g.do(http.MethodPost, redeliverPath, `{"reason_code":"X"}`), http.StatusConflict, "EVIDENCE_ALREADY_RECORDED")
	opsStub.redeliverErr = domain.ErrEvaluationNotFound
	expect(t, g.do(http.MethodPost, redeliverPath, `{"reason_code":"X"}`), http.StatusNotFound, "NOT_FOUND")
	resetOpsStub()
	denied := newRig(st, nil, &stubAuthz{err: domain.ErrAuthorizationDenied})
	expect(t, denied.do(http.MethodPost, redeliverPath, `{"reason_code":"X"}`), http.StatusForbidden, "AUTHORIZATION_DENIED")
	if opsStub.redeliveries != 0 {
		t.Fatal("a denied redelivery reached the store")
	}
}
