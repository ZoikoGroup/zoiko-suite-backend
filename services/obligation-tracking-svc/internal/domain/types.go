package domain

import (
	"errors"
	"time"
)

var (
	ErrObligationNotFound = errors.New("obligation not found")

	// ErrTenantMissing means the request carried no X-Tenant-Id. It is an
	// unauthenticated request, not an empty tenant named "default".
	ErrTenantMissing = errors.New("tenant scope missing")

	// ErrWrongStatus means the obligation is not in the status a command
	// requires — e.g. Schedule called on an obligation not in PLANNED.
	ErrWrongStatus = errors.New("obligation is not in the required status for this action")

	// ErrNotValidated means ValidateExtractedObligation has not run yet —
	// only relevant to an AI-extracted (CANDIDATE) obligation.
	ErrNotValidated = errors.New("obligation has not been validated")

	// ErrAmbiguousDueDateBasis means Schedule was called without both a
	// trigger description and a calculation method. LEG-07 §9: "trigger/date
	// ambiguity blocks due-date certification."
	ErrAmbiguousDueDateBasis = errors.New("trigger_description and calculation_method are required to certify a due date")

	// ErrWaiverAuthorityRequired means Waive was called with no documented
	// authority reference. LEG-07 §9.1: "Waiver is distinct from
	// satisfaction and requires documented authority."
	ErrWaiverAuthorityRequired = errors.New("waiver_authority_reference is required")
)

// ObligationType is deliberately narrow: this service owns contract-derived
// obligations only (docs/original_doc/zoiko_suite_doc4.txt §12.3 "Obligation
// Tracking Service... Extracts, stores, and manages contract-linked
// obligations"). Statutory/regulatory/internal-policy obligations are
// obligations-svc's domain (§6.6/§8.5) — the platform-wide umbrella the spec
// says to route to "by source class" (doc4.txt:531). REGULATORY, STATUTORY,
// and INTERNAL_POLICY previously existed here too, duplicating that scope;
// removed rather than left unused, since an unused-but-present enum value
// invites exactly the duplication this is fixing.
type ObligationType string

const (
	ObligationTypeContractual ObligationType = "CONTRACTUAL"
)

// SourceType enumerates where a contract-linked obligation was extracted
// from — always something contract-lifecycle-svc owns. A source type outside
// this set (e.g. a bare regulation, with no contract involved) belongs in
// obligations-svc instead.
type SourceType string

const (
	SourceTypeContract SourceType = "CONTRACT"
	SourceTypeClause   SourceType = "CLAUSE"
)

func (t SourceType) Valid() bool {
	switch t {
	case SourceTypeContract, SourceTypeClause:
		return true
	default:
		return false
	}
}

// ObligationStatus follows LEG-07's named lifecycle (docs/architecture/
// original_doc §9): Planned -> Active -> Due -> In Progress ->
// Satisfied/Waived/Breached/Disputed/Superseded. CANDIDATE precedes PLANNED
// only for AI-extracted obligations (§9.1's validation invariant) — see
// migration 000002's doc comment.
type ObligationStatus string

const (
	ObligationStatusCandidate  ObligationStatus = "CANDIDATE"
	ObligationStatusPlanned    ObligationStatus = "PLANNED"
	ObligationStatusActive     ObligationStatus = "ACTIVE"
	ObligationStatusDue        ObligationStatus = "DUE"
	ObligationStatusInProgress ObligationStatus = "IN_PROGRESS"
	ObligationStatusSatisfied  ObligationStatus = "SATISFIED"
	ObligationStatusWaived     ObligationStatus = "WAIVED"
	ObligationStatusBreached   ObligationStatus = "BREACHED"
	ObligationStatusDisputed   ObligationStatus = "DISPUTED"
	ObligationStatusSuperseded ObligationStatus = "SUPERSEDED"
)

// IsFinal reports whether no further lifecycle command may act on the
// obligation.
func (s ObligationStatus) IsFinal() bool {
	switch s {
	case ObligationStatusSatisfied, ObligationStatusWaived, ObligationStatusBreached,
		ObligationStatusDisputed, ObligationStatusSuperseded:
		return true
	}
	return false
}

type RiskLevel string

const (
	RiskLevelLow      RiskLevel = "LOW"
	RiskLevelMedium   RiskLevel = "MEDIUM"
	RiskLevelHigh     RiskLevel = "HIGH"
	RiskLevelCritical RiskLevel = "CRITICAL"
)

type Obligation struct {
	ObligationID   string           `json:"obligation_id"`
	TenantID       string           `json:"tenant_id"`
	LegalEntityID  string           `json:"legal_entity_id"`
	SourceType     string           `json:"source_type"` // CONTRACT, CLAUSE
	SourceID       string           `json:"source_id"`
	Title          string           `json:"title"`
	Description    string           `json:"description,omitempty"`
	ObligationType ObligationType   `json:"obligation_type"`
	RiskLevel      RiskLevel        `json:"risk_level"`
	Status         ObligationStatus `json:"status"`
	DueDate        string           `json:"due_date"`
	AssignedTo     string           `json:"assigned_to,omitempty"`

	// ExtractedByAI marks a candidate obligation proposed by AI/document
	// extraction rather than entered manually. LEG-07 §9.1: "AI extraction
	// cannot activate obligation without validation" — such an obligation
	// starts at CANDIDATE and may only reach PLANNED via
	// ValidateExtractedObligation.
	ExtractedByAI bool       `json:"extracted_by_ai"`
	ValidatedAt   *time.Time `json:"validated_at,omitempty"`
	ValidatedBy   *string    `json:"validated_by,omitempty"`

	// Due-date provenance (§9.1), certified by Schedule.
	SourceClauseID     *string    `json:"source_clause_id,omitempty"`
	TriggerDescription string     `json:"trigger_description,omitempty"`
	CalculationMethod  string     `json:"calculation_method,omitempty"`
	ScheduledAt        *time.Time `json:"scheduled_at,omitempty"`
	ScheduledBy        *string    `json:"scheduled_by,omitempty"`

	DueMarkedAt  *time.Time `json:"due_marked_at,omitempty"`
	DueMarkedBy  *string    `json:"due_marked_by,omitempty"`
	InProgressAt *time.Time `json:"in_progress_at,omitempty"`
	InProgressBy *string    `json:"in_progress_by,omitempty"`

	SatisfiedAt *time.Time `json:"satisfied_at,omitempty"`
	SatisfiedBy *string    `json:"satisfied_by,omitempty"`

	WaivedAt                 *time.Time `json:"waived_at,omitempty"`
	WaivedBy                 *string    `json:"waived_by,omitempty"`
	WaiverAuthorityReference *string    `json:"waiver_authority_reference,omitempty"`

	// BreachedAt/By/Note record the fact of breach only. LEG-07 §9.1:
	// "Breach status does not automatically create financial accrual,
	// payment or legal remedy; target domains act separately" — enforced
	// simply by RecordBreach never calling out to any of them.
	BreachedAt *time.Time `json:"breached_at,omitempty"`
	BreachedBy *string    `json:"breached_by,omitempty"`
	BreachNote *string    `json:"breach_note,omitempty"`

	DisputedAt    *time.Time `json:"disputed_at,omitempty"`
	DisputedBy    *string    `json:"disputed_by,omitempty"`
	DisputeReason *string    `json:"dispute_reason,omitempty"`

	SupersededBy *string `json:"superseded_by,omitempty"`

	EffectiveFrom string    `json:"effective_from"`
	EffectiveTo   *string   `json:"effective_to,omitempty"`
	CreatedBy     string    `json:"created_by"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type CreateObligationRequest struct {
	LegalEntityID  string         `json:"legal_entity_id"`
	SourceType     string         `json:"source_type"`
	SourceID       string         `json:"source_id"`
	Title          string         `json:"title"`
	Description    string         `json:"description,omitempty"`
	ObligationType ObligationType `json:"obligation_type"`
	RiskLevel      RiskLevel      `json:"risk_level"`
	DueDate        string         `json:"due_date"`
	AssignedTo     string         `json:"assigned_to,omitempty"`
	ExtractedByAI  bool           `json:"extracted_by_ai,omitempty"`
	EffectiveFrom  string         `json:"effective_from"`
	EffectiveTo    *string        `json:"effective_to,omitempty"`
	CreatedBy      string         `json:"created_by"`
}

// UpdateObligationRequest edits non-protected fields while the obligation
// is still CANDIDATE or PLANNED. Status is deliberately not settable here —
// every transition past PLANNED goes through its own named command so each
// has its own precondition and attribution, not a free-form field any
// caller could set to anything.
type UpdateObligationRequest struct {
	Title          string         `json:"title,omitempty"`
	Description    string         `json:"description,omitempty"`
	ObligationType ObligationType `json:"obligation_type,omitempty"`
	RiskLevel      RiskLevel      `json:"risk_level,omitempty"`
	DueDate        string         `json:"due_date,omitempty"`
	AssignedTo     string         `json:"assigned_to,omitempty"`
	EffectiveTo    *string        `json:"effective_to,omitempty"`
	UpdatedBy      string         `json:"updated_by"`
}

type ValidateExtractedObligationRequest struct {
	ValidatedBy string `json:"validated_by"`
}

type ScheduleObligationRequest struct {
	SourceClauseID     *string `json:"source_clause_id,omitempty"`
	TriggerDescription string  `json:"trigger_description"`
	CalculationMethod  string  `json:"calculation_method"`
	ScheduledBy        string  `json:"scheduled_by"`
}

type MarkDueRequest struct {
	MarkedBy string `json:"marked_by"`
}

type MarkInProgressRequest struct {
	MarkedBy string `json:"marked_by"`
}

type CompleteObligationRequest struct {
	SatisfiedBy string `json:"satisfied_by"`
}

type WaiveObligationRequest struct {
	WaivedBy                 string `json:"waived_by"`
	WaiverAuthorityReference string `json:"waiver_authority_reference"`
}

type RecordBreachRequest struct {
	BreachedBy string `json:"breached_by"`
	BreachNote string `json:"breach_note,omitempty"`
}

type DisputeObligationRequest struct {
	DisputedBy    string `json:"disputed_by"`
	DisputeReason string `json:"dispute_reason"`
}

type SupersedeObligationRequest struct {
	SupersededBy string `json:"superseded_by"`
}
