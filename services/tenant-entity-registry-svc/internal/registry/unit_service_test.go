package registry_test

// REF-09 service: validation before authorization before the store, the right permission per command at the tenant, and a
// store without units answering "unavailable".

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

type unitFake struct {
	*memStore
	registry.UnitStore
	calls  int
	actor  string
	last   string
	lastNo int
	supers bool
	asOf   time.Time
	absent bool
}

func (u *unitFake) CreateQuantityDimension(_ context.Context, _ domain.CreateQuantityDimensionRequest, actor string) (*registry.DimensionWithBase, error) {
	u.calls++
	u.actor, u.last = actor, "dimension"
	return &registry.DimensionWithBase{}, nil
}
func (u *unitFake) CreateUnit(_ context.Context, _ domain.CreateUnitRequest, actor string) (*registry.UnitOutcome, error) {
	u.calls++
	u.actor, u.last = actor, "create"
	return &registry.UnitOutcome{}, nil
}
func (u *unitFake) ProposeConversion(_ context.Context, _ string, supersede bool, _ domain.ProposeConversionRequest, actor string, _ bool) (*registry.UnitOutcome, error) {
	u.calls++
	u.actor, u.last, u.supers = actor, "propose", supersede
	return &registry.UnitOutcome{}, nil
}
func (u *unitFake) ApproveConversion(_ context.Context, _ string, no int, _ domain.ApproveConversionRequest, actor string, _ bool) (*registry.UnitOutcome, error) {
	u.calls++
	u.actor, u.last, u.lastNo = actor, "approve", no
	return &registry.UnitOutcome{}, nil
}
func (u *unitFake) RetireUnit(_ context.Context, _ string, _ domain.RetireUnitRequest, actor string, _ bool) (*registry.UnitOutcome, error) {
	u.calls++
	u.actor, u.last = actor, "retire"
	return &registry.UnitOutcome{}, nil
}
func (u *unitFake) GetUnit(_ context.Context, _ string, asOf time.Time, _ bool) (*domain.UnitView, error) {
	u.calls++
	u.asOf = asOf
	if u.absent {
		return nil, nil
	}
	return &domain.UnitView{}, nil
}
func (u *unitFake) GetConversionVersion(context.Context, string, int) (*domain.ConversionVersion, error) {
	u.calls++
	if u.absent {
		return nil, nil
	}
	return &domain.ConversionVersion{}, nil
}
func (u *unitFake) ConvertQuantity(_ context.Context, q domain.ConvertQuantityQuery) (*domain.ConversionResult, error) {
	u.calls++
	u.asOf = q.AsOf
	return &domain.ConversionResult{}, nil
}

func unitSvc(t *testing.T, az registry.AuthorizationClient) (*registry.Service, *unitFake) {
	t.Helper()
	f := &unitFake{memStore: newMemStore()}
	return newSvc(t, f, az, acceptAllJurisd{}), f
}

func TestUnit_EachCommandIsItsOwnPermissionAtTheTenant(t *testing.T) {
	prop := domain.ProposeConversionRequest{ExpectedVersion: 1, Factor: "1000", SourceClass: domain.ConvStandard, SourceRef: "ISO", EvidenceRef: "e"}
	for name, tc := range map[string]struct {
		resource, action, last string
		run                    func(*registry.Service) error
	}{
		"dimension": {"unit.dimension", "create", "dimension", func(s *registry.Service) error {
			_, err := s.CreateQuantityDimension(tenantCtx("tenant-001"), domain.CreateQuantityDimensionRequest{DimensionCode: "LENGTH", Name: "n",
				BaseUnit: domain.UnitFields{UnitCode: "M", Name: "Metre", Symbol: "m"}})
			return err
		}},
		"create": {"unit", "create", "create", func(s *registry.Service) error {
			_, err := s.CreateUnit(tenantCtx("tenant-001"), domain.CreateUnitRequest{DimensionCode: "LENGTH", UnitFields: domain.UnitFields{UnitCode: "KM", Name: "Kilometre", Symbol: "km"}})
			return err
		}},
		"propose": {"unit", "propose-conversion", "propose", func(s *registry.Service) error {
			_, err := s.ProposeConversion(tenantCtx("tenant-001"), "KM", false, prop)
			return err
		}},
		"supersede": {"unit", "propose-conversion", "propose", func(s *registry.Service) error {
			_, err := s.ProposeConversion(tenantCtx("tenant-001"), "KM", true, prop)
			return err
		}},
		"approve": {"unit", "approve-conversion", "approve", func(s *registry.Service) error {
			_, err := s.ApproveConversion(tenantCtx("tenant-001"), "KM", 2, domain.ApproveConversionRequest{ExpectedVersion: 1, EvidenceRef: "e"})
			return err
		}},
		"retire": {"unit", "retire", "retire", func(s *registry.Service) error {
			_, err := s.RetireUnit(tenantCtx("tenant-001"), "KM", domain.RetireUnitRequest{ExpectedVersion: 1, Reason: "r", EvidenceRef: "e"})
			return err
		}},
	} {
		t.Run(name, func(t *testing.T) {
			az := &recordingAuthZ{}
			svc, f := unitSvc(t, az)
			require.NoError(t, tc.run(svc))
			assert.Equal(t, tc.resource, az.resource)
			assert.Equal(t, tc.action, az.action)
			assert.Equal(t, tc.last, f.last)
			assert.Equal(t, testPrincipal, f.actor)
			assert.Equal(t, name == "supersede", f.supers, "supersede is told to the store, never inferred")

			deny, f2 := unitSvc(t, &recordingAuthZ{err: registry.ErrUnauthorized})
			require.ErrorIs(t, tc.run(deny), registry.ErrUnauthorized)
			assert.Empty(t, f2.last, "a denied caller changes nothing")

			susp, f3 := unitSvc(t, permitAllAuthZ{})
			f3.tenants["tenant-001"].LifecycleState = domain.TenantLifecycleSuspended
			require.Error(t, tc.run(susp), "a suspended tenant cannot change units")
			assert.Empty(t, f3.last)
		})
	}
}

func TestUnit_ValidationComesBeforeAnything(t *testing.T) {
	svc, f := unitSvc(t, permitAllAuthZ{})
	ctx := tenantCtx("tenant-001")
	_, err := svc.CreateQuantityDimension(ctx, domain.CreateQuantityDimensionRequest{DimensionCode: "x"})
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	_, err = svc.CreateUnit(ctx, domain.CreateUnitRequest{DimensionCode: "LENGTH"})
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	_, err = svc.ProposeConversion(ctx, "KM", false, domain.ProposeConversionRequest{Factor: "0", SourceClass: domain.ConvStandard, SourceRef: "s", EvidenceRef: "e"})
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	_, err = svc.ApproveConversion(ctx, "KM", 1, domain.ApproveConversionRequest{})
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	_, err = svc.RetireUnit(ctx, "KM", domain.RetireUnitRequest{})
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	_, err = svc.ConvertQuantity(ctx, domain.ConvertQuantityQuery{Quantity: "one", From: "KM", To: "M"})
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	assert.Zero(t, f.calls)
}

func TestUnit_ReadsDefaultToNowAndAbsentIsNotFound(t *testing.T) {
	svc, f := unitSvc(t, permitAllAuthZ{})
	ctx := tenantCtx("tenant-001")
	_, err := svc.GetUnit(ctx, "KM", time.Time{}, false)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now(), f.asOf, time.Minute, "no date means now")
	_, err = svc.ConvertQuantity(ctx, domain.ConvertQuantityQuery{Quantity: "1", From: "KM", To: "M"})
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now(), f.asOf, time.Minute)

	f.absent = true
	_, err = svc.GetUnit(ctx, "NOPE", time.Now(), false)
	assert.ErrorIs(t, err, registry.ErrNotFound)
	_, err = svc.GetConversionVersion(ctx, "NOPE", 1)
	assert.ErrorIs(t, err, registry.ErrNotFound)
}

func TestUnit_AStoreWithoutUnitsIsUnavailable(t *testing.T) {
	svc := newSvc(t, newMemStore(), permitAllAuthZ{}, acceptAllJurisd{})
	ctx := tenantCtx("tenant-001")
	_, err := svc.ListQuantityDimensions(ctx)
	assert.ErrorIs(t, err, registry.ErrServiceUnavailable)
	_, err = svc.GetUnit(ctx, "KM", time.Now(), false)
	assert.ErrorIs(t, err, registry.ErrServiceUnavailable)
	_, err = svc.ConvertQuantity(ctx, domain.ConvertQuantityQuery{Quantity: "1", From: "KM", To: "M"})
	assert.ErrorIs(t, err, registry.ErrServiceUnavailable)
	_, err = svc.CreateUnit(ctx, domain.CreateUnitRequest{DimensionCode: "LENGTH", UnitFields: domain.UnitFields{UnitCode: "KM", Name: "n", Symbol: "km"}})
	assert.ErrorIs(t, err, registry.ErrServiceUnavailable)
}
