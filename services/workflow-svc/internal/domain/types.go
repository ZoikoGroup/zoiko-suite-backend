// Package domain contains the authoritative domain types for workflow-svc.
//
// workflow_type and stage_status are plain strings — no Go enums, same
// doctrine as every other service in this platform. workflow_status IS a
// real (small) state machine: PENDING -> APPROVED | REJECTED | ESCALATED |
// CANCELLED, enforced in application code.
package domain

import "time"

// WorkflowInstance is one approval request moving through an ordered chain
// of approval stages. Critical constraint (mirrors every other service):
// entity-bound (LegalEntityID), never hard-deleted — cancellation is a
// status transition, not a row removal.
type WorkflowInstance struct {
	WorkflowInstanceID string `json:"workflow_instance_id"`

	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`

	// WorkflowType is data only (e.g. "PURCHASE_APPROVAL").
	WorkflowType string `json:"workflow_type"`

	// WorkflowStatus: PENDING | APPROVED | REJECTED | ESCALATED | CANCELLED.
	// APPROVED/REJECTED/CANCELLED are terminal.
	WorkflowStatus string `json:"workflow_status"`

	// CurrentStage is the 1-based stage_order currently awaiting action.
	// 0 once the workflow reaches a terminal state.
	CurrentStage int `json:"current_stage"`

	InitiatedBy   string     `json:"initiated_by"`
	CorrelationID string     `json:"correlation_id"`
	StartedAt     time.Time  `json:"started_at"`
	CompletedAt   *time.Time `json:"completed_at"`
}

// WorkflowStage is one approver slot in a workflow's ordered chain, supplied
// by the caller at creation time — this service does not resolve "who
// should approve X" from any rule engine; no such rules are specified
// anywhere in the architecture docs. See progress.md.
type WorkflowStage struct {
	WorkflowStageID    string `json:"workflow_stage_id"`
	WorkflowInstanceID string `json:"workflow_instance_id"`

	StageOrder          int    `json:"stage_order"`
	ApproverPrincipalID string `json:"approver_principal_id"`

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

	ActedAt time.Time `json:"acted_at"`
}

// ── params ───────────────────────────────────────────────────────────────────

// CreateWorkflowStageInput is one entry in the caller-supplied approval chain.
type CreateWorkflowStageInput struct {
	ApproverPrincipalID string `json:"approver_principal_id"`
}

type CreateWorkflowParams struct {
	WorkflowInstanceID string
	TenantID           string
	LegalEntityID      string
	WorkflowType       string
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
}

// ── errors ───────────────────────────────────────────────────────────────────

var ErrWorkflowNotFound = errorString("workflow not found")
var ErrNoStages = errorString("workflow must have at least one stage")
var ErrInvalidTransition = errorString("invalid workflow status transition")
var ErrWrongApprover = errorString("actor is not the approver for the current stage")
var ErrStoreUnavailable = errorString("workflow store unavailable")
var ErrAuthorizationDenied = errorString("authorization denied for this approval action")
var ErrAuthorizationServiceUnavailable = errorString("authorization-svc unavailable")

type errorString string

func (e errorString) Error() string { return string(e) }
