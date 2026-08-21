// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires. domain.ReviewRecord carries real
// TenantID/LegalEntityID and a CreatedByPrincipalID actor for the
// created event; completed uses the completing handler's principalID
// rather than ReviewerPrincipalID, since whoever marks a review
// complete is not necessarily its reviewer of record.
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/performance-review-svc/internal/domain"
	"zoiko.io/performance-review-svc/internal/events"
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

func TestPublishReviewCreated_ActorIsCreator(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.performance-review.events", w)

	p.PublishReviewCreated(context.Background(), domain.ReviewRecord{
		ReviewID: "review-1", TenantID: "tenant-1", LegalEntityID: "entity-1",
		CreatedByPrincipalID: "creator-1", CorrelationID: "corr-1",
	})

	require.Len(t, w.msgs, 1)
	env := decode(t, w.msgs[0])
	assert.Equal(t, "review.created", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "performance-review-svc", env.SourceService)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "creator-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublishReviewCompleted_ActorIsCompleter_NotReviewerOfRecord(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.performance-review.events", w)

	p.PublishReviewCompleted(context.Background(), "hr-admin-1", domain.ReviewRecord{
		ReviewID: "review-1", ReviewerPrincipalID: "reviewer-1", CreatedByPrincipalID: "creator-1",
	})

	require.Len(t, w.msgs, 1)
	env := decode(t, w.msgs[0])
	assert.Equal(t, "hr-admin-1", env.ActorID)
}

func TestPublishReviewCreated_RepeatEventsOnSameReview_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.performance-review.events", w)

	for i := 0; i < 2; i++ {
		p.PublishReviewCreated(context.Background(), domain.ReviewRecord{
			ReviewID: "review-1", TenantID: "tenant-1", CorrelationID: "corr-x",
		})
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}

func TestNewPublisher_NilProducer_DoesNotPanic(t *testing.T) {
	p := events.NewPublisher(zap.NewNop(), "zoiko.performance-review.events", nil)
	p.PublishReviewCompleted(context.Background(), "principal-1", domain.ReviewRecord{ReviewID: "review-1"})
}
