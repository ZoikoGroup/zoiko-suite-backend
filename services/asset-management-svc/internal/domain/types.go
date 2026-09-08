// Package domain defines the authoritative domain types for
// asset-management-svc — AST-01 (Fixed Asset Register), AST-02
// (Depreciation), and AST-03 (Asset Event) from ZS-SVC-G-001, co-located
// in one deployable per that document's own §7: "Physical deployment may
// be consolidated, but each logical service retains separate
// authoritative facts, commands, states, evidence and certification
// requirements."
package domain

import "time"

// Fixed asset lifecycle states — AST-01's own state model (verbatim from
// spec): "Candidate→Reviewed→Registered→Capitalized/Active;
// Suspended/Disposed/HeldForSale are lifecycle dimensions driven by
// accepted events; book states tracked separately." See migration
// 000001's doc comment for why "Reviewed" collapses into "Registered"
// and Disposed/HeldForSale are not yet reachable (AST-03's own future
// authority).
const (
	AssetStatusCandidate  = "CANDIDATE"
	AssetStatusRegistered = "REGISTERED"
	AssetStatusActive     = "ACTIVE" // "Capitalized/Active" — one terminal state, reached via RequestCapitalization
	AssetStatusSuspended  = "SUSPENDED"
	AssetStatusMerged     = "MERGED"
	// AssetStatusDisposed is reached only through AST-03's ApplyAssetEvent
	// (a DISPOSAL event), never through any AST-01 command directly — see
	// migration 000003's doc comment. Deliberately left OUT of
	// ValidAssetTransitions below: that map backs GetAvailableActions,
	// which only ever advertises AST-01's own commands, and AST-01 has no
	// command that reaches or leaves DISPOSED.
	AssetStatusDisposed = "DISPOSED"
)

// ValidAssetTransitions enumerates the only legal status moves this v1
// implements directly through AST-01's own commands (AST-03's
// ApplyAssetEvent/ReverseAssetEvent/SupersedeAssetEvent move
// ACTIVE/SUSPENDED <-> DISPOSED directly in the store, bypassing this map
// — see internal/store/asset_event_store.go).
var ValidAssetTransitions = map[string][]string{
	AssetStatusCandidate:  {AssetStatusRegistered},
	AssetStatusRegistered: {AssetStatusActive},
	AssetStatusActive:     {AssetStatusSuspended, AssetStatusMerged},
	AssetStatusSuspended:  {AssetStatusActive},
	AssetStatusMerged:     {},
	AssetStatusDisposed:   {},
}

// FixedAsset is AST-01's own authority — "FixedAsset; AssetComponent;
// asset category/class; tag/serial; ownership entity; custodian/location
// references; acquisition/source references; in-service metadata;
// AssetBookProfile linkage; lifecycle projection. It does not own posted
// GL balances." Deliberately carries NO carrying/book-value field — the
// spec's own negative path, "User edits carrying value directly," is
// enforced structurally by there being no such column to edit.
type FixedAsset struct {
	AssetID       string `json:"asset_id"`
	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`
	AssetCategory string `json:"asset_category"`
	TagSerial     string `json:"tag_serial,omitempty"`
	Description   string `json:"description"`
	CustodianID   string `json:"custodian_id,omitempty"`
	LocationID    string `json:"location_id,omitempty"`

	// AcquisitionSourceRef is the spec's own required "approved
	// acquisition/capital project/source document" input — capitalization
	// is refused without it (ErrCapitalizationRequiresEvidence).
	AcquisitionSourceRef string     `json:"acquisition_source_ref,omitempty"`
	AcquisitionDate      *time.Time `json:"acquisition_date,omitempty"`
	InServiceDate        *time.Time `json:"in_service_date,omitempty"`

	Status            string  `json:"status"`
	MergedIntoAssetID *string `json:"merged_into_asset_id,omitempty"`
	SplitFromAssetID  *string `json:"split_from_asset_id,omitempty"`

	CreatedAt                time.Time  `json:"created_at"`
	CreatedByPrincipalID     string     `json:"created_by_principal_id"`
	RegisteredAt             *time.Time `json:"registered_at,omitempty"`
	RegisteredByPrincipalID  *string    `json:"registered_by_principal_id,omitempty"`
	CapitalizedAt            *time.Time `json:"capitalized_at,omitempty"`
	CapitalizedByPrincipalID *string    `json:"capitalized_by_principal_id,omitempty"`
	SuspendedAt              *time.Time `json:"suspended_at,omitempty"`
	SuspendedByPrincipalID   *string    `json:"suspended_by_principal_id,omitempty"`
	SuspensionReason         *string    `json:"suspension_reason,omitempty"`

	Components      []AssetComponent      `json:"components,omitempty"`
	BookAssignments []AssetBookAssignment `json:"book_assignments,omitempty"`
}

// AssetComponent is one physical/cost component of a FixedAsset — a
// child of the asset, never a separate top-level authority.
type AssetComponent struct {
	ComponentID          string    `json:"component_id"`
	AssetID              string    `json:"asset_id"`
	Description          string    `json:"description"`
	CostSourceRef        string    `json:"cost_source_ref,omitempty"`
	MovedToAssetID       *string   `json:"moved_to_asset_id,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
}

const (
	BookAssignmentStatusActive    = "ACTIVE"
	BookAssignmentStatusSuspended = "SUSPENDED"
)

// AssetBookAssignment is AST-01's own "AssetBookProfile linkage" —
// evidence of which accounting book(s) an asset is assigned to, and the
// useful-life/residual-value PROPOSALS captured at assignment time (real
// depreciation parameters belong to AST-02 alone). "Book states tracked
// separately" from the asset's own lifecycle — its own Status field.
type AssetBookAssignment struct {
	AssignmentID             string    `json:"assignment_id"`
	AssetID                  string    `json:"asset_id"`
	BookID                   string    `json:"book_id"`
	UsefulLifeMonthsProposal *int      `json:"useful_life_months_proposal,omitempty"`
	ResidualValueProposal    *float64  `json:"residual_value_proposal,omitempty"`
	Status                   string    `json:"status"`
	AssignedAt               time.Time `json:"assigned_at"`
	AssignedByPrincipalID    string    `json:"assigned_by_principal_id"`
}

// ── Request types ────────────────────────────────────────────────────────

type CreateAssetCandidateRequest struct {
	LegalEntityID        string     `json:"legal_entity_id"`
	AssetCategory        string     `json:"asset_category"`
	TagSerial            string     `json:"tag_serial,omitempty"`
	Description          string     `json:"description"`
	CustodianID          string     `json:"custodian_id,omitempty"`
	LocationID           string     `json:"location_id,omitempty"`
	AcquisitionSourceRef string     `json:"acquisition_source_ref,omitempty"`
	AcquisitionDate      *time.Time `json:"acquisition_date,omitempty"`
	InServiceDate        *time.Time `json:"in_service_date,omitempty"`
}

type AddComponentRequest struct {
	Description   string `json:"description"`
	CostSourceRef string `json:"cost_source_ref,omitempty"`
}

type AmendNonFinancialMetadataRequest struct {
	Description *string `json:"description,omitempty"`
	CustodianID *string `json:"custodian_id,omitempty"`
	LocationID  *string `json:"location_id,omitempty"`
	TagSerial   *string `json:"tag_serial,omitempty"`
}

type AssignAssetBookProfileRequest struct {
	BookID                   string   `json:"book_id"`
	UsefulLifeMonthsProposal *int     `json:"useful_life_months_proposal,omitempty"`
	ResidualValueProposal    *float64 `json:"residual_value_proposal,omitempty"`
}

type TransferCustodyRequest struct {
	CustodianID string `json:"custodian_id"`
}

type TransferLocationRequest struct {
	LocationID string `json:"location_id"`
}

type SuspendAssetRequest struct {
	Reason string `json:"reason"`
}

type MergeAssetRequest struct {
	// TargetAssetID is the surviving asset this one merges into. Both
	// must share the same legal_entity_id — the spec's own negative path,
	// "Physical asset merged across legal entities," refused otherwise.
	TargetAssetID string `json:"target_asset_id"`
	Reason        string `json:"reason"`
}

type SplitAssetRequest struct {
	// ComponentIDs names which of the source asset's components move to
	// the new asset. The new asset inherits the source's legal_entity_id
	// and asset_category by default.
	ComponentIDs []string `json:"component_ids"`
	Description  string   `json:"description"`
	Reason       string   `json:"reason"`
}

type RequestCapitalizationRequest struct{}

// ── Errors ───────────────────────────────────────────────────────────────

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrAssetNotFound           = errorString("fixed asset not found")
	ErrComponentNotFound       = errorString("asset component not found")
	ErrIdentityMissing         = errorString("caller identity missing")
	ErrTenantScopeMismatch     = errorString("tenant scope mismatch")
	ErrAuthorizationDenied     = errorString("authorization denied for asset management action")
	ErrAuthzServiceUnavailable = errorString("authorization-svc unavailable")
	ErrStoreUnavailable        = errorString("asset management store unavailable")

	// ErrInvalidAssetTransition covers every AST-01 lifecycle action for
	// an asset not currently in the one status that action requires —
	// matches the spec's own forward-only state model.
	ErrInvalidAssetTransition = errorString("fixed asset is not in a status that allows this action")

	// ErrCapitalizationRequiresEvidence is the spec's own negative path,
	// "Asset capitalized without source evidence" — RequestCapitalization
	// refuses without a recorded acquisition_source_ref.
	ErrCapitalizationRequiresEvidence = errorString("capitalization requires a recorded acquisition_source_ref")

	// ErrMergeAcrossLegalEntities is the spec's own negative path,
	// "Physical asset merged across legal entities."
	ErrMergeAcrossLegalEntities = errorString("cannot merge assets across different legal entities")

	// ErrRetroactiveBookProfileChange is the spec's own negative path,
	// "Retroactive book profile change rewrites prior depreciation" —
	// AssignAssetBookProfile refuses to re-target an existing assignment
	// once the asset is ACTIVE; a book reassignment on a capitalized
	// asset would silently invalidate whatever depreciation AST-02 has
	// already run against the prior assignment.
	ErrRetroactiveBookProfileChange = errorString("cannot change an existing book assignment once the asset is ACTIVE — assign a new book or suspend the asset first")

	ErrNoComponentsNamed   = errorString("split requires at least one component_id")
	ErrComponentNotOnAsset = errorString("named component does not belong to the source asset")
	ErrReasonRequired      = errorString("reason is required")
)

// ── AST-02 Depreciation ─────────────────────────────────────────────────────
//
// See migration 000002's doc comment for the full state-model/command
// mapping and why several named states have no command that reaches them
// independently in this v1.

const (
	DepreciationMethodStraightLine = "STRAIGHT_LINE"

	DepreciationScheduleStatusActive     = "ACTIVE"
	DepreciationScheduleStatusSuperseded = "SUPERSEDED"

	DepreciationRunStatusDraft                  = "DRAFT"
	DepreciationRunStatusPopulationFrozen       = "POPULATION_FROZEN"
	DepreciationRunStatusValidated              = "VALIDATED"
	DepreciationRunStatusApproved               = "APPROVED"
	DepreciationRunStatusAccountingEventEmitted = "ACCOUNTING_EVENT_EMITTED"
	DepreciationRunStatusSuperseded             = "SUPERSEDED"
)

// DepreciationSchedule is AST-02's own authority over "DepreciationSchedule"
// — a stable logical schedule_id carrying effective-dated versions, never
// mutated in place once ACTIVE. RecalculateSchedule creates a new version
// and end-dates this one — the spec's own negative path, "Useful life
// changed after approval without invalidation," has no in-place edit path
// to satisfy it any other way.
type DepreciationSchedule struct {
	ScheduleVersionID    string     `json:"schedule_version_id"`
	ScheduleID           string     `json:"schedule_id"`
	Version              int        `json:"version"`
	TenantID             string     `json:"tenant_id"`
	LegalEntityID        string     `json:"legal_entity_id"`
	AssetID              string     `json:"asset_id"`
	BookID               string     `json:"book_id"`
	Method               string     `json:"method"`
	CostBasis            float64    `json:"cost_basis"`
	ResidualValue        float64    `json:"residual_value"`
	UsefulLifeMonths     int        `json:"useful_life_months"`
	InServiceDate        time.Time  `json:"in_service_date"`
	Status               string     `json:"status"`
	EffectiveTo          *time.Time `json:"effective_to,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	CreatedByPrincipalID string     `json:"created_by_principal_id"`
}

type BuildDepreciationScheduleRequest struct {
	AssetID          string     `json:"asset_id"`
	BookID           string     `json:"book_id"`
	CostBasis        float64    `json:"cost_basis"`
	ResidualValue    float64    `json:"residual_value"`
	UsefulLifeMonths int        `json:"useful_life_months"`
	InServiceDate    *time.Time `json:"in_service_date,omitempty"`
}

type RecalculateScheduleRequest struct {
	CostBasis        *float64 `json:"cost_basis,omitempty"`
	ResidualValue    *float64 `json:"residual_value,omitempty"`
	UsefulLifeMonths *int     `json:"useful_life_months,omitempty"`
}

// DepreciationRun is AST-02's own authority over "DepreciationRun" — one
// period's worth of calculated depreciation for a legal entity.
type DepreciationRun struct {
	RunID                              string     `json:"run_id"`
	TenantID                           string     `json:"tenant_id"`
	LegalEntityID                      string     `json:"legal_entity_id"`
	FiscalPeriod                       string     `json:"fiscal_period"`
	DepreciationExpenseAccountCode     string     `json:"depreciation_expense_account_code"`
	AccumulatedDepreciationAccountCode string     `json:"accumulated_depreciation_account_code"`
	Status                             string     `json:"status"`
	JournalID                          *string    `json:"journal_id,omitempty"`
	SupersedesRunID                    *string    `json:"supersedes_run_id,omitempty"`
	SupersededByRunID                  *string    `json:"superseded_by_run_id,omitempty"`
	CreatedAt                          time.Time  `json:"created_at"`
	CreatedByPrincipalID               string     `json:"created_by_principal_id"`
	FrozenAt                           *time.Time `json:"frozen_at,omitempty"`
	ValidatedAt                        *time.Time `json:"validated_at,omitempty"`
	ApprovedAt                         *time.Time `json:"approved_at,omitempty"`
	ApprovedByPrincipalID              *string    `json:"approved_by_principal_id,omitempty"`
	EmittedAt                          *time.Time `json:"emitted_at,omitempty"`
	SupersededAt                       *time.Time `json:"superseded_at,omitempty"`
	SupersededByPrincipalID            *string    `json:"superseded_by_principal_id,omitempty"`

	Lines []DepreciationLine `json:"lines,omitempty"`
}

// DepreciationLine is permanent calculation evidence — one row per
// (run, schedule_version), never mutated (migration 000002's own
// append-only trigger).
type DepreciationLine struct {
	LineID                       string    `json:"line_id"`
	RunID                        string    `json:"run_id"`
	ScheduleVersionID            string    `json:"schedule_version_id"`
	AssetID                      string    `json:"asset_id"`
	BookID                       string    `json:"book_id"`
	PeriodAmount                 float64   `json:"period_amount"`
	AccumulatedDepreciationAfter float64   `json:"accumulated_depreciation_after"`
	CreatedAt                    time.Time `json:"created_at"`
}

type CreateDepreciationRunRequest struct {
	LegalEntityID                      string `json:"legal_entity_id"`
	FiscalPeriod                       string `json:"fiscal_period"`
	DepreciationExpenseAccountCode     string `json:"depreciation_expense_account_code"`
	AccumulatedDepreciationAccountCode string `json:"accumulated_depreciation_account_code"`
}

type SupersedeDepreciationRunRequest struct {
	Reason string `json:"reason"`
}

var (
	ErrScheduleNotFound = errorString("depreciation schedule not found")
	ErrRunNotFound      = errorString("depreciation run not found")

	// ErrAssetNotEligibleForSchedule is returned when BuildDepreciationSchedule
	// is attempted against an asset that is not ACTIVE (not yet capitalized,
	// suspended, merged) — depreciation requires a real, capitalized asset.
	ErrAssetNotEligibleForSchedule = errorString("asset must be ACTIVE (capitalized) to build a depreciation schedule against it")

	// ErrDuplicateScheduleForAssetBook is the spec's own negative path,
	// "Same asset depreciated twice in period," enforced at its root: at
	// most one CURRENT schedule per (asset, book).
	ErrDuplicateScheduleForAssetBook = errorString("this asset already has a current depreciation schedule for this book — use RecalculateSchedule to change it")

	ErrInvalidRunTransition = errorString("depreciation run is not in a status that allows this action")

	// ErrRunAlreadyExistsForPeriod is the other half of "Same asset
	// depreciated twice in period" — at most one live run per (entity,
	// period).
	ErrRunAlreadyExistsForPeriod = errorString("a live depreciation run already exists for this legal entity and fiscal period")

	ErrEmptyFrozenPopulation = errorString("no eligible depreciation schedules were found to freeze into this run")

	ErrSelfApprovalNotPermittedRun = errorString("the principal who created this depreciation run may not also approve it")

	ErrPeriodCheckUnavailable = errorString("financial-close-svc unavailable")
	ErrPeriodLocked           = errorString("cannot apply an asset event into a LOCKED fiscal period")
)

// ── AST-03 Asset Event ───────────────────────────────────────────────────────
//
// See migration 000003's doc comment for the full state-model/command
// mapping, the scope-narrowing decision (DISPOSAL is the only event type
// that drives a real book-state delta in this v1), and all four
// negative-path enforcement mechanisms.

const (
	AssetEventTypeCapitalization       = "CAPITALIZATION"
	AssetEventTypeAddition             = "ADDITION"
	AssetEventTypeComponentReplacement = "COMPONENT_REPLACEMENT"
	AssetEventTypeTransfer             = "TRANSFER"
	AssetEventTypeImpairment           = "IMPAIRMENT"
	AssetEventTypeRevaluation          = "REVALUATION"
	AssetEventTypeDisposal             = "DISPOSAL"

	AssetEventStatusDraft                  = "DRAFT"
	AssetEventStatusValidated              = "VALIDATED"
	AssetEventStatusApproved               = "APPROVED"
	AssetEventStatusApplied                = "APPLIED"
	AssetEventStatusAccountingEventEmitted = "ACCOUNTING_EVENT_EMITTED"
	AssetEventStatusReversed               = "REVERSED"
	AssetEventStatusSuperseded             = "SUPERSEDED"
)

// materialAssetEventTypes are the three event types the spec's own SoD
// language names as requiring maker/checker separation: "Event initiator
// cannot approve material impairment/revaluation/disposal where
// maker-checker applies." Self-approval is refused only for these.
var materialAssetEventTypes = map[string]bool{
	AssetEventTypeImpairment:  true,
	AssetEventTypeRevaluation: true,
	AssetEventTypeDisposal:    true,
}

// IsMaterialAssetEventType reports whether eventType is one of the three
// types the spec names as requiring maker/checker separation on approval.
func IsMaterialAssetEventType(eventType string) bool {
	return materialAssetEventTypes[eventType]
}

// AssetEvent is AST-03's own authority — "AssetEvent; event type; affected
// asset/component/book; source basis; value/effective date; approval
// fingerprint; resulting book-state delta; correction/supersession links."
type AssetEvent struct {
	EventID       string  `json:"event_id"`
	TenantID      string  `json:"tenant_id"`
	LegalEntityID string  `json:"legal_entity_id"`
	AssetID       string  `json:"asset_id"`
	ComponentID   *string `json:"component_id,omitempty"`
	EventType     string  `json:"event_type"`
	Status        string  `json:"status"`

	SourceDocumentRef        string    `json:"source_document_ref"`
	ValuationEvidenceRef     *string   `json:"valuation_evidence_ref,omitempty"`
	Amount                   *float64  `json:"amount,omitempty"`
	Currency                 *string   `json:"currency,omitempty"`
	EffectiveDate            time.Time `json:"effective_date"`
	FiscalPeriod             string    `json:"fiscal_period"`
	ProceedsAmount           *float64  `json:"proceeds_amount,omitempty"`
	DestinationCustodianID   *string   `json:"destination_custodian_id,omitempty"`
	DestinationLocationID    *string   `json:"destination_location_id,omitempty"`
	DestinationLegalEntityID *string   `json:"destination_legal_entity_id,omitempty"`
	DebitAccountCode         *string   `json:"debit_account_code,omitempty"`
	CreditAccountCode        *string   `json:"credit_account_code,omitempty"`
	JournalID                *string   `json:"journal_id,omitempty"`
	CorrectionOfEventID      *string   `json:"correction_of_event_id,omitempty"`

	CreatedAt               time.Time  `json:"created_at"`
	CreatedByPrincipalID    string     `json:"created_by_principal_id"`
	ValidatedAt             *time.Time `json:"validated_at,omitempty"`
	ApprovedAt              *time.Time `json:"approved_at,omitempty"`
	ApprovedByPrincipalID   *string    `json:"approved_by_principal_id,omitempty"`
	AppliedAt               *time.Time `json:"applied_at,omitempty"`
	EmittedAt               *time.Time `json:"emitted_at,omitempty"`
	ReversedAt              *time.Time `json:"reversed_at,omitempty"`
	ReversedByPrincipalID   *string    `json:"reversed_by_principal_id,omitempty"`
	ReversalReason          *string    `json:"reversal_reason,omitempty"`
	SupersededAt            *time.Time `json:"superseded_at,omitempty"`
	SupersededByPrincipalID *string    `json:"superseded_by_principal_id,omitempty"`
	SupersessionReason      *string    `json:"supersession_reason,omitempty"`
}

// CreateAssetEventRequest is the one real create path every named command
// (CreateAssetEvent and the four Record* convenience wrappers) funnels
// through — see migration 000003's doc comment.
type CreateAssetEventRequest struct {
	AssetID                  string     `json:"asset_id"`
	ComponentID              string     `json:"component_id,omitempty"`
	EventType                string     `json:"event_type,omitempty"` // required for CreateAssetEvent; pre-filled by the Record* wrappers
	SourceDocumentRef        string     `json:"source_document_ref"`
	ValuationEvidenceRef     string     `json:"valuation_evidence_ref,omitempty"`
	Amount                   *float64   `json:"amount,omitempty"`
	Currency                 string     `json:"currency,omitempty"`
	EffectiveDate            *time.Time `json:"effective_date,omitempty"`
	FiscalPeriod             string     `json:"fiscal_period"`
	ProceedsAmount           *float64   `json:"proceeds_amount,omitempty"`
	DestinationCustodianID   string     `json:"destination_custodian_id,omitempty"`
	DestinationLocationID    string     `json:"destination_location_id,omitempty"`
	DestinationLegalEntityID string     `json:"destination_legal_entity_id,omitempty"`
	DebitAccountCode         string     `json:"debit_account_code,omitempty"`
	CreditAccountCode        string     `json:"credit_account_code,omitempty"`
	CorrectionOfEventID      string     `json:"correction_of_event_id,omitempty"`
}

type ReverseAssetEventRequest struct {
	Reason string `json:"reason"`
}

type SupersedeAssetEventRequest struct {
	Reason string `json:"reason"`
}

var (
	ErrAssetEventNotFound = errorString("asset event not found")

	ErrInvalidAssetEventTransition = errorString("asset event is not in a status that allows this action")

	ErrAssetEventTypeRequired = errorString("event_type is required")

	// ErrValuationEvidenceRequired is the spec's own negative path,
	// "Impairment amount entered without evidence/approval" — ValidateAssetEvent
	// refuses to validate an IMPAIRMENT or REVALUATION event without a
	// recorded valuation_evidence_ref.
	ErrValuationEvidenceRequired = errorString("impairment/revaluation events require a recorded valuation_evidence_ref before they can be validated")

	// ErrCrossEntityTransferBlocked is the spec's own negative path,
	// "Cross-entity asset transfer bypasses intercompany accounting" —
	// also enforced at the database layer by
	// chk_asset_event_no_cross_entity_transfer; this is the same check
	// surfaced as a named application error before the store is even
	// asked, matching AST-01's own ErrMergeAcrossLegalEntities posture.
	ErrCrossEntityTransferBlocked = errorString(`direct cross-legal-entity transfer is blocked: ownership change requires intercompany/disposal-acquisition treatment`)

	// ErrSelfApprovalNotPermittedEvent is the spec's own SoD: "Event
	// initiator cannot approve material impairment/revaluation/disposal
	// where maker-checker applies" — refused only for
	// IsMaterialAssetEventType event types.
	ErrSelfApprovalNotPermittedEvent = errorString("the principal who created this asset event may not also approve it")

	// ErrAssetNotEligibleForDisposal covers ApplyAssetEvent on a DISPOSAL
	// event whose asset is not currently ACTIVE or SUSPENDED (already
	// disposed, merged, or never capitalized).
	ErrAssetNotEligibleForDisposal = errorString("asset must be ACTIVE or SUSPENDED to be disposed")

	ErrComponentRequiredForReplacement = errorString("component_id is required for a COMPONENT_REPLACEMENT event")

	ErrProceedsRequiredForDisposal = errorString("proceeds_amount is required for a DISPOSAL event")
)
