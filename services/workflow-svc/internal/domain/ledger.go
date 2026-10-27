package domain

import "time"

// LedgerRecord is the governance-ledger record of one workflow decision
// (Doc 03 §8.4 evidence obligations; GOV §1 "no evidence afterthought"). It
// is written to outbox_events (destination decision_log) in the same
// transaction as the decision, and the ledger relay delivers it to
// governance-decision-log-svc in that service's wire shape. DecisionID is
// generated before the transaction, so the API response carries it as the
// decision's evidence_id even before the ledger has received it.
type LedgerRecord struct {
	DecisionID          string         `json:"decision_id"`
	TenantID            string         `json:"tenant_id"`
	LegalEntityID       string         `json:"legal_entity_id"`
	ActionType          string         `json:"action_type"`
	Outcome             string         `json:"outcome"`
	RuleBasis           string         `json:"rule_basis"`
	CorrelationID       string         `json:"correlation_id"`
	CausationID         string         `json:"causation_id,omitempty"`
	WorkflowInstanceID  string         `json:"workflow_instance_id"`
	JurisdictionContext string         `json:"jurisdiction_context,omitempty"`
	ActionSubjectType   string         `json:"action_subject_type,omitempty"`
	ActionSubjectID     string         `json:"action_subject_id,omitempty"`
	EvaluationContext   map[string]any `json:"evaluation_context"`
	DecidedAt           time.Time      `json:"decided_at"`
	// ActorPrincipalID is who decided; the relay writes as workflow-svc's own
	// ledger principal and carries the actor in evaluation_context.
	ActorPrincipalID string `json:"actor_principal_id"`
}
