// Package events builds this service's wire envelopes and hands them to Kafka.
//
// The build and the send are deliberately separate calls. An envelope is built
// inside the store transaction that caused it and written to the outbox; the
// relay sends it later. Before that split, the handler wrote to Kafka after the
// commit and logged the error — so a broker hiccup during a role retirement
// discarded the only notice that the role had changed, told the operator it had
// succeeded, and left every session holding that role carrying its old grants.
// See internal/outbox.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/access-control-svc/internal/domain"
)

// Event names. Exported because the outbox's CHECK constraint, asyncapi.yaml
// and the audit script all have to agree with them, and a string literal
// repeated in four places is a string literal that will disagree in one.
const (
	TypeRoleCreated   = "role.created"
	TypeRoleUpdated   = "role.updated"
	TypeBundleUpdated = "permission.bundle.updated"
)

// SourceService, EventVersion and SchemaVersion are the envelope constants.
const (
	SourceService = "access-control-svc"
	EventVersion  = "1.0"
	SchemaVersion = "1.0"
)

// envelope is this platform's event contract (Doc 03 §19): every published
// event must carry event name, event version, timestamp, tenant ID, legal
// entity ID, jurisdiction context, actor ID, correlation ID, source
// service, and payload schema version. Neither domain.RoleDefinition nor
// domain.PermissionBundleDef carries a legal_entity_id or jurisdiction
// field — a role/bundle definition is tenant-wide config, not scoped to
// one legal entity, so both are correctly omitted rather than fabricated.
type envelope struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	EventVersion  string          `json:"event_version"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	TenantID      string          `json:"tenant_id,omitempty"`
	ActorID       string          `json:"actor_id,omitempty"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

// Outbound is one built, ready-to-send event: the Kafka key and the marshalled
// envelope. It is what goes into the outbox row and what comes back out.
type Outbound struct {
	EventType string
	Key       string
	Body      []byte
}

// Build marshals one envelope.
func Build(eventType, correlationID, tenantID, actorID, key string, payload map[string]any) (Outbound, error) {
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
		ActorID:       actorID,
		CorrelationID: correlationID,
		Payload:       raw,
	})
	if err != nil {
		return Outbound{}, fmt.Errorf("marshal %s envelope: %w", eventType, err)
	}
	return Outbound{EventType: eventType, Key: key, Body: body}, nil
}

// RoleCreated builds role.created.
func RoleCreated(r domain.RoleDefinition, actorID string) (Outbound, error) {
	return Build(TypeRoleCreated, r.CorrelationID, r.TenantID, actorID, r.RoleDefinitionID, map[string]any{
		// role_id first, and not a synonym anyone may drop — see RoleUpdated.
		"role_id":            r.RoleDefinitionID,
		"role_definition_id": r.RoleDefinitionID,
		"role_code":          r.RoleCode,
		"role_name":          r.RoleName,
		"role_scope_type":    r.RoleScopeType,
		"status":             string(r.Status),
	})
}

// RoleUpdated builds role.updated.
//
// ── WHY THE PAYLOAD CARRIES role_id ─────────────────────────────────────────
//
// Two consumers care about this event, and they read different parts of it.
//
// authorization-svc's lifecycle consumer invalidates its cached grant sources
// for the tenant. It needs only the envelope's tenant_id, which this service
// has always sent, and it is verified working — audit §11 watches it happen.
//
// identity-context-svc's handleRoleUpdated revokes the sessions of every
// principal holding the role, because a role's permission bundles are frozen
// into the session envelope at resolve time, so changing what a role grants
// leaves every live envelope asserting the old grant until it expires. It
// unmarshals {"role_id": ...} and bails out with "role.updated names no
// role_id — cannot revoke" when the field is empty. This service emitted
// role_definition_id and nothing else, so that branch was the only branch it
// could ever take.
//
// Fixing the payload is NECESSARY and, today, NOT SUFFICIENT: that consumer's
// reader is configured with a single topic (KAFKA_EVENTS_TOPIC =
// zoiko.identity.events) and so never receives this topic at all. That is a
// wiring gap in identity-context-svc, outside this service's boundary, and it
// is recorded in progress.md rather than fixed from here — but the event now
// carries what that consumer reads, so subscribing it is the only step left.
//
// Both names are sent. role_id is the contract the consumer reads;
// role_definition_id is what this service's own register calls the same id, and
// removing it would break any consumer built against the shape that shipped.
// They are always the same value: CreateRole provisions the role into
// authorization-svc under exactly this id.
func RoleUpdated(r domain.RoleDefinition, actorID string) (Outbound, error) {
	return Build(TypeRoleUpdated, r.CorrelationID, r.TenantID, actorID, r.RoleDefinitionID, map[string]any{
		"role_id":            r.RoleDefinitionID,
		"role_definition_id": r.RoleDefinitionID,
		"role_code":          r.RoleCode,
		"status":             string(r.Status),
	})
}

// BundleUpdated builds permission.bundle.updated.
//
// It carries role_id for the same reason RoleUpdated does: a bundle change IS a
// change to what its role grants, and a consumer reacting to it needs to know
// which role without a second lookup. The store pairs every bundle event with a
// role.updated for the same role — see enqueueBundleChange — because
// permission.bundle.updated is a name identity-context-svc's dispatch switch
// does not match, so on its own it can never revoke a session.
func BundleUpdated(b domain.PermissionBundleDef, actorID string) (Outbound, error) {
	return Build(TypeBundleUpdated, b.CorrelationID, b.TenantID, actorID, b.BundleID, map[string]any{
		"bundle_id":          b.BundleID,
		"role_id":            b.RoleDefinitionID,
		"role_definition_id": b.RoleDefinitionID,
		"bundle_code":        b.BundleCode,
		"permitted_actions":  b.PermittedActions,
		"active_flag":        b.ActiveFlag,
	})
}

// MessageWriter is the one method Publisher needs from *kafka.Writer.
// Narrowed to an interface purely so publisher_test.go can assert
// envelope content without a live broker.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

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
// MessageWriter — used by tests to substitute a fake.
func NewPublisherWithWriter(log *zap.Logger, topic string, producer MessageWriter) *Publisher {
	return &Publisher{log: log, topic: topic, producer: producer}
}

// Publish writes one batch.
//
// One WriteMessages call for the whole batch rather than a loop: kafka-go's
// Writer applies its BatchTimeout per call, so a per-record loop waits it out
// once per event and turns a 200-event backlog into a 200-tick drain.
//
// The error is RETURNED, never swallowed. That is the entire difference between
// this and the fire-and-forget publish it replaces: the relay leaves the rows
// unpublished and retries, instead of logging and moving on.
func (p *Publisher) Publish(ctx context.Context, msgs []kafka.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	if p.producer == nil {
		p.log.Info("simulating publish in dry mode", zap.Int("events", len(msgs)))
		return nil
	}
	if err := p.producer.WriteMessages(ctx, msgs...); err != nil {
		return fmt.Errorf("kafka write to %s: %w", p.topic, err)
	}
	return nil
}
