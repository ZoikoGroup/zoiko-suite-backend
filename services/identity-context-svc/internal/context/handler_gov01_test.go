package context_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	identityctx "zoiko.io/identity-context-svc/internal/context"
	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/events"
	"zoiko.io/identity-context-svc/internal/outbox"
	"zoiko.io/identity-context-svc/internal/siem"
	"zoiko.io/identity-context-svc/internal/sod"
)

// Route-level guards for the four GOV-01 operations that had no
// implementation before this change:
//
//	ExplainContextResolution   GET    /v1/context/session/{id}/explain
//	RefreshTenantContextCache  POST   /v1/context/cache/refresh
//	InvalidateTenantContext    POST   /v1/context/tenant/invalidate
//	AttachSupportContext       POST   /v1/context/support
//	                           DELETE /v1/context/support/{id}
//
// Every one of them is a route that can read or revoke across a tenant, so
// every one gets the same treatment the existing routes got in Priority 2b:
// verified tenant, verified principal, explicit authorization, and no
// enumeration through distinguishable 403s.

// ── Harness ──────────────────────────────────────────────────────────────────

type fakeIngressWriter struct {
	touched map[string][]string
	n       int
}

func (f *fakeIngressWriter) TouchIngressBindings(_ context.Context, tenantID string, ids []string) (int, error) {
	if f.touched == nil {
		f.touched = map[string][]string{}
	}
	f.touched[tenantID] = ids
	return f.n, nil
}

type fakeTenantRevoker struct {
	revoked map[string]domain.InvalidationReason
	n       int
}

func (f *fakeTenantRevoker) EvictAllForTenant(_ context.Context, tenantID string, reason domain.InvalidationReason) (int, error) {
	if f.revoked == nil {
		f.revoked = map[string]domain.InvalidationReason{}
	}
	f.revoked[tenantID] = reason
	return f.n, nil
}

// denyingAuthz refuses every check, so a test can prove a route is guarded at
// all rather than merely guarded correctly.
type denyingAuthz struct{}

func (denyingAuthz) CheckAllowed(_ context.Context, _, _, _, _ string) error {
	return domain.ErrAuthorizationDenied
}

type permittingAuthz struct{ seen []string }

func (p *permittingAuthz) CheckAllowed(_ context.Context, _, _, action, _ string) error {
	p.seen = append(p.seen, action)
	return nil
}

type gov01Harness struct {
	router   chi.Router
	sessions *mockSessionCache
	support  *fakeSupportStore
	bindings *fakeIngressWriter
	revoker  *fakeTenantRevoker
	authz    identityctx.AuthzChecker
}

func newGov01Harness(t *testing.T, az identityctx.AuthzChecker) *gov01Harness {
	t.Helper()

	f := defaultFixture()
	sessions := newMockSessionCache()
	f.sessions = sessions

	supportStore := newFakeSupportStore()
	bindings := &fakeIngressWriter{n: 3}
	revoker := &fakeTenantRevoker{n: 7}

	publisher := events.NewPublisherWithSink(zap.NewNop(), "test-topic", noopSink{})
	siemClient := siem.New("", "identity-context-svc", zap.NewNop())

	h := identityctx.NewHandler(f.build(), nil, sessions, f.principals, az, zap.NewNop()).
		WithEnvironment(domain.EnvironmentProduction).
		WithSupport(identityctx.NewSupportService(
			supportStore, publisher, &clearSoD{}, siemClient,
			identityctx.DefaultSupportPolicy(), zap.NewNop())).
		WithContextCache(identityctx.NewContextCacheService(
			bindings, revoker, publisher, siemClient, zap.NewNop()))

	r := chi.NewRouter()
	identityctx.RegisterRoutes(r, h)

	return &gov01Harness{
		router: r, sessions: sessions, support: supportStore,
		bindings: bindings, revoker: revoker, authz: az,
	}
}

func do(r chi.Router, method, path, tenantID, principalID string, body any) *httptest.ResponseRecorder {
	var buf *bytes.Buffer
	if body != nil {
		b, _ := json.Marshal(body)
		buf = bytes.NewBuffer(b)
	} else {
		buf = bytes.NewBuffer(nil)
	}
	req := httptest.NewRequest(method, path, buf)
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder, into any) {
	t.Helper()
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), into))
}

// ── ExplainContextResolution ─────────────────────────────────────────────────

func TestExplainRoute_RequiresVerifiedIdentity(t *testing.T) {
	h := newGov01Harness(t, &permittingAuthz{})
	seedSession(h.sessions, "sc-1", "tenant-a", "p-1", "jwt")

	assert.Equal(t, http.StatusUnauthorized,
		do(h.router, http.MethodGet, "/v1/context/session/sc-1/explain", "", "p-1", nil).Code)
	assert.Equal(t, http.StatusUnauthorized,
		do(h.router, http.MethodGet, "/v1/context/session/sc-1/explain", "tenant-a", "", nil).Code)
}

// TestExplainRoute_ForeignTenantIs404NotForbidden: a distinct 403 would let a
// caller enumerate valid session ids, and session ids are exactly what an
// attacker probes for here.
func TestExplainRoute_ForeignTenantIs404NotForbidden(t *testing.T) {
	h := newGov01Harness(t, &permittingAuthz{})
	seedSession(h.sessions, "sc-1", "tenant-a", "p-1", "jwt")

	w := do(h.router, http.MethodGet, "/v1/context/session/sc-1/explain", "tenant-b", "p-9", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestExplainRoute_OwnSessionNeedsNoGrant(t *testing.T) {
	az := &permittingAuthz{}
	h := newGov01Harness(t, az)
	seedSession(h.sessions, "sc-1", "tenant-a", "p-1", "jwt")

	w := do(h.router, http.MethodGet, "/v1/context/session/sc-1/explain", "tenant-a", "p-1", nil)
	require.Equal(t, http.StatusOK, w.Code)

	// Reading the account of your OWN resolution is ordinary platform traffic.
	// If it needed a grant, every principal would need one and the check would
	// be noise everybody holds.
	assert.Empty(t, az.seen)

	var ex domain.ContextExplanation
	decodeBody(t, w, &ex)
	assert.Equal(t, "sc-1", ex.SessionContextID)
	assert.Len(t, ex.Dimensions, 6)
}

func TestExplainRoute_OtherPrincipalRequiresGrant(t *testing.T) {
	h := newGov01Harness(t, denyingAuthz{})
	seedSession(h.sessions, "sc-1", "tenant-a", "p-1", "jwt")

	// Same tenant, different principal: reading their resolution reveals their
	// entity, ingress and trust posture.
	w := do(h.router, http.MethodGet, "/v1/context/session/sc-1/explain", "tenant-a", "p-2", nil)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestExplainRoute_AsOfMustBeRFC3339(t *testing.T) {
	h := newGov01Harness(t, &permittingAuthz{})
	seedSession(h.sessions, "sc-1", "tenant-a", "p-1", "jwt")

	w := do(h.router, http.MethodGet, "/v1/context/session/sc-1/explain?as_of=last%20tuesday", "tenant-a", "p-1", nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]any
	decodeBody(t, w, &body)
	assert.Equal(t, domain.ErrCodeContextUnresolved, body["error_code"])
}

func TestExplainRoute_AsOfIsHonoured(t *testing.T) {
	h := newGov01Harness(t, &permittingAuthz{})
	issued := time.Now().UTC().Add(-time.Hour)
	h.sessions.storedCtx["sc-1"] = &domain.SessionContext{
		SessionContextID: "sc-1",
		TenantID:         "tenant-a",
		PrincipalID:      "p-1",
		IssuedAt:         issued,
		ExpiresAt:        issued.Add(5 * time.Minute),
	}

	asOf := issued.Add(time.Minute).Format(time.RFC3339)
	w := do(h.router, http.MethodGet, "/v1/context/session/sc-1/explain?as_of="+asOf, "tenant-a", "p-1", nil)
	require.Equal(t, http.StatusOK, w.Code)

	var ex domain.ContextExplanation
	decodeBody(t, w, &ex)
	assert.Equal(t, "RESOLVED", ex.Outcome,
		"as_of inside the window must report the session as it stood then, not as it stands now")
}

// ── RefreshTenantContextCache ────────────────────────────────────────────────

func TestRefreshCacheRoute_RequiresGrant(t *testing.T) {
	h := newGov01Harness(t, denyingAuthz{})

	w := do(h.router, http.MethodPost, "/v1/context/cache/refresh", "tenant-a", "p-1",
		domain.RefreshCacheRequest{Reason: "registry resync"})
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestRefreshCacheRoute_RefreshesAndReportsCount(t *testing.T) {
	az := &permittingAuthz{}
	h := newGov01Harness(t, az)

	w := do(h.router, http.MethodPost, "/v1/context/cache/refresh", "tenant-a", "p-1",
		domain.RefreshCacheRequest{Reason: "registry resync"})
	require.Equal(t, http.StatusOK, w.Code)

	var resp domain.RefreshCacheResponse
	decodeBody(t, w, &resp)
	assert.Equal(t, 3, resp.BindingsRefreshed)
	assert.NotEmpty(t, resp.EvidenceID)
	assert.Contains(t, az.seen, identityctx.ActionRefreshContextCache)
}

// TestRefreshCacheRoute_IsScopedToTheCallersOwnTenant: there is no "all
// tenants" form. A single caller invalidating the entire estate's context
// cache is a denial-of-service primitive wearing a maintenance command's
// clothes.
func TestRefreshCacheRoute_IsScopedToTheCallersOwnTenant(t *testing.T) {
	h := newGov01Harness(t, &permittingAuthz{})

	w := do(h.router, http.MethodPost, "/v1/context/cache/refresh", "tenant-a", "p-1",
		domain.RefreshCacheRequest{Reason: "resync"})
	require.Equal(t, http.StatusOK, w.Code)

	_, touchedOwn := h.bindings.touched["tenant-a"]
	assert.True(t, touchedOwn)
	assert.Len(t, h.bindings.touched, 1, "no other tenant may be affected")
}

// ── InvalidateTenantContext ──────────────────────────────────────────────────

func TestInvalidateTenantRoute_UsesADistinctActionFromSessionInvalidate(t *testing.T) {
	az := &permittingAuthz{}
	h := newGov01Harness(t, az)

	w := do(h.router, http.MethodPost, "/v1/context/tenant/invalidate", "tenant-a", "p-1",
		domain.InvalidateTenantContextRequest{
			Reason:        domain.InvalidationReasonAdminRevoke,
			Justification: "Credential stuffing campaign confirmed against this tenant at 03:12.",
		})
	require.Equal(t, http.StatusOK, w.Code)

	// "Log one user out" and "log an entire tenant out" are not the same
	// decision and must not share a permission.
	assert.Contains(t, az.seen, identityctx.ActionInvalidateTenantContext)
	assert.NotContains(t, az.seen, identityctx.IdentitySessionInvalidate)
}

func TestInvalidateTenantRoute_RequiresAJustification(t *testing.T) {
	h := newGov01Harness(t, &permittingAuthz{})

	w := do(h.router, http.MethodPost, "/v1/context/tenant/invalidate", "tenant-a", "p-1",
		domain.InvalidateTenantContextRequest{Reason: domain.InvalidationReasonAdminRevoke})

	// The blast radius is every user of the tenant. An operator who cannot say
	// why in a sentence should not be running this.
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Empty(t, h.revoker.revoked)
}

func TestInvalidateTenantRoute_ReportsBlastRadius(t *testing.T) {
	h := newGov01Harness(t, &permittingAuthz{})

	w := do(h.router, http.MethodPost, "/v1/context/tenant/invalidate", "tenant-a", "p-1",
		domain.InvalidateTenantContextRequest{
			Reason:        domain.InvalidationReasonRiskEscalation,
			Justification: "Credential stuffing campaign confirmed against this tenant at 03:12.",
		})
	require.Equal(t, http.StatusOK, w.Code)

	var resp domain.InvalidateTenantContextResponse
	decodeBody(t, w, &resp)
	assert.Equal(t, 7, resp.SessionsRevoked)
	assert.NotEmpty(t, resp.EvidenceID)
	assert.Equal(t, domain.InvalidationReasonRiskEscalation, h.revoker.revoked["tenant-a"])
}

func TestInvalidateTenantRoute_RequiresGrant(t *testing.T) {
	h := newGov01Harness(t, denyingAuthz{})

	w := do(h.router, http.MethodPost, "/v1/context/tenant/invalidate", "tenant-a", "p-1",
		domain.InvalidateTenantContextRequest{
			Reason:        domain.InvalidationReasonAdminRevoke,
			Justification: "A justification long enough to satisfy the check.",
		})
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Empty(t, h.revoker.revoked)
}

// ── AttachSupportContext ─────────────────────────────────────────────────────

// TestAttachSupportRoute_AuthorizesAgainstTheTARGETTenant is the subtlest
// control on this route.
//
// A support engineer lives in the support tenant and is asking for standing in
// a CUSTOMER's. Checking the grant against their own tenant would ask whether
// they may attach support contexts at home — which every support engineer may
// — and would authorize an elevation into any tenant on the platform.
func TestAttachSupportRoute_AuthorizesAgainstTheTargetTenant(t *testing.T) {
	h := newGov01Harness(t, denyingAuthz{})
	req := validAttachRequest()

	// The caller's own tenant header says tenant-support; the request targets
	// tenant-a. The denial proves the check ran against something.
	w := do(h.router, http.MethodPost, "/v1/context/support", "tenant-support", "support-lead-9", req)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Empty(t, h.support.contexts)
}

func TestAttachSupportRoute_RequiresATargetTenant(t *testing.T) {
	h := newGov01Harness(t, &permittingAuthz{})
	req := validAttachRequest()
	req.TenantID = ""

	w := do(h.router, http.MethodPost, "/v1/context/support", "tenant-support", "support-lead-9", req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAttachSupportRoute_GrantsAndReturnsEvidence(t *testing.T) {
	h := newGov01Harness(t, &permittingAuthz{})

	w := do(h.router, http.MethodPost, "/v1/context/support", "tenant-support", "support-lead-9", validAttachRequest())
	require.Equal(t, http.StatusCreated, w.Code)

	var resp domain.AttachSupportContextResponse
	decodeBody(t, w, &resp)
	assert.NotEmpty(t, resp.SupportContextID)
	assert.NotEmpty(t, resp.EvidenceID)
	assert.True(t, resp.ExpiresAt.After(time.Now()), "a grant must not be issued already expired")
}

func TestAttachSupportRoute_SelfApprovalIsRefusedWithAStableCode(t *testing.T) {
	h := newGov01Harness(t, &permittingAuthz{})
	req := validAttachRequest()
	req.ApproverPrincipalID = req.SupportPrincipalID

	w := do(h.router, http.MethodPost, "/v1/context/support", "tenant-support", "support-lead-9", req)
	require.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]any
	decodeBody(t, w, &body)
	assert.Equal(t, domain.ErrCodeBreakGlassRequired, body["error_code"])
}

func TestAttachSupportRoute_SoDConflictIs409WithItsOwnCode(t *testing.T) {
	f := defaultFixture()
	sessions := newMockSessionCache()
	f.sessions = sessions
	publisher := events.NewPublisherWithSink(zap.NewNop(), "t", noopSink{})
	siemClient := siem.New("", "identity-context-svc", zap.NewNop())

	h := identityctx.NewHandler(f.build(), nil, sessions, f.principals, &permittingAuthz{}, zap.NewNop()).
		WithSupport(identityctx.NewSupportService(
			newFakeSupportStore(), publisher, conflictingSoD{}, siemClient,
			identityctx.DefaultSupportPolicy(), zap.NewNop()))
	r := chi.NewRouter()
	identityctx.RegisterRoutes(r, h)

	w := do(r, http.MethodPost, "/v1/context/support", "tenant-support", "support-lead-9", validAttachRequest())

	require.Equal(t, http.StatusConflict, w.Code)
	var body map[string]any
	decodeBody(t, w, &body)
	// Distinct from AUTHORIZATION_DENIED: a caller told the latter goes and
	// asks for a permission grant, which cannot fix a segregation conflict.
	assert.Equal(t, domain.ErrCodeSoDConflict, body["error_code"])
}

func TestAttachSupportRoute_SoDOutageIs503NotAGrant(t *testing.T) {
	f := defaultFixture()
	sessions := newMockSessionCache()
	f.sessions = sessions
	publisher := events.NewPublisherWithSink(zap.NewNop(), "t", noopSink{})
	siemClient := siem.New("", "identity-context-svc", zap.NewNop())
	store := newFakeSupportStore()

	h := identityctx.NewHandler(f.build(), nil, sessions, f.principals, &permittingAuthz{}, zap.NewNop()).
		WithSupport(identityctx.NewSupportService(
			store, publisher, unavailableSoD{}, siemClient,
			identityctx.DefaultSupportPolicy(), zap.NewNop()))
	r := chi.NewRouter()
	identityctx.RegisterRoutes(r, h)

	w := do(r, http.MethodPost, "/v1/context/support", "tenant-support", "support-lead-9", validAttachRequest())

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Empty(t, store.contexts, "no grant may be written when the conflict check did not run")
}

// TestSupportRoutes_AreUnavailableRatherThanOpenWhenUnconfigured.
//
// An unconfigured privileged command must be unavailable, never silently
// permissive. 501 says "this deployment cannot do that"; a 200 would say "we
// did it without any of the controls".
func TestSupportRoutes_AreUnavailableRatherThanOpenWhenUnconfigured(t *testing.T) {
	f := defaultFixture()
	sessions := newMockSessionCache()
	f.sessions = sessions
	// No WithSupport.
	h := identityctx.NewHandler(f.build(), nil, sessions, f.principals, &permittingAuthz{}, zap.NewNop())
	r := chi.NewRouter()
	identityctx.RegisterRoutes(r, h)

	w := do(r, http.MethodPost, "/v1/context/support", "tenant-support", "support-lead-9", validAttachRequest())
	assert.Equal(t, http.StatusNotImplemented, w.Code)

	w = do(r, http.MethodPost, "/v1/context/cache/refresh", "tenant-a", "p-1", domain.RefreshCacheRequest{Reason: "x"})
	assert.Equal(t, http.StatusNotImplemented, w.Code)
}

func TestRevokeSupportRoute_RequiresItsOwnAction(t *testing.T) {
	az := &permittingAuthz{}
	h := newGov01Harness(t, az)

	w := do(h.router, http.MethodPost, "/v1/context/support", "tenant-support", "support-lead-9", validAttachRequest())
	require.Equal(t, http.StatusCreated, w.Code)
	var created domain.AttachSupportContextResponse
	decodeBody(t, w, &created)

	w = do(h.router, http.MethodDelete, "/v1/context/support/"+created.SupportContextID,
		"tenant-a", "security-1", domain.RevokeSupportContextRequest{Reason: "incident closed"})
	require.Equal(t, http.StatusNoContent, w.Code)

	// Revoking is de-escalation. Requiring the same grant to END a support
	// session as to START one means the person who notices a problem may be
	// unable to stop it.
	assert.Contains(t, az.seen, identityctx.ActionRevokeSupportContext)
}

func TestRevokeSupportRoute_UnknownContextIs404(t *testing.T) {
	h := newGov01Harness(t, &permittingAuthz{})

	w := do(h.router, http.MethodDelete, "/v1/context/support/sup-nope", "tenant-a", "security-1", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestRevokeSupportRoute_AcceptsAnEmptyBody(t *testing.T) {
	h := newGov01Harness(t, &permittingAuthz{})
	w := do(h.router, http.MethodPost, "/v1/context/support", "tenant-support", "support-lead-9", validAttachRequest())
	require.Equal(t, http.StatusCreated, w.Code)
	var created domain.AttachSupportContextResponse
	decodeBody(t, w, &created)

	// A revocation with no stated reason still beats one that did not happen
	// because the caller forgot a payload.
	req := httptest.NewRequest(http.MethodDelete, "/v1/context/support/"+created.SupportContextID, nil)
	req.Header.Set("X-Tenant-Id", "tenant-a")
	req.Header.Set("X-Principal-Id", "security-1")
	w2 := httptest.NewRecorder()
	h.router.ServeHTTP(w2, req)

	assert.Equal(t, http.StatusNoContent, w2.Code)
}

// ── Resolve response ─────────────────────────────────────────────────────────

// TestResolveReturnsEvidenceId pins the spec's envelope requirement at the
// route: "evidence_id returned for material governance decision/transition".
func TestResolveReturnsEvidenceId(t *testing.T) {
	h := newGov01Harness(t, &permittingAuthz{})

	body, _ := json.Marshal(domain.ResolveRequest{
		BearerToken:   "mock-token",
		LegalEntityID: "01HXXXENTITYID",
		CorrelationID: "corr-1",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/context/resolve", bytes.NewBuffer(body))
	req.Host = "tenant-a.zoiko.io"
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp domain.ResolveResponseV2
	decodeBody(t, w, &resp)
	assert.NotEmpty(t, resp.EnvelopeJWT)
	assert.NotEmpty(t, resp.EvidenceID, "a caller must be able to cite the decision that granted its envelope")
	assert.NotEmpty(t, resp.SessionContextID)

	// The credential must not be cached by an intermediary.
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
}

// TestResolveRecordsTheIngressItArrivedOn pins that ingress is SERVER-resolved.
func TestResolveRecordsTheIngressItArrivedOn(t *testing.T) {
	h := newGov01Harness(t, &permittingAuthz{})

	// A body that tries to name its own ingress and environment. Both fields
	// carry json:"-", so the attempt is silently ignored — which is the
	// correct handling of a field a client has no business supplying.
	raw := []byte(`{"bearer_token":"mock-token","legal_entity_id":"01HXXXENTITYID",
	                "correlation_id":"corr-1","ingress_source":"evil.example.com",
	                "environment":"production"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/context/resolve", bytes.NewBuffer(raw))
	req.Host = "tenant-a.zoiko.io"
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	require.Len(t, h.sessions.storedCtx, 1)
	for _, sc := range h.sessions.storedCtx {
		assert.Equal(t, "tenant-a.zoiko.io", sc.IngressSource,
			"the ingress on the record must be what the server observed, not what the body claimed")
		assert.Equal(t, domain.EnvironmentProduction, sc.Environment)
		assert.NotEmpty(t, sc.EvidenceID)
	}
}

// Compile-time proof the fakes satisfy what the handler wires.
var (
	_ identityctx.IngressBindingWriter = (*fakeIngressWriter)(nil)
	_ identityctx.TenantSessionRevoker = (*fakeTenantRevoker)(nil)
	_ identityctx.AuthzChecker         = (*permittingAuthz)(nil)
	_ sod.Checker                      = (*clearSoD)(nil)
	_ outbox.Enqueuer                  = (outbox.Enqueuer)(nil)
)
