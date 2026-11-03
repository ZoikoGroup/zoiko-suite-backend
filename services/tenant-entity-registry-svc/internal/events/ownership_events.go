package events

// ORG-04 events (ZS-SVC-I-001 s4.4): OwnershipRelationshipActivated,
// OwnershipRelationshipSuperseded and ControlRelationshipChanged, plus the
// workflow events that surround them.
//
// Spec names are not wire names. The platform convention is lower.dotted, so
// the spec's three events map as below; the mapping is written in asyncapi.yaml
// and asserted by contract_test.go.
//
// OWNERSHIP vs CONTROL. The spec produces separate events for the two: an
// ownership relationship is Activated or Superseded, a control relationship is
// "Changed". Every state change of a CONTROL relationship (activated,
// superseded, ended) is therefore control.relationship.changed, with the change
// in `change_type`. Proposals and rejections are workflow facts, not changes to
// the group structure, and use the same names for both kinds.
//
// Beneficial notes are never in an event. They are sensitive (s4.4) and an
// event reaches every subscriber; a consumer that needs them reads the
// relationship through the permissioned API.

import (
	"fmt"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/outbox"
)

const (
	EventOwnershipProposed   = "ownership.relationship.proposed"   // (workflow; no spec name)
	EventOwnershipActivated  = "ownership.relationship.activated"  // OwnershipRelationshipActivated
	EventOwnershipSuperseded = "ownership.relationship.superseded" // OwnershipRelationshipSuperseded
	EventOwnershipEnded      = "ownership.relationship.ended"      // (EndRelationship; the spec names no event)
	EventOwnershipRejected   = "ownership.relationship.rejected"   // (workflow; no spec name)
	EventControlChanged      = "control.relationship.changed"      // ControlRelationshipChanged
)

// OwnershipEventNames is every ORG-04 wire name the code can emit.
var OwnershipEventNames = []string{
	EventOwnershipProposed, EventOwnershipActivated, EventOwnershipSuperseded,
	EventOwnershipEnded, EventOwnershipRejected, EventControlChanged,
}

// OwnershipSpecEvent maps an ORG-04 wire name to the event name s4.4 gives it;
// "" for the events the spec does not name.
var OwnershipSpecEvent = map[string]string{
	EventOwnershipProposed:   "",
	EventOwnershipActivated:  "OwnershipRelationshipActivated",
	EventOwnershipSuperseded: "OwnershipRelationshipSuperseded",
	EventOwnershipEnded:      "",
	EventOwnershipRejected:   "",
	EventControlChanged:      "ControlRelationshipChanged",
}

// Change types carried in the payload of control.relationship.changed (and, for
// uniformity, of every ORG-04 event).
const (
	OwnershipChangeProposed   = "PROPOSED"
	OwnershipChangeActivated  = "ACTIVATED"
	OwnershipChangeSuperseded = "SUPERSEDED"
	OwnershipChangeEnded      = "ENDED"
	OwnershipChangeRejected   = "REJECTED"
)

// OwnershipRecord renders the outbox record for an ORG-04 write. It accepts the
// row the write produced (*domain.OwnershipRelationship, whose status says what
// happened) or a domain.OwnershipSupersession for the relationship that a
// supersession retired. Anything else renders nothing, so a builder attached to
// one write does not fire for another.
func OwnershipRecord(result any) (*outbox.Record, error) {
	switch v := result.(type) {
	case *domain.OwnershipRelationship:
		return ownershipRecordFor(v, nil)
	case domain.OwnershipSupersession:
		return ownershipRecordFor(v.Previous, v.Replacement)
	}
	return nil, nil
}

func ownershipRecordFor(r, replacement *domain.OwnershipRelationship) (*outbox.Record, error) {
	if r == nil {
		return nil, fmt.Errorf("events: ownership event for a nil relationship")
	}
	change := ownershipChange(r.Status)
	if change == "" {
		return nil, fmt.Errorf("events: no ORG-04 event for a relationship in status %s", r.Status)
	}

	spec := RecordSpec{
		TenantID:      r.TenantID,
		LegalEntityID: r.ChildLegalEntityID,
		CorrelationID: r.CorrelationID,
		ObjectID:      r.RelationshipID,
		ObjectVersion: max64(r.RecordVersion, 1),
		// Ordering per child entity: everything about who owns an entity is in
		// one partition, so a consumer sees proposed before activated before ended.
		PartitionKey: r.ChildLegalEntityID,
	}
	switch r.Status {
	case domain.OwnershipProposed:
		spec.ActorID, spec.EffectiveAt, spec.RecordedAt, spec.EvidenceRef = r.ProposedByPrincipalID, r.ValidFrom, r.ProposedAt, r.EvidenceRef
	case domain.OwnershipActive, domain.OwnershipRejected:
		spec.ActorID, spec.EvidenceRef = deref(r.DecidedByPrincipalID), r.EvidenceRef
		spec.EffectiveAt = r.ValidFrom
		if r.Status == domain.OwnershipRejected && r.DecidedAt != nil {
			spec.EffectiveAt = *r.DecidedAt
		}
		if r.DecidedAt != nil {
			spec.RecordedAt = *r.DecidedAt
		}
	case domain.OwnershipEnded:
		spec.ActorID, spec.EvidenceRef = deref(r.EndedByPrincipalID), deref(r.EndEvidenceRef)
		if r.ValidTo != nil {
			spec.EffectiveAt = *r.ValidTo
		}
		if r.EndedAt != nil {
			spec.RecordedAt = *r.EndedAt
		}
	case domain.OwnershipSuperseded:
		if replacement == nil {
			return nil, fmt.Errorf("events: a superseded relationship needs its replacement")
		}
		// The replacement is what did it: its approver is the actor, its start is
		// when the original stops, its evidence is the evidence.
		spec.ActorID, spec.EvidenceRef = deref(replacement.DecidedByPrincipalID), replacement.EvidenceRef
		spec.EffectiveAt = replacement.ValidFrom
		spec.CorrelationID = replacement.CorrelationID
		if replacement.DecidedAt != nil {
			spec.RecordedAt = *replacement.DecidedAt
		}
	}
	spec.EventType = ownershipEventType(r.Status, r.RelationshipKind)

	payload := map[string]any{
		"tenant_id":              r.TenantID,
		"relationship_id":        r.RelationshipID,
		"parent_legal_entity_id": r.ParentLegalEntityID,
		"child_legal_entity_id":  r.ChildLegalEntityID,
		"relationship_kind":      r.RelationshipKind,
		"ownership_pct":          r.OwnershipPct,
		"voting_pct":             r.VotingPct,
		"control_type":           r.ControlType,
		"status":                 r.Status,
		"change_type":            change,
		"valid_from":             r.ValidFrom,
		"valid_to":               r.ValidTo,
	}
	if r.SupersedesRelationshipID != nil {
		payload["supersedes_relationship_id"] = *r.SupersedesRelationshipID
	}
	if replacement != nil {
		payload["replaced_by_relationship_id"] = replacement.RelationshipID
	}
	spec.Payload = payload
	return BuildRecord(spec)
}

func ownershipChange(s domain.OwnershipStatus) string {
	switch s {
	case domain.OwnershipProposed:
		return OwnershipChangeProposed
	case domain.OwnershipActive:
		return OwnershipChangeActivated
	case domain.OwnershipSuperseded:
		return OwnershipChangeSuperseded
	case domain.OwnershipEnded:
		return OwnershipChangeEnded
	case domain.OwnershipRejected:
		return OwnershipChangeRejected
	}
	return ""
}

// ownershipEventType is the wire name for a relationship's state change.
func ownershipEventType(s domain.OwnershipStatus, kind domain.RelationshipKind) string {
	switch s {
	case domain.OwnershipProposed:
		return EventOwnershipProposed
	case domain.OwnershipRejected:
		return EventOwnershipRejected
	}
	if kind == domain.RelationshipControl {
		return EventControlChanged
	}
	switch s {
	case domain.OwnershipActive:
		return EventOwnershipActivated
	case domain.OwnershipSuperseded:
		return EventOwnershipSuperseded
	case domain.OwnershipEnded:
		return EventOwnershipEnded
	}
	return ""
}
