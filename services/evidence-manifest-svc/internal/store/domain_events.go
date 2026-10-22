package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	svcmiddleware "zoiko.io/evidence-manifest-svc/internal/middleware"
	"zoiko.io/evidence-manifest-svc/internal/outbox"
)

// AUD-03 / AUD-04 event names. They are the names the Audit & Assurance
// specification gives (ZoikoSuite_Audit_Assurance_Detailed_Service_
// Specifications_v1_0, AUD-03 and AUD-04 "Events"), used verbatim. The
// platform event catalogue uses a dotted lower-case style
// (evidence.manifest.generated); which of the two applies to these ten is an
// open naming question, so the specification's own spelling is not altered.
const (
	EventAuditPopulationBuilt      = "AuditPopulationBuilt"
	EventPopulationValidationFail  = "PopulationValidationFailed"
	EventAuditPopulationFrozen     = "AuditPopulationFrozen"
	EventPopulationDeltaAdded      = "PopulationDeltaAdded"
	EventPopulationSuperseded      = "PopulationSuperseded"
	EventSampleDesignApproved      = "SampleDesignApproved"
	EventSampleSelected            = "SampleSelected"
	EventSampleItemExceptionRecord = "SampleItemExceptionRecorded"
	EventSampleEvaluated           = "SampleEvaluated"
	EventSampleSuperseded          = "SampleSuperseded"

	aggregatePopulation = "AUDIT_POPULATION"
	aggregateSample     = "SAMPLE_DESIGN"
)

// domainEvent is the payload of an AUD-03/AUD-04 outbox event. The envelope
// fields match ManifestGeneratedEvent (Doc 03 §19: event name, version,
// timestamp, tenant, legal entity, actor, correlation id, source service);
// event-specific facts are in Details.
type domainEvent struct {
	EventID       string         `json:"event_id"`
	EventType     string         `json:"event_type"`
	EventVersion  string         `json:"event_version"`
	SourceService string         `json:"source_service"`
	AggregateType string         `json:"aggregate_type"`
	AggregateID   string         `json:"aggregate_id"`
	TenantID      string         `json:"tenant_id"`
	LegalEntityID string         `json:"legal_entity_id"`
	ActorID       string         `json:"actor_id,omitempty"`
	CorrelationID string         `json:"correlation_id,omitempty"`
	OccurredAt    time.Time      `json:"occurred_at"`
	Details       map[string]any `json:"details,omitempty"`
}

// emitEvent writes one event to the transactional outbox INSIDE the caller's
// transaction, so the event exists if and only if the state change it reports
// is committed. Callers invoke it only on a real transition — never on an
// idempotent replay, a refused request or a rolled-back attempt.
func emitEvent(ctx context.Context, tx pgx.Tx, eventType, aggregateType, aggregateID, legalEntityID, actorID, correlationID string, details map[string]any) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	evt := domainEvent{
		EventID: "evt-" + uuid.NewString(), EventType: eventType, EventVersion: "1.0",
		SourceService: "evidence-manifest-svc", AggregateType: aggregateType, AggregateID: aggregateID,
		TenantID: tenantID, LegalEntityID: legalEntityID, ActorID: actorID, CorrelationID: correlationID,
		OccurredAt: time.Now().UTC(), Details: details,
	}
	var actor *string
	if actorID != "" {
		actor = &actorID
	}
	if err := outbox.Insert(ctx, tx, outbox.Event{
		AggregateType: aggregateType, AggregateID: aggregateID, EventType: eventType,
		TenantID: tenantID, LegalEntityID: legalEntityID, ActorID: actor,
		CorrelationID: correlationID, Payload: evt,
	}); err != nil {
		return fmt.Errorf("emit %s: %w", eventType, err)
	}
	return nil
}

// populationLegalEntity resolves the legal entity a sampling event belongs to
// from the population the design samples.
func populationLegalEntity(ctx context.Context, tx pgx.Tx, populationID string) (string, error) {
	var le string
	err := tx.QueryRow(ctx, `SELECT legal_entity_id::text FROM audit_populations WHERE population_id=$1`, populationID).Scan(&le)
	return le, err
}
