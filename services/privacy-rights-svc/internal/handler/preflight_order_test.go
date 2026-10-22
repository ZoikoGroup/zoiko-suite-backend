package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authzpkg "zoiko.io/privacy-rights-svc/internal/authz"
	"zoiko.io/privacy-rights-svc/internal/domain"
	"zoiko.io/privacy-rights-svc/internal/handler"
	"zoiko.io/privacy-rights-svc/internal/middleware"
)

// Documented pre-flight: X-Principal-Id (401 AUTHENTICATION_FAILED), then
// X-Tenant-Id (401 TENANT_SCOPE_REQUIRED), then the remaining headers, then the
// authorization grant. The tenant middleware runs before the handler, so it has
// to apply the principal check first or a request with neither header would be
// told about the tenant.

type countingAuthz struct {
	mu    sync.Mutex
	deny  bool
	calls int
}

func (a *countingAuthz) CheckAllowed(_ context.Context, _, _, _ string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if a.deny {
		return authzpkg.ErrAuthorizationDenied
	}
	return nil
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("body is not the JSON envelope: %q", w.Body.String())
	}
	if m["error"] == "" {
		t.Fatalf("envelope has no message: %q", w.Body.String())
	}
	return m["error_code"]
}

func allRoutes() []struct{ method, path string } {
	id := uuid.NewString()
	var out []struct{ method, path string }
	for _, prefix := range []string{"/privacy/rights-requests", "/v1/privacy/rights-requests"} {
		out = append(out,
			struct{ method, path string }{http.MethodPost, prefix},
			struct{ method, path string }{http.MethodGet, prefix + "?subject_ref=s"},
			struct{ method, path string }{http.MethodGet, prefix + "/" + id},
			struct{ method, path string }{http.MethodPost, prefix + "/" + id + "/identity-verification"},
			struct{ method, path string }{http.MethodPost, prefix + "/" + id + "/discovery-manifests"},
			struct{ method, path string }{http.MethodGet, prefix + "/" + id + "/discovery-manifests"},
			struct{ method, path string }{http.MethodPost, prefix + "/" + id + "/wfc-process-ref"},
			struct{ method, path string }{http.MethodPost, prefix + "/" + id + "/close"},
		)
	}
	return out
}

func TestPreflightOrder_NeitherPrincipalNorTenant_IsAuthenticationFailed(t *testing.T) {
	az := &countingAuthz{}
	r := newTestRouter(newStubStore(), az)
	for _, rt := range allRoutes() {
		w := doRawRequest(r, rt.method, rt.path, nil, map[string]string{"X-Principal-Id": ""})
		if w.Code != http.StatusUnauthorized || errorCode(t, w) != domain.ErrCodeAuthenticationFailed {
			t.Errorf("%s %s: want 401 %s, got %d %s", rt.method, rt.path, domain.ErrCodeAuthenticationFailed, w.Code, w.Body.String())
		}
	}
	if az.calls != 0 {
		t.Fatalf("authorization must not be consulted for an unauthenticated caller (%d calls)", az.calls)
	}
}

func TestPreflightOrder_MissingPrincipalWithValidTenant_IsAuthenticationFailed(t *testing.T) {
	r := newTestRouter(newStubStore(), &countingAuthz{})
	for _, rt := range allRoutes() {
		w := doRawRequest(r, rt.method, rt.path, nil, map[string]string{"X-Principal-Id": "", "X-Tenant-Id": testTenant})
		if w.Code != http.StatusUnauthorized || errorCode(t, w) != domain.ErrCodeAuthenticationFailed {
			t.Errorf("%s %s: want 401 %s, got %d %s", rt.method, rt.path, domain.ErrCodeAuthenticationFailed, w.Code, w.Body.String())
		}
	}
	// A whitespace-only principal is no principal.
	w := doRawRequest(r, http.MethodGet, "/privacy/rights-requests?subject_ref=s", nil, map[string]string{"X-Principal-Id": "   ", "X-Tenant-Id": testTenant})
	if w.Code != http.StatusUnauthorized || errorCode(t, w) != domain.ErrCodeAuthenticationFailed {
		t.Errorf("whitespace principal: got %d %s", w.Code, w.Body.String())
	}
}

func TestPreflightOrder_ValidPrincipalWithMissingTenant_IsTenantScopeRequired(t *testing.T) {
	az := &countingAuthz{}
	r := newTestRouter(newStubStore(), az)
	for _, rt := range allRoutes() {
		w := doRawRequest(r, rt.method, rt.path, nil, nil) // principal-01 only
		if w.Code != http.StatusUnauthorized || errorCode(t, w) != domain.ErrCodeTenantScopeRequired {
			t.Errorf("%s %s: want 401 %s, got %d %s", rt.method, rt.path, domain.ErrCodeTenantScopeRequired, w.Code, w.Body.String())
		}
	}
	w := doRawRequest(r, http.MethodGet, "/privacy/rights-requests?subject_ref=s", nil, map[string]string{"X-Tenant-Id": "   "})
	if w.Code != http.StatusUnauthorized || errorCode(t, w) != domain.ErrCodeTenantScopeRequired {
		t.Errorf("whitespace tenant: got %d %s", w.Code, w.Body.String())
	}
	if az.calls != 0 {
		t.Fatalf("authorization must not be consulted without a tenant (%d calls)", az.calls)
	}
}

func TestPreflightOrder_ValidPrincipalAndTenant_ProcessesNormally(t *testing.T) {
	az := &countingAuthz{}
	r := newTestRouter(newStubStore(), az)
	w := doRequest(r, http.MethodPost, "/privacy/rights-requests", domain.CreateRightsRequestRequest{
		SubjectRef: "subject-1", RightFamily: domain.RightAccess,
	}, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	if az.calls != 1 {
		t.Fatalf("authorization must be consulted exactly once, saw %d", az.calls)
	}
}

// Authorization still precedes every lookup: a denied caller gets 403 for an id
// that exists and for one that does not, alike.
func TestPreflightOrder_AuthorizationStillPrecedesLookup(t *testing.T) {
	st := newStubStore()
	az := &countingAuthz{}
	r := newTestRouter(st, az)
	existing := createRequest(t, r)

	az.deny = true
	for _, id := range []string{existing.RequestID, uuid.NewString()} {
		w := doRequest(r, http.MethodGet, "/privacy/rights-requests/"+id, nil, testTenant)
		if w.Code != http.StatusForbidden || errorCode(t, w) != domain.ErrCodeAuthorizationDenied {
			t.Errorf("GET %s while denied: want 403, got %d %s", id, w.Code, w.Body.String())
		}
	}
}

// End to end through the real authorization client: the tenant and correlation
// id the handler verified are what authorization-svc receives.
func TestPreflight_AuthorizationCallCarriesTheVerifiedContext(t *testing.T) {
	var mu sync.Mutex
	var got []http.Header
	authz := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Clone())
		mu.Unlock()
		_, _ = w.Write([]byte(`{"decision_outcome":"GRANTED"}`))
	}))
	defer authz.Close()

	h := handler.New(newStubStore(), authzpkg.NewClient(authz.URL), zap.NewNop())
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	handler.RegisterRoutes(r, h)

	w := doRequestWithHeaders(r, http.MethodPost, "/privacy/rights-requests", domain.CreateRightsRequestRequest{
		SubjectRef: "subject-1", RightFamily: domain.RightAccess,
	}, testTenant, map[string]string{"X-Correlation-ID": "corr-xyz", "X-Principal-Id": "principal-42"})
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	if len(got) != 1 {
		t.Fatalf("want 1 authorization call, got %d", len(got))
	}
	for header, want := range map[string]string{
		"X-Principal-Id":    "principal-42",
		"X-Tenant-Id":       testTenant,
		"X-Legal-Entity-Id": testTenant,
		"X-Correlation-Id":  "corr-xyz",
		"X-Source-Channel":  "web",
	} {
		if v := got[0].Get(header); v != want {
			t.Errorf("authorization-svc received %s = %q, want %q", header, v, want)
		}
	}
	if got[0].Get("X-Request-Id") == "" || got[0].Get("Idempotency-Key") == "" {
		t.Error("authorization-svc must receive X-Request-Id and Idempotency-Key")
	}
	// The caller's own idempotency key is never forwarded as the check's key.
	if got[0].Get("Idempotency-Key") == w.Header().Get("Idempotency-Key") && w.Header().Get("Idempotency-Key") != "" {
		t.Error("the check must use its own idempotency key")
	}
}
