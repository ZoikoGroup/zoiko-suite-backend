package domain

import (
	"errors"
	"time"
)

var (
	ErrClauseNotFound    = errors.New("clause not found")
	ErrTemplateNotFound  = errors.New("template not found")
	ErrDeviationNotFound = errors.New("deviation rule not found")

	// ErrTenantMissing means the request carried no X-Tenant-Id. It is an
	// unauthenticated request, not an empty tenant named "default".
	ErrTenantMissing = errors.New("tenant scope missing")

	// ErrWrongStatus means the entity is not in the status a command
	// requires — e.g. ApproveClause called on a clause not in LEGAL_REVIEW.
	ErrWrongStatus = errors.New("entity is not in the required status for this action")

	// ErrSelfApprovalNotAllowed enforces LEG-06's named maker-checker
	// requirement (docs/architecture/original_doc §8: "maker-checker for
	// approved standard language; AI cannot approve legal text"): the
	// principal who submitted a clause for review, or proposed a deviation,
	// may not be the one who approves it.
	ErrSelfApprovalNotAllowed = errors.New("principal may not approve their own submission")
)

type ClauseCategory string

const (
	ClauseCategoryConfidentiality ClauseCategory = "CONFIDENTIALITY"
	ClauseCategoryIndemnification ClauseCategory = "INDEMNIFICATION"
	ClauseCategoryTermination     ClauseCategory = "TERMINATION"
	ClauseCategoryLiability       ClauseCategory = "LIABILITY"
	ClauseCategoryGoverningLaw    ClauseCategory = "GOVERNING_LAW"
	ClauseCategoryPaymentTerms    ClauseCategory = "PAYMENT_TERMS"
	ClauseCategoryOther           ClauseCategory = "OTHER"
)

// Status is shared by Clause and ContractTemplate. For a Clause it follows
// LEG-06's named 5-state lifecycle (§8: Draft -> Legal Review -> Approved ->
// Active -> Retired/Superseded); a ContractTemplate uses only
// DRAFT/ACTIVE/ARCHIVED, since the spec names no intermediate review state
// for templates (TemplateApproved is still a named canonical event, so
// templates do get an explicit approval gate — just not a multi-step one).
type Status string

const (
	StatusDraft       Status = "DRAFT"
	StatusLegalReview Status = "LEGAL_REVIEW"
	StatusApproved    Status = "APPROVED"
	StatusActive      Status = "ACTIVE"
	StatusRetired     Status = "RETIRED"
	StatusSuperseded  Status = "SUPERSEDED"
	StatusArchived    Status = "ARCHIVED"
)

// IsFinal reports whether no further lifecycle command may act on the
// clause.
func (s Status) IsFinal() bool {
	return s == StatusRetired || s == StatusSuperseded || s == StatusArchived
}

type Clause struct {
	ClauseID       string         `json:"clause_id"`
	TenantID       string         `json:"tenant_id"`
	LegalEntityID  string         `json:"legal_entity_id"`
	Title          string         `json:"title"`
	Category       ClauseCategory `json:"category"`
	Body           string         `json:"body"`
	Status         Status         `json:"status"`
	Version        int            `json:"version"`
	JurisdictionID string         `json:"jurisdiction_id"`
	EffectiveFrom  string         `json:"effective_from"`
	EffectiveTo    *string        `json:"effective_to,omitempty"`
	// AuthoredByAI records that this clause's draft language originated
	// from an AI/LLM assist. LEG-06 §8.1: "LLM-generated draft language can
	// never be promoted to approved clause status without human legal
	// review" — this flag is evidence that the review which occurred was
	// aware of that fact, not a gate the code enforces differently; every
	// clause, AI-authored or not, must pass through the same
	// SubmitForLegalReview -> ApproveClause path, since no path exists that
	// skips it.
	AuthoredByAI bool       `json:"authored_by_ai"`
	SubmittedAt  *time.Time `json:"submitted_at,omitempty"`
	SubmittedBy  *string    `json:"submitted_by,omitempty"`
	ApprovedAt   *time.Time `json:"approved_at,omitempty"`
	ApprovedBy   *string    `json:"approved_by,omitempty"`
	ActivatedAt  *time.Time `json:"activated_at,omitempty"`
	ActivatedBy  *string    `json:"activated_by,omitempty"`
	RetiredAt    *time.Time `json:"retired_at,omitempty"`
	RetiredBy    *string    `json:"retired_by,omitempty"`
	SupersededBy *string    `json:"superseded_by,omitempty"`
	CreatedBy    string     `json:"created_by"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// ClauseVersion is an immutable snapshot of a clause at a point in time —
// LEG-06's named CreateClauseVersion evidence trail. Historical contracts
// retain the clause version they actually executed (§8.1); that invariant
// only means something if this history exists.
type ClauseVersion struct {
	VersionID      string    `json:"version_id"`
	ClauseID       string    `json:"clause_id"`
	TenantID       string    `json:"tenant_id"`
	VersionNumber  int       `json:"version_number"`
	Status         Status    `json:"status"`
	Title          string    `json:"title"`
	Body           string    `json:"body"`
	JurisdictionID string    `json:"jurisdiction_id"`
	EffectiveFrom  string    `json:"effective_from"`
	EffectiveTo    *string   `json:"effective_to,omitempty"`
	ChangeSummary  string    `json:"change_summary"`
	CreatedBy      string    `json:"created_by"`
	CreatedAt      time.Time `json:"created_at"`
}

// RiskClassification is required on every deviation rule (LEG-06 §8.1:
// "clause deviations require risk/playbook classification and accountable
// approval").
type RiskClassification string

const (
	RiskLow      RiskClassification = "LOW"
	RiskMedium   RiskClassification = "MEDIUM"
	RiskHigh     RiskClassification = "HIGH"
	RiskCritical RiskClassification = "CRITICAL"
)

func (r RiskClassification) IsValid() bool {
	switch r {
	case RiskLow, RiskMedium, RiskHigh, RiskCritical:
		return true
	}
	return false
}

type DeviationStatus string

const (
	DeviationStatusProposed DeviationStatus = "PROPOSED"
	DeviationStatusApproved DeviationStatus = "APPROVED"
	DeviationStatusRejected DeviationStatus = "REJECTED"
)

// DeviationRule records a proposed departure from a clause's approved
// standard language, and the accountable approval LEG-06 §8.1 requires
// before it may be used. "Playbook" (the spec's GetPlaybook read surface)
// is this service's set of APPROVED deviation rules for a jurisdiction —
// not a separate heavyweight entity in this pass.
type DeviationRule struct {
	DeviationID        string             `json:"deviation_id"`
	TenantID           string             `json:"tenant_id"`
	LegalEntityID      string             `json:"legal_entity_id"`
	ClauseID           *string            `json:"clause_id,omitempty"`
	JurisdictionID     string             `json:"jurisdiction_id"`
	RiskClassification RiskClassification `json:"risk_classification"`
	Description        string             `json:"description"`
	Status             DeviationStatus    `json:"status"`
	ProposedBy         string             `json:"proposed_by"`
	ProposedAt         time.Time          `json:"proposed_at"`
	ApprovedBy         *string            `json:"approved_by,omitempty"`
	ApprovedAt         *time.Time         `json:"approved_at,omitempty"`
}

type ContractTemplate struct {
	TemplateID     string     `json:"template_id"`
	TenantID       string     `json:"tenant_id"`
	LegalEntityID  string     `json:"legal_entity_id"`
	Title          string     `json:"title"`
	ContractType   string     `json:"contract_type"`
	Description    string     `json:"description,omitempty"`
	ClauseIDs      []string   `json:"clause_ids"`
	Status         Status     `json:"status"`
	Version        int        `json:"version"`
	JurisdictionID string     `json:"jurisdiction_id"`
	EffectiveFrom  string     `json:"effective_from"`
	EffectiveTo    *string    `json:"effective_to,omitempty"`
	ApprovedAt     *time.Time `json:"approved_at,omitempty"`
	ApprovedBy     *string    `json:"approved_by,omitempty"`
	CreatedBy      string     `json:"created_by"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

type CreateClauseRequest struct {
	LegalEntityID  string         `json:"legal_entity_id"`
	Title          string         `json:"title"`
	Category       ClauseCategory `json:"category"`
	Body           string         `json:"body"`
	JurisdictionID string         `json:"jurisdiction_id"`
	EffectiveFrom  string         `json:"effective_from"`
	EffectiveTo    *string        `json:"effective_to,omitempty"`
	AuthoredByAI   bool           `json:"authored_by_ai,omitempty"`
	CreatedBy      string         `json:"created_by"`
}

// UpdateClauseRequest is LEG-06's named CreateClauseVersion command: it may
// only act while the clause is DRAFT or LEGAL_REVIEW — an approved clause
// version is effective-dated evidence, not a field to edit in place.
type UpdateClauseRequest struct {
	Title          string         `json:"title,omitempty"`
	Category       ClauseCategory `json:"category,omitempty"`
	Body           string         `json:"body,omitempty"`
	JurisdictionID string         `json:"jurisdiction_id,omitempty"`
	EffectiveTo    *string        `json:"effective_to,omitempty"`
	ChangeSummary  string         `json:"change_summary"`
	UpdatedBy      string         `json:"updated_by"`
}

type SubmitForLegalReviewRequest struct {
	SubmittedBy string `json:"submitted_by"`
}

type ApproveClauseRequest struct {
	ApprovedBy string `json:"approved_by"`
}

type ActivateClauseRequest struct {
	ActivatedBy string `json:"activated_by"`
}

type RetireClauseRequest struct {
	RetiredBy string `json:"retired_by"`
}

type SupersedeClauseRequest struct {
	SupersededBy string `json:"superseded_by"`
}

type ApproveTemplateRequest struct {
	ApprovedBy string `json:"approved_by"`
}

type CreateDeviationRuleRequest struct {
	LegalEntityID      string             `json:"legal_entity_id"`
	ClauseID           *string            `json:"clause_id,omitempty"`
	JurisdictionID     string             `json:"jurisdiction_id"`
	RiskClassification RiskClassification `json:"risk_classification"`
	Description        string             `json:"description"`
	ProposedBy         string             `json:"proposed_by"`
}

type ApproveDeviationRuleRequest struct {
	ApprovedBy string `json:"approved_by"`
}

type CreateTemplateRequest struct {
	LegalEntityID  string   `json:"legal_entity_id"`
	Title          string   `json:"title"`
	ContractType   string   `json:"contract_type"`
	Description    string   `json:"description,omitempty"`
	ClauseIDs      []string `json:"clause_ids"`
	JurisdictionID string   `json:"jurisdiction_id"`
	EffectiveFrom  string   `json:"effective_from"`
	EffectiveTo    *string  `json:"effective_to,omitempty"`
	CreatedBy      string   `json:"created_by"`
}

type UpdateTemplateRequest struct {
	Title          string   `json:"title,omitempty"`
	ContractType   string   `json:"contract_type,omitempty"`
	Description    string   `json:"description,omitempty"`
	ClauseIDs      []string `json:"clause_ids,omitempty"`
	JurisdictionID string   `json:"jurisdiction_id,omitempty"`
	EffectiveTo    *string  `json:"effective_to,omitempty"`
	UpdatedBy      string   `json:"updated_by"`
}
