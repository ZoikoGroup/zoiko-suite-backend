// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires, and that repeat events for the same contract
// no longer collide on event_id.
//
// Contract execution is Doc 03 §3.7's #5 named mandatory case. Before this
// fix: no actor, no correlation_id, no event_version anywhere on any of the
// five contract events, and — more seriously — event_id was the
// deterministic string "evt-"+eventType+"-"+contractID, which is IDENTICAL
// across every repeat occurrence of the same event type on the same
// contract (e.g. two contract.updated events on the same contract produce
// the same event_id). Any consumer deduplicating via
// INSERT ... ON CONFLICT (event_id) DO NOTHING — the exact pattern this
// platform's own evidence/history consumers use — would silently drop the
// second, real event as if it were a redelivery of the first.
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

	"zoiko.io/contract-lifecycle-svc/internal/events"
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
	CorrelationID string `json:"correlation_id"`
	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActorID       string `json:"actor_id"`
}

func decode(t *testing.T, msg kafka.Message) envelope {
	t.Helper()
	var env envelope
	require.NoError(t, json.Unmarshal(msg.Value, &env))
	return env
}

func TestNewEvent_EnvelopeCarriesActorAndCorrelationID(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, "zoiko.contract.events", zap.NewNop())

	ev := events.NewEvent("evt-1", events.PublishParams{
		EventType: "contract.activated", ContractID: "contract-1", TenantID: "tenant-1",
		LegalEntityID: "entity-1", ActorID: "signer-1", CorrelationID: "corr-1",
		Payload: map[string]string{"status": "ACTIVE"},
	})
	body, err := json.Marshal(ev)
	require.NoError(t, err)
	require.NoError(t, p.PublishOutbox(context.Background(), "evt-1", "contract-1", body))
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Equal(t, "contract.activated", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "1.0", env.SchemaVersion)
	assert.Equal(t, "contract-lifecycle-svc", env.SourceService)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "signer-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.Equal(t, "evt-1", env.EventID)
	assert.Equal(t, "contract-1", string(w.msgs[0].Key), "keyed by contract id, as before")
	require.Len(t, w.msgs[0].Headers, 1)
	assert.Equal(t, "X-Event-ID", w.msgs[0].Headers[0].Key)
	assert.Equal(t, "evt-1", string(w.msgs[0].Headers[0].Value))
}

// A broker failure is returned to the relay (which keeps the row and retries);
// the old Publish logged and returned nil, losing the event.
func TestPublishOutbox_ReturnsBrokerError(t *testing.T) {
	p := events.NewKafkaPublisherWithWriter(failingWriter{}, "zoiko.contract.events", zap.NewNop())
	require.Error(t, p.PublishOutbox(context.Background(), "evt-1", "contract-1", []byte(`{}`)))
}

type failingWriter struct{}

func (failingWriter) WriteMessages(context.Context, ...kafka.Message) error {
	return errors.New("broker down")
}
