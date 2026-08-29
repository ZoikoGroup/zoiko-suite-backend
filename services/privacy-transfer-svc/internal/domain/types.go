// Package domain defines the canonical types for privacy-transfer-svc —
// PRV-05, "Processor, Subprocessor & Transfer Privacy Control", from
// ZS-SVC-W-001 §16/§17. The fifth and final service in this domain build.
//
// v1 scope, documented rather than hidden (same doctrine as PRV-01–04):
//
//   - §16.1's TransferMechanism is defined as "a governed mechanism
//     ID/version from PDC/legal catalogue" — no PDC (jurisdiction-specific
//     legal-rule package) catalogue exists anywhere in this codebase,
//     same gap already documented on PRV-01/PRV-03. This service does
//     NOT invent a legal-validity catalogue; instead TransferMechanism is
//     its own lightweight, directly-registered record — a real legal/
//     compliance reviewer records the mechanism (SCC/BCR/adequacy/
//     derogation), its validity window, and its evidence reference. The
//     trust model is the same one this platform already uses for
//     SoD rules and notice content: a human records a real fact, the
//     system enforces the machine-checkable invariants on top (here,
//     validity-window expiry) — it does not manufacture the legal
//     judgment itself.
//   - §17's DPIA/TIA requirement gate ("PDC rule says assessment
//     required?") cannot be evaluated by this service for the same
//     reason — there is no PDC rule to consult. Whether an assessment is
//     required is therefore an opt-in, CALLER-DECLARED input to the
//     transfer-decision evaluation (AssessmentRequired), mirroring PRV-03's
//     ConsentCheckRequest/LegalHoldCheckRequest pattern: this service
//     never infers the requirement, it only evaluates real evidence once
//     told to look for it.
//   - §16.1's TransferAuthorization is documented as a four-value result:
//     AUTHORIZED, CONDITIONAL, BLOCKED, REVIEW_REQUIRED. CONDITIONAL
//     requires machine-enforceable transfer conditions (route/volume/
//     recipient limitations) that this service has no rule engine to
//     generate — same reasoning as PRV-03's RESTRICT. CONDITIONAL is
//     defined in the wire contract for forward compatibility but never
//     produced by this version. AUTHORIZED, BLOCKED and REVIEW_REQUIRED
//     ARE all reachable — unlike PRV-03's REVIEW_REQUIRED, this one
//     requires no invented business rule: §17.2 states plainly that a
//     missing/expired/conflicted DPIA/TIA or mechanism approval returns
//     "BLOCKED or REVIEW_REQUIRED," which this service encodes literally
//     (REJECT outcome or expired mechanism -> BLOCKED; missing assessment,
//     REMEDIATE outcome, or expired assessment -> REVIEW_REQUIRED, per
//     §17.1's own explicit reassessment triggers).
//   - Every ProcessorRelationship's purpose/activity references are
//     validated against a REAL call to privacy-purpose-registry-svc
//     (PRV-01) when supplied — not trusted as opaque strings, same
//     discipline as PRV-02/PRV-03's dependency on PRV-01.
package domain

import "time"

// ── Processor / Subprocessor inventory ──────────────────────────────────────

type RelationshipStatus string

const (
	RelationshipActive   RelationshipStatus = "ACTIVE"
	RelationshipInactive RelationshipStatus = "INACTIVE"
)

// ProcessorRelationship — §16.1's own field list.
type ProcessorRelationship struct {
	RelationshipID         string             `json:"relationship_id"`
	TenantID               *string            `json:"tenant_id,omitempty"`
	ControllerRef          string             `json:"controller_ref"`
	ProcessorRef           string             `json:"processor_ref"`
	Service                string             `json:"service"`
	ProcessingInstructions string             `json:"processing_instructions,omitempty"`
	PurposeActivityRefs    []string           `json:"purpose_activity_refs"`
	DataCategories         []string           `json:"data_categories"`
	SubjectClasses         []string           `json:"subject_classes"`
	ContractEvidenceRef    string             `json:"contract_evidence_ref,omitempty"`
	Jurisdictions          []string           `json:"jurisdictions"`
	Status                 RelationshipStatus `json:"status"`
	CreatedAt              time.Time          `json:"created_at"`
	CreatedByPrincipalID   string             `json:"created_by_principal_id"`
}

// Subprocessor — §16.1's own field list, always attached to a parent
// ProcessorRelationship.
type Subprocessor struct {
	SubprocessorID            string    `json:"subprocessor_id"`
	TenantID                  *string   `json:"tenant_id,omitempty"`
	RelationshipID            string    `json:"relationship_id"`
	ProviderIdentity          string    `json:"provider_identity"`
	Service                   string    `json:"service"`
	Purpose                   string    `json:"purpose,omitempty"`
	DataScope                 string    `json:"data_scope,omitempty"`
	ProcessingLocations       []string  `json:"processing_locations"`
	OnwardSubprocessors       []string  `json:"onward_subprocessors"`
	NotificationApprovalModel string    `json:"notification_approval_model,omitempty"`
	ContractEvidenceRef       string    `json:"contract_evidence_ref,omitempty"`
	CreatedAt                 time.Time `json:"created_at"`
	CreatedByPrincipalID      string    `json:"created_by_principal_id"`
}

// ── Transfer mechanism ───────────────────────────────────────────────────────

// MechanismType is data only — new values (SCC/BCR/ADEQUACY/DEROGATION/
// OTHER) are added via data, same doctrine as every enum-shaped string
// column in this platform.
type MechanismType string

// TransferMechanism — see the package doc comment on why this is a
// directly-registered record, not a resolved PDC catalogue entry.
type TransferMechanism struct {
	MechanismID          string        `json:"mechanism_id"`
	TenantID             *string       `json:"tenant_id,omitempty"`
	MechanismType        MechanismType `json:"mechanism_type"`
	EvidenceRef          string        `json:"evidence_ref,omitempty"`
	Conditions           string        `json:"conditions,omitempty"`
	ValidFrom            time.Time     `json:"valid_from"`
	ValidUntil           *time.Time    `json:"valid_until,omitempty"` // nil = no expiry
	CreatedAt            time.Time     `json:"created_at"`
	CreatedByPrincipalID string        `json:"created_by_principal_id"`
}

func (m *TransferMechanism) ValidAsOf(t time.Time) bool {
	if t.Before(m.ValidFrom) {
		return false
	}
	if m.ValidUntil != nil && t.After(*m.ValidUntil) {
		return false
	}
	return true
}

// ── Transfer assessment (DPIA/TIA) ──────────────────────────────────────────

type AssessmentOutcome string

const (
	AssessmentApprove   AssessmentOutcome = "APPROVE"
	AssessmentRemediate AssessmentOutcome = "REMEDIATE"
	AssessmentReject    AssessmentOutcome = "REJECT"
)

func (o AssessmentOutcome) Valid() bool {
	switch o {
	case AssessmentApprove, AssessmentRemediate, AssessmentReject:
		return true
	}
	return false
}

// TransferAssessment is append-only evidence of one DPIA/TIA review — a
// re-assessment is a NEW record, never an edit of a prior one (same
// doctrine as every evidence table in this domain). "Current" assessment
// for a relationship is the latest by created_at, same derived-read
// pattern as PRV-02's ResolveConsentStatus.
type TransferAssessment struct {
	AssessmentID        string            `json:"assessment_id"`
	TenantID            *string           `json:"tenant_id,omitempty"`
	RelationshipID      string            `json:"relationship_id"`
	Outcome             AssessmentOutcome `json:"outcome"`
	ReviewerPrincipalID string            `json:"reviewer_principal_id"`
	ResidualRisk        string            `json:"residual_risk,omitempty"`
	EvidenceRef         string            `json:"evidence_ref,omitempty"`
	ReviewTriggerAt     *time.Time        `json:"review_trigger_at,omitempty"` // §17.1: expiry/review date
	CreatedAt           time.Time         `json:"created_at"`
}

func (a *TransferAssessment) ExpiredAsOf(t time.Time) bool {
	return a.ReviewTriggerAt != nil && t.After(*a.ReviewTriggerAt)
}

// ── Transfer authorization decision ─────────────────────────────────────────

// AuthorizationResult — see the package doc comment: CONDITIONAL is
// defined but never produced in this version.
type AuthorizationResult string

const (
	ResultAuthorized     AuthorizationResult = "AUTHORIZED"
	ResultConditional    AuthorizationResult = "CONDITIONAL" // reserved, unreachable in v1
	ResultBlocked        AuthorizationResult = "BLOCKED"
	ResultReviewRequired AuthorizationResult = "REVIEW_REQUIRED"
)

// Reason codes — data only, named so a caller acting on BLOCKED/
// REVIEW_REQUIRED can tell which real check failed.
const (
	ReasonRelationshipNotActive = "PROCESSOR_RELATIONSHIP_NOT_ACTIVE"
	ReasonMechanismNotFound     = "TRANSFER_MECHANISM_NOT_FOUND"
	ReasonMechanismExpired      = "TRANSFER_MECHANISM_INVALID_OR_EXPIRED"
	ReasonAssessmentMissing     = "ASSESSMENT_REQUIRED_NOT_FOUND"
	ReasonAssessmentRejected    = "ASSESSMENT_REJECTED"
	ReasonAssessmentRemediate   = "ASSESSMENT_REQUIRES_REMEDIATION"
	ReasonAssessmentExpired     = "ASSESSMENT_EXPIRED"
	ReasonDependencyUnavailable = "DEPENDENCY_UNAVAILABLE"
)

// TransferDecision is the append-only evidence record for one evaluation
// — same "decision durability" doctrine as PRV-03's PrivacyDecision.
type TransferDecision struct {
	DecisionID              string              `json:"decision_id"`
	TenantID                *string             `json:"tenant_id,omitempty"`
	RelationshipID          string              `json:"relationship_id"`
	TransferMechanismID     string              `json:"transfer_mechanism_id"`
	DestinationJurisdiction string              `json:"destination_jurisdiction,omitempty"`
	AssessmentID            *string             `json:"assessment_id,omitempty"`
	Result                  AuthorizationResult `json:"result"`
	ReasonCodes             []string            `json:"reason_codes"`
	ActorPrincipalID        string              `json:"actor_principal_id"`
	CorrelationID           string              `json:"correlation_id,omitempty"`
	DecidedAt               time.Time           `json:"decided_at"`
}

// ── Request DTOs ─────────────────────────────────────────────────────────────

type UpdateRelationshipStatusRequest struct {
	Status RelationshipStatus `json:"status"`
}

func (s RelationshipStatus) Valid() bool {
	return s == RelationshipActive || s == RelationshipInactive
}

type CreateProcessorRelationshipRequest struct {
	TenantID               string   `json:"tenant_id,omitempty"`
	ControllerRef          string   `json:"controller_ref"`
	ProcessorRef           string   `json:"processor_ref"`
	Service                string   `json:"service"`
	ProcessingInstructions string   `json:"processing_instructions,omitempty"`
	PurposeActivityRefs    []string `json:"purpose_activity_refs,omitempty"`
	DataCategories         []string `json:"data_categories,omitempty"`
	SubjectClasses         []string `json:"subject_classes,omitempty"`
	ContractEvidenceRef    string   `json:"contract_evidence_ref,omitempty"`
	Jurisdictions          []string `json:"jurisdictions,omitempty"`
}

type AttachSubprocessorRequest struct {
	ProviderIdentity          string   `json:"provider_identity"`
	Service                   string   `json:"service"`
	Purpose                   string   `json:"purpose,omitempty"`
	DataScope                 string   `json:"data_scope,omitempty"`
	ProcessingLocations       []string `json:"processing_locations,omitempty"`
	OnwardSubprocessors       []string `json:"onward_subprocessors,omitempty"`
	NotificationApprovalModel string   `json:"notification_approval_model,omitempty"`
	ContractEvidenceRef       string   `json:"contract_evidence_ref,omitempty"`
}

type CreateTransferMechanismRequest struct {
	TenantID      string        `json:"tenant_id,omitempty"`
	MechanismType MechanismType `json:"mechanism_type"`
	EvidenceRef   string        `json:"evidence_ref,omitempty"`
	Conditions    string        `json:"conditions,omitempty"`
	ValidFrom     *time.Time    `json:"valid_from,omitempty"`
	ValidUntil    *time.Time    `json:"valid_until,omitempty"`
}

type RecordTransferAssessmentRequest struct {
	RelationshipID  string            `json:"relationship_id"`
	Outcome         AssessmentOutcome `json:"outcome"`
	ResidualRisk    string            `json:"residual_risk,omitempty"`
	EvidenceRef     string            `json:"evidence_ref,omitempty"`
	ReviewTriggerAt *time.Time        `json:"review_trigger_at,omitempty"`
}

type EvaluateTransferRequest struct {
	TenantID                string `json:"tenant_id,omitempty"`
	RelationshipID          string `json:"relationship_id"`
	TransferMechanismID     string `json:"transfer_mechanism_id"`
	DestinationJurisdiction string `json:"destination_jurisdiction,omitempty"`
	AssessmentRequired      bool   `json:"assessment_required"`
}

// ── sentinel errors ──────────────────────────────────────────────────────────

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrRelationshipNotFound = errorString("processor relationship not found")
	ErrMechanismNotFound    = errorString("transfer mechanism not found")
	ErrDecisionNotFound     = errorString("transfer decision not found")
	ErrPurposeNotActive     = errorString("referenced processing activity is not active")
	ErrStoreUnavailable     = errorString("privacy-transfer store unavailable")
)
