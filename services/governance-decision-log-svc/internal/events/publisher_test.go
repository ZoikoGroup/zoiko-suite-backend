// Package events_test asserts the event envelope actually carries the fields
// Doc 03 §19 and the Event Catalogue envelope require.
package events_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/governance-decision-log-svc/internal/domain"
	"zoiko.io/governance-decision-log-svc/internal/events"
)

type envelope struct {
	EventID            string          `json:"event_id"`
	EventType          string          `json:"event_type"`
	EventVersion       string          `json:"event_version"`
	SourceService      string          `json:"source_service"`
	TenantID           string          `json:"tenant_id"`
	LegalEntityID      string          `json:"legal_entity_id"`
	ActorID            string          `json:"actor_id"`
	CorrelationID      string          `json:"correlation_id"`
	Jurisdiction       *string         `json:"jurisdiction_context"`
	CausationID        *string         `json:"causation_id"`
	WorkflowInstanceID *string         `json:"workflow_instance_id"`
	AggregateID        string          `json:"aggregate_id"`
	PayloadHash        string          `json:"payload_hash"`
	Payload            json.RawMessage `json:"payload"`
}

func build(t *testing.T, d domain.GovernanceDecision) envelope {
	t.Helper()
	raw, err := events.DecisionRecorded(d)
	require.NoError(t, err)
	var env envelope
	require.NoError(t, json.Unmarshal(raw, &env))
	return env
}

func TestDecisionRecorded_EnvelopeCarriesIdentityLinkageAndHash(t *testing.T) {
	cause, wf, jur := "evt-cause", "wf-9", "JUR-GB"
	env := build(t, domain.GovernanceDecision{
		DecisionID: "dec-1", TenantID: "tenant-1", LegalEntityID: "entity-1",
		ActorID: "actor-1", CorrelationID: "corr-1", CausationID: &cause,
		WorkflowInstanceID: &wf, RuleBasis: "PC:v1", DecidedAt: time.Now(), JurisdictionContext: &jur,
	})

	assert.Equal(t, "governance.decision.recorded", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "governance-decision-log-svc", env.SourceService)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "actor-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.Equal(t, "dec-1", env.AggregateID)
	require.NotNil(t, env.CausationID)
	assert.Equal(t, "evt-cause", *env.CausationID)
	require.NotNil(t, env.WorkflowInstanceID)
	assert.Equal(t, "wf-9", *env.WorkflowInstanceID)
	assert.NotEmpty(t, env.EventID)

	sum := sha256.Sum256(env.Payload)
	assert.Equal(t, "sha256:"+hex.EncodeToString(sum[:]), env.PayloadHash)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(env.Payload, &payload))
	assert.Equal(t, "JUR-GB", payload["jurisdiction_context"], "jurisdiction context is the decision's own, not rule_basis")
	require.NotNil(t, env.Jurisdiction)
	assert.Equal(t, "JUR-GB", *env.Jurisdiction)
	assert.Equal(t, "PC:v1", payload["rule_basis"])
}

func TestDecisionRecorded_RepeatEventsOnSameDecision_GetDistinctEventIDs(t *testing.T) {
	d := domain.GovernanceDecision{DecisionID: "dec-1", TenantID: "tenant-1", CorrelationID: "corr-x"}
	assert.NotEqual(t, build(t, d).EventID, build(t, d).EventID)
}

func TestDecisionReplayed_EnvelopeNamesTheReplayerAndTheDecision(t *testing.T) {
	raw, err := events.DecisionReplayed(
		domain.GovernanceDecision{DecisionID: "dec-1", TenantID: "t", LegalEntityID: "le", ActorID: "original-actor"},
		domain.ReplayManifest{ReplayManifestID: "m-1", DecisionID: "dec-1", TenantID: "t", ReplayedByPrincipalID: "auditor", OutcomesMatch: true},
		"corr-r")
	require.NoError(t, err)
	var env struct {
		EventType     string `json:"event_type"`
		ActorID       string `json:"actor_id"`
		AggregateID   string `json:"aggregate_id"`
		CorrelationID string `json:"correlation_id"`
	}
	require.NoError(t, json.Unmarshal(raw, &env))
	assert.Equal(t, "governance.decision.replayed", env.EventType)
	assert.Equal(t, "auditor", env.ActorID)
	assert.Equal(t, "dec-1", env.AggregateID)
	assert.Equal(t, "corr-r", env.CorrelationID)
}
