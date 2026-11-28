package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"zoiko.io/delegated-authority-svc/internal/domain"
	"zoiko.io/delegated-authority-svc/internal/store"
)

// DA-1 (API-4): keyset pages on (created_at, delegation_id) DESC return every
// grant exactly once, including grants that share created_at.
func TestDA1_KeysetPagesEveryGrantOnce(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool)
	ctx := tenantCtx(testTenantA)
	now := time.Now().UTC().Truncate(time.Second)
	want := map[string]bool{}
	for i := 0; i < 5; i++ {
		id := uuid.NewString()
		want[id] = true
		// Distinct actions, so the overlap constraint does not apply.
		g := grant(id, "corr-"+id, testEntityA, principalSelf, principalOther, now, now.Add(24*time.Hour))
		g.ActionType = "ACTION_" + id[:8]
		_, err := s.CreateDelegation(ctx, g)
		require.NoError(t, err)
	}
	got := map[string]int{}
	f := domain.ListDelegationsFilter{LegalEntityID: testEntityA, Limit: 2}
	for page := 0; page < 10; page++ {
		list, err := s.ListDelegations(ctx, f)
		require.NoError(t, err)
		for _, d := range list {
			got[d.DelegationID]++
		}
		if len(list) < f.Limit {
			break
		}
		last := list[len(list)-1]
		f.AfterCreatedAt, f.AfterID = &last.CreatedAt, last.DelegationID
	}
	require.Len(t, got, len(want))
	for id, n := range got {
		require.True(t, want[id])
		require.Equal(t, 1, n, "%s returned %d times", id, n)
	}
}
