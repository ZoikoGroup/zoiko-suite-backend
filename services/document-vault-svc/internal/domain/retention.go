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
	// EffectiveFrom/EffectiveTo are §5.2 effective_from / effective_to. Both
	// optional; the rule applies at t when EffectiveFrom <= t < EffectiveTo.
	EffectiveFrom *time.Time `json:"effective_from,omitempty"`
	EffectiveTo   *time.Time `json:"effective_to,omitempty"`
}

type CreateRetentionRuleVersionParams struct {
	EffectiveFrom        *time.Time
	EffectiveTo          *time.Time
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
	// DispositionApprovalRef is the reference of the external approval decision
	// consumed by POST /v1/dispositions/{id}:approve (§9.2). Stored as evidence
	// (DRC-I13); this service does not verify it — maker-checker is the workflow
	// service's. Nil for states approved through the older route without one.
	DispositionApprovalRef *string   `json:"disposition_approval_ref,omitempty"`
	CreatedByPrincipalID   string    `json:"created_by_principal_id"`
	CreatedAt              time.Time `json:"created_at"`
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
	RetentionStateID       string
	TriggerDate            time.Time
	TriggeredByPrincipalID string
}

type EvaluateDueParams struct {
	RetentionStateID       string
	AsOf                   time.Time
	EvaluatedByPrincipalID string
}

type FlagForReviewParams struct {
	RetentionStateID      string
	ReviewedByPrincipalID string
	ReviewNotes           string
}

type ApproveForDispositionParams struct {
	RetentionStateID      string
	ApprovedByPrincipalID string
	// ApprovalRef is optional here and required by POST /v1/dispositions/{id}:approve.
	ApprovalRef string
}

// CreateDispositionCandidateParams is POST /v1/dispositions (§9.2): "Create
// disposition candidate/job. Hold check + expected retention state."
type CreateDispositionCandidateParams struct {
	RecordID             string
	ExpectedState        RetentionState
	ReviewNotes          string
	CreatedByPrincipalID string
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
	HoldID                       string          `json:"hold_id"`
	TenantID                     string          `json:"tenant_id"`
	MatterRef                    string          `json:"matter_ref"`
	AuthorityRef                 string          `json:"authority_ref,omitempty"`
	HoldReasonCode               string          `json:"hold_reason_code"`
	Status                       LegalHoldStatus `json:"status"`
	IssuedByPrincipalID          string          `json:"issued_by_principal_id"`
	IssuedAt                     time.Time       `json:"issued_at"`
	ActivatedByPrincipalID       *string         `json:"activated_by_principal_id,omitempty"`
	ActivatedAt                  *time.Time      `json:"activated_at,omitempty"`
	ReleaseReason                *string         `json:"release_reason,omitempty"`
	ReleasedByPrincipalID        *string         `json:"released_by_principal_id,omitempty"`
	ReleaseApprovedByPrincipalID *string         `json:"release_approved_by_principal_id,omitempty"`
	ReleasedAt                   *time.Time      `json:"released_at,omitempty"`
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

// ResolveLegalHoldTargetsParams is POST /v1/legal-holds/{id}:resolve-targets for the
// explicit-object scope: the records the hold is to cover.
type ResolveLegalHoldTargetsParams struct {
	HoldID                string
	RecordIDs             []string
	ResolvedByPrincipalID string
}

// Target resolution outcomes (§5.6).
const (
	TargetAdded         = "ADDED"          // became a target (applied immediately when the hold is ACTIVE)
	TargetAlreadyTarget = "ALREADY_TARGET" // was a target of this hold already; nothing changed
)

// ResolvedTarget is one record that is a target of the hold after this resolution.
type ResolvedTarget struct {
	RecordID string `json:"record_id"`
	Outcome  string `json:"outcome"`
	Applied  bool   `json:"applied"`
}

// UnresolvedTarget is a record that could not be made a target. A partially
// resolved hold must expose these rather than present itself as complete (§5.6).
type UnresolvedTarget struct {
	RecordID string `json:"record_id"`
	Reason   string `json:"reason"`
}

// UnresolvedRecordNotFound: no such record in the caller's tenant (another
// tenant's record is indistinguishable from a missing one).
const UnresolvedRecordNotFound = "RECORD_NOT_FOUND"

// HoldResolution is the immutable snapshot one resolve call records.
type HoldResolution struct {
	ResolutionID string             `json:"resolution_id"`
	HoldID       string             `json:"hold_id"`
	HoldStatus   LegalHoldStatus    `json:"hold_status"`
	Resolved     []ResolvedTarget   `json:"resolved_targets"`
	Unresolved   []UnresolvedTarget `json:"unresolved_targets"`
	// Complete is false when any target is unresolved.
	Complete   bool      `json:"complete"`
	ResolvedAt time.Time `json:"resolved_at"`
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
	ErrInvalidEffectiveWindow    = errors.New("effective_to must be after effective_from")
	ErrInvalidTriggerType        = errors.New("invalid trigger_type")
	ErrInvalidDispositionAction  = errors.New("invalid disposition_action")
	ErrRetentionRuleNotDraft     = errors.New("retention rule version is not DRAFT")
	ErrRetentionRuleNotApproved  = errors.New("retention rule version is not APPROVED")
	ErrRetentionRuleSelfApproval = errors.New("the retention rule's own author cannot approve it")
	ErrRetentionRuleNotActive    = errors.New("retention rule version is not ACTIVE")
	ErrRetentionRuleMismatch     = errors.New("retention rule's record_class or jurisdiction_selector does not match the record")

	ErrRetentionStateNotFound   = errors.New("record retention state not found")
	ErrRecordAlreadyBound       = errors.New("this record is already bound to a retention rule")
	ErrRetentionStateNotWaiting = errors.New("record retention state is not WAITING_FOR_TRIGGER")
	ErrRetentionStateNotActive  = errors.New("record retention state is not ACTIVE")
	ErrRetentionStateNotDue     = errors.New("record retention state is not DUE")
	// ErrRetentionStateMismatch — the caller's expected retention state is not the
	// record's actual state (§9.2: dispositions carry the expected retention state).
	ErrRetentionStateMismatch          = errors.New("the record's retention state is not the expected state")
	ErrRetentionStateNotReviewRequired = errors.New("record retention state is not REVIEW_REQUIRED")
	ErrNotYetDue                       = errors.New("retention is not yet due as of the given time")
	ErrRecordUnderLegalHold            = errors.New("record is under an active legal hold and cannot be approved for disposition")

	ErrLegalHoldNotFound     = errors.New("legal hold not found")
	ErrLegalHoldNotDraft     = errors.New("legal hold is not DRAFT")
	ErrLegalHoldNotActive    = errors.New("legal hold is not ACTIVE")
	ErrLegalHoldTargetExists = errors.New("this record is already a target of this legal hold")
	// ErrLegalHoldReleased — a RELEASED hold takes no new targets.
	ErrLegalHoldReleased    = errors.New("legal hold has been released and takes no new targets")
	ErrNoRecordIDsToResolve = errors.New("record_ids must name at least one record")
	ErrNoRecordIDsForHold   = errors.New("at least one record_id is required")
	// ErrLegalHoldSelfRelease is ZS-SVC-S-001 §5.5's "release_approved_by:
	// Separate release authority; self-release restrictions apply" —
	// the principal executing the release cannot also be its own approver.
	ErrLegalHoldSelfRelease = errors.New("the principal releasing a legal hold cannot also be its own release approver")
)

// ── retention evaluation (POST /v1/retention:evaluate, §9.2 / §5.3) ──────────

// RetentionResolution is the outcome of evaluating one record.
type RetentionResolution string

const (
	// ResolutionResolved — exactly one applicable rule, or several with identical terms.
	ResolutionResolved RetentionResolution = "RESOLVED"
	// ResolutionNoApplicableRule — no ACTIVE rule applies. Never defaulted (DRC-I09).
	ResolutionNoApplicableRule RetentionResolution = "NO_APPLICABLE_RULE"
	// ResolutionConflict — applicable rules disagree. §11.1 DRC-006 RETENTION_CONFLICT.
	ResolutionConflict RetentionResolution = "RETENTION_CONFLICT"
)

// ConflictCodeRetentionConflict is §11.1's DRC-006 state name.
const ConflictCodeRetentionConflict = "RETENTION_CONFLICT"

// RetentionRuleSummary is one contributing rule version.
type RetentionRuleSummary struct {
	RetentionRuleVersionID string               `json:"retention_rule_version_id"`
	TriggerType            RetentionTriggerType `json:"trigger_type"`
	DurationDays           int                  `json:"duration_days"`
	DispositionAction      DispositionAction    `json:"disposition_action"`
	LegalBasisRef          string               `json:"legal_basis_ref"`
	PurposeRef             string               `json:"purpose_ref"`
}

// HoldStatus reports ACTIVE legal holds over a record (DRC-I10).
type HoldStatus struct {
	Blocked bool     `json:"blocked"`
	HoldIDs []string `json:"hold_ids"`
}

// RetentionEvaluation is one record's resolved retention position.
type RetentionEvaluation struct {
	RecordID          string                 `json:"record_id"`
	AsOf              time.Time              `json:"as_of"`
	RecordClass       string                 `json:"record_class"`
	JurisdictionScope string                 `json:"jurisdiction_scope"`
	Resolution        RetentionResolution    `json:"resolution"`
	ConflictCode      string                 `json:"conflict_code,omitempty"`
	Reason            string                 `json:"reason"`
	ApplicableRules   []RetentionRuleSummary `json:"applicable_rules"`
	ScheduleState     *RecordRetentionState  `json:"schedule_state,omitempty"`
	LegalHold         HoldStatus             `json:"legal_hold"`
}

// EvaluateRetentionParams is the input of the evaluating, event-publishing form of
// retention evaluation. AsOf is the instant at which rule effective windows are
// tested (§5.3 "effective date").
type EvaluateRetentionParams struct {
	RecordIDs        []string
	AsOf             time.Time
	ActorPrincipalID string
}

// ResolveRetention sets Resolution, ConflictCode and Reason from ev.ApplicableRules.
// Pure: it applies the §5.3 principles without choosing a winner between rules
// that disagree.
func ResolveRetention(ev *RetentionEvaluation) {
	if ev.ApplicableRules == nil {
		ev.ApplicableRules = []RetentionRuleSummary{}
	}
	switch n := len(ev.ApplicableRules); {
	case n == 0:
		ev.Resolution = ResolutionNoApplicableRule
		ev.Reason = "no ACTIVE retention rule applies to this record class and jurisdiction; no default is applied"
	case n == 1:
		ev.Resolution = ResolutionResolved
		ev.Reason = "exactly one ACTIVE retention rule applies"
	default:
		first := ev.ApplicableRules[0]
		identical := true
		for _, r := range ev.ApplicableRules[1:] {
			if r.TriggerType != first.TriggerType || r.DurationDays != first.DurationDays || r.DispositionAction != first.DispositionAction {
				identical = false
				break
			}
		}
		if identical {
			ev.Resolution = ResolutionResolved
			ev.Reason = "several ACTIVE rules apply and specify identical trigger, duration and disposition action; each is a contributing rule"
		} else {
			ev.Resolution = ResolutionConflict
			ev.ConflictCode = ConflictCodeRetentionConflict
			ev.Reason = "several ACTIVE rules apply and differ in trigger, duration or disposition action; no winner is chosen, review required"
		}
	}
}
