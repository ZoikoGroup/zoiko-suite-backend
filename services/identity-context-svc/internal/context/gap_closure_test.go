package context_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	identityctx "zoiko.io/identity-context-svc/internal/context"
	"zoiko.io/identity-context-svc/internal/domain"
)

// Tests for the GOV-01 §4 gaps closed on 2026-09-23. Each names the audit
// finding it pins, because a test that outlives the memory of why it exists is
// the first one somebody deletes.

// ── Gap 1: X-Support-Context-Id was accepted unverified ─────────────────────

// fakeSupportVerifier stands in for SupportService.Verify.
type fakeSupportVerifier struct {
	err error

	gotSupportContextID string
	gotTenantID         string
	gotSupportPrincipal string
	gotSubjectPrincipal string
	calls               int
}

func (f *fakeSupportVerifier) Verify(_ context.Context, supportContextID, tenantID, supportPrincipalID, subjectPrincipalID string) (*domain.SupportContext, error) {
	f.calls++
	f.gotSupportContextID = supportContextID
	f.gotTenantID = tenantID
	f.gotSupportPrincipal = supportPrincipalID
	f.gotSubjectPrincipal = subjectPrincipalID
	if f.err != nil {
		return nil, f.err
	}
	return &domain.SupportContext{SupportContextID: supportContextID, TenantID: tenantID}, nil
}

func supportRequest(id string) domain.ResolveRequest {
	req := baseRequest
	req.SupportContextID = &id
	return req
}

// NP3, the headline case. Before the fix, Resolve stamped whatever the client
// sent onto the session evidence without ever asking whether the grant was
// live — so an EXPIRED elevation produced a perfectly ordinary 200 and a
// session attributed to a grant that had already ended.
func TestResolve_ExpiredSupportContextIsRefused(t *testing.T) {
	f := defaultFixture()
	v := &fakeSupportVerifier{err: domain.ErrSupportContextExpired}

	result, err := f.build().WithSupportVerifier(v).Resolve(context.Background(), supportRequest("sc-expired"))

	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrSupportContextExpired)
	assert.Nil(t, result)

	// No unauthorized side effect: §4's negative-path column requires the
	// refusal to leave nothing behind.
	assert.Empty(t, f.sessions.storedCtx, "a refused elevation must not persist session evidence")
}

// Absent, revoked, another principal's and another tenant's grant all arrive
// as ErrSupportContextNotFound by Verify's own non-enumeration design, so one
// test covers the four: a caller must not be able to tell them apart.
func TestResolve_UnknownRevokedOrForeignSupportContextIsRefused(t *testing.T) {
	f := defaultFixture()
	v := &fakeSupportVerifier{err: domain.ErrSupportContextNotFound}

	result, err := f.build().WithSupportVerifier(v).Resolve(context.Background(), supportRequest("sc-fictional"))

	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrSupportContextNotFound)
	assert.Nil(t, result)
	assert.Empty(t, f.sessions.storedCtx)
}

// The fail-closed case, and the one most likely to be got wrong. ingress and
// residency both treat a nil checker as "nothing to check"; copying that here
// would restore the original defect exactly, because an unwired verifier would
// mean every asserted elevation is admitted unchecked.
func TestResolve_SupportContextWithNoVerifierIsRefusedNotIgnored(t *testing.T) {
	f := defaultFixture()

	result, err := f.build().Resolve(context.Background(), supportRequest("sc-anything"))

	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrSupportContextUnverifiable)
	assert.Nil(t, result)
	assert.Empty(t, f.sessions.storedCtx)
}

// The grant must be checked against the TOKEN's principal and tenant, never
// against anything the caller supplied. This is the assertion that would fail
// if somebody later "simplified" the check to trust a header.
func TestResolve_SupportContextIsVerifiedAgainstTheVerifiedIdentity(t *testing.T) {
	f := defaultFixture()
	v := &fakeSupportVerifier{}

	_, err := f.build().WithSupportVerifier(v).Resolve(context.Background(), supportRequest("sc-live"))
	require.NoError(t, err)

	require.Equal(t, 1, v.calls)
	assert.Equal(t, "sc-live", v.gotSupportContextID)
	assert.Equal(t, activePrincipal.TenantID, v.gotTenantID)
	assert.Equal(t, activePrincipal.PrincipalID, v.gotSupportPrincipal)
	// Empty by design: the session being minted is the operator's own, and
	// subject coverage is decided per request, later, by Covers.
	assert.Empty(t, v.gotSubjectPrincipal)
}

// Ordinary traffic must not pay for the check, and must not be refused by it.
func TestResolve_NoSupportContextSkipsVerificationEntirely(t *testing.T) {
	f := defaultFixture()
	v := &fakeSupportVerifier{err: domain.ErrSupportContextNotFound}

	result, err := f.build().WithSupportVerifier(v).Resolve(context.Background(), baseRequest)

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Zero(t, v.calls, "a request asserting no elevation must not consult the grant register")
}

// ── Gap 4: elevation was invisible downstream ───────────────────────────────

// The session row carried support_context_id; the envelope did not. Every
// consumer therefore saw support traffic as ordinary traffic, so §1's
// "scoped ... fully evidenced" stopped at this service's own database.
func TestResolve_VerifiedElevationReachesTheEnvelopeAndTheEvidence(t *testing.T) {
	f := defaultFixture()
	v := &fakeSupportVerifier{}

	result, err := f.build().WithSupportVerifier(v).Resolve(context.Background(), supportRequest("sc-live"))
	require.NoError(t, err)

	require.Len(t, f.sessions.storedCtx, 1)
	for _, sc := range f.sessions.storedCtx {
		require.NotNil(t, sc.SupportContextID, "session evidence must record the grant")
		assert.Equal(t, "sc-live", *sc.SupportContextID)
	}

	require.NotNil(t, f.signer.captured, "the signer should have seen an envelope")
	require.NotNil(t, f.signer.captured.SupportContextID,
		"the envelope must carry the elevation or no consumer can see it")
	assert.Equal(t, "sc-live", *f.signer.captured.SupportContextID)
	assert.NotEmpty(t, result.EnvelopeJWT)
}

// An ordinary session must carry no claim at all, so a consumer's
// "is this support traffic?" is a presence check that a zero value cannot
// satisfy.
func TestResolve_OrdinarySessionCarriesNoSupportClaim(t *testing.T) {
	f := defaultFixture()

	_, err := f.build().Resolve(context.Background(), baseRequest)
	require.NoError(t, err)

	require.NotNil(t, f.signer.captured)
	assert.Nil(t, f.signer.captured.SupportContextID)
}

// ── Gap 6: envelope facts were parsed and then discarded ────────────────────

// §4 lists source channel as server-resolved context and workload identity as
// a required source input, and requires "correlation/causation IDs" — plural.
// All three reached the service on the canonical envelope and none was ever
// written to the decision.
func TestResolve_EnvelopeFactsReachTheDecisionEvidence(t *testing.T) {
	f := defaultFixture()

	req := baseRequest
	req.SourceChannel = "api"
	req.WorkloadID = "workload-7"
	req.CausationID = "01HXXXCAUSATION"

	_, err := f.build().Resolve(context.Background(), req)
	require.NoError(t, err)

	require.Len(t, f.sessions.storedCtx, 1)
	for _, sc := range f.sessions.storedCtx {
		assert.Equal(t, "api", sc.SourceChannel)
		assert.Equal(t, "workload-7", sc.WorkloadID)
		assert.Equal(t, "01HXXXCAUSATION", sc.CausationID)

		// Named in §4, resolvable by nothing in the estate. Asserted nil so a
		// future placeholder cannot creep in unnoticed — "we never resolved
		// this" must stay distinct from "we resolved it to nothing".
		assert.Nil(t, sc.EntitlementContextRef)
	}
}

// ── Gap 3: break-glass reconciliation never ran ─────────────────────────────

// RunReconciler is the goroutine SupportService.Reconcile's comment has always
// claimed existed. It did not, so an expired unreviewed elevation was reported
// to nobody and the SupportContextsUnreviewed gauge was never set.
func TestRunReconciler_ReportsAcrossTenantsAndStopsOnCancel(t *testing.T) {
	f := newSupportFixture()

	past := time.Now().UTC().Add(-time.Hour)
	f.store.contexts["sc-a"] = &domain.SupportContext{
		SupportContextID: "sc-a", TenantID: "tenant-a",
		SupportPrincipalID: "p-1", ExpiresAt: past,
	}
	f.store.contexts["sc-b"] = &domain.SupportContext{
		SupportContextID: "sc-b", TenantID: "tenant-b",
		SupportPrincipalID: "p-2", ExpiresAt: past,
	}

	svc := f.build()

	// The cross-tenant sweep is the point: a per-tenant one could only ever
	// cover tenants somebody thought to ask about.
	pending, err := svc.ReconcileAll(context.Background(), 10)
	require.NoError(t, err)
	assert.Len(t, pending, 2, "the sweep must see every tenant, not just one")

	// And it must actually stop when the process shuts down.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { svc.RunReconciler(ctx, 10*time.Millisecond, 10); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunReconciler did not return on context cancellation")
	}
}

// Zero means deliberately disabled, which SUPPORT_REVIEW_INTERVAL_MINUTES has
// always documented. It must return rather than default to some interval,
// because a knob that silently ignores its own off switch is worse than none.
func TestRunReconciler_ZeroIntervalDisablesRatherThanDefaults(t *testing.T) {
	svc := newSupportFixture().build()

	done := make(chan struct{})
	go func() { svc.RunReconciler(context.Background(), 0, 10); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a zero interval must disable the reconciler, not start one")
	}
}

// A reviewed grant drops off the pending list — otherwise the report can only
// ever grow and a reviewer has no way to clear it.
func TestReconcileAll_ExcludesReviewedGrants(t *testing.T) {
	f := newSupportFixture()
	past := time.Now().UTC().Add(-time.Hour)
	reviewed := past.Add(time.Minute)
	reviewer := "p-auditor"

	f.store.contexts["sc-done"] = &domain.SupportContext{
		SupportContextID: "sc-done", TenantID: "tenant-a",
		ExpiresAt: past, ReviewedAt: &reviewed, ReviewedBy: &reviewer,
	}

	pending, err := f.build().ReconcileAll(context.Background(), 10)
	require.NoError(t, err)
	assert.Empty(t, pending)
}

var _ = errors.Is

// ── §4 cache state model: Fresh / Stale / Invalidated ───────────────────────

// The three states were all reachable and none was nameable. More importantly
// the INVALIDATED one did nothing: InvalidateTenantContext marks bindings
// invalid by writing the epoch to refreshed_at, the store comment claims the
// resolver revalidates on next use, and the ingress check never read the
// column — so an invalidated binding went on authorising. NP4 passed on paper.
func TestBindingFreshness_NamesAllThreeStates(t *testing.T) {
	now := time.Now().UTC()

	invalidated := &domain.TenantIngressBinding{RefreshedAt: time.Unix(0, 0).UTC()}
	if got := invalidated.Freshness(now, time.Hour); got != domain.CacheInvalidated {
		t.Fatalf("epoch refreshed_at must read as INVALIDATED, got %s", got)
	}

	stale := &domain.TenantIngressBinding{RefreshedAt: now.Add(-2 * time.Hour)}
	if got := stale.Freshness(now, time.Hour); got != domain.CacheStale {
		t.Fatalf("past-TTL binding must read as STALE, got %s", got)
	}

	fresh := &domain.TenantIngressBinding{RefreshedAt: now.Add(-time.Minute)}
	if got := fresh.Freshness(now, time.Hour); got != domain.CacheFresh {
		t.Fatalf("within-TTL binding must read as FRESH, got %s", got)
	}

	// No TTL configured means no staleness bound — a live binding must not be
	// classified stale by a control nobody switched on.
	if got := stale.Freshness(now, 0); got != domain.CacheFresh {
		t.Fatalf("with no TTL a non-invalidated binding must be FRESH, got %s", got)
	}

	// Invalidation does not depend on the TTL: it is a repudiation, not a clock.
	if got := invalidated.Freshness(now, 0); got != domain.CacheInvalidated {
		t.Fatalf("invalidation must hold with no TTL, got %s", got)
	}

	// A nil binding is not authority.
	var missing *domain.TenantIngressBinding
	if got := missing.Freshness(now, time.Hour); got != domain.CacheInvalidated {
		t.Fatalf("a nil binding must not read as usable, got %s", got)
	}
}

// A cross-tenant mismatch must refuse whatever state the cache entry is in.
//
// This pins a regression introduced while adding the state model and caught by
// the existing ingress suite: handling freshness BEFORE the tenant comparison
// let an invalidated binding return early under observe policy, so a request
// arriving on tenant A's hostname bearing tenant B's token was PERMITTED. A
// stale or repudiated binding is still evidence of a routing problem; only the
// order of the two checks decides whether that evidence is acted on.
func TestIngress_MismatchRefusesRegardlessOfCacheState(t *testing.T) {
	for _, tc := range []struct {
		name        string
		refreshedAt time.Time
	}{
		{"fresh", time.Now().UTC()},
		{"stale", time.Now().UTC().Add(-72 * time.Hour)},
		{"invalidated", time.Unix(0, 0).UTC()},
	} {
		for _, policy := range []identityctx.IngressPolicy{
			identityctx.IngressPolicyStrict, identityctx.IngressPolicyObserve,
		} {
			t.Run(tc.name+"/"+string(policy), func(t *testing.T) {
				bindings := &fakeBindings{byIdentifier: map[string]*domain.TenantIngressBinding{
					"tenant-a.zoiko.io": {
						IngressIdentifier: "tenant-a.zoiko.io",
						TenantID:          "tenant-a",
						Environment:       domain.EnvironmentProduction,
						ActiveFlag:        true,
						RefreshedAt:       tc.refreshedAt,
					},
				}}
				checker := identityctx.NewIngressChecker(bindings, policy, zap.NewNop()).
					WithBindingTTL(time.Hour)

				// Arrives on tenant-a's hostname bearing a tenant-b token.
				err := checker.Check(context.Background(), "tenant-a.zoiko.io", "tenant-b")

				assert.ErrorIs(t, err, domain.ErrIngressTenantMismatch,
					"a cross-tenant mismatch must refuse under every policy and every cache state")
			})
		}
	}
}

// An INVALIDATED binding must stop conferring authority — the half of negative
// path #4 that was never implemented. InvalidateTenantContext writes the epoch
// to refreshed_at, the store comment claimed the resolver revalidates on next
// use, and nothing read the column at all.
func TestIngress_InvalidatedBindingIsNotUsableAsAuthority(t *testing.T) {
	newChecker := func(policy identityctx.IngressPolicy) *identityctx.IngressChecker {
		bindings := &fakeBindings{byIdentifier: map[string]*domain.TenantIngressBinding{
			"tenant-a.zoiko.io": {
				IngressIdentifier: "tenant-a.zoiko.io",
				TenantID:          "tenant-a",
				Environment:       domain.EnvironmentProduction,
				ActiveFlag:        true,
				RefreshedAt:       time.Unix(0, 0).UTC(), // the invalidation marker
			},
		}}
		return identityctx.NewIngressChecker(bindings, policy, zap.NewNop())
	}

	// Under strict, an invalidated binding is no binding: refused, exactly as
	// an unknown identifier is.
	err := newChecker(identityctx.IngressPolicyStrict).
		Check(context.Background(), "tenant-a.zoiko.io", "tenant-a")
	assert.ErrorIs(t, err, domain.ErrIngressTenantMismatch,
		"strict policy must refuse an invalidated binding")

	// Under observe it is permitted, consistent with how an unbound identifier
	// is treated — and it still confers nothing, because the tenant comes from
	// the verified token either way.
	err = newChecker(identityctx.IngressPolicyObserve).
		Check(context.Background(), "tenant-a.zoiko.io", "tenant-a")
	assert.NoError(t, err, "observe policy permits, as it does for an unbound ingress")
}

// ── §4: "Read-only; scoped; correlation required" on GOV-01 queries ─────────

// Scoped was enforced by requireTenant/requirePrincipal. Correlation was
// enforced by nothing: the envelope middleware parses and reports it, but its
// default write-strict mode refuses material writes and ADMITS reads, so a
// bare GET with no correlation produced a governance decision that nothing
// could later be traced to.
//
// The console is unaffected — its server-side hop goes through apiGet, which
// spreads envelopeHeaders and always sets the header.
func TestGOV01Queries_RefuseWithoutCorrelation(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
	}{
		{"GetEffectiveContext", "/v1/context/session/sess-1"},
		{"ExplainContextResolution", "/v1/context/session/sess-1/explain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("X-Tenant-Id", "tenant-a")
			req.Header.Set("X-Principal-Id", "p-1")
			// No X-Correlation-ID.

			w := httptest.NewRecorder()
			newGov01Harness(t, &permittingAuthz{}).router.ServeHTTP(w, req)

			assert.Equal(t, http.StatusBadRequest, w.Code,
				"a GOV-01 query with no correlation must be refused, not served")
			assert.Contains(t, w.Body.String(), "X-Correlation-ID")
		})
	}
}

// The same two routes get past the correlation guard when one is supplied —
// so the guard refuses the absence, not the route.
func TestGOV01Queries_ProceedWithCorrelation(t *testing.T) {
	for _, path := range []string{
		"/v1/context/session/sess-1",
		"/v1/context/session/sess-1/explain",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Tenant-Id", "tenant-a")
		req.Header.Set("X-Principal-Id", "p-1")
		req.Header.Set("X-Correlation-ID", "corr-1")

		w := httptest.NewRecorder()
		newGov01Harness(t, &permittingAuthz{}).router.ServeHTTP(w, req)

		assert.NotEqual(t, http.StatusBadRequest, w.Code,
			path+" must get past the correlation guard when one is supplied")
	}
}

// ── §4 evidence/lineage: "policy/version references" ────────────────────────

// The decision recorded a residency policy id and a schema version, neither of
// which says which revision of the ROUTING truth it was decided against. The
// ingress binding carries exactly that in source_version; it was read on every
// resolution and thrown away, so a decision could be replayed but not
// reproduced.
func TestResolve_RecordsTheIngressBindingVersionItDecidedAgainst(t *testing.T) {
	f := defaultFixture()

	bindings := &fakeBindings{byIdentifier: map[string]*domain.TenantIngressBinding{
		"tenant-a.zoiko.io": {
			IngressIdentifier: "tenant-a.zoiko.io",
			TenantID:          activePrincipal.TenantID,
			Environment:       domain.EnvironmentProduction,
			ActiveFlag:        true,
			SourceVersion:     "registry-v42",
			RefreshedAt:       time.Now().UTC(),
		},
	}}

	req := baseRequest
	req.IngressSource = "tenant-a.zoiko.io"

	resolver := f.build().WithIngressChecker(
		identityctx.NewIngressChecker(bindings, identityctx.IngressPolicyObserve, zap.NewNop()))

	_, err := resolver.Resolve(context.Background(), req)
	require.NoError(t, err)

	require.Len(t, f.sessions.storedCtx, 1)
	for _, sc := range f.sessions.storedCtx {
		assert.Equal(t, "registry-v42", sc.IngressBindingVersion,
			"the decision must record which revision of the routing truth it was made against")
	}
}

// No binding, no version — recorded as absent rather than as an empty claim.
func TestResolve_NoIngressBindingRecordsNoVersion(t *testing.T) {
	f := defaultFixture()

	_, err := f.build().Resolve(context.Background(), baseRequest)
	require.NoError(t, err)

	require.Len(t, f.sessions.storedCtx, 1)
	for _, sc := range f.sessions.storedCtx {
		assert.Empty(t, sc.IngressBindingVersion)
	}
}
