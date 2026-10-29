package context_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	identityctx "zoiko.io/identity-context-svc/internal/context"
	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/events"
	"zoiko.io/identity-context-svc/internal/lifecycle"
	"zoiko.io/identity-context-svc/internal/siem"
	"zoiko.io/identity-context-svc/internal/sod"
	"zoiko.io/identity-envelope/apicontract"
)

func supportRouter(checker sod.Checker) chi.Router {
	f := defaultFixture()
	sessions := newMockSessionCache()
	f.sessions = sessions
	h := identityctx.NewHandler(f.build(), nil, sessions, f.principals, &permittingAuthz{}, zap.NewNop()).
		WithSupport(identityctx.NewSupportService(
			newFakeSupportStore(), events.NewPublisherWithSink(zap.NewNop(), "t", noopSink{}), checker,
			siem.New("", "identity-context-svc", zap.NewNop()), identityctx.DefaultSupportPolicy(), zap.NewNop()))
	r := chi.NewRouter()
	identityctx.RegisterRoutes(r, h)
	return r
}

// API-2 (ZS-API-001 §12): every refusal is application/problem+json with a
// stable code. 412 and 428 are not produced here until If-Match lands (API-3).
func TestAPI2_ErrorResponsesAreProblemDetails(t *testing.T) {
	gov := newGov01Harness(t, &permittingAuthz{})
	seedSession(gov.sessions, "sc-1", "tenant-a", "p-1", "jwt")

	malformed := httptest.NewRequest(http.MethodPut, "/v1/principals/p-1/status", bytes.NewBufferString("{"))
	malformed.Header.Set("X-Tenant-Id", "tenant-a")
	malformed.Header.Set("X-Principal-Id", "admin-1")
	rr400 := httptest.NewRecorder()
	statusRouter(&recordingStatusChanger{}).ServeHTTP(rr400, malformed)

	for _, tc := range []struct {
		name   string
		rr     *httptest.ResponseRecorder
		status int
		code   string
	}{
		{"400 malformed body", rr400, 400, "bad_request"},
		{"401 no verified tenant", do(gov.router, http.MethodGet, "/v1/context/session/sc-1/explain", "", "p-1", nil), 401, "unauthorized"},
		{"403 lifecycle refusal", putStatus(t, statusRouter(&recordingStatusChanger{err: fmt.Errorf("%w: own account", lifecycle.ErrForbidden)}), domain.PrincipalStatusDisabled), 403, "self_status_change"},
		{"404 foreign session", do(gov.router, http.MethodGet, "/v1/context/session/sc-1/explain", "tenant-b", "p-9", nil), 404, "CONTEXT_UNRESOLVED"},
		{"409 segregation conflict", do(supportRouter(conflictingSoD{}), http.MethodPost, "/v1/context/support", "tenant-support", requesterID, validAttachRequest()), 409, "SOD_CONFLICT"},
		{"503 segregation check unavailable", do(supportRouter(unavailableSoD{}), http.MethodPost, "/v1/context/support", "tenant-support", requesterID, validAttachRequest()), 503, "UPSTREAM_UNAVAILABLE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.rr.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", tc.rr.Code, tc.status, tc.rr.Body.String())
			}
			if ct := tc.rr.Header().Get("Content-Type"); ct != apicontract.ContentTypeProblem {
				t.Fatalf("Content-Type = %q, want %s", ct, apicontract.ContentTypeProblem)
			}
			var p map[string]any
			if err := json.Unmarshal(tc.rr.Body.Bytes(), &p); err != nil {
				t.Fatalf("body is not JSON: %s", tc.rr.Body.String())
			}
			if p["code"] != tc.code || p["status"] != float64(tc.status) || !strings.HasPrefix(p["type"].(string), apicontract.ProblemBase) {
				t.Fatalf("problem = %v, want code %q", p, tc.code)
			}
		})
	}
}
