// Package domain contains the authoritative domain types for workflow-svc.
//
// workflow_type and stage_status are plain strings — no Go enums, same
// doctrine as every other service in this platform. workflow_status IS a
// real (small) state machine: PENDING -> APPROVED | REJECTED | ESCALATED |
// CANCELLED | INVALIDATED, enforced in application code.
package domain

import "time"

// Canonical workflow statuses per ZS-STATE-001 §6.
const (
	WorkflowStatusPending     = "PENDING"
	WorkflowStatusApproved    = "APPROVED"
	WorkflowStatusRejected    = "REJECTED"
	WorkflowStatusEscalated   = "ESCALATED"
	WorkflowStatusCancelled   = "CANCELLED"
	WorkflowStatusInvalidated = "INVALIDATED"
)

// WorkflowInstance is one approval request moving through an ordered chain
// of approval stages per ZS-STATE-001 §6. Critical constraint (mirrors every
// other service): entity-bound (LegalEntityID), never hard-deleted —
// cancellation and invalidation are status transitions, not row removals.
type WorkflowInstance struct {
	WorkflowInstanceID string `json:"workflow_instance_id"`

	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`

	// WorkflowType is data only (e.g. "PURCHASE_APPROVAL").
	WorkflowType string `json:"workflow_type"`

	// Approval subject binding per ZS-STATE-001 §6.1
	SubjectType        *string `json:"subject_type,omitempty"`
	SubjectID          *string `json:"subject_id,omitempty"`
	SubjectVersion     *int    `json:"subject_version,omitempty"`
	SubjectFingerprint *string `json:"subject_fingerprint,omitempty"`

	// WorkflowStatus: PENDING | APPROVED | REJECTED | ESCALATED | CANCELLED | INVALIDATED.
	// APPROVED/REJECTED/CANCELLED/INVALIDATED are terminal.
	WorkflowStatus string `json:"workflow_status"`

	// CurrentStage is the 1-based stage_order currently awaiting action.
	// 0 once the workflow reaches a terminal state.
	CurrentStage int `json:"current_stage"`

	InitiatedBy   string     `json:"initiated_by"`
	CorrelationID string     `json:"correlation_id"`
	StartedAt     time.Time  `json:"started_at"`
	CompletedAt   *time.Time `json:"completed_at"`

	// Invalidation metadata per ZS-STATE-001 §6.1 / §7
	InvalidatedAt            *time.Time `json:"invalidated_at,omitempty"`
	InvalidationReasonCode   *string    `json:"invalidation_reason_code,omitempty"`
	InvalidationNarrative    *string    `json:"invalidation_narrative,omitempty"`
	InvalidationEvidenceRefs []string   `json:"invalidation_evidence_refs,omitempty"`
}

// WorkflowStage is one approval gate in a workflow's ordered chain,
// supplied by the caller at creation time — this service does not
// resolve "who should approve X" from any rule engine; no such rules
// are specified anywhere in the architecture docs. See progress.md.
//
// StageType is SINGLE (one named approver, the original shape) or
// QUORUM (an eligible pool with an N-of-M threshold, ZS-SVC-R-001
// §5.1/§8.3 — see quorum.go). Exactly one of
// ApproverPrincipalID/RequiredApprovals is set, matching which shape
// the stage is — enforced at the database by migration 000012's
// workflow_stages_shape_matches_type CHECK.
type WorkflowStage struct {
	WorkflowStageID    string `json:"workflow_stage_id"`
	WorkflowInstanceID string `json:"workflow_instance_id"`

	StageOrder int `json:"stage_order"`

	// StageType: SINGLE | QUORUM.
	StageType string `json:"stage_type"`

	// ApproverPrincipalID is set (non-empty) only for a SINGLE stage —
	// kept a plain string, not a pointer, so every existing SINGLE-stage
	// code path (which predates QUORUM entirely) is untouched.
	ApproverPrincipalID string `json:"approver_principal_id,omitempty"`
	// RequiredApprovals is set only for a QUORUM stage — the N in N-of-M.
	RequiredApprovals *int `json:"required_approvals,omitempty"`

	// StageStatus: PENDING | APPROVED | REJECTED | SKIPPED.
	StageStatus string `json:"stage_status"`

	ActedAt   *time.Time `json:"acted_at"`
	Rationale *string    `json:"rationale"`
}

// WorkflowTransition is the append-only audit trail — one row per state
// change, never updated or deleted. Every action taken on a workflow,
// approved or rejected, is evidence.
type WorkflowTransition struct {
	WorkflowTransitionID string `json:"workflow_transition_id"`
	WorkflowInstanceID   string `json:"workflow_instance_id"`

	FromState string  `json:"from_state"`
	ToState   string  `json:"to_state"`
	ActedBy   string  `json:"acted_by"`
	Rationale *string `json:"rationale"`

	// CorrelationID is carried forward from the owning WorkflowInstance's
	// own correlation_id. CausationID is nil unless the caller submitting
	// this specific action supplied one.
	CorrelationID *string `json:"correlation_id,omitempty"`
	CausationID   *string `json:"causation_id,omitempty"`

	ActedAt time.Time `json:"acted_at"`
}

// ── params ───────────────────────────────────────────────────────────────────

// CreateWorkflowStageInput is one entry in the caller-supplied approval
// chain. A SINGLE stage (the default, when StageType is empty) sets
// ApproverPrincipalID only. A QUORUM stage sets StageType, RequiredApprovals
// and QuorumApprovers instead — see quorum.go.
type CreateWorkflowStageInput struct {
	StageType           string   `json:"stage_type,omitempty"`
	ApproverPrincipalID string   `json:"approver_principal_id,omitempty"`
	RequiredApprovals   int      `json:"required_approvals,omitempty"`
	QuorumApprovers     []string `json:"quorum_approvers,omitempty"`
}

type CreateWorkflowParams struct {
	WorkflowInstanceID string
	TenantID           string
	LegalEntityID      string
	WorkflowType       string
	SubjectType        *string
	SubjectID          *string
	SubjectVersion     *int
	SubjectFingerprint *string
	InitiatedBy        string
	CorrelationID      string
	Stages             []CreateWorkflowStageInput
}

// SubmitActionParams holds input for approving or rejecting the current stage.
type SubmitActionParams struct {
	WorkflowInstanceID string
	ActorPrincipalID   string
	// Action: APPROVE | REJECT.
	Action    string
	Rationale *string
	// CausationID is optional: the event/decision that caused this specific
	// action, when the caller knows it.
	CausationID *string
}

// InvalidateWorkflowParams holds input for invalidating a workflow on material change.
type InvalidateWorkflowParams struct {
	WorkflowInstanceID string
	TenantID           string
	ActorPrincipalID   string
	ReasonCode         string
	Narrative          *string
	EvidenceRefs       []string
	CorrelationID      string
	CausationID        *string
}

// VerifyReleaseParams holds input for verifying an approval before release.
type VerifyReleaseParams struct {
	WorkflowInstanceID        string
	TenantID                  string
	ExpectedSubjectVersion    *int
	CurrentSubjectFingerprint string
}

// ReleaseVerificationResult is the outcome of a release gate evaluation.
type ReleaseVerificationResult struct {
	WorkflowInstanceID string  `json:"workflow_instance_id"`
	CanRelease         bool    `json:"can_release"`
	Status             string  `json:"status"` // "VALID" | "INVALID"
	Reason             *string `json:"reason,omitempty"`
	WorkflowStatus     string  `json:"workflow_status"`
	SubjectFingerprint *string `json:"subject_fingerprint,omitempty"`
}

// ── errors ───────────────────────────────────────────────────────────────────

var ErrWorkflowNotFound = errorString("workflow not found")
var ErrNoStages = errorString("workflow must have at least one stage")
var ErrInvalidTransition = errorString("invalid workflow status transition")
var ErrWrongApprover = errorString("actor is not the approver for the current stage")
var ErrStoreUnavailable = errorString("workflow store unavailable")
var ErrAuthorizationDenied = errorString("authorization denied for this approval action")
var ErrAuthorizationServiceUnavailable = errorString("authorization-svc unavailable")

// ErrInitiatorCannotBeApprover is a creation-time validation error:
// Segregation of Duties (docs/original_doc/zoiko_suite_doc1.txt §12.3)
// forbids a workflow's initiator from being listed as an approver in any
// of its own stages.
var ErrInitiatorCannotBeApprover = errorString("initiated_by may not appear as an approver in any stage")

// ErrSelfApprovalNotAllowed is the decision-time, defense-in-depth
// enforcement of the same Segregation of Duties doctrine: the principal who
// initiated a workflow may not submit an approve/reject action on it, even
// if they were (incorrectly) recorded as an assigned approver for the
// current stage.
var ErrSelfApprovalNotAllowed = errorString("principal may not approve or decide on their own submission")

// Subject binding, invalidation & release gate errors per ZS-STATE-001.
var ErrInvalidSubjectFingerprint = errorString("invalid subject fingerprint format")
var ErrInvalidSubjectVersion = errorString("subject version must be non-negative")
var ErrMissingSubjectField = errorString("subject_type and subject_id must both be provided if either is present")
var ErrWorkflowNotApproved = errorString("workflow is not approved")
var ErrWorkflowInvalidated = errorString("workflow approval has been invalidated")
var ErrSubjectFingerprintMismatch = errorString("subject fingerprint does not match approved fingerprint")
var ErrSubjectVersionMismatch = errorString("subject version does not match expected version")
var ErrWorkflowUnboundSubject = errorString("workflow has no bound subject fingerprint for release verification")
var ErrInvalidReasonCode = errorString("invalid or unrecognized reason code")

// Quorum stage errors (ZS-SVC-R-001 §5.1/§8.3) — see quorum.go.
var ErrInvalidStageType = errorString("stage_type must be SINGLE or QUORUM")
var ErrQuorumRequiresApprovers = errorString("a QUORUM stage requires at least one quorum_approvers entry")
var ErrQuorumThresholdExceedsPool = errorString("required_approvals cannot exceed the number of quorum_approvers")
var ErrQuorumDuplicateApprover = errorString("quorum_approvers must not list the same principal twice")
var ErrNotQuorumStage = errorString("the current stage is not a QUORUM stage")
var ErrNotEligibleQuorumApprover = errorString("actor is not an eligible approver for this quorum stage")

type errorString string

func (e errorString) Error() string { return string(e) }
