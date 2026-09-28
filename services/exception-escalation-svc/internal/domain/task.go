package domain

import (
	"errors"
	"time"
)

// BIZ-05 Task / Case Management — a third bolted-on domain in this
// service, beside ExceptionCase/EscalationRecord and AuditFinding. Case
// is a lightweight container; Task is the real governed entity with its
// own lifecycle (New -> Assigned -> In Progress -> Blocked/Escalated ->
// Completed -> Closed, Reopened as its own governed transition).
// task_transitions (see TaskTransition below) is the append-only audit
// trail — same split as workflow-svc's WorkflowInstance/WorkflowTransition.
//
// CreateCase is not named in the doc's own command list (only CloseCase
// is), but the doc's canonical-entity table lists Case as its own
// write-owned entity with nothing else that creates one — filled here,
// same class of gap as BIZ-04's missing RetireForm.

// WorkCaseStatus values for Case.Status. Named to avoid colliding with
// this file's sibling CaseStatus (ExceptionCase's own, unrelated status
// enum).
type WorkCaseStatus string

const (
	WorkCaseOpen   WorkCaseStatus = "OPEN"
	WorkCaseClosed WorkCaseStatus = "CLOSED"
)

type Case struct {
	CaseID        string         `json:"case_id"`
	TenantID      string         `json:"tenant_id"`
	LegalEntityID string         `json:"legal_entity_id"`
	CaseType      string         `json:"case_type"`
	Purpose       string         `json:"purpose,omitempty"`
	Status        WorkCaseStatus `json:"status"`
	CreatedBy     string         `json:"created_by"`
	CreatedAt     time.Time      `json:"created_at"`
	ClosedBy      string         `json:"closed_by,omitempty"`
	ClosedAt      *time.Time     `json:"closed_at,omitempty"`
	ClosureReason string         `json:"closure_reason,omitempty"`
}

// TaskStatus values. New -> Assigned -> In Progress -> Blocked/Escalated
// -> Completed -> Closed, exactly as the doc's own lifecycle line
// states, plus Cancelled (the doc's own Cancel command) and Reopened as
// a governed transition back into IN_PROGRESS from COMPLETED/CLOSED
// (not its own terminal status — see ReopenTask's own doc comment).
type TaskStatus string

const (
	TaskStatusNew        TaskStatus = "NEW"
	TaskStatusAssigned   TaskStatus = "ASSIGNED"
	TaskStatusInProgress TaskStatus = "IN_PROGRESS"
	TaskStatusBlocked    TaskStatus = "BLOCKED"
	TaskStatusEscalated  TaskStatus = "ESCALATED"
	TaskStatusCompleted  TaskStatus = "COMPLETED"
	TaskStatusClosed     TaskStatus = "CLOSED"
	TaskStatusCancelled  TaskStatus = "CANCELLED"
)

type TaskPriority string

const (
	TaskPriorityLow      TaskPriority = "LOW"
	TaskPriorityMedium   TaskPriority = "MEDIUM"
	TaskPriorityHigh     TaskPriority = "HIGH"
	TaskPriorityCritical TaskPriority = "CRITICAL"
)

type Task struct {
	TaskID           string       `json:"task_id"`
	TenantID         string       `json:"tenant_id"`
	LegalEntityID    string       `json:"legal_entity_id"`
	CaseID           *string      `json:"case_id,omitempty"`
	TaskType         string       `json:"task_type"`
	Priority         TaskPriority `json:"priority"`
	BusinessTrigger  string       `json:"business_trigger,omitempty"`
	LinkedObjectType string       `json:"linked_object_type"`
	LinkedObjectID   string       `json:"linked_object_id"`
	RequiredEvidence string       `json:"required_evidence,omitempty"`
	Status           TaskStatus   `json:"status"`
	AssignedToRole   string       `json:"assigned_to_role,omitempty"`
	AssignedToUser   string       `json:"assigned_to_user,omitempty"`
	SLADeadline      *time.Time   `json:"sla_deadline,omitempty"`
	BlockedReason    string       `json:"blocked_reason,omitempty"`
	EscalatedToRole  string       `json:"escalated_to_role,omitempty"`
	EscalatedAt      *time.Time   `json:"escalated_at,omitempty"`
	CompletionNotes  string       `json:"completion_notes,omitempty"`
	CompletedAt      *time.Time   `json:"completed_at,omitempty"`
	ClosedBy         string       `json:"closed_by,omitempty"`
	ClosedAt         *time.Time   `json:"closed_at,omitempty"`
	CancelReason     string       `json:"cancel_reason,omitempty"`
	CancelledAt      *time.Time   `json:"cancelled_at,omitempty"`
	ReopenedCount    int          `json:"reopened_count"`
	CreatedBy        string       `json:"created_by"`
	CreatedAt        time.Time    `json:"created_at"`
	UpdatedAt        time.Time    `json:"updated_at"`
}

// TaskTransition is the append-only audit trail — one row per state
// change, never updated or deleted. Backs GetHistory.
type TaskTransition struct {
	TransitionID     string    `json:"transition_id"`
	TaskID           string    `json:"task_id"`
	TenantID         string    `json:"tenant_id"`
	FromStatus       string    `json:"from_status"`
	ToStatus         string    `json:"to_status"`
	ActorPrincipalID string    `json:"actor_principal_id"`
	Reason           string    `json:"reason,omitempty"`
	OccurredAt       time.Time `json:"occurred_at"`
}

// SLAState is GetSLAState's own result — computed live at read time from
// the stored deadline, not via a background clock service. This is
// deliberate: the doc's own failure mode ("SLA clock service failure
// does not silently close/escalate") is trivially satisfied when nothing
// auto-transitions a task's state at all — there is no clock to fail.
type SLAState struct {
	TaskID      string     `json:"task_id"`
	SLADeadline *time.Time `json:"sla_deadline,omitempty"`
	Overdue     bool       `json:"overdue"`
	// TimeRemaining is omitted (nil) when there is no deadline set, or
	// once it has passed (Overdue is the signal at that point).
	TimeRemainingSeconds *int64 `json:"time_remaining_seconds,omitempty"`
}

// LinkedObjectStatus is GetLinkedObjectStatus's own result. BIZ-05 does
// not own linked-object state (the doc's own purpose line), and no
// generic cross-service dispatcher exists in this codebase to look one
// up live — same conclusion reached for BIZ-04's RouteToDomain. This is
// a passthrough of what the task itself records, not a live lookup.
type LinkedObjectStatus struct {
	TaskID           string `json:"task_id"`
	LinkedObjectType string `json:"linked_object_type"`
	LinkedObjectID   string `json:"linked_object_id"`
	// Tracked is always false in this wave — documents plainly that this
	// service does not (and per its own purpose line, must not) track
	// the linked object's real status.
	Tracked bool   `json:"tracked"`
	Note    string `json:"note"`
}

// ── params ───────────────────────────────────────────────────────────────────

type CreateCaseParams struct {
	TenantID, LegalEntityID, CaseType, Purpose, CreatedByPrincipalID string
}

type CloseCaseParams struct {
	CaseID, TenantID, ActorPrincipalID, ClosureReason string
}

type CreateTaskParams struct {
	TenantID, LegalEntityID          string
	CaseID                           string // optional
	TaskType                         string
	Priority                         TaskPriority
	BusinessTrigger                  string
	LinkedObjectType, LinkedObjectID string
	RequiredEvidence                 string
	SLADeadline                      *time.Time
	AssignedToRole, AssignedToUser   string
	CreatedByPrincipalID             string
}

type AssignTaskParams struct {
	TaskID, TenantID, ActorPrincipalID string
	AssignedToRole, AssignedToUser     string
}

type StartTaskParams struct {
	TaskID, TenantID, ActorPrincipalID string
}

type BlockTaskParams struct {
	TaskID, TenantID, ActorPrincipalID, Reason string
}

type EscalateTaskParams struct {
	TaskID, TenantID, ActorPrincipalID, EscalatedToRole, Reason string
}

type CompleteTaskParams struct {
	TaskID, TenantID, ActorPrincipalID, CompletionNotes string
}

type ReopenTaskParams struct {
	TaskID, TenantID, ActorPrincipalID, Reason string
}

type CancelTaskParams struct {
	TaskID, TenantID, ActorPrincipalID, Reason string
}

// ListQueueParams filters ListQueue's read.
type ListQueueParams struct {
	TenantID, LegalEntityID, AssignedToUser, AssignedToRole, Status string
}

// ── errors ───────────────────────────────────────────────────────────────────

var (
	ErrCaseNotFound        = errors.New("case not found")
	ErrCaseAlreadyClosedWC = errors.New("case is already closed")
	ErrTaskNotFound        = errors.New("task not found")
	ErrTaskInvalidState    = errors.New("task is not in a state that permits this action")
)
