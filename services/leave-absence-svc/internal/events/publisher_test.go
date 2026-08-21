// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires. Neither domain.LeaveRequest nor domain.
// LeaveBalance has a legal_entity_id or jurisdiction field of its own;
// legal_entity_id is threaded through explicitly from the handler (the
// employee's resolved legal entity). actor_id for approve/reject comes
// from the request's own ReviewerID field. jurisdiction stays omitted: no
// source exists anywhere in this service.
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/leave-absence-svc/internal/domain"
	"zoiko.io/leave-absence-svc/internal/events"
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

func reviewerPtr(id string) *string { return &id }

func TestPublishLeaveApproved_ActorIsReviewer(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.leave-absence.events", w)

	p.PublishLeaveApproved(context.Background(), "corr-1", "entity-1", domain.LeaveRequest{
		RequestID: "req-1", TenantID: "tenant-1", ReviewerID: reviewerPtr("reviewer-1"),
	})

	require.Len(t, w.msgs, 1)
	env := decode(t, w.msgs[0])
	assert.Equal(t, "leave.approved", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "leave-absence-svc", env.SourceService)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "reviewer-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublishLeaveRejected_NilReviewerPointer_DoesNotPanic(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.leave-absence.events", w)

	p.PublishLeaveRejected(context.Background(), "corr-1", "entity-1", domain.LeaveRequest{RequestID: "req-1"})

	require.Len(t, w.msgs, 1)
	env := decode(t, w.msgs[0])
	assert.Empty(t, env.ActorID)
}

func TestPublishLeaveRequested_RepeatEventsOnSameRequest_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.leave-absence.events", w)

	for i := 0; i < 2; i++ {
		p.PublishLeaveRequested(context.Background(), "corr-x", "entity-1", "submitter-1", domain.LeaveRequest{
			RequestID: "req-1", TenantID: "tenant-1",
		})
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}

func TestNewPublisher_NilProducer_DoesNotPanic(t *testing.T) {
	p := events.NewPublisher(zap.NewNop(), "zoiko.leave-absence.events", nil)
	p.PublishBalanceUpdated(context.Background(), "corr-1", "entity-1", "principal-1", domain.LeaveBalance{BalanceID: "bal-1"})
}
