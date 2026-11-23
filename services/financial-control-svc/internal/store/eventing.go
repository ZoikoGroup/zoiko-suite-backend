package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"zoiko.io/eventing/envelope"
	"zoiko.io/eventing/outbox"

	"zoiko.io/financial-control-svc/internal/domain"
)

// Events are written through the shared transactional outbox
// (services/eventing, ZS-EVENT-001 §6): enqueueEvent inserts one
// eventing_outbox row on the caller's transaction — the same one that applies
// the state change and its transition history — and the shared relay
// (cmd/server) delivers it. A control fact is never durable without its event
// nor announced without being durable.

// Event types as ZS-CONTROL-001 §26 ("Domain Event Contract") names them, plus
// the service's additions marked below. These are the LEGACY event_type every
// existing consumer filters on; the canonical type is derived by canonicalType.
const (
	RunCreated             = "control.run.created"
	PopulationFrozen       = "control.population.frozen"
	ExecutionCompleted     = "control.execution.completed"
	ExceptionOpened        = "control.exception.opened"
	ExceptionAssigned      = "control.exception.assigned"
	RemediationRecorded    = "control.exception.remediation_recorded"
	RunReperformed         = "control.run.reperformed"
	RunReadyForCert        = "control.run.ready_for_certification"
	RunCertified           = "control.run.certified"
	RunCertificationReject = "control.run.certification_rejected"
	RunFailed              = "control.run.failed"
	CloseGateBlocked       = "control.close_gate.blocked"
	CloseGateSatisfied     = "control.close_gate.satisfied"
	ExceptionSLABreached   = "control.exception.sla_breached"

	// Not in §26 — service additions.
	// RunCertificationRevoked: a certified run's certification was revoked
	// (prospectively; ZS-SVC-Z-001 RB-05).
	RunCertificationRevoked = "control.run.certification_revoked"
	ExceptionWaived         = "control.exception.waived"
	ExceptionCarried        = "control.exception.carried_forward"
	ExceptionReperformed    = "control.exception.reperformed"
	ExceptionClosed         = "control.exception.closed"
	RunSuperseded           = "control.run.superseded"
	RunExpired              = "control.run.expired"
)

// canonicalType is the ZS-EVENT-001 §5.1 type for a §26 name:
// com.zoikosuite.<domain>.<aggregate>.<past-tense-fact>, keeping §26's own
// domain ("control"), aggregate and fact words and only moving them into the
// standard's lower-case kebab form. The Event Catalogue (§19) lists no family
// for this producer, so §26 is the authority for the words themselves.
func canonicalType(legacy string) string {
	return "com.zoikosuite." + strings.ReplaceAll(legacy, "_", "-")
}

// Aggregates the events are keyed on (envelope subject and partition key).
const (
	aggregateRun       = "control-run"
	aggregateException = "control-exception"
)

// Option configures a PgStore.
type Option func(*PgStore)

// WithEventProfile sets the governed envelope attributes stamped on every
// event: residency region (EVENT_RESIDENCY_REGION) and data classification
// (EVENT_CLASSIFICATION). Both are per-deployment decisions this service does
// not make; without them every write fails at enqueue time rather than
// publishing an event with an invented value.
func WithEventProfile(region string, classification envelope.Classification) Option {
	return func(s *PgStore) { s.eventRegion, s.eventClassification = region, classification }
}

// enqueueEvent builds the canonical envelope for one control fact and inserts
// it on tx — one INSERT, no extra round-trip. Tenant and legal entity come
// from the run the fact belongs to; actor and correlation from the command
// that caused it.
func (s *PgStore) enqueueEvent(ctx context.Context, tx pgx.Tx, legacyType, aggregateType, aggregateID string, aggregateVersion *int64,
	r domain.ControlRun, actor, correlationID string, payload map[string]any) error {
	now := time.Now().UTC()
	// Kept in the data for consumers that read it there, as before.
	payload["occurred_at"] = now.Format(time.RFC3339Nano)
	env, err := envelope.New(envelope.Spec{
		Type:             canonicalType(legacyType),
		LegacyType:       legacyType,
		Service:          "financial-control-svc",
		SchemaVersion:    "1.0.0",
		OccurredAt:       now,
		TenantID:         r.TenantID,
		LegalEntityID:    r.LegalEntityID,
		AggregateType:    aggregateType,
		AggregateID:      aggregateID,
		AggregateVersion: aggregateVersion,
		CorrelationID:    correlationID,
		ActorID:          actor,
		ResidencyRegion:  s.eventRegion,
		Classification:   s.eventClassification,
		Data:             payload,
	})
	if err != nil {
		return fmt.Errorf("build %s event: %w", legacyType, err)
	}
	return outbox.Enqueue(ctx, tx, env)
}

// emit enqueues a run event, versioned with the run's post-change version
// (the old publisher read the same value from the payload's "version").
func (s *PgStore) emit(ctx context.Context, tx pgx.Tx, r domain.ControlRun, eventType, actor, correlationID string, payload map[string]any) error {
	v := int64(r.Version)
	return s.enqueueEvent(ctx, tx, eventType, aggregateRun, r.RunID, &v, r, actor, correlationID, payload)
}

// emitException enqueues an exception event, keyed on the exception.
func (s *PgStore) emitException(ctx context.Context, tx pgx.Tx, r domain.ControlRun, exceptionID, eventType, actor, correlationID string,
	payload map[string]any) error {
	return s.enqueueEvent(ctx, tx, eventType, aggregateException, exceptionID, nil, r, actor, correlationID, payload)
}
