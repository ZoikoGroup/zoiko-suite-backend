package events

// ORG-08 address events (ZS-SVC-I-001 s4.8: AddressVerified, AddressSuperseded).
//
// An event carries identifiers and states, never the address: an address of a natural person is personal data and
// an event reaches every subscriber. A consumer that needs the address reads it through the permissioned API.

import (
	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/outbox"
)

const (
	EventAddressCreated    = "address.created"    // (no spec name)
	EventAddressVerified   = "address.verified"   // AddressVerified
	EventAddressSuperseded = "address.superseded" // AddressSuperseded
	EventAddressLinked     = "address.linked"     // LinkAddressToParty
)

// AddressEventNames is every address wire name the code can emit.
var AddressEventNames = []string{EventAddressCreated, EventAddressVerified, EventAddressSuperseded, EventAddressLinked}

func addressSpec(eventType string, a domain.Address, v domain.AddressVersion, actor, corr string, payload map[string]any) RecordSpec {
	payload["tenant_id"] = a.TenantID
	payload["address_id"] = a.AddressID
	payload["address_type"] = a.AddressType
	payload["version_no"] = v.VersionNo
	return RecordSpec{
		EventType: eventType, TenantID: a.TenantID, ActorID: actor, CorrelationID: corr,
		ObjectID: a.AddressID, ObjectVersion: int64(v.VersionNo), PartitionKey: a.AddressID,
		EvidenceRef: v.EvidenceRef, Payload: payload,
	}
}

// AddressCreatedRecord is address.created.
func AddressCreatedRecord(a domain.Address, v domain.AddressVersion, corr string) (*outbox.Record, error) {
	return BuildRecord(addressSpec(EventAddressCreated, a, v, a.CreatedBy, corr, map[string]any{
		"country": v.Country, "valid_from": v.ValidFrom, "source_class": v.SourceClass,
	}))
}

// AddressVerifiedRecord is address.verified: a version reached VERIFIED. It says an observation passed, not that
// the address is legal or current.
func AddressVerifiedRecord(a domain.Address, v domain.AddressVersion, o domain.AddressObservation, actor, corr string) (*outbox.Record, error) {
	return BuildRecord(addressSpec(EventAddressVerified, a, v, actor, corr, map[string]any{
		"verifier": o.Verifier, "result": o.Result, "confidence": o.Confidence,
	}))
}

// AddressSupersededRecord is address.superseded: a version was replaced by the next.
func AddressSupersededRecord(a domain.Address, old, next domain.AddressVersion, actor, corr string) (*outbox.Record, error) {
	return BuildRecord(addressSpec(EventAddressSuperseded, a, next, actor, corr, map[string]any{
		"superseded_version_no": old.VersionNo, "valid_from": next.ValidFrom,
	}))
}

// AddressLinkedRecord is address.linked.
func AddressLinkedRecord(a domain.Address, v domain.AddressVersion, l domain.AddressLink, actor, corr string) (*outbox.Record, error) {
	spec := addressSpec(EventAddressLinked, a, v, actor, corr, map[string]any{
		"party_id": l.PartyID, "link_id": l.LinkID, "link_type": l.LinkType, "valid_from": l.ValidFrom,
	})
	spec.EvidenceRef = l.EvidenceRef
	return BuildRecord(spec)
}
