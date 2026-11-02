package store_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/store"
)

// ORG-06 SuspendAssignment in the role projection: a suspended assignment is
// not framed into a session and is not a holder of its role; a resume restores
// both; and the answer does not depend on the order the two events arrive in.

func projectAssignment(t *testing.T, s *store.PgStore, tenant, id, principal, role string) {
	t.Helper()
	require.NoError(t, s.UpsertRoleAssignment(context.Background(), tenant, domain.PrincipalRoleAssignment{
		AssignmentID: id, PrincipalID: principal, RoleID: role,
		EffectiveFrom: time.Now().UTC().Add(-time.Hour), AssignedBy: "acs",
	}))
}

func framed(t *testing.T, s *store.PgStore, principal, tenant string) []string {
	t.Helper()
	got, err := s.FindActiveRoleAssignments(context.Background(), principal, tenant, nil)
	require.NoError(t, err)
	ids := []string{}
	for _, a := range got {
		ids = append(ids, a.AssignmentID)
	}
	return ids
}

func holders(t *testing.T, s *store.PgStore, role, tenant string) []string {
	t.Helper()
	got, err := s.FindPrincipalIDsByRole(context.Background(), role, tenant, time.Now().UTC())
	require.NoError(t, err)
	return got
}

func TestPgStore_SuspendedAssignmentIsNotFramedAndResumeRestoresIt(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	insertPrincipal(t, ctx, pool, "p-1", "t-1", "idp|one", "ACTIVE")
	projectAssignment(t, s, "t-1", "a-1", "p-1", "role-x")
	projectAssignment(t, s, "t-1", "a-2", "p-1", "role-y")

	require.ElementsMatch(t, []string{"a-1", "a-2"}, framed(t, s, "p-1", "t-1"))
	require.Equal(t, []string{"p-1"}, holders(t, s, "role-x", "t-1"))

	t0 := time.Now().UTC()
	require.NoError(t, s.SuspendRoleAssignment(ctx, "t-1", "a-1", t0))

	require.Equal(t, []string{"a-2"}, framed(t, s, "p-1", "t-1"), "only the suspended assignment is withheld")
	require.Empty(t, holders(t, s, "role-x", "t-1"), "a suspended assignment is not a holder of its role")
	require.Equal(t, []string{"p-1"}, holders(t, s, "role-y", "t-1"))

	require.NoError(t, s.ResumeRoleAssignment(ctx, "t-1", "a-1", t0.Add(time.Minute)))
	require.ElementsMatch(t, []string{"a-1", "a-2"}, framed(t, s, "p-1", "t-1"))
	require.Equal(t, []string{"p-1"}, holders(t, s, "role-x", "t-1"))
}

// Delivery is at least once and not ordered. The latest event by its own time
// decides, whichever is consumed last.
func TestPgStore_SuspensionIsOrderIndependentAndIdempotent(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	insertPrincipal(t, ctx, pool, "p-1", "t-1", "idp|one", "ACTIVE")
	projectAssignment(t, s, "t-1", "a-1", "p-1", "role-x")
	base := time.Now().UTC()

	suspend, resume := base.Add(1*time.Minute), base.Add(2*time.Minute)

	// The resume (later) is consumed BEFORE the suspend (earlier): still active.
	require.NoError(t, s.ResumeRoleAssignment(ctx, "t-1", "a-1", resume))
	require.NoError(t, s.SuspendRoleAssignment(ctx, "t-1", "a-1", suspend))
	require.Equal(t, []string{"a-1"}, framed(t, s, "p-1", "t-1"), "an older suspension must not override a newer resume")

	// A redelivered suspend changes nothing.
	require.NoError(t, s.SuspendRoleAssignment(ctx, "t-1", "a-1", suspend))
	require.Equal(t, []string{"a-1"}, framed(t, s, "p-1", "t-1"))

	// A genuinely later suspension wins, and redelivering the old resume does not undo it.
	require.NoError(t, s.SuspendRoleAssignment(ctx, "t-1", "a-1", resume.Add(time.Minute)))
	require.Empty(t, framed(t, s, "p-1", "t-1"))
	require.NoError(t, s.ResumeRoleAssignment(ctx, "t-1", "a-1", resume))
	require.Empty(t, framed(t, s, "p-1", "t-1"), "a redelivered older resume must not lift a newer suspension")

	// Nor does redelivering the OLD suspension after the newer one: it would
	// otherwise move suspended_at backwards, behind the resume, and lift the hold.
	require.NoError(t, s.SuspendRoleAssignment(ctx, "t-1", "a-1", suspend))
	require.Empty(t, framed(t, s, "p-1", "t-1"), "a redelivered older suspension must not undo a newer one")

	// And the newest resume lifts it.
	require.NoError(t, s.ResumeRoleAssignment(ctx, "t-1", "a-1", resume.Add(2*time.Minute)))
	require.Equal(t, []string{"a-1"}, framed(t, s, "p-1", "t-1"))
}

// A resume is not a re-grant: it cannot reopen what a revocation closed.
func TestPgStore_ResumeDoesNotReopenARevokedAssignment(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	insertPrincipal(t, ctx, pool, "p-1", "t-1", "idp|one", "ACTIVE")
	projectAssignment(t, s, "t-1", "a-1", "p-1", "role-x")
	now := time.Now().UTC()

	require.NoError(t, s.SuspendRoleAssignment(ctx, "t-1", "a-1", now))
	require.NoError(t, s.EndRoleAssignment(ctx, "t-1", "a-1", now.Add(time.Second)))
	time.Sleep(1100 * time.Millisecond) // past the end instant
	require.NoError(t, s.ResumeRoleAssignment(ctx, "t-1", "a-1", now.Add(time.Minute)))

	require.Empty(t, framed(t, s, "p-1", "t-1"), "a resumed assignment that was revoked in the meantime is framed again")
}

func TestPgStore_SuspensionIsTenantScopedAndToleratesUnknownAssignments(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	insertPrincipal(t, ctx, pool, "p-1", "t-1", "idp|one", "ACTIVE")
	projectAssignment(t, s, "t-1", "a-1", "p-1", "role-x")

	// Another tenant naming the same assignment id suspends nothing here.
	require.NoError(t, s.SuspendRoleAssignment(ctx, "t-2", "a-1", time.Now().UTC()))
	require.Equal(t, []string{"a-1"}, framed(t, s, "p-1", "t-1"))

	// An assignment the projection never held is not an error.
	require.NoError(t, s.SuspendRoleAssignment(ctx, "t-1", "never-projected", time.Now().UTC()))
	require.NoError(t, s.ResumeRoleAssignment(ctx, "t-1", "never-projected", time.Now().UTC()))

	// Required arguments.
	require.Error(t, s.SuspendRoleAssignment(ctx, "", "a-1", time.Now()))
	require.Error(t, s.ResumeRoleAssignment(ctx, "t-1", "", time.Now()))
}

// Migration 000016 refuses to drop the suspension columns while an assignment is
// suspended (dropping them would start framing it again), and reverses cleanly
// once nothing is.
func TestPgStore_Migration000016DownIsRefusedWhileAnAssignmentIsSuspended(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	insertPrincipal(t, ctx, pool, "p-1", "t-1", "idp|one", "ACTIVE")
	projectAssignment(t, s, "t-1", "a-1", "p-1", "role-x")

	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(thisFile), "../../deployments/migrations")
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		return string(b)
	}

	now := time.Now().UTC()
	require.NoError(t, s.SuspendRoleAssignment(ctx, "t-1", "a-1", now))
	_, err := pool.Exec(ctx, read("000016_role_assignment_suspension.down.sql"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "000016 down refused")

	require.NoError(t, s.ResumeRoleAssignment(ctx, "t-1", "a-1", now.Add(time.Minute)))
	_, err = pool.Exec(ctx, read("000016_role_assignment_suspension.down.sql"))
	require.NoError(t, err, "with nothing suspended the down migration applies")
	_, err = pool.Exec(ctx, read("000016_role_assignment_suspension.up.sql"))
	require.NoError(t, err, "and the up migration applies again")
}
