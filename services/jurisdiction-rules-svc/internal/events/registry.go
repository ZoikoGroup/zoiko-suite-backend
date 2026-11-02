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

// PublishRegistryEvent implements RegistryPublisher. The event goes to the
// transactional outbox (the OutboxWorker relays it to Kafka), so a broker
// outage cannot lose it. aggregate_id keys the partition so all events about
// one record stay ordered.
func (p *OutboxPublisher) PublishRegistryEvent(ctx context.Context, eventType, aggregateID, actorID, correlationID string, payload map[string]any) error {
	body := make(map[string]any, len(payload)+1)
	for k, v := range payload {
		body[k] = v
	}
	body["aggregate_id"] = aggregateID
	jurisdictionID, _ := payload["jurisdiction_id"].(string)
	return p.emitToOutbox(ctx, p.store.Pool(), eventType, correlationID, jurisdictionID, actorID, body)
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

// ZS-JUR-001 s26 events for Waves 4, 5 and 8. jurisdiction.support-changed is
// the registry family name s26 gives; the filing and einvoice names are the s26
// "External compliance" family. The obligation events s26 also lists, `created`
// and `overdue`, need per-entity obligation instances (not built) and are not
// emitted: only `due-date-calculated` is a fact this service produces.
const (
	EventRolloutCreated            = "jurisdiction-rollout.created"
	EventRolloutStatusChanged      = "jurisdiction-rollout.status-changed"
	EventRolloutAttested           = "jurisdiction-rollout.attested"
	EventRolloutExpertDecided      = "jurisdiction-rollout.expert-decided"
	EventJurisdictionSupportChange = "jurisdiction.support-changed"

	EventFilingSubmitted    = "filing.submitted"
	EventFilingAccepted     = "filing.accepted"
	EventFilingRejected     = "filing.rejected"
	EventFilingStatus       = "filing.status-changed"
	EventEInvoiceSubmitted  = "einvoice.submitted"
	EventEInvoiceAccepted   = "einvoice.accepted"
	EventEInvoiceRejected   = "einvoice.rejected"
	EventEInvoiceStatus     = "einvoice.status-changed"
	EventObligationDueDated = "regulatory-obligation.due-date-calculated"
)

// SubmissionEventType names the s26 event for a submission of the given kind
// (FILING or EINVOICE) that has just reached status. Lifecycles are data on the
// profile, so only the conventional receipt states map to accepted/rejected;
// every other applied status is reported as status-changed.
func SubmissionEventType(kind, status string) string {
	filing := kind == "FILING"
	pick := func(f, e string) string {
		if filing {
			return f
		}
		return e
	}
	switch status {
	case "ACCEPTED", "CLEARED", "FILED":
		return pick(EventFilingAccepted, EventEInvoiceAccepted)
	case "REJECTED":
		return pick(EventFilingRejected, EventEInvoiceRejected)
	}
	return pick(EventFilingStatus, EventEInvoiceStatus)
}

// SubmissionRegisteredEventType is the s26 event for a newly registered submission.
func SubmissionRegisteredEventType(kind string) string {
	if kind == "FILING" {
		return EventFilingSubmitted
	}
	return EventEInvoiceSubmitted
}

// ZS-JUR-001 s26 "Runtime decisions" family: a jurisdiction determination was made (s9).
const EventJurisdictionDecisionResolved = "jurisdiction-decision.resolved"

// ZS-JUR-001 s23: a RELEASED version was marked SUPERSEDED by a newer one (historical resolution stays permitted).
const EventPackSuperseded = "jurisdiction-pack.superseded"

// ZS-JUR-001 s23, s31: a remediation case was opened for decisions made with a withdrawn or rolled-back pack version, or closed once every one was
// answered for. Opened is announced for automatic cases (withdrawal, rollback) as well as manual ones.
const (
	EventRemediationOpened = "jurisdiction-remediation.opened"
	EventRemediationClosed = "jurisdiction-remediation.closed"
)
