package events

// ORG-08 establishment events (ZS-SVC-I-001 s4.8: EstablishmentActivated, EstablishmentClosed).
//
// They carry identifiers, the type and the status move, never the establishment's name or address. Tax and legal
// consumers decide what a presence means; this service states only that a presence was recorded, activated or closed.

import (
	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/outbox"
)

const (
	EventEstablishmentCreated     = "establishment.created"     // (no spec name)
	EventEstablishmentActivated   = "establishment.activated"   // EstablishmentActivated
	EventEstablishmentDeactivated = "establishment.deactivated" // (no spec name)
	EventEstablishmentClosed      = "establishment.closed"      // EstablishmentClosed
)

// EstablishmentEventNames is every establishment wire name the code can emit.
var EstablishmentEventNames = []string{EventEstablishmentCreated, EventEstablishmentActivated, EventEstablishmentDeactivated, EventEstablishmentClosed}

// EstablishmentStatusEvent is the event name for a move to status to.
func EstablishmentStatusEvent(to domain.EstablishmentStatus) string {
	switch to {
	case domain.EstActive:
		return EventEstablishmentActivated
	case domain.EstInactive:
		return EventEstablishmentDeactivated
	case domain.EstClosed:
		return EventEstablishmentClosed
	}
	return ""
}

func establishmentSpec(eventType string, e domain.Establishment, actor, corr, evidence, effective string, payload map[string]any) RecordSpec {
	payload["tenant_id"] = e.TenantID
	payload["establishment_id"] = e.EstablishmentID
	payload["legal_entity_id"] = e.LegalEntityID
	payload["establishment_type"] = e.EstablishmentType
	payload["address_id"] = e.AddressID
	payload["status"] = e.Status
	payload["material"] = e.Material
	payload["effective_date"] = effective
	return RecordSpec{
		EventType: eventType, TenantID: e.TenantID, LegalEntityID: e.LegalEntityID, ActorID: actor, CorrelationID: corr,
		ObjectID: e.EstablishmentID, ObjectVersion: e.RecordVersion, PartitionKey: e.EstablishmentID,
		EvidenceRef: evidence, Payload: payload,
	}
}

// EstablishmentCreatedRecord is establishment.created.
func EstablishmentCreatedRecord(e domain.Establishment, corr string) (*outbox.Record, error) {
	return BuildRecord(establishmentSpec(EventEstablishmentCreated, e, e.CreatedBy, corr, e.EvidenceRef, e.ValidFrom,
		map[string]any{"source_class": e.SourceClass}))
}

// EstablishmentStatusRecord is establishment.activated, .deactivated or .closed.
func EstablishmentStatusRecord(e domain.Establishment, t domain.EstablishmentTransition, corr string) (*outbox.Record, error) {
	return BuildRecord(establishmentSpec(EstablishmentStatusEvent(t.To), e, t.Actor, corr, t.EvidenceRef, t.EffectiveDate,
		map[string]any{"from_status": t.From, "to_status": t.To, "command": t.Command}))
}
