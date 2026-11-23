package domain

// R-001 WFC-05: deadline clocks (s7.1-7.4, s9; R-INV-14..19, R-INV-26).
//
// A clock is the computed deadline of one obligation under one regime (its
// class). The due instant is never entered by a caller: it is the answer of
// jurisdiction-rules-svc's POST /v1/obligation-calculations:calculate, stored
// with every fact of its provenance (rule, calendar version, time zone,
// business-day convention, extension, pack) and the trigger that started it.
//
// due_at never changes on a clock (a database trigger refuses it). A
// recalculation that moves it supersedes the clock and creates a successor,
// so every earlier due instant stays readable (R-INV-17, NP-32).
//
// The state a reader sees is COMPUTED from the stored state, the clock's
// times and now (EffectiveState): a clock past due_at reads OVERDUE whether or
// not a sweep has run (R-INV-26, NP-16). The sweep only persists the moves and
// emits their events, guarded on the row version (NP-37).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"
)

// ClockClass is the regime of a deadline (R-001 s7.1).
type ClockClass string

const (
	ClockClassHardLegal   ClockClass = "HARD_LEGAL"   // statute; a tenant SLA never moves it (R-INV-15)
	ClockClassRegulatory  ClockClass = "REGULATORY"   // a regulator's deadline short of statute
	ClockClassContractual ClockClass = "CONTRACTUAL"  // a contract term
	ClockClassInternalSLA ClockClass = "INTERNAL_SLA" // the tenant's own target
	ClockClassAdvisory    ClockClass = "ADVISORY"     // informational
)

// ClockClasses is the set of valid classes.
var ClockClasses = map[ClockClass]bool{
	ClockClassHardLegal: true, ClockClassRegulatory: true, ClockClassContractual: true,
	ClockClassInternalSLA: true, ClockClassAdvisory: true,
}

// ClockState is the lifecycle state of a clock (R-001 s7.2).
type ClockState string

const (
	ClockStatePlanned       ClockState = "PLANNED"
	ClockStateActive        ClockState = "ACTIVE"
	ClockStateDueSoon       ClockState = "DUE_SOON"
	ClockStateDue           ClockState = "DUE"
	ClockStateOverdue       ClockState = "OVERDUE"
	ClockStateFulfilled     ClockState = "FULFILLED"
	ClockStateWaived        ClockState = "WAIVED"
	ClockStateNotApplicable ClockState = "NOT_APPLICABLE"
	ClockStateSuperseded    ClockState = "SUPERSEDED"
)

// ClockStatuses is the set of valid states.
var ClockStatuses = map[ClockState]bool{
	ClockStatePlanned: true, ClockStateActive: true, ClockStateDueSoon: true, ClockStateDue: true,
	ClockStateOverdue: true, ClockStateFulfilled: true, ClockStateWaived: true,
	ClockStateNotApplicable: true, ClockStateSuperseded: true,
}

// Terminal reports whether no command can move the clock any more.
func (s ClockState) Terminal() bool {
	switch s {
	case ClockStateFulfilled, ClockStateWaived, ClockStateNotApplicable, ClockStateSuperseded:
		return true
	}
	return false
}

// Running reports whether the clock is triggered and not terminal: ACTIVE,
// DUE_SOON, DUE or OVERDUE.
func (s ClockState) Running() bool {
	switch s {
	case ClockStateActive, ClockStateDueSoon, ClockStateDue, ClockStateOverdue:
		return true
	}
	return false
}

// rank orders the running states; a computed state never goes backwards.
func (s ClockState) rank() int {
	switch s {
	case ClockStateActive:
		return 1
	case ClockStateDueSoon:
		return 2
	case ClockStateDue:
		return 3
	case ClockStateOverdue:
		return 4
	}
	return 0
}

// EffectiveState is the state a reader sees at now. A terminal or PLANNED
// clock reads as stored. A running clock reads OVERDUE once now is after
// due_at, DUE from the start of its local due date, DUE_SOON from its warning
// instant, and never earlier than its stored state (R-INV-26, NP-16).
func EffectiveState(stored ClockState, now, dueAt, dueDayStart time.Time, dueSoonAt *time.Time) ClockState {
	if !stored.Running() {
		return stored
	}
	computed := ClockStateActive
	switch {
	case now.After(dueAt):
		computed = ClockStateOverdue
	case !now.Before(dueDayStart):
		computed = ClockStateDue
	case dueSoonAt != nil && !now.Before(*dueSoonAt):
		computed = ClockStateDueSoon
	}
	if computed.rank() < stored.rank() {
		return stored
	}
	return computed
}

// Clock commands (ST-3: named commands, never a free target state).
const (
	ClockCommandActivate      = "activate"
	ClockCommandSatisfy       = "satisfy"
	ClockCommandWaive         = "waive"
	ClockCommandNotApplicable = "not-applicable"
	ClockCommandRecalculate   = "recalculate"
	ClockCommandSweep         = "sweep" // system: persists a computed move
	ClockCommandSupersede     = "supersede"
	ClockCommandStart         = "start"
)

// ClockCommandAllowed reports whether a command may run on a clock in the
// given effective state.
func ClockCommandAllowed(command string, effective ClockState) bool {
	switch command {
	case ClockCommandActivate:
		return effective == ClockStatePlanned
	case ClockCommandSatisfy:
		return effective.Running()
	case ClockCommandWaive, ClockCommandNotApplicable, ClockCommandRecalculate:
		return effective == ClockStatePlanned || effective.Running()
	}
	return false
}

// Clock is one deadline clock with its full provenance.
type Clock struct {
	ClockID       string     `json:"clock_id"`
	ClockKey      string     `json:"clock_key"`
	ObligationID  string     `json:"obligation_id"`
	TenantID      string     `json:"tenant_id"`
	LegalEntityID string     `json:"legal_entity_id"`
	ClockClass    ClockClass `json:"clock_class"`

	// StoredState is what the database holds; ClockState is EffectiveState at
	// the time of the read.
	StoredState ClockState `json:"stored_state"`
	ClockState  ClockState `json:"clock_state"`

	// Provenance (R-INV-16): exactly what jurisdiction-rules-svc answered.
	JurisdictionCode      string          `json:"jurisdiction_code"`
	RuleObligationCode    string          `json:"rule_obligation_code"`
	ObligationRuleID      string          `json:"obligation_rule_id"`
	RuleVersion           int             `json:"rule_version"`
	RuleContentDigest     string          `json:"rule_content_digest"`
	CalendarCode          *string         `json:"calendar_code,omitempty"`
	CalendarVersion       *int            `json:"calendar_version,omitempty"`
	CalendarVersionID     *string         `json:"calendar_version_id,omitempty"`
	Timezone              string          `json:"timezone"`
	BusinessDayAdjustment *string         `json:"business_day_adjustment,omitempty"`
	AnchorDate            *string         `json:"anchor_date,omitempty"`
	UnadjustedDate        *string         `json:"unadjusted_date,omitempty"`
	LocalDueDate          string          `json:"local_due_date"`
	DueAt                 time.Time       `json:"due_at"`
	DueSoonAt             *time.Time      `json:"due_soon_at,omitempty"`
	ExtensionDaysGranted  int             `json:"extension_days_granted"`
	PackRef               *string         `json:"pack_ref,omitempty"`
	PackVersion           *string         `json:"pack_version,omitempty"`
	PackVersionID         *string         `json:"pack_version_id,omitempty"`
	CalculationRef        string          `json:"calculation_ref"`
	CalculationExplain    json.RawMessage `json:"calculation_explanation"`

	// Trigger evidence (s7.1, NP-12).
	Trigger TriggerEvidence `json:"trigger"`

	// Supersession (R-INV-17).
	SupersedesClockID   *string    `json:"supersedes_clock_id,omitempty"`
	SupersededByClockID *string    `json:"superseded_by_clock_id,omitempty"`
	SupersededAt        *time.Time `json:"superseded_at,omitempty"`
	SupersessionReason  *string    `json:"supersession_reason,omitempty"`

	// Satisfaction (R-INV-14).
	SatisfiedAt           *time.Time `json:"satisfied_at,omitempty"`
	SatisfiedBy           *string    `json:"satisfied_by,omitempty"`
	EvidenceRefs          []string   `json:"evidence_refs"`
	EvidenceEvaluationID  *string    `json:"evidence_evaluation_id,omitempty"`
	EvidenceOutcome       *string    `json:"evidence_outcome,omitempty"`
	EvidenceExemptReason  *string    `json:"evidence_exempt_reason,omitempty"`
	DomainConfirmationRef *string    `json:"domain_confirmation_ref,omitempty"`
	SubjectState          *string    `json:"subject_state,omitempty"`

	// Waiver / not applicable (s7.1).
	AuthorityAction         *string    `json:"authority_action,omitempty"`
	RequestedBy             *string    `json:"requested_by,omitempty"`
	ApprovedBy              *string    `json:"approved_by,omitempty"`
	ApprovalReference       *string    `json:"approval_reference,omitempty"`
	ReasonCode              *string    `json:"reason_code,omitempty"`
	ReasonText              *string    `json:"reason_text,omitempty"`
	AuthorityEvidenceRefs   []string   `json:"authority_evidence_refs"`
	ApplicabilityDecisionID *string    `json:"applicability_decision_id,omitempty"`
	WaivedAt                *time.Time `json:"waived_at,omitempty"`
	NotApplicableAt         *time.Time `json:"not_applicable_at,omitempty"`

	// Lateness (NP-38): set once, never cleared.
	OverdueSince *time.Time `json:"overdue_since,omitempty"`

	// Escalation (s7.4): priority and owners only.
	EscalationPolicyID      *string  `json:"escalation_policy_id,omitempty"`
	EscalationPolicyVersion *int     `json:"escalation_policy_version,omitempty"`
	Priority                string   `json:"priority"`
	AdditionalOwners        []string `json:"additional_owners"`

	// Common columns (s9.1).
	Plane           string    `json:"plane"`
	DataClass       string    `json:"data_class"`
	ResidencyRegion *string   `json:"residency_region,omitempty"`
	CorrelationID   *string   `json:"correlation_id,omitempty"`
	CreatedBy       *string   `json:"created_by,omitempty"`
	UpdatedBy       *string   `json:"updated_by,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	RowVersion      int       `json:"row_version"`
}

// DueDayStart is the first instant of the clock's local due date in its time
// zone (the DUE state starts there). A clock without a usable zone or date
// falls back to 24 hours before due_at.
func (c *Clock) DueDayStart() time.Time {
	loc, err := time.LoadLocation(c.Timezone)
	if err == nil && c.LocalDueDate != "" {
		if d, perr := time.ParseInLocation("2006-01-02", c.LocalDueDate, loc); perr == nil {
			return d
		}
	}
	return c.DueAt.Add(-24 * time.Hour)
}

// Resolve sets ClockState to the effective state at now.
func (c *Clock) Resolve(now time.Time) {
	c.ClockState = EffectiveState(c.StoredState, now, c.DueAt, c.DueDayStart(), c.DueSoonAt)
}

// TriggerEvidence is what started a clock (R-001 s7.1, NP-12). A backdated
// correction never erases the original trigger.
type TriggerEvidence struct {
	EventID             string     `json:"event_id"`
	EventType           string     `json:"event_type"`
	EventTime           time.Time  `json:"event_time"`
	BusinessEventTime   *time.Time `json:"business_event_time,omitempty"`
	SourceObjectType    string     `json:"source_object_type"`
	SourceObjectID      string     `json:"source_object_id"`
	SourceObjectVersion *string    `json:"source_object_version,omitempty"`
	IngestedAt          time.Time  `json:"ingested_at"`
	// IngestionLatencyMS is ingested_at minus the event time: the detection
	// latency NP-12 asks to record separately from the business time.
	IngestionLatencyMS *int64 `json:"ingestion_latency_ms,omitempty"`
}

// DueFacts are the facts jurisdiction-rules-svc computes a due date from.
type DueFacts struct {
	PeriodEnd            string `json:"period_end,omitempty"`
	EventDate            string `json:"event_date,omitempty"`
	RegistrationDate     string `json:"registration_date,omitempty"`
	AnniversaryDate      string `json:"anniversary_date,omitempty"`
	AnchorYear           int    `json:"anchor_year,omitempty"`
	ExtensionDays        int    `json:"extension_days,omitempty"`
	ExtensionEvidenceRef string `json:"extension_evidence_ref,omitempty"`
}

// Calculation is jurisdiction-rules-svc's answer, reduced to what a clock
// stores. Outcome other than DUE_DATE_CALCULATED never makes a clock.
type Calculation struct {
	DecisionID            string
	Outcome               string
	Message               string
	ObligationRuleID      string
	RuleVersion           int
	ContentDigest         string
	AnchorDate            string
	UnadjustedDate        string
	DueDate               string
	DueAt                 *time.Time
	Timezone              string
	CalendarCode          string
	CalendarVersion       int
	CalendarVersionID     string
	BusinessDayAdjustment string
	ExtensionGranted      int
	PackRef               string
	PackVersion           string
	PackVersionID         string
	Explanation           json.RawMessage
}

// CalculationOutcomeCalculated is the one outcome that makes a clock.
const CalculationOutcomeCalculated = "DUE_DATE_CALCULATED"

// MilestoneSpec is one entry of a warning or escalation schedule, relative to
// due_at (negative = before).
type MilestoneSpec struct {
	Kind          string         `json:"kind"`
	OffsetSeconds int64          `json:"offset_seconds"`
	Action        string         `json:"action"`
	Params        map[string]any `json:"params,omitempty"`
}

// Milestone kinds and actions (R-001 s7.1, s7.4).
const (
	MilestoneWarning    = "WARNING"
	MilestoneEscalation = "ESCALATION"

	ActionMarkDueSoon     = "MARK_DUE_SOON"
	ActionRaisePriority   = "RAISE_PRIORITY"
	ActionAddOwner        = "ADD_OWNER"
	ActionOpenCase        = "OPEN_CASE"
	ActionNotify          = "NOTIFY"
	ActionRequireApproval = "REQUIRE_APPROVAL"
)

// MilestoneActions is the set of valid actions; MilestoneKinds the kinds.
var (
	MilestoneActions = map[string]bool{ActionMarkDueSoon: true, ActionRaisePriority: true, ActionAddOwner: true,
		ActionOpenCase: true, ActionNotify: true, ActionRequireApproval: true}
	MilestoneKinds = map[string]bool{MilestoneWarning: true, MilestoneEscalation: true}
)

// ValidMilestone reports why a milestone spec is unusable ("" if usable).
func ValidMilestone(m MilestoneSpec) string {
	switch {
	case !MilestoneKinds[m.Kind]:
		return "kind must be WARNING or ESCALATION"
	case !MilestoneActions[m.Action]:
		return "action must be MARK_DUE_SOON, RAISE_PRIORITY, ADD_OWNER, OPEN_CASE, NOTIFY or REQUIRE_APPROVAL"
	case m.Action == ActionMarkDueSoon && m.Kind != MilestoneWarning:
		return "MARK_DUE_SOON is a WARNING milestone"
	case m.Action == ActionMarkDueSoon && m.OffsetSeconds >= 0:
		return "MARK_DUE_SOON must fire before due_at (negative offset_seconds)"
	case m.OffsetSeconds < -366*24*3600 || m.OffsetSeconds > 366*24*3600:
		return "offset_seconds must be within a year of due_at"
	case m.Action == ActionAddOwner:
		if s, _ := m.Params["owner"].(string); s == "" {
			return "ADD_OWNER needs params.owner"
		}
	case m.Action == ActionRaisePriority:
		if s, _ := m.Params["priority"].(string); s != "HIGH" && s != "CRITICAL" {
			return "RAISE_PRIORITY needs params.priority HIGH or CRITICAL"
		}
	}
	return ""
}

// Milestone is a scheduled warning or escalation of one clock.
type Milestone struct {
	MilestoneID             string         `json:"milestone_id"`
	ClockID                 string         `json:"clock_id"`
	Kind                    string         `json:"kind"`
	OffsetSeconds           int64          `json:"offset_seconds"`
	FireAt                  time.Time      `json:"fire_at"`
	Action                  string         `json:"action"`
	Params                  map[string]any `json:"params"`
	EscalationPolicyID      *string        `json:"escalation_policy_id,omitempty"`
	EscalationPolicyVersion *int           `json:"escalation_policy_version,omitempty"`
	FiredAt                 *time.Time     `json:"fired_at,omitempty"`
}

// ClockTransition is one append-only history row of a clock.
type ClockTransition struct {
	TransitionID  string    `json:"transition_id"`
	ClockID       string    `json:"clock_id"`
	FromState     *string   `json:"from_state,omitempty"`
	ToState       string    `json:"to_state"`
	Command       string    `json:"command"`
	Actor         string    `json:"actor"`
	ReasonCode    *string   `json:"reason_code,omitempty"`
	ReasonText    *string   `json:"reason_text,omitempty"`
	EvidenceRefs  []string  `json:"evidence_refs"`
	RowVersion    int       `json:"row_version"`
	EffectiveAt   time.Time `json:"effective_at"`
	RecordedAt    time.Time `json:"recorded_at"`
	CorrelationID *string   `json:"correlation_id,omitempty"`
}

// EscalationPolicy is a versioned, immutable escalation schedule.
type EscalationPolicy struct {
	EscalationPolicyID string          `json:"escalation_policy_id"`
	PolicyCode         string          `json:"policy_code"`
	Version            int             `json:"version"`
	Status             string          `json:"status"`
	Milestones         []MilestoneSpec `json:"milestones"`
	EffectiveFrom      time.Time       `json:"effective_from"`
	EffectiveTo        *time.Time      `json:"effective_to,omitempty"`
	CreatedBy          string          `json:"created_by"`
	CreatedAt          time.Time       `json:"created_at"`
	RowVersion         int             `json:"row_version"`
}

// EscalationEvent is one append-only record of an escalation action.
type EscalationEvent struct {
	EscalationEventID string         `json:"escalation_event_id"`
	ClockID           string         `json:"clock_id"`
	MilestoneID       *string        `json:"milestone_id,omitempty"`
	Action            string         `json:"action"`
	Outcome           string         `json:"outcome"`
	ResultRef         *string        `json:"result_ref,omitempty"`
	Detail            map[string]any `json:"detail"`
	OccurredAt        time.Time      `json:"occurred_at"`
}

// StartClockParams creates a clock from a calculation.
type StartClockParams struct {
	ObligationID       string
	ClockKey           string
	RequestHash        string
	ClockClass         ClockClass
	Planned            bool
	JurisdictionCode   string
	RuleObligationCode string
	Calc               Calculation
	Trigger            TriggerEvidence
	DueSoonAt          *time.Time
	EscalationPolicyID *string
	ExtraMilestones    []MilestoneSpec
	SupersedesClockID  *string // set by recalculation
	SupersessionReason *string
	// CalculationRequest is the jurisdiction request (jurisdiction, code,
	// facts, pinned pack) kept so a recalculation asks the same question.
	CalculationRequest json.RawMessage
	// SystemTenantID is set by system workers (calendar consumer), which act
	// on a clock outside any request.
	SystemTenantID string
	Actor          string
	CorrelationID  string
	CausationID    string
}

// ClockCommandParams carries one named command on one clock.
type ClockCommandParams struct {
	ClockID         string
	Command         string
	ExpectedVersion int // 0 = no If-Match (only the system sweep omits it)
	Actor           string
	CorrelationID   string
	Now             time.Time

	// satisfy
	EvidenceRefs          []string
	EvidenceExemptReason  *string
	EvidenceEvaluationID  *string
	EvidenceOutcome       *string
	DomainConfirmationRef *string
	SubjectState          *string

	// waive / not applicable
	ReasonCode              *string
	ReasonText              *string
	ApprovalReference       *string
	ApprovedBy              *string
	ApplicabilityDecisionID *string
}

// ListClocksFilter filters a clock list (API-4 keyset cursor).
type ListClocksFilter struct {
	ObligationID   string
	ClockClass     ClockClass
	StoredState    ClockState
	Limit          int
	AfterCreatedAt *time.Time
	AfterID        string
}

// ObligationSummary is the console KPI read (OB-S12).
type ObligationSummary struct {
	AsOf                  time.Time      `json:"as_of"`
	ObligationsByStatus   map[string]int `json:"obligations_by_status"`
	ObligationsBySource   map[string]int `json:"obligations_by_source"`
	ApplicableObligations int            `json:"applicable_obligations"`
	DueWithin30Days       int            `json:"due_within_30_days"`
	OverdueClocks         int            `json:"overdue_clocks"`
	ClocksByState         map[string]int `json:"clocks_by_state"`
	ClocksByClass         map[string]int `json:"clocks_by_class"`
	FulfilledClocks       int            `json:"fulfilled_clocks"`
	FulfilledWithEvidence int            `json:"fulfilled_with_evidence"`
	FulfilledLate         int            `json:"fulfilled_late"`
	AwaitingReview        *int           `json:"awaiting_review"`
	AwaitingReviewNote    string         `json:"awaiting_review_note,omitempty"`
}

// AuthoritySubjectFingerprint is the subject fingerprint an approval of a
// waiver or a not-applicable decision is bound to in workflow-svc (R-INV-07):
// the clock, the action, the reason code and the evidence. A waiver approved
// for one reason or one set of evidence cannot be reused for another.
func AuthoritySubjectFingerprint(clockID, action, reasonCode string, evidenceRefs []string) string {
	refs := append([]string(nil), evidenceRefs...)
	sort.Strings(refs)
	raw, _ := json.Marshal(struct {
		ClockID      string   `json:"clock_id"`
		Action       string   `json:"action"`
		ReasonCode   string   `json:"reason_code"`
		EvidenceRefs []string `json:"evidence_refs"`
	}{clockID, action, reasonCode, refs})
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Clock errors.
var (
	ErrClockNotFound            = errorString("clock not found")
	ErrClockCommandNotAllowed   = errorString("the clock's state does not allow this command")
	ErrClockVersionMismatch     = errorString("clock version mismatch: the clock has been modified by another request")
	ErrClockKeyConflict         = errorString("clock key already used for a different request")
	ErrEvidenceMissing          = errorString("required evidence is missing")
	ErrApprovalRequired         = errorString("an approved workflow decided by another principal is required")
	ErrCalculationRefused       = errorString("jurisdiction-rules-svc did not calculate a due date")
	ErrWorkflowUnavailable      = errorString("workflow-svc unavailable")
	ErrEvidenceUnavailable      = errorString("evidence-requirements-svc unavailable")
	ErrEscalationPolicyNotFound = errorString("escalation policy not found")
	ErrApplicabilityMismatch    = errorString("the applicability decision does not resolve this obligation as not applicable")
)
