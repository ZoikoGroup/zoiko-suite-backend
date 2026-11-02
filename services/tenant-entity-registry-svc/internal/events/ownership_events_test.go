package events_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/events"
)

func s(v string) *string { return &v }

func ownershipRow(kind domain.RelationshipKind, status domain.OwnershipStatus) *domain.OwnershipRelationship {
	pct := 60.0
	valid := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	decided := time.Date(2026, 2, 20, 9, 0, 0, 0, time.UTC)
	r := &domain.OwnershipRelationship{
		RelationshipID: "rel-1", TenantID: "tenant-1",
		ParentLegalEntityID: "parent-1", ChildLegalEntityID: "child-1",
		RelationshipKind: kind, ValidFrom: valid, Status: status,
		EvidenceRef: "filing-1", Reason: "r", BeneficialNotes: s("SECRET: owned via trust X"),
		ProposedByPrincipalID: "maker", ProposedAt: decided.Add(-time.Hour),
		CorrelationID: "corr-1", RecordVersion: 3,
	}
	if kind == domain.RelationshipOwnership {
		r.OwnershipPct = &pct
	} else {
		ct := domain.ControlBoard
		r.ControlType = &ct
	}
	if status != domain.OwnershipProposed {
		r.DecidedByPrincipalID, r.DecidedAt = s("checker"), &decided
	}
	return r
}

func decodeRaw(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	env := map[string]any{}
	require.NoError(t, json.Unmarshal(raw, &env))
	return env
}

// Each state change renders the event s4.4 and the asyncapi file say it does.
func TestOwnershipRecord_WireNameFollowsStatusAndKind(t *testing.T) {
	endedAt := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	ended := func(kind domain.RelationshipKind) *domain.OwnershipRelationship {
		r := ownershipRow(kind, domain.OwnershipEnded)
		r.EndedByPrincipalID, r.EndedAt, r.EndEvidenceRef, r.ValidTo = s("ender"), &endedAt, s("disposal"), &endedAt
		return r
	}
	superseded := func(kind domain.RelationshipKind) domain.OwnershipSupersession {
		return domain.OwnershipSupersession{
			Previous:    ownershipRow(kind, domain.OwnershipSuperseded),
			Replacement: ownershipRow(kind, domain.OwnershipActive),
		}
	}

	for name, c := range map[string]struct {
		input any
		want  string
	}{
		"proposed ownership":   {ownershipRow(domain.RelationshipOwnership, domain.OwnershipProposed), events.EventOwnershipProposed},
		"proposed control":     {ownershipRow(domain.RelationshipControl, domain.OwnershipProposed), events.EventOwnershipProposed},
		"activated ownership":  {ownershipRow(domain.RelationshipOwnership, domain.OwnershipActive), events.EventOwnershipActivated},
		"activated control":    {ownershipRow(domain.RelationshipControl, domain.OwnershipActive), events.EventControlChanged},
		"superseded ownership": {superseded(domain.RelationshipOwnership), events.EventOwnershipSuperseded},
		"superseded control":   {superseded(domain.RelationshipControl), events.EventControlChanged},
		"ended ownership":      {ended(domain.RelationshipOwnership), events.EventOwnershipEnded},
		"ended control":        {ended(domain.RelationshipControl), events.EventControlChanged},
		"rejected":             {ownershipRow(domain.RelationshipOwnership, domain.OwnershipRejected), events.EventOwnershipRejected},
	} {
		rec, err := events.OwnershipRecord(c.input)
		require.NoError(t, err, name)
		require.NotNil(t, rec, name)
		assert.Equal(t, c.want, rec.EventType, name)
	}
}

// Every ORG-04 wire name is reachable, so the contract lists no dead event.
func TestOwnershipRecord_EveryDeclaredEventCanBeProduced(t *testing.T) {
	produced := map[string]bool{}
	ended := ownershipRow(domain.RelationshipOwnership, domain.OwnershipEnded)
	endedAt := time.Now().UTC()
	ended.EndedByPrincipalID, ended.EndedAt, ended.EndEvidenceRef, ended.ValidTo = s("e"), &endedAt, s("ev"), &endedAt
	for _, in := range []any{
		ownershipRow(domain.RelationshipOwnership, domain.OwnershipProposed),
		ownershipRow(domain.RelationshipOwnership, domain.OwnershipActive),
		ownershipRow(domain.RelationshipControl, domain.OwnershipActive),
		ownershipRow(domain.RelationshipOwnership, domain.OwnershipRejected),
		domain.OwnershipSupersession{Previous: ownershipRow(domain.RelationshipOwnership, domain.OwnershipSuperseded), Replacement: ownershipRow(domain.RelationshipOwnership, domain.OwnershipActive)},
		ended,
	} {
		rec, err := events.OwnershipRecord(in)
		require.NoError(t, err)
		produced[rec.EventType] = true
	}
	for _, name := range events.OwnershipEventNames {
		assert.True(t, produced[name], "%s is declared but nothing produces it", name)
	}
	assert.Len(t, produced, len(events.OwnershipEventNames))
}

func TestOwnershipRecord_CarriesTheSection7Minimum(t *testing.T) {
	rec, err := events.OwnershipRecord(ownershipRow(domain.RelationshipOwnership, domain.OwnershipActive))
	require.NoError(t, err)
	env := decodeRaw(t, rec.Payload)

	assert.Equal(t, "tenant-1", env["tenant_id"])
	assert.Equal(t, "rel-1", env["object_id"])
	assert.EqualValues(t, 3, env["object_version"], "the version the write left behind")
	assert.Equal(t, "checker", env["actor_id"], "an approval is attributed to the approver, not the proposer")
	assert.Equal(t, "corr-1", env["correlation_id"])
	assert.Equal(t, "filing-1", env["evidence_ref"])
	assert.Equal(t, "child-1", env["legal_entity_id"])
	assert.Equal(t, "2026-03-01T00:00:00Z", env["effective_at"], "business time is the relationship's start")
	assert.Equal(t, "2026-02-20T09:00:00Z", env["recorded_at"], "record time is when it was approved")
	assert.Equal(t, "child-1", rec.PartitionKey, "ordered per child entity")

	payload := env["payload"].(map[string]any)
	assert.Equal(t, "ACTIVATED", payload["change_type"])
	assert.Equal(t, "OWNERSHIP", payload["relationship_kind"])
	assert.EqualValues(t, 60, payload["ownership_pct"])
	assert.Equal(t, "parent-1", payload["parent_legal_entity_id"])
}

// Beneficial notes reach only callers with the sensitive permission; an event
// reaches every subscriber, so it must not carry them.
func TestOwnershipRecord_NeverCarriesBeneficialNotes(t *testing.T) {
	for _, in := range []any{
		ownershipRow(domain.RelationshipOwnership, domain.OwnershipProposed),
		ownershipRow(domain.RelationshipOwnership, domain.OwnershipActive),
		domain.OwnershipSupersession{Previous: ownershipRow(domain.RelationshipOwnership, domain.OwnershipSuperseded), Replacement: ownershipRow(domain.RelationshipOwnership, domain.OwnershipActive)},
	} {
		rec, err := events.OwnershipRecord(in)
		require.NoError(t, err)
		assert.NotContains(t, string(rec.Payload), "SECRET")
		assert.NotContains(t, string(rec.Payload), "beneficial")
	}
}

func TestOwnershipRecord_SupersessionIsAttributedToTheReplacement(t *testing.T) {
	repl := ownershipRow(domain.RelationshipOwnership, domain.OwnershipActive)
	repl.RelationshipID, repl.ValidFrom = "rel-2", time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	repl.EvidenceRef, repl.CorrelationID = "filing-2", "corr-2"
	repl.DecidedByPrincipalID = s("approver-2")
	prev := ownershipRow(domain.RelationshipOwnership, domain.OwnershipSuperseded)
	prev.RecordVersion = 5

	rec, err := events.OwnershipRecord(domain.OwnershipSupersession{Previous: prev, Replacement: repl})
	require.NoError(t, err)
	env := decodeRaw(t, rec.Payload)

	assert.Equal(t, "rel-1", env["object_id"], "the event attests the relationship that was retired")
	assert.EqualValues(t, 5, env["object_version"])
	assert.Equal(t, "approver-2", env["actor_id"])
	assert.Equal(t, "filing-2", env["evidence_ref"])
	assert.Equal(t, "corr-2", env["correlation_id"])
	assert.Equal(t, "2026-06-01T00:00:00Z", env["effective_at"], "the original stops where its replacement starts")
	assert.Equal(t, "rel-2", env["payload"].(map[string]any)["replaced_by_relationship_id"])
}

func TestOwnershipRecord_EndIsAttributedToWhoEndedIt(t *testing.T) {
	endedAt := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	r := ownershipRow(domain.RelationshipOwnership, domain.OwnershipEnded)
	r.EndedByPrincipalID, r.EndedAt, r.EndEvidenceRef, r.ValidTo = s("ender"), &endedAt, s("disposal-1"), &endedAt

	rec, err := events.OwnershipRecord(r)
	require.NoError(t, err)
	env := decodeRaw(t, rec.Payload)
	assert.Equal(t, "ender", env["actor_id"])
	assert.Equal(t, "disposal-1", env["evidence_ref"], "an end carries its own evidence, not the start's")
	assert.Equal(t, "2026-09-01T00:00:00Z", env["effective_at"])
}

func TestOwnershipRecord_RefusesWhatItCannotRender(t *testing.T) {
	rec, err := events.OwnershipRecord("not an ownership result")
	require.NoError(t, err)
	assert.Nil(t, rec, "a result it does not recognise renders nothing")

	_, err = events.OwnershipRecord(domain.OwnershipSupersession{Previous: ownershipRow(domain.RelationshipOwnership, domain.OwnershipSuperseded)})
	require.Error(t, err, "a retired relationship with no replacement cannot be attributed")

	_, err = events.OwnershipRecord((*domain.OwnershipRelationship)(nil))
	require.Error(t, err)
}

// asyncapi.yaml and the code give the same spec name to each wire name.
func TestAsyncAPI_OwnershipSpecEventMappingMatchesTheCode(t *testing.T) {
	doc := loadAsyncAPI(t)
	byWire := map[string]string{}
	for _, m := range doc.Components.Messages {
		spec := ""
		if m.SpecEvent != nil {
			spec = *m.SpecEvent
		}
		byWire[m.Name] = spec
	}
	for _, wire := range events.OwnershipEventNames {
		got, declared := byWire[wire]
		require.True(t, declared, "%s is not declared in asyncapi.yaml", wire)
		assert.Equal(t, events.OwnershipSpecEvent[wire], got, "%s: x-spec-event differs from the code", wire)
	}
	assert.Len(t, events.OwnershipSpecEvent, len(events.OwnershipEventNames))
}
