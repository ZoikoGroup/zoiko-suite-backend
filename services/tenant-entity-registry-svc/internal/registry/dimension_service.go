package registry

// REF-08 Financial Dimension Registry: CreateDimensionType, CreateDimensionValue, ReparentDimensionValue,
// ActivateDimensionValue, DeactivateDimensionValue, MapSourceObject and the reads GetDimensionValue, ResolveDimension,
// GetHierarchyAsOf and ListPostingEligibleValues (ZS-SVC-I-001 s4.18).
//
// A command on an entity's own value is authorized at that entity, and on a tenant-wide value at the tenant. A hierarchy
// change to a controlled type is proposed by one principal and approved by another. This service answers whether a value
// may take a posting; the posting service must ask it, because it cannot stop a posting itself.

import (
	"context"
	"fmt"
	"time"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

func (s *Service) dimensionStore() (DimensionStore, error) {
	ds, ok := s.store.(DimensionStore)
	if !ok {
		return nil, ErrServiceUnavailable
	}
	return ds, nil
}

func (s *Service) authorizeDimension(ctx context.Context, legalEntityID, action string) error {
	if legalEntityID != "" {
		return s.authorizeEntity(ctx, legalEntityID, "dimension", action)
	}
	return s.authorize(ctx, "dimension", action)
}

func (s *Service) dimensionValueIdentity(ctx context.Context, ds DimensionStore, id string) (*domain.DimensionValue, error) {
	v, err := ds.GetDimensionValue(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("store.GetDimensionValue: %w", err)
	}
	if v == nil {
		return nil, ErrNotFound
	}
	return v, nil
}

// CreateDimensionType is CreateDimensionType.
func (s *Service) CreateDimensionType(ctx context.Context, req domain.CreateDimensionTypeRequest) (*domain.DimensionType, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.authorize(ctx, "dimension.type", "create"); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	ds, err := s.dimensionStore()
	if err != nil {
		return nil, err
	}
	out, err := ds.CreateDimensionType(ctx, req, domain.PrincipalFromContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("store.CreateDimensionType: %w", err)
	}
	return out, nil
}

// ListDimensionTypes lists the tenant's dimension types.
func (s *Service) ListDimensionTypes(ctx context.Context) ([]domain.DimensionType, error) {
	ds, err := s.dimensionStore()
	if err != nil {
		return nil, err
	}
	out, err := ds.ListDimensionTypes(ctx)
	if err != nil {
		return nil, fmt.Errorf("store.ListDimensionTypes: %w", err)
	}
	return out, nil
}

// CreateDimensionValue is CreateDimensionValue.
func (s *Service) CreateDimensionValue(ctx context.Context, req domain.CreateDimensionValueRequest) (*DimensionOutcome, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.authorizeDimension(ctx, req.LegalEntityID, "create"); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	if req.LegalEntityID != "" {
		if err := s.assertEntityOperational(ctx, req.LegalEntityID); err != nil {
			return nil, err
		}
	}
	ds, err := s.dimensionStore()
	if err != nil {
		return nil, err
	}
	out, err := ds.CreateDimensionValue(ctx, req, domain.PrincipalFromContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("store.CreateDimensionValue: %w", err)
	}
	return out, nil
}

// ReparentDimensionValue is ReparentDimensionValue.
func (s *Service) ReparentDimensionValue(ctx context.Context, valueID string, req domain.ReparentDimensionValueRequest) (*DimensionOutcome, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	ds, err := s.dimensionStore()
	if err != nil {
		return nil, err
	}
	v, err := s.dimensionValueIdentity(ctx, ds, valueID)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeDimension(ctx, v.LegalEntityID, "reparent"); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	out, err := ds.ProposeHierarchyChange(ctx, valueID, req, domain.PrincipalFromContext(ctx), s.expectedVersionOptional)
	if err != nil {
		return nil, fmt.Errorf("store.ProposeHierarchyChange: %w", err)
	}
	return out, nil
}

// ApproveDimensionHierarchy approves a pending hierarchy change of a controlled type.
func (s *Service) ApproveDimensionHierarchy(ctx context.Context, valueID string, versionNo int, req domain.ApproveDimensionHierarchyRequest) (*DimensionOutcome, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	ds, err := s.dimensionStore()
	if err != nil {
		return nil, err
	}
	v, err := s.dimensionValueIdentity(ctx, ds, valueID)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeDimension(ctx, v.LegalEntityID, "approve-hierarchy"); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	out, err := ds.ApproveHierarchyChange(ctx, valueID, versionNo, req, domain.PrincipalFromContext(ctx), s.expectedVersionOptional)
	if err != nil {
		return nil, fmt.Errorf("store.ApproveHierarchyChange: %w", err)
	}
	return out, nil
}

// ChangeDimensionStatus is ActivateDimensionValue and DeactivateDimensionValue: to is ACTIVE or INACTIVE.
func (s *Service) ChangeDimensionStatus(ctx context.Context, valueID, to string, req domain.DimensionStatusRequest) (*DimensionOutcome, error) {
	command, action := "", ""
	switch to {
	case domain.DimActive:
		command, action = "ActivateDimensionValue", "activate"
	case domain.DimInactive:
		command, action = "DeactivateDimensionValue", "deactivate"
	default:
		return nil, fmt.Errorf("%w: a dimension value is activated or deactivated by command", ErrInvalidInput)
	}
	if err := req.ValidateFor(to); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	ds, err := s.dimensionStore()
	if err != nil {
		return nil, err
	}
	v, err := s.dimensionValueIdentity(ctx, ds, valueID)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeDimension(ctx, v.LegalEntityID, action); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	out, err := ds.ChangeDimensionStatus(ctx, valueID, command, to, req, domain.PrincipalFromContext(ctx), s.expectedVersionOptional)
	if err != nil {
		return nil, fmt.Errorf("store.ChangeDimensionStatus: %w", err)
	}
	return out, nil
}

// MapSourceObject is MapSourceObject.
func (s *Service) MapSourceObject(ctx context.Context, valueID string, req domain.MapSourceObjectRequest) (*MappingOutcome, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	ds, err := s.dimensionStore()
	if err != nil {
		return nil, err
	}
	v, err := s.dimensionValueIdentity(ctx, ds, valueID)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeDimension(ctx, v.LegalEntityID, "map-source"); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	out, err := ds.MapSourceObject(ctx, valueID, req, domain.PrincipalFromContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("store.MapSourceObject: %w", err)
	}
	return out, nil
}

// GetDimensionValue is GetDimensionValue: the value as it was on asOf (now when zero).
func (s *Service) GetDimensionValue(ctx context.Context, valueID string, asOf time.Time, history bool) (*domain.DimensionValueView, error) {
	ds, err := s.dimensionStore()
	if err != nil {
		return nil, err
	}
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}
	v, err := ds.GetDimensionValueView(ctx, valueID, asOf, history)
	if err != nil {
		return nil, fmt.Errorf("store.GetDimensionValueView: %w", err)
	}
	if v == nil {
		return nil, ErrNotFound
	}
	return v, nil
}

// GetHierarchyAsOf is GetHierarchyAsOf: the type's hierarchy as it stood on asOf.
func (s *Service) GetHierarchyAsOf(ctx context.Context, typeCode, legalEntityID string, asOf time.Time) (*domain.HierarchyAsOf, error) {
	ds, err := s.dimensionStore()
	if err != nil {
		return nil, err
	}
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}
	out, err := ds.GetHierarchyAsOf(ctx, typeCode, legalEntityID, asOf)
	if err != nil {
		return nil, fmt.Errorf("store.GetHierarchyAsOf: %w", err)
	}
	return out, nil
}

// ListPostingEligibleValues is ListPostingEligibleValues.
func (s *Service) ListPostingEligibleValues(ctx context.Context, typeCode, legalEntityID string, asOf time.Time) ([]EligibleDimensionValue, error) {
	ds, err := s.dimensionStore()
	if err != nil {
		return nil, err
	}
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}
	out, err := ds.ListPostingEligibleValues(ctx, typeCode, legalEntityID, asOf)
	if err != nil {
		return nil, fmt.Errorf("store.ListPostingEligibleValues: %w", err)
	}
	return out, nil
}

// ResolveDimension is ResolveDimension.
func (s *Service) ResolveDimension(ctx context.Context, q domain.ResolveDimensionQuery) (*domain.DimensionResolution, error) {
	if err := q.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	ds, err := s.dimensionStore()
	if err != nil {
		return nil, err
	}
	if q.AsOf.IsZero() {
		q.AsOf = time.Now().UTC()
	}
	out, err := ds.ResolveDimension(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store.ResolveDimension: %w", err)
	}
	return out, nil
}
