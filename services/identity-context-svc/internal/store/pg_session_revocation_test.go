package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/events"
	"zoiko.io/identity-context-svc/internal/store"
)

// IC-16 / D1-1.F21: identity.session.revoked commits in the invalidation's own
// transaction, and a repeated invalidation writes no second event.
func TestIC16_RevocationEventCommitsWithTheInvalidationOnce(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	insertPrincipal(t, ctx, pool, "p-1", "t-1", "alice", "ACTIVE")
	require.NoError(t, s.InsertSessionContext(ctx, sessionFor("sc-1", "p-1", "t-1")))

	rec, err := events.Render(events.EventSessionRevoked, "t-1", "", "admin-1", "corr-1", "sc-1",
		map[string]any{"session_context_id": "sc-1", "tenant_id": "t-1", "actor": "admin-1"})
	require.NoError(t, err)
	count := func() int {
		var n int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM event_outbox WHERE event_id = $1`, rec.EventID).Scan(&n))
		return n
	}
	require.NoError(t, s.MarkSessionInvalidated(ctx, "sc-1", "t-1", domain.InvalidationReasonAdminRevoke, time.Now().UTC(), rec))
	require.Equal(t, 1, count(), "the event commits with the invalidation")

	rec2, _ := events.Render(events.EventSessionRevoked, "t-1", "", "admin-1", "corr-2", "sc-1", map[string]any{"session_context_id": "sc-1"})
	require.NoError(t, s.MarkSessionInvalidated(ctx, "sc-1", "t-1", domain.InvalidationReasonAdminRevoke, time.Now().UTC(), rec2))
	var n2 int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM event_outbox WHERE event_id = $1`, rec2.EventID).Scan(&n2))
	require.Zero(t, n2, "an already-invalidated session emits nothing")
}
