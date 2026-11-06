package registry

import (
	"context"
	"time"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

// UnitStore is the data-access contract for REF-09 units of measure (migration 000025). Asserted by the service like
// PartyStore. Every method runs in the caller's tenant under row-level security.
type UnitStore interface {
	// CreateQuantityDimension creates a dimension with its base unit, which is ACTIVE at once: its factor is 1 by definition.
	CreateQuantityDimension(ctx context.Context, req domain.CreateQuantityDimensionRequest, actor string) (*DimensionWithBase, error)
	ListQuantityDimensions(ctx context.Context) ([]domain.QuantityDimension, error)

	// CreateUnit opens a DRAFT non-base unit of an existing dimension; it converts to nothing until a conversion is active.
	CreateUnit(ctx context.Context, req domain.CreateUnitRequest, actor string) (*UnitOutcome, error)
	// ProposeConversion records the next conversion version of a non-base unit. supersede is true to replace an active
	// conversion (prospective: it starts after the active one began and not before today); false is the unit's first. A
	// STANDARD conversion takes effect at once; any other waits PENDING for an independent approver.
	ProposeConversion(ctx context.Context, unitCode string, supersede bool, req domain.ProposeConversionRequest, actor string, allowUnversioned bool) (*UnitOutcome, error)
	// ApproveConversion takes a pending conversion into effect. Its proposer cannot approve it (ErrSelfApproval).
	ApproveConversion(ctx context.Context, unitCode string, versionNo int, req domain.ApproveConversionRequest, actor string, allowUnversioned bool) (*UnitOutcome, error)
	// RetireUnit rejects new use of the unit from today. A base unit retires only when nothing else in its dimension is live.
	RetireUnit(ctx context.Context, unitCode string, req domain.RetireUnitRequest, actor string, allowUnversioned bool) (*UnitOutcome, error)

	// GetUnit returns the unit as it was on asOf; (nil, nil) when absent.
	GetUnit(ctx context.Context, unitCode string, asOf time.Time, history bool) (*domain.UnitView, error)
	// GetConversionVersion returns one version; (nil, nil) when absent.
	GetConversionVersion(ctx context.Context, unitCode string, versionNo int) (*domain.ConversionVersion, error)
	ListUnitsByDimension(ctx context.Context, dimensionCode string) ([]domain.Unit, error)
	// ConvertQuantity converts exactly, on asOf. ErrDimensionMismatch before any arithmetic; ErrConversionMissing when a
	// unit has no approved conversion in force; ErrReferenceInvalid when a unit is retired on that date.
	ConvertQuantity(ctx context.Context, q domain.ConvertQuantityQuery) (*domain.ConversionResult, error)
}

// DimensionWithBase is a quantity dimension and the base unit it was created with.
type DimensionWithBase struct {
	Dimension domain.QuantityDimension `json:"dimension"`
	BaseUnit  domain.Unit              `json:"base_unit"`
}

// UnitOutcome is a unit after a command and the conversion version it produced or touched.
type UnitOutcome struct {
	Unit       domain.Unit               `json:"unit"`
	Conversion *domain.ConversionVersion `json:"conversion,omitempty"`
	Previous   *domain.ConversionVersion `json:"previous_conversion,omitempty"`
}
