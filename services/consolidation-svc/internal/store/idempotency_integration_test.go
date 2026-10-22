//go:build integration

package store_test

// A14: the command_idempotency claims are written in the same transaction as
// the command they protect, are unique per tenant, replay the same command on
// the same resource, and conflict on anything else. These tests use a real
// Postgres because the guarantee is the transaction and the primary key, not
// the Go code around them.

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"zoiko.io/consolidation-svc/internal/domain"
	svcmiddleware "zoiko.io/consolidation-svc/internal/middleware"
)

// commandClaimCount counts the rows visible to tenantID under the table's RLS
// policy. An empty tenantID leaves app.tenant_id unset, which must expose no
// rows at all.
func commandClaimCount(t *testing.T, pool *pgxpool.Pool, tenantID string) int {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	if tenantID != "" {
		_, err = tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID)
		require.NoError(t, err)
	}
	var n int
	require.NoError(t, tx.QueryRow(ctx, "SELECT count(*) FROM command_idempotency").Scan(&n))
	return n
}

// TestPgStore_A14_StartRun_SameKey_ReplaysRun_RealDB is the store half of
// A14's central requirement: the second CreateRun under the same key returns
// the first run, in whatever state it has reached, and no second row exists.
func TestPgStore_A14_StartRun_SameKey_ReplaysRun_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := newTestStore(pool)
	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	key := "start-run-" + uuid.NewString()

	run := newTestRun(tenantID, "le-group")
	run.Status = "RUNNING"
	created, replay, err := s.CreateRun(ctx, run, key, "principal-1", "corr-1")
	require.NoError(t, err)
	require.False(t, replay)
	require.Equal(t, run.ConsolidationRunID, created.ConsolidationRunID)

	require.NoError(t, s.CompleteRun(ctx, run.ConsolidationRunID, "COMPLETED", 0,
		time.Now().UTC(), "principal-1", "corr-1", 0, nil))

	// A retry is a fresh request with its own server-generated run id; only
	// the key ties it to the first attempt.
	retry := newTestRun(tenantID, "le-group")
	retry.Status = "RUNNING"
	replayed, replay, err := s.CreateRun(ctx, retry, key, "principal-1", "corr-2")
	require.NoError(t, err)
	require.True(t, replay)
	require.Equal(t, run.ConsolidationRunID, replayed.ConsolidationRunID)
	require.Equal(t, "COMPLETED", replayed.Status, "replay must answer from the run's persisted state")

	runs, err := s.ListRuns(ctx, "le-group")
	require.NoError(t, err)
	require.Len(t, runs, 1, "a retried StartRun must not create a second run")
	require.Equal(t, 1, commandClaimCount(t, pool, tenantID))
}

// TestPgStore_A14_StartRun_SameKeyForAnotherRequest_Conflicts_RealDB proves
// the key is bound to the run's inputs, not just to the command name.
func TestPgStore_A14_StartRun_SameKeyForAnotherRequest_Conflicts_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := newTestStore(pool)
	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	key := "start-run-conflict-" + uuid.NewString()

	run := newTestRun(tenantID, "le-group")
	run.Status = "RUNNING"
	_, _, err := s.CreateRun(ctx, run, key, "principal-1", "corr-1")
	require.NoError(t, err)

	other := newTestRun(tenantID, "le-group")
	other.Status = "RUNNING"
	other.FiscalPeriod = "2026-08"
	_, _, err = s.CreateRun(ctx, other, key, "principal-1", "corr-2")
	require.ErrorIs(t, err, domain.ErrIdempotencyKeyConflict)

	runs, err := s.ListRuns(ctx, "le-group")
	require.NoError(t, err)
	require.Len(t, runs, 1)
}

// TestPgStore_A14_AdjustmentCommands_ReplayUnderSameKey_RealDB walks the whole
// lifecycle with one key per command and retries each: every retry replays the
// resource as it now stands, never moves it twice, and leaves one claim.
func TestPgStore_A14_AdjustmentCommands_ReplayUnderSameKey_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := newTestStore(pool)
	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	adj := newTestAdjustment(tenantID, "group-1", "preparer-1")
	createKey := "create-adj-" + uuid.NewString()
	created, replay, err := s.CreateAdjustment(ctx, adj, createKey, "corr-1")
	require.NoError(t, err)
	require.False(t, replay)
	require.Equal(t, adj.ConsolidationAdjustmentID, created.ConsolidationAdjustmentID)

	duplicate := newTestAdjustment(tenantID, "group-1", "preparer-1")
	replayedCreate, replay, err := s.CreateAdjustment(ctx, duplicate, createKey, "corr-2")
	require.NoError(t, err)
	require.True(t, replay)
	require.Equal(t, adj.ConsolidationAdjustmentID, replayedCreate.ConsolidationAdjustmentID)
	adjustments, err := s.ListAdjustments(ctx, "group-1", "2026-07")
	require.NoError(t, err)
	require.Len(t, adjustments, 1, "a retried create must not create a second adjustment")

	approveKey := "approve-adj-" + uuid.NewString()
	approved, replay, err := s.ApproveAdjustment(ctx, adj.ConsolidationAdjustmentID, "approver-1", approveKey, "corr-1")
	require.NoError(t, err)
	require.False(t, replay)
	require.Equal(t, domain.AdjustmentStatusApproved, approved.Status)
	reapproved, replay, err := s.ApproveAdjustment(ctx, adj.ConsolidationAdjustmentID, "approver-1", approveKey, "corr-2")
	require.NoError(t, err)
	require.True(t, replay)
	require.Equal(t, domain.AdjustmentStatusApproved, reapproved.Status)

	postKey := "post-adj-" + uuid.NewString()
	journalID := uuid.NewString()
	posted, replay, err := s.MarkAdjustmentPosted(ctx, adj.ConsolidationAdjustmentID, "poster-1", journalID, postKey, "corr-1")
	require.NoError(t, err)
	require.False(t, replay)
	require.Equal(t, domain.AdjustmentStatusPosted, posted.Status)
	require.NotNil(t, posted.ConsolidationBookJournalID)
	require.Equal(t, journalID, *posted.ConsolidationBookJournalID)
	reposted, replay, err := s.MarkAdjustmentPosted(ctx, adj.ConsolidationAdjustmentID, "poster-1", uuid.NewString(), postKey, "corr-2")
	require.NoError(t, err)
	require.True(t, replay)
	require.Equal(t, domain.AdjustmentStatusPosted, reposted.Status)
	require.Equal(t, journalID, *reposted.ConsolidationBookJournalID,
		"a replay must report the journal the first post recorded, not the retry's")

	reverseKey := "reverse-adj-" + uuid.NewString()
	reversed, replay, err := s.ReverseAdjustment(ctx, adj.ConsolidationAdjustmentID, "reverser-1", "correcting an error", nil, reverseKey, "corr-1")
	require.NoError(t, err)
	require.False(t, replay)
	require.Equal(t, domain.AdjustmentStatusReversed, reversed.Status)
	rereversed, replay, err := s.ReverseAdjustment(ctx, adj.ConsolidationAdjustmentID, "reverser-1", "correcting an error", nil, reverseKey, "corr-2")
	require.NoError(t, err)
	require.True(t, replay)
	require.Equal(t, domain.AdjustmentStatusReversed, rereversed.Status)

	// The handler's pre-flight lookup sees each recorded command...
	for _, tc := range []struct {
		command string
		key     string
	}{
		{domain.CommandCreateAdjustment, createKey},
		{domain.CommandApproveAdjustment, approveKey},
		{domain.CommandPostAdjustment, postKey},
		{domain.CommandReverseAdjustment, reverseKey},
	} {
		ok, err := s.ReplayedAdjustmentCommand(ctx, adj.ConsolidationAdjustmentID, tc.command, tc.key)
		require.NoError(t, err)
		require.True(t, ok, tc.command)
	}
	// ...and refuses a key claimed for another command.
	_, err = s.ReplayedAdjustmentCommand(ctx, adj.ConsolidationAdjustmentID, domain.CommandReverseAdjustment, postKey)
	require.ErrorIs(t, err, domain.ErrIdempotencyKeyConflict)

	require.Equal(t, 4, commandClaimCount(t, pool, tenantID), "one claim per successful command")
}

// TestPgStore_A14_RefusedCommandDoesNotConsumeKey_RealDB proves the claim is
// recorded in the command's own transaction: a rejected command leaves the key
// unused, so the client may correct the request and retry with it.
func TestPgStore_A14_RefusedCommandDoesNotConsumeKey_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := newTestStore(pool)
	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	adj := newTestAdjustment(tenantID, "group-1", "preparer-1")
	_, _, err := s.CreateAdjustment(ctx, adj, "create-"+uuid.NewString(), "corr-1")
	require.NoError(t, err)

	key := "retry-after-refusal-" + uuid.NewString()
	_, _, err = s.MarkAdjustmentPosted(ctx, adj.ConsolidationAdjustmentID, "poster-1", uuid.NewString(), key, "corr-1")
	require.ErrorIs(t, err, domain.ErrInvalidAdjustmentTransition)
	require.Equal(t, 1, commandClaimCount(t, pool, tenantID), "the refused post must not record a claim")

	approved, replay, err := s.ApproveAdjustment(ctx, adj.ConsolidationAdjustmentID, "approver-1", key, "corr-2")
	require.NoError(t, err)
	require.False(t, replay, "the key was never consumed, so this is a first attempt")
	require.Equal(t, domain.AdjustmentStatusApproved, approved.Status)
	require.Equal(t, 2, commandClaimCount(t, pool, tenantID))
}

// TestCommandIdempotency_RLS_IsolatesTenants_RealDB proves the table's policy:
// the key is unique per tenant, one tenant's claim is invisible to another, and
// a connection with no app.tenant_id sees nothing.
func TestCommandIdempotency_RLS_IsolatesTenants_RealDB(t *testing.T) {
	// The embedded superuser bypasses row-level security, so this test runs as
	// the same NOSUPERUSER NOBYPASSRLS role the service uses in deployment.
	app := appPool(t, openTestPool(t))
	s := newTestStore(app)
	tenantA, tenantB := uuid.NewString(), uuid.NewString()
	ctxA := svcmiddleware.WithTenant(context.Background(), tenantA)
	ctxB := svcmiddleware.WithTenant(context.Background(), tenantB)
	key := "shared-key-" + uuid.NewString()

	runA := newTestRun(tenantA, "group-a")
	runA.Status = "RUNNING"
	_, _, err := s.CreateRun(ctxA, runA, key, "principal-a", "corr-a")
	require.NoError(t, err)

	runB := newTestRun(tenantB, "group-b")
	runB.Status = "RUNNING"
	createdB, replay, err := s.CreateRun(ctxB, runB, key, "principal-b", "corr-b")
	require.NoError(t, err, "the same key in another tenant is a different claim")
	require.False(t, replay)
	require.Equal(t, runB.ConsolidationRunID, createdB.ConsolidationRunID)

	retryA := newTestRun(tenantA, "group-a")
	retryA.Status = "RUNNING"
	replayedA, replay, err := s.CreateRun(ctxA, retryA, key, "principal-a", "corr-a2")
	require.NoError(t, err)
	require.True(t, replay)
	require.Equal(t, runA.ConsolidationRunID, replayedA.ConsolidationRunID)

	require.Equal(t, 1, commandClaimCount(t, app, tenantA))
	require.Equal(t, 1, commandClaimCount(t, app, tenantB))
	require.Equal(t, 0, commandClaimCount(t, app, ""), "no tenant context must expose no claims")
}

// 000008's down refuses while any claim exists and round-trips on an empty
// table — dropping it would forget that a key already created a run.
func TestMigration000008_Down_RealDB(t *testing.T) {
	pool := openTestPool(t)
	_, filename, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(filename), "../../deployments/migrations")
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		return string(b)
	}
	ctx := context.Background()

	_, err := pool.Exec(ctx, read("000008_add_command_idempotency.down.sql"))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, read("000008_add_command_idempotency.up.sql"))
	require.NoError(t, err)

	s := newTestStore(pool)
	tenantID := uuid.NewString()
	tctx := svcmiddleware.WithTenant(ctx, tenantID)
	run := newTestRun(tenantID, "le-group")
	run.Status = "RUNNING"
	require.NoError(t, createRun(s, tctx, run, "principal-1", "corr-1"))

	_, err = pool.Exec(ctx, read("000008_add_command_idempotency.down.sql"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot roll back 000008")
	require.Equal(t, 1, commandClaimCount(t, pool, tenantID), "the refused rollback must keep the claim")
}
