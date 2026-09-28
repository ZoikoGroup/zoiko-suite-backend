package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/events"
	"zoiko.io/tenant-entity-registry-svc/internal/outbox"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

// The formerly direct-publish write paths, now on the transactional outbox
// (28 Sep 2026). Against a real Postgres: the fact and its event commit
// together, and an event that cannot be rendered aborts the fact.

func lastEnvelope(t *testing.T, f *orgFixture, eventType string) map[string]any {
	t.Helper()
	var raw []byte
	require.NoError(t, f.pool.QueryRow(context.Background(), `
		SELECT payload FROM event_outbox WHERE event_type = $1 AND tenant_id = $2
		 ORDER BY created_at DESC LIMIT 1`, eventType, f.tenantID).Scan(&raw))
	env := map[string]any{}
	require.NoError(t, json.Unmarshal(raw, &env))
	return env
}

func newEntity(f *orgFixture, status domain.EntityStatus) *domain.LegalEntity {
	id := uuid.New().String()
	return &domain.LegalEntity{
		LegalEntityID: id, TenantID: f.tenantID, EntityCode: "EC-" + id[:8],
		LegalName: "Atomic Ltd", EntityType: domain.EntityTypeSubsidiary,
		DefaultCurrencyCode: "USD", FiscalCalendarID: uuid.New().String(),
		EntityStatus: status, PrimaryJurisdictionID: uuid.New().String(),
		DataResidencyPolicyID: f.policyID, RecordVersion: 1,
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "p-maker",
	}
}

func withEntityCreated(ctx context.Context) context.Context {
	return registry.WithEvent(ctx, func(res any) (*outbox.Record, error) {
		if e, ok := res.(*domain.LegalEntity); ok {
			return events.EntityCreatedRecord(e, "corr-create")
		}
		return nil, nil
	})
}

// Entity, profile version 1 and entity.created land in one transaction — the
// profile used to be a second write whose failure was only logged.
func TestCreateEntity_EntityProfileAndEventCommitTogether(t *testing.T) {
	f := newORGFixture(t)
	e := newEntity(f, domain.EntityStatusDraft)
	e.InitialProfile = &domain.LegalEntityProfileVersion{
		ProfileVersionID: uuid.New().String(), TenantID: f.tenantID, LegalEntityID: e.LegalEntityID,
		LegalName: e.LegalName, EffectiveFrom: e.CreatedAt, CreatedByPrincipalID: "p-maker",
	}
	before := outboxCount(t, f, "entity.created")

	require.NoError(t, f.s.CreateEntity(withEntityCreated(f.ctx), e))

	asOf, err := f.s.GetEntityProfileAsOf(f.ctx, e.LegalEntityID, time.Now().UTC())
	require.NoError(t, err)
	require.NotNil(t, asOf.Profile, "version 1 exists the moment the entity does")
	assert.Equal(t, before+1, outboxCount(t, f, "entity.created"))

	env := lastEnvelope(t, f, "entity.created")
	assert.Equal(t, e.LegalEntityID, env["object_id"])
	assert.EqualValues(t, 1, env["object_version"])
	assert.NotEmpty(t, env["recorded_at"])
}

// If the event cannot be produced the fact must not be committed either.
func TestCreateEntity_AnEventThatCannotBeRenderedAbortsTheWrite(t *testing.T) {
	f := newORGFixture(t)
	e := newEntity(f, domain.EntityStatusDraft)
	ctx := registry.WithEvent(f.ctx, func(any) (*outbox.Record, error) {
		return nil, errors.New("render failed")
	})

	require.Error(t, f.s.CreateEntity(ctx, e))
	got, err := f.s.GetEntityByID(f.ctx, e.LegalEntityID)
	require.NoError(t, err)
	assert.Nil(t, got, "no entity without its event")
}

// The generic status route now bumps record_version (an expected_version
// must see a status change) and names the previous status; a same-status
// re-apply changes nothing and says nothing.
func TestTransitionEntityStatus_VersionPreviousStatusAndNoOpReapply(t *testing.T) {
	f := newORGFixture(t)
	ctx := registry.WithEvent(f.ctx, func(res any) (*outbox.Record, error) {
		if c, ok := res.(events.StatusChange); ok {
			return events.EntityStatusChangedRecord(c, "corr-status")
		}
		return nil, nil
	})
	before, err := f.s.GetEntityByID(f.ctx, f.entityID)
	require.NoError(t, err)
	n0 := outboxCount(t, f, "entity.status.changed")

	affected, _, err := f.s.TransitionEntityStatus(ctx, f.entityID, domain.EntityStatusDormant,
		[]domain.EntityStatus{domain.EntityStatusActive, domain.EntityStatusDormant}, "p-actor", "corr-status")
	require.NoError(t, err)
	require.EqualValues(t, 1, affected)

	after, err := f.s.GetEntityByID(f.ctx, f.entityID)
	require.NoError(t, err)
	assert.Equal(t, before.RecordVersion+1, after.RecordVersion)
	env := lastEnvelope(t, f, "entity.status.changed")
	assert.EqualValues(t, after.RecordVersion, env["object_version"])
	assert.Equal(t, "ACTIVE", env["payload"].(map[string]any)["previous_status"])

	// Same → same: affected, but no version and no event.
	affected, _, err = f.s.TransitionEntityStatus(ctx, f.entityID, domain.EntityStatusDormant,
		[]domain.EntityStatus{domain.EntityStatusDormant}, "p-actor", "corr-status")
	require.NoError(t, err)
	require.EqualValues(t, 1, affected)
	again, err := f.s.GetEntityByID(f.ctx, f.entityID)
	require.NoError(t, err)
	assert.Equal(t, after.RecordVersion, again.RecordVersion)
	assert.Equal(t, n0+1, outboxCount(t, f, "entity.status.changed"))
}

// End-dating used to report success (204 and an event) for an id that did
// not exist or was already closed.
func TestEndDateHierarchy_RefusesUnknownAndAlreadyClosed(t *testing.T) {
	f := newORGFixture(t)
	child := newEntity(f, domain.EntityStatusActive)
	require.NoError(t, f.s.CreateEntity(f.ctx, child))
	h := &domain.EntityHierarchy{
		HierarchyID: uuid.New().String(), TenantID: f.tenantID,
		ParentLegalEntityID: f.entityID, ChildLegalEntityID: child.LegalEntityID,
		RelationshipType: domain.HierarchyRelationshipOwnership, EffectiveFrom: time.Now().UTC().Add(-time.Hour),
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "p",
	}
	require.NoError(t, f.s.CreateHierarchy(f.ctx, h))

	err := f.s.EndDateHierarchy(f.ctx, uuid.New().String(), time.Now().UTC(), "p", "c")
	assert.ErrorIs(t, err, registry.ErrNotFound)

	require.NoError(t, f.s.EndDateHierarchy(f.ctx, h.HierarchyID, time.Now().UTC(), "p", "c"))
	err = f.s.EndDateHierarchy(f.ctx, h.HierarchyID, time.Now().UTC(), "p", "c")
	assert.ErrorIs(t, err, registry.ErrStateConflict)
}

// §8 NP5's notification was rendered, logged and never sent.
func TestRecordRegistryConflict_EnqueuesTheQuarantineEvent(t *testing.T) {
	f := newORGFixture(t)
	ctx := registry.WithEvent(f.ctx, func(res any) (*outbox.Record, error) {
		if c, ok := res.(*domain.EntityRegistryConflict); ok {
			return events.RegistryConflictQuarantinedRecord(c)
		}
		return nil, nil
	})
	before := outboxCount(t, f, "entity.registry_conflict.quarantined")
	require.NoError(t, f.s.RecordRegistryConflict(ctx, &domain.EntityRegistryConflict{
		ConflictID: uuid.New().String(), TenantID: f.tenantID, RegistrationNumber: "RC-X",
		JurisdictionID: uuid.New().String(), ExistingLegalEntityID: f.entityID,
		AttemptedPayload: map[string]any{}, Status: domain.RegistryConflictOpen,
		DetectedAt: time.Now().UTC(), DetectedByPrincipalID: "p",
	}))
	assert.Equal(t, before+1, outboxCount(t, f, "entity.registry_conflict.quarantined"))
}
