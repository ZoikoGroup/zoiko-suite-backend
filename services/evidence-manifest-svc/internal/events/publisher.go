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
	log    *zap.Logger
}

func NewPublisher(writer *kafka.Writer, log *zap.Logger) *Publisher {
	return &Publisher{writer: writer, log: log}
}

// NewPublisherWithWriter is NewPublisher but with a caller-supplied
// MessageWriter — used by tests to substitute a fake.
func NewPublisherWithWriter(writer MessageWriter, log *zap.Logger) *Publisher {
	return &Publisher{writer: writer, log: log}
}

// manifestGeneratedEvent is this platform's event contract (Doc 03 §19):
// every published event must carry event name, event version, timestamp,
// tenant ID, legal entity ID, jurisdiction context, actor ID, correlation
// ID, source service, and payload schema version. domain.EvidenceManifest
// carries real TenantID/LegalEntityID and RequestedBy as its actor; it has
// no jurisdiction or correlation_id field, so correlation_id is threaded
// through explicitly from the request's X-Correlation-ID header.
type manifestGeneratedEvent struct {
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

// PublishManifestGenerated is fire-and-forget from the handler's perspective
// (a Kafka outage must not fail manifest generation, which already succeeded
// and was durably recorded in Postgres) — but the error is always returned to
// the caller to log loudly, per this platform's "never silently swallow a
// publish failure" doctrine.
func (p *Publisher) PublishManifestGenerated(ctx context.Context, m *domain.EvidenceManifest, correlationID string) error {
	checksum := ""
	if m.ChecksumSHA256 != nil {
		checksum = *m.ChecksumSHA256
	}
	generatedAt := time.Now().UTC()
	if m.GeneratedAt != nil {
		generatedAt = *m.GeneratedAt
	}

	evt := manifestGeneratedEvent{
		// A fresh UUID per publish, not a deterministic string — see
		// docs/architecture/known-gaps.md's event_id collision writeup.
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

	if err := p.writer.WriteMessages(ctx, kafka.Message{Key: []byte(m.ManifestID), Value: data}); err != nil {
		return fmt.Errorf("evidence.manifest.generated: kafka write: %w", err)
	}
	return nil
}
