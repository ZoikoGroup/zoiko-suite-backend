package handler_test

import (
	"context"
	"strings"
	"testing"

	"zoiko.io/data-quality-svc/internal/domain"
	"zoiko.io/data-quality-svc/internal/store"
)

// HTTP-layer tests for governed risk acceptance: each route's own permission, the strict request, and the status each
// store error maps to.

type riskStore struct {
	store.Store
	calls int
	err   error
}

func (s *riskStore) RequestRiskAcceptance(_ context.Context, _, runID, _ string, req domain.RequestRiskAcceptanceRequest, _ domain.IdempotencyClaim) (*domain.RiskAcceptance, error) {
	s.calls++
	return &domain.RiskAcceptance{AcceptanceID: "dqk_x", RunID: runID, ResultID: req.ResultID, Status: domain.RiskProposed}, s.err
}

func (s *riskStore) ApproveRiskAcceptance(_ context.Context, _, id, _ string, _ domain.ApproveRiskAcceptanceRequest, _ domain.IdempotencyClaim) (*domain.RiskAcceptance, error) {
	s.calls++
	return &domain.RiskAcceptance{AcceptanceID: id, Status: domain.RiskAccepted}, s.err
}

func (s *riskStore) ListRiskAcceptances(_ context.Context, _, _ string) ([]domain.RiskAcceptance, error) {
	s.calls++
	return []domain.RiskAcceptance{{AcceptanceID: "dqk_x"}}, s.err
}

const (
	riskReqPath = "/v1/data-quality/runs/dqn_x/risk-acceptances"
	riskAppPath = "/v1/data-quality/risk-acceptances/dqk_x:approve"
	riskReqBody = `{"result_id":"dqr_x","reason":"known misfire","evidence_refs":["incident-1"]}`
	riskAppBody = `{"reason":"reviewed"}`
)

func TestRiskAPI_EachRouteNeedsItsOwnPermission(t *testing.T) {
	for _, rt := range []struct{ method, path, body, action string }{
		{"POST", riskReqPath, riskReqBody, "DQC_RISK_REQUEST"},
		{"POST", riskAppPath, riskAppBody, "DQC_RISK_APPROVE"},
		{"GET", riskReqPath, ``, "DATA_QUALITY_READ"},
	} {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			st := &riskStore{}
			if rec := do(t, newServer(t, st, denyAction(t, rt.action)), rt.method, rt.path, rt.body, nil); rec.Code != 403 || st.calls != 0 {
				t.Fatalf("without %s: %d, store calls %d", rt.action, rec.Code, st.calls)
			}
			// Asking, approving and certifying are different permissions: denying the others blocks nothing here.
			for _, other := range []string{"DQC_RISK_REQUEST", "DQC_RISK_APPROVE", "DATA_QUALITY_MANAGE"} {
				if other == rt.action {
					continue
				}
				if rec := do(t, newServer(t, &riskStore{}, denyAction(t, other)), rt.method, rt.path, rt.body, nil); rec.Code >= 400 {
					t.Fatalf("denying %s blocked a route that needs %s: %d %s", other, rt.action, rec.Code, rec.Body.String())
				}
			}
		})
	}
}

func TestRiskAPI_StrictBodies(t *testing.T) {
	st := &riskStore{}
	srv := newServer(t, st, denyAction(t, ""))
	for name, tc := range map[string]struct{ path, body string }{
		"request empty":         {riskReqPath, ``},
		"request unknown field": {riskReqPath, strings.Replace(riskReqBody, `"reason"`, `"approvals":2,"reason"`, 1)},
		"approve empty":         {riskAppPath, ``},
		"approve unknown field": {riskAppPath, `{"reason":"x","force":true}`},
	} {
		if rec := do(t, srv, "POST", tc.path, tc.body, nil); rec.Code != 400 {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if st.calls != 0 {
		t.Fatalf("a malformed request reached the store %d times", st.calls)
	}
	if rec := do(t, srv, "POST", riskReqPath, riskReqBody, nil); rec.Code != 201 {
		t.Fatalf("a valid request: %d", rec.Code)
	}
	if rec := do(t, srv, "POST", riskAppPath, riskAppBody, nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ACCEPTED"`) {
		t.Fatalf("a valid approval: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRiskAPI_ErrorStatuses(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"not found":                 {domain.ErrRiskNotFound, 404},
		"invalid":                   {domain.ErrRiskInvalid, 400},
		"approver is requester":     {domain.ErrRiskApproverIsRequester, 403},
		"certifier not independent": {domain.ErrRiskCertifierNotIndependent, 403},
		"not permitted":             {domain.ErrRiskNotPermitted, 409},
		"not acceptable":            {domain.ErrRiskNotAcceptable, 409},
		"already requested":         {domain.ErrRiskAlreadyRequested, 409},
		"already approved":          {domain.ErrRiskAlreadyApproved, 409},
		"already accepted":          {domain.ErrRiskAlreadyAccepted, 409},
	} {
		srv := newServer(t, &riskStore{err: tc.err}, denyAction(t, ""))
		if rec := do(t, srv, "POST", riskAppPath, riskAppBody, nil); rec.Code != tc.want {
			t.Fatalf("%s: %d, want %d: %s", name, rec.Code, tc.want, rec.Body.String())
		}
	}
	// Certification is a different route: a certifier who approved the risk is refused there with the same 403.
	srv := newServer(t, &facadeStore{err: domain.ErrRiskCertifierNotIndependent}, denyAction(t, ""))
	if rec := do(t, srv, "POST", "/v1/data-quality/runs/dqn_x:certify", ``, nil); rec.Code != 403 {
		t.Fatalf("certifier not independent on :certify: %d %s", rec.Code, rec.Body.String())
	}
}
