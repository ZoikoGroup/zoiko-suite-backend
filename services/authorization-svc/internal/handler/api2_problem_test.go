package handler_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/handler"
	apicontract "zoiko.io/identity-envelope/apicontract"
)

// API-2 (ZS-API-001 §12): every refusal is RFC 9457 problem+json with a
// stable code, whichever writer produced it — writeError, or the legacy
// `{"error": …}` bodies writeJSON converts. The §16 class stays beside the
// code as an extension member. 412/428 are not answered here until API-3.
func TestAPI2_ErrorResponsesAreProblemDetails(t *testing.T) {
	authorize := func(s *stubStore, legalEntity string) *httptest.ResponseRecorder {
		body := `{"principal_id":"p-1","legal_entity_id":"` + legalEntity + `","action_type":"PAYMENT_APPROVE","tenant_id":"` + goodTenant + `"}`
		w := httptest.NewRecorder()
		newTestRouter(s).ServeHTTP(w, httptest.NewRequest(http.MethodPost, handler.AuthorizePath, bytes.NewBufferString(body)))
		return w
	}
	retireBundle := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/admin/permission-bundles/b-elsewhere/retire", nil)
		req.Header.Set("X-Principal-Id", "admin-1")
		req.Header.Set("X-Tenant-Id", goodTenant)
		w := httptest.NewRecorder()
		newTestRouter(&stubStore{bundleActiveErr: domain.ErrPermissionBundleNotFound}).ServeHTTP(w, req)
		return w
	}
	assign := func(sodErr error) *httptest.ResponseRecorder {
		s := newSoDAssignStore([]string{"payment.prepare"}, "payment.release")
		s.sodErr = sodErr
		body := `{"principal_id":"preparer-1","role_id":"r-new","effective_from":"2026-01-01T00:00:00Z","legal_entity_id":"22222222-2222-4222-8222-222222222222"}`
		return postAsAdminTo(s, "/v1/admin/role-assignments", body)
	}

	for _, tc := range []struct {
		name   string
		w      *httptest.ResponseRecorder
		status int
		code   string
		class  string
	}{
		{"400 malformed legal entity", authorize(grantingStore(), "not-a-uuid"), 400, "invalid_scope", "CONTEXT_UNRESOLVED"},
		{"401 no principal", cmdDo(newCmdStore(&stubStore{denyAdmin: true}), http.MethodPost, "", "/v1/admin/authorization-cache/invalidate", `{}`), 401, "missing_principal", "CONTEXT_UNRESOLVED"},
		{"403 not permitted", cmdDo(newCmdStore(&stubStore{denyAdmin: true}), http.MethodPost, "clerk-1", "/v1/admin/authorization-cache/invalidate", `{}`), 403, "authorization_denied", "AUTHORIZATION_DENIED"},
		{"404 bundle in another tenant", retireBundle(), 404, "permission_bundle_not_found", ""},
		{"409 segregation-of-duties conflict", assign(nil), 409, "sod_conflict", "SOD_CONFLICT"},
		{"503 store unavailable", authorize(&stubStore{rbacErr: domain.ErrStoreUnavailable}, goodEntity), 503, "store_unavailable", ""},
		{"503 SoD check unavailable", assign(errors.New("db down")), 503, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.w.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", tc.w.Code, tc.status, tc.w.Body.String())
			}
			if ct := tc.w.Header().Get("Content-Type"); ct != apicontract.ContentTypeProblem {
				t.Fatalf("Content-Type = %q, want %s: %s", ct, apicontract.ContentTypeProblem, tc.w.Body.String())
			}
			var p map[string]any
			if err := json.Unmarshal(tc.w.Body.Bytes(), &p); err != nil {
				t.Fatalf("body is not JSON: %s", tc.w.Body.String())
			}
			code, _ := p["code"].(string)
			typ, _ := p["type"].(string)
			if code == "" || (tc.code != "" && code != tc.code) || p["status"] != float64(tc.status) ||
				!strings.HasPrefix(typ, apicontract.ProblemBase) || p["title"] == nil {
				t.Fatalf("problem = %v, want code %q", p, tc.code)
			}
			if _, legacy := p["error"]; legacy {
				t.Fatalf("legacy error member survived: %v", p)
			}
			if tc.class != "" && p["error_class"] != tc.class {
				t.Fatalf("error_class = %v, want %s", p["error_class"], tc.class)
			}
			if tc.status == http.StatusServiceUnavailable && p["retryable"] != true {
				t.Fatalf("a 503 must be retryable: %v", p)
			}
		})
	}
}
