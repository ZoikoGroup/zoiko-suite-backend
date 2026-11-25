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

	"zoiko.io/vat-gst-svc/internal/authz"
	"zoiko.io/vat-gst-svc/internal/domain"
	"zoiko.io/vat-gst-svc/internal/middleware"
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
	st.returns["v1"] = &domain.VATReturn{ReturnID: "v1", TenantID: "tenant-test-01", LegalEntityID: "le-001",
		JurisdictionID: "GB", Status: domain.StatusDraft, CreatedBy: "preparer-1"}
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

func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, w.Body.String())
	}
	return body["code"]
}

var readPaths = []string{"/v1/vat-returns/v1", "/v1/vat-returns"}

func TestReads_RequirePrincipal(t *testing.T) {
	r, _ := hardeningRouter(&actionAuthz{})
	for _, path := range readPaths {
		req := buildRequest(http.MethodGet, path, nil)
		req.Header.Del("X-Principal-Id")
		if w := serve(r, req); w.Code != http.StatusUnauthorized {
			t.Errorf("%s without principal: expected 401, got %d", path, w.Code)
		}
	}
}

func TestReads_DeniedIs403_UnreachableIs503(t *testing.T) {
	r, _ := hardeningRouter(&actionAuthz{deny: map[string]bool{VATReturnRead: true}})
	for _, path := range readPaths {
		if w := serve(r, buildRequest(http.MethodGet, path, nil)); w.Code != http.StatusForbidden {
			t.Errorf("%s denied: expected 403, got %d", path, w.Code)
		}
	}
	r, _ = hardeningRouter(&actionAuthz{down: map[string]bool{VATReturnRead: true}})
	for _, path := range readPaths {
		if w := serve(r, buildRequest(http.MethodGet, path, nil)); w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s authz unreachable: expected 503, got %d", path, w.Code)
		}
	}
}

func TestReads_AllowedAreScoped(t *testing.T) {
	az := &actionAuthz{}
	r, _ := hardeningRouter(az)
	for _, path := range []string{"/v1/vat-returns/v1", "/v1/vat-returns", "/v1/vat-returns?legal_entity_id=le-009"} {
		if w := serve(r, buildRequest(http.MethodGet, path, nil)); w.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d", path, w.Code)
		}
	}
	want := []string{
		"principal-test-01|le-001|" + VATReturnRead,
		"principal-test-01|tenant-test-01|" + VATReturnRead,
		"principal-test-01|le-009|" + VATReturnRead,
	}
	if len(az.checks) != 3 || az.checks[0] != want[0] || az.checks[1] != want[1] || az.checks[2] != want[2] {
		t.Fatalf("unexpected authz checks: %v", az.checks)
	}
}

func fileReq(filedBy string) *http.Request {
	req := buildRequest(http.MethodPost, "/v1/vat-returns/v1/file", domain.FileVATReturnRequest{FiledBy: filedBy})
	req.Header.Set("X-Correlation-ID", "corr-9")
	return req
}

func TestFile_FiledByOfAnotherPrincipalIs403ActorMismatch(t *testing.T) {
	r, st := hardeningRouter(&actionAuthz{})
	w := serve(r, fileReq("someone-else"))
	if w.Code != http.StatusForbidden || errorCode(t, w) != codeActorMismatch {
		t.Fatalf("expected 403 actor_mismatch, got %d %s", w.Code, w.Body.String())
	}
	if st.returns["v1"].Status != domain.StatusDraft {
		t.Fatal("a refused filing must not file the return")
	}
}

func TestFile_AbsentFiledByIsFilledFromPrincipal(t *testing.T) {
	r, st := hardeningRouter(&actionAuthz{})
	w := serve(r, fileReq(""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d %s", w.Code, w.Body.String())
	}
	got := st.returns["v1"]
	if got.FiledBy == nil || *got.FiledBy != "principal-test-01" {
		t.Fatalf("filed_by must be the principal, got %v", got.FiledBy)
	}
	if len(st.metas) != 1 || st.metas[0].ActorID != "principal-test-01" || st.metas[0].CorrelationID != "corr-9" {
		t.Fatalf("outbox event meta must carry principal and correlation, got %+v", st.metas)
	}
}

func TestFile_MatchingFiledByIsAccepted(t *testing.T) {
	r, _ := hardeningRouter(&actionAuthz{})
	if w := serve(r, fileReq("principal-test-01")); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d %s", w.Code, w.Body.String())
	}
}
