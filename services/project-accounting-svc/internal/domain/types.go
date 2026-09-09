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
