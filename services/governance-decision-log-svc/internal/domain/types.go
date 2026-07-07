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
// here (policy_version_id, action_subject_type, action_subject_id,
// workflow_instance_id) belong inside EvaluationContext until there's a
// concrete need to query on them directly.
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

	// DecidedAt is when the governance decision was made upstream (by
	// Policy/Authorization/Workflow), not when it was logged here. If the
	// caller omits it, the handler defaults it to server-receipt time.
	DecidedAt time.Time `json:"decided_at"`
}
