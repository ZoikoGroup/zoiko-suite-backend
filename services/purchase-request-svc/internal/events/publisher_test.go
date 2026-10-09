package events_test

import (
	"context"
	"errors"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/purchase-request-svc/internal/events"
)

type fakeWriter struct {
	msgs []kafka.Message
	err  error
}

func (f *fakeWriter) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	if f.err != nil {
		return f.err
	}
	f.msgs = append(f.msgs, msgs...)
	return nil
}

func TestPublishOutbox_KeysByAggregateAndCarriesStableEventID(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.purchase-request.events", w)

	require.NoError(t, p.PublishOutbox(context.Background(), "ob-1", "req-1", []byte(`{"event_type":"PurchaseRequisitionApproved"}`)))

	require.Len(t, w.msgs, 1)
	assert.Equal(t, "req-1", string(w.msgs[0].Key))
	assert.JSONEq(t, `{"event_type":"PurchaseRequisitionApproved"}`, string(w.msgs[0].Value))
	require.Len(t, w.msgs[0].Headers, 1)
	assert.Equal(t, "X-Event-ID", w.msgs[0].Headers[0].Key)
	assert.Equal(t, "ob-1", string(w.msgs[0].Headers[0].Value))
}

// A broker failure must reach the relay (which keeps the row and retries); it
// is never swallowed the way the old post-commit publish did.
func TestPublishOutbox_ReturnsBrokerFailure(t *testing.T) {
	w := &fakeWriter{err: errors.New("broker down")}
	p := events.NewPublisherWithWriter(zap.NewNop(), "t", w)
	assert.Error(t, p.PublishOutbox(context.Background(), "ob-1", "req-1", []byte(`{}`)))
}
