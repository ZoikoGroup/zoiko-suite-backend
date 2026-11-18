package store

// BIZ-05 task and case events on the shared eventing outbox (ZS-EVENT-001).
//
// task.created, task.assigned, task.escalated, task.completed, task.reopened and case.closed
// are written to eventing_outbox in the SAME transaction as the change they announce, with
// canonical com.zoikosuite.business.* types, exactly like the BIZ-08 deadline events (see
// eventing_events.go). They used to be published by the handler after the commit with a
// failure only logged, so a broker problem lost them silently. Payload schemas live in
// contracts/events and are enforced by the store tests. The old event_type is carried as the
// envelope's legacy type. Starting, blocking and cancelling a task name no event in the spec
// and write none, as before.

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/eventing/envelope"
	"zoiko.io/eventing/outbox"
	"zoiko.io/exception-escalation-svc/internal/domain"
)

type workEventKind struct {
	canonicalType string
	legacyType    string
	aggregateType string
}

var (
	taskCreatedEvent   = workEventKind{"com.zoikosuite.business.task.created", "task.created", "task"}
	taskAssignedEvent  = workEventKind{"com.zoikosuite.business.task.assigned", "task.assigned", "task"}
	taskEscalatedEvent = workEventKind{"com.zoikosuite.business.task.escalated", "task.escalated", "task"}
	taskCompletedEvent = workEventKind{"com.zoikosuite.business.task.completed", "task.completed", "task"}
	taskReopenedEvent  = workEventKind{"com.zoikosuite.business.task.reopened", "task.reopened", "task"}
	caseClosedEvent    = workEventKind{"com.zoikosuite.business.case.closed", "case.closed", "case"}
)

// enqueueWorkEvent writes one task or case event to eventing_outbox in the caller's
// transaction. An empty event region fails the write (and rolls the change back) rather than
// emitting an event with a guessed region.
func (s *PgStore) enqueueWorkEvent(ctx context.Context, tx pgx.Tx, k workEventKind, correlationID, actorID, tenantID, legalEntityID, aggregateID string, data map[string]any) error {
	env, err := envelope.New(envelope.Spec{
		Type:            k.canonicalType,
		LegacyType:      k.legacyType,
		Service:         "exception-escalation-svc",
		SchemaVersion:   "1.0.0",
		OccurredAt:      time.Now().UTC(),
		TenantID:        tenantID,
		LegalEntityID:   legalEntityID,
		AggregateType:   k.aggregateType,
		AggregateID:     aggregateID,
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

// taskEventData is the payload shared by the task events.
func taskEventData(t *domain.Task) map[string]any {
	m := map[string]any{
		"task_id":         t.TaskID,
		"tenant_id":       t.TenantID,
		"legal_entity_id": t.LegalEntityID,
		"task_type":       t.TaskType,
		"priority":        string(t.Priority),
		"status":          string(t.Status),
	}
	if t.CaseID != nil {
		m["case_id"] = *t.CaseID
	}
	if t.LinkedObjectType != "" {
		m["linked_object_type"] = t.LinkedObjectType
		m["linked_object_id"] = t.LinkedObjectID
	}
	if t.AssignedToRole != "" {
		m["assigned_to_role"] = t.AssignedToRole
	}
	if t.AssignedToUser != "" {
		m["assigned_to_user"] = t.AssignedToUser
	}
	return m
}
