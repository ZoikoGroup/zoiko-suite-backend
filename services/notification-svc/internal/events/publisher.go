// Package events builds and publishes this service's domain events.
//
// The package is split in two, and the split is the whole design:
//
//   - Sent and Failed turn a concluded delivery into a sealed envelope. The
//     store calls these INSIDE the transaction that records the conclusion and
//     enqueues the result in event_outbox, so the FACT that a notice concluded
//     is committed atomically with the conclusion itself.
//   - Publish hands already-built envelopes to Kafka. internal/outbox calls it
//     from a background relay, where a failure is a retry rather than a loss.
//
// Before the outbox existed these were one act: a Kafka write from the handler
// and from the retry worker AFTER the commit, with the error logged and
// discarded. The whole of emit's answer to a broker refusal was
//
//	p.log.Error("failed to publish event", ...)
//
// and then it returned. A broker hiccup at the moment a notice concluded
// therefore left the delivery correctly recorded, the caller correctly told
// 201, the register correctly showing SENT — and no consumer anywhere ever
// learning that the notification went out, or that it did not.
//
// On this service that is the worst possible failure shape. It exists to be
// the evidence that a governed notice was issued; an escalation chain waiting
// on notification.failed before paging a human simply never fires, because
// nothing is in an error state, no retry is scheduled, no metric moves, and
// the register shows a healthy row. The one thing that would have reported the
// loss is the event that was lost.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
)

// Contract constants, asserted against asyncapi.yaml by scripts/audit.sh.
const (
	EventVersion  = "1.0"
	SchemaVersion = "1.0"
	SourceService = "notification-svc"

	// TypeSent is emitted when a delivery concludes SENT — meaning a provider
	// accepted the message, never that anyone received or read it
	// (ZS-SVC-Y-001 §0.4). For IN_APP, where the register row IS the delivery,
	// it does mean delivered.
	TypeSent = "notification.sent"

	// TypeFailed is emitted when a delivery concludes FAILED — terminally.
	// NOT when a transient failure is rescheduled: a notification awaiting
	// another attempt has not failed, and publishing a failure a later attempt
	// reverses would have consumers act on an outcome that did not happen.
	TypeFailed = "notification.failed"
)

// envelope is this platform's event contract (Doc 03 §19): every published
// event carries event name, event version, timestamp, tenant ID, legal entity
// ID, jurisdiction context, actor ID, correlation ID, source service, and
// payload schema version.
//
// domain.Notification carries a real TenantID and LegalEntityID, and its actor
// is CreatedByPrincipalID — the principal who initiated the send, not
// RecipientPrincipalID, which is who the notice goes TO and is carried
// separately in the payload. No jurisdiction field exists on the domain object.
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

// Outbound is one sealed envelope on its way to the outbox.
//
// Key becomes the Kafka partition key, and it is the notification id — the
// aggregate — not the correlation id, which is what the pre-outbox publisher
// used. One event exists per notification today, so the two behave alike; the
// aggregate key is what keeps that true if a second ever does.
type Outbound struct {
	EventType string
	Key       string
	Body      []byte
}

// Build marshals one envelope.
//
// Returns an error rather than logging one. The pre-outbox publisher swallowed
// a marshal failure with a log line and returned, which on the conclusion path
// meant the notification concluded and the event vanished — the same loss as a
// broker outage, from a different cause. The caller now enqueues inside the
// delivery transaction, so a failure here refuses the write instead.
func Build(eventType, correlationID, tenantID, legalEntityID, actorID, key string, payload map[string]any) (Outbound, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Outbound{}, fmt.Errorf("marshal %s payload: %w", eventType, err)
	}
	body, err := json.Marshal(envelope{
		// A fresh UUID per publish, not a deterministic string — see
		// docs/architecture/known-gaps.md's event_id collision writeup.
		EventID:       "evt-" + uuid.NewString(),
		EventType:     eventType,
		EventVersion:  EventVersion,
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: SchemaVersion,
		SourceService: SourceService,
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		ActorID:       actorID,
		CorrelationID: correlationID,
		Payload:       raw,
	})
	if err != nil {
		return Outbound{}, fmt.Errorf("marshal %s envelope: %w", eventType, err)
	}
	return Outbound{EventType: eventType, Key: key, Body: body}, nil
}

// Sent seals notification.sent for a delivery that a provider accepted.
//
// The payload deliberately carries no subject, body or recipient address. A
// notification's content is the most sensitive thing this service holds, the
// topic is readable by every consumer on the bus, and a consumer that needs the
// content can read the register under its own authorization. What goes on the
// wire is the fact and its coordinates.
func Sent(correlationID string, n domain.Notification) (Outbound, error) {
	return Build(TypeSent, correlationID, n.TenantID, n.LegalEntityID, n.CreatedByPrincipalID, n.NotificationID, map[string]any{
		"notification_id":        n.NotificationID,
		"tenant_id":              n.TenantID,
		"legal_entity_id":        n.LegalEntityID,
		"recipient_principal_id": n.RecipientPrincipalID,
		"channel":                n.Channel,
		"source_event_type":      n.SourceEventType,
		"source_reference":       n.SourceReference,
		"sent_at":                n.SentAt,
		// Which provider took it and under what identifier. Acceptance
		// evidence, never a delivery receipt — the same weaker claim the
		// column carries.
		"provider_response": n.ProviderResponse,
		// How much work the delivery took. A consumer measuring notification
		// health cannot get this from the fact alone, and one attempt versus
		// four is the difference between a healthy relay and a limping one.
		"delivery_attempts": n.DeliveryAttempts,
	})
}

// Failed seals notification.failed for a delivery that terminally failed.
//
// reason is passed separately rather than read from n.FailureReason because
// the two call sites append to it — the worker adds the exhaustion note — and
// the event must carry what was actually recorded on the row.
func Failed(correlationID string, n domain.Notification, reason string) (Outbound, error) {
	return Build(TypeFailed, correlationID, n.TenantID, n.LegalEntityID, n.CreatedByPrincipalID, n.NotificationID, map[string]any{
		"notification_id":        n.NotificationID,
		"tenant_id":              n.TenantID,
		"legal_entity_id":        n.LegalEntityID,
		"recipient_principal_id": n.RecipientPrincipalID,
		"channel":                n.Channel,
		"source_event_type":      n.SourceEventType,
		"source_reference":       n.SourceReference,
		"failure_reason":         reason,
		"delivery_attempts":      n.DeliveryAttempts,
		// The row's own sent_at, which on a FAILED notification is when the
		// delivery CONCLUDED. time.Now() here would be the enqueue instant
		// instead, which drifts from the record by however long the
		// transaction takes and by however long the relay is behind — so a
		// consumer correlating the event against the register would find two
		// different answers to when the notice failed.
		"failed_at": n.SentAt,
	})
}

// MessageWriter is the one method Publisher needs from *kafka.Writer.
// Narrowed to an interface so the relay's tests can assert envelope content
// without a live broker.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

// Publisher writes already-sealed envelopes to Kafka. It is the relay's half of
// this package and has no idea what a notification is.
type Publisher struct {
	log      *zap.Logger
	topic    string
	producer MessageWriter
}

func NewPublisher(log *zap.Logger, topic string, producer *kafka.Writer) *Publisher {
	// producer may be a nil *kafka.Writer (dry-run mode) — storing that
	// directly into the MessageWriter interface field would make the interface
	// itself non-nil, defeating the p.producer == nil check in Publish. Keep
	// the field genuinely nil in that case.
	if producer == nil {
		return &Publisher{log: log, topic: topic}
	}
	return &Publisher{log: log, topic: topic, producer: producer}
}

// NewPublisherWithWriter is NewPublisher with a caller-supplied MessageWriter —
// used by tests to substitute a fake.
func NewPublisherWithWriter(log *zap.Logger, topic string, producer MessageWriter) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

// Publish writes a whole claimed batch in one call.
//
// One call, not a loop. kafka-go's Writer waits out BatchTimeout per
// WriteMessages invocation, so a per-record loop pays that latency once per
// event and a backlog of 200 drains at 200 × BatchTimeout rather than in one
// round trip.
//
// A nil producer is dry-run: KAFKA_BROKERS empty means "no broker", and the
// relay then marks the batch published rather than growing a backlog nothing
// will ever drain. That is a deployment saying it has no bus, not a bus that is
// down — the difference matters, because the second must retry and the first
// must not.
func (p *Publisher) Publish(ctx context.Context, msgs []kafka.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	if p.producer == nil {
		p.log.Info("simulating publish — no Kafka brokers configured", zap.Int("events", len(msgs)))
		return nil
	}
	// Topic is set on the Writer, not on the Message — kafka-go rejects a
	// Message carrying a Topic when the Writer already has one.
	if err := p.producer.WriteMessages(ctx, msgs...); err != nil {
		return fmt.Errorf("kafka write to %s: %w", p.topic, err)
	}
	p.log.Debug("events published", zap.String("topic", p.topic), zap.Int("events", len(msgs)))
	return nil
}
