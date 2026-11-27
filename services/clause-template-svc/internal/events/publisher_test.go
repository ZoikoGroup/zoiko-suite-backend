// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires. Both jurisdiction and legal_entity_id are
// real data here — domain.Clause and domain.ContractTemplate both carry
// them — unlike most services in this platform where jurisdiction is
// correctly left empty for lack of a source.
package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/clause-template-svc/internal/events"
)

type fakeWriter struct {
	msgs []kafka.Message
}

func (f *fakeWriter) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	f.msgs = append(f.msgs, msgs...)
	return nil
}

type envelope struct {
	EventID       string `json:"event_id"`
	EventType     string `json:"event_type"`
	EventVersion  string `json:"event_version"`
	SchemaVersion string `json:"schema_version"`
	SourceService string `json:"source_service"`
	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`
	Jurisdiction  string `json:"jurisdiction"`
	ActorID       string `json:"actor_id"`
	CorrelationID string `json:"correlation_id"`
}

func decode(t *testing.T, msg kafka.Message) envelope {
	t.Helper()
	var env envelope
	require.NoError(t, json.Unmarshal(msg.Value, &env))
	return env
}

func TestPublish_EnvelopeCarriesJurisdictionAndLegalEntity(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, "zoiko.clause-template.events", zap.NewNop())

	err := publish(p, events.PublishParams{
		EventType: "clause.created", EntityID: "clause-1", TenantID: "tenant-1",
		LegalEntityID: "entity-1", Jurisdiction: "US-NY", ActorID: "author-1", CorrelationID: "corr-1",
		Payload: map[string]string{"title": "Indemnification"},
	})
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Equal(t, "clause.created", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "1.0", env.SchemaVersion)
	assert.Equal(t, "clause-template-svc", env.SourceService)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "US-NY", env.Jurisdiction)
	assert.Equal(t, "author-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublish_RepeatEventsOnSameClause_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, "zoiko.clause-template.events", zap.NewNop())

	for i := 0; i < 2; i++ {
		err := publish(p, events.PublishParams{
			EventType: "clause.updated", EntityID: "clause-1", TenantID: "tenant-1",
			ActorID: "editor-1", CorrelationID: "corr-x",
			Payload: map[string]int{"version": i + 2},
		})
		require.NoError(t, err)
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}

// publish builds the envelope exactly as the store does when it writes the
// outbox row, then delivers it the way the outbox relay does.
func publish(p *events.KafkaPublisher, params events.PublishParams) error {
	id := uuid.NewString()
	body, err := json.Marshal(events.NewEvent(id, params))
	if err != nil {
		return err
	}
	return p.PublishOutbox(context.Background(), id, params.EntityID, body)
}

type failingWriter struct{}

func (failingWriter) WriteMessages(context.Context, ...kafka.Message) error {
	return errors.New("broker down")
}

// A broker failure is returned to the relay (which keeps the row and
// retries); the old Publish logged it and returned nil, losing the event.
func TestPublishOutbox_ReturnsBrokerError(t *testing.T) {
	p := events.NewKafkaPublisherWithWriter(failingWriter{}, "t", zap.NewNop())
	require.Error(t, p.PublishOutbox(context.Background(), "id", "k", []byte(`{}`)))
}

func TestPublishOutbox_CarriesStableEventIDHeaderAndKey(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, "t", zap.NewNop())
	require.NoError(t, p.PublishOutbox(context.Background(), "evt-id-1", "entity-9", []byte(`{}`)))
	require.Len(t, w.msgs, 1)
	assert.Equal(t, "entity-9", string(w.msgs[0].Key))
	require.Len(t, w.msgs[0].Headers, 1)
	assert.Equal(t, "X-Event-ID", w.msgs[0].Headers[0].Key)
	assert.Equal(t, "evt-id-1", string(w.msgs[0].Headers[0].Value))
}
