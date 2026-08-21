// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires. domain.Position and domain.OrgAssignment
// both carry real TenantID/LegalEntityID but no actor field of their
// own; actor_id is threaded through explicitly from the handler's
// already-verified principalID. jurisdiction stays omitted: no source
// exists anywhere in this service.
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/org-structure-svc/internal/domain"
	"zoiko.io/org-structure-svc/internal/events"
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

func TestPublishPositionCreated_EnvelopeCarriesLegalEntityAndActor(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.org-structure.events", w)

	p.PublishPositionCreated(context.Background(), "corr-1", "creator-1", domain.Position{
		PositionID: "pos-1", TenantID: "tenant-1", LegalEntityID: "entity-1",
	})

	require.Len(t, w.msgs, 1)
	env := decode(t, w.msgs[0])
	assert.Equal(t, "position.created", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "org-structure-svc", env.SourceService)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "creator-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublishOrgStructureChanged_EnvelopeCarriesTenantLegalEntityActor(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.org-structure.events", w)

	p.PublishOrgStructureChanged(context.Background(), "corr-1", "tenant-1", "entity-1", "creator-1", "DEPARTMENT_CREATED", "dept-1")

	require.Len(t, w.msgs, 1)
	env := decode(t, w.msgs[0])
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "creator-1", env.ActorID)
}

func TestPublishEmployeeAssigned_RepeatEventsOnSameAssignment_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.org-structure.events", w)

	for i := 0; i < 2; i++ {
		p.PublishEmployeeAssigned(context.Background(), "corr-x", "assigner-1", domain.OrgAssignment{
			AssignmentID: "assign-1", TenantID: "tenant-1",
		})
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}

func TestNewPublisher_NilProducer_DoesNotPanic(t *testing.T) {
	p := events.NewPublisher(zap.NewNop(), "zoiko.org-structure.events", nil)
	p.PublishPositionCreated(context.Background(), "corr-1", "creator-1", domain.Position{PositionID: "pos-1"})
}
