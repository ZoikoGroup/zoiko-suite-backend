package handler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zoiko.io/identity-envelope/apicontract"
	"zoiko.io/secret-vault-integration-svc/internal/domain"
)

// API-2 (ZS-API-001 §12): every refusal is application/problem+json with a
// stable code. 412 and 428 are not produced here until If-Match lands (API-3).
func TestAPI2_ErrorResponsesAreProblemDetails(t *testing.T) {
	const policy = `{"secret_class":"X","secret_path":"kv/x","created_by_principal_id":"admin-1"}`
	serve := func(s *stubStore, req *http.Request) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		defaultRouter(s).ServeHTTP(w, req)
		return w
	}
	create := func(body string) *http.Request {
		return authed(httptest.NewRequest(http.MethodPost, "/v1/secret-policies", strings.NewReader(body)))
	}
	foreignWorkload := &stubStore{applicableByPath: &domain.ApplicableSecretPolicyVersion{
		SecretPolicyVersion: domain.SecretPolicyVersion{
			SecretPolicyVersionID: "22222222-0000-4000-8000-000000000001",
			AllowedWorkloadIDs:    json.RawMessage(`["svc-a"]`),
		},
		SecretClass: "DATABASE_CREDENTIAL",
		SecretPath:  "kv/db",
	}}

	for _, tc := range []struct {
		name   string
		w      *httptest.ResponseRecorder
		status int
		code   string
	}{
		{"400 missing field", serve(&stubStore{}, create(`{"secret_class":"X"}`)), 400, "missing_field"},
		{"401 no verified caller", serve(&stubStore{}, httptest.NewRequest(http.MethodPost, "/v1/secret-policies", strings.NewReader(policy))), 401, "missing_principal"},
		{"403 workload not allowed", serve(foreignWorkload, asWorkload(authed(httptest.NewRequest(http.MethodPost, "/v1/secrets/broker",
			strings.NewReader(brokerBody("kv/db", "svc-b", "req-1")))), "svc-b")), 403, "access_denied"},
		{"404 unknown policy", serve(&stubStore{versionErr: domain.ErrSecretPolicyNotFound}, authed(httptest.NewRequest(http.MethodPost,
			"/v1/secret-policies/44444444-0000-4000-8000-00000000dead/versions",
			strings.NewReader(`{"max_lease_duration_seconds":300,"effective_from":"2026-01-01T00:00:00Z","created_by_principal_id":"admin-1"}`)))), 404, "secret_policy_not_found"},
		{"409 conflicting policy", serve(&stubStore{policyErr: domain.ErrConflict}, create(policy)), 409, "secret_policy_conflict"},
		{"503 store unavailable", serve(&stubStore{policyErr: domain.ErrStoreUnavailable}, create(policy)), 503, "store_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.w.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", tc.w.Code, tc.status, tc.w.Body.String())
			}
			if ct := tc.w.Header().Get("Content-Type"); ct != apicontract.ContentTypeProblem {
				t.Fatalf("Content-Type = %q, want %s", ct, apicontract.ContentTypeProblem)
			}
			var p map[string]any
			if err := json.Unmarshal(tc.w.Body.Bytes(), &p); err != nil {
				t.Fatalf("body is not JSON: %s", tc.w.Body.String())
			}
			if p["code"] != tc.code || p["status"] != float64(tc.status) || !strings.HasPrefix(p["type"].(string), apicontract.ProblemBase) {
				t.Fatalf("problem = %v, want code %q", p, tc.code)
			}
		})
	}
}
