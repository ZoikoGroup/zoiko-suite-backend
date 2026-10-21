package handler_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/handler"
	"zoiko.io/authorization-svc/internal/registry"
	"zoiko.io/authorization-svc/internal/siem"
)

// The tenant of an evaluation is resolved on the server from the entity
// registry's projection (Governance Control Plane invariants #1 and #2), not
// taken from the caller. Before this, a tenantless question about tenant B's
// entity was answered from tenant A's tenant-wide grants, and a header or body
// naming tenant A did the same.

const (
	tenantA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	tenantB = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	entityB = "22222222-2222-4222-8222-222222222222"
)

func authorizeWith(t *testing.T, s *stubStore, headerTenant, bodyTenant string, enforce bool) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"principal_id":"p-1","legal_entity_id":"` + entityB + `","action_type":"GOVERNANCE_DECISION_RECORD"`
	if bodyTenant != "" {
		body += `,"tenant_id":"` + bodyTenant + `"`
	}
	body += `}`
	req := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewBufferString(body))
	if headerTenant != "" {
		req.Header.Set("X-Tenant-Id", headerTenant)
	}
	w := httptest.NewRecorder()
	if enforce {
		enforcingRouter(s).ServeHTTP(w, req)
	} else {
		newTestRouter(s).ServeHTTP(w, req)
	}
	return w
}

// Tenantless: scoped to the owning tenant, and recorded with it, so the
// tenant-scoped audit read (ListAccessDecisions) can see the decision.
func TestTenantResolution_TenantlessScopedToEntityOwner(t *testing.T) {
	s := &stubStore{rbacActions: []string{"GOVERNANCE_DECISION_RECORD"}, entityTenant: tenantB}
	w := authorizeWith(t, s, "", "", false)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if s.grantedTenantArg != tenantB {
		t.Errorf("grants evaluated in tenant %q, want the entity owner %q (empty = across every tenant)", s.grantedTenantArg, tenantB)
	}
	if s.recordedParams.TenantID != tenantB {
		t.Errorf("decision recorded with tenant %q, want %q — a tenantless record is invisible to the tenant audit read", s.recordedParams.TenantID, tenantB)
	}
}

// Header or body naming another tenant than the entity's owner is refused:
// tenant A's tenant-wide assignment would otherwise match tenant B's entity.
func TestTenantResolution_ForeignEntityRefused(t *testing.T) {
	for _, tc := range []struct{ name, header, body string }{
		{"header", tenantA, ""},
		{"body", "", tenantA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &stubStore{rbacActions: []string{"GOVERNANCE_DECISION_RECORD"}, entityTenant: tenantB}
			w := authorizeWith(t, s, tc.header, tc.body, false)
			if w.Code != http.StatusForbidden {
				t.Fatalf("tenant A asking about tenant B's entity: want 403, got %d: %s", w.Code, w.Body.String())
			}
			if s.recordedParams.ActionType != "" || s.grantedTenantArg != "" {
				t.Error("evaluated despite the refusal")
			}
		})
	}
}

// A body tenant that matches the registry is admitted (now server-confirmed).
func TestTenantResolution_MatchingBodyAdmitted(t *testing.T) {
	s := &stubStore{rbacActions: []string{"GOVERNANCE_DECISION_RECORD"}, entityTenant: tenantB}
	if w := authorizeWith(t, s, "", tenantB, false); w.Code != http.StatusOK || s.grantedTenantArg != tenantB {
		t.Fatalf("matching body tenant: code %d, tenant %q", w.Code, s.grantedTenantArg)
	}
}

// Enforce mode no longer needs the caller to send a tenant when the registry
// knows the entity; an entity it has never mentioned is still refused.
func TestTenantResolution_EnforceAdmitsRegistryKnownEntity(t *testing.T) {
	known := &stubStore{rbacActions: []string{"GOVERNANCE_DECISION_RECORD"}, entityTenant: tenantB}
	if w := authorizeWith(t, known, "", "", true); w.Code != http.StatusOK {
		t.Fatalf("enforce, registry-known entity: want 200, got %d: %s", w.Code, w.Body.String())
	}
	unknown := &stubStore{rbacActions: []string{"GOVERNANCE_DECISION_RECORD"}}
	if w := authorizeWith(t, unknown, "", "", true); w.Code != http.StatusBadRequest {
		t.Fatalf("enforce, unknown entity: want 400, got %d", w.Code)
	}
}

// The owner lookup failing is a refusal, never a fall back to the caller's
// claim or to a cross-tenant evaluation (invariant #3).
func TestTenantResolution_LookupFailureFailsClosed(t *testing.T) {
	s := &stubStore{rbacActions: []string{"GOVERNANCE_DECISION_RECORD"}, entityTenantErr: domain.ErrStoreUnavailable}
	w := authorizeWith(t, s, tenantB, "", false)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("owner lookup down: want 503, got %d", w.Code)
	}
	if s.recordedParams.ActionType != "" {
		t.Error("a decision was made without the owner")
	}
}

// ── entities the projection has not heard of: asked of the registry ─────────

type fakeRegistry struct {
	owner string // the tenant that holds the entity; "" = none
	err   error
	asked []string
}

func (f *fakeRegistry) EntityInTenant(_ context.Context, _, tenantID string) (registry.Membership, error) {
	f.asked = append(f.asked, tenantID)
	if f.err != nil {
		return registry.Membership{}, f.err
	}
	return registry.Membership{Found: tenantID == f.owner, Status: "ACTIVE"}, nil
}

func registryRouter(s *stubStore, reg registry.Client, enforce bool) chi.Router {
	r := chi.NewRouter()
	h := handler.New(s, &stubPublisher{}, &stubValidator{}, siem.New("", "authorization-svc", zap.NewNop()), platformID, enforce, zap.NewNop())
	h.SetEntityRegistry(reg)
	handler.RegisterRoutes(r, h)
	return r
}

func authorizeVia(r chi.Router, headerTenant, bodyTenant string) *httptest.ResponseRecorder {
	body := `{"principal_id":"p-1","legal_entity_id":"` + entityB + `","action_type":"GOVERNANCE_DECISION_RECORD"`
	if bodyTenant != "" {
		body += `,"tenant_id":"` + bodyTenant + `"`
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewBufferString(body+`}`))
	if headerTenant != "" {
		req.Header.Set("X-Tenant-Id", headerTenant)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// A claimed tenant the registry confirms is used, and the answer is projected.
func TestRegistry_ClaimConfirmedAndProjected(t *testing.T) {
	for _, tc := range []struct{ name, header, body string }{{"header", tenantB, ""}, {"body", "", tenantB}} {
		t.Run(tc.name, func(t *testing.T) {
			s := &stubStore{rbacActions: []string{"GOVERNANCE_DECISION_RECORD"}}
			w := authorizeVia(registryRouter(s, &fakeRegistry{owner: tenantB}, false), tc.header, tc.body)
			if w.Code != http.StatusOK || s.grantedTenantArg != tenantB {
				t.Fatalf("code %d tenant %q: %s", w.Code, s.grantedTenantArg, w.Body.String())
			}
			if len(s.projectedEntities) != 1 || s.projectedEntities[0].TenantID != tenantB || s.projectedEntities[0].LegalEntityID != entityB {
				t.Errorf("registry answer not projected: %+v", s.projectedEntities)
			}
		})
	}
}

// A claimed tenant the registry does not confirm is refused — the old
// entity's owner is checked, not assumed.
func TestRegistry_UnconfirmedClaimRefused(t *testing.T) {
	for _, tc := range []struct{ name, header, body string }{{"header", tenantA, ""}, {"body", "", tenantA}} {
		t.Run(tc.name, func(t *testing.T) {
			s := &stubStore{rbacActions: []string{"GOVERNANCE_DECISION_RECORD"}}
			w := authorizeVia(registryRouter(s, &fakeRegistry{owner: tenantB}, false), tc.header, tc.body)
			if w.Code != http.StatusForbidden || s.recordedParams.ActionType != "" {
				t.Fatalf("tenant A claiming tenant B's unprojected entity: code %d, recorded %q", w.Code, s.recordedParams.ActionType)
			}
		})
	}
}

// Tenantless: the principal's own tenants are tried; the one holding the
// entity scopes and records the decision.
func TestRegistry_TenantlessResolvedFromPrincipalTenants(t *testing.T) {
	s := &stubStore{rbacActions: []string{"GOVERNANCE_DECISION_RECORD"}, principalTenants: []string{tenantA, tenantB}}
	reg := &fakeRegistry{owner: tenantB}
	w := authorizeVia(registryRouter(s, reg, true), "", "")
	if w.Code != http.StatusOK || s.grantedTenantArg != tenantB || s.recordedParams.TenantID != tenantB {
		t.Fatalf("code %d, evaluated in %q, recorded in %q: %s", w.Code, s.grantedTenantArg, s.recordedParams.TenantID, w.Body.String())
	}
}

// Tenantless and in none of the principal's tenants: refused, never
// evaluated across tenants (that is how tenant A's grants reached tenant B).
func TestRegistry_TenantlessForeignEntityRefused(t *testing.T) {
	s := &stubStore{rbacActions: []string{"GOVERNANCE_DECISION_RECORD"}, principalTenants: []string{tenantA}}
	w := authorizeVia(registryRouter(s, &fakeRegistry{owner: tenantB}, false), "", "")
	if w.Code != http.StatusForbidden || s.recordedParams.ActionType != "" {
		t.Fatalf("want 403 and nothing evaluated, got %d (recorded %q)", w.Code, s.recordedParams.ActionType)
	}
}

// Registry or store down: refused, never a fall back to the claim.
func TestRegistry_FailuresFailClosed(t *testing.T) {
	s := &stubStore{rbacActions: []string{"GOVERNANCE_DECISION_RECORD"}}
	if w := authorizeVia(registryRouter(s, &fakeRegistry{err: registry.ErrUnavailable}, false), tenantB, ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("registry down: want 503, got %d", w.Code)
	}
	s2 := &stubStore{rbacActions: []string{"GOVERNANCE_DECISION_RECORD"}, principalTenantsErr: domain.ErrStoreUnavailable}
	if w := authorizeVia(registryRouter(s2, &fakeRegistry{owner: tenantB}, false), "", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("principal tenants unreadable: want 503, got %d", w.Code)
	}
	if s.recordedParams.ActionType != "" || s2.recordedParams.ActionType != "" {
		t.Error("a decision was made without a verified tenant")
	}
}

// A projected owner never costs a registry round trip.
func TestRegistry_ProjectedEntityNotAsked(t *testing.T) {
	s := &stubStore{rbacActions: []string{"GOVERNANCE_DECISION_RECORD"}, entityTenant: tenantB}
	reg := &fakeRegistry{owner: tenantB}
	if w := authorizeVia(registryRouter(s, reg, false), "", ""); w.Code != http.StatusOK || len(reg.asked) != 0 {
		t.Fatalf("code %d, registry asked %v", w.Code, reg.asked)
	}
}

// A malformed claimed tenant is still a 400, not a registry refusal.
func TestRegistry_MalformedClaimStill400(t *testing.T) {
	s := &stubStore{}
	if w := authorizeVia(registryRouter(s, &fakeRegistry{}, false), "not-a-uuid", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}
