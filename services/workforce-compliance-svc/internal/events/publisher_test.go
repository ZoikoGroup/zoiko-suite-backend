// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires. domain.WorkAuthorization, domain.VisaRecord,
// and domain.WorkingHourLog all carry real TenantID/LegalEntityID and their
// own CorrelationID; actor_id comes from the handler's already
// authz-checked principalID. domain.ComplianceAlert carries no
// correlation_id field of its own, so PublishComplianceAlertRaised takes
// one explicitly. No jurisdiction field exists anywhere in this service.
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/workforce-compliance-svc/internal/domain"
	"zoiko.io/workforce-compliance-svc/internal/events"
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

func TestPublishWorkAuthVerified_EnvelopeCarriesLegalEntityAndActor(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, zap.NewNop())

	p.PublishWorkAuthVerified(context.Background(), "verifier-1", domain.WorkAuthorization{
		AuthID: "auth-1", TenantID: "tenant-1", LegalEntityID: "entity-1", CorrelationID: "corr-1",
	})

	require.Len(t, w.msgs, 1)
	env := decode(t, w.msgs[0])
	assert.Equal(t, "work_authorization.verified", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "workforce-compliance-svc", env.SourceService)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "verifier-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublishComplianceAlertRaised_EnvelopeCarriesExplicitCorrelationID(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, zap.NewNop())

	p.PublishComplianceAlertRaised(context.Background(), "resolver-1", "corr-alert-1", domain.ComplianceAlert{
		AlertID: "alert-1", TenantID: "tenant-1", LegalEntityID: "entity-1",
	})

	require.Len(t, w.msgs, 1)
	env := decode(t, w.msgs[0])
	assert.Equal(t, "compliance.alert_raised", env.EventType)
	assert.Equal(t, "resolver-1", env.ActorID)
	assert.Equal(t, "corr-alert-1", env.CorrelationID)
}

func TestPublishVisaExpirationFlagged_RepeatEventsOnSameVisa_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewKafkaPublisherWithWriter(w, zap.NewNop())

	for i := 0; i < 2; i++ {
		p.PublishVisaExpirationFlagged(context.Background(), "flagger-1", domain.VisaRecord{
			VisaID: "visa-1", TenantID: "tenant-1", LegalEntityID: "entity-1",
		})
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}

func TestNewKafkaPublisher_NoBrokers_DoesNotPanic(t *testing.T) {
	p := events.NewKafkaPublisher(nil, "zoiko.workforce-compliance.events", zap.NewNop())
	p.PublishWorkingHoursBreach(context.Background(), "logger-1", domain.WorkingHourLog{LogID: "log-1"})
}
