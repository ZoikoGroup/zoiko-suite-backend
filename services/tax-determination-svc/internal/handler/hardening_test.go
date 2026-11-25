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

	"zoiko.io/tax-determination-svc/internal/authz"
	"zoiko.io/tax-determination-svc/internal/domain"
	"zoiko.io/tax-determination-svc/internal/middleware"
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
	h := New(st, az, &stubRulesClient{}, &stubJurisdiction{}, &stubRegistry{}, zap.NewNop())
	r := chi.NewRouter()
	r.Use(middleware.TenantContextMiddleware)
	RegisterRoutes(r, h)
	return r, st
}

func seedDetermination(st *stubStore, evaluator string) *domain.TaxDetermination {
	d := &domain.TaxDetermination{DeterminationID: "tdet-1", TenantID: "tenant-test-01", LegalEntityID: "le-001",
		JurisdictionID: "uk-england", Status: domain.StatusCalculated, EvaluatedBy: evaluator}
	st.determinations[d.DeterminationID] = d
	return d
}

func serve(r http.Handler, req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, w.Body.String())
	}
	return body["code"]
}

func TestReads_RequirePrincipal(t *testing.T) {
	r, st := hardeningRouter(&actionAuthz{})
	seedDetermination(st, "evaluator-1")
	for _, path := range []string{"/v1/tax-determinations/tdet-1", "/v1/tax-determinations"} {
		req := buildRequest(http.MethodGet, path, nil)
		req.Header.Del("X-Principal-Id")
		if w := serve(r, req); w.Code != http.StatusUnauthorized {
			t.Errorf("%s without principal: expected 401, got %d", path, w.Code)
		}
	}
}

func TestReads_DeniedIs403_UnreachableIs503(t *testing.T) {
	deny := &actionAuthz{deny: map[string]bool{ActionTaxDeterminationRead: true}}
	r, st := hardeningRouter(deny)
	seedDetermination(st, "evaluator-1")
	for _, path := range []string{"/v1/tax-determinations/tdet-1", "/v1/tax-determinations"} {
		if w := serve(r, buildRequest(http.MethodGet, path, nil)); w.Code != http.StatusForbidden {
			t.Errorf("%s denied: expected 403, got %d", path, w.Code)
		}
	}
	down := &actionAuthz{down: map[string]bool{ActionTaxDeterminationRead: true}}
	r, st = hardeningRouter(down)
	seedDetermination(st, "evaluator-1")
	if w := serve(r, buildRequest(http.MethodGet, "/v1/tax-determinations/tdet-1", nil)); w.Code != http.StatusServiceUnavailable {
		t.Errorf("authz unreachable: expected 503, got %d", w.Code)
	}
}

func TestReads_AllowedAreScopedToLegalEntityAndTenant(t *testing.T) {
	az := &actionAuthz{}
	r, st := hardeningRouter(az)
	seedDetermination(st, "evaluator-1")
	if w := serve(r, buildRequest(http.MethodGet, "/v1/tax-determinations/tdet-1", nil)); w.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d", w.Code)
	}
	if w := serve(r, buildRequest(http.MethodGet, "/v1/tax-determinations", nil)); w.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d", w.Code)
	}
	want := []string{
		"user-test-01|le-001|" + ActionTaxDeterminationRead,
		"user-test-01|tenant-test-01|" + ActionTaxDeterminationRead,
	}
	if len(az.checks) != 2 || az.checks[0] != want[0] || az.checks[1] != want[1] {
		t.Fatalf("unexpected authz checks: %v", az.checks)
	}
}

func baseDetermineRequest() domain.DetermineTaxRequest {
	return withTAX03(domain.DetermineTaxRequest{TransactionID: "tx-1", SourceModule: "INVOICE", LegalEntityID: "le-001",
		JurisdictionID: "uk-england", TaxCategory: "VAT", GrossAmount: 100, Currency: "GBP", EffectiveFrom: "2026-01-01"})
}

func TestDetermine_EvaluatedByOfAnotherPrincipalIs403ActorMismatch(t *testing.T) {
	r, st := hardeningRouter(&actionAuthz{})
	body := baseDetermineRequest()
	body.EvaluatedBy = "someone-else"
	w := serve(r, buildRequest(http.MethodPost, "/v1/tax-determinations", body))
	if w.Code != http.StatusForbidden || errorCode(t, w) != codeActorMismatch {
		t.Fatalf("expected 403 actor_mismatch, got %d %s", w.Code, w.Body.String())
	}
	if len(st.determinations) != 0 {
		t.Fatal("a refused determination must not be stored")
	}
}

func TestDetermine_AbsentEvaluatedByIsFilledFromPrincipal(t *testing.T) {
	r, st := hardeningRouter(&actionAuthz{})
	req := buildRequest(http.MethodPost, "/v1/tax-determinations", baseDetermineRequest())
	req.Header.Set("X-Correlation-ID", "corr-7")
	w := serve(r, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d %s", w.Code, w.Body.String())
	}
	var det domain.TaxDetermination
	_ = json.Unmarshal(w.Body.Bytes(), &det)
	if det.EvaluatedBy != "user-test-01" {
		t.Fatalf("evaluated_by must be the principal, got %q", det.EvaluatedBy)
	}
	if len(st.metas) != 1 || st.metas[0].ActorID != "user-test-01" || st.metas[0].CorrelationID != "corr-7" {
		t.Fatalf("outbox event meta must carry principal and correlation, got %+v", st.metas)
	}
}

func overrideRequest(principal, updatedBy string) *http.Request {
	req := buildRequest(http.MethodPost, "/v1/tax-determinations/tdet-1/override",
		domain.OverrideTaxRequest{OverriddenTaxAmount: 5, Reason: "certificate verified", UpdatedBy: updatedBy})
	req.Header.Set("X-Principal-Id", principal)
	return req
}

func TestOverride_ByEvaluatorIs403SegregationOfDuties(t *testing.T) {
	r, st := hardeningRouter(&actionAuthz{})
	seedDetermination(st, "evaluator-1")
	w := serve(r, overrideRequest("evaluator-1", ""))
	if w.Code != http.StatusForbidden || errorCode(t, w) != codeSegregationOfDuties {
		t.Fatalf("expected 403 segregation_of_duties, got %d %s", w.Code, w.Body.String())
	}
	if st.determinations["tdet-1"].Status != domain.StatusCalculated {
		t.Fatal("a refused override must not change the determination")
	}
}

func TestOverride_UpdatedByOfAnotherPrincipalIs403ActorMismatch(t *testing.T) {
	r, st := hardeningRouter(&actionAuthz{})
	seedDetermination(st, "evaluator-1")
	w := serve(r, overrideRequest("reviewer-1", "someone-else"))
	if w.Code != http.StatusForbidden || errorCode(t, w) != codeActorMismatch {
		t.Fatalf("expected 403 actor_mismatch, got %d %s", w.Code, w.Body.String())
	}
}

func TestOverride_ByOtherPrincipalSucceeds(t *testing.T) {
	r, st := hardeningRouter(&actionAuthz{})
	seedDetermination(st, "evaluator-1")
	w := serve(r, overrideRequest("reviewer-1", "reviewer-1"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d %s", w.Code, w.Body.String())
	}
	if len(st.metas) != 1 || st.metas[0].ActorID != "reviewer-1" {
		t.Fatalf("override event must carry the overriding principal, got %+v", st.metas)
	}
}

func TestOverride_DeniedIs403(t *testing.T) {
	r, st := hardeningRouter(&actionAuthz{deny: map[string]bool{ActionTaxDeterminationOverride: true}})
	seedDetermination(st, "evaluator-1")
	if w := serve(r, overrideRequest("reviewer-1", "")); w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}
