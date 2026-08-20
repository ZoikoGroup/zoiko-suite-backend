// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires that this service has real data for
// (event_version, tenant_id, actor_id, correlation_id). legal_entity_id
// and jurisdiction are correctly omitted: neither domain.RoleDefinition
// nor domain.PermissionBundleDef is scoped to one legal entity — a role
// definition is tenant-wide config.
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

	"zoiko.io/access-control-svc/internal/domain"
	"zoiko.io/access-control-svc/internal/events"
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
	ActorID       string `json:"actor_id"`
	CorrelationID string `json:"correlation_id"`
}

func decode(t *testing.T, msg kafka.Message) envelope {
	t.Helper()
	var env envelope
	require.NoError(t, json.Unmarshal(msg.Value, &env))
	return env
}

func TestPublishRoleUpdated_EnvelopeCarriesActor(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.access-control.events", w)

	p.PublishRoleUpdated(context.Background(), domain.RoleDefinition{
		RoleDefinitionID: "role-1", TenantID: "tenant-1", CorrelationID: "corr-1",
		Status: domain.RoleStatusActive, UpdatedAt: time.Now(),
	}, "updater-1")

	require.Len(t, w.msgs, 1)
	env := decode(t, w.msgs[0])
	assert.Equal(t, "role.updated", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "access-control-svc", env.SourceService)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "updater-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublishRoleCreated_RepeatEventsOnSameRole_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.access-control.events", w)

	for i := 0; i < 2; i++ {
		p.PublishRoleCreated(context.Background(), domain.RoleDefinition{
			RoleDefinitionID: "role-1", TenantID: "tenant-1", CorrelationID: "corr-x",
		}, "creator-1")
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}

func TestNewPublisher_NilProducer_DoesNotPanic(t *testing.T) {
	p := events.NewPublisher(zap.NewNop(), "zoiko.access-control.events", nil)
	p.PublishBundleUpdated(context.Background(), domain.PermissionBundleDef{BundleID: "bundle-1"}, "actor-1")
}
