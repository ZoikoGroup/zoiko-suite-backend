package events

// ORG-09 registration events (ZS-SVC-I-001 s4.9: RegistrationVerified, RegistrationRenewed, RegistrationExpiring,
// RegistrationExpired, RegistrationRevoked).
//
// They carry identifiers, the type, jurisdiction, status move and dates, never the registration number: a tax or
// payroll number is sensitive and an event reaches every subscriber. A consumer that needs it reads the registration
// through the permissioned API. They state what an authority was recorded as saying; they conclude nothing about tax.

import (
	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/outbox"
)

const (
	EventRegistrationCreated    = "registration.created"    // (no spec name)
	EventRegistrationVerified   = "registration.verified"   // RegistrationVerified
	EventRegistrationRenewed    = "registration.renewed"    // RegistrationRenewed
	EventRegistrationSuspended  = "registration.suspended"  // (no spec name)
	EventRegistrationExpiring   = "registration.expiring"   // RegistrationExpiring
	EventRegistrationExpired    = "registration.expired"    // RegistrationExpired
	EventRegistrationRevoked    = "registration.revoked"    // RegistrationRevoked
	EventRegistrationSuperseded = "registration.superseded" // (no spec name)
)

// RegistrationEventNames is every registration wire name the code can emit.
var RegistrationEventNames = []string{
	EventRegistrationCreated, EventRegistrationVerified, EventRegistrationRenewed, EventRegistrationSuspended,
	EventRegistrationExpiring, EventRegistrationExpired, EventRegistrationRevoked, EventRegistrationSuperseded,
}

// RegistrationStatusEvent is the event for a registration moving to status to by command.
func RegistrationStatusEvent(command string, to domain.RegistrationStatus) string {
	switch {
	case command == "RenewRegistration":
		return EventRegistrationRenewed
	case to == domain.RegActive:
		return EventRegistrationVerified
	case to == domain.RegSuspended:
		return EventRegistrationSuspended
	case to == domain.RegExpired:
		return EventRegistrationExpired
	case to == domain.RegRevoked:
		return EventRegistrationRevoked
	case to == domain.RegSuperseded:
		return EventRegistrationSuperseded
	}
	return ""
}

func registrationSpec(eventType string, r domain.Registration, actor, corr, evidence, effective string, payload map[string]any) RecordSpec {
	payload["tenant_id"] = r.TenantID
	payload["registration_id"] = r.RegistrationID
	payload["registration_type"] = r.RegistrationType
	payload["jurisdiction"] = r.Jurisdiction
	payload["status"] = r.Status
	payload["material"] = r.Material
	payload["expiry_date"] = r.ExpiryDate
	payload["effective_date"] = effective
	if r.HolderLegalEntityID != "" {
		payload["holder_legal_entity_id"] = r.HolderLegalEntityID
	}
	if r.HolderPartyID != "" {
		payload["holder_party_id"] = r.HolderPartyID
	}
	return RecordSpec{
		EventType: eventType, TenantID: r.TenantID, LegalEntityID: r.HolderLegalEntityID, ActorID: actor, CorrelationID: corr,
		ObjectID: r.RegistrationID, ObjectVersion: r.RecordVersion, PartitionKey: r.RegistrationID,
		EvidenceRef: evidence, Payload: payload,
	}
}

// RegistrationCreatedRecord is registration.created.
func RegistrationCreatedRecord(r domain.Registration, corr string) (*outbox.Record, error) {
	return BuildRecord(registrationSpec(EventRegistrationCreated, r, r.CreatedBy, corr, r.EvidenceRef, r.EffectiveFrom,
		map[string]any{"source_class": r.SourceClass}))
}

// RegistrationStatusRecord is the event for a transition: verified, renewed, suspended, expired, revoked or
// superseded.
func RegistrationStatusRecord(r domain.Registration, t domain.RegistrationTransition, corr string) (*outbox.Record, error) {
	return BuildRecord(registrationSpec(RegistrationStatusEvent(t.Command, t.To), r, t.Actor, corr, t.EvidenceRef, t.EffectiveDate,
		map[string]any{"from_status": t.From, "to_status": t.To, "command": t.Command}))
}

// RegistrationExpiringRecord is registration.expiring: a warning, from the sweep, that an active registration ends
// on its expiry date unless renewed.
func RegistrationExpiringRecord(r domain.Registration, daysLeft int, actor string) (*outbox.Record, error) {
	return BuildRecord(registrationSpec(EventRegistrationExpiring, r, actor, "", r.EvidenceRef, r.ExpiryDate,
		map[string]any{"days_left": daysLeft}))
}
