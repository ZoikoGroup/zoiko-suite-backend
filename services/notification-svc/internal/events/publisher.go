// Package events builds and publishes this service's domain events.
//
// The package is split in two, and the split is the whole design:
//
//   - The builders (Sent, Failed, OutcomeUnknown, Template*) turn a state change
//     into a sealed envelope. The store enqueues the result in event_outbox
//     INSIDE the transaction that records the change, so the fact that
//     something happened is committed atomically with the thing itself.
//   - Publish hands already-built envelopes to Kafka. internal/outbox calls it
//     from a background relay, where a failure is a retry rather than a loss.
//
// These used to be one act: a Kafka write from the handler and the retry
// worker AFTER the commit, with the error logged and discarded. A broker hiccup
// at the moment a notice concluded therefore left the delivery recorded, the
// caller told 201, the register showing SENT — and no consumer ever learning
// that the notification went out, or that it did not. An escalation chain
// waiting on notification.failed simply never fired, because nothing was in an
// error state. The one thing that would have reported the loss was the event
// that was lost.
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

// Contract constants. Keep in step with asyncapi.yaml and with the
// event_outbox_event_known CHECK in migration 000010.
const (
	EventVersion  = "1.0"
	SchemaVersion = "1.0"
	SourceService = "notification-svc"

	// TypeSent: a delivery concluded SENT — a provider accepted the message,
	// never that anyone received or read it (ZS-SVC-Y-001 §3.3). For IN_APP,
	// where the register row IS the delivery, it does mean delivered.
	TypeSent = "notification.sent"
	// TypeFailed: a delivery concluded FAILED, terminally. Not emitted when a
	// transient failure is rescheduled — a notification awaiting another
	// attempt has not failed.
	TypeFailed = "notification.failed"
	// TypeOutcomeUnknown: an attempt's outcome is genuinely ambiguous (BIZ-10,
	// §3.4). The notification is PENDING_UNKNOWN until resolved.
	TypeOutcomeUnknown = "notification.outcome_unknown"

	TypeTemplateCreated         = "template.created"
	TypeTemplateVersionApproved = "template.version_approved"
	TypeTemplatePublished       = "template.published"
	TypeTemplateRetired         = "template.retired"

	// ZS-SVC-Y-001 §10.2 canonical attempt events (migration 000014): one per
	// durable attempt row, enqueued in the transaction that writes it.
	TypeAttemptCreated = "delivery.attempt.created"
	TypeAttemptUnknown = "delivery.attempt.unknown"
)

// envelope is this platform's event contract (Doc 03 §19): event name, event
// version, timestamp, tenant ID, legal entity ID, actor ID, correlation ID,
// source service and payload schema version. The actor is the principal who
// caused the change — for a notification, CreatedByPrincipalID, not the
// recipient, who is carried separately in the payload.
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

// Outbound is one sealed envelope on its way to the outbox. Key becomes the
// Kafka partition key: the aggregate the event is about (a notification,
// template or version id), so every event about one aggregate lands on one
// partition, in order.
type Outbound struct {
	EventType string
	Key       string
	Body      []byte
}

// Build marshals one envelope.
//
// Returns an error rather than logging one. The pre-outbox publisher swallowed
// a marshal failure with a log line, which meant the state change committed and
// the event vanished. The caller now enqueues inside the same transaction, so a
// failure here refuses the write instead.
func Build(eventType, correlationID, tenantID, legalEntityID, actorID, key string, payload map[string]any) (Outbound, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Outbound{}, fmt.Errorf("marshal %s payload: %w", eventType, err)
	}
	body, err := json.Marshal(envelope{
		// A fresh UUID per event, not a deterministic string — see
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

// Sent seals notification.sent.
//
// The payload carries no subject, body or recipient address: content is the
// most sensitive thing this service holds and the topic is readable by every
// consumer. A consumer that needs content reads the register under its own
// authorization.
func Sent(correlationID string, n domain.Notification) (Outbound, error) {
	return buildFor(n, TypeSent, correlationID, n.TenantID, n.LegalEntityID, n.CreatedByPrincipalID, n.NotificationID, map[string]any{
		"notification_id":        n.NotificationID,
		"tenant_id":              n.TenantID,
		"legal_entity_id":        n.LegalEntityID,
		"recipient_principal_id": n.RecipientPrincipalID,
		"channel":                n.Channel,
		"source_event_type":      n.SourceEventType,
		"source_reference":       n.SourceReference,
		"sent_at":                n.SentAt,
		// Acceptance evidence, never a delivery receipt — the same weaker
		// claim the column carries.
		"provider_response": n.ProviderResponse,
		// One attempt versus four is the difference between a healthy relay
		// and a limping one.
		"delivery_attempts": n.DeliveryAttempts,
		// Set when this SENT came from resolving an ambiguous attempt, so a
		// consumer can tell a resolution from a first-time conclusion.
		"resolved_at": n.ResolvedAt,
	})
}

// Failed seals notification.failed.
//
// reason is passed separately because call sites append to it (the worker adds
// the exhaustion note) and the event must carry what was recorded on the row.
// failed_at is the row's own sent_at — when the delivery concluded — rather
// than time.Now(), which would drift from the record by however long the
// transaction and the relay take.
func Failed(correlationID string, n domain.Notification, reason string) (Outbound, error) {
	failedAt := n.SentAt
	if failedAt == nil {
		now := time.Now().UTC()
		failedAt = &now
	}
	return buildFor(n, TypeFailed, correlationID, n.TenantID, n.LegalEntityID, n.CreatedByPrincipalID, n.NotificationID, map[string]any{
		"notification_id":        n.NotificationID,
		"tenant_id":              n.TenantID,
		"legal_entity_id":        n.LegalEntityID,
		"recipient_principal_id": n.RecipientPrincipalID,
		"channel":                n.Channel,
		"source_event_type":      n.SourceEventType,
		"source_reference":       n.SourceReference,
		"failure_reason":         reason,
		"delivery_attempts":      n.DeliveryAttempts,
		"failed_at":              failedAt,
		"resolved_at":            n.ResolvedAt,
	})
}

// OutcomeUnknown seals notification.outcome_unknown (BIZ-10).
func OutcomeUnknown(correlationID string, n domain.Notification, reason string) (Outbound, error) {
	return buildFor(n, TypeOutcomeUnknown, correlationID, n.TenantID, n.LegalEntityID, n.CreatedByPrincipalID, n.NotificationID, map[string]any{
		"notification_id":        n.NotificationID,
		"tenant_id":              n.TenantID,
		"legal_entity_id":        n.LegalEntityID,
		"recipient_principal_id": n.RecipientPrincipalID,
		"channel":                n.Channel,
		"reason":                 reason,
		"unknown_at":             n.UnknownAt,
		"delivery_attempts":      n.DeliveryAttempts,
	})
}

// TemplateCreated seals template.created (BIZ-03).
func TemplateCreated(correlationID string, d domain.TemplateDefinition) (Outbound, error) {
	return Build(TypeTemplateCreated, correlationID, d.TenantID, d.LegalEntityID, d.OwnerPrincipalID, d.TemplateID, map[string]any{
		"template_id":        d.TemplateID,
		"tenant_id":          d.TenantID,
		"legal_entity_id":    d.LegalEntityID,
		"name":               d.Name,
		"owner_principal_id": d.OwnerPrincipalID,
	})
}

// TemplateVersionApproved seals template.version_approved (BIZ-03).
func TemplateVersionApproved(correlationID string, v domain.TemplateVersion) (Outbound, error) {
	actor := ""
	if v.ApprovedByPrincipalID != nil {
		actor = *v.ApprovedByPrincipalID
	}
	return Build(TypeTemplateVersionApproved, correlationID, v.TenantID, v.LegalEntityID, actor, v.TemplateID, map[string]any{
		"version_id":               v.VersionID,
		"template_id":              v.TemplateID,
		"tenant_id":                v.TenantID,
		"legal_entity_id":          v.LegalEntityID,
		"locale":                   v.Locale,
		"version_number":           v.VersionNumber,
		"approved_by_principal_id": actor,
	})
}

// TemplatePublished seals template.published (BIZ-03). actor is the principal
// who published, which the version row does not name on its own.
func TemplatePublished(correlationID, actor string, v domain.TemplateVersion) (Outbound, error) {
	return Build(TypeTemplatePublished, correlationID, v.TenantID, v.LegalEntityID, actor, v.TemplateID, map[string]any{
		"version_id":      v.VersionID,
		"template_id":     v.TemplateID,
		"tenant_id":       v.TenantID,
		"legal_entity_id": v.LegalEntityID,
		"locale":          v.Locale,
		"version_number":  v.VersionNumber,
		"content_hash":    v.ContentHash,
	})
}

// TemplateRetired seals template.retired (BIZ-03).
func TemplateRetired(correlationID string, d domain.TemplateDefinition) (Outbound, error) {
	actor := ""
	if d.RetiredByPrincipalID != nil {
		actor = *d.RetiredByPrincipalID
	}
	return Build(TypeTemplateRetired, correlationID, d.TenantID, d.LegalEntityID, actor, d.TemplateID, map[string]any{
		"template_id":             d.TemplateID,
		"tenant_id":               d.TenantID,
		"legal_entity_id":         d.LegalEntityID,
		"retired_by_principal_id": actor,
	})
}

// MessageWriter is the one method Publisher needs from *kafka.Writer, narrowed
// so the relay's tests can assert envelope content without a live broker.
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
	// producer may be a nil *kafka.Writer (dry-run mode). Storing that in the
	// MessageWriter field would make the interface itself non-nil and defeat
	// the dry-run check in Publish, so keep the field genuinely nil.
	if producer == nil {
		return &Publisher{log: log, topic: topic}
	}
	return &Publisher{log: log, topic: topic, producer: producer}
}

// NewPublisherWithWriter is NewPublisher with a caller-supplied MessageWriter.
func NewPublisherWithWriter(log *zap.Logger, topic string, producer MessageWriter) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

// Publish writes a whole claimed batch in one call.
//
// One call, not a loop: kafka-go's Writer waits out BatchTimeout per
// WriteMessages call, so a per-record loop would drain a backlog of 200 at
// 200 × BatchTimeout.
//
// A nil producer is dry-run: KAFKA_BROKERS empty means "no broker", and the
// relay then marks the batch published rather than growing a backlog nothing
// will ever drain. That is a deployment with no bus, not a bus that is down —
// the second must retry and the first must not.
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

// AttemptCreated seals delivery.attempt.created (§10.2) for one durable attempt.
// Keyed on the notification, so every attempt of one communication shares a
// partition with its notification.* events and arrives in order.
func AttemptCreated(correlationID string, n domain.Notification, a domain.DeliveryAttempt) (Outbound, error) {
	p := map[string]any{
		"attempt_id":               a.AttemptID,
		"communication_id":         n.NotificationID,
		"notification_id":          n.NotificationID,
		"attempt_number":           a.AttemptNumber,
		"origin":                   a.Origin,
		"channel":                  a.Channel,
		"provider_binding":         a.ProviderName,
		"outcome":                  a.Outcome,
		"retryable":                a.Retryable,
		"resend_reason":            a.ResendReason,
		"attempted_at":             a.AttemptedAt,
		"payload_hash":             n.RenderedContentHash,
		"recipient_address_source": n.RecipientAddressSource,
	}
	// The privacy decision that governed this attempt, when one did (omitted, never blank).
	if a.PrivacyDecisionID != "" {
		p["privacy_decision_id"] = a.PrivacyDecisionID
	}
	if a.PrivacyResult != "" {
		p["privacy_result"] = a.PrivacyResult
	}
	return buildFor(n, TypeAttemptCreated, correlationID, n.TenantID, n.LegalEntityID, a.ActorPrincipalID, n.NotificationID, p)
}

// AttemptUnknown seals delivery.attempt.unknown (§10.2): an attempt whose
// outcome is ambiguous, with the cause and the deadline by which a person is
// expected to resolve it.
func AttemptUnknown(correlationID string, n domain.Notification, a domain.DeliveryAttempt, resolutionDueAt time.Time) (Outbound, error) {
	return buildFor(n, TypeAttemptUnknown, correlationID, n.TenantID, n.LegalEntityID, a.ActorPrincipalID, n.NotificationID, map[string]any{
		"attempt_id":        a.AttemptID,
		"communication_id":  n.NotificationID,
		"notification_id":   n.NotificationID,
		"ambiguity_cause":   a.FailureReason,
		"resolution_due_at": resolutionDueAt,
		"reason_code":       "NCD-014",
	})
}

// buildFor seals an event about one notification and stamps it with the identity of
// the communication (ZS-SVC-Y-001 INV-02, identity plan step 6).
//
// Every notification.* and delivery.attempt.* event names the communication the same
// way, whichever send path produced it:
//
//	communication_id    the stable id of this logical communication (the register id)
//	notification_id     the same value, kept for consumers written before communication_id
//	message_intent_id   the ledger intent it was produced from, when there is one
//	communication_class what kind of message it is (S0/T0/A1/L1/M1), when stated
//	intent_version_id   the exact communication intent version it was sent under, when it used one
//
// The additions are additive: no existing field changes meaning or moves, so a
// consumer of the earlier shape keeps working. An absent intent or class is omitted,
// never sent as an empty string, so "not linked" is distinguishable from a blank id.
func buildFor(n domain.Notification, eventType, correlationID, tenantID, legalEntityID, actorID, key string, payload map[string]any) (Outbound, error) {
	payload["communication_id"] = n.NotificationID
	if n.MessageIntentID != "" {
		payload["message_intent_id"] = n.MessageIntentID
	}
	if n.CommunicationClass != "" {
		payload["communication_class"] = n.CommunicationClass
	}
	// The exact intent version the message was sent under (INV-04), when it used one.
	if n.IntentVersionID != "" {
		payload["intent_version_id"] = n.IntentVersionID
	}
	return Build(eventType, correlationID, tenantID, legalEntityID, actorID, key, payload)
}
