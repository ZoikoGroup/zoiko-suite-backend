package domain

import (
	"errors"
	"time"
)

// BIZ-08 Business Deadline — a fourth bolted-on domain in this service,
// beside ExceptionCase/EscalationRecord, AuditFinding, and Task/Case. See
// migration 000004's own doc comment for the full design rationale.
//
// BIZ-08 owns generic operational deadline instances for non-tax/
// non-legal business obligations. It may MIRROR an authoritative TAX/LEG
// deadline for work-coordination purposes, but a mirrored deadline's due
// date can never be recalculated locally — the real enforcement of the
// doc's own prohibited anti-pattern, "Recalculating legal/tax deadlines
// in BIZ-08 instead of consuming authoritative source."

type DeadlineStatus string

const (
	DeadlineStatusScheduled  DeadlineStatus = "SCHEDULED"
	DeadlineStatusCompleted  DeadlineStatus = "COMPLETED"
	DeadlineStatusWaived     DeadlineStatus = "WAIVED"
	DeadlineStatusCancelled  DeadlineStatus = "CANCELLED"
	DeadlineStatusSuperseded DeadlineStatus = "SUPERSEDED"
)

// Deadline is BIZ-08's own authority. Upcoming/Due/Overdue are NOT
// stored statuses — they are computed live from DueAt against SCHEDULED,
// same "no background clock service" doctrine as BIZ-05's own SLAState.
type Deadline struct {
	DeadlineID       string         `json:"deadline_id"`
	TenantID         string         `json:"tenant_id"`
	LegalEntityID    string         `json:"legal_entity_id"`
	Title            string         `json:"title"`
	LinkedObjectType string         `json:"linked_object_type,omitempty"`
	LinkedObjectID   string         `json:"linked_object_id,omitempty"`
	DueAt            time.Time      `json:"due_at"`
	OwnerPrincipalID string         `json:"owner_principal_id,omitempty"`
	Status           DeadlineStatus `json:"status"`

	// Mirroring / source lock — see MirrorAuthoritativeDeadlineParams's
	// own doc comment. SourceType is empty for a deadline BIZ-08 owns
	// outright.
	SourceType    string `json:"source_type,omitempty"`
	SourceRef     string `json:"source_ref,omitempty"`
	SourceVersion string `json:"source_version,omitempty"`

	// CalcRule/CalcInputs back ExplainCalculation (Wave 2) — recorded at
	// creation/recalculation time, not derived after the fact.
	CalcRule   string `json:"calc_rule,omitempty"`
	CalcInputs string `json:"calc_inputs,omitempty"`

	CompletedAt            *time.Time `json:"completed_at,omitempty"`
	CompletedByPrincipalID string     `json:"completed_by_principal_id,omitempty"`
	WaivedAt               *time.Time `json:"waived_at,omitempty"`
	WaivedByPrincipalID    string     `json:"waived_by_principal_id,omitempty"`
	WaiverReason           string     `json:"waiver_reason,omitempty"`
	CancelledAt            *time.Time `json:"cancelled_at,omitempty"`
	CancelledByPrincipalID string     `json:"cancelled_by_principal_id,omitempty"`
	CancelReason           string     `json:"cancel_reason,omitempty"`
	SupersededAt           *time.Time `json:"superseded_at,omitempty"`
	SupersededByDeadlineID string     `json:"superseded_by_deadline_id,omitempty"`

	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// DeadlineEscalation is the append-only escalation evidence trail —
// Wave 2's own Escalate command writes these. Same split as
// TaskTransition.
type DeadlineEscalation struct {
	EscalationID           string    `json:"escalation_id"`
	DeadlineID             string    `json:"deadline_id"`
	TenantID               string    `json:"tenant_id"`
	EscalatedToRole        string    `json:"escalated_to_role"`
	Reason                 string    `json:"reason,omitempty"`
	EscalatedAt            time.Time `json:"escalated_at"`
	EscalatedByPrincipalID string    `json:"escalated_by_principal_id"`
}

// DeadlineSourceInfo is GetSourceDeadline's own result — a passthrough
// of what this deadline itself records about its authoritative source,
// same posture as BIZ-05's own GetLinkedObjectStatus: no generic
// cross-service dispatcher exists in this codebase to look a TAX/LEG
// deadline up live, so this reports the recorded lock, not a live fetch.
type DeadlineSourceInfo struct {
	DeadlineID    string    `json:"deadline_id"`
	SourceType    string    `json:"source_type,omitempty"`
	SourceRef     string    `json:"source_ref,omitempty"`
	SourceVersion string    `json:"source_version,omitempty"`
	DueAt         time.Time `json:"due_at"`
	// Mirrored is false for a deadline BIZ-08 owns outright.
	Mirrored bool   `json:"mirrored"`
	Note     string `json:"note"`
}

// ── params ───────────────────────────────────────────────────────────────────

// CreateDeadlineParams — BIZ-08's own CreateDeadline command, for a
// deadline BIZ-08 calculates and owns outright (source fields left
// empty).
type CreateDeadlineParams struct {
	TenantID, LegalEntityID          string
	Title                            string
	LinkedObjectType, LinkedObjectID string
	DueAt                            time.Time
	OwnerPrincipalID                 string
	CalcRule, CalcInputs             string
	CreatedByPrincipalID             string
}

// MirrorAuthoritativeDeadlineParams — BIZ-08's own
// MirrorAuthoritativeDeadline command. SourceType/SourceRef/SourceVersion
// are required and lock this deadline: Recalculate refuses once these
// are set (Wave 2). Calling this again for a source that already has a
// live local mirror either supersedes it cleanly (the prior row was
// still SCHEDULED — untouched by any local action) or is refused with
// ErrDeadlineSourceConflict (a human already completed/waived/cancelled
// the prior row locally, so silently moving its due date would rewrite
// what that local action was actually taken against).
type MirrorAuthoritativeDeadlineParams struct {
	TenantID, LegalEntityID              string
	Title                                string
	LinkedObjectType, LinkedObjectID     string
	DueAt                                time.Time
	OwnerPrincipalID                     string
	SourceType, SourceRef, SourceVersion string
	CreatedByPrincipalID                 string
}

type AssignOwnerParams struct {
	DeadlineID, TenantID, ActorPrincipalID, OwnerPrincipalID string
}

type CompleteDeadlineParams struct {
	DeadlineID, TenantID, ActorPrincipalID string
}

// ── errors ───────────────────────────────────────────────────────────────────

var (
	ErrDeadlineNotFound       = errors.New("deadline not found")
	ErrDeadlineInvalidState   = errors.New("deadline is not in a state that permits this action")
	ErrDeadlineSourceRequired = errors.New("source_type, source_ref and source_version are required to mirror an authoritative deadline")

	// ErrDeadlineSourceConflict is the doc's own named stable error,
	// DEADLINE_SOURCE_CONFLICT — see MirrorAuthoritativeDeadlineParams's
	// own doc comment for when this fires.
	ErrDeadlineSourceConflict = errors.New("mirrored deadline conflicts with authoritative TAX/LEG/business source")

	// ErrCannotRecalculateMirroredDeadline is Wave 2's own guard,
	// declared here alongside its sibling errors — Recalculate must
	// refuse a mirrored deadline (SourceType set), full stop.
	ErrCannotRecalculateMirroredDeadline = errors.New("a mirrored deadline's due date cannot be recalculated locally — it must be re-mirrored from its authoritative source")
)
