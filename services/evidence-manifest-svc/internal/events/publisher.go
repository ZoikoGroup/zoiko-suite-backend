// Package events publishes evidence.manifest.generated once a manifest is
// successfully assembled (docs/architecture/03-microservices.md §14.4
// "Published Events").
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/evidence-manifest-svc/internal/domain"
)

// MessageWriter is the one method Publisher needs from *kafka.Writer.
// Narrowed to an interface purely so publisher_test.go can assert
// envelope content without a live broker.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

type Publisher struct {
	writer MessageWriter
	topic  string
	log    *zap.Logger
}

func NewPublisher(writer MessageWriter, log *zap.Logger, topic ...string) *Publisher {
	t := "zoiko.evidence.events"
	if len(topic) > 0 && topic[0] != "" {
		t = topic[0]
	}
	return &Publisher{writer: writer, topic: t, log: log}
}

// NewPublisherWithWriter is NewPublisher but with a caller-supplied
// MessageWriter — used by tests to substitute a fake.
func NewPublisherWithWriter(writer MessageWriter, log *zap.Logger, topic ...string) *Publisher {
	return NewPublisher(writer, log, topic...)
}

// ManifestGeneratedEvent is this platform's event contract (Doc 03 §19):
// every published event must carry event name, event version, timestamp,
// tenant ID, legal entity ID, jurisdiction context, actor ID, correlation
// ID, source service, and payload schema version. domain.EvidenceManifest
// carries real TenantID/LegalEntityID and RequestedBy as its actor; it has
// no jurisdiction or correlation_id field, so correlation_id is threaded
// through explicitly from the request's X-Correlation-ID header.
type ManifestGeneratedEvent struct {
	EventID       string `json:"event_id"`
	EventType     string `json:"event_type"`
	EventVersion  string `json:"event_version"`
	SourceService string `json:"source_service"`
	ManifestID    string `json:"manifest_id"`
	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActorID       string `json:"actor_id,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`

	ScenarioType   string    `json:"scenario_type"`
	ChecksumSHA256 string    `json:"checksum_sha256"`
	GeneratedAt    time.Time `json:"generated_at"`
}

// PublishOutbox sends a pre-serialized outbox event payload to Kafka with
// the aggregateID as the message key and X-Event-ID as a header. Kafka
// errors are returned so the outbox relay can track them for retry.
func (p *Publisher) PublishOutbox(ctx context.Context, outboxEventID, aggregateID string, payload []byte) error {
	// Topic is deliberately NOT set on the message: p.writer (a *kafka.Writer
	// in production, constructed in cmd/server with its own Topic field set)
	// already pins the topic at the writer level, and kafka-go's
	// Writer.WriteMessages refuses a message that ALSO carries a Topic —
	// "Topic must not be specified for both Writer and Message", unconditionally,
	// on every call, regardless of whether a broker is even reachable. Same
	// defect found and fixed in document-vault-svc's internal/events/publisher.go.
	msg := kafka.Message{
		Key:   []byte(aggregateID),
		Value: payload,
		Headers: []kafka.Header{
			{Key: "X-Event-ID", Value: []byte(outboxEventID)},
		},
	}
	if err := p.writer.WriteMessages(ctx, msg); err != nil {
		p.log.Error("failed to publish outbox event to kafka",
			zap.String("outbox_event_id", outboxEventID),
			zap.String("aggregate_id", aggregateID),
			zap.String("topic", p.topic),
			zap.Error(err),
		)
		return err
	}
	return nil
}

// PublishManifestGenerated emits the manifest generated event directly.
func (p *Publisher) PublishManifestGenerated(ctx context.Context, m *domain.EvidenceManifest, correlationID string) error {
	checksum := ""
	if m.ChecksumSHA256 != nil {
		checksum = *m.ChecksumSHA256
	}
	generatedAt := time.Now().UTC()
	if m.GeneratedAt != nil {
		generatedAt = *m.GeneratedAt
	}

	evt := ManifestGeneratedEvent{
		EventID:        "evt-" + uuid.New().String(),
		EventType:      "evidence.manifest.generated",
		EventVersion:   "1.0",
		SourceService:  "evidence-manifest-svc",
		ManifestID:     m.ManifestID,
		TenantID:       m.TenantID,
		LegalEntityID:  m.LegalEntityID,
		ActorID:        m.RequestedBy,
		CorrelationID:  correlationID,
		ScenarioType:   string(m.ScenarioType),
		ChecksumSHA256: checksum,
		GeneratedAt:    generatedAt,
	}
	data, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshal evidence.manifest.generated: %w", err)
	}

	// Same reason Topic is omitted in PublishOutbox above — p.writer already
	// pins it.
	msg := kafka.Message{
		Key:   []byte(m.ManifestID),
		Value: data,
		Headers: []kafka.Header{
			{Key: "X-Event-ID", Value: []byte(evt.EventID)},
		},
	}
	if err := p.writer.WriteMessages(ctx, msg); err != nil {
		return fmt.Errorf("evidence.manifest.generated: kafka write: %w", err)
	}
	return nil
}

// LogOnlyPublisher satisfies outbox.Publisher without a broker. Selected
// only when KAFKA_BROKERS is explicitly empty (local/dev), same posture as
// document-vault-svc and accounts-payable-svc.
type LogOnlyPublisher struct {
	log *zap.Logger
}

func NewLogOnlyPublisher(log *zap.Logger) *LogOnlyPublisher {
	log.Warn("no Kafka brokers configured — outbox events will be logged, not published")
	return &LogOnlyPublisher{log: log}
}

func (p *LogOnlyPublisher) PublishOutbox(_ context.Context, outboxEventID, aggregateID string, _ []byte) error {
	p.log.Info("outbox event not published (no broker configured)",
		zap.String("outbox_event_id", outboxEventID),
		zap.String("aggregate_id", aggregateID),
	)
	return nil
}

func (p *LogOnlyPublisher) PublishManifestGenerated(_ context.Context, m *domain.EvidenceManifest, _ string) error {
	p.log.Info("manifest event not published (no broker configured)",
		zap.String("manifest_id", m.ManifestID),
	)
	return nil
}
