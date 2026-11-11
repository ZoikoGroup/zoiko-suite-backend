package events

// MDM-01 party contact point events. They carry identifiers, the channel, purpose, status, validity and the sha256
// digest of the address, never the address itself: a consumer that needs to reach the party reads the contact point
// through the permissioned read.

import (
	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/outbox"
)

const (
	EventContactPointProposed = "party.contact_point.proposed" // (no spec name)
	EventContactPointVerified = "party.contact_point.verified" // (no spec name)
	EventContactPointRetired  = "party.contact_point.retired"  // (no spec name)
)

// ContactPointEventNames is every contact point wire name the code can emit.
var ContactPointEventNames = []string{EventContactPointProposed, EventContactPointVerified, EventContactPointRetired}

// ContactPointRecord is the event of the given type for the contact point as it now stands. Its value is not read.
func ContactPointRecord(eventType string, c domain.ContactPoint, actor, corr string) (*outbox.Record, error) {
	payload := map[string]any{
		"tenant_id": c.TenantID, "party_id": c.PartyID, "contact_point_id": c.ContactPointID, "channel": c.Channel,
		"purpose": c.Purpose, "status": c.Status, "value_digest": c.ValueDigest, "valid_from": c.ValidFrom, "valid_to": c.ValidTo,
	}
	return BuildRecord(RecordSpec{
		EventType: eventType, TenantID: c.TenantID, ActorID: actor, CorrelationID: corr,
		ObjectID: c.ContactPointID, ObjectVersion: c.RecordVersion, PartitionKey: c.PartyID,
		EvidenceRef: c.EvidenceRef, Payload: payload,
	})
}
