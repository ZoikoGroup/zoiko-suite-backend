// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires. domain.EmploymentContract carries real
// TenantID/LegalEntityID; domain.ContractAmendment carries a real
// AmendedBy actor, used for the amended event, while issue/terminate
// thread actor_id explicitly from the handler's already-verified
// principalID. jurisdiction stays omitted: no source exists anywhere in
// this service.
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/employment-contracts-svc/internal/domain"
	"zoiko.io/employment-contracts-svc/internal/events"
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

func TestPublishContractAmended_ActorIsAmender(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.employment-contracts.events", w)

	p.PublishContractAmended(context.Background(),
		"corr-1",
		domain.EmploymentContract{ContractID: "contract-1", TenantID: "tenant-1", LegalEntityID: "entity-1"},
		domain.ContractAmendment{AmendedBy: "amender-1"},
	)

	require.Len(t, w.msgs, 1)
	env := decode(t, w.msgs[0])
	assert.Equal(t, "employment.contract.amended", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "employment-contracts-svc", env.SourceService)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "amender-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublishContractIssued_RepeatEventsOnSameContract_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.employment-contracts.events", w)

	for i := 0; i < 2; i++ {
		p.PublishContractIssued(context.Background(), "corr-x", "issuer-1", domain.EmploymentContract{
			ContractID: "contract-1", TenantID: "tenant-1",
		})
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}

func TestNewPublisher_NilProducer_DoesNotPanic(t *testing.T) {
	p := events.NewPublisher(zap.NewNop(), "zoiko.employment-contracts.events", nil)
	p.PublishContractTerminated(context.Background(), "corr-1", "principal-1", domain.EmploymentContract{ContractID: "contract-1"})
}
