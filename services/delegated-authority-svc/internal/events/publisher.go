package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
	"zoiko.io/delegated-authority-svc/internal/domain"
)

// Event type names, in one place because three things now agree on them: the
// builders below, the outbox's CHECK constraint, and asyncapi.yaml.
const (
	EventDelegated = "authority.delegated"
	EventRevoked   = "authority.revoked"
	EventExpired   = "authority.expired"
)

// envelope is this platform's event contract (Doc 03 §19): every published
// event must carry event name, event version, timestamp, tenant ID, legal
// entity ID, jurisdiction context, actor ID, correlation ID, source
// service, and payload schema version. domain.DelegationGrant carries
// real TenantID/LegalEntityID and per-lifecycle-stage actors
// (CreatedBy/RevokedByPrincipalID); expiry is a background-job transition
// with no principal, so actor_id is correctly omitted for that event.
// The struct has no jurisdiction field.
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

// Build renders one lifecycle event into the key and body that will be written
// to Kafka.
//
// It is called at STATE-CHANGE time, inside the same transaction as the write,
// and the bytes are stored in the outbox — not rebuilt when the relay drains.
// That ordering is deliberate: the envelope's actor and correlation describe
// the request that caused the change, and by the time the relay runs that
// request is long gone. Rebuilding later would either lose them or invent them.
func Build(eventType string, d domain.DelegationGrant) (key string, body []byte, err error) {
	var actorID string
	var payload map[string]any

	switch eventType {
	case EventDelegated:
		actorID = d.CreatedByPrincipalID
		payload = map[string]any{
			"delegation_id":          d.DelegationID,
			"legal_entity_id":        d.LegalEntityID,
			"delegator_principal_id": d.DelegatorPrincipalID,
			"delegate_principal_id":  d.DelegatePrincipalID,
			"action_type":            d.ActionType,
			"effective_from":         d.EffectiveFrom,
			"effective_to":           d.EffectiveTo,
		}
	case EventRevoked:
		actorID = deref(d.RevokedByPrincipalID)
		payload = map[string]any{
			"delegation_id":          d.DelegationID,
			"legal_entity_id":        d.LegalEntityID,
			"delegator_principal_id": d.DelegatorPrincipalID,
			"delegate_principal_id":  d.DelegatePrincipalID,
			"action_type":            d.ActionType,
		}
	case EventExpired:
		// No actor, and that is a fact about the transition rather than a gap:
		// nobody expires a delegation, its window closes. Naming the principal
		// whose read happened to observe the lapse would attribute an act to
		// someone who did not perform one — on a register whose entire purpose
		// is recording who did what.
		actorID = ""
		payload = map[string]any{
			"delegation_id":          d.DelegationID,
			"legal_entity_id":        d.LegalEntityID,
			"delegator_principal_id": d.DelegatorPrincipalID,
			"delegate_principal_id":  d.DelegatePrincipalID,
			"action_type":            d.ActionType,
			"effective_to":           d.EffectiveTo,
		}
	default:
		return "", nil, fmt.Errorf("events: unknown event type %q", eventType)
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return "", nil, fmt.Errorf("events: marshal payload for %s: %w", eventType, err)
	}
	body, err = json.Marshal(envelope{
		// A fresh UUID per publish, not a deterministic string — see
		// docs/architecture/known-gaps.md's event_id collision writeup.
		EventID:       "evt-" + uuid.New().String(),
		EventType:     eventType,
		EventVersion:  "1.0",
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: "1.0",
		SourceService: "delegated-authority-svc",
		TenantID:      d.TenantID,
		LegalEntityID: d.LegalEntityID,
		ActorID:       actorID,
		CorrelationID: d.CorrelationID,
		Payload:       raw,
	})
	if err != nil {
		return "", nil, fmt.Errorf("events: marshal envelope for %s: %w", eventType, err)
	}
	// Keyed by delegation, so every event about one grant lands on the same
	// partition and a consumer sees delegated before revoked. Keyed by tenant
	// they could be reordered across partitions, and a consumer could act on a
	// revocation before it knows the grant exists.
	return d.DelegationID, body, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// MessageWriter is the one method Publisher needs from *kafka.Writer.
// Narrowed to an interface so tests can assert what was written without a
// live broker.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

// Publisher writes already-built envelopes to Kafka. It has no knowledge of the
// domain: everything it sends came out of the outbox, which is the only thing
// that decides an event happened.
type Publisher struct {
	log      *zap.Logger
	topic    string
	producer MessageWriter
}

func NewPublisher(log *zap.Logger, topic string, producer *kafka.Writer) *Publisher {
	// producer may be a nil *kafka.Writer (dry-run mode) — storing that
	// directly into the MessageWriter interface field would make the
	// interface itself non-nil, defeating the p.producer == nil check in
	// Publish. Keep the field genuinely nil in that case.
	if producer == nil {
		return &Publisher{log: log, topic: topic}
	}
	return &Publisher{log: log, topic: topic, producer: producer}
}

// NewPublisherWithWriter is NewPublisher but with a caller-supplied
// MessageWriter — used by tests and by the relay's own tests.
func NewPublisherWithWriter(log *zap.Logger, topic string, producer MessageWriter) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

// Publish writes a batch in ONE call.
//
// One call rather than a loop, because kafka-go does not flush a batch of one
// until BatchTimeout elapses: a per-record loop turns a drain of 200 events
// into 200 sequential timer waits, with no errors and no retries to show for
// it. identity-context-svc's outbox drained at 1.03 events/second for exactly
// this reason.
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
