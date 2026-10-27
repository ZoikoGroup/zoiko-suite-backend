package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/obligations-svc/internal/domain"
	"zoiko.io/obligations-svc/internal/handler"
	svcmiddleware "zoiko.io/obligations-svc/internal/middleware"
)

type stubRegistry struct {
	err   error
	calls []string
}

func (r *stubRegistry) CheckEntity(_ context.Context, tenantID, legalEntityID string) error {
	r.calls = append(r.calls, tenantID+"/"+legalEntityID)
	return r.err
}

func completionRouter(s *stubStore, reg *stubRegistry, a *stubAuthz) chi.Router {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	h := handler.New(s, &stubValidator{}, a, zap.NewNop()).WithRegistry(reg)
	handler.RegisterRoutes(r, h)
	handler.RegisterApplicabilityRoutes(r, h)
	return r
}

func do(t *testing.T, r chi.Router, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := withIdentity(httptest.NewRequest(method, path, bytes.NewBufferString(body)))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func problem(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("error Content-Type = %q, want application/problem+json (RFC 9457); body %s", ct, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("problem body: %v", err)
	}
	for _, k := range []string{"type", "title", "status", "code", "error"} {
		if _, ok := body[k]; !ok {
			t.Fatalf("problem document has no %q: %v", k, body)
		}
	}
	return body
}

func existing() *domain.Obligation {
	return &domain.Obligation{ObligationID: "11111111-0000-0000-0000-000000000001", LegalEntityID: "le-1",
		ObligationStatus: "OPEN", Version: 3, CreatedAt: time.Now().UTC()}
}

func TestCreateObligation_LegalEntityMustExistInTenant(t *testing.T) {
	o := existing()
	for _, tc := range []struct {
		name     string
		regErr   error
		wantCode int
		wantErr  string
	}{
		{"unknown or other tenant's entity", domain.ErrLegalEntityNotFound, http.StatusUnprocessableEntity, "legal_entity_not_found"},
		{"registry down fails closed", domain.ErrRegistryUnavailable, http.StatusServiceUnavailable, "registry_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &stubStore{obligation: o, obligationCreated: true}
			reg := &stubRegistry{err: tc.regErr}
			w := do(t, completionRouter(s, reg, &stubAuthz{}), http.MethodPost, "/v1/obligations", validCreateBody(), nil)
			if w.Code != tc.wantCode {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.wantCode, w.Body.String())
			}
			if got := problem(t, w)["error"]; got != tc.wantErr {
				t.Fatalf("error %v, want %s", got, tc.wantErr)
			}
			if len(reg.calls) != 1 || reg.calls[0] != testTenant+"/le-1" {
				t.Fatalf("registry asked %v; want the caller's tenant and the body's entity", reg.calls)
			}
			if s.createParams.LegalEntityID != "" {
				t.Fatal("the obligation was written although the entity was refused")
			}
		})
	}
}

func TestCreateObligation_201CarriesLocationAndETag(t *testing.T) {
	o := existing()
	s := &stubStore{obligation: o, obligationCreated: true}
	w := do(t, completionRouter(s, &stubRegistry{}, &stubAuthz{}), http.MethodPost, "/v1/obligations", validCreateBody(), nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); loc != "/v1/obligations/"+o.ObligationID {
		t.Fatalf("Location = %q", loc)
	}
	if et := w.Header().Get("ETag"); et != `"3"` {
		t.Fatalf("ETag = %q", et)
	}
}

func TestUpdateObligationStatus_UnknownValueIs400Not409(t *testing.T) {
	s := &stubStore{findObligation: existing()}
	w := do(t, completionRouter(s, &stubRegistry{}, &stubAuthz{}), http.MethodPost,
		"/v1/obligations/"+existing().ObligationID+"/status", `{"obligation_status":"FOO"}`, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", w.Code, w.Body.String())
	}
	if problem(t, w)["error"] != "invalid_field" {
		t.Fatalf("body %s", w.Body.String())
	}
}

func TestErrorsCarryTheStableClass(t *testing.T) {
	s := &stubStore{findObligation: existing(), updateErr: domain.ErrVersionMismatch}
	w := do(t, completionRouter(s, &stubRegistry{}, &stubAuthz{}), http.MethodPatch,
		"/v1/obligations/"+existing().ObligationID, `{"severity_level":"LOW"}`, map[string]string{"If-Match": `"2"`})
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if c := problem(t, w)["error_class"]; c != "CONCURRENCY_CONFLICT" {
		t.Fatalf("error_class = %v, want CONCURRENCY_CONFLICT (§16)", c)
	}
	denied := do(t, completionRouter(&stubStore{findObligation: existing()}, &stubRegistry{}, &stubAuthz{err: domain.ErrAuthorizationDenied}),
		http.MethodGet, "/v1/obligations/"+existing().ObligationID, "", nil)
	if c := problem(t, denied)["error_class"]; c != "AUTHORIZATION_DENIED" {
		t.Fatalf("403 error_class = %v", c)
	}
}

func TestPatchObligation_NeedsIfMatchAndAuthorizesAtTheEntity(t *testing.T) {
	s := &stubStore{findObligation: existing(), updated: existing(), transitioned: true}
	a := &stubAuthz{}
	r := completionRouter(s, &stubRegistry{}, a)
	w := do(t, r, http.MethodPatch, "/v1/obligations/"+existing().ObligationID, `{"severity_level":"LOW"}`, nil)
	if w.Code != http.StatusPreconditionRequired {
		t.Fatalf("no If-Match: status %d, want 428", w.Code)
	}
	w = do(t, r, http.MethodPatch, "/v1/obligations/"+existing().ObligationID, `{"severity_level":"LOW"}`, map[string]string{"If-Match": `W/"3"`})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if s.updateParams.ExpectedVersion != 3 || s.updateParams.ActorPrincipalID != testPrincipal || *s.updateParams.SeverityLevel != "LOW" {
		t.Fatalf("store got %+v", s.updateParams)
	}
	if len(a.calls) != 1 || a.calls[0] != "OBLIGATION_UPDATE" {
		t.Fatalf("authz calls %v", a.calls)
	}
	w = do(t, r, http.MethodPatch, "/v1/obligations/"+existing().ObligationID, `{"obligation_code":"X"}`, map[string]string{"If-Match": `"3"`})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("an immutable field must be refused: %d", w.Code)
	}
}

func TestFilingStatus_ValidationAndRoute(t *testing.T) {
	s := &stubStore{findObligation: existing(), filingReq: &domain.FilingRequirement{FilingRequirementID: "f-1", FilingStatus: "SUBMITTED", RowVersion: 2}}
	r := completionRouter(s, &stubRegistry{}, &stubAuthz{})
	path := "/v1/obligations/" + existing().ObligationID + "/filing-requirements/f-1/status"
	if w := do(t, r, http.MethodPost, path, `{"filing_status":"DONE"}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown filing status: %d", w.Code)
	}
	if w := do(t, r, http.MethodPost, path, `{"filing_status":"REJECTED"}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("rejection without a reason: %d", w.Code)
	}
	w := do(t, r, http.MethodPost, path, `{"filing_status":"SUBMITTED"}`, map[string]string{"If-Match": `"1"`})
	if w.Code != http.StatusOK || w.Header().Get("ETag") != `"2"` {
		t.Fatalf("submit: %d etag %q %s", w.Code, w.Header().Get("ETag"), w.Body.String())
	}
	if s.filingStatusParams.ExpectedVersion != 1 || s.filingStatusParams.NewStatus != "SUBMITTED" {
		t.Fatalf("store got %+v", s.filingStatusParams)
	}
}

func TestListObligations_FullPageCarriesTheCursor(t *testing.T) {
	page := make([]*domain.Obligation, 2)
	for i := range page {
		page[i] = existing()
	}
	s := &stubStore{list: page}
	r := completionRouter(s, &stubRegistry{}, &stubAuthz{})
	w := do(t, r, http.MethodGet, "/v1/obligations?legal_entity_id=le-1&limit=2", "", nil)
	if w.Code != http.StatusOK || w.Header().Get("X-Next-Cursor") == "" || w.Header().Get("Link") == "" {
		t.Fatalf("status %d, next %q, link %q", w.Code, w.Header().Get("X-Next-Cursor"), w.Header().Get("Link"))
	}
	next := w.Header().Get("X-Next-Cursor")
	if w := do(t, r, http.MethodGet, "/v1/obligations?legal_entity_id=le-1&cursor="+next, "", nil); w.Code != http.StatusOK {
		t.Fatalf("following the cursor: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, r, http.MethodGet, "/v1/obligations?legal_entity_id=le-1&cursor=not-a-cursor", "", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("a forged cursor: %d", w.Code)
	}
	if w := do(t, r, http.MethodGet, "/v1/obligations?legal_entity_id=le-1&limit=201", "", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("limit above 200: %d", w.Code)
	}
}

func TestApplicability_ValidationReplayAndLocation(t *testing.T) {
	d := &domain.ApplicabilityDecision{ApplicabilityDecisionID: "ad-1"}
	s := &stubStore{findObligation: existing(), applicabilityDecision: d}
	r := completionRouter(s, &stubRegistry{}, &stubAuthz{})
	path := "/v1/obligations/" + existing().ObligationID + "/applicability-decisions"
	bad := applicabilityBody(testPrincipal, "", "rule-engine")
	bad = string(bytes.Replace([]byte(bad), []byte(`"APPLICABLE"`), []byte(`"UNASSESSED"`), 1))
	if w := do(t, r, http.MethodPost, path, bad, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("UNASSESSED is never stored: %d", w.Code)
	}
	both := applicabilityBody(testPrincipal, testPrincipal, "rule-engine")
	if w := do(t, r, http.MethodPost, path, both, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("both a human and a system: %d", w.Code)
	}
	good := applicabilityBody(testPrincipal, "", "rule-engine")
	w := do(t, r, http.MethodPost, path, good, nil)
	if w.Code != http.StatusCreated || w.Header().Get("Location") == "" {
		t.Fatalf("create: %d location %q", w.Code, w.Header().Get("Location"))
	}
	s.applicabilityReplayed = true
	if w := do(t, r, http.MethodPost, path, good, nil); w.Code != http.StatusOK {
		t.Fatalf("an idempotent replay answers 200, got %d", w.Code)
	}
	if s.applicabilityParams.RequestHash == "" {
		t.Fatal("the store was given no request hash to compare a retry against")
	}
	s.applicabilityReplayed, s.applicabilityCreateErr = false, domain.ErrIdempotencyKeyConflict
	if w := do(t, r, http.MethodPost, path, good, nil); w.Code != http.StatusConflict || problem(t, w)["error_class"] != "IDEMPOTENCY_MISMATCH" {
		t.Fatalf("same key, different body: %d %s", w.Code, w.Body.String())
	}
}

func TestDefinitions_CreateAndRaiseFromDefinition(t *testing.T) {
	v := &domain.ObligationDefinitionVersion{Version: 2, JurisdictionID: "jur-1", ObligationType: "FILING",
		ObligationSourceType: "JURISDICTION_RULE", ObligationSourceID: "rule-1", SourceReference: "IN-GST-FILING-RULE-07",
		SeverityLevel: "HIGH", ResponsibleFunction: "Tax"}
	def := &domain.ObligationDefinition{ObligationDefinitionID: "def-1", CurrentVersion: 2, DefinitionStatus: "ACTIVE", Current: v}
	s := &stubStore{definition: def, obligation: existing(), obligationCreated: true}
	a := &stubAuthz{}
	r := completionRouter(s, &stubRegistry{}, a)

	body := `{"definition_code":"GSTR-3B","jurisdiction_id":"jur-1","obligation_type":"FILING","obligation_source_type":"JURISDICTION_RULE",
		"obligation_source_id":"rule-1","source_reference":"IN-GST-FILING-RULE-07","severity_level":"HIGH","responsible_function":"Tax"}`
	w := do(t, r, http.MethodPost, "/v1/obligation-definitions", body, nil)
	if w.Code != http.StatusCreated || w.Header().Get("Location") != "/v1/obligation-definitions/def-1" {
		t.Fatalf("create definition: %d %q", w.Code, w.Header().Get("Location"))
	}
	if a.calls[len(a.calls)-1] != "OBLIGATION_DEFINITION_MANAGE" {
		t.Fatalf("authz %v", a.calls)
	}

	raise := `{"obligation_definition_id":"def-1","legal_entity_id":"le-1","obligation_code":"GST-2026-10",
		"due_date":"2026-11-20T00:00:00Z","created_by_principal_id":"` + testPrincipal + `"}`
	w = do(t, r, http.MethodPost, "/v1/obligations", raise, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("raise from definition: %d %s", w.Code, w.Body.String())
	}
	p := s.createParams
	if p.ObligationDefinitionID != "def-1" || p.DefinitionVersion != 2 || p.SourceReference != v.SourceReference || p.SeverityLevel != "HIGH" {
		t.Fatalf("fields not taken from the definition: %+v", p)
	}
	clash := `{"obligation_definition_id":"def-1","legal_entity_id":"le-1","obligation_code":"GST-2026-11","severity_level":"LOW",
		"due_date":"2026-11-20T00:00:00Z","created_by_principal_id":"` + testPrincipal + `"}`
	if w := do(t, r, http.MethodPost, "/v1/obligations", clash, nil); w.Code != http.StatusBadRequest || problem(t, w)["field"] != "severity_level" {
		t.Fatalf("a field that contradicts the definition: %d %s", w.Code, w.Body.String())
	}
	version := `{"jurisdiction_id":"jur-1","obligation_type":"FILING","obligation_source_type":"JURISDICTION_RULE",
		"obligation_source_id":"rule-1","source_reference":"IN-GST-FILING-RULE-07","severity_level":"LOW","responsible_function":"Tax",
		"change_reason":"severity review"}`
	if w := do(t, r, http.MethodPost, "/v1/obligation-definitions/def-1/versions", version, nil); w.Code != http.StatusPreconditionRequired {
		t.Fatalf("a new version without If-Match: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, r, http.MethodPost, "/v1/obligation-definitions/def-1/versions", version, map[string]string{"If-Match": `"2"`}); w.Code != http.StatusOK {
		t.Fatalf("a new version with If-Match: %d %s", w.Code, w.Body.String())
	}
	if s.newVersionParams.ExpectedVersion != 2 || s.newVersionParams.ChangeReason != "severity review" {
		t.Fatalf("store got %+v", s.newVersionParams)
	}
}
