package events_test

import (
	"context"
	"errors"
	"testing"

	kafka "github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/payment-authorization-svc/internal/events"
)

type mockMessageWriter struct {
	messages []kafka.Message
	err      error
}

func (m *mockMessageWriter) WriteMessages(ctx context.Context, msgs ...kafka.Message) error {
	m.messages = append(m.messages, msgs...)
	return m.err
}

func TestPublishOutbox_KeyAndHeaders(t *testing.T) {
	writer := &mockMessageWriter{}
	pub := events.NewKafkaPublisherWithWriter(writer, "zoiko.payment-authorization.events", zap.NewNop())

	outboxEventID := "outbox-uuid-123"
	aggregateID := "auth-id-456"
	payload := []byte(`{"event_id":"evt-123","entity_id":"auth-id-456"}`)

	err := pub.PublishOutbox(context.Background(), outboxEventID, aggregateID, payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(writer.messages) != 1 {
		t.Fatalf("expected 1 kafka message, got %d", len(writer.messages))
	}

	msg := writer.messages[0]
	if string(msg.Key) != aggregateID {
		t.Fatalf("expected message Key %s, got %s", aggregateID, string(msg.Key))
	}
	if string(msg.Value) != string(payload) {
		t.Fatalf("expected message Value %s, got %s", string(payload), string(msg.Value))
	}

	var foundXEventID bool
	for _, h := range msg.Headers {
		if h.Key == "X-Event-ID" {
			foundXEventID = true
			if string(h.Value) != outboxEventID {
				t.Fatalf("expected X-Event-ID %s, got %s", outboxEventID, string(h.Value))
			}
		}
	}
	if !foundXEventID {
		t.Fatal("expected X-Event-ID header in Kafka message")
	}
}

func TestPublishOutbox_KafkaErrorPropagated(t *testing.T) {
	expectedErr := errors.New("kafka cluster unreachable")
	writer := &mockMessageWriter{err: expectedErr}
	pub := events.NewKafkaPublisherWithWriter(writer, "zoiko.payment-authorization.events", zap.NewNop())

	err := pub.PublishOutbox(context.Background(), "evt-1", "auth-1", []byte(`{}`))
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected error %v, got %v", expectedErr, err)
	}
}
