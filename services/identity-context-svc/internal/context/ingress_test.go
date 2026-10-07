package context_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	identityctx "zoiko.io/identity-context-svc/internal/context"
	"zoiko.io/identity-context-svc/internal/domain"
)

// GOV-01's minimum negative-path acceptance set, numbers 1 and 2:
//
//	1. Spoofed tenant header is ignored/rejected
//	2. Unknown hostname cannot fall back to another tenant
//
// Number 2 was previously not merely untested but UNTESTABLE: nothing in the
// service read a hostname, so the tenant came solely from the token claim and
// there was no fallback to attack. These tests exist because the ingress
// binding now does.

type fakeBindings struct {
	byIdentifier map[string]*domain.TenantIngressBinding
	err          error
	// lookups records what was asked for, so a test can assert the identifier
	// was normalised before the query rather than after.
	lookups []string
}

func (f *fakeBindings) FindIngressBinding(_ context.Context, identifier string) (*domain.TenantIngressBinding, error) {
	f.lookups = append(f.lookups, identifier)
	if f.err != nil {
		return nil, f.err
	}
	return f.byIdentifier[identifier], nil
}

func boundTo(tenantID string) *domain.TenantIngressBinding {
	return &domain.TenantIngressBinding{
		TenantID:    tenantID,
		Environment: domain.EnvironmentProduction,
		ActiveFlag:  true,
	}
}

// ── CanonicalIngress ─────────────────────────────────────────────────────────

func TestCanonicalIngress_PrefersExplicitEdgeHeader(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/context/resolve", nil)
	r.Host = "direct.example.com"
	r.Header.Set("X-Forwarded-Host", "proxy.example.com")
	r.Header.Set("X-Canonical-Ingress", "Edge.Example.COM")

	// A platform routing through a gateway knows its own ingress name better
	// than whatever Host survived the hop.
	assert.Equal(t, "edge.example.com", identityctx.CanonicalIngress(r))
}

func TestCanonicalIngress_TakesFirstForwardedHost(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/context/resolve", nil)
	r.Host = "internal.svc.cluster.local"
	// A proxy chain APPENDS, so the first entry is what the client asked for
	// and the last is whatever the nearest hop rewrote it to. Taking the last
	// would let an intermediate proxy choose the ingress identity.
	r.Header.Set("X-Forwarded-Host", "tenant-a.zoiko.io, edge-2.internal, edge-3.internal")

	assert.Equal(t, "tenant-a.zoiko.io", identityctx.CanonicalIngress(r))
}

func TestCanonicalIngress_StripsPort(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/context/resolve", nil)
	r.Host = "tenant-a.zoiko.io:8443"

	// api.example.com and api.example.com:443 are the same ingress. A binding
	// table that had to enumerate ports would miss on the first deployment
	// that changed one.
	assert.Equal(t, "tenant-a.zoiko.io", identityctx.CanonicalIngress(r))
}

func TestCanonicalIngress_NoHostIsUnknownNotEmpty(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/context/resolve", nil)
	r.Host = ""

	// A literal UNKNOWN rather than "", for the same reason risk_signal_source
	// records UNAVAILABLE: "we did not observe one" and "we forgot to record
	// one" must not look identical in the evidence.
	assert.Equal(t, domain.IngressUnknown, identityctx.CanonicalIngress(r))
}

// ── Negative path #2: unknown hostname cannot fall back ──────────────────────

func TestIngress_UnknownHostnameCannotFallBackToAnotherTenant(t *testing.T) {
	bindings := &fakeBindings{byIdentifier: map[string]*domain.TenantIngressBinding{
		"tenant-a.zoiko.io": boundTo("tenant-a"),
	}}
	checker := identityctx.NewIngressChecker(bindings, identityctx.IngressPolicyStrict, zap.NewNop())

	err := checker.Check(context.Background(), "attacker.example.com", "tenant-a")

	require.ErrorIs(t, err, domain.ErrIngressTenantMismatch,
		"an unbound hostname must be refused under strict policy, never defaulted to a tenant")
}

func TestIngress_UnknownHostnameIsPermittedUnderObserve(t *testing.T) {
	bindings := &fakeBindings{byIdentifier: map[string]*domain.TenantIngressBinding{}}
	checker := identityctx.NewIngressChecker(bindings, identityctx.IngressPolicyObserve, zap.NewNop())

	// Observe mode permits an identifier nobody has told us about. That grants
	// nothing on its own — the tenant still comes from the verified token —
	// and it is what lets the control ship before every environment has its
	// bindings seeded.
	require.NoError(t, checker.Check(context.Background(), "new-region.zoiko.io", "tenant-a"))
}

// ── Negative path #1: a forged host cannot cross a tenant boundary ───────────

func TestIngress_MismatchIsRefusedUnderEveryPolicy(t *testing.T) {
	bindings := &fakeBindings{byIdentifier: map[string]*domain.TenantIngressBinding{
		"tenant-a.zoiko.io": boundTo("tenant-a"),
	}}

	for _, policy := range []identityctx.IngressPolicy{
		identityctx.IngressPolicyStrict,
		identityctx.IngressPolicyObserve,
	} {
		t.Run(string(policy), func(t *testing.T) {
			checker := identityctx.NewIngressChecker(bindings, policy, zap.NewNop())

			// Tenant A's hostname, tenant B's token. There is no legitimate
			// request of this shape.
			err := checker.Check(context.Background(), "tenant-a.zoiko.io", "tenant-b")

			require.ErrorIs(t, err, domain.ErrIngressTenantMismatch)
		})
	}
}

func TestIngress_MatchingBindingIsPermitted(t *testing.T) {
	bindings := &fakeBindings{byIdentifier: map[string]*domain.TenantIngressBinding{
		"tenant-a.zoiko.io": boundTo("tenant-a"),
	}}
	checker := identityctx.NewIngressChecker(bindings, identityctx.IngressPolicyStrict, zap.NewNop())

	require.NoError(t, checker.Check(context.Background(), "tenant-a.zoiko.io", "tenant-a"))
}

// TestIngress_NeverSuppliesATenant is the invariant that keeps GOV-01 out of
// tenant master data.
//
// A binding can CONTRADICT a token's claim, which is a refusal. It must never
// SUPPLY a tenant the token did not claim — that would make a hostname
// sufficient to select a tenant, which is exactly the "no trusted client
// context" refinement the spec opens with.
//
// Asserted structurally: Check returns only an error, so there is no channel
// through which a tenant could be handed back. This test documents that the
// signature is the control.
func TestIngress_NeverSuppliesATenant(t *testing.T) {
	bindings := &fakeBindings{byIdentifier: map[string]*domain.TenantIngressBinding{
		"tenant-a.zoiko.io": boundTo("tenant-a"),
	}}
	checker := identityctx.NewIngressChecker(bindings, identityctx.IngressPolicyStrict, zap.NewNop())

	// An empty claimed tenant cannot be filled in from the binding; it is a
	// mismatch, because "" is not "tenant-a".
	err := checker.Check(context.Background(), "tenant-a.zoiko.io", "")
	require.ErrorIs(t, err, domain.ErrIngressTenantMismatch)
}

func TestIngress_LookupFailureFailsClosed(t *testing.T) {
	bindings := &fakeBindings{err: errors.New("postgres is gone")}
	checker := identityctx.NewIngressChecker(bindings, identityctx.IngressPolicyObserve, zap.NewNop())

	err := checker.Check(context.Background(), "tenant-a.zoiko.io", "tenant-a")

	// A binding table we cannot read is a control that did not run, and this
	// one exists to stop cross-tenant resolution.
	require.ErrorIs(t, err, identityctx.ErrUpstreamUnavailable)
}

func TestIngress_AbsentIdentifierIsRefusedOnlyUnderStrict(t *testing.T) {
	bindings := &fakeBindings{byIdentifier: map[string]*domain.TenantIngressBinding{}}

	strict := identityctx.NewIngressChecker(bindings, identityctx.IngressPolicyStrict, zap.NewNop())
	require.ErrorIs(t, strict.Check(context.Background(), domain.IngressUnknown, "tenant-a"),
		domain.ErrIngressTenantMismatch)

	observe := identityctx.NewIngressChecker(bindings, identityctx.IngressPolicyObserve, zap.NewNop())
	require.NoError(t, observe.Check(context.Background(), domain.IngressUnknown, "tenant-a"))
}

// ── Residency ────────────────────────────────────────────────────────────────

func TestResidency_DisabledWhenNoPoliciesConfigured(t *testing.T) {
	p := identityctx.NewResidencyPolicy("eu-west-1", nil, zap.NewNop())

	assert.False(t, p.Enforced())
	// An empty list disables enforcement rather than refusing everything.
	// Refusing every resolution until somebody populates a list would take the
	// platform down, and a control that does that on deployment gets reverted
	// rather than fixed.
	require.NoError(t, p.Check("residency-us", "entity-1"))
}

func TestResidency_RefusesUnservablePolicy(t *testing.T) {
	p := identityctx.NewResidencyPolicy("eu-west-1", []string{"residency-eu"}, zap.NewNop())

	require.NoError(t, p.Check("residency-eu", "entity-1"))
	require.ErrorIs(t, p.Check("residency-us", "entity-2"), domain.ErrResidencyDenied,
		"a policy this region may not serve must refuse, not merely be recorded")
}

// TestResidency_MissingPolicyIsPermittedButLogged separates a registry problem
// from a residency violation.
//
// The registry treats data_residency_policy_id as mandatory, so an empty one
// means the registry did not return it. Refusing every login over that would
// be the wrong response to the wrong diagnosis.
func TestResidency_MissingPolicyIsPermitted(t *testing.T) {
	p := identityctx.NewResidencyPolicy("eu-west-1", []string{"residency-eu"}, zap.NewNop())

	require.NoError(t, p.Check("", "entity-3"))
}
