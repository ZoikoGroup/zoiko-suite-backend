package handler_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"zoiko.io/audit-event-store-svc/internal/authz"
	"zoiko.io/audit-event-store-svc/internal/envelope"
	"zoiko.io/audit-event-store-svc/internal/handler"
	"zoiko.io/audit-event-store-svc/internal/store"
)

// Every route requires X-Tenant-Id and X-Principal-Id. A workload identity
// (X-Workload-Id) must not stand in for the principal: envelope.Actor() falls
// back to it, so the fallback only shows up when the real envelope middleware is
// mounted — which the other tests in this package do not do. These tests mount
// it (observe mode: parse and propagate, never refuse), exactly as the server
// does for the envelope fields it parses.

type principalRecorder struct {
	err        error
	principals []string
	actions    []string
}

func (p *principalRecorder) CheckAllowed(_ context.Context, principalID, _, action string) error {
	p.principals = append(p.principals, principalID)
	p.actions = append(p.actions, action)
	return p.err
}

func routerWithEnvelope(t *testing.T, az handler.AuthzChecker) chi.Router {
	t.Helper()
	r := chi.NewRouter()
	r.Use(envelope.MiddlewareWithMode(envelope.ServicePolicy(), envelope.ModeObserve, nil))
	handler.RegisterRoutes(r, handler.New(store.NewFakeStore(), az, zaptest.NewLogger(t)))
	return r
}

var allRoutes = []struct{ method, path, body string }{
	{http.MethodGet, "/v1/events", ""},
	{http.MethodPost, "/v1/events/verify", ""},
	{http.MethodPost, "/v1/archives", `{"from_sequence":1,"to_sequence":1}`},
	{http.MethodGet, "/v1/archives/arch-1", ""},
	{http.MethodPost, "/v1/archives/arch-1/verify", ""},
	{http.MethodGet, "/v1/archives/arch-1/verifications", ""},
}

func do(r http.Handler, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestPrincipal_Missing_Is401_OnEveryRoute(t *testing.T) {
	for _, rt := range allRoutes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			az := &principalRecorder{}
			w := do(routerWithEnvelope(t, az), rt.method, rt.path, rt.body, map[string]string{"X-Tenant-Id": "tenant-1"})
			assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), "principal_required")
			assert.Empty(t, az.actions, "authorization must not be consulted without a principal")
		})
	}
}

// The critical regression: tenant + workload id, no principal.
func TestPrincipal_WorkloadOnly_Is401_OnEveryRoute(t *testing.T) {
	for _, rt := range allRoutes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			az := &principalRecorder{}
			w := do(routerWithEnvelope(t, az), rt.method, rt.path, rt.body, map[string]string{
				"X-Tenant-Id":   "tenant-1",
				"X-Workload-Id": "billing-svc",
			})
			assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), "principal_required")
			assert.Empty(t, az.actions, "a workload id must not reach authorization as if it were a principal")
		})
	}
}

// A real principal still goes through the normal authorization flow, and the
// principal asked about is X-Principal-Id — never the workload id, even when
// both are sent.
func TestPrincipal_Present_ReachesAuthorizationWithThePrincipal(t *testing.T) {
	for _, withWorkload := range []bool{false, true} {
		for _, rt := range allRoutes {
			name := rt.method + " " + rt.path
			if withWorkload {
				name += " +workload"
			}
			t.Run(name, func(t *testing.T) {
				az := &principalRecorder{err: authz.ErrAuthorizationDenied}
				hdr := map[string]string{"X-Tenant-Id": "tenant-1", "X-Principal-Id": "alice"}
				if withWorkload {
					hdr["X-Workload-Id"] = "billing-svc"
				}
				w := do(routerWithEnvelope(t, az), rt.method, rt.path, rt.body, hdr)
				assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
				require.Equal(t, []string{"alice"}, az.principals)
			})
		}
	}
}

func TestPrincipal_Present_AndGranted_Succeeds(t *testing.T) {
	az := &principalRecorder{}
	w := do(routerWithEnvelope(t, az), http.MethodGet, "/v1/events", "", map[string]string{
		"X-Tenant-Id": "tenant-1", "X-Principal-Id": "alice", "X-Workload-Id": "billing-svc",
	})
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, []string{"alice"}, az.principals)
}

// Tenant is still required independently of the principal.
func TestPrincipal_PresentButNoTenant_StillTenantRequired(t *testing.T) {
	az := &principalRecorder{}
	w := do(routerWithEnvelope(t, az), http.MethodGet, "/v1/events", "", map[string]string{"X-Principal-Id": "alice"})
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "tenant_required")
}
