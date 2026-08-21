// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires that this service has real data for.
// domain.SecretLease is independently-nullable-scoped (nil TenantID/
// LegalEntityID for a platform-wide secret) — correctly omitted rather
// than fabricated. secret.access.requested fires before any lease
// exists, so it has no tenant/legal-entity scope yet — also correctly
// omitted. secret.rotation.completed sources its actor from the
// caller-supplied RotatedByPrincipalID.
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

	"zoiko.io/secret-vault-integration-svc/internal/domain"
	"zoiko.io/secret-vault-integration-svc/internal/events"
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

func strPtr(s string) *string { return &s }

func TestPublishAccessGranted_EnvelopeCarriesTenantLegalEntityAndActor(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.secret-vault-integration.events", w)

	err := p.PublishAccessGranted(context.Background(), domain.SecretLease{
		LeaseID: "lease-1", TenantID: strPtr("tenant-1"), LegalEntityID: strPtr("entity-1"),
		RequestedByPrincipalID: "requester-1", ExpiresAt: time.Now(),
	}, "corr-1")
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Equal(t, "secret.access.granted", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "secret-vault-integration-svc", env.SourceService)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "requester-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublishAccessRequested_NoTenantOrLegalEntity_NoLeaseYet(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.secret-vault-integration.events", w)

	err := p.PublishAccessRequested(context.Background(), "kv/db/creds", "requester-1", "corr-1")
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Empty(t, env.TenantID)
	assert.Empty(t, env.LegalEntityID)
	assert.Equal(t, "requester-1", env.ActorID)
}

func TestPublishRotationCompleted_ActorIsRotator(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.secret-vault-integration.events", w)

	err := p.PublishRotationCompleted(context.Background(), "policy-1", "kv/db/creds", "rotator-1", 3, "corr-1")
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Equal(t, "rotator-1", env.ActorID)
}

func TestPublishAccessGranted_RepeatEventsOnSameLease_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.secret-vault-integration.events", w)

	for i := 0; i < 2; i++ {
		err := p.PublishAccessGranted(context.Background(), domain.SecretLease{LeaseID: "lease-1"}, "corr-x")
		require.NoError(t, err)
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}
