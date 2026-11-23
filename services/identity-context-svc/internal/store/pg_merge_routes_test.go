// The two merge routes against a real Postgres (TEST_DATABASE_URL, like pg_store_test.go).
package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/store"
)

// MergeSubjects (POST /v1/subjects/{id}/merge, identity_merges) and
// MergeIdentities (POST /v1/identities/{id}/merge, principal_merges) keep
// separate ledgers. A duplicate merged by either is merged for both: the other
// route refuses to merge it again, and both alias reads resolve it.

func TestMergeRoutes_ASubjectsMergeHoldsForTheIdentitiesRoute(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	for _, id := range []string{"p-a", "p-b", "p-c"} {
		insertPrincipal(t, ctx, pool, id, "t-1", "idp|"+id, "ACTIVE")
	}
	_, err := s.MergeSubjects(ctx, "t-1", "p-a", "p-b", "p-admin", "duplicate HR import", "c1", 1)
	require.NoError(t, err)

	m, created, err := s.MergeIdentities(ctx, "t-1", "p-a", "p-b", "duplicate HR import", "p-admin", "c2", eventFn)
	require.NoError(t, err, "the same merge asked again by the other route is a replay")
	assert.False(t, created)
	assert.Equal(t, "p-a", m.SurvivorID)
	assert.Equal(t, "p-b", m.DuplicateID)
	assert.Empty(t, m.LinkOutcomes)

	_, _, err = s.MergeIdentities(ctx, "t-1", "p-c", "p-b", "again", "p-admin", "c3", eventFn)
	assert.ErrorIs(t, err, store.ErrMergeConflict, "p-b was merged into p-a already")

	into, err := s.MergedInto(ctx, "t-1", "p-b")
	require.NoError(t, err)
	assert.Equal(t, "p-a", into)
	ident, err := s.GetSubjectIdentity(ctx, "t-1", "p-b")
	require.NoError(t, err)
	require.NotNil(t, ident)
	assert.Equal(t, "p-a", ident.MergedInto)

	var recorded int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM principal_merges WHERE duplicate_id = 'p-b'`).Scan(&recorded))
	assert.Zero(t, recorded, "a replay records no second merge")
}

func TestMergeRoutes_AnIdentitiesMergeHoldsForTheSubjectsRoute(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	for _, id := range []string{"p-a", "p-b", "p-c"} {
		insertPrincipal(t, ctx, pool, id, "t-1", "idp|"+id, "ACTIVE")
	}
	_, created, err := s.MergeIdentities(ctx, "t-1", "p-a", "p-b", "duplicate HR import", "p-admin", "c1", eventFn)
	require.NoError(t, err)
	require.True(t, created)

	got, merged, err := s.ResolveSubjectID(ctx, "t-1", "p-b")
	require.NoError(t, err)
	assert.True(t, merged)
	assert.Equal(t, "p-a", got)

	_, err = s.MergeSubjects(ctx, "t-1", "p-c", "p-b", "p-admin", "again", "c2", 1)
	assert.ErrorIs(t, err, store.ErrMergeConflict, "p-b was merged into p-a already")

	sum, err := s.GetIdentitySummary(ctx, "t-1")
	require.NoError(t, err)
	assert.Equal(t, 1, sum.MergedIdentities, "the console counts merges made by either route")
}
