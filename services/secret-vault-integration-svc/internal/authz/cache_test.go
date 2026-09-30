package authz

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	svcenvelope "zoiko.io/secret-vault-integration-svc/internal/envelope"
)

// envCtx builds a context carrying a parsed canonical envelope for tenantID,
// which is where CheckAllowed reads the caller's tenant from.
func envCtx(tenantID string) context.Context {
	req := httptest.NewRequest(http.MethodPost, "/v1/secrets/leases/x/revoke", nil)
	req.Header.Set(svcenvelope.HeaderTenantID, tenantID)
	req.Header.Set(svcenvelope.HeaderActorSubjectID, "principal-1")
	req.Header.Set(svcenvelope.HeaderRequestID, "req-1")
	req.Header.Set(svcenvelope.HeaderCorrelationID, "corr-1")
	req.Header.Set(svcenvelope.HeaderSourceChannel, "api")
	// This service's §4 policy makes both of these mandatory on a material
	// write, and a revoke is one — without them the middleware refuses the
	// request and the handler never runs.
	req.Header.Set(svcenvelope.HeaderIdempotencyKey, "idem-1")
	req.Header.Set(svcenvelope.HeaderPurposeContext, "OPERATIONS")

	var captured context.Context
	svcenvelope.Middleware(svcenvelope.ServicePolicy(), svcenvelope.DefaultReporter())(
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { captured = r.Context() }),
	).ServeHTTP(httptest.NewRecorder(), req)

	if captured == nil {
		panic("envelope middleware refused the test request before it reached the handler")
	}
	return captured
}

// answeringByTenant is an authorization-svc stand-in that grants only to
// grantTenant — the real service's behaviour, where roles are tenant-scoped
// and its own tables are under row-level security.
func answeringByTenant(t *testing.T, grantTenant string, calls *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		outcome := "DENIED"
		basis := "no_grant"
		if r.Header.Get(svcenvelope.HeaderTenantID) == grantTenant {
			outcome = "GRANTED"
			basis = "rbac:role=TEST"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"decision_outcome":   outcome,
			"decision_basis":     basis,
			"access_decision_id": "decision-1",
		})
	}))
}

const (
	tenantA = "11111111-1111-1111-1111-111111111111"
	tenantB = "22222222-2222-2222-2222-222222222222"
)

// TestDecisionCache_IsScopedPerTenant pins a real cross-tenant authorization
// defect, found by driving the running service rather than by reading the code.
//
// The decision cache was keyed on (principal, entity, action) only. But
// authorization-svc's answer genuinely depends on the tenant — roles are
// tenant-scoped, its tables are under RLS, and this client forwards
// X-Tenant-Id precisely because of that. So the cache stored an answer under a
// question it had not asked, and for the rest of the TTL served it to whichever
// tenant asked next.
//
// This direction is the dangerous one: a principal authorized in tenant A,
// acting immediately afterwards in tenant B where it holds no role at all, was
// handed A's cached GRANT and passed the gate. In this service that is a window
// to revoke another tenant's leases, write its secret material, or rotate its
// secrets.
func TestDecisionCache_IsScopedPerTenant(t *testing.T) {
	calls := 0
	srv := answeringByTenant(t, tenantA, &calls)
	defer srv.Close()
	c := NewHTTPClient(srv.URL, zap.NewNop())

	if err := c.CheckAllowed(envCtx(tenantA), "principal-1", "entity-1", "SECRET_LEASE_REVOKE"); err != nil {
		t.Fatalf("tenant A holds the grant and must be allowed: %v", err)
	}
	if err := c.CheckAllowed(envCtx(tenantB), "principal-1", "entity-1", "SECRET_LEASE_REVOKE"); err == nil {
		t.Fatal("tenant B holds no role and was allowed: tenant A's cached decision leaked across the tenant boundary")
	}
	if calls != 2 {
		t.Errorf("expected one live check per tenant, got %d — the second was served from cache", calls)
	}
}

// TestDecisionCache_DenialDoesNotLeakAcrossTenants is the same defect in the
// other direction, and the one that looks like an outage rather than a breach:
// a denial in a tenant the caller has no role in was served back to the tenant
// where it is legitimately authorized, locking an operator out of its own data.
func TestDecisionCache_DenialDoesNotLeakAcrossTenants(t *testing.T) {
	calls := 0
	srv := answeringByTenant(t, tenantA, &calls)
	defer srv.Close()
	c := NewHTTPClient(srv.URL, zap.NewNop())

	// Denied tenant first, so the cached entry is a DENIAL.
	if err := c.CheckAllowed(envCtx(tenantB), "principal-1", "entity-1", "SECRET_ROTATE"); err == nil {
		t.Fatal("tenant B must be denied")
	}
	if err := c.CheckAllowed(envCtx(tenantA), "principal-1", "entity-1", "SECRET_ROTATE"); err != nil {
		t.Fatalf("tenant A holds the grant and must still be allowed: %v", err)
	}
}

// TestDecisionCache_StillCachesWithinOneTenant guards the fix from being a
// silent removal of the cache. Repeating the same question in the same tenant
// must still be answered locally — that is what the cache is for, and
// authorization-svc sits on the request path of every mutation here.
func TestDecisionCache_StillCachesWithinOneTenant(t *testing.T) {
	calls := 0
	srv := answeringByTenant(t, tenantA, &calls)
	defer srv.Close()
	c := NewHTTPClient(srv.URL, zap.NewNop())

	for i := 0; i < 3; i++ {
		if err := c.CheckAllowed(envCtx(tenantA), "principal-1", "entity-1", "SECRET_MATERIAL_WRITE"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if calls != 1 {
		t.Errorf("expected 1 live check for 3 identical in-tenant questions, got %d", calls)
	}
}

// TestDecisionCache_SeparatesActions is the property the original key did get
// right, asserted so the rewrite cannot lose it: a grant for one action must
// never answer for another.
func TestDecisionCache_SeparatesActions(t *testing.T) {
	calls := 0
	srv := answeringByTenant(t, tenantA, &calls)
	defer srv.Close()
	c := NewHTTPClient(srv.URL, zap.NewNop())

	_ = c.CheckAllowed(envCtx(tenantA), "principal-1", "entity-1", "SECRET_ROTATE")
	_ = c.CheckAllowed(envCtx(tenantA), "principal-1", "entity-1", "SECRET_LEASE_REVOKE")
	if calls != 2 {
		t.Errorf("expected a live check per distinct action, got %d", calls)
	}
}
