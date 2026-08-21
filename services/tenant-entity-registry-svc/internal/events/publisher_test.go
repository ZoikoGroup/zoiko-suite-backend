// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires. domain.Tenant is a top-level object with
// no legal_entity_id of its own — correctly omitted. Every other domain
// struct already carries real Created/UpdatedByPrincipalID actor
// fields, now surfaced at the envelope level. LegalEntity's
// PrimaryJurisdictionID and EntityJurisdictionAssignment's
// JurisdictionID are genuine jurisdiction context.
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/events"
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

func TestPublishEntityCreated_EnvelopeCarriesJurisdictionAndActor(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.entity.events", w)

	p.PublishEntityCreated(context.Background(), &domain.LegalEntity{
		LegalEntityID: "entity-1", TenantID: "tenant-1", PrimaryJurisdictionID: "uk-england",
		CreatedByPrincipalID: "creator-1",
	}, "corr-1")

	require.Len(t, w.msgs, 1)
	env := decode(t, w.msgs[0])
	assert.Equal(t, "entity.created", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "tenant-entity-registry-svc", env.SourceService)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "uk-england", env.Jurisdiction)
	assert.Equal(t, "creator-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublishTenantCreated_NoLegalEntityID_TopLevelObject(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.entity.events", w)

	p.PublishTenantCreated(context.Background(), &domain.Tenant{
		TenantID: "tenant-1", CreatedByPrincipalID: "creator-1",
	}, "corr-1")

	require.Len(t, w.msgs, 1)
	env := decode(t, w.msgs[0])
	assert.Empty(t, env.LegalEntityID)
	assert.Equal(t, "creator-1", env.ActorID)
}

func TestPublishEntityStatusChanged_ActorThreadedExplicitly(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.entity.events", w)

	p.PublishEntityStatusChanged(context.Background(), "tenant-1", "entity-1", "transitioner-1",
		domain.EntityStatus("ACTIVE"), domain.EntityStatus("SUSPENDED"), "corr-1")

	require.Len(t, w.msgs, 1)
	env := decode(t, w.msgs[0])
	assert.Equal(t, "transitioner-1", env.ActorID)
}

func TestPublishEntityUpdated_RepeatEventsOnSameEntity_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.entity.events", w)

	for i := 0; i < 2; i++ {
		p.PublishEntityUpdated(context.Background(), &domain.LegalEntity{LegalEntityID: "entity-1"}, "corr-x")
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}
