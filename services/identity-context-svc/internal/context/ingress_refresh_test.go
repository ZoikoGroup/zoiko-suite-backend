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

type fakeRegistry struct {
	tenant, lifecycle string
	found             bool
	err               error
	calls             int
}

func (f *fakeRegistry) ResolveHost(_ context.Context, _ string) (string, string, bool, error) {
	f.calls++
	return f.tenant, f.lifecycle, f.found, f.err
}

// storeBindings is a fakeBindings that also accepts the refreshed write, as the Postgres store does.
type storeBindings struct {
	fakeBindings
	writes []domain.TenantIngressBinding
}

func (s *storeBindings) UpsertIngressBinding(_ context.Context, b domain.TenantIngressBinding) error {
	s.writes = append(s.writes, b)
	if !b.ActiveFlag {
		delete(s.byIdentifier, b.IngressIdentifier)
		return nil
	}
	s.byIdentifier[b.IngressIdentifier] = &b
	return nil
}

func newStore(initial map[string]*domain.TenantIngressBinding) *storeBindings {
	if initial == nil {
		initial = map[string]*domain.TenantIngressBinding{}
	}
	return &storeBindings{fakeBindings: fakeBindings{byIdentifier: initial}}
}

func refreshing(b *storeBindings, reg *fakeRegistry, policy identityctx.IngressPolicy) *identityctx.IngressChecker {
	return identityctx.NewIngressChecker(b, policy, zap.NewNop()).
		WithBindingTTL(time.Hour).
		WithRefresh(reg, b, domain.EnvironmentProduction, time.Minute, 100)
}

func staleBinding(tenant string) *domain.TenantIngressBinding {
	b := boundTo(tenant)
	b.RefreshedAt = time.Now().Add(-48 * time.Hour)
	return b
}

func invalidatedBinding(tenant string) *domain.TenantIngressBinding {
	b := boundTo(tenant)
	b.RefreshedAt = time.Unix(0, 0)
	return b
}

// S1-B3: a host the registry binds is written FRESH on first use, so strict policy no longer refuses a provisioned
// host whose binding was never seeded.
func TestIngressRefresh_AMissingBindingIsLearnedFromTheRegistry(t *testing.T) {
	b := newStore(nil)
	reg := &fakeRegistry{tenant: "tenant-a", lifecycle: "ACTIVE", found: true}
	c := refreshing(b, reg, identityctx.IngressPolicyStrict)

	dec, err := c.Evaluate(context.Background(), "tenant-a.zoiko.io", "tenant-a")
	require.NoError(t, err)
	assert.Equal(t, domain.CacheFresh, dec.Freshness)
	assert.Contains(t, dec.SourceVersion, "registry:resolve-tenant:ACTIVE@")
	require.Len(t, b.writes, 1)
	assert.Equal(t, domain.EnvironmentProduction, b.writes[0].Environment)

	// Fresh now: the next request does not ask the registry again.
	_, err = c.Evaluate(context.Background(), "tenant-a.zoiko.io", "tenant-a")
	require.NoError(t, err)
	assert.Equal(t, 1, reg.calls)
}

// The refresh confirms or corrects what the registry says; it never lets a token pick its own tenant.
func TestIngressRefresh_NeverSuppliesATenantTheTokenDidNotClaim(t *testing.T) {
	b := newStore(nil)
	reg := &fakeRegistry{tenant: "tenant-a", lifecycle: "ACTIVE", found: true}
	err := refreshing(b, reg, identityctx.IngressPolicyObserve).Check(context.Background(), "tenant-a.zoiko.io", "tenant-b")
	require.ErrorIs(t, err, domain.ErrIngressTenantMismatch, "the registry binds the host to tenant-a: a tenant-b token is refused")
}

func TestIngressRefresh_AStaleOrInvalidatedBindingIsRevalidatedAndCorrected(t *testing.T) {
	// INVALIDATED, same tenant: usable again once the registry confirms it.
	b := newStore(map[string]*domain.TenantIngressBinding{"h.zoiko.io": invalidatedBinding("tenant-a")})
	reg := &fakeRegistry{tenant: "tenant-a", lifecycle: "ACTIVE", found: true}
	dec, err := refreshing(b, reg, identityctx.IngressPolicyStrict).Evaluate(context.Background(), "h.zoiko.io", "tenant-a")
	require.NoError(t, err)
	assert.Equal(t, domain.CacheFresh, dec.Freshness, "an invalidated binding the registry confirms is fresh again")

	// STALE and the registry now binds the host elsewhere: corrected, and the old tenant token is refused.
	b = newStore(map[string]*domain.TenantIngressBinding{"h.zoiko.io": staleBinding("tenant-a")})
	reg = &fakeRegistry{tenant: "tenant-b", lifecycle: "ACTIVE", found: true}
	err = refreshing(b, reg, identityctx.IngressPolicyObserve).Check(context.Background(), "h.zoiko.io", "tenant-a")
	require.ErrorIs(t, err, domain.ErrIngressTenantMismatch)
	assert.Equal(t, "tenant-b", b.byIdentifier["h.zoiko.io"].TenantID)
}

func TestIngressRefresh_ARegistryThatNoLongerBindsTheHostDeactivatesTheBinding(t *testing.T) {
	b := newStore(map[string]*domain.TenantIngressBinding{"h.zoiko.io": staleBinding("tenant-a")})
	reg := &fakeRegistry{found: false}
	err := refreshing(b, reg, identityctx.IngressPolicyStrict).Check(context.Background(), "h.zoiko.io", "tenant-a")
	require.ErrorIs(t, err, domain.ErrIngressTenantMismatch, "removed by the registry: refused under strict")
	require.Len(t, b.writes, 1)
	assert.False(t, b.writes[0].ActiveFlag)
}

// A refresh that fails never widens access: the old rules stand.
func TestIngressRefresh_ARegistryOutageChangesNothing(t *testing.T) {
	reg := &fakeRegistry{err: errors.New("registry down")}
	stale := newStore(map[string]*domain.TenantIngressBinding{"h.zoiko.io": staleBinding("tenant-a")})
	dec, err := refreshing(stale, reg, identityctx.IngressPolicyStrict).Evaluate(context.Background(), "h.zoiko.io", "tenant-a")
	require.NoError(t, err, "a STALE binding is still admitted within policy")
	assert.Equal(t, domain.CacheStale, dec.Freshness)

	inv := newStore(map[string]*domain.TenantIngressBinding{"h.zoiko.io": invalidatedBinding("tenant-a")})
	require.ErrorIs(t, refreshing(inv, reg, identityctx.IngressPolicyStrict).Check(context.Background(), "h.zoiko.io", "tenant-a"),
		domain.ErrIngressTenantMismatch, "an INVALIDATED binding stays unusable when it cannot be revalidated")

	none := newStore(nil)
	require.ErrorIs(t, refreshing(none, reg, identityctx.IngressPolicyStrict).Check(context.Background(), "new.zoiko.io", "tenant-a"),
		domain.ErrIngressTenantMismatch, "an unknown host stays refused when the registry cannot answer")
	assert.Empty(t, stale.writes)
}

func TestIngressRefresh_IsThrottledPerIdentifierAndOverall(t *testing.T) {
	b := newStore(nil)
	reg := &fakeRegistry{found: false}
	c := identityctx.NewIngressChecker(b, identityctx.IngressPolicyObserve, zap.NewNop()).
		WithRefresh(reg, b, domain.EnvironmentProduction, time.Hour, 3)
	for i := 0; i < 5; i++ {
		_ = c.Check(context.Background(), "same.zoiko.io", "t")
	}
	assert.Equal(t, 1, reg.calls, "one identifier is re-read at most once per interval")
	for _, h := range []string{"a.zoiko.io", "b.zoiko.io", "c.zoiko.io", "d.zoiko.io", "e.zoiko.io"} {
		_ = c.Check(context.Background(), h, "t")
	}
	assert.LessOrEqual(t, reg.calls, 3, "made-up hosts cannot become a flood of registry reads")
}

func TestIngressRefresh_OffUnlessFullyConfigured(t *testing.T) {
	b := newStore(nil)
	reg := &fakeRegistry{tenant: "tenant-a", found: true}
	c := identityctx.NewIngressChecker(b, identityctx.IngressPolicyStrict, zap.NewNop()).
		WithRefresh(reg, b, domain.Environment("bogus"), time.Minute, 5).
		WithRefresh(reg, b, domain.EnvironmentProduction, 0, 5)
	require.ErrorIs(t, c.Check(context.Background(), "tenant-a.zoiko.io", "tenant-a"), domain.ErrIngressTenantMismatch)
	assert.Zero(t, reg.calls)
}

// A FRESH binding is trusted as it is: the registry is not asked, whatever the throttle says.
func TestIngressRefresh_AFreshBindingIsNotReRead(t *testing.T) {
	fresh := boundTo("tenant-a")
	fresh.RefreshedAt = time.Now()
	b := newStore(map[string]*domain.TenantIngressBinding{"h.zoiko.io": fresh})
	reg := &fakeRegistry{tenant: "tenant-b", found: true}
	require.NoError(t, refreshing(b, reg, identityctx.IngressPolicyStrict).Check(context.Background(), "h.zoiko.io", "tenant-a"))
	assert.Zero(t, reg.calls)
	assert.Empty(t, b.writes)
}
