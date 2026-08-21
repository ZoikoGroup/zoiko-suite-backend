// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires that this service has real data for
// (event_version, jurisdiction_id, actor_id, correlation_id). tenant_id
// and legal_entity_id are correctly omitted: domain.Jurisdiction and
// domain.JurisdictionRule are platform-wide reference data with no such
// field.
package events_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/events"
)

type fakeWriter struct {
	msgs []kafka.Message
}

func (f *fakeWriter) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	f.msgs = append(f.msgs, msgs...)
	return nil
}

type envelope struct {
	EventID        string `json:"event_id"`
	EventType      string `json:"event_type"`
	EventVersion   string `json:"event_version"`
	SourceService  string `json:"source_service"`
	JurisdictionID string `json:"jurisdiction_id"`
	ActorID        string `json:"actor_id"`
	CorrelationID  string `json:"correlation_id"`
}

func decode(t *testing.T, msg kafka.Message) envelope {
	t.Helper()
	var env envelope
	require.NoError(t, json.Unmarshal(msg.Value, &env))
	return env
}

func strPtr(s string) *string { return &s }

func TestPublishRuleActivated_ActorIsUpdater_NotOriginalCreator(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.jurisdiction-rules.events", w)

	err := p.PublishRuleActivated(context.Background(), domain.JurisdictionRule{
		JurisdictionRuleID: "rule-1", JurisdictionID: "jur-1",
		CreatedByPrincipalID: "creator-1", UpdatedByPrincipalID: strPtr("activator-1"),
	}, "corr-1")
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Equal(t, "jurisdiction.rule.activated", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "jurisdiction-rules-svc", env.SourceService)
	assert.Equal(t, "jur-1", env.JurisdictionID)
	assert.Equal(t, "activator-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublishJurisdictionCreated_ActorFallsBackToCreator(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.jurisdiction-rules.events", w)

	err := p.PublishJurisdictionCreated(context.Background(), domain.Jurisdiction{
		JurisdictionID: "jur-1", CreatedByPrincipalID: "creator-1", CreatedAt: time.Now(),
	}, "corr-1")
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Equal(t, "creator-1", env.ActorID)
}

func TestPublishLegalDriftDetected_RepeatEventsOnSameRule_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.jurisdiction-rules.events", w)

	for i := 0; i < 2; i++ {
		err := p.PublishLegalDriftDetected(context.Background(),
			domain.JurisdictionRule{JurisdictionRuleID: "rule-1", JurisdictionID: "jur-1"},
			domain.DriftEvent{DriftEventID: "drift-1", RecordedByPrincipalID: "recorder-1", EffectiveAt: time.Now()},
			"corr-x")
		require.NoError(t, err)
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}
