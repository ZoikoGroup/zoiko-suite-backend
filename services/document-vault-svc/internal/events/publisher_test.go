package events

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// recordingWriter captures the kafka.Message(s) PublishOutbox builds,
// without touching the network. This is the test that would NOT have
// caught the real defect (see TestPublishOutbox_RealWriter_DoesNotSetTopicOnMessage
// below for the one that does) — it exists to pin the message shape (key,
// value, headers) independent of kafka-go's own Writer validation.
type recordingWriter struct {
	got []kafka.Message
	err error
}

func (w *recordingWriter) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	w.got = append(w.got, msgs...)
	return w.err
}

func TestPublishOutbox_SendsKeyValueAndEventIDHeader(t *testing.T) {
	w := &recordingWriter{}
	p := NewPublisher(zap.NewNop(), "zoiko.document-vault.events", w)

	err := p.PublishOutbox(context.Background(), "evt-1", "agg-1", []byte(`{"type":"document.uploaded"}`))
	if err != nil {
		t.Fatalf("PublishOutbox: %v", err)
	}
	if len(w.got) != 1 {
		t.Fatalf("got %d messages, want 1", len(w.got))
	}
	msg := w.got[0]
	if string(msg.Key) != "agg-1" {
		t.Errorf("Key = %q, want %q", msg.Key, "agg-1")
	}
	if string(msg.Value) != `{"type":"document.uploaded"}` {
		t.Errorf("Value = %q", msg.Value)
	}
	var gotEventID string
	for _, h := range msg.Headers {
		if h.Key == "X-Event-ID" {
			gotEventID = string(h.Value)
		}
	}
	if gotEventID != "evt-1" {
		t.Errorf("X-Event-ID header = %q, want %q", gotEventID, "evt-1")
	}
}

// TestPublishOutbox_RealWriter_DoesNotSetTopicOnMessage is the regression
// test for the actual defect found live: every outbox publish failed with
// "kafka.(*Writer): Topic must not be specified for both Writer and
// Message", because PublishOutbox set msg.Topic to the SAME topic the
// *kafka.Writer in cmd/server/main.go already carries on w.Topic — a
// combination kafka-go's real Writer.WriteMessages unconditionally
// rejects, every single call, regardless of whether a broker is even
// reachable (the check runs before any dial). recordingWriter above never
// exercised this, because it is a hand-written stub that just appends
// to a slice — it has no opinion about Topic at all, which is exactly
// how this got past every existing test.
//
// This test uses a REAL *kafka.Writer (the same type cmd/server
// constructs) with Topic set, same as production, and a deliberately
// invalid address so it never actually attempts network I/O: kafka-go
// validates the Writer/Message topic conflict synchronously, before any
// connection is opened, so this is fast and fully offline.
func TestPublishOutbox_RealWriter_DoesNotSetTopicOnMessage(t *testing.T) {
	w := &kafka.Writer{
		Addr:  kafka.TCP("127.0.0.1:0"), // port 0: never a live listener, dial is never reached anyway
		Topic: "zoiko.document-vault.events",
	}
	defer func() { _ = w.Close() }()

	p := NewPublisher(zap.NewNop(), "zoiko.document-vault.events", w)
	err := p.PublishOutbox(context.Background(), "evt-1", "agg-1", []byte(`{}`))

	if err != nil && strings.Contains(err.Error(), "must not be specified for both Writer and Message") {
		t.Fatalf("PublishOutbox set Topic on the message even though the Writer already has one — "+
			"this is the exact defect that made every outbox publish fail: %v", err)
	}
	// Any other error (e.g. a dial/connection failure, since port 0 is not
	// listening) is fine and expected here — this test only pins the one
	// specific failure mode that silently broke publishing entirely.
}

// TestPublishOutbox_AgainstRealBroker is a live end-to-end proof, skipped
// unless a real Kafka broker is reachable (same opt-in pattern as other
// services' TEST_DATABASE_URL-gated suites). KAFKA_TEST_BROKER should be
// the host-reachable listener — this platform's dev stack advertises
// localhost:9092 for exactly this purpose (see docker-compose.yml's
// KAFKA_ADVERTISED_LISTENERS).
func TestPublishOutbox_AgainstRealBroker(t *testing.T) {
	broker := os.Getenv("KAFKA_TEST_BROKER")
	if broker == "" {
		t.Skip("Skipping live Kafka test: KAFKA_TEST_BROKER not set")
	}
	conn, dialErr := net.DialTimeout("tcp", broker, 2*time.Second)
	if dialErr != nil {
		t.Skipf("Skipping live Kafka test: %s unreachable: %v", broker, dialErr)
	}
	_ = conn.Close()

	topic := "zoiko.document-vault-svc.publisher-test"
	w := &kafka.Writer{
		Addr:                   kafka.TCP(broker),
		Topic:                  topic,
		AllowAutoTopicCreation: true,
		BatchTimeout:           10 * time.Millisecond,
	}
	defer func() { _ = w.Close() }()

	p := NewPublisher(zap.NewNop(), topic, w)

	// A brand-new topic's auto-creation (KAFKA_AUTO_CREATE_TOPICS_ENABLE on
	// the broker) can race the first produce attempt that triggers it — the
	// client's metadata fetch asks the broker to create the topic, but the
	// produce in that SAME call can still land before creation has
	// propagated, answering "Unknown Topic Or Partition". That race is a
	// property of this test using a fresh topic name, not of PublishOutbox
	// itself, so it is retried briefly rather than treated as a failure.
	var lastErr error
	for attempt := 0; attempt < 10; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		lastErr = p.PublishOutbox(ctx, "evt-live-1", "agg-live-1", []byte(`{"type":"document.uploaded"}`))
		cancel()
		if lastErr == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	if errors.Is(lastErr, context.DeadlineExceeded) {
		t.Fatalf("publish timed out — broker reachable but write never completed: %v", lastErr)
	}
	t.Fatalf("PublishOutbox against a real broker failed after retries: %v", lastErr)
}
