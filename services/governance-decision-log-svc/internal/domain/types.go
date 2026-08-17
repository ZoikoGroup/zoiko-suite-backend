// Package domain contains the authoritative domain types for
// governance-decision-log-svc.
//
// GovernanceDecision is an append-only evidence record: once written it is
// never updated or deleted (doctrine.md — no soft-delete on material
// objects, evidence is append-only). action_type, outcome, and rule_basis
// are plain strings — data driven, never Go enums or switch/case branches.
package domain

import (
	"encoding/json"
	"errors"
	"time"
)

// ErrDecisionNotFound is returned when a lookup finds no matching row.
var ErrDecisionNotFound = errors.New("governance decision not found")

// ErrStoreUnavailable is returned when the store cannot be reached at all
// (as distinct from a legitimate "not found"). Callers must treat the two
// differently — see jurisdiction-rules-svc's fail-closed precedent.
var ErrStoreUnavailable = errors.New("governance decision store unavailable")

// GovernanceDecision is the durable record of one governance evaluation.
//
// This is the MVP schema (see CONTEXT.md "FINALIZED — MVP schema"): a
// deliberate simplification of the full GovernanceDecision entity in
// docs/architecture/04-data-model.md §7.1. Fields not promoted to columns
// here (policy_version_id, action_subject_type, action_subject_id) belong
// inside EvaluationContext until there's a concrete need to query on them
// directly. workflow_instance_id and causation_id WERE in that category but
// were promoted to first-class columns (migration 000003): this is the
// platform's canonical governance evidence log, and "every decision made
// during workflow instance X" is a real query, not a hypothetical one.
type GovernanceDecision struct {
	// DecisionID is caller-supplied and is the idempotency/dedup key.
	DecisionID string `json:"decision_id"`

	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`

	// ActorID is the principal that triggered the evaluated action.
	ActorID string `json:"actor_id"`

	ActionType string `json:"action_type"`

	// Outcome is data-driven, e.g. GRANTED, DENIED, ESCALATED.
	Outcome string `json:"outcome"`

	// RuleBasis references the policy/jurisdiction rule that produced
	// Outcome — doctrine requires basis, not just outcome, to be stored.
	RuleBasis string `json:"rule_basis"`

	// EvaluationContext is a JSONB catch-all for caller-supplied context
	// that doesn't yet have a first-class column (e.g. policy_version_id,
	// workflow_instance_id). Optional.
	EvaluationContext json.RawMessage `json:"evaluation_context,omitempty"`

	CorrelationID string `json:"correlation_id"`

	// WorkflowInstanceID is nil when this decision was not made in the
	// context of a workflow instance.
	WorkflowInstanceID *string `json:"workflow_instance_id,omitempty"`
	// CausationID is nil when the causing event/decision is not known.
	CausationID *string `json:"causation_id,omitempty"`

	// DecidedAt is when the governance decision was made upstream (by
	// Policy/Authorization/Workflow), not when it was logged here. If the
	// caller omits it, the handler defaults it to server-receipt time.
	DecidedAt time.Time `json:"decided_at"`
}

// ErrReplayNotSupportedForActionType is returned when a decision's
// action_type has no replay logic implemented — same v1 scope limitation
// as policy-svc's own Evaluate endpoint (APPROVAL_THRESHOLD only), stated
// explicitly rather than silently claiming a replay that didn't happen.
var ErrReplayNotSupportedForActionType = errors.New("replay not implemented for this decision's action_type")

// ErrReplayManifestNotFound is returned when a lookup finds no matching
// replay manifest.
var ErrReplayManifestNotFound = errors.New("replay manifest not found")

// ReplayManifest is doc7's replay-reproducibility record (backlog item
// 34): a permanent statement that decision DecisionID was re-evaluated
// against the EXACT PolicyVersionID it originally used, and whether that
// replay reproduced the OriginalOutcome. Append-only — never updated.
type ReplayManifest struct {
	ReplayManifestID      string    `json:"replay_manifest_id"`
	DecisionID            string    `json:"decision_id"`
	PolicyVersionID       string    `json:"policy_version_id"`
	ReplayedOutcome       string    `json:"replayed_outcome"`
	OriginalOutcome       string    `json:"original_outcome"`
	OutcomesMatch         bool      `json:"outcomes_match"`
	ReplayNotes           *string   `json:"replay_notes,omitempty"`
	ReplayedAt            time.Time `json:"replayed_at"`
	ReplayedByPrincipalID string    `json:"replayed_by_principal_id"`
}
