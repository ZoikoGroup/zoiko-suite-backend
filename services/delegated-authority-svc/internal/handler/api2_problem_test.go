package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/delegated-authority-svc/internal/domain"
	"zoiko.io/identity-envelope/apicontract"
)

// downAuthZ answers every check with authorization-svc unreachable.
type downAuthZ struct{ stubAuthZ }

func (downAuthZ) CheckAllowed(context.Context, string, string, string) error {
	return domain.ErrAuthzServiceUnavailable
}

func (downAuthZ) CheckHeldInOwnRight(context.Context, string, string, string) error {
	return domain.ErrAuthzServiceUnavailable
}

// API-2 (ZS-API-001 §12): every refusal is application/problem+json with a
// stable code. A stale version is still 409 here, not 412; that moves with
// If-Match in API-3.
func TestAPI2_ErrorResponsesAreProblemDetails(t *testing.T) {
	now := time.Now()
	body := func() map[string]any {
		return delegationBody("delegator-1", uuid.NewString(), now, now.Add(24*time.Hour))
	}

	r := newRouter(newStubStore(), &stubAuthZ{}, nil)
	revoked := createActiveDelegation(t, r)
	_ = revokeReq(r, revoked.DelegationID, revoked.Version, "admin-1")
	active := createActiveDelegation(t, r)

	for _, tc := range []struct {
		name   string
		rr     *httptest.ResponseRecorder
		status int
		code   string
	}{
		{"400 invalid time window", doReq(r, http.MethodPost, "/v1/delegations/", delegationBody("delegator-1", uuid.NewString(), now, now), "caller-1"), 400, "invalid_time_window"},
		{"401 no verified caller", doReq(r, http.MethodPost, "/v1/delegations/", body(), ""), 401, "identity_missing"},
		{"403 delegator lacks authority", doReq(newRouter(newStubStore(), &stubAuthZ{delegatorDenied: "delegator-1"}, nil), http.MethodPost, "/v1/delegations/", body(), "caller-1"), 403, "delegator_lacks_authority"},
		{"404 unknown delegation", doReq(r, http.MethodGet, "/v1/delegations/"+uuid.NewString(), nil, "caller-1"), 404, "not_found"},
		{"409 already revoked", revokeReq(r, revoked.DelegationID, revoked.Version+1, "admin-1"), 409, "invalid_transition"},
		{"428 no expected version", doReq(r, http.MethodPost, "/v1/delegations/"+active.DelegationID+"/revoke", map[string]any{"reason": "x"}, "admin-1"), 428, "version_required"},
		{"503 authorization unavailable", doReq(newRouterAuthz(newStubStore(), &downAuthZ{}), http.MethodPost, "/v1/delegations/", body(), "caller-1"), 503, "authz_unavailable"},
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
