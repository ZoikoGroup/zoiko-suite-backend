// Package domain defines the authoritative domain types for
// project-accounting-svc — PRJ-01 (Project/Job Master) from ZS-SVC-G-001,
// the first of four capabilities this service will host (PRJ-01 through
// PRJ-04), co-located per that document's own §7: "Physical deployment
// may be consolidated, but each logical service retains separate
// authoritative facts, commands, states, evidence and certification
// requirements."
package domain

import "time"

// Project lifecycle states — PRJ-01's own state model (verbatim from
// spec): "Draft→Approved→Active→Suspended→Closing→Closed; Reopened
// controlled." See migration 000001's doc comment for why Closing has no
// command that reaches it independently, and why Suspended has no
// reverse command, in this v1.
const (
	ProjectStatusDraft     = "DRAFT"
	ProjectStatusApproved  = "APPROVED"
	ProjectStatusActive    = "ACTIVE"
	ProjectStatusSuspended = "SUSPENDED"
	ProjectStatusClosed    = "CLOSED"
)

const (
	RecognitionMethodPercentageOfCompletion = "PERCENTAGE_OF_COMPLETION"
	RecognitionMethodCompletedContract      = "COMPLETED_CONTRACT"
	RecognitionMethodTimeAndMaterials       = "TIME_AND_MATERIALS"

	BillingTypeFixedPrice       = "FIXED_PRICE"
	BillingTypeTimeAndMaterials = "TIME_AND_MATERIALS"
	BillingTypeCostPlus         = "COST_PLUS"
)

// Project is PRJ-01's own authority — "FinancialProject; ProjectWorkPackage/
// WBS; legal entity; customer/contract references; project type;
// manager/cost center; accounting/billing policy refs; dimensions;
// lifecycle." LegalEntityID is set once at creation and never exposed as
// an editable field of any later command — see migration 000001's doc
// comment on negative path #1.
type Project struct {
	ProjectID     string `json:"project_id"`
	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ProjectCode   string `json:"project_code"`
	Name          string `json:"name"`
	ProjectType   string `json:"project_type,omitempty"`

	CustomerRef        string     `json:"customer_ref,omitempty"`
	ContractRef        *string    `json:"contract_ref,omitempty"`
	ManagerPrincipalID string     `json:"manager_principal_id,omitempty"`
	CostCenter         string     `json:"cost_center,omitempty"`
	GroupReference     string     `json:"group_reference,omitempty"`
	StartDate          *time.Time `json:"start_date,omitempty"`
	EndDate            *time.Time `json:"end_date,omitempty"`

	Status string `json:"status"`

	CreatedAt              time.Time  `json:"created_at"`
	CreatedByPrincipalID   string     `json:"created_by_principal_id"`
	ApprovedAt             *time.Time `json:"approved_at,omitempty"`
	ApprovedByPrincipalID  *string    `json:"approved_by_principal_id,omitempty"`
	ActivatedAt            *time.Time `json:"activated_at,omitempty"`
	SuspendedAt            *time.Time `json:"suspended_at,omitempty"`
	SuspendedByPrincipalID *string    `json:"suspended_by_principal_id,omitempty"`
	SuspensionReason       *string    `json:"suspension_reason,omitempty"`
	ClosedAt               *time.Time `json:"closed_at,omitempty"`
	ClosedByPrincipalID    *string    `json:"closed_by_principal_id,omitempty"`
	CloseReason            *string    `json:"close_reason,omitempty"`
	ReopenedAt             *time.Time `json:"reopened_at,omitempty"`
	ReopenedByPrincipalID  *string    `json:"reopened_by_principal_id,omitempty"`
	ReopenReason           *string    `json:"reopen_reason,omitempty"`

	WorkPackages []WorkPackage `json:"work_packages,omitempty"`
}

// WorkPackage is PRJ-01's own "ProjectWorkPackage/WBS" — append-only,
// never mutated or deleted once created. See migration 000001's doc
// comment on negative path #4.
type WorkPackage struct {
	WBSID                string    `json:"wbs_id"`
	ProjectID            string    `json:"project_id"`
	WBSCode              string    `json:"wbs_code"`
	Description          string    `json:"description,omitempty"`
	ParentWBSID          *string   `json:"parent_wbs_id,omitempty"`
	EffectiveFrom        time.Time `json:"effective_from"`
	CreatedAt            time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
}

// FinancialProfile is PRJ-01's own "accounting/billing policy refs" —
// versioned, effective-dated, and — per AmendFinancialProfile's own
// future-effective enforcement — only ever created with a future
// EffectiveFrom, never retroactively. See migration 000001's doc comment
// on negative path #2.
type FinancialProfile struct {
	ProfileVersionID     string     `json:"profile_version_id"`
	ProfileID            string     `json:"profile_id"`
	Version              int        `json:"version"`
	ProjectID            string     `json:"project_id"`
	RecognitionMethod    string     `json:"recognition_method"`
	BillingType          string     `json:"billing_type"`
	Currency             string     `json:"currency"`
	EffectiveFrom        time.Time  `json:"effective_from"`
	EffectiveTo          *time.Time `json:"effective_to,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	CreatedByPrincipalID string     `json:"created_by_principal_id"`
}

// ── Request types ────────────────────────────────────────────────────────

type CreateProjectRequest struct {
	LegalEntityID      string     `json:"legal_entity_id"`
	ProjectCode        string     `json:"project_code"`
	Name               string     `json:"name"`
	ProjectType        string     `json:"project_type,omitempty"`
	CustomerRef        string     `json:"customer_ref,omitempty"`
	ManagerPrincipalID string     `json:"manager_principal_id,omitempty"`
	CostCenter         string     `json:"cost_center,omitempty"`
	GroupReference     string     `json:"group_reference,omitempty"`
	StartDate          *time.Time `json:"start_date,omitempty"`
	EndDate            *time.Time `json:"end_date,omitempty"`
}

type AddWorkPackageRequest struct {
	WBSCode     string `json:"wbs_code"`
	Description string `json:"description,omitempty"`
	ParentWBSID string `json:"parent_wbs_id,omitempty"`
}

type AmendFinancialProfileRequest struct {
	RecognitionMethod string     `json:"recognition_method"`
	BillingType       string     `json:"billing_type"`
	Currency          string     `json:"currency"`
	EffectiveFrom     *time.Time `json:"effective_from,omitempty"`
}

type SuspendProjectRequest struct {
	Reason string `json:"reason"`
}

type CloseProjectRequest struct {
	Reason string `json:"reason"`
}

type ReopenProjectRequest struct {
	Reason string `json:"reason"`
}

type LinkContractRequest struct {
	ContractRef string `json:"contract_ref"`
}

// ── Errors ───────────────────────────────────────────────────────────────

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrProjectNotFound         = errorString("project not found")
	ErrIdentityMissing         = errorString("caller identity missing")
	ErrTenantScopeMismatch     = errorString("tenant scope mismatch")
	ErrAuthorizationDenied     = errorString("authorization denied for project accounting action")
	ErrAuthzServiceUnavailable = errorString("authorization-svc unavailable")
	ErrStoreUnavailable        = errorString("project accounting store unavailable")

	ErrInvalidProjectTransition = errorString("project is not in a status that allows this action")

	ErrDuplicateProjectCode = errorString("a project with this project_code already exists for this legal entity")

	// ErrSelfApprovalNotPermittedProject is the spec's own SoD, "Project
	// creator cannot self-approve high-value/regulated project profile" —
	// no materiality tiering exists in this platform, so refused
	// universally, the same bootstrap-gap posture used throughout this
	// session.
	ErrSelfApprovalNotPermittedProject = errorString("the principal who created this project may not also approve it")

	// ErrFinancialProfileRequiredForActivation is this v1's real
	// enforcement of the spec's own failure semantics, "Missing policy/
	// currency/book blocks activation."
	ErrFinancialProfileRequiredForActivation = errorString("cannot activate a project with no financial profile assigned")

	// ErrFinancialProfileMustBeFutureEffective is AmendFinancialProfile's
	// own real enforcement of negative path #2, "Recognition policy
	// changed after run without invalidation."
	ErrFinancialProfileMustBeFutureEffective = errorString("financial profile effective_from must be in the future")

	// ErrSelfReopenNotPermitted is the spec's own SoD, "closed project
	// reopen requires independent approval."
	ErrSelfReopenNotPermitted = errorString("the principal who closed this project may not also reopen it")

	ErrReasonRequired = errorString("reason is required")

	ErrDuplicateWBSCode = errorString("a work package with this wbs_code already exists for this project")
)

// ── PRJ-02 Project Cost Capture ─────────────────────────────────────────────
//
// See migration 000002's doc comment for the full state-model/command
// mapping and all four negative-path enforcement mechanisms.

const (
	CostSourceTypeAP         = "AP"
	CostSourceTypePayroll    = "PAYROLL"
	CostSourceTypeInventory  = "INVENTORY"
	CostSourceTypeAsset      = "ASSET"
	CostSourceTypeAllocation = "ALLOCATION"
	CostSourceTypeManual     = "MANUAL"

	CostEntryStatusCaptured = "CAPTURED"
	CostEntryStatusAccepted = "ACCEPTED"
	CostEntryStatusReversed = "REVERSED"
)

// CostEntry is PRJ-02's own "ProjectCostEntry" — append-only; its own
// economic fields never change once written. See migration 000002's doc
// comment on negative paths #1 and #4.
type CostEntry struct {
	EntryID               string     `json:"entry_id"`
	LegalEntityID         string     `json:"legal_entity_id"`
	ProjectID             string     `json:"project_id"`
	WBSID                 *string    `json:"wbs_id,omitempty"`
	SourceType            string     `json:"source_type"`
	SourceReference       string     `json:"source_reference"`
	CostCategory          string     `json:"cost_category,omitempty"`
	Quantity              *float64   `json:"quantity,omitempty"`
	Amount                float64    `json:"amount"`
	Currency              string     `json:"currency"`
	TransactionDate       time.Time  `json:"transaction_date"`
	Billable              bool       `json:"billable"`
	Capitalizable         bool       `json:"capitalizable"`
	Status                string     `json:"status"`
	ReclassifiesEntryID   *string    `json:"reclassifies_entry_id,omitempty"`
	ReversesEntryID       *string    `json:"reverses_entry_id,omitempty"`
	Reason                *string    `json:"reason,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
	CreatedByPrincipalID  string     `json:"created_by_principal_id"`
	ValidatedAt           *time.Time `json:"validated_at,omitempty"`
	ApprovedAt            *time.Time `json:"approved_at,omitempty"`
	ApprovedByPrincipalID *string    `json:"approved_by_principal_id,omitempty"`
}

// CostCertification is CertifyCostPopulation's own named evidence — a
// point-in-time attestation over a project's own cost population.
type CostCertification struct {
	CertificationID        string    `json:"certification_id"`
	ProjectID              string    `json:"project_id"`
	EntryCount             int       `json:"entry_count"`
	TotalAmount            float64   `json:"total_amount"`
	CertifiedAt            time.Time `json:"certified_at"`
	CertifiedByPrincipalID string    `json:"certified_by_principal_id"`
}

// ── Request types ────────────────────────────────────────────────────────

// CaptureProjectCostRequest is the one real create path both
// CaptureProjectCost and AllocateSharedCostToProject funnel through — the
// latter pre-sets SourceType to ALLOCATION.
type CaptureProjectCostRequest struct {
	ProjectID       string     `json:"project_id"`
	WBSID           string     `json:"wbs_id,omitempty"`
	SourceType      string     `json:"source_type,omitempty"` // required for CaptureProjectCost; pre-filled by AllocateSharedCostToProject
	SourceReference string     `json:"source_reference"`
	CostCategory    string     `json:"cost_category,omitempty"`
	Quantity        *float64   `json:"quantity,omitempty"`
	Amount          float64    `json:"amount"`
	Currency        string     `json:"currency"`
	TransactionDate *time.Time `json:"transaction_date,omitempty"`
}

type ReclassifyProjectCostRequest struct {
	CostCategory  string   `json:"cost_category,omitempty"`
	Billable      *bool    `json:"billable,omitempty"`
	Capitalizable *bool    `json:"capitalizable,omitempty"`
	Amount        *float64 `json:"amount,omitempty"`
	Reason        string   `json:"reason"`
}

type ReverseProjectCostRequest struct {
	Reason string `json:"reason"`
}

type MarkBillableEligibilityRequest struct {
	Billable      bool `json:"billable"`
	Capitalizable bool `json:"capitalizable"`
}

// ── Errors ───────────────────────────────────────────────────────────────

var (
	ErrCostEntryNotFound = errorString("project cost entry not found")

	ErrInvalidCostEntryTransition = errorString("cost entry is not in a status that allows this action")

	ErrSourceTypeRequired = errorString("source_type is required")

	// ErrSelfApprovalNotPermittedReclassify is the spec's own SoD, "Manual
	// project-cost adjustment above threshold requires independent
	// approval" — no materiality tiering exists in this platform, so
	// refused universally, the same bootstrap-gap posture used throughout
	// this session.
	ErrSelfApprovalNotPermittedReclassify = errorString("the principal who captured this cost entry may not also approve a reclassification of it")

	ErrSelfApprovalNotPermittedReversal = errorString("the principal who captured this cost entry may not also approve its reversal")

	ErrCostEntryAlreadyReversed = errorString("this cost entry has already been reversed")
)

// ── PRJ-03 Project Revenue & WIP ────────────────────────────────────────────
//
// See migration 000003's doc comment for the full state-model/command
// mapping and all four negative-path enforcement mechanisms.

const (
	RecognitionRunStatusDraft                  = "DRAFT"
	RecognitionRunStatusPopulationFrozen       = "POPULATION_FROZEN"
	RecognitionRunStatusCalculated             = "CALCULATED"
	RecognitionRunStatusReviewed               = "REVIEWED"
	RecognitionRunStatusApproved               = "APPROVED"
	RecognitionRunStatusAccountingEventEmitted = "ACCOUNTING_EVENT_EMITTED"
	RecognitionRunStatusSuperseded             = "SUPERSEDED"

	BalanceTypeContractAsset     = "CONTRACT_ASSET"
	BalanceTypeContractLiability = "CONTRACT_LIABILITY"
	BalanceTypeNone              = "NONE"
)

// RecognitionEstimate is SetApprovedEstimate's own authority — versioned,
// effective-dated, and only ever created with a future EffectiveFrom,
// never retroactively. See migration 000003's doc comment on negative
// path #2.
type RecognitionEstimate struct {
	EstimateVersionID    string     `json:"estimate_version_id"`
	EstimateID           string     `json:"estimate_id"`
	Version              int        `json:"version"`
	ProjectID            string     `json:"project_id"`
	EstimateToComplete   float64    `json:"estimate_to_complete"`
	EffectiveFrom        time.Time  `json:"effective_from"`
	EffectiveTo          *time.Time `json:"effective_to,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	CreatedByPrincipalID string     `json:"created_by_principal_id"`
}

// RecognitionRun is PRJ-03's own "ProjectRecognitionRun" — its own
// calculated figures are immutable once CALCULATED (see migration
// 000003's own reject-mutation trigger); only approval/emission/
// supersession metadata changes afterward.
type RecognitionRun struct {
	RunID                       string     `json:"run_id"`
	LegalEntityID               string     `json:"legal_entity_id"`
	ProjectID                   string     `json:"project_id"`
	FiscalPeriod                string     `json:"fiscal_period"`
	Status                      string     `json:"status"`
	ContractValue               *float64   `json:"contract_value,omitempty"`
	BilledToDate                *float64   `json:"billed_to_date,omitempty"`
	EstimateToComplete          *float64   `json:"estimate_to_complete,omitempty"`
	ITDCostIncurred             *float64   `json:"itd_cost_incurred,omitempty"`
	PercentComplete             *float64   `json:"percent_complete,omitempty"`
	CumulativeRecognizedRevenue *float64   `json:"cumulative_recognized_revenue,omitempty"`
	PeriodRecognizedRevenue     *float64   `json:"period_recognized_revenue,omitempty"`
	RecognizedCost              *float64   `json:"recognized_cost,omitempty"`
	Margin                      *float64   `json:"margin,omitempty"`
	BalanceType                 *string    `json:"balance_type,omitempty"`
	BalanceAmount               *float64   `json:"balance_amount,omitempty"`
	RevenueAccountCode          string     `json:"revenue_account_code,omitempty"`
	WIPAccountCode              string     `json:"wip_account_code,omitempty"`
	JournalID                   *string    `json:"journal_id,omitempty"`
	SupersedesRunID             *string    `json:"supersedes_run_id,omitempty"`
	SupersededByRunID           *string    `json:"superseded_by_run_id,omitempty"`
	CreatedAt                   time.Time  `json:"created_at"`
	CreatedByPrincipalID        string     `json:"created_by_principal_id"`
	FrozenAt                    *time.Time `json:"frozen_at,omitempty"`
	CalculatedAt                *time.Time `json:"calculated_at,omitempty"`
	ValidatedAt                 *time.Time `json:"validated_at,omitempty"`
	ApprovedAt                  *time.Time `json:"approved_at,omitempty"`
	ApprovedByPrincipalID       *string    `json:"approved_by_principal_id,omitempty"`
	EmittedAt                   *time.Time `json:"emitted_at,omitempty"`
	SupersededAt                *time.Time `json:"superseded_at,omitempty"`
	SupersededByPrincipalID     *string    `json:"superseded_by_principal_id,omitempty"`
}

// ── Request types ────────────────────────────────────────────────────────

type SetApprovedEstimateRequest struct {
	ProjectID          string     `json:"project_id"`
	EstimateToComplete float64    `json:"estimate_to_complete"`
	EffectiveFrom      *time.Time `json:"effective_from,omitempty"`
}

type CreateRecognitionRunRequest struct {
	ProjectID          string  `json:"project_id"`
	FiscalPeriod       string  `json:"fiscal_period"`
	ContractValue      float64 `json:"contract_value"`
	BilledToDate       float64 `json:"billed_to_date"`
	RevenueAccountCode string  `json:"revenue_account_code"`
	WIPAccountCode     string  `json:"wip_account_code"`
}

type SupersedeRecognitionRunRequest struct {
	Reason string `json:"reason"`
}

// ── Errors ───────────────────────────────────────────────────────────────

var (
	ErrRecognitionRunNotFound = errorString("recognition run not found")

	ErrInvalidRecognitionRunTransition = errorString("recognition run is not in a status that allows this action")

	ErrRecognitionRunAlreadyExistsForPeriod = errorString("a live recognition run already exists for this project and fiscal period")

	// ErrEstimateMustBeFutureEffective is SetApprovedEstimate's own real
	// enforcement of negative path #2, "Progress estimate changed after
	// approval without invalidation."
	ErrEstimateMustBeFutureEffective = errorString("estimate effective_from must be in the future")

	ErrApprovedEstimateRequired = errorString("cannot freeze a recognition run with no approved estimate assigned")

	ErrContractValueRequired = errorString("contract_value is required and must be positive")

	// ErrSelfApprovalNotPermittedRecognition is the spec's own SoD,
	// "Estimator/preparer cannot self-approve material estimate or
	// recognition override" — no materiality tiering exists in this
	// platform, so refused universally.
	ErrSelfApprovalNotPermittedRecognition = errorString("the principal who created this recognition run may not also approve it")

	ErrPeriodCheckUnavailable = errorString("financial-close-svc unavailable")
	ErrPeriodLocked           = errorString("cannot certify a recognition run into a LOCKED fiscal period")
)

// ── PRJ-04 Project Profitability ────────────────────────────────────────────
//
// See migration 000004's doc comment for the full state-model/command
// mapping and all four negative-path enforcement mechanisms. PRJ-04 is a
// read model — it owns no business lifecycle authority of its own.

const (
	ProfitabilityProjectionStatusCurrent    = "CURRENT"
	ProfitabilityProjectionStatusStale      = "STALE"
	ProfitabilityProjectionStatusRebuilding = "REBUILDING"

	ProfitabilitySnapshotStatusDraft      = "DRAFT"
	ProfitabilitySnapshotStatusReconciled = "RECONCILED"
	ProfitabilitySnapshotStatusCertified  = "CERTIFIED"
)

// ProfitabilityProjection is PRJ-04's own live read model over PRJ-02's
// real ITD cost and PRJ-03's real revenue/WIP — RefreshProfitabilityProjection
// is the only way its own figures change.
type ProfitabilityProjection struct {
	ProjectionID           string     `json:"projection_id"`
	LegalEntityID          string     `json:"legal_entity_id"`
	ProjectID              string     `json:"project_id"`
	Status                 string     `json:"status"`
	Revenue                float64    `json:"revenue"`
	Cost                   float64    `json:"cost"`
	Margin                 float64    `json:"margin"`
	BilledAmount           float64    `json:"billed_amount"`
	UnbilledAmount         float64    `json:"unbilled_amount"`
	CostWatermarkAt        time.Time  `json:"cost_watermark_at"`
	RevenueRunID           *string    `json:"revenue_run_id,omitempty"`
	RevenueWatermarkAt     *time.Time `json:"revenue_watermark_at,omitempty"`
	RefreshedAt            time.Time  `json:"refreshed_at"`
	RefreshedByPrincipalID string     `json:"refreshed_by_principal_id"`
	CreatedAt              time.Time  `json:"created_at"`
}

// ProfitabilitySnapshot is a frozen, evidenced copy of the projection at
// build time — the only artifact this capability can certify. See
// migration 000004's own reject-mutation trigger: its own economic
// figures and watermarks never change again once built.
type ProfitabilitySnapshot struct {
	SnapshotID             string     `json:"snapshot_id"`
	LegalEntityID          string     `json:"legal_entity_id"`
	ProjectID              string     `json:"project_id"`
	Status                 string     `json:"status"`
	Revenue                float64    `json:"revenue"`
	Cost                   float64    `json:"cost"`
	Margin                 float64    `json:"margin"`
	BilledAmount           float64    `json:"billed_amount"`
	UnbilledAmount         float64    `json:"unbilled_amount"`
	CostWatermarkAt        time.Time  `json:"cost_watermark_at"`
	RevenueRunID           *string    `json:"revenue_run_id,omitempty"`
	RevenueWatermarkAt     *time.Time `json:"revenue_watermark_at,omitempty"`
	BuiltAt                time.Time  `json:"built_at"`
	BuiltByPrincipalID     string     `json:"built_by_principal_id"`
	CertifiedAt            *time.Time `json:"certified_at,omitempty"`
	CertifiedByPrincipalID *string    `json:"certified_by_principal_id,omitempty"`
}

// ── Errors ───────────────────────────────────────────────────────────────

var (
	ErrProjectionNotBuilt = errorString("no profitability projection exists yet for this project — call RefreshProfitabilityProjection first")

	// ErrProjectionStale is BuildProfitabilitySnapshot's own real
	// enforcement of negative path #1, "Stale project margin shown as
	// certified" — refused at build time.
	ErrProjectionStale = errorString("cannot build a snapshot from a stale profitability projection — refresh it first")

	ErrSnapshotNotFound = errorString("profitability snapshot not found")

	ErrInvalidSnapshotTransition = errorString("profitability snapshot is not in a status that allows this action")

	// ErrSnapshotStaleAtCertification is CertifyProfitabilitySnapshot's own
	// real enforcement of negative path #1 at certify time — re-verified
	// against LIVE source data, not just the build-time check.
	ErrSnapshotStaleAtCertification = errorString("source cost/revenue data has changed since this snapshot was built — build a new snapshot before certifying")
)
