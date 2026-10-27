// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires. domain.EvidenceManifest carries real
// TenantID/LegalEntityID and RequestedBy as its actor; it has no
// jurisdiction field, correctly omitted.
package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/evidence-manifest-svc/internal/domain"
	"zoiko.io/evidence-manifest-svc/internal/events"
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

func TestPublishManifestGenerated_EnvelopeCarriesLegalEntityAndActor(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(w, zap.NewNop())

	err := p.PublishManifestGenerated(context.Background(), &domain.EvidenceManifest{
		ManifestID: "manifest-1", TenantID: "tenant-1", LegalEntityID: "entity-1",
		RequestedBy: "requester-1",
	}, "corr-1")
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	env := decode(t, w.msgs[0])
	assert.Equal(t, "evidence.manifest.generated", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "evidence-manifest-svc", env.SourceService)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "requester-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
}

func TestPublishManifestGenerated_RepeatEventsOnSameManifest_GetDistinctEventIDs(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(w, zap.NewNop())

	for i := 0; i < 2; i++ {
		err := p.PublishManifestGenerated(context.Background(), &domain.EvidenceManifest{
			ManifestID: "manifest-1", TenantID: "tenant-1",
		}, "corr-x")
		require.NoError(t, err)
	}

	require.Len(t, w.msgs, 2)
	first := decode(t, w.msgs[0])
	second := decode(t, w.msgs[1])
	assert.NotEqual(t, first.EventID, second.EventID)
}

func TestPublishOutbox_HeadersAndPayload(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(w, zap.NewNop(), "test.topic")

	payload := []byte(`{"test":"outbox-data"}`)
	err := p.PublishOutbox(context.Background(), "outbox-evt-123", "manifest-999", payload)
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	msg := w.msgs[0]
	// Topic must NOT be set on the message: the *kafka.Writer constructed in
	// cmd/server/main.go already pins it, and kafka-go's real
	// Writer.WriteMessages unconditionally rejects a message that ALSO
	// carries one ("Topic must not be specified for both Writer and
	// Message") — every call, regardless of whether a broker is reachable.
	// This field used to assert the opposite ("test.topic"), which is
	// exactly how that defect shipped: fakeWriter has no opinion about
	// Topic at all, so this test never exercised kafka-go's real validation.
	// See TestPublishOutbox_RealWriter_DoesNotSetTopicOnMessage below for
	// the test that does.
	assert.Empty(t, msg.Topic)
	assert.Equal(t, []byte("manifest-999"), msg.Key)
	assert.Equal(t, payload, msg.Value)
	require.Len(t, msg.Headers, 1)
	assert.Equal(t, "X-Event-ID", msg.Headers[0].Key)
	assert.Equal(t, []byte("outbox-evt-123"), msg.Headers[0].Value)
}

// TestPublishOutbox_RealWriter_DoesNotSetTopicOnMessage is the regression
// test for the actual defect found live (same defect, same fix, as
// document-vault-svc's internal/events/publisher.go). It uses a REAL
// *kafka.Writer (not fakeWriter) specifically so kafka-go's own
// Writer/Message topic-conflict validation runs. The address is
// deliberately invalid (port 0) so this never attempts real network I/O —
// kafka-go validates the topic synchronously, before any dial.
func TestPublishOutbox_RealWriter_DoesNotSetTopicOnMessage(t *testing.T) {
	w := &kafka.Writer{
		Addr:  kafka.TCP("127.0.0.1:0"),
		Topic: "zoiko.evidence.events",
	}
	defer func() { _ = w.Close() }()

	p := events.NewPublisherWithWriter(w, zap.NewNop(), "zoiko.evidence.events")
	err := p.PublishOutbox(context.Background(), "evt-1", "agg-1", []byte(`{}`))

	if err != nil {
		assert.NotContains(t, err.Error(), "must not be specified for both Writer and Message",
			"PublishOutbox set Topic on the message even though the Writer already has one — "+
				"this is the exact defect that made every outbox publish fail")
	}
}

// TestPublishManifestGenerated_RealWriter_DoesNotSetTopicOnMessage is the
// same regression test for the service's OTHER publish path — the one that
// does not go through the outbox at all (see PublishManifestGenerated's own
// doc comment). It carried the identical defect.
func TestPublishManifestGenerated_RealWriter_DoesNotSetTopicOnMessage(t *testing.T) {
	w := &kafka.Writer{
		Addr:  kafka.TCP("127.0.0.1:0"),
		Topic: "zoiko.evidence.events",
	}
	defer func() { _ = w.Close() }()

	p := events.NewPublisherWithWriter(w, zap.NewNop(), "zoiko.evidence.events")
	err := p.PublishManifestGenerated(context.Background(), &domain.EvidenceManifest{ManifestID: "m1", TenantID: "t1", LegalEntityID: "le1"}, "corr-1")

	if err != nil {
		assert.NotContains(t, err.Error(), "must not be specified for both Writer and Message",
			"PublishManifestGenerated set Topic on the message even though the Writer already has one")
	}
}

func TestLogOnlyPublisher_DoesNotPanic(t *testing.T) {
	p := events.NewLogOnlyPublisher(zap.NewNop())
	err := p.PublishOutbox(context.Background(), "outbox-1", "manifest-1", []byte("{}"))
	assert.NoError(t, err)

	err = p.PublishManifestGenerated(context.Background(), &domain.EvidenceManifest{ManifestID: "m1"}, "c1")
	assert.NoError(t, err)
}

// TestPublishOutbox_AgainstRealBroker is a live end-to-end proof, skipped
// unless a real Kafka broker is reachable (same opt-in pattern used in
// document-vault-svc's own publisher_test.go). This platform's dev stack
// advertises localhost:9092 for exactly this purpose (see docker-compose.yml's
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

	topic := "zoiko.evidence-manifest-svc.publisher-test"
	w := &kafka.Writer{
		Addr:                   kafka.TCP(broker),
		Topic:                  topic,
		AllowAutoTopicCreation: true,
		BatchTimeout:           10 * time.Millisecond,
	}
	defer func() { _ = w.Close() }()

	p := events.NewPublisherWithWriter(w, zap.NewNop(), topic)

	// A brand-new topic's auto-creation can race the first produce attempt
	// that triggers it (observed in document-vault-svc's identical test) —
	// retried briefly rather than treated as a failure.
	var lastErr error
	for attempt := 0; attempt < 10; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		lastErr = p.PublishOutbox(ctx, "evt-live-1", "agg-live-1", []byte(`{"type":"evidence.manifest.generated"}`))
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
