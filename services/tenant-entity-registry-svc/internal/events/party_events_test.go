package events_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/events"
	"zoiko.io/tenant-entity-registry-svc/internal/outbox"
)

var testParty = domain.Party{
	PartyID: "0190a000-0000-7000-8000-00000000aaaa", TenantID: "0190a000-0000-7000-8000-00000000bbbb",
	PartyKind: domain.PartyLegalEntity, Status: domain.PartyActive, MasterVersion: 4, CreatedBy: "p-steward",
	CreatedAt: time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC),
}

// Every party event names its type, its party and tenant, and carries no name, identifier value or evidence text.
func TestPartyEvents_CarryIdentityAndVersionButNeverAValue(t *testing.T) {
	fact := domain.PartyFact{FactID: "f1", FactType: domain.FactLegalName, Value: "Secret Person Name", ValidFrom: "2025-01-01",
		Status: domain.FactVerified, SourceClass: domain.SourceAuthorityRegistry, EvidenceRef: "private-evidence-text", ProposedBy: "p-1"}
	ident := domain.PartyIdentifier{IdentifierID: "i1", Scheme: domain.SchemeTaxID, Scope: "GB", Value: "1234567890",
		VerificationStatus: "VERIFIED", EvidenceRef: "private-evidence-text"}
	role := domain.PartyRole{RoleID: "r1", RoleType: domain.RoleSupplier, ValidFrom: "2025-01-01", EvidenceRef: "private-evidence-text"}
	c := domain.ResolutionCase{CaseID: "c1", TenantID: testParty.TenantID, CaseType: domain.CaseDuplicateReview, OpenedBy: "p-1",
		Reason: "private-evidence-text", Candidates: []domain.MatchCandidate{{PartyID: "p-9"}}}

	cases := map[string]func() (*outbox.Record, error){
		events.EventPartyCreated: func() (*outbox.Record, error) { return events.PartyCreatedRecord(testParty, "corr") },
		events.EventPartyProfileAmended: func() (*outbox.Record, error) {
			return events.PartyFactRecord(events.EventPartyProfileAmended, testParty, fact, "corr")
		},
		events.EventPartyFactPublished: func() (*outbox.Record, error) {
			return events.PartyFactRecord(events.EventPartyFactPublished, testParty, fact, "corr")
		},
		events.EventPartyRoleAdded: func() (*outbox.Record, error) {
			return events.PartyRoleRecord(events.EventPartyRoleAdded, testParty, role, "p-1", "corr")
		},
		events.EventPartyRoleRemoved: func() (*outbox.Record, error) {
			return events.PartyRoleRecord(events.EventPartyRoleRemoved, testParty, role, "p-1", "corr")
		},
		events.EventPartyIdentifierLinked: func() (*outbox.Record, error) {
			return events.PartyIdentifierRecord(testParty, ident, "p-1", "corr")
		},
		events.EventPartiesMerged: func() (*outbox.Record, error) {
			return events.PartiesMergedRecord(testParty, []string{"alias-1"}, "case-1", "p-1", "corr", map[string]any{"alias-1": map[string]any{"previous_status": "ACTIVE", "identifiers_moved": []string{"i1"}, "roles_moved": []string{"SUPPLIER"}}})
		},
		events.EventPartySplit: func() (*outbox.Record, error) {
			return events.PartySplitRecord(testParty, "alias-1", "case-1", "p-1", "corr", map[string]any{})
		},
		events.EventMasterConflictResolved: func() (*outbox.Record, error) {
			return events.ConflictResolvedRecord(domain.ResolutionCase{CaseID: "c1", TenantID: testParty.TenantID, CaseType: domain.CaseFactConflict, DecidedBy: "p-2", Decision: "KEEP_VERIFIED"}, "corr")
		},
		events.EventMasterConflictOpened: func() (*outbox.Record, error) { return events.ConflictOpenedRecord(c, "corr") },
	}

	var produced []string
	for wantType, build := range cases {
		rec, err := build()
		require.NoError(t, err, wantType)
		assert.Equal(t, wantType, rec.EventType)
		assert.Equal(t, testParty.TenantID, rec.TenantID, wantType)
		envelope := string(rec.Payload)
		assert.Contains(t, envelope, testParty.TenantID, wantType)
		// The envelope's evidence_ref is a pointer to the evidence (ORG s7), as on every ORG event; no value, and no
		// evidence text, is in the payload object a consumer reads.
		var env struct {
			Payload json.RawMessage `json:"payload"`
		}
		require.NoError(t, json.Unmarshal(rec.Payload, &env), wantType)
		for _, secret := range []string{"Secret Person Name", "1234567890", "private-evidence-text"} {
			assert.NotContains(t, string(env.Payload), secret, "%s: the payload carries no name, identifier value or evidence text", wantType)
		}
		for _, secret := range []string{"Secret Person Name", "1234567890"} {
			assert.NotContains(t, envelope, secret, "%s: nothing in the event carries a name or an identifier value", wantType)
		}
		produced = append(produced, rec.EventType)
	}
	assert.ElementsMatch(t, events.PartyEventNames, produced, "the list of party events is the list the builders produce")
}

func TestPartyEvents_PartitionByPartyAtItsVersion(t *testing.T) {
	rec, err := events.PartyCreatedRecord(testParty, "corr")
	require.NoError(t, err)
	assert.Equal(t, testParty.PartyID, rec.PartitionKey, "one partition per party: created before amended")
	assert.Equal(t, "party.created", rec.EventType)
}
