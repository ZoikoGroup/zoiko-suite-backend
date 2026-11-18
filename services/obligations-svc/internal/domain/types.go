// Package domain contains the authoritative domain types for obligations-svc.
//
// obligation_source_type, obligation_type, obligation_status, and
// severity_level are all plain strings — no Go enums, iota, or switch/case
// branches in validation logic. New values are added via data only, no code
// change required, same doctrine as policy-svc's policy_type and
// jurisdiction-rules-svc's jurisdiction_type/rule_domain fields. Only the
// obligation_status state-machine transition check switches on status
// values, since that's an actual state machine, not an open-ended tag.
package domain

import "time"

// Obligation tracks a single statutory, regulatory, contractual, or internal
// policy obligation linked to an operational action.
//
// Critical constraint (03-microservices.md §8.5): every obligation must be
// entity-bound (LegalEntityID) and jurisdiction-bound (JurisdictionID) —
// neither field may ever be empty.
//
// Critical enhancement — "Atomic Linking": every obligation must be able to
// point to its originating source (a contract clause, filing rule, policy
// mandate, or jurisdiction rule reference). SourceReference is required for
// exactly this reason and must never be empty.
//
// Spec §8.5: "Owns: obligation-to-workflow mappings" — workflow_id links to
// a workflow & approvals service instance.
//
// No hard-delete: an obligation is closed via an ObligationStatus transition
// to CLOSED (ClosedAt stamped), never deleted.
type Obligation struct {
	ObligationID string `json:"obligation_id"`

	LegalEntityID  string `json:"legal_entity_id"`
	JurisdictionID string `json:"jurisdiction_id"`

	// WorkflowID links this obligation to a workflow instance in the
	// workflow & approvals service. Optional — not all obligations are
	// workflow-driven.
	WorkflowID *string `json:"workflow_id,omitempty"`

	// ObligationSourceType/ObligationSourceID identify the originating
	// record (e.g. "CONTRACT_CLAUSE", "FILING_RULE", "POLICY_MANDATE",
	// "JURISDICTION_RULE") in whatever service owns that source. Not a
	// foreign key — the source may live in a different service entirely.
	ObligationSourceType string `json:"obligation_source_type"`
	ObligationSourceID   string `json:"obligation_source_id"`

	// ObligationCode is a stable, human-readable identifier and the
	// idempotent creation dedup key — DATA ONLY, never a code switch/case.
	ObligationCode string `json:"obligation_code"`

	// ObligationType is a VARCHAR tag stored as data (e.g. "FILING",
	// "TAX_PAYMENT", "REGULATORY_REPORT"). New types require a data
	// migration only, never a code change.
	ObligationType string `json:"obligation_type"`

	// ObligationStatus: OPEN | IN_PROGRESS | OVERDUE | CLOSED. This one IS
	// a real state machine — see store.transitionObligationStatus for the
	// enforced legal transitions.
	ObligationStatus string `json:"obligation_status"`

	DueDate time.Time `json:"due_date"`

	// SeverityLevel is data only (e.g. "LOW", "MEDIUM", "HIGH", "CRITICAL").
	SeverityLevel string `json:"severity_level"`

	// ResponsibleFunction is a free-text/data tag identifying the owning
	// business function (e.g. "Finance", "Legal", "Tax").
	ResponsibleFunction string `json:"responsible_function"`

	// SourceReference is the human-readable Atomic Linking reference
	// (e.g. "Contract #4821 Clause 12.3", "IN-GST-FILING-RULE-07").
	// Required, never empty — see doc comment above.
	SourceReference string `json:"source_reference"`

	CreatedAt            time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`

	// UpdatedAt is stamped on every status transition.
	UpdatedAt time.Time `json:"updated_at"`

	// ClosedAt is nil until the obligation transitions to CLOSED, set
	// exactly once, never unset or overwritten afterwards.
	ClosedAt *time.Time `json:"closed_at"`

	// Version is the optimistic locking version. Incremented on every
	// successful update. Used with If-Match header for concurrency control.
	Version int `json:"version"`

	// ObligationDefinitionID and DefinitionVersion pin the definition version
	// the obligation was raised from; both nil for a free-standing obligation.
	ObligationDefinitionID *string `json:"obligation_definition_id,omitempty"`
	DefinitionVersion      *int    `json:"definition_version,omitempty"`
}

// FilingRequirement is a filing obligation scoped under a parent Obligation.
type FilingRequirement struct {
	FilingRequirementID string `json:"filing_requirement_id"`
	ObligationID        string `json:"obligation_id"`

	FilingType        string `json:"filing_type"`
	FilingAuthority   string `json:"filing_authority"`
	SubmissionChannel string `json:"submission_channel"`

	// FilingStatus follows FilingStatuses: PENDING → IN_PREPARATION →
	// SUBMITTED → ACCEPTED | REJECTED, with WITHDRAWN as an exit.
	FilingStatus string `json:"filing_status"`

	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	RowVersion int       `json:"row_version"`
}

// CreateObligationParams holds input parameters for creating an obligation.
type CreateObligationParams struct {
	ObligationID         string    `json:"obligation_id"`
	LegalEntityID        string    `json:"legal_entity_id"`
	JurisdictionID       string    `json:"jurisdiction_id"`
	ObligationSourceType string    `json:"obligation_source_type"`
	ObligationSourceID   string    `json:"obligation_source_id"`
	ObligationCode       string    `json:"obligation_code"`
	ObligationType       string    `json:"obligation_type"`
	DueDate              time.Time `json:"due_date"`
	SeverityLevel        string    `json:"severity_level"`
	ResponsibleFunction  string    `json:"responsible_function"`
	SourceReference      string    `json:"source_reference"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
	WorkflowID           *string   `json:"workflow_id,omitempty"`
	// CorrelationID is the caller's trace id. The store uses it to write the
	// obligation.created outbox record in the same transaction as the insert.
	CorrelationID string `json:"-"`

	// ObligationDefinitionID raises the obligation from a definition; the
	// store pins the definition's current version and refuses an entity the
	// definition is not mapped to.
	ObligationDefinitionID string `json:"-"`
	DefinitionVersion      int    `json:"-"`
}

// ListObligationsFilter holds optional filters for querying obligations.
// Zero-value (empty string / nil) fields are not applied.
type ListObligationsFilter struct {
	// TenantID is optional on the filter and mandatory in the store, which
	// reads it from the request context. Supplying a different one here is
	// refused: a filter may narrow a read, never widen it to another tenant.
	TenantID       string
	LegalEntityID  string
	JurisdictionID string
	ObligationType string
	Status         string
	DueBefore      *time.Time
	DueAfter       *time.Time

	// Limit and Offset bound the register read. It was unbounded — every
	// obligation a tenant had ever recorded, in one response, forever.
	Limit  int
	Offset int

	// AfterCreatedAt / AfterID continue a keyset (cursor) read after the last
	// row of the previous page, in the list's created_at DESC, id DESC order.
	AfterCreatedAt *time.Time
	AfterID        string
}

// CreateFilingRequirementParams holds input parameters for creating a
// filing requirement under an obligation.
type CreateFilingRequirementParams struct {
	FilingRequirementID string
	ObligationID        string
	FilingType          string
	FilingAuthority     string
	SubmissionChannel   string
}

// CreateComplianceStatusParams holds input parameters for creating a compliance status.
type CreateComplianceStatusParams struct {
	ComplianceStatusID       string
	TenantID                 string
	LegalEntityID            string
	DomainCode               string
	StatusCode               string
	StatusReason             string
	EvidenceSufficiencyState string
	EvaluatedAt              time.Time
	CreatedByPrincipalID     string
	CorrelationID            string
}

// CreateExceptionCaseParams holds input parameters for creating an exception case.
type CreateExceptionCaseParams struct {
	ExceptionCaseID        string
	TenantID               string
	LegalEntityID          string
	ExceptionType          string
	SeverityLevel          string
	LinkedObjectType       string
	LinkedObjectID         string
	CreatedByPrincipalID   string
	CorrelationID          string
}

// CreateEscalationRecordParams holds input parameters for creating an escalation record.
type CreateEscalationRecordParams struct {
	EscalationRecordID     string
	ExceptionCaseID        string
	EscalatedToRole        string
	EscalationReason       string
	CreatedByPrincipalID   string
	CorrelationID          string
}

// ListComplianceStatusFilter holds optional filters for querying compliance statuses.
type ListComplianceStatusFilter struct {
	TenantID       string
	LegalEntityID  string
	DomainCode     string
	StatusCode     string
	Limit          int
	Offset         int
}

// ListExceptionCasesFilter holds optional filters for querying exception cases.
type ListExceptionCasesFilter struct {
	TenantID       string
	LegalEntityID  string
	ExceptionType  string
	CaseStatus     string
	Limit          int
	Offset         int
}

// ErrObligationNotFound is returned when an obligation_id does not exist.
var ErrObligationNotFound = errorString("obligation not found")

// ErrInvalidTransition is returned when an obligation_status transition is
// illegal per the state machine (e.g. transitioning out of CLOSED).
var ErrInvalidTransition = errorString("invalid obligation status transition")

// ErrConflict is returned when an idempotent creation request matches an
// existing record's dedup key but has differing attributes (409 Conflict).
var ErrConflict = errorString("conflict: record already exists with differing attributes")

// ErrStoreUnavailable is returned when the database cannot be reached.
// Callers must fail-closed — treat as unavailable, not as "not found".
var ErrStoreUnavailable = errorString("obligations store unavailable")

// ErrJurisdictionNotFound is returned when JurisdictionID does not resolve
// against jurisdiction-rules-svc.
var ErrJurisdictionNotFound = errorString("jurisdiction not found")

// ErrJurisdictionServiceUnavailable is returned when jurisdiction-rules-svc
// cannot be reached to validate JurisdictionID.
var ErrJurisdictionServiceUnavailable = errorString("jurisdiction-rules-svc unavailable")

// ErrTenantMissing is returned when a request carries no X-Tenant-Id. It is an
// unscoped request, refused rather than defaulted to some shared bucket.
var ErrTenantMissing = errorString("tenant scope missing")

// ErrAuthorizationDenied is returned when authorization-svc answers DENIED.
var ErrAuthorizationDenied = errorString("not authorized for this obligation action")

// ErrAuthorizationUnavailable is returned when authorization-svc cannot be
// reached or gives an answer this service cannot read. Mutations fail closed.
var ErrAuthorizationUnavailable = errorString("authorization service unavailable")

// ErrInvalidIdentifier is what a malformed UUID becomes. obligation_id and the
// entity/jurisdiction ids are uuid columns, so a mistyped one dies inside the
// driver as SQLSTATE 22P02 before any row is examined — and used to surface as
// 503 store_unavailable, an outage status for a typo in a URL.
var ErrInvalidIdentifier = errorString("identifier is not a valid UUID")

// ErrVersionMismatch is returned when an optimistic locking check fails
// (expected_version/If-Match header value doesn't match current version).
var ErrVersionMismatch = errorString("version mismatch: the resource has been modified by another request")

// ErrIdempotencyKeyConflict is returned when an Idempotency-Key is reused
// with a different request payload.
var ErrIdempotencyKeyConflict = errorString("idempotency key conflict: key already used with different request")

// ErrIdentityMissing is returned when a request carries no X-Principal-Id.
var ErrIdentityMissing = errorString("identity missing")

// ErrForbidden is returned when authorization check fails (403).
var ErrForbidden = errorString("forbidden")

// ErrMissingField is returned when a required field is missing from the request.
var ErrMissingField = errorString("missing field")

// ErrInvalidField is returned when a field value is invalid.
var ErrInvalidField = errorString("invalid field")

// ErrInvalidJSON is returned when the request body is not valid JSON.
var ErrInvalidJSON = errorString("invalid JSON")

// ErrUnknownField is returned when the request contains unknown fields.
var ErrUnknownField = errorString("unknown field")

// ErrRequestTooLarge is returned when the request body exceeds the size limit.
var ErrRequestTooLarge = errorString("request too large")

// ErrInvalidLimit is returned when the limit query parameter is invalid.
var ErrInvalidLimit = errorString("invalid limit")

// ErrInvalidOffset is returned when the offset query parameter is invalid.
var ErrInvalidOffset = errorString("invalid offset")

// ErrInvalidIfMatch is returned when the If-Match header is invalid.
var ErrInvalidIfMatch = errorString("invalid If-Match header")

// ErrEnvelopeIncomplete is returned when the envelope validation fails.
var ErrEnvelopeIncomplete = errorString("envelope incomplete")

// ErrWorkflowAlreadyLinked is returned when an obligation is already linked to a workflow.
var ErrWorkflowAlreadyLinked = errorString("workflow already linked")

// ErrWorkflowNotLinked is returned when an obligation is not linked to a workflow.
var ErrWorkflowNotLinked = errorString("workflow not linked")

// ErrCreatedByMismatch is returned when created_by_principal_id doesn't match the authenticated principal.
var ErrCreatedByMismatch = errorString("created_by mismatch")

// ErrLegalEntityMismatch is returned when X-Legal-Entity-Id doesn't match body legal_entity_id.
var ErrLegalEntityMismatch = errorString("legal entity mismatch")

// ComplianceStatus represents the compliance status of a legal entity for a domain.
type ComplianceStatus struct {
	ComplianceStatusID       string    `json:"compliance_status_id"`
	TenantID                 string    `json:"tenant_id"`
	LegalEntityID            string    `json:"legal_entity_id"`
	DomainCode               string    `json:"domain_code"`
	StatusCode               string    `json:"status_code"`
	StatusReason             string    `json:"status_reason"`
	EvaluatedAt              time.Time `json:"evaluated_at"`
	EvidenceSufficiencyState string    `json:"evidence_sufficiency_state"`
	CreatedAt                time.Time `json:"created_at"`
	CreatedByPrincipalID     string    `json:"created_by_principal_id"`
	UpdatedAt                time.Time `json:"updated_at"`
	Version                  int       `json:"version"`
}

// ExceptionCase represents an exception case for a compliance/obligation issue.
type ExceptionCase struct {
	ExceptionCaseID   string    `json:"exception_case_id"`
	TenantID          string    `json:"tenant_id"`
	LegalEntityID     string    `json:"legal_entity_id"`
	ExceptionType     string    `json:"exception_type"`
	SeverityLevel     string    `json:"severity_level"`
	LinkedObjectType  string    `json:"linked_object_type"`
	LinkedObjectID    string    `json:"linked_object_id"`
	CaseStatus        string    `json:"case_status"`
	EscalatedAt       *time.Time `json:"escalated_at,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	CreatedByPrincipalID string  `json:"created_by_principal_id"`
	UpdatedAt         time.Time `json:"updated_at"`
	Version           int       `json:"version"`
}

// EscalationRecord represents an escalation record for an exception case.
type EscalationRecord struct {
	EscalationRecordID  string    `json:"escalation_record_id"`
	ExceptionCaseID     string    `json:"exception_case_id"`
	EscalatedToRole     string    `json:"escalated_to_role"`
	EscalationReason    string    `json:"escalation_reason"`
	EscalationStatus    string    `json:"escalation_status"`
	EscalatedAt         time.Time `json:"escalated_at"`
	CreatedAt           time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
}

// ErrComplianceStatusNotFound is returned when a compliance status is not found.
var ErrComplianceStatusNotFound = errorString("compliance_status not found")

// ErrExceptionCaseNotFound is returned when an exception case is not found.
var ErrExceptionCaseNotFound = errorString("exception_case not found")

// ErrEscalationRecordNotFound is returned when an escalation record is not found.
var ErrEscalationRecordNotFound = errorString("escalation_record not found")

type errorString string

func (e errorString) Error() string { return string(e) }

// =============================================================================
// WF-3: Deadline Clocks (WFC-05)
// =============================================================================
// A "clock" is a computed deadline with calendar provenance, attached to an
// obligation. An obligation may have multiple clocks (e.g. a HARD_LEGAL clock
// for the statutory filing deadline, and a SOFT_INTERNAL clock for the team's
// internal review). Clocks are evaluated by a background worker that sweeps
// them against their computed deadlines and transitions through states.
//
// Clock classes (data, not code): HARD_LEGAL, SOFT_INTERNAL, ADVISORY, etc.
// States: PLANNED → ACTIVE → DUE_SOON → DUE → OVERDUE → (FULFILLED | WAIVED | NOT_APPLICABLE | SUPERSEDED)
// FULFILLED/WAIVED/NOT_APPLICABLE/SUPERSEDED are terminal. Only ACTIVE clocks
// are evaluated by the sweep.
//
// Calendar provenance: each clock's deadline is computed from a jurisdiction-
// rules business calendar (D2-1.F08). The calendar code and rule version are
// stored so a consumer can reproduce the deadline.

// ClockClass is a tag identifying the clock's regime. Data only — no code switch.
type ClockClass string

const (
	ClockClassHardLegal     ClockClass = "HARD_LEGAL"     // statutory/regulatory deadline, fail-closed on miss
	ClockClassSoftInternal  ClockClass = "SOFT_INTERNAL"  // internal review deadline, warning only
	ClockClassAdvisory      ClockClass = "ADVISORY"       // no enforcement, informational
	ClockClassExtension     ClockClass = "EXTENSION"      // granted extension period
)

// ClockState is the state of a clock. This IS a state machine.
type ClockState string

const (
	ClockStatePlanned         ClockState = "PLANNED"          // created, deadline not yet active
	ClockStateActive          ClockState = "ACTIVE"           // deadline active, being tracked
	ClockStateDueSoon         ClockState = "DUE_SOON"         // within warning window (DUE_SOON threshold)
	ClockStateDue             ClockState = "DUE"              // deadline day reached
	ClockStateOverdue         ClockState = "OVERDUE"          // deadline passed without satisfaction
	ClockStateFulfilled       ClockState = "FULFILLED"        // satisfied with evidence (terminal)
	ClockStateWaived          ClockState = "WAIVED"           // waived with authority + evidence (terminal)
	ClockStateNotApplicable   ClockState = "NOT_APPLICABLE"   // N-A with authority (terminal)
	ClockStateSuperseded      ClockState = "SUPERSEDED"       // superseded by calendar correction (terminal)
)

// ClockStatuses is the set of valid clock states for validation.
var ClockStatuses = map[ClockState]bool{
	ClockStatePlanned: true, ClockStateActive: true, ClockStateDueSoon: true,
	ClockStateDue: true, ClockStateOverdue: true, ClockStateFulfilled: true,
	ClockStateWaived: true, ClockStateNotApplicable: true, ClockStateSuperseded: true,
}

// Clock represents a computed deadline clock attached to an obligation.
type Clock struct {
	ClockID              string     `json:"clock_id"`
	ObligationID         string     `json:"obligation_id"`
	TenantID             string     `json:"tenant_id"`
	LegalEntityID        string     `json:"legal_entity_id"`

	ClockClass           ClockClass `json:"clock_class"`
	ClockState           ClockState `json:"clock_state"`

	// Deadline computation
	CalendarCode         string     `json:"calendar_code"`          // jurisdiction-rules calendar code
	CalendarRuleID       string     `json:"calendar_rule_id"`       // jurisdiction-rules rule that produced this deadline
	CalendarRuleVersion  int        `json:"calendar_rule_version"`  // version of the rule at computation time
	ComputedDeadline     time.Time  `json:"computed_deadline"`      // the actual deadline timestamp
	DueSoonThreshold     time.Duration `json:"due_soon_threshold"`  // warning window before deadline (e.g. 7d)

	// Escalation
	EscalationLadder     []EscalationStep `json:"escalation_ladder"` // ordered steps when overdue

	// Satisfaction / Waiver / N-A
	SatisfiedAt          *time.Time `json:"satisfied_at,omitempty"`
	SatisfiedByPrincipal *string    `json:"satisfied_by_principal,omitempty"`
	SatisfiedEvidenceIDs []string   `json:"satisfied_evidence_ids,omitempty"` // evaluation IDs
	WaivedAt             *time.Time `json:"waived_at,omitempty"`
	WaivedByPrincipal    *string    `json:"waived_by_principal,omitempty"`
	WaivedReason         *string    `json:"waived_reason,omitempty"`
	WaivedEvidenceIDs    []string   `json:"waived_evidence_ids,omitempty"`
	NotApplicableAt      *time.Time `json:"not_applicable_at,omitempty"`
	NotApplicableByPrincipal *string `json:"not_applicable_by_principal,omitempty"`
	NotApplicableReason  *string    `json:"not_applicable_reason,omitempty"`
	SupersededAt         *time.Time `json:"superseded_at,omitempty"`
	SupersededByClockID  *string    `json:"superseded_by_clock_id,omitempty"`

	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
	Version              int       `json:"version"` // optimistic lock for clock transitions
}

// EscalationStep is one rung of an escalation ladder.
type EscalationStep struct {
	StepOrder       int           `json:"step_order"`        // 1-based
	AfterOverdueBy  time.Duration `json:"after_overdue_by"`  // e.g. 24h, 72h
	EscalatedToRole string        `json:"escalated_to_role"` // role code to notify/assign
	ActionType      string        `json:"action_type"`       // NOTIFY, REASSIGN, INVALIDATE
}

// CreateClockParams holds input for creating a clock on an obligation.
type CreateClockParams struct {
	ObligationID         string
	ClockClass           ClockClass
	CalendarCode         string
	CalendarRuleID       string
	CalendarRuleVersion  int
	ComputedDeadline     time.Time
	DueSoonThreshold     time.Duration
	EscalationLadder     []EscalationStep
	CreatedByPrincipalID string
	CorrelationID        string
}

// ListClocksFilter holds optional filters for querying clocks.
type ListClocksFilter struct {
	TenantID       string
	LegalEntityID  string
	ObligationID   string
	ClockClass     ClockClass
	ClockState     ClockState
	Limit          int
	Offset         int
	AfterCreatedAt *time.Time
	AfterID        string
}

// ListFilingRequirementsFilter holds optional filters for querying filing requirements.
type ListFilingRequirementsFilter struct {
	ObligationID string
	Limit        int
	Offset       int
	AfterCreatedAt *time.Time
	AfterID      string
}

// ClockTransitionParams holds input for transitioning a clock.
type ClockTransitionParams struct {
	ClockID              string
	NewState             ClockState
	ExpectedVersion      int
	ActorPrincipalID     string
	CorrelationID        string
	// For FULFILLED:
	EvidenceEvaluationIDs []string
	// For WAIVED:
	Reason          *string
	AuthorityPrincipalID *string
	// For NOT_APPLICABLE:
	NotApplicableReason *string
	NotApplicableAuthorityPrincipalID *string
	// For SUPERSEDED:
	SupersededByClockID *string
}

// ErrClockNotFound is returned when a clock_id does not exist.
var ErrClockNotFound = errorString("clock not found")

// ErrInvalidClockTransition is returned when a clock_state transition is illegal.
var ErrInvalidClockTransition = errorString("invalid clock state transition")

// ErrClockVersionMismatch is returned when an optimistic locking check fails on a clock.
var ErrClockVersionMismatch = errorString("clock version mismatch: the clock has been modified by another request")

// ErrWaiverAuthorityRequired is returned when a waiver lacks required authority.
var ErrWaiverAuthorityRequired = errorString("waiver requires authority principal")

// ErrNotApplicableAuthorityRequired is returned when N-A lacks required authority.
var ErrNotApplicableAuthorityRequired = errorString("not applicable requires authority principal")

// ClockStateTransitions defines the legal state transitions for a clock.
// From -> allowed To states.
var ClockStateTransitions = map[ClockState][]ClockState{
	ClockStatePlanned:       {ClockStateActive, ClockStateSuperseded},
	ClockStateActive:        {ClockStateDueSoon, ClockStateDue, ClockStateFulfilled, ClockStateWaived, ClockStateNotApplicable, ClockStateSuperseded},
	ClockStateDueSoon:       {ClockStateDue, ClockStateFulfilled, ClockStateWaived, ClockStateNotApplicable, ClockStateSuperseded},
	ClockStateDue:           {ClockStateOverdue, ClockStateFulfilled, ClockStateWaived, ClockStateNotApplicable, ClockStateSuperseded},
	ClockStateOverdue:       {ClockStateFulfilled, ClockStateWaived, ClockStateNotApplicable, ClockStateSuperseded},
	// Terminal states: no transitions out
	ClockStateFulfilled:     {},
	ClockStateWaived:        {},
	ClockStateNotApplicable: {},
	ClockStateSuperseded:    {},
}
