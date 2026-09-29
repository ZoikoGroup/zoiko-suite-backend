// Package events_test asserts the outbox records carry the envelope Doc 03
// §19 requires and the ORG §7 minimum payload: tenant_id, object_id,
// object_version, effective_at, recorded_at, actor, correlation_id and
// evidence_ref where material.
//
// These replace the tests of the direct Kafka publisher, which was removed on
// 28 Sep 2026: every event now goes through event_outbox inside its write's
// transaction. The payload assertions are the same ones, so a consumer of the
// old events sees the same `payload` object.
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

type envelope struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	TenantID      string          `json:"tenant_id"`
	LegalEntityID string          `json:"legal_entity_id"`
	Jurisdiction  string          `json:"jurisdiction"`
	ActorID       string          `json:"actor_id"`
	CorrelationID string          `json:"correlation_id"`
	ObjectID      string          `json:"object_id"`
	ObjectVersion int64           `json:"object_version"`
	EffectiveAt   time.Time       `json:"effective_at"`
	RecordedAt    time.Time       `json:"recorded_at"`
	EvidenceRef   string          `json:"evidence_ref"`
	Payload       json.RawMessage `json:"payload"`
}

func decode(t *testing.T, rec *outbox.Record) envelope {
	t.Helper()
	require.NotNil(t, rec)
	var env envelope
	require.NoError(t, json.Unmarshal(rec.Payload, &env))
	return env
}

func payloadOf(t *testing.T, env envelope) map[string]any {
	t.Helper()
	m := map[string]any{}
	require.NoError(t, json.Unmarshal(env.Payload, &m))
	return m
}

// ORG §7: an event a consumer cannot pin to a version is not usable for
// historical reconstruction, so a record without one is refused outright.
func TestBuildRecord_RefusesAnEventWithoutObjectIdentityOrVersion(t *testing.T) {
	_, err := events.BuildRecord(events.RecordSpec{EventType: "x.y", TenantID: "t", ObjectID: "o"})
	assert.Error(t, err, "object_version 0 must be refused")
	_, err = events.BuildRecord(events.RecordSpec{EventType: "x.y", TenantID: "t", ObjectVersion: 1})
	assert.Error(t, err, "empty object_id must be refused")
}

func TestBuildRecord_CarriesTheSection7MinimumPayload(t *testing.T) {
	eff := time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)
	rec, err := events.BuildRecord(events.RecordSpec{
		EventType: events.EventLegalEntityProfileAmended, TenantID: "tenant-1",
		ActorID: "p-1", CorrelationID: "corr-1", ObjectID: "entity-1", ObjectVersion: 4,
		EffectiveAt: eff, EvidenceRef: "CH-123", Payload: map[string]any{"k": "v"},
	})
	require.NoError(t, err)
	env := decode(t, rec)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "entity-1", env.ObjectID)
	assert.Equal(t, int64(4), env.ObjectVersion)
	assert.True(t, env.EffectiveAt.Equal(eff), "effective_at is business time")
	assert.False(t, env.RecordedAt.IsZero(), "recorded_at is always set")
	assert.Equal(t, "p-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.Equal(t, "CH-123", env.EvidenceRef)
}

func TestStampVersion_SetsTheVersionAndMergesPayload(t *testing.T) {
	rec, err := events.BuildRecord(events.RecordSpec{
		EventType: "entity.profile.amended", TenantID: "t", ObjectID: "e", ObjectVersion: 1,
		Payload: map[string]any{"a": 1},
	})
	require.NoError(t, err)
	require.NoError(t, events.StampVersion(rec, 7, map[string]any{"profile_version": 3}))
	env := decode(t, rec)
	assert.Equal(t, int64(7), env.ObjectVersion)
	p := payloadOf(t, env)
	assert.EqualValues(t, 1, p["a"])
	assert.EqualValues(t, 3, p["profile_version"])
}

func TestEntityCreatedRecord_EnvelopeCarriesJurisdictionActorAndVersion(t *testing.T) {
	rec, err := events.EntityCreatedRecord(&domain.LegalEntity{
		LegalEntityID: "entity-1", TenantID: "tenant-1", PrimaryJurisdictionID: "uk-england",
		CreatedByPrincipalID: "creator-1", RecordVersion: 1, CreatedAt: time.Now().UTC(),
	}, "corr-1")
	require.NoError(t, err)
	env := decode(t, rec)
	assert.Equal(t, "entity.created", env.EventType)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "uk-england", env.Jurisdiction)
	assert.Equal(t, "creator-1", env.ActorID)
	assert.Equal(t, "entity-1", env.ObjectID)
	assert.Equal(t, int64(1), env.ObjectVersion)
	assert.Equal(t, "entity-1", rec.PartitionKey, "one entity's events share a partition")
}

func TestEntityUpdatedRecord_RepeatEventsGetDistinctIDsAndTheirOwnVersions(t *testing.T) {
	a, err := events.EntityUpdatedRecord(&domain.LegalEntity{LegalEntityID: "e", TenantID: "t", RecordVersion: 2}, "c")
	require.NoError(t, err)
	b, err := events.EntityUpdatedRecord(&domain.LegalEntity{LegalEntityID: "e", TenantID: "t", RecordVersion: 3}, "c")
	require.NoError(t, err)
	assert.NotEqual(t, a.EventID, b.EventID)
	assert.Equal(t, int64(2), decode(t, a).ObjectVersion)
	assert.Equal(t, int64(3), decode(t, b).ObjectVersion)
}

// The generic entity status route used to publish previous_status as "".
func TestEntityStatusChangedRecord_CarriesPreviousStatus(t *testing.T) {
	rec, err := events.EntityStatusChangedRecord(events.StatusChange{
		TenantID: "t", ObjectID: "e", Previous: "ACTIVE", New: "DORMANT", RecordVersion: 5, ActorID: "p",
	}, "c")
	require.NoError(t, err)
	env := decode(t, rec)
	p := payloadOf(t, env)
	assert.Equal(t, "ACTIVE", p["previous_status"])
	assert.Equal(t, "DORMANT", p["new_status"])
	assert.Equal(t, int64(5), env.ObjectVersion)
}

// A reclassified workspace changes whether it may ever be charged, and
// commercial-account-svc sits in a different plane — so workspace.updated has
// to carry the new classification rather than only the id.
func TestWorkspaceUpdatedRecord_CarriesCommercialFields(t *testing.T) {
	entity, account := "entity-9", "acct-42"
	rec, err := events.WorkspaceUpdatedRecord(&domain.Workspace{
		WorkspaceID: "ws-1", TenantID: "tenant-1", LegalEntityID: &entity, Name: "Acme Production",
		BillingClassification: domain.BillingClassificationCommercialStandalone,
		BillingSource:         domain.BillingSourceDirect, CommercialAccountID: &account,
		UpdatedByPrincipalID: "principal-7", RecordVersion: 2,
	}, "corr-1")
	require.NoError(t, err)
	env := decode(t, rec)
	assert.Equal(t, "workspace.updated", env.EventType)
	assert.Equal(t, "entity-9", env.LegalEntityID)
	assert.Equal(t, "principal-7", env.ActorID, "the actor must be the principal that made the change")
	p := payloadOf(t, env)
	assert.Equal(t, "ws-1", p["workspace_id"])
	assert.Equal(t, "COMMERCIAL_STANDALONE", p["billing_classification"])
	assert.Equal(t, "DIRECT", p["billing_source"])
	assert.Equal(t, "acct-42", p["commercial_account_id"])
}

// A workspace with no legal entity is a tenant-level object; the envelope's
// legal_entity_id must be empty rather than the string "<nil>".
func TestWorkspaceUpdatedRecord_NoLegalEntity_EmptyEnvelopeField(t *testing.T) {
	rec, err := events.WorkspaceUpdatedRecord(&domain.Workspace{
		WorkspaceID: "ws-1", TenantID: "tenant-1", RecordVersion: 1,
		BillingClassification: domain.BillingClassificationInternal,
	}, "corr-1")
	require.NoError(t, err)
	assert.Equal(t, "", decode(t, rec).LegalEntityID)
}

func TestWorkspaceStatusChangedRecord_CarriesPreviousStatus(t *testing.T) {
	rec, err := events.WorkspaceStatusChangedRecord(events.StatusChange{
		TenantID: "tenant-1", ObjectID: "ws-1", Previous: "ACTIVE", New: "ARCHIVED", RecordVersion: 3, ActorID: "principal-7",
	}, "corr-1")
	require.NoError(t, err)
	env := decode(t, rec)
	assert.Equal(t, "workspace.status.changed", env.EventType)
	p := payloadOf(t, env)
	assert.Equal(t, "ACTIVE", p["previous_status"])
	assert.Equal(t, "ARCHIVED", p["new_status"])
}

// The END_DATED hierarchy event used to carry only an id and an end date.
func TestHierarchyChangedRecord_EndDatedCarriesTheFullRow(t *testing.T) {
	end := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	rec, err := events.HierarchyChangedRecord(&domain.EntityHierarchy{
		HierarchyID: "h-1", TenantID: "t", ParentLegalEntityID: "p", ChildLegalEntityID: "c",
		EffectiveTo: &end, UpdatedByPrincipalID: "ender", RecordVersion: 2,
	}, events.HierarchyChangeEndDated, "corr")
	require.NoError(t, err)
	env := decode(t, rec)
	p := payloadOf(t, env)
	assert.Equal(t, "p", p["parent_legal_entity_id"])
	assert.Equal(t, "c", p["child_legal_entity_id"])
	assert.Equal(t, "ender", env.ActorID)
	assert.True(t, env.EffectiveAt.Equal(end))
	assert.Equal(t, int64(2), env.ObjectVersion)
}

func TestRegistryConflictRecords_QuarantineAndResolution(t *testing.T) {
	corr := "corr-1"
	q, err := events.RegistryConflictQuarantinedRecord(&domain.EntityRegistryConflict{
		ConflictID: "c-1", TenantID: "t", RegistrationNumber: "123", JurisdictionID: "gb",
		ExistingLegalEntityID: "e-1", DetectedByPrincipalID: "p", CorrelationID: &corr,
		DetectedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	assert.Equal(t, "entity.registry_conflict.quarantined", decode(t, q).EventType)

	r, err := events.RegistryConflictResolvedRecord(events.ConflictResolution{
		TenantID: "t", ConflictID: "c-1", Status: "RESOLVED_DUPLICATE", Note: "n",
		ResolvedBy: "p", ApprovedBy: "a", CorrelationID: corr, At: time.Now().UTC(),
	})
	require.NoError(t, err)
	env := decode(t, r)
	assert.Equal(t, "entity.registry_conflict.resolved", env.EventType)
	assert.Equal(t, int64(2), env.ObjectVersion)
}
