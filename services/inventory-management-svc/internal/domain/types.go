// Package domain defines the authoritative domain types for
// inventory-management-svc — INV-01 (Item/Product Master) from
// ZS-SVC-G-001, the first of five capabilities this service will host
// (INV-01 through INV-05), co-located per that document's own §7:
// "Physical deployment may be consolidated, but each logical service
// retains separate authoritative facts, commands, states, evidence and
// certification requirements."
package domain

import "time"

// Item lifecycle states — INV-01's own state model (verbatim from spec):
// "Draft→Active→Suspended→Retired." See migration 000001's doc comment
// for why Suspended has no command that reaches it in this v1.
const (
	ItemStatusDraft   = "DRAFT"
	ItemStatusActive  = "ACTIVE"
	ItemStatusRetired = "RETIRED"
)

// ValidItemTransitions enumerates the only legal status moves this v1
// implements.
var ValidItemTransitions = map[string][]string{
	ItemStatusDraft:   {ItemStatusActive},
	ItemStatusActive:  {ItemStatusRetired},
	ItemStatusRetired: {},
}

const (
	ValuationMethodFIFO            = "FIFO"
	ValuationMethodWeightedAverage = "WEIGHTED_AVERAGE"
	ValuationMethodStandardCost    = "STANDARD_COST"
)

// InventoryItem is INV-01's own authority — "InventoryItem; item/SKU
// identity; stocking/base UOM; item type; lot/serial/expiry policy;
// inventory classification; entity inventory profile; valuation-policy
// reference; lifecycle." BaseUOM is set once at creation and never
// exposed as an editable field of AmendInventoryProfile — see migration
// 000001's doc comment on negative path #2.
type InventoryItem struct {
	ItemID        string `json:"item_id"`
	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`
	SKU           string `json:"sku"`
	Description   string `json:"description"`
	BaseUOM       string `json:"base_uom"`
	ItemType      string `json:"item_type,omitempty"`

	PhysicalCharacteristics string  `json:"physical_characteristics,omitempty"`
	CatalogItemID           *string `json:"catalog_item_id,omitempty"`

	Status string `json:"status"`

	CreatedAt              time.Time  `json:"created_at"`
	CreatedByPrincipalID   string     `json:"created_by_principal_id"`
	ActivatedAt            *time.Time `json:"activated_at,omitempty"`
	ActivatedByPrincipalID *string    `json:"activated_by_principal_id,omitempty"`
	RetiredAt              *time.Time `json:"retired_at,omitempty"`
	RetiredByPrincipalID   *string    `json:"retired_by_principal_id,omitempty"`
	RetirementReason       *string    `json:"retirement_reason,omitempty"`
}

// TrackingPolicy is INV-01's own "lot/serial/expiry policy" — versioned,
// effective-dated, never mutated in place. INV-01 records it; enforcing
// it at movement time is INV-03's own future authority (see migration
// 000001's doc comment on negative path #3).
type TrackingPolicy struct {
	PolicyVersionID string `json:"policy_version_id"`
	PolicyID        string `json:"policy_id"`
	Version         int    `json:"version"`
	ItemID          string `json:"item_id"`

	RequiresLotTracking    bool `json:"requires_lot_tracking"`
	RequiresSerialTracking bool `json:"requires_serial_tracking"`
	RequiresExpiryTracking bool `json:"requires_expiry_tracking"`

	EffectiveFrom time.Time  `json:"effective_from"`
	EffectiveTo   *time.Time `json:"effective_to,omitempty"`

	CreatedAt            time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
}

// ValuationPolicy is INV-01's own "valuation-policy reference" —
// versioned, effective-dated, and — per SetValuationPolicyFutureEffective's
// own name — only ever created with a future EffectiveFrom, never
// retroactively. See migration 000001's doc comment.
type ValuationPolicy struct {
	PolicyVersionID string `json:"policy_version_id"`
	PolicyID        string `json:"policy_id"`
	Version         int    `json:"version"`
	ItemID          string `json:"item_id"`

	ValuationMethod string `json:"valuation_method"`

	EffectiveFrom time.Time  `json:"effective_from"`
	EffectiveTo   *time.Time `json:"effective_to,omitempty"`

	CreatedAt            time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
}

// ── Request types ────────────────────────────────────────────────────────

type CreateInventoryItemRequest struct {
	LegalEntityID           string `json:"legal_entity_id"`
	SKU                     string `json:"sku"`
	Description             string `json:"description"`
	BaseUOM                 string `json:"base_uom"`
	ItemType                string `json:"item_type,omitempty"`
	PhysicalCharacteristics string `json:"physical_characteristics,omitempty"`
}

// AmendInventoryProfileRequest deliberately excludes BaseUOM and SKU —
// neither is ever editable after creation. See migration 000001's doc
// comment on negative path #2.
type AmendInventoryProfileRequest struct {
	Description             *string `json:"description,omitempty"`
	ItemType                *string `json:"item_type,omitempty"`
	PhysicalCharacteristics *string `json:"physical_characteristics,omitempty"`
}

type SetTrackingPolicyRequest struct {
	RequiresLotTracking    bool `json:"requires_lot_tracking"`
	RequiresSerialTracking bool `json:"requires_serial_tracking"`
	RequiresExpiryTracking bool `json:"requires_expiry_tracking"`
}

type SetValuationPolicyRequest struct {
	ValuationMethod string     `json:"valuation_method"`
	EffectiveFrom   *time.Time `json:"effective_from,omitempty"`
}

type RetireInventoryItemRequest struct {
	Reason string `json:"reason"`
}

type LinkCommercialCatalogItemRequest struct {
	CatalogItemID string `json:"catalog_item_id"`
}

// ── Errors ───────────────────────────────────────────────────────────────

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrItemNotFound            = errorString("inventory item not found")
	ErrIdentityMissing         = errorString("caller identity missing")
	ErrTenantScopeMismatch     = errorString("tenant scope mismatch")
	ErrAuthorizationDenied     = errorString("authorization denied for inventory management action")
	ErrAuthzServiceUnavailable = errorString("authorization-svc unavailable")
	ErrStoreUnavailable        = errorString("inventory management store unavailable")

	ErrInvalidItemTransition = errorString("inventory item is not in a status that allows this action")

	ErrDuplicateSKU = errorString("an item with this sku already exists for this legal entity")

	// ErrValuationPolicyRequiredForActivation is this v1's real
	// enforcement of the spec's own failure semantics, "Missing ...
	// valuation policy ... blocks stock movement/valuation" — collapsed
	// onto the one gate this service itself owns: an item cannot become
	// ACTIVE (and therefore eligible for movement) without a valuation
	// policy already assigned.
	ErrValuationPolicyRequiredForActivation = errorString("cannot activate an item with no valuation policy assigned")

	// ErrValuationPolicyMustBeFutureEffective is
	// SetValuationPolicyFutureEffective's own name enforced as a real
	// application check — the spec's own SoD, "Retroactive valuation-
	// policy change requires controlled migration/revaluation approval,"
	// has no in-place edit path to satisfy it any other way: a new
	// version is only ever accepted with an effective_from later than now.
	ErrValuationPolicyMustBeFutureEffective = errorString("valuation policy effective_from must be in the future")

	ErrReasonRequired = errorString("reason is required")
)

// ── INV-02 Inventory Location ────────────────────────────────────────────────
//
// See migration 000002's doc comment for the full state-model/command
// mapping and all four negative-path enforcement mechanisms.

const (
	LocationTypeWarehouse      = "WAREHOUSE"
	LocationTypeSite           = "SITE"
	LocationTypeBin            = "BIN"
	LocationTypeQuarantineArea = "QUARANTINE_AREA"
	LocationTypeTransit        = "TRANSIT"

	LocationStatusDraft      = "DRAFT"
	LocationStatusActive     = "ACTIVE"
	LocationStatusSuspended  = "SUSPENDED"
	LocationStatusQuarantine = "QUARANTINE"
	LocationStatusRetired    = "RETIRED"
)

// InventoryLocation is INV-02's own authority — "InventoryLocation;
// warehouse/site/bin/quarantine/transit location; parent hierarchy;
// custody entity; operational state; location type; counting/
// negative-stock attributes." LegalEntityID is set once at creation and
// never exposed as an editable field of any later command — see
// migration 000002's doc comment on negative path #1.
type InventoryLocation struct {
	LocationID      string `json:"location_id"`
	TenantID        string `json:"tenant_id"`
	LegalEntityID   string `json:"legal_entity_id"`
	LocationCode    string `json:"location_code"`
	LocationType    string `json:"location_type"`
	Description     string `json:"description,omitempty"`
	CustodianEntity string `json:"custodian_entity,omitempty"`

	Status string `json:"status"`

	CreatedAt                time.Time  `json:"created_at"`
	CreatedByPrincipalID     string     `json:"created_by_principal_id"`
	ActivatedAt              *time.Time `json:"activated_at,omitempty"`
	ActivatedByPrincipalID   *string    `json:"activated_by_principal_id,omitempty"`
	SuspendedAt              *time.Time `json:"suspended_at,omitempty"`
	SuspendedByPrincipalID   *string    `json:"suspended_by_principal_id,omitempty"`
	SuspensionReason         *string    `json:"suspension_reason,omitempty"`
	QuarantinedAt            *time.Time `json:"quarantined_at,omitempty"`
	QuarantinedByPrincipalID *string    `json:"quarantined_by_principal_id,omitempty"`
	QuarantineReason         *string    `json:"quarantine_reason,omitempty"`
	ReleasedAt               *time.Time `json:"released_at,omitempty"`
	ReleasedByPrincipalID    *string    `json:"released_by_principal_id,omitempty"`
	RetiredAt                *time.Time `json:"retired_at,omitempty"`
	RetiredByPrincipalID     *string    `json:"retired_by_principal_id,omitempty"`
	RetirementReason         *string    `json:"retirement_reason,omitempty"`
}

// LocationHierarchyVersion is INV-02's own "parent hierarchy" — versioned,
// effective-dated, never mutated in place. See migration 000002's doc
// comment: this is the real structural answer to "hierarchy changes
// versioned."
type LocationHierarchyVersion struct {
	HierarchyVersionID string  `json:"hierarchy_version_id"`
	Version            int     `json:"version"`
	LocationID         string  `json:"location_id"`
	ParentLocationID   *string `json:"parent_location_id,omitempty"`

	EffectiveFrom time.Time  `json:"effective_from"`
	EffectiveTo   *time.Time `json:"effective_to,omitempty"`

	CreatedAt            time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
}

// ── Request types ────────────────────────────────────────────────────────

type CreateInventoryLocationRequest struct {
	LegalEntityID    string `json:"legal_entity_id"`
	LocationCode     string `json:"location_code"`
	LocationType     string `json:"location_type"`
	Description      string `json:"description,omitempty"`
	CustodianEntity  string `json:"custodian_entity,omitempty"`
	ParentLocationID string `json:"parent_location_id,omitempty"`
}

// AmendLocationMetadataRequest deliberately excludes LegalEntityID and
// LocationType — neither is ever editable after creation. See migration
// 000002's doc comment on negative path #1.
type AmendLocationMetadataRequest struct {
	Description     *string `json:"description,omitempty"`
	CustodianEntity *string `json:"custodian_entity,omitempty"`
}

type SuspendLocationRequest struct {
	Reason string `json:"reason"`
}

type ReparentLocationRequest struct {
	NewParentLocationID string `json:"new_parent_location_id"`
}

type SetQuarantineStateRequest struct {
	Quarantine bool   `json:"quarantine"`
	Reason     string `json:"reason"`
}

type RetireLocationRequest struct {
	Reason string `json:"reason"`
}

// ── Errors ───────────────────────────────────────────────────────────────

var (
	ErrLocationNotFound = errorString("inventory location not found")

	ErrInvalidLocationTransition = errorString("inventory location is not in a status that allows this action")

	ErrDuplicateLocationCode = errorString("a location with this location_code already exists for this legal entity")

	// ErrReparentAcrossLegalEntities is the spec's own negative path,
	// "Physical location change silently changes legal owner" — the
	// enforcement half that lives at reparent time (the other half is
	// LegalEntityID's own structural immutability).
	ErrReparentAcrossLegalEntities = errorString("cannot reparent a location under a parent belonging to a different legal entity")

	// ErrCircularLocationHierarchy is the spec's own negative path,
	// "Circular warehouse/bin hierarchy created."
	ErrCircularLocationHierarchy = errorString("this reparent would create a circular location hierarchy")

	// ErrSelfQuarantineReleaseNotPermitted is the spec's own SoD,
	// "quarantine release can require independent approval" — the
	// principal releasing a quarantine must differ from the principal who
	// set it.
	ErrSelfQuarantineReleaseNotPermitted = errorString("the principal who quarantined this location may not also release it")

	ErrLocationNotQuarantined = errorString("location is not currently quarantined")
)
