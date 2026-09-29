package registry_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/entitlement"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

// ORG-03 §4.3 required source inputs at CreateEntity (29 Sep 2026 re-audit).
// The legal form, registered address and supporting evidence were not fields
// of the create request: a client sending them had them silently discarded,
// so "invalid ELF code at creation" was accepted with a 201 live.

func strictEntityReq() domain.CreateEntityRequest {
	return domain.CreateEntityRequest{
		TenantID: gapsTenant, EntityCode: "IN1", LegalName: "Inputs Co", EntityType: domain.EntityTypeSubsidiary,
		DefaultCurrencyCode: "GBP", FiscalCalendarID: "0f000000-0000-4000-8000-000000000001",
		PrimaryJurisdictionID: "JUR-UK", DataResidencyPolicyID: "p",
		LegalFormCode: "h0po", LegalFormSource: "GLEIF-ELF-1.6", LegalFormLocalText: "Private limited company",
		RegistryAuthority: "Companies House",
		RegisteredOffice:  json.RawMessage(`{"line1":"1 High St","city":"London","country":"GB"}`),
		SourceEvidenceRef: "CH-EXTRACT-1",
	}
}

func strictEntitySvc(t *testing.T) (*registry.Service, *memStore) {
	t.Helper()
	svc, ms := baseSvc(t)
	svc.ConfigureProvisioning(entitlement.NewStubChecker(zap.NewNop()), nil, false)
	return svc, ms
}

func TestCreateEntity_RecordsTheSourceInputsOnProfileVersionOne(t *testing.T) {
	svc, ms := strictEntitySvc(t)
	e, err := svc.CreateEntity(tenantCtx(gapsTenant), strictEntityReq())
	require.NoError(t, err)

	v := ms.org().profileVersions[e.LegalEntityID][0]
	require.NotNil(t, v.LegalFormCode)
	assert.Equal(t, "H0PO", *v.LegalFormCode, "normalised to upper case")
	assert.Equal(t, "GLEIF-ELF-1.6", *v.LegalFormSource)
	assert.Equal(t, "Private limited company", *v.LegalFormLocalText)
	assert.Equal(t, "Companies House", *v.RegistryAuthority)
	require.NotNil(t, v.RegisteredOffice)
	assert.JSONEq(t, `{"line1":"1 High St","city":"London","country":"GB"}`, *v.RegisteredOffice)
	assert.Equal(t, "CH-EXTRACT-1", *v.SourceEvidenceRef)
}

func TestCreateEntity_RefusesMissingOrInvalidSourceInputs(t *testing.T) {
	cases := map[string]struct {
		mut  func(r *domain.CreateEntityRequest)
		want error
	}{
		"invalid ELF code":        {func(r *domain.CreateEntityRequest) { r.LegalFormCode = "bad!" }, registry.ErrInvalidInput},
		"ELF code without source": {func(r *domain.CreateEntityRequest) { r.LegalFormSource = "" }, registry.ErrSourceUnverified},
		"ELF without local text":  {func(r *domain.CreateEntityRequest) { r.LegalFormLocalText = "" }, registry.ErrInvalidInput},
		"no registered office":    {func(r *domain.CreateEntityRequest) { r.RegisteredOffice = nil }, registry.ErrInvalidInput},
		"null registered office":  {func(r *domain.CreateEntityRequest) { r.RegisteredOffice = json.RawMessage(`null`) }, registry.ErrInvalidInput},
		"no supporting evidence":  {func(r *domain.CreateEntityRequest) { r.SourceEvidenceRef = " " }, registry.ErrSourceUnverified},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			svc, ms := strictEntitySvc(t)
			req := strictEntityReq()
			c.mut(&req)
			_, err := svc.CreateEntity(tenantCtx(gapsTenant), req)
			require.ErrorIs(t, err, c.want)
			assert.Empty(t, ms.entities, "nothing is written on a refused create")
		})
	}
}

// The legal form stays optional: entity_type answers §4.3's "entity
// type/legal form", and not every entity has an ELF code.
func TestCreateEntity_LegalFormIsOptional(t *testing.T) {
	svc, _ := strictEntitySvc(t)
	req := strictEntityReq()
	req.LegalFormCode, req.LegalFormSource, req.LegalFormLocalText = "", "", ""
	_, err := svc.CreateEntity(tenantCtx(gapsTenant), req)
	require.NoError(t, err)
}

// Local development keeps the relaxed contract until the console sends the
// new fields (LEGACY_PROVISIONING_INPUTS, refused at boot in staging/prod).
func TestCreateEntity_LegacyInputsRelaxOnlyTheRequiredFields(t *testing.T) {
	svc, _ := baseSvc(t) // legacy mode
	req := strictEntityReq()
	req.RegisteredOffice, req.SourceEvidenceRef = nil, ""
	_, err := svc.CreateEntity(tenantCtx(gapsTenant), req)
	require.NoError(t, err)

	req.LegalFormCode = "bad!"
	_, err = svc.CreateEntity(tenantCtx(gapsTenant), req)
	require.ErrorIs(t, err, registry.ErrInvalidInput, "the ELF control is never relaxed")
}
