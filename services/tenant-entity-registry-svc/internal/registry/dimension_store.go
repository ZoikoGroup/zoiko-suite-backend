package registry

import (
	"context"
	"time"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

// DimensionStore is the data-access contract for REF-08 financial dimensions (migration 000024). Asserted by the service
// like PartyStore. Every method runs in the caller's tenant under row-level security.
type DimensionStore interface {
	CreateDimensionType(ctx context.Context, req domain.CreateDimensionTypeRequest, actor string) (*domain.DimensionType, error)
	ListDimensionTypes(ctx context.Context) ([]domain.DimensionType, error)

	// CreateDimensionValue opens a DRAFT value with its first hierarchy version. A code is stable within its scope: unique
	// per type, per entity for an entity's own values, and an entity's code never shadows a tenant-wide one (ErrConflict).
	CreateDimensionValue(ctx context.Context, req domain.CreateDimensionValueRequest, actor string) (*DimensionOutcome, error)
	// ProposeHierarchyChange is ReparentDimensionValue: the next hierarchy version (a new parent, or root, and optionally a
	// new label), prospective and loop-free. For a controlled type it is PENDING until someone else approves it.
	ProposeHierarchyChange(ctx context.Context, valueID string, req domain.ReparentDimensionValueRequest, actor string, allowUnversioned bool) (*DimensionOutcome, error)
	// ApproveHierarchyChange takes a pending version into effect. Its proposer cannot approve it (ErrSelfApproval).
	ApproveHierarchyChange(ctx context.Context, valueID string, versionNo int, req domain.ApproveDimensionHierarchyRequest, actor string, allowUnversioned bool) (*DimensionOutcome, error)
	// ChangeDimensionStatus is ActivateDimensionValue or DeactivateDimensionValue.
	ChangeDimensionStatus(ctx context.Context, valueID, command, to string, req domain.DimensionStatusRequest, actor string, allowUnversioned bool) (*DimensionOutcome, error)
	// MapSourceObject maps an operational source object to a value, ending its previous mapping where this one begins.
	MapSourceObject(ctx context.Context, valueID string, req domain.MapSourceObjectRequest, actor string) (*MappingOutcome, error)

	// GetDimensionValue returns the value's identity; (nil, nil) when absent.
	GetDimensionValue(ctx context.Context, valueID string) (*domain.DimensionValue, error)
	// GetDimensionValueView returns the value as it was on asOf; (nil, nil) when absent.
	GetDimensionValueView(ctx context.Context, valueID string, asOf time.Time, history bool) (*domain.DimensionValueView, error)
	GetHierarchyAsOf(ctx context.Context, typeCode, legalEntityID string, asOf time.Time) (*domain.HierarchyAsOf, error)
	// ListPostingEligibleValues lists the values that could take a new posting on asOf, with any controlled exception.
	ListPostingEligibleValues(ctx context.Context, typeCode, legalEntityID string, asOf time.Time) ([]EligibleDimensionValue, error)
	// ResolveDimension finds a value by code or by operational source object; ErrNotFound when there is none.
	ResolveDimension(ctx context.Context, q domain.ResolveDimensionQuery) (*domain.DimensionResolution, error)
}

// DimensionOutcome is a value after a command, with the hierarchy version and status step it produced.
type DimensionOutcome struct {
	Value      domain.DimensionValue       `json:"value"`
	Hierarchy  *domain.HierarchyVersion    `json:"hierarchy,omitempty"`
	Transition *domain.DimensionTransition `json:"transition,omitempty"`
}

// MappingOutcome is a source mapping and the one it replaced.
type MappingOutcome struct {
	Mapping  domain.SourceMapping  `json:"mapping"`
	Previous *domain.SourceMapping `json:"previous,omitempty"`
	NoOp     bool                  `json:"-"`
}

// EligibleDimensionValue is a value that can take a new posting.
type EligibleDimensionValue struct {
	ValueID       string `json:"value_id"`
	Code          string `json:"code"`
	Label         string `json:"label"`
	LegalEntityID string `json:"legal_entity_id,omitempty"`
	// Exception is set when the value is inactive but a controlled exception still covers the date.
	Exception bool `json:"exception,omitempty"`
}
