package registry_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/fiscalcal"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

type fakeFiscal struct {
	exists  bool
	err     error
	tenant  string
	calenID string
}

func (f *fakeFiscal) Exists(_ context.Context, tenantID, calendarID string) (bool, error) {
	f.tenant, f.calenID = tenantID, calendarID
	return f.exists, f.err
}

// ORG-03 s4.3 / S2-C1: the fiscal calendar must exist for the tenant, not merely look like an id.
func TestCreateEntity_FiscalCalendarMustExistForTheTenantAndFailsClosed(t *testing.T) {
	svc, _ := strictEntitySvc(t)
	f := &fakeFiscal{exists: true}
	svc.WithFiscalCalendarValidator(f)

	_, err := svc.CreateEntity(tenantCtx(gapsTenant), strictEntityReq())
	require.NoError(t, err)
	assert.Equal(t, gapsTenant, f.tenant, "asked under the entity's own tenant")
	assert.Equal(t, strictEntityReq().FiscalCalendarID, f.calenID)

	f.exists = false
	_, err = svc.CreateEntity(tenantCtx(gapsTenant), strictEntityReq())
	assert.ErrorIs(t, err, registry.ErrReferenceInvalid, "an unknown calendar is refused")

	f.err = fiscalcal.ErrUnavailable
	_, err = svc.CreateEntity(tenantCtx(gapsTenant), strictEntityReq())
	assert.ErrorIs(t, err, registry.ErrServiceUnavailable, "an unreachable service fails closed")

	// A malformed id never reaches the service.
	f.err, f.tenant = nil, ""
	r := strictEntityReq()
	r.FiscalCalendarID = "not-a-uuid"
	_, err = svc.CreateEntity(tenantCtx(gapsTenant), r)
	assert.ErrorIs(t, err, registry.ErrInvalidInput)
	assert.Empty(t, f.tenant)
}
