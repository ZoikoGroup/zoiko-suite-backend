// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires that this service has real data for
// (event_version, jurisdiction_id, actor_id, correlation_id). tenant_id
// and legal_entity_id are correctly omitted: domain.Jurisdiction and
// domain.JurisdictionRule are platform-wide reference data with no such
// field.
package events_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/jurisdiction-rules-svc/internal/events"
)

type envelope struct {
	EventID        string `json:"event_id"`
	EventType      string `json:"event_type"`
	EventVersion   string `json:"event_version"`
	SourceService  string `json:"source_service"`
	JurisdictionID string `json:"jurisdiction_id"`
	ActorID        string `json:"actor_id"`
	CorrelationID  string `json:"correlation_id"`
}

func decode(t *testing.T, msg []byte) envelope {
	t.Helper()
	var env envelope
	require.NoError(t, json.Unmarshal(msg, &env))
	return env
}

func TestEventTypeConstants(t *testing.T) {
	assert.Equal(t, "jurisdiction.created", events.EventJurisdictionCreated)
	assert.Equal(t, "jurisdiction.deactivated", events.EventJurisdictionDeactivated)
	assert.Equal(t, "jurisdiction.rule.updated", events.EventRuleUpdated)
	assert.Equal(t, "jurisdiction.rule.activated", events.EventRuleActivated)
	assert.Equal(t, "legal.drift.detected", events.EventLegalDriftDetected)
}