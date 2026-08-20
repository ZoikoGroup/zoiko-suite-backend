// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires. domain.EvidenceManifest carries real
// TenantID/LegalEntityID and RequestedBy as its actor; it has no
// jurisdiction field, correctly omitted.
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/evidence-manifest-svc/internal/domain"
	"zoiko.io/evidence-manifest-svc/internal/events"
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
	SourceService string `json:"source_service"`
	TenantID      string `json:"tenant_id"`
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

func TestPublishManifestGenerated_EnvelopeCarriesLegalEntityAndActor(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(w, zap.NewNop())

	err := p.PublishManifestGenerated(context.Background(), &domain.EvidenceManifest{
		ManifestID: "manifest-1", TenantID: "tenant-1", LegalEntityID: "entity-1",
		RequestedBy: "requester-1",
	}, "corr-1")
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Equal(t, "evidence.manifest.generated", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "evidence-manifest-svc", env.SourceService)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "requester-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublishManifestGenerated_RepeatEventsOnSameManifest_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(w, zap.NewNop())

	for i := 0; i < 2; i++ {
		err := p.PublishManifestGenerated(context.Background(), &domain.EvidenceManifest{
			ManifestID: "manifest-1", TenantID: "tenant-1",
		}, "corr-x")
		require.NoError(t, err)
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}
