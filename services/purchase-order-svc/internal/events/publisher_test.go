package events_test

import (
	"context"
	"errors"
	"testing"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/purchase-order-svc/internal/events"
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

func TestPublishOutbox_DeliversEnvelopeUnchangedWithStableEventID(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "topic", w)

	payload := []byte(`{"event_type":"PurchaseOrderIssued"}`)
	if err := p.PublishOutbox(context.Background(), "ob-1", "po-1", payload); err != nil {
		t.Fatalf("expected delivery, got %v", err)
	}
	if len(w.msgs) != 1 || string(w.msgs[0].Key) != "po-1" || string(w.msgs[0].Value) != string(payload) {
		t.Fatalf("unexpected message %+v", w.msgs)
	}
	if h := w.msgs[0].Headers; len(h) != 1 || h[0].Key != "X-Event-ID" || string(h[0].Value) != "ob-1" {
		t.Fatalf("expected the outbox id as X-Event-ID, got %+v", h)
	}
}

// A failed write must be RETURNED so the relay retries it; the old publisher
// logged and dropped it.
func TestPublishOutbox_ReturnsDeliveryFailure(t *testing.T) {
	p := events.NewPublisherWithWriter(zap.NewNop(), "topic", &fakeWriter{err: errors.New("broker down")})
	if err := p.PublishOutbox(context.Background(), "ob-2", "po-2", []byte(`{}`)); err == nil {
		t.Fatal("expected the delivery failure to be returned")
	}
}
