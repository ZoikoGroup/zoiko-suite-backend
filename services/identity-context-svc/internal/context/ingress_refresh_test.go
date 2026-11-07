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

type fakeBindingStore struct {
	tenants     []string
	upserted    []domain.TenantIngressBinding
	deactivated map[string][]string
}

func (f *fakeBindingStore) ListIngressBindingTenants(context.Context) ([]string, error) {
	return f.tenants, nil
}
func (f *fakeBindingStore) UpsertIngressBinding(_ context.Context, b domain.TenantIngressBinding) error {
	f.upserted = append(f.upserted, b)
	return nil
}
func (f *fakeBindingStore) DeactivateIngressBindingsExcept(_ context.Context, tenantID string, keep []string) (int, error) {
	if f.deactivated == nil {
		f.deactivated = map[string][]string{}
	}
	f.deactivated[tenantID] = keep
	return 0, nil
}

type fakeHostRegistry map[string][]identityctx.HostBinding

func (f fakeHostRegistry) ListTenantHostBindings(_ context.Context, tenantID string) ([]identityctx.HostBinding, error) {
	b, ok := f[tenantID]
	if !ok {
		return nil, errors.New("registry unavailable")
	}
	return b, nil
}

// TestIngressBindingRefreshedBeforeTTL: the refresher runs at TTL/2 and
// rewrites every bound tenant's bindings from the registry, so none reaches
// its TTL while the registry answers.
func TestIngressBindingRefreshedBeforeTTL(t *testing.T) {
	ttl := 24 * time.Hour
	assert.Equal(t, 12*time.Hour, identityctx.RefreshInterval(ttl), "refresh at half the TTL")

	st := &fakeBindingStore{tenants: []string{"t-a", "t-b"}}
	reg := fakeHostRegistry{
		"t-a": {{HostBindingID: "hb-1", Hostname: "Acme.Zoiko.io", TenantID: "t-a", ActiveFlag: true}},
		// t-b: the registry is down for this tenant.
	}
	n, err := identityctx.NewIngressRefresher(st, reg, domain.EnvironmentLocal, ttl, zap.NewNop()).RefreshOnce(context.Background())

	require.Error(t, err, "a tenant the registry could not answer for is reported")
	assert.Equal(t, 1, n)
	require.Len(t, st.upserted, 1)
	assert.Equal(t, domain.TenantIngressBinding{IngressIdentifier: "acme.zoiko.io", TenantID: "t-a",
		Environment: domain.EnvironmentLocal, ActiveFlag: true, SourceVersion: "registry:hb-1"}, st.upserted[0])
	assert.Equal(t, []string{"acme.zoiko.io"}, st.deactivated["t-a"], "hosts the registry dropped are deactivated")
	_, touched := st.deactivated["t-b"]
	assert.False(t, touched, "an unreachable registry never deactivates a tenant's bindings")
}

func TestIngressRefreshNeverBindsAHostToAnotherTenant(t *testing.T) {
	st := &fakeBindingStore{tenants: []string{"t-a"}}
	reg := fakeHostRegistry{"t-a": {{HostBindingID: "hb-9", Hostname: "evil.example", TenantID: "t-b", ActiveFlag: true}}}
	_, err := identityctx.NewIngressRefresher(st, reg, domain.EnvironmentLocal, time.Hour, zap.NewNop()).RefreshOnce(context.Background())
	require.NoError(t, err)
	assert.Empty(t, st.upserted)
}
