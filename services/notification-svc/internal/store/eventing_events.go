package store

// BIZ-03 template events on the shared eventing outbox (ZS-EVENT-001).
//
// template.created, template.version_approved, template.published and template.retired are
// written to eventing_outbox in the same transaction as the change, with canonical
// com.zoikosuite.business.template.* types. The payload schemas live in contracts/events and
// are enforced by the store tests. The old event_type is carried as the envelope's legacy
// type. The service's other events (notification.*, delivery.attempt.*, communication.*)
// still use event_outbox.

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/eventing/envelope"
	"zoiko.io/eventing/outbox"
)

type templateEventKind struct {
	canonicalType string
	legacyType    string
}

var (
	templateCreatedEvent         = templateEventKind{"com.zoikosuite.business.template.created", "template.created"}
	templateVersionApprovedEvent = templateEventKind{"com.zoikosuite.business.template.version-approved", "template.version_approved"}
	templatePublishedEvent       = templateEventKind{"com.zoikosuite.business.template.published", "template.published"}
	templateRetiredEvent         = templateEventKind{"com.zoikosuite.business.template.retired", "template.retired"}
)

// enqueueTemplateEvent writes one template event to eventing_outbox in the caller's
// transaction. An empty event region fails the write (and rolls the change back) rather
// than emitting an event with a guessed region.
func (s *PgStore) enqueueTemplateEvent(ctx context.Context, tx pgx.Tx, k templateEventKind, correlationID, tenantID, legalEntityID, actorID, templateID string, data map[string]any) error {
	env, err := envelope.New(envelope.Spec{
		Type:            k.canonicalType,
		LegacyType:      k.legacyType,
		Service:         "notification-svc",
		SchemaVersion:   "1.0.0",
		OccurredAt:      time.Now().UTC(),
		TenantID:        tenantID,
		LegalEntityID:   legalEntityID,
		AggregateType:   "template",
		AggregateID:     templateID,
		CorrelationID:   correlationID,
		ActorID:         actorID,
		ResidencyRegion: s.eventRegion,
		Classification:  envelope.Internal,
		Data:            data,
	})
	if err != nil {
		return fmt.Errorf("build %s event: %w", k.canonicalType, err)
	}
	if err := outbox.Enqueue(ctx, tx, env); err != nil {
		return fmt.Errorf("enqueue %s event: %w", k.canonicalType, err)
	}
	return nil
}
