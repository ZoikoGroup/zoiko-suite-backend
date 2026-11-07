package registry_test

// REF-08 service: validation, authorization at the value's own owner, the correct permission per command, and a store
// without dimensions answering "unavailable".

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

const dimEntity = "0190a000-0000-7000-8000-00000000dddd"

type dimFake struct {
	*memStore
	registry.DimensionStore
	calls  int
	actor  string
	value  *domain.DimensionValue
	last   string
	lastNo int
}

func (d *dimFake) CreateDimensionType(_ context.Context, _ domain.CreateDimensionTypeRequest, actor string) (*domain.DimensionType, error) {
	d.calls++
	d.actor = actor
	return &domain.DimensionType{}, nil
}
func (d *dimFake) CreateDimensionValue(_ context.Context, _ domain.CreateDimensionValueRequest, actor string) (*registry.DimensionOutcome, error) {
	d.calls++
	d.actor = actor
	return &registry.DimensionOutcome{}, nil
}
func (d *dimFake) GetDimensionValue(_ context.Context, id string) (*domain.DimensionValue, error) {
	d.calls++
	if id == "missing" {
		return nil, nil
	}
	cp := *d.value
	return &cp, nil
}
func (d *dimFake) ProposeHierarchyChange(_ context.Context, _ string, _ domain.ReparentDimensionValueRequest, actor string, _ bool) (*registry.DimensionOutcome, error) {
	d.calls++
	d.actor, d.last = actor, "reparent"
	return &registry.DimensionOutcome{}, nil
}
func (d *dimFake) ApproveHierarchyChange(_ context.Context, _ string, no int, _ domain.ApproveDimensionHierarchyRequest, actor string, _ bool) (*registry.DimensionOutcome, error) {
	d.calls++
	d.actor, d.last, d.lastNo = actor, "approve", no
	return &registry.DimensionOutcome{}, nil
}
func (d *dimFake) ChangeDimensionStatus(_ context.Context, _, command, to string, _ domain.DimensionStatusRequest, actor string, _ bool) (*registry.DimensionOutcome, error) {
	d.calls++
	d.actor, d.last = actor, command+":"+to
	return &registry.DimensionOutcome{}, nil
}
func (d *dimFake) MapSourceObject(_ context.Context, _ string, _ domain.MapSourceObjectRequest, actor string) (*registry.MappingOutcome, error) {
	d.calls++
	d.actor, d.last = actor, "map"
	return &registry.MappingOutcome{}, nil
}
func (d *dimFake) GetDimensionValueView(_ context.Context, id string, _ time.Time, _ bool) (*domain.DimensionValueView, error) {
	d.calls++
	if id == "missing" {
		return nil, nil
	}
	return &domain.DimensionValueView{}, nil
}
func (d *dimFake) GetHierarchyAsOf(context.Context, string, string, time.Time) (*domain.HierarchyAsOf, error) {
	d.calls++
	return &domain.HierarchyAsOf{}, nil
}
func (d *dimFake) ListPostingEligibleValues(context.Context, string, string, time.Time) ([]registry.EligibleDimensionValue, error) {
	d.calls++
	return nil, nil
}
func (d *dimFake) ResolveDimension(context.Context, domain.ResolveDimensionQuery) (*domain.DimensionResolution, error) {
	d.calls++
	return &domain.DimensionResolution{}, nil
}

func dimSvc(t *testing.T, az registry.AuthorizationClient, entityScoped bool) (*registry.Service, *dimFake) {
	t.Helper()
	f := &dimFake{memStore: newMemStore(), value: &domain.DimensionValue{ValueID: "v1", TypeCode: "REGION", Code: "A"}}
	if entityScoped {
		f.value.LegalEntityID = dimEntity
	}
	f.entities[dimEntity] = &domain.LegalEntity{LegalEntityID: dimEntity, TenantID: "tenant-001", EntityStatus: domain.EntityStatusActive}
	return newSvc(t, f, az, acceptAllJurisd{}), f
}

func TestDimension_TypeAndValueCreationValidateThenAuthorizeThenStore(t *testing.T) {
	svc, f := dimSvc(t, permitAllAuthZ{}, true)
	_, err := svc.CreateDimensionType(tenantCtx("tenant-001"), domain.CreateDimensionTypeRequest{TypeCode: "bad", Name: "n"})
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	_, err = svc.CreateDimensionValue(tenantCtx("tenant-001"), domain.CreateDimensionValueRequest{TypeCode: "REGION", Code: "a", Label: "l", EvidenceRef: "e"})
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	assert.Zero(t, f.calls)

	az := &recordingAuthZ{}
	tsvc, tf := dimSvc(t, az, true)
	_, err = tsvc.CreateDimensionType(tenantCtx("tenant-001"), domain.CreateDimensionTypeRequest{TypeCode: "REGION", Name: "n", Hierarchical: true})
	require.NoError(t, err)
	assert.Equal(t, "dimension.type", az.resource)
	assert.Equal(t, "create", az.action)
	assert.Equal(t, testPrincipal, tf.actor)

	good := domain.CreateDimensionValueRequest{TypeCode: "REGION", LegalEntityID: dimEntity, Code: "UK", Label: "United Kingdom", EvidenceRef: "e"}
	_, err = tsvc.CreateDimensionValue(tenantCtx("tenant-001"), good)
	require.NoError(t, err)
	assert.Equal(t, "dimension", az.resource)
	assert.Equal(t, dimEntity, az.scopeID, "an entity's own value is authorized at that entity")
	good.LegalEntityID = ""
	_, err = tsvc.CreateDimensionValue(tenantCtx("tenant-001"), good)
	require.NoError(t, err)
	assert.NotEqual(t, dimEntity, az.scopeID, "a tenant-wide value is authorized at the tenant")

	denied, f2 := dimSvc(t, denyAllAuthZ{}, true)
	_, err = denied.CreateDimensionValue(tenantCtx("tenant-001"), domain.CreateDimensionValueRequest{TypeCode: "REGION", Code: "UK", Label: "l", EvidenceRef: "e"})
	require.ErrorIs(t, err, registry.ErrUnauthorized)
	assert.Zero(t, f2.calls)

	f.tenants["tenant-001"].LifecycleState = domain.TenantLifecycleSuspended
	_, err = svc.CreateDimensionType(tenantCtx("tenant-001"), domain.CreateDimensionTypeRequest{TypeCode: "REGION", Name: "n"})
	require.Error(t, err)
	assert.Zero(t, f.calls)
}

func TestDimension_EachCommandIsItsOwnPermissionAtTheValuesOwner(t *testing.T) {
	p := "0190a000-0000-7000-8000-00000000aaaa"
	for name, tc := range map[string]struct {
		action, last string
		run          func(*registry.Service) error
	}{
		"reparent": {"reparent", "reparent", func(s *registry.Service) error {
			_, err := s.ReparentDimensionValue(tenantCtx("tenant-001"), "v1", domain.ReparentDimensionValueRequest{ExpectedVersion: 1, ParentValueID: &p, Reason: "r", EvidenceRef: "e"})
			return err
		}},
		"approve": {"approve-hierarchy", "approve", func(s *registry.Service) error {
			_, err := s.ApproveDimensionHierarchy(tenantCtx("tenant-001"), "v1", 2, domain.ApproveDimensionHierarchyRequest{ExpectedVersion: 1, EvidenceRef: "e"})
			return err
		}},
		"activate": {"activate", "ActivateDimensionValue:ACTIVE", func(s *registry.Service) error {
			_, err := s.ChangeDimensionStatus(tenantCtx("tenant-001"), "v1", domain.DimActive, domain.DimensionStatusRequest{ExpectedVersion: 1, EvidenceRef: "e"})
			return err
		}},
		"deactivate": {"deactivate", "DeactivateDimensionValue:INACTIVE", func(s *registry.Service) error {
			_, err := s.ChangeDimensionStatus(tenantCtx("tenant-001"), "v1", domain.DimInactive, domain.DimensionStatusRequest{ExpectedVersion: 1, Reason: "r", EvidenceRef: "e"})
			return err
		}},
		"map": {"map-source", "map", func(s *registry.Service) error {
			_, err := s.MapSourceObject(tenantCtx("tenant-001"), "v1", domain.MapSourceObjectRequest{SourceDomain: "PROJECT", SourceObjectID: "P", EvidenceRef: "e"})
			return err
		}},
	} {
		t.Run(name, func(t *testing.T) {
			az := &recordingAuthZ{}
			svc, f := dimSvc(t, az, true)
			require.NoError(t, tc.run(svc))
			assert.Equal(t, "dimension", az.resource)
			assert.Equal(t, tc.action, az.action)
			assert.Equal(t, dimEntity, az.scopeID)
			assert.Equal(t, tc.last, f.last)
			assert.Equal(t, testPrincipal, f.actor)

			deny, f2 := dimSvc(t, &recordingAuthZ{err: registry.ErrUnauthorized}, true)
			require.ErrorIs(t, tc.run(deny), registry.ErrUnauthorized)
			assert.Empty(t, f2.last, "a denied caller changes nothing")

			tenantWide, _ := dimSvc(t, az, false)
			require.NoError(t, tc.run(tenantWide))
			assert.NotEqual(t, dimEntity, az.scopeID)
		})
	}

	svc, f := dimSvc(t, permitAllAuthZ{}, true)
	for name, run := range map[string]func() error{
		"missing": func() error {
			_, err := svc.ChangeDimensionStatus(tenantCtx("tenant-001"), "missing", domain.DimActive, domain.DimensionStatusRequest{EvidenceRef: "e"})
			return err
		},
		"approve": func() error {
			_, err := svc.ApproveDimensionHierarchy(tenantCtx("tenant-001"), "missing", 2, domain.ApproveDimensionHierarchyRequest{EvidenceRef: "e"})
			return err
		},
		"map missing": func() error {
			_, err := svc.MapSourceObject(tenantCtx("tenant-001"), "missing", domain.MapSourceObjectRequest{SourceDomain: "P", SourceObjectID: "1", EvidenceRef: "e"})
			return err
		},
	} {
		assert.ErrorIs(t, run(), registry.ErrNotFound, name)
	}
	_, err := svc.ChangeDimensionStatus(tenantCtx("tenant-001"), "v1", domain.DimDraft, domain.DimensionStatusRequest{EvidenceRef: "e"})
	require.ErrorIs(t, err, registry.ErrInvalidInput, "nothing moves back to draft")
	_, err = svc.ChangeDimensionStatus(tenantCtx("tenant-001"), "v1", domain.DimInactive, domain.DimensionStatusRequest{EvidenceRef: "e"})
	require.ErrorIs(t, err, registry.ErrInvalidInput, "deactivating states why")
	assert.Equal(t, 0, f.lastNo)
}

func TestDimension_ReadsResolveAndAnUnavailableStore(t *testing.T) {
	svc, _ := dimSvc(t, permitAllAuthZ{}, true)
	_, err := svc.GetDimensionValue(tenantCtx("tenant-001"), "missing", time.Time{}, false)
	require.ErrorIs(t, err, registry.ErrNotFound)
	_, err = svc.GetDimensionValue(tenantCtx("tenant-001"), "v1", time.Time{}, true)
	require.NoError(t, err)
	_, err = svc.GetHierarchyAsOf(tenantCtx("tenant-001"), "REGION", "", time.Time{})
	require.NoError(t, err)
	_, err = svc.ListPostingEligibleValues(tenantCtx("tenant-001"), "REGION", "", time.Time{})
	require.NoError(t, err)
	_, err = svc.ResolveDimension(tenantCtx("tenant-001"), domain.ResolveDimensionQuery{TypeCode: "REGION", Code: "A"})
	require.NoError(t, err)
	_, err = svc.ResolveDimension(tenantCtx("tenant-001"), domain.ResolveDimensionQuery{TypeCode: "REGION"})
	require.ErrorIs(t, err, registry.ErrInvalidInput)

	bare := newSvc(t, newMemStore(), permitAllAuthZ{}, acceptAllJurisd{})
	_, err = bare.GetDimensionValue(tenantCtx("tenant-001"), "v1", time.Time{}, false)
	require.ErrorIs(t, err, registry.ErrServiceUnavailable)
	_, err = bare.ListDimensionTypes(tenantCtx("tenant-001"))
	require.ErrorIs(t, err, registry.ErrServiceUnavailable)
	_, err = bare.ResolveDimension(tenantCtx("tenant-001"), domain.ResolveDimensionQuery{TypeCode: "R", Code: "A"})
	require.ErrorIs(t, err, registry.ErrServiceUnavailable)
}
