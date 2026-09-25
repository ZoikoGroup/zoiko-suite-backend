package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/commercial-account-svc/internal/authz"
	"zoiko.io/commercial-account-svc/internal/domain"
	svcmiddleware "zoiko.io/commercial-account-svc/internal/middleware"
)

type entStub struct {
	err     error
	tenant  string
	calls   []string
	decided domain.CapabilityDecision
	applied *domain.CommercialRestriction
}

func (e *entStub) rec(ctx context.Context, name string) {
	e.calls = append(e.calls, name)
	e.tenant = svcmiddleware.TenantFromContext(ctx)
}
func (e *entStub) EvaluateCapability(ctx context.Context, key string, _ *int64, _ time.Time) (*domain.CapabilityDecision, error) {
	e.rec(ctx, "Evaluate")
	d := e.decided
	d.CapabilityKey = key
	return &d, e.err
}
func (e *entStub) GetEffectiveEntitlements(ctx context.Context, _ time.Time) ([]domain.CapabilityDecision, error) {
	e.rec(ctx, "GetEffective")
	return nil, e.err
}
func (e *entStub) GetLimit(ctx context.Context, _ string, _ time.Time) (*domain.CapabilityDecision, error) {
	e.rec(ctx, "GetLimit")
	return &e.decided, e.err
}
func (e *entStub) ExplainDecision(ctx context.Context, _ string, _ *int64, _ time.Time) (*domain.CapabilityDecision, error) {
	e.rec(ctx, "Explain")
	return &e.decided, e.err
}
func (e *entStub) RecomputeEntitlements(ctx context.Context, _ string, _ time.Time) ([]domain.CapabilityDecision, error) {
	e.rec(ctx, "Recompute")
	return nil, e.err
}
func (e *entStub) ApplyRestriction(ctx context.Context, r *domain.CommercialRestriction, _ domain.IdempotencyClaim) (*domain.CommercialRestriction, error) {
	e.rec(ctx, "ApplyRestriction")
	e.applied = r
	return r, e.err
}
func (e *entStub) RemoveRestriction(ctx context.Context, id, _, _ string, _ time.Time) (*domain.CommercialRestriction, error) {
	e.rec(ctx, "RemoveRestriction")
	return &domain.CommercialRestriction{RestrictionID: id}, e.err
}
func (e *entStub) ListRestrictions(ctx context.Context) ([]domain.CommercialRestriction, error) {
	e.rec(ctx, "ListRestrictions")
	return nil, e.err
}
func (e *entStub) PublishEntitlementPolicy(ctx context.Context, p *domain.EntitlementPolicyVersion, _ domain.IdempotencyClaim) (*domain.EntitlementPolicyVersion, error) {
	e.rec(ctx, "PublishPolicy")
	return p, e.err
}
func (e *entStub) GetEntitlementPolicy(ctx context.Context, _ time.Time) (*domain.EntitlementPolicyVersion, error) {
	e.rec(ctx, "GetPolicy")
	return nil, e.err
}

func newEntRouter(st *entStub, az *scopedAuthz) http.Handler {
	logger, _ := zap.NewDevelopment()
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	RegisterEntitlementRoutes(r, NewEntitlementHandler(st, az, logger).WithClock(func() time.Time { return fixedNow }))
	return r
}

func entHeaders() map[string]string { return map[string]string{"X-Tenant-Id": tenantOrg} }

func TestEntitlementHandler_SelfReadUsesTheVerifiedTenant(t *testing.T) {
	st := &entStub{}
	az := &scopedAuthz{}
	w := serve(t, newEntRouter(st, az), req{method: http.MethodPost, path: "/v1/commercial/entitlements:evaluate",
		body: `{"capability_key":"api_access"}`, headers: entHeaders()})
	if w.Code != http.StatusOK || st.tenant != tenantOrg || az.checked[0] != tenantOrg+"|"+ActionEntitlementRead {
		t.Fatalf("HTTP %d tenant=%s checked=%v %s", w.Code, st.tenant, az.checked, w.Body.String())
	}
}

// An operator reading another organization's entitlements needs the
// platform restriction-read grant, not the organization's own.
func TestEntitlementHandler_OperatorReadNeedsThePlatformGrant(t *testing.T) {
	st := &entStub{}
	az := &scopedAuthz{}
	w := serve(t, newEntRouter(st, az), req{method: http.MethodGet,
		path: "/v1/commercial/entitlements/effective?organization_id=" + customerOrg, headers: entHeaders()})
	if w.Code != http.StatusOK || st.tenant != customerOrg || az.checked[0] != platformScopeID+"|"+ActionRestrictionRead {
		t.Fatalf("HTTP %d tenant=%s checked=%v", w.Code, st.tenant, az.checked)
	}
	denied := &scopedAuthz{deny: map[string]error{platformScopeID + "|" + ActionRestrictionRead: authzpkg.ErrAuthorizationDenied}}
	st = &entStub{}
	w = serve(t, newEntRouter(st, denied), req{method: http.MethodGet,
		path: "/v1/commercial/entitlements/effective?organization_id=" + customerOrg, headers: entHeaders()})
	if w.Code != http.StatusForbidden || len(st.calls) != 0 {
		t.Fatalf("an operator without the grant read another org's entitlements: HTTP %d", w.Code)
	}
}

func TestEntitlementHandler_MissingCapabilityKeyRefused(t *testing.T) {
	st := &entStub{}
	w := serve(t, newEntRouter(st, &scopedAuthz{}), req{method: http.MethodPost, path: "/v1/commercial/entitlements:evaluate",
		body: `{}`, headers: entHeaders()})
	if w.Code != http.StatusBadRequest || problemCode(t, w) != CodeCapabilityKeyRequired || len(st.calls) != 0 {
		t.Fatalf("HTTP %d %s", w.Code, w.Body.String())
	}
}

// A restriction is never applied or lifted by the organization it targets —
// only platform authority, never a self-service org-scoped grant.
func TestEntitlementHandler_RestrictionsArePlatformAuthorityOnly(t *testing.T) {
	az := &scopedAuthz{}
	st := &entStub{}
	w := serve(t, newEntRouter(st, az), req{method: http.MethodPost, path: "/v1/commercial/restrictions",
		body:    `{"organization_id":"` + customerOrg + `","level":"RESTRICTED","reason_code":"FRAUD_HOLD","policy_ref":"pol-1","basis_ref":"case-77"}`,
		headers: map[string]string{"Idempotency-Key": "k"}})
	if w.Code != http.StatusCreated || st.tenant != customerOrg || az.checked[0] != platformScopeID+"|"+ActionRestrictionApply {
		t.Fatalf("apply: HTTP %d tenant=%s checked=%v %s", w.Code, st.tenant, az.checked, w.Body.String())
	}
	if st.applied.Level != domain.RestrictionRestricted || st.applied.PolicyRef != "pol-1" {
		t.Fatalf("applied restriction: %+v", st.applied)
	}

	denied := &scopedAuthz{deny: map[string]error{platformScopeID + "|" + ActionRestrictionApply: authzpkg.ErrAuthorizationDenied}}
	st = &entStub{}
	w = serve(t, newEntRouter(st, denied), req{method: http.MethodPost, path: "/v1/commercial/restrictions",
		body:    `{"organization_id":"` + customerOrg + `","level":"RESTRICTED","reason_code":"FRAUD_HOLD","policy_ref":"p","basis_ref":"b"}`,
		headers: map[string]string{"Idempotency-Key": "k"}})
	if w.Code != http.StatusForbidden || len(st.calls) != 0 {
		t.Fatalf("a principal without the apply grant restricted an org: HTTP %d", w.Code)
	}

	id := domain.NewCommercialID(domain.PrefixRestriction)
	st = &entStub{}
	w = serve(t, newEntRouter(st, &scopedAuthz{}), req{method: http.MethodPost, path: "/v1/commercial/restrictions/" + id + ":remove",
		body: `{"reason":"customer paid"}`})
	if w.Code != http.StatusOK || len(st.calls) != 1 || st.calls[0] != "RemoveRestriction" {
		t.Fatalf("remove: HTTP %d calls=%v", w.Code, st.calls)
	}
	st = &entStub{}
	w = serve(t, newEntRouter(st, &scopedAuthz{}), req{method: http.MethodPost, path: "/v1/commercial/restrictions/" + id + ":remove", body: `{}`})
	if w.Code != http.StatusBadRequest || len(st.calls) != 0 {
		t.Fatalf("a restriction was removed without a reason: HTTP %d", w.Code)
	}
}

func TestEntitlementHandler_PublishPolicyValidatesReadOnlyDays(t *testing.T) {
	st := &entStub{}
	w := serve(t, newEntRouter(st, &scopedAuthz{}), req{method: http.MethodPost, path: "/v1/commercial/entitlement-policy",
		body: `{"ended_outcome":"READ_ONLY","ended_read_only_days":0,"reason":"standard grace"}`, headers: map[string]string{"Idempotency-Key": "k"}})
	if w.Code != http.StatusBadRequest || len(st.calls) != 0 {
		t.Fatalf("a READ_ONLY policy with zero days was published: HTTP %d", w.Code)
	}
	w = serve(t, newEntRouter(st, &scopedAuthz{}), req{method: http.MethodPost, path: "/v1/commercial/entitlement-policy",
		body: `{"ended_outcome":"READ_ONLY","ended_read_only_days":14,"reason":"standard grace"}`, headers: map[string]string{"Idempotency-Key": "k"}})
	if w.Code != http.StatusCreated {
		t.Fatalf("a valid policy: HTTP %d %s", w.Code, w.Body.String())
	}
}
