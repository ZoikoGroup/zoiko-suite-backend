package domain

import (
	"errors"
	"time"
)

// DRC-03 Retention, Disposition & Legal Hold (ZS-SVC-S-001 §5).
// Deliberately dry-run only, per the spec's own build order: "Wave 3 —
// Retention Engine: ... Exit: Automatic deletion remains disabled" and
// "Wave 4 — Legal Hold: ... Exit: Disposition still dry-run only."
// RecordRetentionState stops at APPROVED_FOR_DISPOSITION — no command
// anywhere in this package deletes, destroys or anonymizes anything.

type RetentionTriggerType string

const (
	TriggerCreatedAt     RetentionTriggerType = "CREATED_AT"
	TriggerDeclaredAt    RetentionTriggerType = "DECLARED_AT"
	TriggerPeriodEnd     RetentionTriggerType = "PERIOD_END"
	TriggerTaxYearEnd    RetentionTriggerType = "TAX_YEAR_END"
	TriggerEmploymentEnd RetentionTriggerType = "EMPLOYMENT_END"
	TriggerContractEnd   RetentionTriggerType = "CONTRACT_END"
	TriggerMatterClosed  RetentionTriggerType = "MATTER_CLOSED"
	TriggerCustomEvent   RetentionTriggerType = "CUSTOM_EVENT"
)

func (t RetentionTriggerType) Valid() bool {
	switch t {
	case TriggerCreatedAt, TriggerDeclaredAt, TriggerPeriodEnd, TriggerTaxYearEnd,
		TriggerEmploymentEnd, TriggerContractEnd, TriggerMatterClosed, TriggerCustomEvent:
		return true
	}
	return false
}

type DispositionAction string

const (
	DispositionDelete    DispositionAction = "DELETE"
	DispositionDestroy   DispositionAction = "DESTROY"
	DispositionAnonymize DispositionAction = "ANONYMIZE"
	DispositionReview    DispositionAction = "REVIEW"
	DispositionArchive   DispositionAction = "ARCHIVE"
)

func (d DispositionAction) Valid() bool {
	switch d {
	case DispositionDelete, DispositionDestroy, DispositionAnonymize, DispositionReview, DispositionArchive:
		return true
	}
	return false
}

type RetentionRuleStatus string

const (
	RetentionRuleDraft      RetentionRuleStatus = "DRAFT"
	RetentionRuleApproved   RetentionRuleStatus = "APPROVED"
	RetentionRuleActive     RetentionRuleStatus = "ACTIVE"
	RetentionRuleSuperseded RetentionRuleStatus = "SUPERSEDED"
	RetentionRuleWithdrawn  RetentionRuleStatus = "WITHDRAWN"
)

// RetentionRuleVersion is §5's governed, versioned retention rule
// registry. DRAFT may be edited freely; once APPROVED its terms
// (record_class/jurisdiction/trigger/duration/disposition_action) are
// permanent, mirroring the maker-checker gate already used for
// RecordClassification (migration 000008) — the approver can never be
// the same principal as the creator.
type RetentionRuleVersion struct {
	RetentionRuleVersionID string               `json:"retention_rule_version_id"`
	TenantID               string               `json:"tenant_id"`
	RecordClass            RecordClass          `json:"record_class"`
	JurisdictionSelector   string               `json:"jurisdiction_selector"`
	LegalBasisRef          string               `json:"legal_basis_ref"`
	PurposeRef             string               `json:"purpose_ref"`
	TriggerType            RetentionTriggerType `json:"trigger_type"`
	DurationDays           int                  `json:"duration_days"`
	DispositionAction      DispositionAction    `json:"disposition_action"`
	Status                 RetentionRuleStatus  `json:"status"`
	CreatedByPrincipalID   string               `json:"created_by_principal_id"`
	CreatedAt              time.Time            `json:"created_at"`
	ApprovedByPrincipalID  *string              `json:"approved_by_principal_id,omitempty"`
	ApprovedAt             *time.Time           `json:"approved_at,omitempty"`
	SupersededByVersionID  *string              `json:"superseded_by_version_id,omitempty"`
}

type CreateRetentionRuleVersionParams struct {
	RecordClass          string
	JurisdictionSelector string
	LegalBasisRef        string
	PurposeRef           string
	TriggerType          string
	DurationDays         int
	DispositionAction    string
	CreatedByPrincipalID string
}

type ApproveRetentionRuleVersionParams struct {
	RetentionRuleVersionID string
	ApprovedByPrincipalID  string
}

// RetentionState is §5.4's dry-run lifecycle — stops at
// APPROVED_FOR_DISPOSITION, never reaches an actual disposition
// outcome.
type RetentionState string

const (
	RetentionWaitingForTrigger      RetentionState = "WAITING_FOR_TRIGGER"
	RetentionActive                 RetentionState = "ACTIVE"
	RetentionDue                    RetentionState = "DUE"
	RetentionReviewRequired         RetentionState = "REVIEW_REQUIRED"
	RetentionApprovedForDisposition RetentionState = "APPROVED_FOR_DISPOSITION"
)

// RecordRetentionState is the 1:1, per-record binding to a governed
// retention rule and its dry-run evaluation state.
type RecordRetentionState struct {
	RetentionStateID                    string         `json:"retention_state_id"`
	TenantID                            string         `json:"tenant_id"`
	RecordID                            string         `json:"record_id"`
	RetentionRuleVersionID              string         `json:"retention_rule_version_id"`
	TriggerDate                         *time.Time     `json:"trigger_date,omitempty"`
	DueAt                               *time.Time     `json:"due_at,omitempty"`
	State                               RetentionState `json:"state"`
	ReviewNotes                         string         `json:"review_notes,omitempty"`
	ReviewedByPrincipalID               *string        `json:"reviewed_by_principal_id,omitempty"`
	ReviewedAt                          *time.Time     `json:"reviewed_at,omitempty"`
	ApprovedForDispositionByPrincipalID *string        `json:"approved_for_disposition_by_principal_id,omitempty"`
	ApprovedForDispositionAt            *time.Time     `json:"approved_for_disposition_at,omitempty"`
	CreatedByPrincipalID                string         `json:"created_by_principal_id"`
	CreatedAt                           time.Time      `json:"created_at"`
}

// BindRetentionRuleParams binds a record to an ACTIVE retention rule
// version. If TriggerDate is nil, the state starts WAITING_FOR_TRIGGER
// (the triggering event — e.g. CONTRACT_END — hasn't happened yet); if
// provided, the state starts ACTIVE immediately with due_at computed.
type BindRetentionRuleParams struct {
	RecordID               string
	RetentionRuleVersionID string
	TriggerDate            *time.Time
	CreatedByPrincipalID   string
}

type RecordTriggerEventParams struct {
	RetentionStateID string
	TriggerDate      time.Time
}

type EvaluateDueParams struct {
	RetentionStateID string
	AsOf             time.Time
}

type FlagForReviewParams struct {
	RetentionStateID      string
	ReviewedByPrincipalID string
	ReviewNotes           string
}

type ApproveForDispositionParams struct {
	RetentionStateID      string
	ApprovedByPrincipalID string
}

// LegalHoldStatus per §5.5. PARTIALLY_APPLIED/RELEASE_PENDING/ERROR
// are deliberately not modeled — this wave applies and releases a
// hold's targets synchronously in the same transaction as the
// triggering command, so those intermediate/failure states have
// nothing that could produce them.
type LegalHoldStatus string

const (
	LegalHoldDraft    LegalHoldStatus = "DRAFT"
	LegalHoldActive   LegalHoldStatus = "ACTIVE"
	LegalHoldReleased LegalHoldStatus = "RELEASED"
)

// LegalHold is §5.5's hold entity — a hold is opened DRAFT, activated
// (which applies it to every named target in the same transaction),
// and eventually released (which releases every target together).
type LegalHold struct {
	HoldID                 string          `json:"hold_id"`
	TenantID               string          `json:"tenant_id"`
	MatterRef              string          `json:"matter_ref"`
	AuthorityRef           string          `json:"authority_ref,omitempty"`
	HoldReasonCode         string          `json:"hold_reason_code"`
	Status                 LegalHoldStatus `json:"status"`
	IssuedByPrincipalID    string          `json:"issued_by_principal_id"`
	IssuedAt               time.Time       `json:"issued_at"`
	ActivatedByPrincipalID *string         `json:"activated_by_principal_id,omitempty"`
	ActivatedAt            *time.Time      `json:"activated_at,omitempty"`
	ReleaseReason                *string    `json:"release_reason,omitempty"`
	ReleasedByPrincipalID        *string    `json:"released_by_principal_id,omitempty"`
	ReleaseApprovedByPrincipalID *string    `json:"release_approved_by_principal_id,omitempty"`
	ReleasedAt                   *time.Time `json:"released_at,omitempty"`
}

type CreateLegalHoldParams struct {
	MatterRef           string
	AuthorityRef        string
	HoldReasonCode      string
	RecordIDs           []string
	IssuedByPrincipalID string
}

type ActivateLegalHoldParams struct {
	HoldID                 string
	ActivatedByPrincipalID string
}

type AddLegalHoldTargetParams struct {
	HoldID               string
	RecordID             string
	CreatedByPrincipalID string
}

type ReleaseLegalHoldParams struct {
	HoldID                       string
	ReleaseReason                string
	ReleasedByPrincipalID        string
	ReleaseApprovedByPrincipalID string
}

// LegalHoldTarget is one record covered (or formerly covered) by a
// hold. A target actively blocks disposition exactly when
// AppliedAt != nil && ReleasedAt == nil (DRC-I10).
type LegalHoldTarget struct {
	HoldTargetID string     `json:"hold_target_id"`
	TenantID     string     `json:"tenant_id"`
	HoldID       string     `json:"hold_id"`
	RecordID     string     `json:"record_id"`
	AppliedAt    *time.Time `json:"applied_at,omitempty"`
	ReleasedAt   *time.Time `json:"released_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

// ── errors ───────────────────────────────────────────────────────────────────

var (
	ErrRetentionRuleNotFound     = errors.New("retention rule version not found")
	ErrInvalidTriggerType        = errors.New("invalid trigger_type")
	ErrInvalidDispositionAction  = errors.New("invalid disposition_action")
	ErrRetentionRuleNotDraft     = errors.New("retention rule version is not DRAFT")
	ErrRetentionRuleNotApproved  = errors.New("retention rule version is not APPROVED")
	ErrRetentionRuleSelfApproval = errors.New("the retention rule's own author cannot approve it")
	ErrRetentionRuleNotActive    = errors.New("retention rule version is not ACTIVE")
	ErrRetentionRuleMismatch     = errors.New("retention rule's record_class or jurisdiction_selector does not match the record")

	ErrRetentionStateNotFound          = errors.New("record retention state not found")
	ErrRecordAlreadyBound              = errors.New("this record is already bound to a retention rule")
	ErrRetentionStateNotWaiting        = errors.New("record retention state is not WAITING_FOR_TRIGGER")
	ErrRetentionStateNotActive         = errors.New("record retention state is not ACTIVE")
	ErrRetentionStateNotDue            = errors.New("record retention state is not DUE")
	ErrRetentionStateNotReviewRequired = errors.New("record retention state is not REVIEW_REQUIRED")
	ErrNotYetDue                       = errors.New("retention is not yet due as of the given time")
	ErrRecordUnderLegalHold            = errors.New("record is under an active legal hold and cannot be approved for disposition")

	ErrLegalHoldNotFound     = errors.New("legal hold not found")
	ErrLegalHoldNotDraft     = errors.New("legal hold is not DRAFT")
	ErrLegalHoldNotActive    = errors.New("legal hold is not ACTIVE")
	ErrLegalHoldTargetExists = errors.New("this record is already a target of this legal hold")
	ErrNoRecordIDsForHold    = errors.New("at least one record_id is required")
	// ErrLegalHoldSelfRelease is ZS-SVC-S-001 §5.5's "release_approved_by:
	// Separate release authority; self-release restrictions apply" —
	// the principal executing the release cannot also be its own approver.
	ErrLegalHoldSelfRelease = errors.New("the principal releasing a legal hold cannot also be its own release approver")
)
