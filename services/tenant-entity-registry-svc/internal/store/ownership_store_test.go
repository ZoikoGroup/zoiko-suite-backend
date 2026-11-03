package store_test

// ORG-04 against a real Postgres: the table's CHECKs, the guarded decision
// UPDATE, tenant isolation and a decision race.

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

func f64(v float64) *float64 { return &v }

// ownershipEntity adds a second operational entity to the fixture's tenant.
func ownershipEntity(t *testing.T, f *orgFixture) string {
	t.Helper()
	e := newEntity(f, domain.EntityStatusActive)
	require.NoError(t, f.s.CreateEntity(f.ctx, e))
	return e.LegalEntityID
}

func newRelationship(f *orgFixture, parent, child string) *domain.OwnershipRelationship {
	now := time.Now().UTC().Truncate(time.Microsecond)
	return &domain.OwnershipRelationship{
		RelationshipID: uuid.New().String(), TenantID: f.tenantID,
		ParentLegalEntityID: parent, ChildLegalEntityID: child,
		RelationshipKind: domain.RelationshipOwnership, OwnershipPct: f64(60), VotingPct: f64(55.5),
		ValidFrom: now.Add(-24 * time.Hour), RecordedAt: now,
		Status: domain.OwnershipProposed, EvidenceRef: "filing-1", Reason: "acquisition",
		ProposedByPrincipalID: "p-maker", ProposedAt: now, CorrelationID: "corr-1", RecordVersion: 1,
	}
}

func TestOwnership_RoundTripsEveryField(t *testing.T) {
	f := newORGFixture(t)
	child := ownershipEntity(t, f)
	r := newRelationship(f, f.entityID, child)
	note := "beneficial owner: A. Person"
	r.BeneficialNotes = &note
	to := r.ValidFrom.Add(100 * 24 * time.Hour)
	r.ValidTo = &to

	require.NoError(t, f.s.CreateOwnershipRelationship(f.ctx, r))
	got, err := f.s.GetOwnershipRelationship(f.ctx, r.RelationshipID)
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, domain.OwnershipProposed, got.Status)
	assert.Equal(t, domain.RelationshipOwnership, got.RelationshipKind)
	assert.InDelta(t, 60, *got.OwnershipPct, 0.0001)
	assert.InDelta(t, 55.5, *got.VotingPct, 0.0001)
	assert.Equal(t, note, *got.BeneficialNotes)
	assert.True(t, r.ValidFrom.Equal(got.ValidFrom))
	assert.True(t, to.Equal(*got.ValidTo))
	assert.Equal(t, "filing-1", got.EvidenceRef)
	assert.Equal(t, "corr-1", got.CorrelationID)
	assert.EqualValues(t, 1, got.RecordVersion)
	assert.Nil(t, got.DecidedByPrincipalID)
}

func TestOwnership_ControlRelationshipNeedsNoEquity(t *testing.T) {
	f := newORGFixture(t)
	child := ownershipEntity(t, f)
	r := newRelationship(f, f.entityID, child)
	ctl := domain.ControlBoard
	r.RelationshipKind, r.OwnershipPct, r.VotingPct, r.ControlType = domain.RelationshipControl, nil, nil, &ctl

	require.NoError(t, f.s.CreateOwnershipRelationship(f.ctx, r))
	got, err := f.s.GetOwnershipRelationship(f.ctx, r.RelationshipID)
	require.NoError(t, err)
	assert.Equal(t, domain.ControlBoard, *got.ControlType)
	assert.Nil(t, got.OwnershipPct)
}

// The table refuses what the service would also refuse, so a second writer
// cannot bypass the service.
func TestOwnership_TheTableRefusesWhatTheServiceRefuses(t *testing.T) {
	f := newORGFixture(t)
	child := ownershipEntity(t, f)

	bad := map[string]func(*domain.OwnershipRelationship){
		"self relationship":     func(r *domain.OwnershipRelationship) { r.ChildLegalEntityID = r.ParentLegalEntityID },
		"zero pct":              func(r *domain.OwnershipRelationship) { r.OwnershipPct = f64(0) },
		"pct over 100":          func(r *domain.OwnershipRelationship) { r.OwnershipPct = f64(100.5) },
		"ownership without pct": func(r *domain.OwnershipRelationship) { r.OwnershipPct = nil },
		"control without type": func(r *domain.OwnershipRelationship) {
			r.RelationshipKind, r.OwnershipPct = domain.RelationshipControl, nil
		},
		"valid_to before valid_from": func(r *domain.OwnershipRelationship) {
			past := r.ValidFrom.Add(-time.Hour)
			r.ValidTo = &past
		},
		"blank evidence":  func(r *domain.OwnershipRelationship) { r.EvidenceRef = "   " },
		"unknown kind":    func(r *domain.OwnershipRelationship) { r.RelationshipKind = "FRIENDSHIP" },
		"unknown entity":  func(r *domain.OwnershipRelationship) { r.ChildLegalEntityID = uuid.New().String() },
		"decided on file": func(r *domain.OwnershipRelationship) { r.Status = domain.OwnershipActive },
	}
	for name, mutate := range bad {
		t.Run(name, func(t *testing.T) {
			r := newRelationship(f, f.entityID, child)
			mutate(r)
			require.Error(t, f.s.CreateOwnershipRelationship(f.ctx, r))
			got, err := f.s.GetOwnershipRelationship(f.ctx, r.RelationshipID)
			require.NoError(t, err)
			assert.Nil(t, got)
		})
	}
}

func TestOwnership_DuplicateProposalIsAConflict(t *testing.T) {
	f := newORGFixture(t)
	child := ownershipEntity(t, f)
	first := newRelationship(f, f.entityID, child)
	require.NoError(t, f.s.CreateOwnershipRelationship(f.ctx, first))

	dup := newRelationship(f, f.entityID, child)
	dup.ValidFrom = first.ValidFrom
	require.ErrorIs(t, f.s.CreateOwnershipRelationship(f.ctx, dup), registry.ErrConflict)

	// A different start date is a different fact.
	later := newRelationship(f, f.entityID, child)
	later.ValidFrom = first.ValidFrom.Add(48 * time.Hour)
	require.NoError(t, f.s.CreateOwnershipRelationship(f.ctx, later))
}

func TestOwnership_DecisionIsGuardedByStatusVersionAndSegregation(t *testing.T) {
	f := newORGFixture(t)
	child := ownershipEntity(t, f)
	r := newRelationship(f, f.entityID, child)
	require.NoError(t, f.s.CreateOwnershipRelationship(f.ctx, r))
	id := r.RelationshipID

	t.Run("the proposer is refused by the database as well", func(t *testing.T) {
		_, err := f.s.DecideOwnershipRelationship(f.ctx, id, 1, domain.OwnershipActive, "p-maker", "", "")
		require.ErrorIs(t, err, registry.ErrSelfApproval)
	})
	t.Run("stale version", func(t *testing.T) {
		_, err := f.s.DecideOwnershipRelationship(f.ctx, id, 9, domain.OwnershipActive, "p-checker", "", "")
		require.ErrorIs(t, err, registry.ErrVersionConflict)
	})
	t.Run("unknown id", func(t *testing.T) {
		_, err := f.s.DecideOwnershipRelationship(f.ctx, uuid.New().String(), 1, domain.OwnershipActive, "p-checker", "", "")
		require.ErrorIs(t, err, registry.ErrNotFound)
	})
	t.Run("nothing changed after the refusals", func(t *testing.T) {
		got, _ := f.s.GetOwnershipRelationship(f.ctx, id)
		assert.Equal(t, domain.OwnershipProposed, got.Status)
		assert.EqualValues(t, 1, got.RecordVersion)
	})
	t.Run("a different principal approves", func(t *testing.T) {
		out, err := f.s.DecideOwnershipRelationship(f.ctx, id, 1, domain.OwnershipActive, "p-checker", "verified", "corr-2")
		require.NoError(t, err)
		assert.Equal(t, domain.OwnershipActive, out.Status)
		assert.Equal(t, "p-checker", *out.DecidedByPrincipalID)
		assert.Equal(t, "verified", *out.DecisionNote)
		assert.Equal(t, "corr-2", out.CorrelationID)
		assert.EqualValues(t, 2, out.RecordVersion)
		assert.NotNil(t, out.DecidedAt)
	})
	t.Run("a decided relationship cannot be decided again", func(t *testing.T) {
		_, err := f.s.DecideOwnershipRelationship(f.ctx, id, 2, domain.OwnershipRejected, "p-other", "", "")
		require.ErrorIs(t, err, registry.ErrInvalidTransition)
	})
}

func TestOwnership_RejectRecordsTheDecider(t *testing.T) {
	f := newORGFixture(t)
	child := ownershipEntity(t, f)
	r := newRelationship(f, f.entityID, child)
	require.NoError(t, f.s.CreateOwnershipRelationship(f.ctx, r))

	out, err := f.s.DecideOwnershipRelationship(f.ctx, r.RelationshipID, 1, domain.OwnershipRejected, "p-checker", "no filing", "")
	require.NoError(t, err)
	assert.Equal(t, domain.OwnershipRejected, out.Status)

	// A rejected proposal frees the identity for a corrected one.
	again := newRelationship(f, f.entityID, child)
	again.ValidFrom = r.ValidFrom
	require.NoError(t, f.s.CreateOwnershipRelationship(f.ctx, again))
}

// Two approvers racing for one proposal: exactly one wins, the other is told
// the relationship is no longer PROPOSED or has moved.
func TestOwnership_ConcurrentDecisionsHaveOneWinner(t *testing.T) {
	f := newORGFixture(t)
	child := ownershipEntity(t, f)
	r := newRelationship(f, f.entityID, child)
	require.NoError(t, f.s.CreateOwnershipRelationship(f.ctx, r))

	const racers = 8
	var wg sync.WaitGroup
	results := make(chan error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := f.s.DecideOwnershipRelationship(f.ctx, r.RelationshipID, 1, domain.OwnershipActive,
				"checker-"+uuid.New().String()[:6], "", "")
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)

	wins := 0
	for err := range results {
		if err == nil {
			wins++
		}
	}
	assert.Equal(t, 1, wins, "exactly one decision may apply")
	got, _ := f.s.GetOwnershipRelationship(f.ctx, r.RelationshipID)
	assert.EqualValues(t, 2, got.RecordVersion, "the version moved once")
}

// Another tenant sees and decides nothing, even holding the id.
func TestOwnership_IsTenantIsolated(t *testing.T) {
	a := newORGFixture(t)
	child := ownershipEntity(t, a)
	r := newRelationship(a, a.entityID, child)
	require.NoError(t, a.s.CreateOwnershipRelationship(a.ctx, r))

	b := secondTenant(t, a)
	got, err := b.s.GetOwnershipRelationship(b.ctx, r.RelationshipID)
	require.NoError(t, err)
	assert.Nil(t, got, "tenant B must not see tenant A's relationship")

	_, err = b.s.DecideOwnershipRelationship(b.ctx, r.RelationshipID, 1, domain.OwnershipActive, "p-checker", "", "")
	require.ErrorIs(t, err, registry.ErrNotFound)

}

// The tenant of a new row is the verified one, whatever the body says.
func TestOwnership_TheTenantIsTheVerifiedOneNotTheBodys(t *testing.T) {
	a := newORGFixture(t)
	b := secondTenant(t, a)
	r := newRelationship(b, b.entityID, ownershipEntity(t, b))
	r.TenantID = a.tenantID // a forged body

	require.NoError(t, b.s.CreateOwnershipRelationship(b.ctx, r))
	got, err := b.s.GetOwnershipRelationship(b.ctx, r.RelationshipID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, b.tenantID, got.TenantID)
	seenByA, err := a.s.GetOwnershipRelationship(a.ctx, r.RelationshipID)
	require.NoError(t, err)
	assert.Nil(t, seenByA)
}

// A relationship cannot join another tenant's entity, even for a writer that
// skips the service: the foreign keys include tenant_id.
func TestOwnership_TheDatabaseRefusesACrossTenantRelationship(t *testing.T) {
	a := newORGFixture(t)
	b := secondTenant(t, a)

	// Tenant A relates its own entity to tenant B's.
	r := newRelationship(a, a.entityID, b.entityID)
	err := a.s.CreateOwnershipRelationship(a.ctx, r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "or_child_in_tenant")

	r = newRelationship(a, b.entityID, a.entityID)
	err = a.s.CreateOwnershipRelationship(a.ctx, r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "or_parent_in_tenant")
}

// secondTenant seeds another tenant, with one active entity, into the database
// a's fixture already opened. newORGFixture cannot be called twice in one test:
// it rebuilds the schema, which would erase the first tenant.
func secondTenant(t *testing.T, a *orgFixture) *orgFixture {
	t.Helper()
	tenantID, policyID, entityID := uuid.New().String(), uuid.New().String(), uuid.New().String()
	ctx := domain.WithTenant(context.Background(), tenantID)
	require.NoError(t, a.s.CreateTenantWithDefaultResidencyPolicy(ctx,
		&domain.Tenant{
			TenantID: tenantID, TenantCode: "ORG-" + tenantID[:8], LegalName: "Second Co",
			Status: domain.TenantStatusActive, DefaultCurrencyCode: "USD", PrimaryTimezone: "UTC",
			PrimaryLocale: "en-US", DefaultDataResidencyPolicyID: policyID,
			LifecycleState: domain.TenantLifecycleActive, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "p-seed",
		},
		&domain.DataResidencyPolicy{
			DataResidencyPolicyID: policyID, TenantID: tenantID, PolicyName: "default", PolicyCode: "DEF-" + tenantID[:8],
			ResidencyMode: domain.ResidencyModePreferredRegion, ConflictResolutionMode: domain.ConflictResolutionFailClosed,
			ActiveFlag: true, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "p-seed",
		}))
	b := &orgFixture{s: a.s, pool: a.pool, tenantID: tenantID, entityID: entityID, policyID: policyID, ctx: ctx}
	e := newEntity(b, domain.EntityStatusActive)
	e.LegalEntityID = entityID
	require.NoError(t, b.s.CreateEntity(ctx, e))
	return b
}

// Migration 000014 reverses cleanly and applies again, leaving no table and no
// key behind in between. Rollback is otherwise untested.
func TestOwnership_MigrationDownAndUpAreReversible(t *testing.T) {
	f := newORGFixture(t)
	ctx := context.Background()
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(thisFile), "../../deployments/migrations")
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		return string(b)
	}
	exists := func(q string) bool {
		var n int
		require.NoError(t, f.pool.QueryRow(ctx, q).Scan(&n))
		return n > 0
	}
	const table = `SELECT count(*) FROM pg_tables WHERE tablename = 'ownership_relationships'`
	const key = `SELECT count(*) FROM pg_constraint WHERE conname = 'le_tenant_entity_unique'`

	require.True(t, exists(table))
	require.True(t, exists(key))

	_, err := f.pool.Exec(ctx, read("000014_ownership_relationships.down.sql"))
	require.NoError(t, err)
	assert.False(t, exists(table), "down must drop the table")
	assert.False(t, exists(key), "down must drop the key it added to legal_entities")

	_, err = f.pool.Exec(ctx, read("000014_ownership_relationships.up.sql"))
	require.NoError(t, err, "up must apply again after down")
	assert.True(t, exists(table))
}
