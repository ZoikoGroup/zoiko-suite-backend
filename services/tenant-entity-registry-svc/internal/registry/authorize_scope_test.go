package registry_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/authz"
	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/envelope"
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

// ── F-2 (19 Oct 2026): entity commands are evaluated at the target entity ──

const (
	callerEntityID = "e1000000-0000-4000-8000-000000000001" // on the caller's envelope
	targetEntityID = "e1000000-0000-4000-8000-000000000002" // the entity a command acts on
	parentEntityID = "e1000000-0000-4000-8000-000000000003"
)

// scopesAuthZ records every scope it is asked about and grants only the
// scopes listed in allow (nil grants everything).
type scopesAuthZ struct {
	scopes []string
	allow  map[string]bool
}

func (a *scopesAuthZ) Authorize(_ context.Context, _, scopeID, _, _ string) error {
	a.scopes = append(a.scopes, scopeID)
	if a.allow != nil && !a.allow[scopeID] {
		return authz.ErrUnauthorized
	}
	return nil
}

func callerAt(entityID string) context.Context {
	return domain.WithLegalEntity(tenantCtx("ten-001"), entityID)
}

// A grant at the caller's own entity must not reach another entity: this is
// the defect, in its original shape.
func TestAuthorizeScope_EntityCommandsAskAboutTheTarget(t *testing.T) {
	az := &scopesAuthZ{allow: map[string]bool{callerEntityID: true}}
	ms := newMemStore()
	seedEntity(ms, targetEntityID)
	seedEntity(ms, callerEntityID)
	svc := newSvc(t, ms, az, acceptAllJurisd{})
	name := "Renamed"

	_, err := svc.UpdateEntity(callerAt(callerEntityID), targetEntityID, domain.UpdateEntityRequest{TradingName: &name})
	require.ErrorIs(t, err, registry.ErrUnauthorized, "a grant at E1 must not update E2")
	assert.Equal(t, []string{targetEntityID}, az.scopes)

	az.scopes = nil
	err = svc.TransitionEntityStatus(callerAt(callerEntityID), targetEntityID, domain.TransitionEntityStatusRequest{NewStatus: domain.EntityStatusSuspended})
	require.ErrorIs(t, err, registry.ErrUnauthorized, "a grant at E1 must not deactivate E2")
	assert.Equal(t, []string{targetEntityID}, az.scopes)

	// The same caller acting on its own entity is permitted.
	az.scopes = nil
	_, err = svc.UpdateEntity(callerAt(callerEntityID), callerEntityID, domain.UpdateEntityRequest{TradingName: &name})
	require.NoError(t, err)
	assert.Equal(t, []string{callerEntityID}, az.scopes)
}

// The sensitive-identifier gate asks about the bundle's entity.
func TestAuthorizeScope_SensitiveBundleReadAsksAboutTheBundlesEntity(t *testing.T) {
	az := &scopesAuthZ{allow: map[string]bool{callerEntityID: true}}
	ms := newMemStore()
	ms.bundles["b-target"] = &domain.TaxIdentityBundle{TaxIdentityBundleID: "b-target", TenantID: "ten-001",
		LegalEntityID: targetEntityID, DataClassification: "RESTRICTED"}
	svc := newSvc(t, ms, az, acceptAllJurisd{})

	ctx := envelope.WithEnvelope(callerAt(callerEntityID), envelope.Envelope{PurposeContext: "statutory-filing"})
	_, err := svc.GetTaxIdentityBundle(ctx, "b-target")
	require.ErrorIs(t, err, registry.ErrUnauthorized)
	assert.Equal(t, []string{targetEntityID}, az.scopes)
}

// A hierarchy end-date changes both entities, so both are asked about; an id
// that does not exist is asked about at the caller's scope, as before, so the
// answer reveals nothing about which ids exist.
func TestAuthorizeScope_HierarchyEndDateAsksAboutBothEntities(t *testing.T) {
	az := &scopesAuthZ{}
	ms := newMemStore()
	ms.hierarchies = map[string]*domain.EntityHierarchy{
		"h-1": {HierarchyID: "h-1", TenantID: "ten-001", ChildLegalEntityID: targetEntityID, ParentLegalEntityID: parentEntityID},
	}
	svc := newSvc(t, ms, az, acceptAllJurisd{})

	require.NoError(t, svc.EndDateHierarchy(callerAt(callerEntityID), "h-1", time.Now().UTC(), "corr"))
	assert.Equal(t, []string{targetEntityID, parentEntityID}, az.scopes)

	az.scopes = nil
	_ = svc.EndDateHierarchy(callerAt(callerEntityID), "h-missing", time.Now().UTC(), "corr")
	assert.Equal(t, []string{callerEntityID}, az.scopes)
}

// A malformed entity id is the caller's mistake: 400, and authorization-svc
// is not asked (its 400 would surface here as a fail-closed 503).
func TestAuthorizeScope_MalformedEntityIDIsInvalidInputNotUnavailable(t *testing.T) {
	az := &scopesAuthZ{}
	svc := newSvc(t, newMemStore(), az, acceptAllJurisd{})
	name := "x"

	_, err := svc.UpdateEntity(callerAt(callerEntityID), "not-a-uuid", domain.UpdateEntityRequest{TradingName: &name})
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	assert.Empty(t, az.scopes)
}

// Tenant-level commands keep the caller's envelope entity: they act on no
// single entity, and authorization-svc answers them from tenant-wide grants.
func TestAuthorizeScope_TenantLevelCommandsKeepTheCallersEntity(t *testing.T) {
	az := &scopesAuthZ{allow: map[string]bool{}}
	svc := newSvc(t, newMemStore(), az, acceptAllJurisd{})

	err := svc.TransitionTenantLifecycle(callerAt(callerEntityID), "ten-001", domain.TransitionTenantLifecycleRequest{})
	require.ErrorIs(t, err, registry.ErrUnauthorized)
	assert.Equal(t, []string{callerEntityID}, az.scopes)
}
