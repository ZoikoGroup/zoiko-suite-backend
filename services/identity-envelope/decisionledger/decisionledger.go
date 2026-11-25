// Package decisionledger delivers a governance engine's verdicts to
// governance-decision-log-svc (Doc 01 s7: "Every governance decision is itself
// evidence"), for the writers that joined in the automatic decision capture
// assignment of 25 Nov 2026: authorization-svc, jurisdiction-rules-svc,
// obligations-svc and evidence-requirements-svc.
//
// The pattern is policy-svc's evidence relay (decision C6): the writer puts a
// Record into its own decision_log_outbox table in the transaction of the
// decision it records (Enqueue), and a Relay leases due rows, POSTs each to the
// ledger's mTLS listener with the writer's workload certificate, and marks it
// delivered, retried with backoff, or parked. A ledger outage delays the
// evidence; it never fails or slows the source decision (evidence fails safe,
// Doc 03 s22).
//
// The ledger record's decision_id is derived from the source record, and the
// Idempotency-Key from the decision_id, so a redelivery after a lost response
// is answered as a replay (200), never a second decision.
package decisionledger

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Record is one governance decision as governance-decision-log-svc's
// POST /v1/decisions takes it. The actor is never in the body: the ledger
// takes it from the authenticated principal (the writer's service identity);
// the person who caused the decision belongs in EvaluationContext under
// OnBehalfOfKey. Never put attribute values or Restricted fields here.
type Record struct {
	DecisionID          string         `json:"decision_id"`
	TenantID            string         `json:"tenant_id"`
	LegalEntityID       string         `json:"legal_entity_id"`
	ActionType          string         `json:"action_type"`
	Outcome             string         `json:"outcome"`
	RuleBasis           string         `json:"rule_basis"`
	JurisdictionContext string         `json:"jurisdiction_context,omitempty"`
	PolicyVersionID     string         `json:"policy_version_id,omitempty"`
	ActionSubjectType   string         `json:"action_subject_type,omitempty"`
	ActionSubjectID     string         `json:"action_subject_id,omitempty"`
	EvaluationContext   map[string]any `json:"evaluation_context,omitempty"`
	CorrelationID       string         `json:"correlation_id"`
	CausationID         string         `json:"causation_id,omitempty"`
	WorkflowInstanceID  string         `json:"workflow_instance_id,omitempty"`
	DecidedAt           time.Time      `json:"decided_at"`
	Title               string         `json:"title,omitempty"`
	Summary             string         `json:"summary,omitempty"`
	Domain              string         `json:"domain,omitempty"`
}

// OnBehalfOfKey is the evaluation_context key naming the principal whose
// request caused the decision (the writer itself is the ledger actor).
const OnBehalfOfKey = "on_behalf_of"

// limits are governance-decision-log-svc's field limits (handler.CreateDecision).
var limits = []struct {
	name string
	get  func(*Record) *string
	max  int
}{
	{"action_type", func(r *Record) *string { return &r.ActionType }, 128},
	{"rule_basis", func(r *Record) *string { return &r.RuleBasis }, 256},
	{"jurisdiction_context", func(r *Record) *string { return &r.JurisdictionContext }, 64},
	{"policy_version_id", func(r *Record) *string { return &r.PolicyVersionID }, 64},
	{"action_subject_type", func(r *Record) *string { return &r.ActionSubjectType }, 64},
	{"action_subject_id", func(r *Record) *string { return &r.ActionSubjectID }, 64},
	{"causation_id", func(r *Record) *string { return &r.CausationID }, 128},
	{"workflow_instance_id", func(r *Record) *string { return &r.WorkflowInstanceID }, 128},
	{"title", func(r *Record) *string { return &r.Title }, 200},
	{"summary", func(r *Record) *string { return &r.Summary }, 2000},
}

// ErrInvalidRecord is returned by Normalize for a record the ledger would refuse.
var ErrInvalidRecord = errors.New("decisionledger: invalid record")

// Normalize clips the free-text fields to the ledger's limits (the full value
// can travel in EvaluationContext) and checks the fields the ledger requires.
// Identifiers are never clipped: a decision_id, tenant, entity, outcome or
// correlation id over its limit is an error, because a clipped identifier
// would name something else.
func (r *Record) Normalize() error {
	for _, l := range limits {
		if p := l.get(r); len(*p) > l.max {
			*p = (*p)[:l.max]
		}
	}
	if r.DecidedAt.IsZero() {
		return fmt.Errorf("%w: decided_at is required", ErrInvalidRecord)
	}
	r.DecidedAt = r.DecidedAt.UTC()
	for _, f := range []struct {
		name, v string
		max     int
	}{
		{"decision_id", r.DecisionID, 64}, {"tenant_id", r.TenantID, 64}, {"legal_entity_id", r.LegalEntityID, 64},
		{"action_type", r.ActionType, 128}, {"outcome", r.Outcome, 32}, {"rule_basis", r.RuleBasis, 256},
		{"correlation_id", r.CorrelationID, 64},
	} {
		if f.v == "" {
			return fmt.Errorf("%w: %s is required", ErrInvalidRecord, f.name)
		}
		if len(f.v) > f.max {
			return fmt.Errorf("%w: %s is longer than %d", ErrInvalidRecord, f.name, f.max)
		}
	}
	return nil
}

// ErrPermanent marks a delivery the ledger refused in a way no retry can fix
// (409: a different decision already holds this decision_id or key). The relay
// parks the row for an operator instead of retrying it.
var ErrPermanent = errors.New("decisionledger: refused permanently")

// Sender delivers one record to the ledger.
type Sender interface {
	Send(ctx context.Context, rec Record) error
}

// Pending is one leased outbox row.
type Pending struct {
	DecisionID string
	TenantID   string
	Record     Record
	Attempts   int
}

// Queue is the writer's decision_log_outbox, as the relay uses it.
type Queue interface {
	// Claim leases up to limit due rows: their next attempt moves lease
	// ahead, so another replica does not deliver them at the same time.
	Claim(ctx context.Context, limit int, lease time.Duration) ([]Pending, error)
	MarkDelivered(ctx context.Context, decisionID string) error
	MarkRetry(ctx context.Context, decisionID, lastError string, next time.Time) error
	MarkParked(ctx context.Context, decisionID, lastError string) error
}
