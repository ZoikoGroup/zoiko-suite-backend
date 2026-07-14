// Package events publishes evidence.manifest.generated once a manifest is
// successfully assembled (docs/architecture/03-microservices.md §14.4
// "Published Events").
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/evidence-manifest-svc/internal/domain"
)

type Publisher struct {
	writer *kafka.Writer
	log    *zap.Logger
}

func NewPublisher(writer *kafka.Writer, log *zap.Logger) *Publisher {
	return &Publisher{writer: writer, log: log}
}

type manifestGeneratedEvent struct {
	EventType      string    `json:"event_type"`
	ManifestID     string    `json:"manifest_id"`
	TenantID       string    `json:"tenant_id"`
	LegalEntityID  string    `json:"legal_entity_id"`
	ScenarioType   string    `json:"scenario_type"`
	ChecksumSHA256 string    `json:"checksum_sha256"`
	GeneratedAt    time.Time `json:"generated_at"`
}

// PublishManifestGenerated is fire-and-forget from the handler's perspective
// (a Kafka outage must not fail manifest generation, which already succeeded
// and was durably recorded in Postgres) — but the error is always returned to
// the caller to log loudly, per this platform's "never silently swallow a
// publish failure" doctrine.
func (p *Publisher) PublishManifestGenerated(ctx context.Context, m *domain.EvidenceManifest) error {
	checksum := ""
	if m.ChecksumSHA256 != nil {
		checksum = *m.ChecksumSHA256
	}
	generatedAt := time.Now().UTC()
	if m.GeneratedAt != nil {
		generatedAt = *m.GeneratedAt
	}

	evt := manifestGeneratedEvent{
		EventType:      "evidence.manifest.generated",
		ManifestID:     m.ManifestID,
		TenantID:       m.TenantID,
		LegalEntityID:  m.LegalEntityID,
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
