package store

// BIZ-08 deadline events on the shared eventing outbox (ZS-EVENT-001).
//
// deadline.created, deadline.due_soon, deadline.overdue, deadline.completed and
// deadline.superseded are written to eventing_outbox in the SAME transaction as the change
// they announce, with canonical com.zoikosuite.business.deadline.* types. They used to be
// published by the handler after the commit, with a failure only logged; for due_soon and
// overdue the "notified" timestamp was committed first, so a failed publish was never
// retried. Payload schemas live in contracts/events and are enforced by the store tests.
// The old event_type is carried as the envelope's legacy type. The service's other events
// (exception cases, findings, tasks) still use the direct publisher.

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/eventing/envelope"
	"zoiko.io/eventing/outbox"
	"zoiko.io/exception-escalation-svc/internal/domain"
)

type deadlineEventKind struct {
	canonicalType string
	legacyType    string
}

var (
	deadlineCreatedEvent    = deadlineEventKind{"com.zoikosuite.business.deadline.created", "deadline.created"}
	deadlineDueSoonEvent    = deadlineEventKind{"com.zoikosuite.business.deadline.became-due-soon", "deadline.due_soon"}
	deadlineOverdueEvent    = deadlineEventKind{"com.zoikosuite.business.deadline.became-overdue", "deadline.overdue"}
	deadlineCompletedEvent  = deadlineEventKind{"com.zoikosuite.business.deadline.completed", "deadline.completed"}
	deadlineSupersededEvent = deadlineEventKind{"com.zoikosuite.business.deadline.superseded", "deadline.superseded"}
)

// enqueueDeadlineEvent writes one deadline event to eventing_outbox in the caller's
// transaction. An empty event region fails the write (and rolls the change back) rather
// than emitting an event with a guessed region.
func (s *PgStore) enqueueDeadlineEvent(ctx context.Context, tx pgx.Tx, k deadlineEventKind, correlationID, actorID, tenantID, legalEntityID, deadlineID string, data map[string]any) error {
	env, err := envelope.New(envelope.Spec{
		Type:            k.canonicalType,
		LegacyType:      k.legacyType,
		Service:         "exception-escalation-svc",
		SchemaVersion:   "1.0.0",
		OccurredAt:      time.Now().UTC(),
		TenantID:        tenantID,
		LegalEntityID:   legalEntityID,
		AggregateType:   "deadline",
		AggregateID:     deadlineID,
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

// deadlineEventData is the payload shared by the lifecycle events.
func deadlineEventData(d *domain.Deadline) map[string]any {
	m := map[string]any{
		"deadline_id":     d.DeadlineID,
		"tenant_id":       d.TenantID,
		"legal_entity_id": d.LegalEntityID,
		"title":           d.Title,
		"due_at":          d.DueAt.UTC().Format(time.RFC3339),
		"status":          string(d.Status),
	}
	if d.LinkedObjectType != "" {
		m["linked_object_type"] = d.LinkedObjectType
		m["linked_object_id"] = d.LinkedObjectID
	}
	if d.OwnerPrincipalID != "" {
		m["owner_principal_id"] = d.OwnerPrincipalID
	}
	if d.SourceType != "" {
		m["source_type"] = d.SourceType
		m["source_ref"] = d.SourceRef
		m["source_version"] = d.SourceVersion
	}
	return m
}
