// Package events builds the domain events this service publishes.
//
// Nothing here talks to Kafka on the request path. A decision's event is built
// by DecisionRecorded, written to the outbox in the same transaction as the
// decision itself (store.RecordDecision), and published later by the relay
// (internal/outbox). A broker outage or a client disconnect after commit can
// therefore delay an event but never lose it (GCP §2 invariant #10, Event
// Catalogue §6).
package events

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"

	"zoiko.io/governance-decision-log-svc/internal/domain"
)

// DecisionRecordedType is the event name Doc 03 §8.7 publishes. The Event
// Catalogue's com.zoikosuite.* pattern is not used: which naming is the wire
// contract is an open group-level question (audit C3).
const DecisionRecordedType = "governance.decision.recorded"

// DecisionReplayedType records that a decision was re-evaluated against its
// exact policy version (GCP §2 invariant #11) and whether it reproduced. A
// replay writes a permanent manifest, a material change, so it publishes like
// any other (invariant #10). Listed with Doc 03 §8.7's published events.
const DecisionReplayedType = "governance.decision.replayed"

// envelope is this platform's event contract (Doc 03 §19) plus the linkage and
// integrity fields of the Event Catalogue example envelope: causation id,
// workflow instance, the aggregate the event is about, and a hash of the
// payload so a consumer can detect a payload altered in transit or at rest.
type envelope struct {
	EventID            string          `json:"event_id"`
	EventType          string          `json:"event_type"`
	EventVersion       string          `json:"event_version"`
	EmittedAt          time.Time       `json:"emitted_at"`
	SchemaVersion      string          `json:"schema_version"`
	SourceService      string          `json:"source_service"`
	TenantID           string          `json:"tenant_id,omitempty"`
	LegalEntityID      string          `json:"legal_entity_id,omitempty"`
	ActorID            string          `json:"actor_id,omitempty"`
	CorrelationID      string          `json:"correlation_id"`
	Jurisdiction       *string         `json:"jurisdiction_context,omitempty"`
	CausationID        *string         `json:"causation_id,omitempty"`
	WorkflowInstanceID *string         `json:"workflow_instance_id,omitempty"`
	AggregateType      string          `json:"aggregate_type"`
	AggregateID        string          `json:"aggregate_id"`
	PayloadHash        string          `json:"payload_hash"`
	Payload            json.RawMessage `json:"payload"`
}

// MessageWriter is the one method the relay needs from *kafka.Writer, narrowed
// so tests can assert what is published without a broker.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

// DecisionRecorded returns the full governance.decision.recorded envelope for
// d, ready to be stored in the outbox and published unchanged.
//
// The payload carries every decision field so a consumer needs no follow-up
// read. jurisdiction_context is the jurisdiction the decision was made in
// (000015), never rule_basis passed off as one.
func DecisionRecorded(d domain.GovernanceDecision) ([]byte, error) {
	return build(DecisionRecordedType, d, map[string]any{
		"decision_id":          d.DecisionID,
		"tenant_id":            d.TenantID,
		"legal_entity_id":      d.LegalEntityID,
		"actor_id":             d.ActorID,
		"action_type":          d.ActionType,
		"outcome":              d.Outcome,
		"rule_basis":           d.RuleBasis,
		"jurisdiction_context": d.JurisdictionContext,
		"policy_version_id":    d.PolicyVersionID,
		"action_subject_type":  d.ActionSubjectType,
		"action_subject_id":    d.ActionSubjectID,
		"workflow_instance_id": d.WorkflowInstanceID,
		"causation_id":         d.CausationID,
		"correlation_id":       d.CorrelationID,
		"decided_at":           d.DecidedAt,
	}, d.ActorID, d.CorrelationID)
}

// DecisionReplayed returns the governance.decision.replayed envelope for a
// newly recorded replay manifest m of decision d. The actor is the principal
// who ran the replay; the aggregate is the decision replayed.
func DecisionReplayed(d domain.GovernanceDecision, m domain.ReplayManifest, correlationID string) ([]byte, error) {
	return build(DecisionReplayedType, d, map[string]any{
		"replay_manifest_id":       m.ReplayManifestID,
		"decision_id":              m.DecisionID,
		"tenant_id":                m.TenantID,
		"policy_version_id":        m.PolicyVersionID,
		"original_outcome":         m.OriginalOutcome,
		"replayed_outcome":         m.ReplayedOutcome,
		"outcomes_match":           m.OutcomesMatch,
		"replayed_by_principal_id": m.ReplayedByPrincipalID,
		"replayed_at":              m.ReplayedAt,
	}, m.ReplayedByPrincipalID, correlationID)
}

func build(eventType string, d domain.GovernanceDecision, fields map[string]any, actorID, correlationID string) ([]byte, error) {
	payload, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(payload)
	return json.Marshal(envelope{
		// A fresh UUID per event, not a deterministic string — see
		// docs/architecture/known-gaps.md's event_id collision writeup.
		EventID:            "evt-" + uuid.New().String(),
		EventType:          eventType,
		EventVersion:       "1.0",
		EmittedAt:          time.Now().UTC(),
		SchemaVersion:      "1.2",
		SourceService:      "governance-decision-log-svc",
		TenantID:           d.TenantID,
		LegalEntityID:      d.LegalEntityID,
		ActorID:            actorID,
		CorrelationID:      correlationID,
		Jurisdiction:       d.JurisdictionContext,
		CausationID:        d.CausationID,
		WorkflowInstanceID: d.WorkflowInstanceID,
		AggregateType:      "GovernanceDecision",
		AggregateID:        d.DecisionID,
		PayloadHash:        "sha256:" + hex.EncodeToString(sum[:]),
		Payload:            payload,
	})
}
