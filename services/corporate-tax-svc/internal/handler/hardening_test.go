package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/corporate-tax-svc/internal/authz"
	"zoiko.io/corporate-tax-svc/internal/domain"
	"zoiko.io/corporate-tax-svc/internal/middleware"
)

// actionAuthz denies the listed actions and fails the listed ones as
// unreachable; everything else is allowed. It records each check.
type actionAuthz struct {
	deny, down map[string]bool
	checks     []string // "principal|scope|action"
}

func (a *actionAuthz) CheckAllowed(_ context.Context, principalID, scope, action string) error {
	a.checks = append(a.checks, principalID+"|"+scope+"|"+action)
	if a.deny[action] {
		return authz.ErrAuthorizationDenied
	}
	if a.down[action] {
		return errors.New("authorization-svc unreachable")
	}
	return nil
}

func hardeningRouter(az AuthzChecker) (chi.Router, *stubStore) {
	st := newStubStore()
	submitter := "submitter-1"
	st.returns["r1"] = &domain.TaxReturn{ReturnID: "r1", TenantID: "tenant-test-01", LegalEntityID: "le-001",
		JurisdictionID: "GB", Status: domain.StatusSubmitted, SubmittedBy: &submitter, CreatedBy: "preparer-1"}
	st.nexus["n1"] = &domain.NexusRecord{NexusRecordID: "n1", LegalEntityID: "le-002", JurisdictionID: "GB"}
	r := chi.NewRouter()
	r.Use(middleware.TenantContextMiddleware)
	RegisterRoutes(r, New(st, az, zap.NewNop()))
	return r, st
}

func serve(r http.Handler, req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

var readCases = []struct{ path, action string }{
	{"/v1/corporate-tax-returns/r1", ActionTaxReturnRead},
	{"/v1/corporate-tax-returns", ActionTaxReturnRead},
	{"/v1/nexus-records/n1", ActionNexusRecordRead},
	{"/v1/nexus-records", ActionNexusRecordRead},
}

func TestReads_RequirePrincipal(t *testing.T) {
	r, _ := hardeningRouter(&actionAuthz{})
	for _, c := range readCases {
		req := buildRequest(http.MethodGet, c.path, nil)
		req.Header.Del("X-Principal-Id")
		if w := serve(r, req); w.Code != http.StatusUnauthorized {
			t.Errorf("%s without principal: expected 401, got %d", c.path, w.Code)
		}
	}
}

func TestReads_DeniedIs403_UnreachableIs503(t *testing.T) {
	for _, c := range readCases {
		r, _ := hardeningRouter(&actionAuthz{deny: map[string]bool{c.action: true}})
		if w := serve(r, buildRequest(http.MethodGet, c.path, nil)); w.Code != http.StatusForbidden {
			t.Errorf("%s denied: expected 403, got %d", c.path, w.Code)
		}
		r, _ = hardeningRouter(&actionAuthz{down: map[string]bool{c.action: true}})
		if w := serve(r, buildRequest(http.MethodGet, c.path, nil)); w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s authz unreachable: expected 503, got %d", c.path, w.Code)
		}
	}
}

func TestReads_AllowedAreScoped(t *testing.T) {
	az := &actionAuthz{}
	r, _ := hardeningRouter(az)
	paths := []string{"/v1/corporate-tax-returns/r1", "/v1/corporate-tax-returns",
		"/v1/corporate-tax-returns?legal_entity_id=le-009", "/v1/nexus-records/n1", "/v1/nexus-records"}
	for _, p := range paths {
		if w := serve(r, buildRequest(http.MethodGet, p, nil)); w.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d", p, w.Code)
		}
	}
	want := []string{
		"principal-test-01|le-001|" + ActionTaxReturnRead,
		"principal-test-01|tenant-test-01|" + ActionTaxReturnRead,
		"principal-test-01|le-009|" + ActionTaxReturnRead,
		"principal-test-01|le-002|" + ActionNexusRecordRead,
		"principal-test-01|tenant-test-01|" + ActionNexusRecordRead,
	}
	if len(az.checks) != len(want) {
		t.Fatalf("unexpected authz checks: %v", az.checks)
	}
	for i := range want {
		if az.checks[i] != want[i] {
			t.Fatalf("check %d: want %s, got %s", i, want[i], az.checks[i])
		}
	}
}

func assessReq(principal string) *http.Request {
	req := buildRequest(http.MethodPost, "/v1/corporate-tax-returns/r1/assess",
		domain.AssessTaxReturnRequest{AssessedTaxAmount: 10, AssessmentReference: "REF-1"})
	req.Header.Set("X-Principal-Id", principal)
	req.Header.Set("X-Correlation-ID", "corr-5")
	return req
}

func TestAssess_BySubmitterIs403SegregationOfDuties(t *testing.T) {
	r, st := hardeningRouter(&actionAuthz{})
	w := serve(r, assessReq("submitter-1"))
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != http.StatusForbidden || body["code"] != codeSegregationOfDuties {
		t.Fatalf("expected 403 segregation_of_duties, got %d %s", w.Code, w.Body.String())
	}
	if st.returns["r1"].Status != domain.StatusSubmitted || len(st.metas) != 0 {
		t.Fatal("a refused assessment must not reach the store")
	}
}

func TestAssess_ByOtherPrincipalSucceeds(t *testing.T) {
	r, st := hardeningRouter(&actionAuthz{})
	if w := serve(r, assessReq("assessor-1")); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d %s", w.Code, w.Body.String())
	}
	if len(st.metas) != 1 || st.metas[0].ActorID != "assessor-1" || st.metas[0].CorrelationID != "corr-5" {
		t.Fatalf("outbox event meta must carry the assessor and correlation, got %+v", st.metas)
	}
}

func TestAssess_DeniedIs403(t *testing.T) {
	r, _ := hardeningRouter(&actionAuthz{deny: map[string]bool{ActionTaxReturnAssess: true}})
	if w := serve(r, assessReq("assessor-1")); w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}
