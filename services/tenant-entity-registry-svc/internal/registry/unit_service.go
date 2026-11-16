package registry

// REF-09 Units of Measure: CreateUnit, ApproveConversion, SupersedeConversion, RetireUnit and the reads GetUnit,
// ConvertQuantity, GetConversionVersion and ListUnitsByDimension (ZS-SVC-I-001 s4.19).
//
// Units are tenant-wide reference data, so every command is authorized at the tenant. Dimensionality is checked before
// arithmetic, and a non-standard conversion is approved by someone other than its proposer. This service owns no
// quantities; the domains that hold them pin the conversion version a result applied.

import (
	"context"
	"fmt"
	"time"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

func (s *Service) unitStore() (UnitStore, error) {
	us, ok := s.store.(UnitStore)
	if !ok {
		return nil, ErrServiceUnavailable
	}
	return us, nil
}

// unitCommand is the shared preamble of every unit command: validate, authorize, check the tenant may transact.
func (s *Service) unitCommand(ctx context.Context, resource, action string, validate func() error) (UnitStore, error) {
	if err := validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.authorize(ctx, resource, action); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	return s.unitStore()
}

// CreateQuantityDimension creates a quantity dimension with its base unit.
func (s *Service) CreateQuantityDimension(ctx context.Context, req domain.CreateQuantityDimensionRequest) (*DimensionWithBase, error) {
	us, err := s.unitCommand(ctx, "unit.dimension", "create", req.Validate)
	if err != nil {
		return nil, err
	}
	out, err := us.CreateQuantityDimension(ctx, req, domain.PrincipalFromContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("store.CreateQuantityDimension: %w", err)
	}
	return out, nil
}

// ListQuantityDimensions lists the tenant's quantity dimensions.
func (s *Service) ListQuantityDimensions(ctx context.Context) ([]domain.QuantityDimension, error) {
	us, err := s.unitStore()
	if err != nil {
		return nil, err
	}
	out, err := us.ListQuantityDimensions(ctx)
	if err != nil {
		return nil, fmt.Errorf("store.ListQuantityDimensions: %w", err)
	}
	return out, nil
}

// CreateUnit is CreateUnit.
func (s *Service) CreateUnit(ctx context.Context, req domain.CreateUnitRequest) (*UnitOutcome, error) {
	us, err := s.unitCommand(ctx, "unit", "create", req.Validate)
	if err != nil {
		return nil, err
	}
	out, err := us.CreateUnit(ctx, req, domain.PrincipalFromContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("store.CreateUnit: %w", err)
	}
	return out, nil
}

// ProposeConversion records a unit's first conversion, or with supersede (SupersedeConversion) replaces the active one.
func (s *Service) ProposeConversion(ctx context.Context, unitCode string, supersede bool, req domain.ProposeConversionRequest) (*UnitOutcome, error) {
	us, err := s.unitCommand(ctx, "unit", "propose-conversion", req.Validate)
	if err != nil {
		return nil, err
	}
	out, err := us.ProposeConversion(ctx, unitCode, supersede, req, domain.PrincipalFromContext(ctx), s.expectedVersionOptional)
	if err != nil {
		return nil, fmt.Errorf("store.ProposeConversion: %w", err)
	}
	return out, nil
}

// ApproveConversion is ApproveConversion.
func (s *Service) ApproveConversion(ctx context.Context, unitCode string, versionNo int, req domain.ApproveConversionRequest) (*UnitOutcome, error) {
	us, err := s.unitCommand(ctx, "unit", "approve-conversion", req.Validate)
	if err != nil {
		return nil, err
	}
	out, err := us.ApproveConversion(ctx, unitCode, versionNo, req, domain.PrincipalFromContext(ctx), s.expectedVersionOptional)
	if err != nil {
		return nil, fmt.Errorf("store.ApproveConversion: %w", err)
	}
	return out, nil
}

// RetireUnit is RetireUnit.
func (s *Service) RetireUnit(ctx context.Context, unitCode string, req domain.RetireUnitRequest) (*UnitOutcome, error) {
	us, err := s.unitCommand(ctx, "unit", "retire", req.Validate)
	if err != nil {
		return nil, err
	}
	out, err := us.RetireUnit(ctx, unitCode, req, domain.PrincipalFromContext(ctx), s.expectedVersionOptional)
	if err != nil {
		return nil, fmt.Errorf("store.RetireUnit: %w", err)
	}
	return out, nil
}

// GetUnit is GetUnit, as the unit was on asOf.
func (s *Service) GetUnit(ctx context.Context, unitCode string, asOf time.Time, history bool) (*domain.UnitView, error) {
	us, err := s.unitStore()
	if err != nil {
		return nil, err
	}
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}
	out, err := us.GetUnit(ctx, unitCode, asOf, history)
	if err != nil {
		return nil, fmt.Errorf("store.GetUnit: %w", err)
	}
	if out == nil {
		return nil, ErrNotFound
	}
	return out, nil
}

// GetConversionVersion is GetConversionVersion.
func (s *Service) GetConversionVersion(ctx context.Context, unitCode string, versionNo int) (*domain.ConversionVersion, error) {
	us, err := s.unitStore()
	if err != nil {
		return nil, err
	}
	out, err := us.GetConversionVersion(ctx, unitCode, versionNo)
	if err != nil {
		return nil, fmt.Errorf("store.GetConversionVersion: %w", err)
	}
	if out == nil {
		return nil, ErrNotFound
	}
	return out, nil
}

// ListUnitsByDimension is ListUnitsByDimension.
func (s *Service) ListUnitsByDimension(ctx context.Context, dimensionCode string) ([]domain.Unit, error) {
	us, err := s.unitStore()
	if err != nil {
		return nil, err
	}
	out, err := us.ListUnitsByDimension(ctx, dimensionCode)
	if err != nil {
		return nil, fmt.Errorf("store.ListUnitsByDimension: %w", err)
	}
	return out, nil
}

// ConvertQuantity is ConvertQuantity.
func (s *Service) ConvertQuantity(ctx context.Context, q domain.ConvertQuantityQuery) (*domain.ConversionResult, error) {
	if err := q.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	us, err := s.unitStore()
	if err != nil {
		return nil, err
	}
	if q.AsOf.IsZero() {
		q.AsOf = time.Now().UTC()
	}
	out, err := us.ConvertQuantity(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store.ConvertQuantity: %w", err)
	}
	return out, nil
}
