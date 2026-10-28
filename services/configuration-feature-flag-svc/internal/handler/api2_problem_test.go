package handler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zoiko.io/configuration-feature-flag-svc/internal/domain"
	"zoiko.io/identity-envelope/apicontract"
)

// API-2 (ZS-API-001 §12): every refusal is application/problem+json with a
// stable code. 412 and 428 are not produced here until If-Match lands (API-3).
func TestAPI2_ErrorResponsesAreProblemDetails(t *testing.T) {
	const upsert = `{"key":"k","value":100,"environment":"staging","created_by_principal_id":"admin-1"}`
	serve := func(store *stubStore, req *http.Request) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		newTestRouter(store).ServeHTTP(w, req)
		return w
	}
	post := func(body string) *http.Request {
		return authed(httptest.NewRequest(http.MethodPost, "/v1/config", strings.NewReader(body)))
	}

	for _, tc := range []struct {
		name   string
		w      *httptest.ResponseRecorder
		status int
		code   string
	}{
		{"400 malformed body", serve(&stubStore{}, post("{")), 400, "invalid_json"},
		{"400 undeclared key", serve(&stubStore{configEntryErr: domain.ErrKeyNotRegistered}, post(upsert)), 400, "key_not_registered"},
		{"401 no verified caller", serve(&stubStore{}, httptest.NewRequest(http.MethodPost, "/v1/config", strings.NewReader(upsert))), 401, "missing_principal"},
		{"403 scope not allowed", serve(&stubStore{configEntryErr: domain.ErrScopeNotAllowed}, post(upsert)), 403, "scope_not_allowed"},
		{"404 unknown entry", serve(&stubStore{findConfigErr: domain.ErrConfigEntryNotFound},
			scoped(httptest.NewRequest(http.MethodGet, "/v1/config/missing?environment=staging", nil))), 404, "config_entry_not_found"},
		{"409 material key needs a change", serve(&stubStore{configEntryErr: domain.ErrMaterialKeyRequiresChange}, post(upsert)), 409, "material_key_requires_change"},
		{"503 store unavailable", serve(&stubStore{configEntryErr: domain.ErrStoreUnavailable}, post(upsert)), 503, "store_unavailable"},
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
