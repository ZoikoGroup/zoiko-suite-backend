package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/store"
)

// S1-4: a grant delivered after its revocation must not re-open the role.
func TestPgStore_RevokeBeforeGrantStaysRevoked(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	insertPrincipal(t, ctx, pool, "p-1", "t-1", "idp|one", "ACTIVE")
	now := time.Now().UTC()

	// The revoke arrives first: nothing is projected yet, a tombstone is.
	require.NoError(t, s.EndRoleAssignment(ctx, "t-1", "a-late", now))
	// Then the late grant, open-ended.
	require.NoError(t, s.UpsertRoleAssignment(ctx, "t-1", domain.PrincipalRoleAssignment{
		AssignmentID: "a-late", PrincipalID: "p-1", RoleID: "role-x", EffectiveFrom: now.Add(-time.Hour), AssignedBy: "acs",
	}))
	active, err := s.FindActiveRoleAssignments(ctx, "p-1", "t-1", nil)
	require.NoError(t, err)
	require.Empty(t, active, "a revoked assignment was re-opened by a late grant")

	// A redelivered grant cannot extend an ended assignment either.
	require.NoError(t, s.UpsertRoleAssignment(ctx, "t-1", domain.PrincipalRoleAssignment{
		AssignmentID: "a-late", PrincipalID: "p-1", RoleID: "role-x", EffectiveFrom: now.Add(-time.Hour), AssignedBy: "acs",
	}))
	active, err = s.FindActiveRoleAssignments(ctx, "p-1", "t-1", nil)
	require.NoError(t, err)
	require.Empty(t, active)

	// The ordinary order still works: grant, then revoke.
	require.NoError(t, s.UpsertRoleAssignment(ctx, "t-1", domain.PrincipalRoleAssignment{
		AssignmentID: "a-normal", PrincipalID: "p-1", RoleID: "role-y", EffectiveFrom: now.Add(-time.Hour), AssignedBy: "acs",
	}))
	active, err = s.FindActiveRoleAssignments(ctx, "p-1", "t-1", nil)
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.NoError(t, s.EndRoleAssignment(ctx, "t-1", "a-normal", time.Now().UTC()))
	active, err = s.FindActiveRoleAssignments(ctx, "p-1", "t-1", nil)
	require.NoError(t, err)
	require.Empty(t, active)

	// Tombstones are tenant-scoped: another tenant's revoke of the same id
	// does not end this tenant's assignment.
	insertPrincipal(t, ctx, pool, "p-2", "t-2", "idp|two", "ACTIVE")
	require.NoError(t, s.EndRoleAssignment(ctx, "t-1", "a-shared", now))
	require.NoError(t, s.UpsertRoleAssignment(ctx, "t-2", domain.PrincipalRoleAssignment{
		AssignmentID: "a-shared", PrincipalID: "p-2", RoleID: "role-z", EffectiveFrom: now.Add(-time.Hour), AssignedBy: "acs",
	}))
	active, err = s.FindActiveRoleAssignments(ctx, "p-2", "t-2", nil)
	require.NoError(t, err)
	require.Len(t, active, 1)
}
