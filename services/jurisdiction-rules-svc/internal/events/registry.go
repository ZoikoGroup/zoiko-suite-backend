package events

import (
	"context"

	"go.uber.org/zap"
)

// ZS-JUR-001 s26 registry event families. Published as facts after the
// write commits, in the same envelope as every other event of this service.
const (
	EventRegimeCreated          = "regulatory-regime.created"
	EventSourceCaptured         = "regulatory-source.captured"
	EventSourceReviewed         = "regulatory-source.reviewed"
	EventSourceSuperseded       = "regulatory-source.superseded"
	EventInterpretationRecorded = "regulatory-interpretation.recorded"
	EventInterpretationApproved = "regulatory-interpretation.approved"
	EventPackCreated            = "jurisdiction-pack.created"
	EventPackVersionDrafted     = "jurisdiction-pack.version-drafted"
	EventPackVersionSubmitted   = "jurisdiction-pack.version-submitted"
	EventRuleProvenanceSet      = "regulatory-rule.provenance-set"
)

// RegistryPublisher emits the registry events. It is a separate interface so
// the original Publisher contract (and its test doubles) is untouched.
type RegistryPublisher interface {
	PublishRegistryEvent(ctx context.Context, eventType, aggregateID, actorID, correlationID string, payload map[string]any) error
}

// PublishRegistryEvent implements RegistryPublisher. aggregate_id keys the
// Kafka partition so all events about one record stay ordered.
func (p *KafkaPublisher) PublishRegistryEvent(ctx context.Context, eventType, aggregateID, actorID, correlationID string, payload map[string]any) error {
	body := make(map[string]any, len(payload)+1)
	for k, v := range payload {
		body[k] = v
	}
	body["aggregate_id"] = aggregateID
	jurisdictionID, _ := payload["jurisdiction_id"].(string)
	return p.emit(ctx, eventType, correlationID, jurisdictionID, actorID, body)
}

// PublishRegistryEvent implements RegistryPublisher by dropping the event.
func (p *NoopPublisher) PublishRegistryEvent(_ context.Context, eventType, _, _, _ string, _ map[string]any) error {
	p.log.Debug("registry event dropped — no Kafka brokers configured", zap.String("event_type", eventType))
	return nil
}

// ZS-JUR-001 Wave 1 events.
const (
	EventPackVersionCompiled    = "jurisdiction-pack.version-compiled"
	EventPackVersionSigned      = "jurisdiction-pack.version-signed"
	EventPackVerificationFailed = "jurisdiction-pack.verification-failed"
	EventSigningKeyRegistered   = "pack-signing-key.registered"
	EventSigningKeyRetired      = "pack-signing-key.retired"
	EventSigningKeyRevoked      = "pack-signing-key.revoked"
)

// ZS-JUR-001 Wave 3 events.
const (
	EventPackTestsRun  = "jurisdiction-pack.tests-run"
	EventPackReviewed  = "jurisdiction-pack.reviewed"
	EventPackCertified = "jurisdiction-pack.certified"
)

// ZS-JUR-001 Wave 7 events (s26 pack lifecycle, obligations and source families).
const (
	EventPackReleased         = "jurisdiction-pack.released"
	EventPackWithdrawn        = "jurisdiction-pack.withdrawn"
	EventPackBlocked          = "jurisdiction-pack.emergency-blocked"
	EventPackUnblocked        = "jurisdiction-pack.unblocked"
	EventPackPromoted         = "jurisdiction-pack.promoted"
	EventPackRolledBack       = "jurisdiction-pack.rolled-back"
	EventHotfixDeclared       = "jurisdiction-pack.hotfix-declared"
	EventHotfixRetrospective  = "jurisdiction-pack.hotfix-retrospective-completed"
	EventSourceChangeCaptured = "regulatory-source-change.captured"
	EventSourceChangeInReview = "regulatory-source-change.review-started"
	EventSourceChangeClosed   = "regulatory-source-change.closed"
)

// ZS-JUR-001 Wave 4 (calendar half) events. jurisdiction.calendar.changed is
// the name the service's original specification (03-microservices.md s8.2)
// reserved for this signal.
const (
	EventCalendarChanged         = "jurisdiction.calendar.changed"
	EventObligationRulePublished = "regulatory-obligation-rule.published"
)
