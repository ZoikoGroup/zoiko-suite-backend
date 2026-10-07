package context_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	identityctx "zoiko.io/identity-context-svc/internal/context"
	"zoiko.io/identity-context-svc/internal/domain"
)

// Items 7–10 of the 2026-09-23 GOV-01 audit, closed 2026-09-28.
//
// §4 lists source channel and workload identity as SERVER-resolved. Both arrive
// as client headers the edge does not strip, and 000008 recorded them verbatim.
// They are now resolved against the verified principal's type, and a
// contradicted assertion is not recorded.

func principalOfType(t domain.PrincipalType) *domain.Principal {
	p := *activePrincipal
	p.PrincipalType = t
	return &p
}

// resolveOne runs a resolution for a principal of the given type and returns
// the one session row it recorded.
func resolveOne(t *testing.T, pt domain.PrincipalType, channel, workload string) domain.SessionContext {
	t.Helper()
	f := defaultFixture()
	f.principals = &mockPrincipalStore{principal: principalOfType(pt)}
	req := baseRequest
	req.SourceChannel = channel
	req.WorkloadID = workload

	_, err := f.build().Resolve(context.Background(), req)
	require.NoError(t, err, "a contradicted source input must never refuse the resolution")
	require.Len(t, f.sessions.storedCtx, 1)
	for _, sc := range f.sessions.storedCtx {
		return *sc
	}
	panic("unreachable")
}

func TestResolve_SourceInputsAreResolvedNotCopied(t *testing.T) {
	const svc = domain.PrincipalTypeServiceAccount
	const api = domain.PrincipalTypeAPIClient
	const human = domain.PrincipalTypeHuman

	for _, tc := range []struct {
		name              string
		pt                domain.PrincipalType
		channel, workload string
		wantChannel       string
		wantChannelBasis  domain.SourceInputBasis
		wantWorkload      string
		wantWorkloadBasis domain.SourceInputBasis
	}{
		// The console: a human on the web. Unchanged in effect.
		{"human web", human, "web", "", "web", domain.BasisAssertedConsistent, "", domain.BasisNotPresented},
		// THE spoof: a human claiming to be a scheduled job. Not recorded.
		{"human claims scheduled_job", human, "scheduled_job", "", "", domain.BasisRejectedInconsistent, "", domain.BasisNotPresented},
		{"human claims system", human, "system", "", "", domain.BasisRejectedInconsistent, "", domain.BasisNotPresented},
		// Nothing to derive a human's channel from — web and mobile are equally
		// likely, so nothing is guessed.
		{"human no channel", human, "", "", "", domain.BasisNotPresented, "", domain.BasisNotPresented},
		// A workload id on a human session cannot be attested here.
		{"human asserts workload", human, "web", "bff-7", "web", domain.BasisAssertedConsistent, "", domain.BasisUnverifiableDiscarded},

		// A service account IS its workload; the server derives both.
		{"service no headers", svc, "", "", "system", domain.BasisServerDerived, activePrincipal.PrincipalID, domain.BasisVerifiedPrincipal},
		{"service scheduled_job", svc, "scheduled_job", "", "scheduled_job", domain.BasisAssertedConsistent, activePrincipal.PrincipalID, domain.BasisVerifiedPrincipal},
		{"service claims web", svc, "web", "", "", domain.BasisRejectedInconsistent, activePrincipal.PrincipalID, domain.BasisVerifiedPrincipal},
		// Naming ANOTHER workload is a claim to be someone else: the verified
		// id is recorded instead, and the basis records the contradiction.
		{"service names other workload", svc, "system", "someone-else", "system", domain.BasisServerDerived, activePrincipal.PrincipalID, domain.BasisRejectedInconsistent},
		{"service names itself", svc, "", activePrincipal.PrincipalID, "system", domain.BasisServerDerived, activePrincipal.PrincipalID, domain.BasisVerifiedPrincipal},

		{"api client no headers", api, "", "", "api", domain.BasisServerDerived, activePrincipal.PrincipalID, domain.BasisVerifiedPrincipal},
		{"api client integration", api, "integration", "", "integration", domain.BasisAssertedConsistent, activePrincipal.PrincipalID, domain.BasisVerifiedPrincipal},
		{"api client claims mobile", api, "mobile", "", "", domain.BasisRejectedInconsistent, activePrincipal.PrincipalID, domain.BasisVerifiedPrincipal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := resolveOne(t, tc.pt, tc.channel, tc.workload)
			assert.Equal(t, tc.wantChannel, sc.SourceChannel)
			assert.Equal(t, tc.wantChannelBasis, sc.SourceChannelBasis)
			assert.Equal(t, tc.wantWorkload, sc.WorkloadID)
			assert.Equal(t, tc.wantWorkloadBasis, sc.WorkloadIDBasis)
		})
	}
}

// ── Item 7: entitlement context reference ────────────────────────────────────

type fakeEntitlements struct {
	ref *string
	err error
}

func (f fakeEntitlements) ResolveEntitlementContext(context.Context, string, string) (*string, error) {
	return f.ref, f.err
}

func TestResolve_EntitlementContextOutcomeIsRecorded(t *testing.T) {
	ref := "ent-snapshot-9"
	for _, tc := range []struct {
		name       string
		resolver   identityctx.EntitlementResolver
		wantRef    *string
		wantStatus domain.EntitlementContextStatus
	}{
		// Every deployment today: COM-03 does not exist.
		{"not configured", nil, nil, domain.EntitlementUpstreamNotConfigured},
		{"resolved", fakeEntitlements{ref: &ref}, &ref, domain.EntitlementResolved},
		// Authentication must not depend on the commercial plane being up.
		{"upstream down", fakeEntitlements{err: errors.New("503")}, nil, domain.EntitlementUpstreamUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := defaultFixture()
			r := f.build()
			if tc.resolver != nil {
				r = r.WithEntitlementResolver(tc.resolver)
			}
			_, err := r.Resolve(context.Background(), baseRequest)
			require.NoError(t, err, "an entitlement outcome must never refuse the resolution")

			require.Len(t, f.sessions.storedCtx, 1)
			for _, sc := range f.sessions.storedCtx {
				assert.Equal(t, tc.wantRef, sc.EntitlementContextRef)
				assert.Equal(t, tc.wantStatus, sc.EntitlementContextStatus)
			}
		})
	}
}

// ── Item 10: cache state on the decision ─────────────────────────────────────

func resolveViaIngress(t *testing.T, policy identityctx.IngressPolicy, refreshedAt time.Time) domain.SessionContext {
	t.Helper()
	f := defaultFixture()
	bindings := &fakeBindings{byIdentifier: map[string]*domain.TenantIngressBinding{
		"tenant-a.zoiko.io": {
			IngressIdentifier: "tenant-a.zoiko.io",
			TenantID:          activePrincipal.TenantID,
			Environment:       domain.EnvironmentProduction,
			ActiveFlag:        true,
			SourceVersion:     "registry-v42",
			RefreshedAt:       refreshedAt,
		},
	}}
	req := baseRequest
	req.IngressSource = "tenant-a.zoiko.io"
	r := f.build().WithIngressChecker(
		identityctx.NewIngressChecker(bindings, policy, zap.NewNop()).WithBindingTTL(time.Hour))

	_, err := r.Resolve(context.Background(), req)
	require.NoError(t, err)
	require.Len(t, f.sessions.storedCtx, 1)
	for _, sc := range f.sessions.storedCtx {
		return *sc
	}
	panic("unreachable")
}

func TestResolve_IngressCacheStateIsRecordedOnTheDecision(t *testing.T) {
	fresh := resolveViaIngress(t, identityctx.IngressPolicyStrict, time.Now().UTC())
	assert.Equal(t, domain.CacheFresh, fresh.IngressCacheState)
	assert.Equal(t, "registry-v42", fresh.IngressBindingVersion)

	// Admitted — §4 allows bounded-TTL reads for non-material queries — but the
	// decision now says it was made against a stale routing fact.
	stale := resolveViaIngress(t, identityctx.IngressPolicyStrict, time.Now().UTC().Add(-3*time.Hour))
	assert.Equal(t, domain.CacheStale, stale.IngressCacheState)

	// Under observe an invalidated binding is admitted but confirms nothing:
	// the state is recorded, the version is not.
	inv := resolveViaIngress(t, identityctx.IngressPolicyObserve, time.Unix(0, 0).UTC())
	assert.Equal(t, domain.CacheInvalidated, inv.IngressCacheState)
	assert.Empty(t, inv.IngressBindingVersion, "an invalidated binding must not lend its version to the decision")
}

func TestResolve_NoIngressBindingRecordsNoCacheState(t *testing.T) {
	f := defaultFixture()
	_, err := f.build().Resolve(context.Background(), baseRequest)
	require.NoError(t, err)
	for _, sc := range f.sessions.storedCtx {
		assert.Empty(t, sc.IngressCacheState)
	}
}

// ── Explain shows what the decision recorded ─────────────────────────────────

// These inputs were written from migration 000008 and never read back, so the
// explain endpoint could not show them.
func TestExplain_ReportsSourceInputsWithTheirBasis(t *testing.T) {
	sc := resolveOne(t, domain.PrincipalTypeServiceAccount, "", "")
	sc.IngressCacheState = domain.CacheStale
	sc.IngressBindingVersion = "registry-v42"
	sc.CausationID = "cause-1"

	ex := identityctx.Explain(&sc, time.Now())

	si := ex.SourceInputs
	assert.Equal(t, "system", si.SourceChannel)
	assert.Equal(t, domain.BasisServerDerived, si.SourceChannelBasis)
	assert.Equal(t, activePrincipal.PrincipalID, si.WorkloadID)
	assert.Equal(t, domain.BasisVerifiedPrincipal, si.WorkloadIDBasis)
	assert.Equal(t, "cause-1", si.CausationID)
	assert.Equal(t, "registry-v42", si.IngressBindingVersion)
	assert.Equal(t, domain.CacheStale, si.IngressCacheState)
	assert.Equal(t, domain.EntitlementUpstreamNotConfigured, si.EntitlementContextStatus)
}
