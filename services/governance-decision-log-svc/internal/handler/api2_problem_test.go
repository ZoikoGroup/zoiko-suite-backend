package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/governance-decision-log-svc/internal/authz"
	"zoiko.io/governance-decision-log-svc/internal/domain"
	"zoiko.io/governance-decision-log-svc/internal/handler"
	"zoiko.io/identity-envelope/apicontract"
)

// API-2 (ZS-API-001 §12): every refusal is application/problem+json with a
// stable code. 412 and 428 are not produced here until If-Match lands (API-3).
func TestAPI2_ErrorResponsesAreProblemDetails(t *testing.T) {
	denied := chi.NewRouter()
	handler.RegisterRoutes(denied, handler.New(&stubStore{}, &stubAuthz{err: authz.ErrDenied}, &stubPolicyClient{}, testAuthzScopeID, zap.NewNop()))

	noTenant := httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody()))
	noTenant.Header.Set("X-Principal-Id", testPrincipal)
	noTenant.Header.Set("Idempotency-Key", "k-401")

	notFound := authed(httptest.NewRequest(http.MethodGet, "/v1/decisions/does-not-exist", nil))

	changed := authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody())))
	changed.Header.Set("Idempotency-Key", "a-different-key")
	stored := &domain.GovernanceDecision{
		DecisionID: "dec-001", TenantID: testTenant, LegalEntityID: testEntity,
		ActorID: testPrincipal, ActionType: "PAYROLL_RELEASE", Outcome: "GRANTED",
		RuleBasis: "policy-v3-sod", CorrelationID: "corr-001",
	}

	for _, tc := range []struct {
		name   string
		router http.Handler
		req    *http.Request
		status int
		code   string
	}{
		{"400 malformed body", newTestRouter(&stubStore{created: true}),
			authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(`{not json`))), 400, "INVALID_REQUEST"},
		{"401 no tenant scope", newTestRouter(&stubStore{created: true}), noTenant, 401, "TENANT_SCOPE_MISSING"},
		{"403 authorization denied", denied,
			authed(httptest.NewRequest(http.MethodPost, gatedRoutes[0].path, bytes.NewBufferString(gatedRoutes[0].body))), 403, "AUTHORIZATION_DENIED"},
		{"404 unknown decision", newTestRouter(&stubStore{findByIDErr: domain.ErrDecisionNotFound}), notFound, 404, "NOT_FOUND"},
		{"409 changed body on a recorded decision", newTestRouter(&stubStore{created: false, existing: stored}), changed, 409, "DECISION_ID_CONFLICT"},
		{"503 store unavailable", newTestRouter(&stubStore{err: domain.ErrStoreUnavailable}),
			authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody()))), 503, "SERVICE_UNAVAILABLE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			tc.router.ServeHTTP(rr, tc.req)
			if rr.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", rr.Code, tc.status, rr.Body.String())
			}
			if ct := rr.Header().Get("Content-Type"); ct != apicontract.ContentTypeProblem {
				t.Fatalf("Content-Type = %q, want %s", ct, apicontract.ContentTypeProblem)
			}
			var p map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil {
				t.Fatalf("body is not JSON: %s", rr.Body.String())
			}
			if p["code"] != tc.code || p["status"] != float64(tc.status) || !strings.HasPrefix(p["type"].(string), apicontract.ProblemBase) {
				t.Fatalf("problem = %v, want code %q", p, tc.code)
			}
		})
	}
}
