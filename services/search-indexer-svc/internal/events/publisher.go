// Package events publishes this service's own domain events — the eight
// esr.* events of ZS-SVC-AB-001 §11.2.
//
// Events are facts, not commands, and the topic is append-only. Same posture
// as obligations-svc, policy-svc and tenant-entity-registry-svc's producers.
//
// One rule governs every payload here: NOTHING IN AN esr.* EVENT IS A
// TENANT'S CONTENT. These events describe the SEARCH PLANE — which generation
// activated, how far a checkpoint got, that a restriction was proven — and
// their consumers are SRE, audit, DQC and the privacy/records services. A
// query digest may travel; a query may not (INV-17). A source_ref may travel;
// the document behind it may not.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// Topic is this service's own event topic.
//
// A dedicated topic rather than publishing onto each source's topic: these are
// facts about the search plane, and putting them on, say, the obligations
// topic would make every obligations consumer parse events about index
// generations.
const Topic = "zoiko.search.events"

// Event names, §11.2. Wire names, matching the spec exactly — unusually, this
// is a case where the spec's names and the wire names agree, because this
// service is the first producer of them.
const (
	EventGenerationReady     = "esr.index_generation.ready"
	EventGenerationActivated = "esr.index_generation.activated"
	EventCheckpointAdvanced  = "esr.index_checkpoint.advanced"
	EventRestrictionProp     = "esr.restriction.propagated"
	EventRestrictionFailed   = "esr.restriction.failed"
	EventSearchDegraded      = "esr.search.degraded"
	EventSecurityFilterDeny  = "esr.security_filter.denied"
	EventReindexFailed       = "esr.reindex.failed"
)

// envelope is this platform's event contract (Doc 03 §19).
type envelope struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	EventVersion  string          `json:"event_version"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	TenantID      string          `json:"tenant_id,omitempty"`
	LegalEntityID string          `json:"legal_entity_id,omitempty"`
	ActorID       string          `json:"actor_id,omitempty"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

// MessageWriter is the one method Publisher needs from *kafka.Writer,
// narrowed so tests can assert envelope content without a live broker.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

type Publisher struct {
	log      *zap.Logger
	topic    string
	producer MessageWriter
}

func NewPublisher(log *zap.Logger, producer MessageWriter) *Publisher {
	return &Publisher{log: log, topic: Topic, producer: producer}
}

// GenerationReady announces a validated, not-yet-serving generation.
func (p *Publisher) GenerationReady(ctx context.Context, scope, generationID, contractID, partition, digest, correlationID string) error {
	return p.emit(ctx, EventGenerationReady, correlationID, "", "", scope, map[string]any{
		"scope_name":        scope,
		"generation_id":     generationID,
		"contract_id":       contractID,
		"partition":         partition,
		"validation_digest": digest,
	})
}

// GenerationActivated announces a completed alias cutover. Carries both
// generations, because §8.3's alias-drift detection compares "serving
// generation" against "desired signed generation" and needs the pair.
func (p *Publisher) GenerationActivated(ctx context.Context, scope, oldGen, newGen, actorID, correlationID string) error {
	return p.emit(ctx, EventGenerationActivated, correlationID, "", actorID, scope, map[string]any{
		"scope_name":            scope,
		"previous_generation":   oldGen,
		"new_generation":        newGen,
		"activated_by_actor_id": actorID,
		"activation_time":       time.Now().UTC(),
	})
}

// CheckpointAdvanced reports ingestion progress to DQC and SRE.
func (p *Publisher) CheckpointAdvanced(ctx context.Context, scope, partition string, watermark, lagMS int64, generationID, freshness string) error {
	return p.emit(ctx, EventCheckpointAdvanced, "", "", "", scope, map[string]any{
		"scope_name":       scope,
		"source_partition": partition,
		"watermark":        watermark,
		"lag_ms":           lagMS,
		"generation_id":    generationID,
		"freshness":        freshness,
	})
}

// RestrictionPropagated reports a restriction PROVEN invisible.
//
// Emitted at VERIFIED, never at APPLIED. §2.2: "APPLIED is not VERIFIED until
// search visibility is tested", and PRV/DRC consume this event as evidence
// that their erasure or restriction obligation has been met in the search
// plane — so emitting it on a write that has not been checked would be
// telling a privacy service a thing was done when it had only been attempted.
func (p *Publisher) RestrictionPropagated(ctx context.Context, tenantID, scope, sourceType, sourceID, reason, sourceEventID string, verifiedAt time.Time) error {
	return p.emit(ctx, EventRestrictionProp, sourceEventID, tenantID, "", scope, map[string]any{
		"scope_name":      scope,
		"source_type":     sourceType,
		"source_id":       sourceID,
		"reason":          reason,
		"source_event_id": sourceEventID,
		"verified_at":     verifiedAt.UTC(),
	})
}

// RestrictionFailed reports a restriction that could not be propagated or
// verified. §11.2 routes this to WFC incident, SRE and Security, and §8.2 says
// the affected scope may be blocked or degraded rather than continuing known
// over-disclosure — so the payload carries the age and the blast radius those
// decisions need.
func (p *Publisher) RestrictionFailed(ctx context.Context, tenantID, scope, sourceType, sourceID, reason string, ageSeconds float64, blastRadius int) error {
	return p.emit(ctx, EventRestrictionFailed, "", tenantID, "", scope, map[string]any{
		"scope_name":   scope,
		"source_type":  sourceType,
		"source_id":    sourceID,
		"reason":       reason,
		"age_seconds":  ageSeconds,
		"blast_radius": blastRadius,
	})
}

// SearchDegraded announces that a scope answered incompletely.
func (p *Publisher) SearchDegraded(ctx context.Context, tenantID, scope, cause, completeness string, partitions []string, correlationID string) error {
	return p.emit(ctx, EventSearchDegraded, correlationID, tenantID, "", scope, map[string]any{
		"scope_name":         scope,
		"partitions":         partitions,
		"cause":              cause,
		"completeness_state": completeness,
	})
}

// SecurityFilterDenied reports a query the planner refused.
//
// The actor is HASHED, not named. §11.2 specifies "actor/workload hash" for
// this event and not for the others, and the reason is the consumer: this
// stream goes to Security for abuse detection, where the question is "is one
// actor doing this repeatedly" — which a stable hash answers — rather than
// "who is it", which belongs in the tenant-scoped evidence table behind an
// authorization check.
func (p *Publisher) SecurityFilterDenied(ctx context.Context, tenantID, scope, reasonCode, actorHash, correlationID string) error {
	return p.emit(ctx, EventSecurityFilterDeny, correlationID, tenantID, "", scope, map[string]any{
		"scope_name":  scope,
		"reason_code": reasonCode,
		"actor_hash":  actorHash,
	})
}

// ReindexFailed announces a generation that failed to build or validate.
func (p *Publisher) ReindexFailed(ctx context.Context, scope, generationID, stage, reason, correlationID string) error {
	return p.emit(ctx, EventReindexFailed, correlationID, "", "", scope, map[string]any{
		"scope_name":       scope,
		"generation_id":    generationID,
		"validation_stage": stage,
		"reason":           reason,
	})
}

// emit serialises the payload into the canonical envelope and writes it.
//
// The Kafka key is the SCOPE, so every event about one search surface lands on
// one partition and therefore arrives in order. A consumer reading
// generation.ready before generation.activated for the same scope would
// otherwise be a routine occurrence rather than a bug.
func (p *Publisher) emit(ctx context.Context, eventType, correlationID, tenantID, actorID, key string, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("event %q: marshal payload: %w", eventType, err)
	}
	eventID := "evt-" + uuid.New().String()
	if correlationID == "" {
		correlationID = eventID
	}

	env := envelope{
		EventID:       eventID,
		EventType:     eventType,
		EventVersion:  "1.0",
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: "1.0",
		SourceService: "search-indexer-svc",
		TenantID:      tenantID,
		ActorID:       actorID,
		CorrelationID: correlationID,
		Payload:       json.RawMessage(raw),
	}
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("event %q: marshal envelope: %w", eventType, err)
	}

	msg := kafka.Message{
		Key:   []byte(key),
		Value: data,
		// X-Event-ID so a consumer can dedupe across broker redelivery
		// without falling back to topic:partition:offset — the fallback
		// workflow-history-svc's runner documents, which cannot absorb a
		// producer-side retry that lands on a different offset.
		Headers: []kafka.Header{{Key: "X-Event-ID", Value: []byte(eventID)}},
	}
	if err := p.producer.WriteMessages(ctx, msg); err != nil {
		return fmt.Errorf("event %q: kafka write: %w", eventType, err)
	}

	p.log.Info("event published",
		zap.String("event_type", eventType),
		zap.String("topic", p.topic),
		zap.String("correlation_id", correlationID))
	return nil
}

// NopPublisher discards events. Used in tests and in the one legitimate
// runtime case: a deployment with no broker configured, where refusing to
// start would take out search over an observability dependency.
type NopPublisher struct{}

func (NopPublisher) WriteMessages(context.Context, ...kafka.Message) error { return nil }
