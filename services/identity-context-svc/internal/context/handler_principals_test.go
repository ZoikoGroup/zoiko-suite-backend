// Tests that GET/PUT /v1/principals/... require X-Tenant-Id and thread it
// through to the store. Before this fix, these four routes took only
// principalID from the URL and never checked any tenant header at all —
// any caller could read or update any tenant's principal, role
// assignments, delegations, or status by principal_id alone.
package context_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	identityctx "zoiko.io/identity-context-svc/internal/context"
	"zoiko.io/identity-context-svc/internal/domain"
)

// stubAuthz is a test double for identityctx.AuthzChecker. It GRANTS by
// default so these tenant-scoping tests keep exercising what they were
// written for; the deny and unavailable branches are covered in
// handler_authz_test.go.
type stubAuthz struct {
	err error
}

func (s *stubAuthz) CheckAllowed(_ context.Context, _, _, _, _ string) error { return s.err }

func newPrincipalsRouter(store *mockPrincipalStore) chi.Router {
	r := chi.NewRouter()
	// Resolver and authenticator are nil: these tests exercise only the
	// /v1/principals routes, which touch neither.
	h := identityctx.NewHandler(nil, nil, nil, store, &stubAuthz{}, zap.NewNop())
	identityctx.RegisterRoutes(r, h)
	return r
}

func TestPrincipalRoutes_MissingTenantHeader_Rejected(t *testing.T) {
	store := &mockPrincipalStore{principal: &domain.Principal{PrincipalID: "p-1", RecordVersion: 1}}
	r := newPrincipalsRouter(store)

	cases := []struct {
		method, path string
	}{
		{"GET", "/v1/principals/p-1"},
		{"GET", "/v1/principals/p-1/roles"},
		{"GET", "/v1/principals/p-1/delegations"},
		{"PUT", "/v1/principals/p-1/status"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		// 401, not 400. Changed deliberately: a request with no verified
		// tenant is unauthenticated, not malformed, and the distinction
		// matters to a client deciding whether to retry or re-authenticate.
		if w.Code != 401 {
			t.Errorf("%s %s with no X-Tenant-Id: got %d, want 401", c.method, c.path, w.Code)
		}
	}
}

func TestGetPrincipal_TenantHeaderPresent_ReachesStore(t *testing.T) {
	store := &mockPrincipalStore{principal: &domain.Principal{PrincipalID: "p-1", TenantID: "tenant-a", RecordVersion: 1}}
	r := newPrincipalsRouter(store)

	req := httptest.NewRequest("GET", "/v1/principals/p-1", nil)
	req.Header.Set("X-Tenant-Id", "tenant-a")
	// The caller is the same principal being read, so this exercises the
	// self-exemption path — no authorization round-trip needed for your own
	// principal, which is ordinary platform traffic.
	req.Header.Set("X-Principal-Id", "p-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("got %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// ── API-3 ETag/If-Match tests ──────────────────────────────────────────────────

// TestETag_GetPrincipalReturnsETag verifies that GET /v1/principals/{id}
// returns an ETag header with the record_version.
func TestETag_GetPrincipalReturnsETag(t *testing.T) {
	store := &mockPrincipalStore{principal: &domain.Principal{PrincipalID: "p-1", TenantID: "tenant-a", RecordVersion: 42}}
	r := newPrincipalsRouter(store)

	req := httptest.NewRequest("GET", "/v1/principals/p-1", nil)
	req.Header.Set("X-Tenant-Id", "tenant-a")
	req.Header.Set("X-Principal-Id", "p-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("ETag header is missing")
	}
	if etag != `"v42"` {
		t.Fatalf("ETag = %q, want %q", etag, `"v42"`)
	}
}

// TestIfMatch_UpdatePrincipalStatus_MissingReturns428 verifies that
// PUT /v1/principals/{id}/status requires an If-Match header and returns
// 428 Precondition Required when missing.
func TestIfMatch_UpdatePrincipalStatus_MissingReturns428(t *testing.T) {
	store := &mockPrincipalStore{principal: &domain.Principal{PrincipalID: "p-1", TenantID: "tenant-a", RecordVersion: 1}}
	changer := &recordingStatusChanger{}
	h := identityctx.NewHandler(nil, nil, nil, store, &permittingAuthz{}, zap.NewNop()).WithStatusChanger(changer)
	r := chi.NewRouter()
	identityctx.RegisterRoutes(r, h)

	body, _ := json.Marshal(domain.UpdateStatusRequest{Status: domain.PrincipalStatusDisabled, Reason: "left the company"})
	req := httptest.NewRequest(http.MethodPut, "/v1/principals/p-1/status", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", "tenant-a")
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Correlation-ID", "corr-status")
	// No If-Match header
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusPreconditionRequired {
		t.Fatalf("expected 428, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body is not JSON: %s", w.Body.String())
	}
	if resp.Code != "precondition_required" {
		t.Fatalf("code = %q, want %q", resp.Code, "precondition_required")
	}
}

// TestIfMatch_UpdatePrincipalStatus_StaleReturns412 verifies that
// PUT /v1/principals/{id}/status with a stale ETag returns 412 Precondition Failed.
func TestIfMatch_UpdatePrincipalStatus_StaleReturns412(t *testing.T) {
	store := &mockPrincipalStore{principal: &domain.Principal{PrincipalID: "p-1", TenantID: "tenant-a", RecordVersion: 1}}
	changer := &recordingStatusChanger{}
	h := identityctx.NewHandler(nil, nil, nil, store, &permittingAuthz{}, zap.NewNop()).WithStatusChanger(changer)
	r := chi.NewRouter()
	identityctx.RegisterRoutes(r, h)

	body, _ := json.Marshal(domain.UpdateStatusRequest{Status: domain.PrincipalStatusDisabled, Reason: "left the company"})
	req := httptest.NewRequest(http.MethodPut, "/v1/principals/p-1/status", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", "tenant-a")
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Correlation-ID", "corr-status")
	req.Header.Set("If-Match", `"v999"`) // Stale version
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("expected 412, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Code           string `json:"code"`
		CurrentVersion int64  `json:"current_version"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body is not JSON: %s", w.Body.String())
	}
	if resp.Code != "stale_version" {
		t.Fatalf("code = %q, want %q", resp.Code, "stale_version")
	}
	if resp.CurrentVersion != 1 {
		t.Fatalf("current_version = %d, want 1", resp.CurrentVersion)
	}
	// Response should include the current ETag
	etag := w.Header().Get("ETag")
	if etag != `"v1"` {
		t.Fatalf("response ETag = %q, want %q", etag, `"v1"`)
	}
}

// TestIfMatch_UpdatePrincipalStatus_CorrectReturns204 verifies that
// PUT /v1/principals/{id}/status with a correct ETag succeeds with 204.
func TestIfMatch_UpdatePrincipalStatus_CorrectReturns204(t *testing.T) {
	store := &mockPrincipalStore{principal: &domain.Principal{PrincipalID: "p-1", TenantID: "tenant-a", RecordVersion: 1}}
	changer := &recordingStatusChanger{}
	h := identityctx.NewHandler(nil, nil, nil, store, &permittingAuthz{}, zap.NewNop()).WithStatusChanger(changer)
	r := chi.NewRouter()
	identityctx.RegisterRoutes(r, h)

	body, _ := json.Marshal(domain.UpdateStatusRequest{Status: domain.PrincipalStatusDisabled, Reason: "left the company"})
	req := httptest.NewRequest(http.MethodPut, "/v1/principals/p-1/status", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", "tenant-a")
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Correlation-ID", "corr-status")
	req.Header.Set("If-Match", `"v1"`) // Correct version
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}
}
