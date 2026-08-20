// Package events_test asserts Publish actually writes to Kafka
// synchronously and returns the real error (before this fix it fired a
// detached goroutine against context.Background(), never set event_id at
// all, and unconditionally returned nil regardless of outcome), and that
// the envelope carries the fields Doc 03 §19 requires. domain.
// ForecastModel carries a real LegalEntityID but no jurisdiction field.
package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/forecasting-svc/internal/events"
)

type fakeWriter struct {
	msgs []kafka.Message
	err  error
}

func (f *fakeWriter) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	if f.err != nil {
		return f.err
	}
	f.msgs = append(f.msgs, msgs...)
	return nil
}

type envelope struct {
	EventID       string `json:"event_id"`
	EventType     string `json:"event_type"`
	EventVersion  string `json:"event_version"`
	SourceService string `json:"source_service"`
	LegalEntityID string `json:"legal_entity_id"`
	ActorID       string `json:"actor_id"`
	CorrelationID string `json:"correlation_id"`
}

func decode(t *testing.T, msg kafka.Message) envelope {
	t.Helper()
	var env envelope
	require.NoError(t, json.Unmarshal(msg.Value, &env))
	return env
}

func TestPublish_EnvelopeCarriesLegalEntityAndActor(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(w, zap.NewNop())

	err := p.Publish(context.Background(), events.PublishParams{
		EventType: "forecast.generated", SubjectID: "model-1", TenantID: "tenant-1",
		LegalEntityID: "entity-1", ActorID: "creator-1", CorrelationID: "corr-1",
		Payload: map[string]string{"status": "ACTIVE"},
	})
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Equal(t, "forecast.generated", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "forecasting-svc", env.SourceService)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "creator-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublish_RepeatEventsOnSameModel_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(w, zap.NewNop())

	for i := 0; i < 2; i++ {
		err := p.Publish(context.Background(), events.PublishParams{
			EventType: "forecast.archived", SubjectID: "model-1", TenantID: "tenant-1",
			ActorID: "archiver-1", CorrelationID: "corr-x",
			Payload: map[string]int{"attempt": i + 1},
		})
		require.NoError(t, err)
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}

func TestPublish_WriteFailure_ReturnsError(t *testing.T) {
	w := &fakeWriter{err: errors.New("broker unreachable")}
	p := events.NewPublisherWithWriter(w, zap.NewNop())

	err := p.Publish(context.Background(), events.PublishParams{
		EventType: "forecast.generated", SubjectID: "model-1", TenantID: "tenant-1",
	})
	require.Error(t, err)
}
