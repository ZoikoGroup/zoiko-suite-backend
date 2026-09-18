// Package events contains the domain event publisher and consumer.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/outbox"
)

// ── Event type names ─────────────────────────────────────────────────────────
//
// Declared as constants rather than written inline at each call site, because
// three of them are also matched by this service's own CONSUMER and a literal
// that agreed with its handler by coincidence is how the risk-signal loop
// below came to exist in the first place.

const (
	EventContextResolved   = "identity.context.resolved"
	EventResolutionFailed  = "identity.context.resolution_failed"
	EventSessionInvalidated = "session.invalidated"
	EventPrincipalStatusChanged = "principal.status.changed"
	EventAuthenticationSucceeded = "identity.authentication.succeeded"
	EventAuthenticationFailed    = "identity.authentication.failed"

	// EventRiskSignalUnavailable replaces the "session.risk.changed" this
	// service used to publish when its risk cache missed.
	//
	// That name was wrong twice over. Nothing had CHANGED — the cache was
	// simply empty — and, worse, this service's own consumer subscribes to
	// session.risk.changed as the sole writer of that cache. The service was
	// therefore consuming its own telemetry: every cache-miss resolution
	// published an event that came straight back, burned a Redis dedupe key,
	// and logged "risk signal cached" for a signal that UpsertSignal had
	// silently declined to cache (its valid_to is the zero time, so the TTL
	// computes negative and the write is skipped).
	//
	// The posture was never affected, so nothing user-visible was wrong. What
	// was wrong is that the only alarm telling an operator the Intelligence
	// Plane was never wired up was being answered by the service that raised
	// it. This is an ALARM, and its name now says so.
	EventRiskSignalUnavailable = "identity.risk_signal.unavailable"

	// ── GOV-01 contract events (spec section 17) ─────────────────────────────

	// EventTenantContextCacheInvalidated is GOV-01's named
	// TenantContextCacheInvalidated. Consumers are GOV-03 and the gateways,
	// which hold their own derived caches.
	EventTenantContextCacheInvalidated = "identity.context.cache_invalidated"

	// EventSupportContextAttached is GOV-01's named SupportContextAttached.
	EventSupportContextAttached = "identity.support_context.attached"

	// EventSupportContextRevoked covers both an explicit early revocation and
	// an expiry observed by the reconciler. The payload's reason distinguishes
	// them; the event does not, because a consumer's response is the same.
	EventSupportContextRevoked = "identity.support_context.revoked"

	// EventTenantContextInvalidated reports a tenant-wide session revocation.
	EventTenantContextInvalidated = "identity.context.tenant_invalidated"

	// EventDispositionExecuted is GOV-09's DispositionExecuted, emitted by the
	// retention worker with its certificate.
	EventDispositionExecuted = "identity.retention.disposition_executed"

	// EventDispositionBlocked reports rows that were due but held. GOV-09's
	// contract names DispositionFailed; a legal hold is not a failure, so this
	// is a distinct name rather than a misuse of that one.
	EventDispositionBlocked = "identity.retention.disposition_blocked"
)

// SourceServiceName is stamped on every envelope this service emits, and is
// what the consumer's self-source guard matches against.
const SourceServiceName = "identity-context-svc"

// envelope is this platform's event contract (Doc 03 §19): every published
// event must carry event name, event version, timestamp, tenant ID, legal
// entity ID, jurisdiction context, actor ID, correlation ID, source
// service, and payload schema version. Not every event this service
// publishes has a tenant/legal-entity/actor to source from (e.g. a failed
// resolution may not have resolved a tenant at all) — those fields are
// correctly omitted per-event rather than fabricated when the underlying
// call site has nothing real to put there.
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
	CorrelationID string          `json:"correlation_id,omitempty"`
	Payload       json.RawMessage `json:"payload"`
}

// MessageWriter is the one method a direct-to-Kafka sink needs from
// *kafka.Writer. Narrowed to an interface purely so publisher_test.go can
// assert envelope content without a live broker.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

// Sink is where a rendered event goes.
//
// Production uses OutboxSink, which makes the event durable in Postgres before
// anything touches a broker. DirectSink writes straight to Kafka and exists
// for tests only — it is the OLD behaviour, and its doc comment says so, so
// nobody reintroduces it into a wiring path by accident.
type Sink interface {
	Emit(ctx context.Context, rec outbox.Record) error
}

// OutboxSink is the production sink. See package outbox for why.
type OutboxSink struct {
	store outbox.Enqueuer
}

func NewOutboxSink(store outbox.Enqueuer) *OutboxSink { return &OutboxSink{store: store} }

func (s *OutboxSink) Emit(ctx context.Context, rec outbox.Record) error {
	return s.store.Enqueue(ctx, rec)
}

// DirectSink writes straight to Kafka with no durability.
//
// TEST ONLY. This is the pre-outbox behaviour: an event lost here is lost
// permanently, with the business write already committed. Nothing in
// cmd/server wires it.
type DirectSink struct {
	writer MessageWriter
}

func NewDirectSink(w MessageWriter) *DirectSink { return &DirectSink{writer: w} }

func (s *DirectSink) Emit(ctx context.Context, rec outbox.Record) error {
	// Topic is set on the Writer itself, not here — kafka-go rejects a Message
	// that also specifies Topic when the Writer already has one.
	return s.writer.WriteMessages(ctx, kafka.Message{
		Key:   []byte(rec.PartitionKey),
		Value: rec.Payload,
	})
}

// Publisher implements EventPublisher against the platform event backbone.
//
// Evidence obligation: every resolution (success AND failure) produces a
// durable event. Since the outbox landed, "durable" means committed to
// Postgres before the caller's request completes — not handed to a goroutine
// that may or may not reach a broker. Delivery is at-least-once and every
// consumer in the estate dedupes on event_id.
//
// Events are facts, not commands. Published topics are append-only.
type Publisher struct {
	log   *zap.Logger
	topic string
	sink  Sink
}

// NewPublisher builds the production publisher over the transactional outbox.
func NewPublisher(log *zap.Logger, topic string, store outbox.Enqueuer) *Publisher {
	return &Publisher{log: log, topic: topic, sink: NewOutboxSink(store)}
}

// NewPublisherWithWriter is NewPublisher but writing straight to a
// caller-supplied MessageWriter. Used by tests to substitute a fake; see
// DirectSink for why it is not a production path.
func NewPublisherWithWriter(log *zap.Logger, topic string, producer MessageWriter) *Publisher {
	return &Publisher{log: log, topic: topic, sink: NewDirectSink(producer)}
}

// NewPublisherWithSink lets a caller supply any sink. Used by the resolver's
// transactional path and by tests.
func NewPublisherWithSink(log *zap.Logger, topic string, sink Sink) *Publisher {
	return &Publisher{log: log, topic: topic, sink: sink}
}

func (p *Publisher) PublishContextResolved(
	ctx context.Context,
	principalID, tenantID, legalEntityID, sessionContextID, correlationID string,
) error {
	return p.emit(ctx, EventContextResolved, tenantID, legalEntityID, principalID, correlationID, sessionContextID, map[string]any{
		"principal_id":       principalID,
		"tenant_id":          tenantID,
		"legal_entity_id":    legalEntityID,
		"session_context_id": sessionContextID,
		"correlation_id":     correlationID,
	})
}

func (p *Publisher) PublishResolutionFailed(
	ctx context.Context,
	subject, correlationID, reason string,
) error {
	return p.emit(ctx, EventResolutionFailed, "", "", "", correlationID, subject, map[string]any{
		"principal_id_or_subject": subject,
		"correlation_id":          correlationID,
		"failure_reason":          reason,
	})
}

func (p *Publisher) PublishSessionInvalidated(
	ctx context.Context,
	sessionContextID, principalID string,
	reason domain.InvalidationReason,
	correlationID string,
) error {
	return p.emit(ctx, EventSessionInvalidated, "", "", principalID, correlationID, sessionContextID, map[string]any{
		"session_context_id":  sessionContextID,
		"principal_id":        principalID,
		"invalidation_reason": reason,
		"correlation_id":      correlationID,
	})
}

// PublishRiskSignalUnavailable raises the alarm that the risk cache had no
// signal for a principal.
//
// The payload deliberately does NOT carry a signal_value. It previously
// published new_posture STANDARD and signal_source UNAVAILABLE in the shape of
// a risk signal, which is what made it consumable as one. There is no signal
// here — that is the entire message.
func (p *Publisher) PublishRiskSignalUnavailable(ctx context.Context, principalID, correlationID string) error {
	return p.emit(ctx, EventRiskSignalUnavailable, "", "", principalID, correlationID, principalID, map[string]any{
		"principal_id":    principalID,
		"defaulted_to":    string(domain.TrustPostureStandard),
		"correlation_id":  correlationID,
		"observed_source": "UNAVAILABLE",
	})
}

func (p *Publisher) PublishPrincipalStatusChanged(
	ctx context.Context,
	principalID, tenantID string,
	newStatus domain.PrincipalStatus,
	actorID, correlationID string,
) error {
	return p.emit(ctx, EventPrincipalStatusChanged, tenantID, "", actorID, correlationID, principalID, map[string]any{
		"principal_id":   principalID,
		"tenant_id":      tenantID,
		"new_status":     string(newStatus),
		"actor":          actorID,
		"correlation_id": correlationID,
	})
}

// PublishAuthenticationSucceeded records that a human proved possession of a
// principal's credential.
//
// Distinct from identity.context.resolved: that event says an envelope was
// issued, this one says a password was verified. They are usually seconds
// apart but they answer different questions, and collapsing them would make
// "someone logged in but never obtained an envelope" — a resolution that
// failed on tenant, entity or trust posture AFTER a correct password —
// invisible in the event stream.
func (p *Publisher) PublishAuthenticationSucceeded(
	ctx context.Context,
	principalID, tenantID, correlationID string,
) error {
	return p.emit(ctx, EventAuthenticationSucceeded, tenantID, "", principalID, correlationID, principalID, map[string]any{
		"principal_id":    principalID,
		"tenant_id":       tenantID,
		"credential_type": "PASSWORD",
		"correlation_id":  correlationID,
	})
}

// PublishAuthenticationFailed records a rejected attempt.
//
// subject is the email as supplied. It is emitted even when it matches no
// principal, because a stream of failures against addresses that do not exist
// is the signature of an enumeration sweep, and dropping those events would
// hide exactly the attack the constant-time verification path is defending
// against. principalID is empty in that case rather than fabricated.
func (p *Publisher) PublishAuthenticationFailed(
	ctx context.Context,
	subject, principalID, tenantID, correlationID, reason string,
) error {
	return p.emit(ctx, EventAuthenticationFailed, tenantID, "", principalID, correlationID, subject, map[string]any{
		"subject":         subject,
		"principal_id":    principalID,
		"tenant_id":       tenantID,
		"credential_type": "PASSWORD",
		"failure_reason":  reason,
		"correlation_id":  correlationID,
	})
}

// ── GOV-01 contract events ───────────────────────────────────────────────────

// PublishTenantContextCacheInvalidated announces that GOV-01 dropped routing
// hints, so downstream holders of derived context caches drop theirs.
func (p *Publisher) PublishTenantContextCacheInvalidated(
	ctx context.Context,
	tenantID, actorID, reason, correlationID string,
	identifiers []string,
) error {
	return p.emit(ctx, EventTenantContextCacheInvalidated, tenantID, "", actorID, correlationID, tenantID, map[string]any{
		"tenant_id":           tenantID,
		"ingress_identifiers": identifiers,
		"reason":              reason,
		"actor":               actorID,
		"correlation_id":      correlationID,
	})
}

// PublishSupportContextAttached announces a privileged elevation.
//
// The justification travels with it deliberately. A security team watching
// this topic should not have to query this service to learn WHY somebody was
// granted access to a customer tenant.
func (p *Publisher) PublishSupportContextAttached(ctx context.Context, sc domain.SupportContext) error {
	payload := map[string]any{
		"support_context_id":    sc.SupportContextID,
		"tenant_id":             sc.TenantID,
		"support_principal_id":  sc.SupportPrincipalID,
		"approver_principal_id": sc.ApproverPrincipalID,
		"reason_code":           sc.ReasonCode,
		"justification":         sc.Justification,
		"ticket_ref":            sc.TicketRef,
		"granted_at":            sc.GrantedAt,
		"expires_at":            sc.ExpiresAt,
		"evidence_id":           sc.EvidenceID,
		"correlation_id":        sc.CorrelationID,
	}
	if sc.SubjectPrincipalID != nil {
		payload["subject_principal_id"] = *sc.SubjectPrincipalID
	}
	return p.emit(ctx, EventSupportContextAttached, sc.TenantID, "", sc.ApproverPrincipalID,
		sc.CorrelationID, sc.SupportContextID, payload)
}

// PublishSupportContextRevoked announces that an elevation ended.
func (p *Publisher) PublishSupportContextRevoked(
	ctx context.Context,
	supportContextID, tenantID, supportPrincipalID, reason, actorID, correlationID string,
) error {
	return p.emit(ctx, EventSupportContextRevoked, tenantID, "", actorID, correlationID, supportContextID, map[string]any{
		"support_context_id":   supportContextID,
		"tenant_id":            tenantID,
		"support_principal_id": supportPrincipalID,
		"reason":               reason,
		"actor":                actorID,
		"correlation_id":       correlationID,
	})
}

// PublishTenantContextInvalidated announces a tenant-wide session revocation.
func (p *Publisher) PublishTenantContextInvalidated(
	ctx context.Context,
	tenantID string,
	revoked int,
	reason domain.InvalidationReason,
	justification, actorID, correlationID string,
) error {
	return p.emit(ctx, EventTenantContextInvalidated, tenantID, "", actorID, correlationID, tenantID, map[string]any{
		"tenant_id":           tenantID,
		"sessions_revoked":    revoked,
		"invalidation_reason": reason,
		"justification":       justification,
		"actor":               actorID,
		"correlation_id":      correlationID,
	})
}

// PublishDispositionExecuted reports a completed retention sweep with its
// certificate id.
func (p *Publisher) PublishDispositionExecuted(
	ctx context.Context,
	tenantID string,
	outcome domain.DispositionOutcome,
	correlationID string,
) error {
	return p.emit(ctx, EventDispositionExecuted, tenantID, "", SourceServiceName, correlationID, outcome.CertificateID, map[string]any{
		"tenant_id":      tenantID,
		"record_class":   outcome.RecordClass,
		"considered":     outcome.Considered,
		"disposed":       outcome.Disposed,
		"held_back":      outcome.HeldBack,
		"certificate_id": outcome.CertificateID,
		"swept_at":       outcome.SweptAt,
		"correlation_id": correlationID,
	})
}

// PublishDispositionBlocked reports rows held back by a legal hold.
func (p *Publisher) PublishDispositionBlocked(
	ctx context.Context,
	tenantID, recordClass string,
	held int,
	correlationID string,
) error {
	return p.emit(ctx, EventDispositionBlocked, tenantID, "", SourceServiceName, correlationID, tenantID, map[string]any{
		"tenant_id":      tenantID,
		"record_class":   recordClass,
		"held_back":      held,
		"reason":         domain.ErrCodeLegalHoldActive,
		"correlation_id": correlationID,
	})
}

// ── Rendering ────────────────────────────────────────────────────────────────

// Render builds the outbox record for an event without emitting it.
//
// Exported because the resolver enqueues the context.resolved event inside the
// SAME transaction that writes the session_contexts row. That is the one place
// where atomicity between the business write and its evidence actually matters:
// a session that exists with no event, or an event for a session that failed
// to persist, are both states an audit cannot explain.
func Render(
	eventType, tenantID, legalEntityID, actorID, correlationID, key string,
	payload map[string]any,
) (outbox.Record, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return outbox.Record{}, fmt.Errorf("event %q: marshal payload: %w", eventType, err)
	}
	env := envelope{
		// A fresh UUID per publish, not a deterministic string — see
		// docs/architecture/known-gaps.md's event_id collision writeup.
		EventID:       "evt-" + uuid.New().String(),
		EventType:     eventType,
		EventVersion:  "1.0",
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: "1.0",
		SourceService: SourceServiceName,
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		ActorID:       actorID,
		CorrelationID: correlationID,
		Payload:       json.RawMessage(raw),
	}
	data, err := json.Marshal(env)
	if err != nil {
		return outbox.Record{}, fmt.Errorf("event %q: marshal envelope: %w", eventType, err)
	}
	return outbox.Record{
		EventID:      env.EventID,
		EventType:    eventType,
		TenantID:     tenantID,
		PartitionKey: key,
		Payload:      data,
	}, nil
}

// EnqueueTx renders an event and writes it inside an existing transaction.
//
// The tx must already have app.tenant_id set for tenantID — every caller here
// is already inside a withRLS block, so it does.
func EnqueueTx(
	ctx context.Context,
	store outbox.Enqueuer,
	tx pgx.Tx,
	eventType, tenantID, legalEntityID, actorID, correlationID, key string,
	payload map[string]any,
) error {
	rec, err := Render(eventType, tenantID, legalEntityID, actorID, correlationID, key, payload)
	if err != nil {
		return err
	}
	return store.EnqueueTx(ctx, tx, rec)
}

// emit renders the event and hands it to the sink.
func (p *Publisher) emit(ctx context.Context, eventType, tenantID, legalEntityID, actorID, correlationID, key string, payload map[string]any) error {
	rec, err := Render(eventType, tenantID, legalEntityID, actorID, correlationID, key, payload)
	if err != nil {
		return err
	}
	if err := p.sink.Emit(ctx, rec); err != nil {
		return fmt.Errorf("event %q: %w", eventType, err)
	}

	p.log.Debug("event enqueued",
		zap.String("event_type", eventType),
		zap.String("event_id", rec.EventID),
		zap.String("topic", p.topic),
	)
	return nil
}
