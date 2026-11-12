package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"zoiko.io/eventing/envelope"
	"zoiko.io/eventing/outbox"

	"zoiko.io/reconciliation-engine-svc/internal/domain"
)

// Events go through the shared transactional outbox (services/eventing,
// ZS-EVENT-001 §6): enqueueEvent writes one eventing_outbox row on the
// write's own transaction, so the event commits or rolls back with the fact
// it describes, and the shared relay (cmd/server) delivers it.
//
// The domain segment is "accounting": the Enterprise Domain Event Catalogue
// lists the Reconciliation Engine's events as accounting.reconciliation-break.*.
// An exception is that catalogue's reconciliation break, so every exception
// event shares the reconciliation-break aggregate (and so its partition key).
const eventService = "reconciliation-engine-svc"

// Aggregate types: the kebab nouns the canonical subject and partition key are
// built from. Each maps one-to-one onto a legacy outbox_events.aggregate_type
// (see migration 000012's carry-over).
const (
	aggregateRun           = "reconciliation-run"           // legacy reconciliation_run
	aggregateBreak         = "reconciliation-break"         // legacy reconciliation_exception
	aggregateCertification = "reconciliation-certification" // legacy certification
	aggregateDefinition    = "reconciliation-definition"    // legacy reconciliation_definition
)

// eventKind is one event this service emits: its canonical type, the legacy
// event_type existing consumers filter on (emitted alongside while legacy
// fields are on), and the aggregate it is about.
type eventKind struct {
	Type, LegacyType, Aggregate string
}

var (
	evRunStarted         = eventKind{"com.zoikosuite.accounting.reconciliation-run.started", "CONTROL.ReconciliationStarted", aggregateRun}
	evRunFailed          = eventKind{"com.zoikosuite.accounting.reconciliation-run.failed", "CONTROL.ReconciliationFailed", aggregateRun}
	evRunCompleted       = eventKind{"com.zoikosuite.accounting.reconciliation-run.completed", "CONTROL.ReconciliationCompleted", aggregateRun}
	evRunVarianceFound   = eventKind{"com.zoikosuite.accounting.reconciliation-run.variance-found", "CONTROL.ExceptionRaised", aggregateRun}
	evRunReperformed     = eventKind{"com.zoikosuite.accounting.reconciliation-run.reperformed", "CONTROL.ReconciliationCompleted", aggregateRun}
	evRunSuperseded      = eventKind{"com.zoikosuite.accounting.reconciliation-run.superseded", "CONTROL.ReconciliationSuperseded", aggregateRun}
	evBreakDetected      = eventKind{"com.zoikosuite.accounting.reconciliation-break.detected", "CONTROL.ExceptionRaised", aggregateBreak}
	evBreakAssigned      = eventKind{"com.zoikosuite.accounting.reconciliation-break.assigned", "CONTROL.ExceptionUpdated", aggregateBreak}
	evBreakStatusChanged = eventKind{"com.zoikosuite.accounting.reconciliation-break.status-changed", "CONTROL.ExceptionUpdated", aggregateBreak}
	evCertIssued         = eventKind{"com.zoikosuite.accounting.reconciliation-certification.issued", "CONTROL.Certified", aggregateCertification}
	evCertRevoked        = eventKind{"com.zoikosuite.accounting.reconciliation-certification.revoked", "CONTROL.CertificationRevoked", aggregateCertification}
	evRulesPublished     = eventKind{"com.zoikosuite.accounting.reconciliation-definition.rule-version-published", "CONTROL.RuleVersionPublished", aggregateDefinition}
	evRulesApproved      = eventKind{"com.zoikosuite.accounting.reconciliation-definition.rule-version-approved", "CONTROL.RuleVersionApproved", aggregateDefinition}
)

// Option configures a PgStore.
type Option func(*PgStore)

// WithEventRegion sets the residency region stamped on every event
// (EVENT_RESIDENCY_REGION). Without it every write fails at enqueue time —
// an event must never leave with a made-up region.
func WithEventRegion(region string) Option {
	return func(s *PgStore) { s.eventRegion = region }
}

// enqueueEvent builds the canonical envelope for one write and inserts it on
// tx — one INSERT, no extra round-trip. tenantID is the transaction's own
// tenant (the one withTenant declared for RLS); legalEntityID is the entity
// the record belongs to, empty only for records that predate entity scoping.
// The actor and correlation come from the request that made the write.
func (s *PgStore) enqueueEvent(ctx context.Context, tx pgx.Tx, kind eventKind, tenantID, legalEntityID, aggregateID, actor string,
	claim domain.IdempotencyClaim, data any) error {
	env, err := envelope.New(envelope.Spec{
		Type:            kind.Type,
		LegacyType:      kind.LegacyType,
		Service:         eventService,
		SchemaVersion:   "1.0.0",
		OccurredAt:      time.Now().UTC(),
		TenantID:        tenantID,
		LegalEntityID:   legalEntityID,
		AggregateType:   kind.Aggregate,
		AggregateID:     aggregateID,
		CorrelationID:   claim.CorrelationID,
		ActorID:         actor,
		ResidencyRegion: s.eventRegion,
		Classification:  envelope.Confidential,
		Data:            data,
	})
	if err != nil {
		return fmt.Errorf("build %s event: %w", kind.Type, err)
	}
	return outbox.Enqueue(ctx, tx, env)
}
