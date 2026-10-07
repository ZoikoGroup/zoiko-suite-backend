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

// Event type names, in one place because three things agree on them: the
// builders below, the outbox's CHECK constraint (migration 000004) and
// asyncapi.yaml. The first five are the names spec REF-05 lists;
// PeriodCommandRejected is the security/control event for negative path 31.
const (
	EventPeriodOpened          = "PeriodOpened"
	EventPeriodSoftClosed      = "PeriodSoftClosed"
	EventPeriodHardClosed      = "PeriodHardClosed"
	EventPeriodReopened        = "PeriodReopened"
	EventPeriodReclosed        = "PeriodReclosed"
	EventPeriodCommandRejected = "PeriodCommandRejected"

	SourceService              = "accounting-period-svc"
	ScopeTenant                = "TENANT"
	ObjectTypeAccountingPeriod = "accounting_period"
)

// AllEventTypes lists every event type this service emits.
var AllEventTypes = []string{EventPeriodOpened, EventPeriodSoftClosed, EventPeriodHardClosed,
	EventPeriodReopened, EventPeriodReclosed, EventPeriodCommandRejected}

// Event is what a state change reports. It is rendered into the platform event
// envelope by Build, inside the same transaction as the change.
type Event struct {
	Type string

	// TenantID owns the period.
	TenantID string
	Scope    string

	ObjectType    string
	ObjectID      string
	ObjectVersion int64
	EffectiveAt   time.Time
	RecordedAt    time.Time
	Actor         string
	CorrelationID string
	CausationID   string
	// Acc14WorkflowRef is the ACC-14 workflow that authorised the change, when any.
	Acc14WorkflowRef string

	// Data is event-specific, non-secret detail (keys, states, versions).
	Data map[string]any
}

// envelope is this platform's event contract (Doc 03 section 19).
type envelope struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	EventVersion  string          `json:"event_version"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	TenantID      string          `json:"tenant_id"`
	ActorID       string          `json:"actor_id,omitempty"`
	CorrelationID string          `json:"correlation_id,omitempty"`
	CausationID   string          `json:"causation_id,omitempty"`
	Payload       json.RawMessage `json:"payload"`
}

// Build renders one event into the Kafka key and body. It is called at
// STATE-CHANGE time, inside the writing transaction, and the bytes are stored
// in the outbox; the relay never rebuilds them.
func Build(e Event) (key string, body []byte, err error) {
	known := false
	for _, t := range AllEventTypes {
		if t == e.Type {
			known = true
		}
	}
	if !known {
		return "", nil, fmt.Errorf("events: unknown event type %q", e.Type)
	}
	if e.TenantID == "" || e.ObjectID == "" {
		return "", nil, fmt.Errorf("events: %s requires tenant_id and object_id", e.Type)
	}
	payload := map[string]any{
		"scope":          e.Scope,
		"tenant_id":      e.TenantID,
		"object_type":    e.ObjectType,
		"object_id":      e.ObjectID,
		"object_version": e.ObjectVersion,
		"effective_at":   e.EffectiveAt.UTC(),
		"recorded_at":    e.RecordedAt.UTC(),
		"actor":          e.Actor,
		"correlation_id": e.CorrelationID,
	}
	if e.CausationID != "" {
		payload["causation_id"] = e.CausationID
	}
	if e.Acc14WorkflowRef != "" {
		payload["acc14_workflow_ref"] = e.Acc14WorkflowRef
	}
	for k, v := range e.Data {
		if _, taken := payload[k]; !taken {
			payload[k] = v
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", nil, fmt.Errorf("events: marshal payload for %s: %w", e.Type, err)
	}
	body, err = json.Marshal(envelope{
		EventID:       "evt-" + uuid.NewString(),
		EventType:     e.Type,
		EventVersion:  "1.0",
		EmittedAt:     e.RecordedAt.UTC(),
		SchemaVersion: "1.0",
		SourceService: SourceService,
		TenantID:      e.TenantID,
		ActorID:       e.Actor,
		CorrelationID: e.CorrelationID,
		CausationID:   e.CausationID,
		Payload:       raw,
	})
	if err != nil {
		return "", nil, fmt.Errorf("events: marshal envelope for %s: %w", e.Type, err)
	}
	// Keyed by period, so every event about one period lands on one partition
	// and a consumer sees them in order.
	return e.ObjectID, body, nil
}

// MessageWriter is the one method Publisher needs from *kafka.Writer.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

// Publisher writes already-built envelopes to Kafka. It has no knowledge of the
// domain: everything it sends came out of the outbox.
type Publisher struct {
	log      *zap.Logger
	topic    string
	producer MessageWriter
}

func NewPublisher(log *zap.Logger, topic string, producer *kafka.Writer) *Publisher {
	// A nil *kafka.Writer stored in the interface would make the interface
	// non-nil; keep the field genuinely nil (dry-run mode).
	if producer == nil {
		return &Publisher{log: log, topic: topic}
	}
	return &Publisher{log: log, topic: topic, producer: producer}
}

// NewPublisherWithWriter is NewPublisher with a caller-supplied MessageWriter.
func NewPublisherWithWriter(log *zap.Logger, topic string, producer MessageWriter) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

// Publish writes a batch in ONE call (kafka-go does not flush a batch of one
// until BatchTimeout, so a per-record loop would serialise timer waits).
func (p *Publisher) Publish(ctx context.Context, msgs []kafka.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	if p.producer == nil {
		p.log.Info("simulating publish in dry mode", zap.Int("events", len(msgs)), zap.String("topic", p.topic))
		return nil
	}
	if err := p.producer.WriteMessages(ctx, msgs...); err != nil {
		return fmt.Errorf("kafka write %d message(s) to %s: %w", len(msgs), p.topic, err)
	}
	return nil
}
