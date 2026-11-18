package events

// ORG-09 / MDM-03 identifier verification event (ZS-SVC-U-001 s20.1: identifier_verified).
//
// It carries identifiers and the state move, never the identifier's value: an identifier can be sensitive and an event
// reaches every subscriber. A conflict opened by a higher-authority disagreement is published as master.conflict.opened
// (ConflictOpenedRecord), as for every other stewardship case.

import (
	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/outbox"
)

// EventPartyIdentifierVerified is published when an observation verified an identifier or moved it to a higher
// authority.
const EventPartyIdentifierVerified = "party.identifier.verified" // identifier_verified

// PartyIdentifierVerifiedRecord is party.identifier.verified.
func PartyIdentifierVerifiedRecord(p domain.Party, i domain.PartyIdentifier, o domain.IdentifierObservation, actor, corr string) (*outbox.Record, error) {
	spec := partySpec(EventPartyIdentifierVerified, p, actor, corr, map[string]any{
		"identifier_id": i.IdentifierID, "scheme": i.Scheme, "scope": i.Scope, "verification_status": i.VerificationStatus,
		"effect": o.Effect, "source_class": o.SourceClass, "source_ref": o.SourceRef, "source_version": o.SourceVersion,
		"observation_id": o.ObservationID, "observed_at": o.ObservedAt,
	})
	spec.EvidenceRef = o.EvidenceRef
	spec.EffectiveAt = o.ObservedAt
	return BuildRecord(spec)
}
