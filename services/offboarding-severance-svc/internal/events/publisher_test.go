// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires. Before this fix, every PublishX method
// accepted a principalID parameter but silently discarded it — it never
// reached the envelope as actor_id. domain.TerminationRequest and
// domain.OffboardingChecklist both carry real TenantID/LegalEntityID/
// CorrelationID; neither has a jurisdiction field.
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/offboarding-severance-svc/internal/domain"
	"zoiko.io/offboarding-severance-svc/internal/events"
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

func TestPublishTerminationApproved_EnvelopeCarriesActor(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, zap.NewNop())

	p.PublishTerminationApproved(context.Background(), "approver-1", domain.TerminationRequest{
		TerminationID: "term-1", TenantID: "tenant-1", LegalEntityID: "entity-1", CorrelationID: "corr-1",
	})

	require.Len(t, w.msgs, 1)
	env := decode(t, w.msgs[0])
	assert.Equal(t, "termination.approved", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "offboarding-severance-svc", env.SourceService)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "approver-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublishEmployeeTerminated_RepeatEventsOnSameRequest_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, zap.NewNop())

	for i := 0; i < 2; i++ {
		p.PublishEmployeeTerminated(context.Background(), "processor-1", domain.TerminationRequest{
			TerminationID: "term-1", TenantID: "tenant-1",
		})
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}

func TestNewKafkaPublisher_NoBrokers_DoesNotPanic(t *testing.T) {
	p := events.NewKafkaPublisher(nil, "zoiko.offboarding-severance.events", zap.NewNop())
	p.PublishOffboardingCompleted(context.Background(), "principal-1", domain.OffboardingChecklist{ChecklistID: "chk-1"})
}
