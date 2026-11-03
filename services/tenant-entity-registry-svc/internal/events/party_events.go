package events

// ORG-07 / MDM-01 party events (ZS-SVC-I-001 s4.7: PartyCreated, PartyProfileAmended, PartyRoleAdded;
// ZS-SVC-U-001 s20.1: party_created, party_fact_published, identifier_verified, master_conflict_opened).
//
// The platform convention is lower.dotted, so the spec names map as in asyncapi.yaml (x-spec-event), and
// contract_test.go holds the two together.
//
// An event carries identifiers and decisions, never a value: no name, no identifier value, no evidence text. A
// natural person's name is personal data and an identifier may be sensitive; an event reaches every subscriber.
// A consumer that needs the value reads the party through the permissioned API.

import (
	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/outbox"
)

const (
	EventPartyCreated          = "party.created"           // PartyCreated / party_created
	EventPartyProfileAmended   = "party.profile.amended"   // PartyProfileAmended
	EventPartyFactPublished    = "party.fact.published"    // party_fact_published
	EventPartyRoleAdded        = "party.role.added"        // PartyRoleAdded
	EventPartyRoleRemoved      = "party.role.removed"      // (no spec name)
	EventPartyIdentifierLinked = "party.identifier.linked" // LinkExternalIdentifier
	EventMasterConflictOpened  = "master.conflict.opened"  // master_conflict_opened
)

// PartyEventNames is every party wire name the code can emit.
var PartyEventNames = []string{
	EventPartyCreated, EventPartyProfileAmended, EventPartyFactPublished, EventPartyRoleAdded,
	EventPartyRoleRemoved, EventPartyIdentifierLinked, EventMasterConflictOpened,
}

func partySpec(eventType string, p domain.Party, actor, corr string, payload map[string]any) RecordSpec {
	payload["tenant_id"] = p.TenantID
	payload["party_id"] = p.PartyID
	payload["master_version"] = p.MasterVersion
	return RecordSpec{
		EventType: eventType, TenantID: p.TenantID, ActorID: actor, CorrelationID: corr,
		ObjectID: p.PartyID, ObjectVersion: p.MasterVersion,
		// One partition per party: created before amended before merged.
		PartitionKey: p.PartyID, Payload: payload,
	}
}

// PartyCreatedRecord is party.created.
func PartyCreatedRecord(p domain.Party, corr string) (*outbox.Record, error) {
	spec := partySpec(EventPartyCreated, p, p.CreatedBy, corr, map[string]any{
		"party_kind": p.PartyKind, "status": p.Status, "legal_entity_id": p.LegalEntityID,
	})
	spec.EffectiveAt = p.CreatedAt
	return BuildRecord(spec)
}

// PartyFactRecord is party.profile.amended or party.fact.published for one fact.
func PartyFactRecord(eventType string, p domain.Party, f domain.PartyFact, corr string) (*outbox.Record, error) {
	spec := partySpec(eventType, p, f.ProposedBy, corr, map[string]any{
		"fact_id": f.FactID, "fact_type": f.FactType, "valid_from": f.ValidFrom, "valid_to": f.ValidTo,
		"source_class": f.SourceClass, "quality_state": f.Status,
	})
	spec.EvidenceRef = f.EvidenceRef
	return BuildRecord(spec)
}

// PartyRoleRecord is party.role.added or party.role.removed.
func PartyRoleRecord(eventType string, p domain.Party, r domain.PartyRole, actor, corr string) (*outbox.Record, error) {
	spec := partySpec(eventType, p, actor, corr, map[string]any{
		"role_id": r.RoleID, "role_type": r.RoleType, "valid_from": r.ValidFrom, "valid_to": r.ValidTo,
	})
	spec.EvidenceRef = r.EvidenceRef
	return BuildRecord(spec)
}

// PartyIdentifierRecord is party.identifier.linked. The value is never in it; the scheme and the verification
// state are.
func PartyIdentifierRecord(p domain.Party, i domain.PartyIdentifier, actor, corr string) (*outbox.Record, error) {
	spec := partySpec(EventPartyIdentifierLinked, p, actor, corr, map[string]any{
		"identifier_id": i.IdentifierID, "scheme": i.Scheme, "scope": i.Scope, "verification_status": i.VerificationStatus,
	})
	spec.EvidenceRef = i.EvidenceRef
	return BuildRecord(spec)
}

// ConflictOpenedRecord is master.conflict.opened for a stewardship case. subject is the party the case is about,
// or the first candidate when a create was held back before any party existed.
func ConflictOpenedRecord(c domain.ResolutionCase, corr string) (*outbox.Record, error) {
	ids := make([]string, 0, len(c.Candidates))
	for _, cand := range c.Candidates {
		ids = append(ids, cand.PartyID)
	}
	subject := c.CaseID
	if c.SubjectPartyID != nil {
		subject = *c.SubjectPartyID
	}
	return BuildRecord(RecordSpec{
		EventType: EventMasterConflictOpened, TenantID: c.TenantID, ActorID: c.OpenedBy, CorrelationID: corr,
		ObjectID: c.CaseID, ObjectVersion: 1, PartitionKey: subject,
		Payload: map[string]any{
			"tenant_id": c.TenantID, "case_id": c.CaseID, "conflict_type": c.CaseType, "subject_party_id": c.SubjectPartyID,
			"candidate_party_ids": ids, "severity": "REVIEW_REQUIRED",
		},
	})
}
