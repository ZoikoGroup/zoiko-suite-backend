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

// ── INV-03 Inventory Movement ────────────────────────────────────────────────
//
// See migration 000003's doc comment for the full state-model/command
// mapping and all four negative-path enforcement mechanisms — including
// the two negative paths INV-01/INV-02 each deferred to this capability.

const (
	MovementTypeReceipt      = "RECEIPT"
	MovementTypeIssue        = "ISSUE"
	MovementTypeTransfer     = "TRANSFER"
	MovementTypeAdjustment   = "ADJUSTMENT"
	MovementTypeReversal     = "REVERSAL"
	MovementTypeSupersession = "SUPERSESSION"

	MovementStatusDraft     = "DRAFT"
	MovementStatusValidated = "VALIDATED"
	MovementStatusCommitted = "COMMITTED"
)

// InventoryMovement is INV-03's own authority — "InventoryMovement;
// movement line; source/destination; quantity/UOM; item/lot/serial;
// business/effective/posting dates; source reference; movement type;
// reversal/supersession link." Quantity is always a positive magnitude;
// direction is implied by movement_type and which of
// Source/DestinationLocationID is set — see migration 000003's doc
// comment for the full mapping.
type InventoryMovement struct {
	MovementID    string `json:"movement_id"`
	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`
	MovementType  string `json:"movement_type"`
	Status        string `json:"status"`

	ItemID                string    `json:"item_id"`
	SourceLocationID      *string   `json:"source_location_id,omitempty"`
	DestinationLocationID *string   `json:"destination_location_id,omitempty"`
	Quantity              float64   `json:"quantity"`
	UOM                   string    `json:"uom"`
	LotNumber             *string   `json:"lot_number,omitempty"`
	SerialNumber          *string   `json:"serial_number,omitempty"`
	SourceReference       string    `json:"source_reference"`
	SourceIdempotencyKey  string    `json:"source_idempotency_key"`
	BusinessDate          time.Time `json:"business_date"`
	FiscalPeriod          string    `json:"fiscal_period"`
	ReversesMovementID    *string   `json:"reverses_movement_id,omitempty"`
	SupersedesMovementID  *string   `json:"supersedes_movement_id,omitempty"`
	Reason                *string   `json:"reason,omitempty"`

	CreatedAt              time.Time  `json:"created_at"`
	CreatedByPrincipalID   string     `json:"created_by_principal_id"`
	ValidatedAt            *time.Time `json:"validated_at,omitempty"`
	CommittedAt            *time.Time `json:"committed_at,omitempty"`
	CommittedByPrincipalID *string    `json:"committed_by_principal_id,omitempty"`
}

// ── Request types ────────────────────────────────────────────────────────

// CreateInventoryMovementRequest is the one real create path every named
// command (CreateInventoryMovement and the four type-specific wrappers)
// funnels through — see migration 000003's doc comment.
type CreateInventoryMovementRequest struct {
	MovementType          string     `json:"movement_type,omitempty"` // required for CreateInventoryMovement; pre-filled by the type-specific wrappers
	ItemID                string     `json:"item_id"`
	SourceLocationID      string     `json:"source_location_id,omitempty"`
	DestinationLocationID string     `json:"destination_location_id,omitempty"`
	Quantity              float64    `json:"quantity"`
	UOM                   string     `json:"uom"`
	LotNumber             string     `json:"lot_number,omitempty"`
	SerialNumber          string     `json:"serial_number,omitempty"`
	SourceReference       string     `json:"source_reference"`
	SourceIdempotencyKey  string     `json:"source_idempotency_key"`
	BusinessDate          *time.Time `json:"business_date,omitempty"`
	FiscalPeriod          string     `json:"fiscal_period"`
	Reason                string     `json:"reason,omitempty"`
}

type ReverseMovementRequest struct {
	Reason string `json:"reason"`
}

type SupersedeMovementRequest struct {
	Reason string `json:"reason"`
}

// ── Errors ───────────────────────────────────────────────────────────────

var (
	ErrMovementNotFound = errorString("inventory movement not found")

	ErrInvalidMovementTransition = errorString("inventory movement is not in a status that allows this action")

	ErrMovementTypeRequired = errorString("movement_type is required")

	// ErrDuplicateIdempotencyKey signals the store already has a movement
	// under this key — the caller-facing handler resolves this to the
	// EXISTING movement rather than an error, per the spec's own failure
	// semantics: "Duplicate idempotency key returns original result."
	ErrDuplicateIdempotencyKey = errorString("a movement already exists for this source_idempotency_key")

	ErrReceiptRequiresDestinationOnly = errorString("a RECEIPT movement requires destination_location_id and must not set source_location_id")
	ErrIssueRequiresSourceOnly        = errorString("an ISSUE movement requires source_location_id and must not set destination_location_id")
	ErrTransferRequiresBothLocations  = errorString("a TRANSFER movement requires both source_location_id and destination_location_id")
	ErrAdjustmentRequiresOneLocation  = errorString("an ADJUSTMENT movement requires exactly one of source_location_id (decrease) or destination_location_id (increase)")

	// ErrLotIdentityRequired is INV-01's own deferred negative path,
	// "Lot-tracked item moved without lot identity" — enforced here, at
	// the one place stock actually moves.
	ErrLotIdentityRequired = errorString("this item's tracking policy requires a lot_number for every movement")

	// ErrSerialIdentityRequired is the serial-tracking half of the same
	// deferred negative path.
	ErrSerialIdentityRequired = errorString("this item's tracking policy requires a serial_number for every movement")

	// ErrLocationNotEligible is INV-02's own deferred negative path,
	// "Movement enters retired location" (generalized to any non-ACTIVE
	// location) — enforced here, at the one place stock actually moves.
	ErrLocationNotEligible = errorString("source and destination locations must both be ACTIVE")

	// ErrItemNotEligibleForMovement covers a movement against an item
	// that is not currently ACTIVE.
	ErrItemNotEligibleForMovement = errorString("item must be ACTIVE to be moved")

	// ErrUOMMismatch is the spec's own negative path, "UOM ambiguity
	// blocks commit" — this v1's real, simple enforcement: a movement's
	// own uom must exactly match the item's base_uom (no REF UOM
	// conversion service exists yet to reconcile a mismatch safely).
	ErrUOMMismatch = errorString("movement uom does not match the item's own base_uom")

	// ErrSerialAlreadyResident and ErrSerialNotAtSourceLocation are the
	// spec's own negative path, "Serial-numbered unit appears in two
	// locations."
	ErrSerialAlreadyResident     = errorString("this serial number already has a residency record — it cannot be received again without first leaving the tracked estate")
	ErrSerialNotAtSourceLocation = errorString("this serial number does not currently reside at the claimed source location")

	// ErrNegativeStockNotAllowed is the spec's own negative path,
	// "Negative stock allowed despite policy prohibition" — no
	// per-item/location override exists in this v1, refused universally.
	ErrNegativeStockNotAllowed = errorString("this movement would take on-hand quantity below zero")

	ErrPeriodCheckUnavailable = errorString("financial-close-svc unavailable")
	ErrPeriodLocked           = errorString("cannot commit a movement into a LOCKED fiscal period")
)

// ── INV-04 Inventory Valuation ───────────────────────────────────────────────
//
// See migration 000004's doc comment for the full state-model/command
// mapping, the scope-narrowing decision, and all four negative-path
// enforcement mechanisms.

const (
	ValuationEntryTypeInbound  = "INBOUND"
	ValuationEntryTypeOutbound = "OUTBOUND"

	ValuationRunStatusDraft                  = "DRAFT"
	ValuationRunStatusPopulationFrozen       = "POPULATION_FROZEN"
	ValuationRunStatusApproved               = "APPROVED"
	ValuationRunStatusAccountingEventEmitted = "ACCOUNTING_EVENT_EMITTED"

	WriteDownStatusAccountingEventEmitted = "ACCOUNTING_EVENT_EMITTED"
	WriteDownStatusReversed               = "REVERSED"
)

// CostLayer is INV-04's own "InventoryCostLayer/Pool" — one row per
// INBOUND movement, created exactly once (UNIQUE(tenant_id,
// source_movement_id)).
type CostLayer struct {
	LayerID           string    `json:"layer_id"`
	ItemID            string    `json:"item_id"`
	LocationID        string    `json:"location_id"`
	SourceMovementID  string    `json:"source_movement_id"`
	OriginalQuantity  float64   `json:"original_quantity"`
	RemainingQuantity float64   `json:"remaining_quantity"`
	UnitCost          float64   `json:"unit_cost"`
	CreatedAt         time.Time `json:"created_at"`
}

// ValuationEntry is INV-04's own "ValuationEntry" — exactly one row per
// movement, ever (UNIQUE(tenant_id, movement_id)).
type ValuationEntry struct {
	EntryID              string    `json:"entry_id"`
	LegalEntityID        string    `json:"legal_entity_id"`
	ItemID               string    `json:"item_id"`
	LocationID           string    `json:"location_id"`
	MovementID           string    `json:"movement_id"`
	EntryType            string    `json:"entry_type"`
	Quantity             float64   `json:"quantity"`
	Value                float64   `json:"value"`
	ValuationMethod      string    `json:"valuation_method"`
	FiscalPeriod         string    `json:"fiscal_period"`
	RunID                *string   `json:"run_id,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
}

// LayerConsumption is the spec's own named "cost-layer trace" evidence —
// which layers, and how much of each, an OUTBOUND entry drew from.
type LayerConsumption struct {
	ConsumptionID         string    `json:"consumption_id"`
	ValuationEntryID      string    `json:"valuation_entry_id"`
	LayerID               string    `json:"layer_id"`
	QuantityConsumed      float64   `json:"quantity_consumed"`
	UnitCostAtConsumption float64   `json:"unit_cost_at_consumption"`
	CreatedAt             time.Time `json:"created_at"`
}

// ValuationRun mirrors AST-02's own DepreciationRun — one batch GL
// posting per (legal_entity, fiscal_period).
type ValuationRun struct {
	RunID                 string     `json:"run_id"`
	LegalEntityID         string     `json:"legal_entity_id"`
	FiscalPeriod          string     `json:"fiscal_period"`
	InventoryAccountCode  string     `json:"inventory_account_code"`
	COGSAccountCode       string     `json:"cogs_account_code"`
	Status                string     `json:"status"`
	JournalID             *string    `json:"journal_id,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
	CreatedByPrincipalID  string     `json:"created_by_principal_id"`
	ApprovedAt            *time.Time `json:"approved_at,omitempty"`
	ApprovedByPrincipalID *string    `json:"approved_by_principal_id,omitempty"`
	EmittedAt             *time.Time `json:"emitted_at,omitempty"`

	Entries []ValuationEntry `json:"entries,omitempty"`
}

// WriteDown is RecordInventoryWriteDown/ReverseWriteDown's own authority.
// ValuationEvidenceRef is required — negative path #3, "NRV write-down
// lacks evidence."
type WriteDown struct {
	WriteDownID           string     `json:"write_down_id"`
	LegalEntityID         string     `json:"legal_entity_id"`
	ItemID                string     `json:"item_id"`
	LocationID            string     `json:"location_id"`
	Amount                float64    `json:"amount"`
	ValuationEvidenceRef  string     `json:"valuation_evidence_ref"`
	ExpenseAccountCode    string     `json:"expense_account_code"`
	InventoryAccountCode  string     `json:"inventory_account_code"`
	JournalID             *string    `json:"journal_id,omitempty"`
	Status                string     `json:"status"`
	CreatedAt             time.Time  `json:"created_at"`
	CreatedByPrincipalID  string     `json:"created_by_principal_id"`
	ReversedAt            *time.Time `json:"reversed_at,omitempty"`
	ReversedByPrincipalID *string    `json:"reversed_by_principal_id,omitempty"`
	ReversalReason        *string    `json:"reversal_reason,omitempty"`
}

// ── Request types ────────────────────────────────────────────────────────

// ValueMovementRequest supplies caller-declared cost inputs — no AP cost
// source or REF FX service exists yet, the same bootstrap-gap posture
// used throughout this session. UnitCost is required for an INBOUND
// movement (the layer's own cost) and for a STANDARD_COST-method
// OUTBOUND movement (no standard-cost master exists yet); it is ignored
// for FIFO/WEIGHTED_AVERAGE OUTBOUND movements, whose cost is always
// derived from existing layers.
type ValueMovementRequest struct {
	UnitCost *float64 `json:"unit_cost,omitempty"`
}

type CreateValuationRunRequest struct {
	LegalEntityID        string `json:"legal_entity_id"`
	FiscalPeriod         string `json:"fiscal_period"`
	InventoryAccountCode string `json:"inventory_account_code"`
	COGSAccountCode      string `json:"cogs_account_code"`
}

type RecordWriteDownRequest struct {
	ItemID               string  `json:"item_id"`
	LocationID           string  `json:"location_id"`
	Amount               float64 `json:"amount"`
	ValuationEvidenceRef string  `json:"valuation_evidence_ref"`
	ExpenseAccountCode   string  `json:"expense_account_code"`
	InventoryAccountCode string  `json:"inventory_account_code"`
	FiscalPeriod         string  `json:"fiscal_period"`
}

type ReverseWriteDownRequest struct {
	Reason string `json:"reason"`
}

// ── Errors ───────────────────────────────────────────────────────────────

var (
	ErrValuationEntryNotFound = errorString("valuation entry not found")
	ErrValuationRunNotFound   = errorString("valuation run not found")
	ErrWriteDownNotFound      = errorString("write-down not found")

	// ErrMovementAlreadyValued is the spec's own negative path, "Same
	// movement consumes two cost layers twice" — enforced structurally by
	// UNIQUE(tenant_id, movement_id) on inventory_valuation_entries; a
	// second ValueMovement call against the same movement is refused, not
	// silently repeated.
	ErrMovementAlreadyValued = errorString("this movement has already been valued")

	ErrMovementNotCommitted = errorString("only a COMMITTED movement can be valued")

	// ErrUnitCostRequired covers both an INBOUND movement missing its own
	// unit_cost and a STANDARD_COST-method OUTBOUND movement missing its
	// own caller-declared standard cost.
	ErrUnitCostRequired = errorString("unit_cost is required for this movement")

	// ErrInsufficientCostLayers is a real integrity guard distinct from
	// INV-03's own negative-stock check: INV-03 blocks a movement that
	// would take PHYSICAL on-hand negative; this blocks valuing an
	// OUTBOUND movement whose item/location has no valued cost layers
	// covering it — a legitimate gap when, e.g., opening balances were
	// never valued.
	ErrInsufficientCostLayers = errorString("insufficient valued cost layers to cover this movement's quantity")

	ErrInvalidRunTransition = errorString("valuation run is not in a status that allows this action")

	ErrValuationRunAlreadyExistsForPeriod = errorString("a live valuation run already exists for this legal entity and fiscal period")

	ErrEmptyValuationPopulation = errorString("no unclaimed FINAL valuation entries were found to freeze into this run")

	ErrSelfApprovalNotPermittedRun = errorString("the principal who created this valuation run may not also approve it")

	// ErrValuationEvidenceRequired is the spec's own negative path, "NRV
	// write-down lacks evidence."
	ErrValuationEvidenceRequired = errorString("a write-down requires a recorded valuation_evidence_ref")

	ErrSelfWriteDownReversalNotPermitted = errorString("the principal who recorded this write-down may not also reverse it")

	ErrWriteDownAlreadyReversed = errorString("write-down has already been reversed")
)

// ── INV-05 Stock Count ────────────────────────────────────────────────────────
//
// See migration 000005's doc comment for the full state-model/command
// mapping and all four negative-path enforcement mechanisms.

const (
	StockCountStatusPlanned              = "PLANNED"
	StockCountStatusPopulationFrozen     = "POPULATION_FROZEN"
	StockCountStatusCounting             = "COUNTING"
	StockCountStatusVarianceReview       = "VARIANCE_REVIEW"
	StockCountStatusAdjustmentsGenerated = "ADJUSTMENTS_GENERATED"
	StockCountStatusCertified            = "CERTIFIED"
	StockCountStatusCancelled            = "CANCELLED"

	CountLineStatusPending             = "PENDING"
	CountLineStatusCounted             = "COUNTED"
	CountLineStatusNeedsRecount        = "NEEDS_RECOUNT"
	CountLineStatusVarianceApproved    = "VARIANCE_APPROVED"
	CountLineStatusAdjustmentGenerated = "ADJUSTMENT_GENERATED"
)

// StockCount is INV-05's own authority — "StockCountSession; count
// population/snapshot; CountLine; observed quantity; recount; variance;
// reason; approval; adjustment proposal; completion certificate."
type StockCount struct {
	CountID                string     `json:"count_id"`
	LegalEntityID          string     `json:"legal_entity_id"`
	FiscalPeriod           string     `json:"fiscal_period"`
	Status                 string     `json:"status"`
	CutoffAt               *time.Time `json:"cutoff_at,omitempty"`
	CreatedAt              time.Time  `json:"created_at"`
	CreatedByPrincipalID   string     `json:"created_by_principal_id"`
	FrozenAt               *time.Time `json:"frozen_at,omitempty"`
	CertifiedAt            *time.Time `json:"certified_at,omitempty"`
	CertifiedByPrincipalID *string    `json:"certified_by_principal_id,omitempty"`
	CancelledAt            *time.Time `json:"cancelled_at,omitempty"`
	CancelledByPrincipalID *string    `json:"cancelled_by_principal_id,omitempty"`
	CancelReason           *string    `json:"cancel_reason,omitempty"`

	LocationIDs []string         `json:"location_ids,omitempty"`
	Lines       []StockCountLine `json:"lines,omitempty"`
}

// StockCountLine is INV-05's own "CountLine" — one row per (item,
// location) in the frozen population. SystemQuantity is set exactly
// once, at freeze time, and never mutated again — see migration
// 000005's doc comment on negative path #2.
type StockCountLine struct {
	LineID                        string     `json:"line_id"`
	CountID                       string     `json:"count_id"`
	ItemID                        string     `json:"item_id"`
	LocationID                    string     `json:"location_id"`
	SystemQuantity                float64    `json:"system_quantity"`
	AssignedCounterPrincipalID    *string    `json:"assigned_counter_principal_id,omitempty"`
	ObservedQuantity              *float64   `json:"observed_quantity,omitempty"`
	ObservedAt                    *time.Time `json:"observed_at,omitempty"`
	ObservedByPrincipalID         *string    `json:"observed_by_principal_id,omitempty"`
	Status                        string     `json:"status"`
	VarianceApprovedAt            *time.Time `json:"variance_approved_at,omitempty"`
	VarianceApprovedByPrincipalID *string    `json:"variance_approved_by_principal_id,omitempty"`
	AdjustmentMovementID          *string    `json:"adjustment_movement_id,omitempty"`
	CreatedAt                     time.Time  `json:"created_at"`
}

// ── Request types ────────────────────────────────────────────────────────

type CreateStockCountRequest struct {
	LegalEntityID string   `json:"legal_entity_id"`
	FiscalPeriod  string   `json:"fiscal_period"`
	LocationIDs   []string `json:"location_ids"`
}

// RecordedBlindCount is RecordBlindCount's own response shape — it
// deliberately has NO field for system_quantity at all, the real
// enforcement of negative path #1, "Counter sees system quantity in
// blind count." Not a runtime redaction of StockCountLine; a distinct
// type that structurally cannot carry it.
type RecordedBlindCount struct {
	LineID           string    `json:"line_id"`
	ItemID           string    `json:"item_id"`
	LocationID       string    `json:"location_id"`
	ObservedQuantity float64   `json:"observed_quantity"`
	ObservedAt       time.Time `json:"observed_at"`
}

type RecordBlindCountRequest struct {
	ObservedQuantity float64 `json:"observed_quantity"`
}

type AssignCounterRequest struct {
	CounterPrincipalID string `json:"counter_principal_id"`
}

type CancelStockCountRequest struct {
	Reason string `json:"reason"`
}

// ── Errors ───────────────────────────────────────────────────────────────

var (
	ErrStockCountNotFound         = errorString("stock count not found")
	ErrCountLineNotFound          = errorString("stock count line not found")
	ErrInvalidCountTransition     = errorString("stock count is not in a status that allows this action")
	ErrInvalidCountLineTransition = errorString("stock count line is not in a status that allows this action")

	ErrEmptyCountPopulation = errorString("no eligible item/location combinations were found to freeze into this count")

	// ErrSelfVarianceApprovalNotPermitted is the spec's own SoD,
	// "Counter cannot approve own material variance" — enforced at the
	// LINE level: the approving principal must differ from that specific
	// line's own observed_by_principal_id.
	ErrSelfVarianceApprovalNotPermitted = errorString("the principal who recorded this line's count may not also approve its variance")

	ErrLineNotYetCounted = errorString("this line has not been counted yet")

	ErrNoVarianceApprovedLines = errorString("no variance-approved lines were found to generate adjustments for")
)
