// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires. domain.Obligation carries real
// LegalEntityID and JurisdictionID (genuine jurisdiction context,
// surfaced at the envelope level) but no tenant_id field at all —
// correctly omitted, never fabricated. Status-transition events thread
// actor_id through explicitly since the domain object has no
// UpdatedByPrincipalID field.
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/obligations-svc/internal/domain"
	"zoiko.io/obligations-svc/internal/events"
	"zoiko.io/obligations-svc/internal/middleware"
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

func TestPublishObligationClosed_EnvelopeCarriesJurisdictionAndActor(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.obligations.events", w)

	err := p.PublishObligationClosed(context.Background(), domain.Obligation{
		ObligationID: "obl-1", LegalEntityID: "entity-1", JurisdictionID: "uk-england",
	}, "closer-1", "corr-1")
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Equal(t, "obligation.closed", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "obligations-svc", env.SourceService)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "uk-england", env.Jurisdiction)
	assert.Equal(t, "closer-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublishObligationUpdated_RepeatEventsOnSameObligation_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.obligations.events", w)

	for i := 0; i < 2; i++ {
		err := p.PublishObligationUpdated(context.Background(), domain.Obligation{
			ObligationID: "obl-1",
		}, "updater-1", "corr-x")
		require.NoError(t, err)
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}

// Doc 03 §19 lists tenant ID as a mandatory envelope field, and
// search-indexer-svc refuses to project an event without one (INV-02: "every
// projection document carries trusted tenant identity").
//
// The value comes from the REQUEST CONTEXT, which is where the middleware put
// the gateway-set X-Tenant-Id after requireTenant refused everything that
// lacked it — not from the payload, and not from a second lookup.
func TestPublish_EnvelopeCarriesTheVerifiedTenant(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.obligations.events", w)

	const tenant = "11111111-1111-1111-1111-111111111111"
	ctx := middleware.WithTenant(context.Background(), tenant)

	require.NoError(t, p.PublishObligationCreated(ctx, domain.Obligation{
		ObligationID:   "ob-1",
		LegalEntityID:  "22222222-2222-2222-2222-222222222222",
		ObligationCode: "GST-Q4",
	}, "corr-1"))

	require.Len(t, w.msgs, 1)
	var env struct {
		TenantID      string `json:"tenant_id"`
		LegalEntityID string `json:"legal_entity_id"`
	}
	require.NoError(t, json.Unmarshal(w.msgs[0].Value, &env))
	assert.Equal(t, tenant, env.TenantID)
	assert.Equal(t, "22222222-2222-2222-2222-222222222222", env.LegalEntityID)
}

// An unscoped context omits the field rather than emitting an empty string.
//
// omitempty, deliberately: a downstream consumer must be able to tell "no
// tenant" from "the empty tenant", and search-indexer-svc quarantines the
// event either way. Emitting "" would make a missing tenant look like a
// present one to anything doing a bare presence check.
func TestPublish_OmitsTenantWhenTheContextCarriesNone(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.obligations.events", w)

	require.NoError(t, p.PublishObligationCreated(context.Background(),
		domain.Obligation{ObligationID: "ob-1", LegalEntityID: "le-1"}, "corr-1"))

	require.Len(t, w.msgs, 1)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(w.msgs[0].Value, &raw))
	assert.NotContains(t, raw, "tenant_id")
}
