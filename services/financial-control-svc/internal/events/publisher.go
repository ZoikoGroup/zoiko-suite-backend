// Package events publishes financial-control-svc's domain events.
//
// Events are never published inline from a handler. State and event are
// written in ONE transaction (internal/outbox) and relayed afterwards, so a
// control fact is never durable without its event nor announced without being
// durable (ZS-EVENT-001, ZS-CONTROL-001 §26).
package events

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/financial-control-svc/internal/outbox"
)

// Event types — ZS-CONTROL-001 §26.
const (
	RunCreated             = "control.run.created"
	PopulationFrozen       = "control.population.frozen"
	ExecutionCompleted     = "control.execution.completed"
	ExceptionOpened        = "control.exception.opened"
	ExceptionAssigned      = "control.exception.assigned"
	RemediationRecorded    = "control.exception.remediation_recorded"
	RunReperformed         = "control.run.reperformed"
	RunReadyForCert        = "control.run.ready_for_certification"
	RunCertified           = "control.run.certified"
	RunCertificationReject = "control.run.certification_rejected"
	// RunCertificationRevoked: a certified run's certification was revoked
	// (prospectively; ZS-SVC-Z-001 RB-05). Carries the prior certification,
	// reason, evidence and effective time.
	RunCertificationRevoked = "control.run.certification_revoked"
	RunFailed               = "control.run.failed"
	CloseGateBlocked        = "control.close_gate.blocked"
	CloseGateSatisfied      = "control.close_gate.satisfied"
	ExceptionSLABreached    = "control.exception.sla_breached"
	ExceptionWaived         = "control.exception.waived"
	ExceptionCarried        = "control.exception.carried_forward"
	ExceptionReperformed    = "control.exception.reperformed"
	ExceptionClosed         = "control.exception.closed"
	RunSuperseded           = "control.run.superseded"
	RunExpired              = "control.run.expired"
)

// envelope is the platform's canonical event wrapper, byte-compatible with every other
// producer and with the consumers that parse it (audit-event-store-svc requires event_type,
// emitted_at, schema_version, source_service and payload; see
// identity-context-svc/internal/events/publisher.go).
//
// The fields below the divider are ADDITIVE ZS-EVENT-001 attributes (event id, aggregate
// identity/version, payload hash, residency, classification). Existing consumers ignore unknown
// fields, so they cost nothing. The CloudEvents naming ZS-EVENT-001 also prescribes
// (specversion/type/source/time, com.zoikosuite.* types) is NOT applied: no other producer or
// consumer in the repository uses it, and adopting it in one service would make that service
// unreadable to the audit store. It is a platform-wide decision, recorded as blocked.
type envelope struct {
	EventType     string          `json:"event_type"`
	EventVersion  string          `json:"event_version"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	CorrelationID string          `json:"correlation_id"`
	TenantID      string          `json:"tenant_id,omitempty"`
	LegalEntityID string          `json:"legal_entity_id,omitempty"`
	ActorID       string          `json:"actor_id,omitempty"`
	Payload       json.RawMessage `json:"payload"`

	// --- additive ZS-EVENT-001 attributes ---
	EventID          string `json:"event_id"`
	AggregateType    string `json:"aggregate_type"`
	AggregateID      string `json:"aggregate_id"`
	AggregateVersion *int64 `json:"aggregate_version,omitempty"`
	PublishedAt      string `json:"published_at"`
	PayloadHash      string `json:"payload_hash"`
	ResidencyRegion  string `json:"residency_region"`
	Classification   string `json:"classification"`
}

const (
	// Unspecified marks the two attributes whose values are governed decisions this service cannot
	// make itself (residency region, data classification); they are configured per deployment.
	Unspecified = "UNSPECIFIED"
)

// MessageWriter is the one method Publisher needs from *kafka.Writer.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

type Publisher struct {
	log            *zap.Logger
	topic          string
	producer       MessageWriter
	residency      string
	classification string
	brokers        []string
	dialer         *kafka.Dialer
	probeTimeout   time.Duration
}

func NewPublisher(log *zap.Logger, topic string, producer MessageWriter) *Publisher {
	return &Publisher{
		log: log, topic: topic, producer: producer, residency: Unspecified, classification: Unspecified,
		dialer: &kafka.Dialer{Timeout: 10 * time.Second}, probeTimeout: 5 * time.Second,
	}
}

// WithBrokers records the broker addresses Probe dials directly (bypassing
// the batching kafka.Writer, which only reports a write failure well after
// the fact). Required for Probe to distinguish a down broker from a
// malformed event; without it, Probe always reports an error, which is the
// safe default (see Probe).
func (p *Publisher) WithBrokers(brokers []string) *Publisher {
	p.brokers = brokers
	return p
}

// Probe reports whether the topic is currently servable, by dialing a
// broker and reading the topic's partition metadata directly — the same
// check kafkaout.Writer.Probe performs, kept local here rather than taking a
// dependency on the eventing module, since this service's envelope
// deliberately does not adopt that module's CloudEvents shape (see the
// envelope type's doc comment).
//
// An empty broker list (WithBrokers was never called) always fails closed:
// outbox.Relay treats a Probe error as "the broker is down" and releases the
// batch uncharged, so a misconfigured Publisher can only make the relay
// retry forever, never dead-letter a well-formed event it was never able to
// genuinely test.
func (p *Publisher) Probe(ctx context.Context) error {
	if len(p.brokers) == 0 {
		return fmt.Errorf("probe: no brokers configured")
	}
	ctx, cancel := context.WithTimeout(ctx, p.probeTimeout)
	defer cancel()
	var lastErr error
	for _, broker := range p.brokers {
		if lastErr = p.probeBroker(ctx, broker); lastErr == nil {
			return nil
		}
	}
	return lastErr
}

func (p *Publisher) probeBroker(ctx context.Context, broker string) error {
	conn, err := p.dialer.DialContext(ctx, "tcp", broker)
	if err != nil {
		return fmt.Errorf("probe: dial %s: %w", broker, err)
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	partitions, err := conn.ReadPartitions(p.topic)
	if err != nil {
		return fmt.Errorf("probe: topic %s metadata from %s: %w", p.topic, broker, err)
	}
	if len(partitions) == 0 {
		return fmt.Errorf("probe: topic %s has no partitions", p.topic)
	}
	for _, part := range partitions {
		if part.Error != nil {
			return fmt.Errorf("probe: topic %s partition %d: %w", p.topic, part.ID, part.Error)
		}
	}
	return nil
}

// WithProfile sets the deployment's residency region and data classification. Blank values keep
// UNSPECIFIED, which downstream policy can refuse; nothing is guessed.
func (p *Publisher) WithProfile(residencyRegion, classification string) *Publisher {
	if residencyRegion != "" {
		p.residency = residencyRegion
	}
	if classification != "" {
		p.classification = classification
	}
	return p
}

// PartitionKey is the opaque, stable hash of tenant and aggregate ZS-EVENT-001 requires, so one
// aggregate stays ordered without one tenant becoming a hot partition.
func PartitionKey(tenantID, aggregateID string) []byte {
	h := sha256.Sum256([]byte(tenantID + ":" + aggregateID))
	return []byte(hex.EncodeToString(h[:]))
}

// PublishOutbox satisfies outbox.Publisher. The stable outbox id is the event_id (and the
// X-Event-ID header) across every retry so consumers can de-duplicate.
func (p *Publisher) PublishOutbox(ctx context.Context, d outbox.Delivery) error {
	sum := sha256.Sum256(d.Payload)
	env := envelope{
		EventType:       d.EventType,
		EventVersion:    "1.0",
		EmittedAt:       d.OccurredAt.UTC(), // when the fact was committed, not relay time
		SchemaVersion:   "1.0",
		SourceService:   "financial-control-svc",
		CorrelationID:   d.CorrelationID,
		TenantID:        d.TenantID,
		LegalEntityID:   d.LegalEntityID,
		ActorID:         d.ActorID,
		Payload:         json.RawMessage(d.Payload),
		EventID:         d.OutboxEventID,
		AggregateType:   d.AggregateType,
		AggregateID:     d.AggregateID,
		PublishedAt:     time.Now().UTC().Format(time.RFC3339Nano),
		PayloadHash:     "sha256:" + hex.EncodeToString(sum[:]),
		ResidencyRegion: p.residency,
		Classification:  p.classification,
	}
	// Per-aggregate ordering: run events carry the run's optimistic-concurrency version.
	var probe struct {
		Version *int64 `json:"version"`
	}
	if json.Unmarshal(d.Payload, &probe) == nil && probe.Version != nil {
		env.AggregateVersion = probe.Version
	}
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("event %q: marshal envelope: %w", d.EventType, err)
	}
	msg := kafka.Message{
		Key:     PartitionKey(d.TenantID, d.AggregateID),
		Value:   data,
		Headers: []kafka.Header{{Key: "X-Event-ID", Value: []byte(d.OutboxEventID)}},
	}
	if err := p.producer.WriteMessages(ctx, msg); err != nil {
		return fmt.Errorf("event %q: kafka write: %w", d.EventType, err)
	}
	p.log.Info("outbox event published",
		zap.String("event_id", d.OutboxEventID), zap.String("event_type", d.EventType), zap.String("topic", p.topic))
	return nil
}
