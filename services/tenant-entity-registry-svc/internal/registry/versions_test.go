package registry_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

// ORG §4.2/§4.3: commands use expected_version. Strict mode (the default
// outside local development) refuses a command that sends none, instead of
// substituting the version the service just read.

func strictVersionSvc(t *testing.T) (*registry.Service, *memStore) {
	t.Helper()
	svc, ms := baseSvc(t)
	svc.ConfigureConcurrency(false)
	return svc, ms
}

func TestExpectedVersion_RequiredOnTenantCommands(t *testing.T) {
	svc, ms := strictVersionSvc(t)
	ms.tenants[orgTenant].LifecycleState = domain.TenantLifecycleActive
	before := ms.tenants[orgTenant].RecordVersion

	_, err := svc.ExecuteTenantCommand(tenantCtx(orgTenant), orgTenant, domain.TenantCommandSuspend,
		domain.ExecuteTenantCommandRequest{Reason: "incident"})
	require.ErrorIs(t, err, registry.ErrVersionRequired)
	assert.Equal(t, before, ms.tenants[orgTenant].RecordVersion, "nothing ran")

	_, err = svc.ExecuteTenantCommand(tenantCtx(orgTenant), orgTenant, domain.TenantCommandSuspend,
		domain.ExecuteTenantCommandRequest{Reason: "incident", ExpectedVersion: before})
	require.NoError(t, err)
}

func TestExpectedVersion_RequiredOnDefaultsAndHomeRegion(t *testing.T) {
	svc, ms := strictVersionSvc(t)
	_, err := svc.ChangeDefaultLocale(tenantCtx(orgTenant), orgTenant,
		domain.ChangeDefaultLocaleRequest{PrimaryLocale: "fr-FR", Reason: "r"})
	require.ErrorIs(t, err, registry.ErrVersionRequired)

	req := homeRegionReq()
	require.ErrorIs(t, svc.ChangeHomeRegion(tenantCtx(orgTenant), orgTenant, req), registry.ErrVersionRequired)
	req.ExpectedVersion = ms.tenants[orgTenant].RecordVersion
	pendingOf(t, svc.ChangeHomeRegion(tenantCtx(orgTenant), orgTenant, req))
}

func TestExpectedVersion_StaleIsStillAVersionConflict(t *testing.T) {
	svc, ms := strictVersionSvc(t)
	ms.tenants[orgTenant].LifecycleState = domain.TenantLifecycleActive
	_, err := svc.ExecuteTenantCommand(tenantCtx(orgTenant), orgTenant, domain.TenantCommandSuspend,
		domain.ExecuteTenantCommandRequest{Reason: "incident", ExpectedVersion: ms.tenants[orgTenant].RecordVersion + 3})
	require.ErrorIs(t, err, registry.ErrVersionConflict)
}
