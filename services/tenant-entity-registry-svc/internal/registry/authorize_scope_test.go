package registry_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/authz"
	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

const testEntityID = "22222222-2222-2222-2222-222222222222"

// TestAuthorizeScope_EnvelopeEntityBeatsTheTenant pins the fix: the entity
// position must carry the envelope's legal entity (X-Legal-Entity-Id), not
// the tenant id. authorization-svc resolves the entity's owning tenant and
// replies 403 tenant_scope_mismatch to a tenant id, which this client turns
// into ErrServiceUnavailable — every guarded command answered 503.
func TestAuthorizeScope_EnvelopeEntityBeatsTheTenant(t *testing.T) {
	rec := &recordingAuthZ{err: authz.ErrUnauthorized}
	svc := newSvc(t, newMemStore(), rec, acceptAllJurisd{})
	ctx := domain.WithLegalEntity(tenantCtx("tenant-1"), testEntityID)

	err := svc.TransitionTenantLifecycle(ctx, "tenant-1", domain.TransitionTenantLifecycleRequest{})

	require.ErrorIs(t, err, registry.ErrUnauthorized)
	assert.Equal(t, testEntityID, rec.scopeID)
	assert.Equal(t, testPrincipal, rec.principalID)
}

// TestAuthorizeScope_NoEntityFallsBackToThePlatformScope: a caller whose
// envelope names no entity is evaluated at the configured platform scope —
// the same scope ProvisionTenant uses — where only a platform-scope grant can
// answer. The tenant id is never a legal entity and must never travel in its
// position.
func TestAuthorizeScope_NoEntityFallsBackToThePlatformScope(t *testing.T) {
	rec := &recordingAuthZ{err: authz.ErrUnauthorized}
	svc := newSvc(t, newMemStore(), rec, acceptAllJurisd{})
	ctx := tenantCtx("tenant-1")

	err := svc.TransitionTenantLifecycle(ctx, "tenant-1", domain.TransitionTenantLifecycleRequest{})

	require.ErrorIs(t, err, registry.ErrUnauthorized)
	assert.Equal(t, testPlatformScope, rec.scopeID)
	assert.NotEqual(t, "tenant-1", rec.scopeID)
}

// TestAuthorizeScope_PlatformDecisionsIgnoreTheEnvelopeEntity: ProvisionTenant
// is a platform decision by §4.2 and must stay evaluated at the platform
// scope even when the operator's envelope carries an entity.
func TestAuthorizeScope_PlatformDecisionsIgnoreTheEnvelopeEntity(t *testing.T) {
	rec := &recordingAuthZ{err: authz.ErrUnauthorized}
	svc := newSvc(t, newMemStore(), rec, acceptAllJurisd{})
	ctx := domain.WithLegalEntity(tenantCtx("tenant-1"), testEntityID)

	_, err := svc.ProvisionTenant(ctx, domain.ProvisionTenantRequest{}, "corr")

	require.ErrorIs(t, err, registry.ErrUnauthorized)
	assert.Equal(t, testPlatformScope, rec.scopeID)
}
