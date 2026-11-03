package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/store"
)

// The authentication decisions this service already logs feed the console's
// Sign-in events register in the same transaction (IC-2 writers).
func TestIC2_DecisionsFeedAuthenticationEvents(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	insertPrincipal(t, ctx, pool, "p-1", "t-1", "idp|p1", "ACTIVE")

	require.NoError(t, s.RecordAuthDenied(ctx, "p-1", "t-1", "account_locked", "corr-1"))
	require.NoError(t, s.RecordMFADecision(ctx, "p-1", "t-1", "MFA_STEP_UP", "ALLOWED", "totp_verified", "corr-2"))
	require.NoError(t, s.RecordMFADecision(ctx, "p-1", "t-1", "MFA_ENROL", "ALLOWED", "totp_enrolment_started", "corr-3"))

	events, err := s.ListAuthenticationEventsPage(ctx, "t-1", store.AuthEventFilter{}, 0, 10)
	require.NoError(t, err)
	require.Len(t, events, 2, "enrolment is not a sign-in event")
	byType := map[string]domain.AuthenticationEvent{}
	for _, e := range events {
		byType[e.EventType] = e
	}
	assert.Equal(t, "FAILURE", byType["FAILED_LOGIN"].Outcome)
	assert.Equal(t, "SUCCESS", byType["STEP_UP"].Outcome)
	var details map[string]string
	require.NoError(t, json.Unmarshal(byType["FAILED_LOGIN"].Details, &details))
	assert.Equal(t, "account_locked", details["basis"])

	failures, err := s.ListAuthenticationEventsPage(ctx, "t-1", store.AuthEventFilter{Outcome: "FAILURE"}, 0, 10)
	require.NoError(t, err)
	assert.Len(t, failures, 1)
	future := time.Now().Add(time.Hour)
	none, err := s.ListAuthenticationEventsPage(ctx, "t-1", store.AuthEventFilter{Since: &future}, 0, 10)
	require.NoError(t, err)
	assert.Empty(t, none)
}

func TestIC2_ReadModelsAreAppendOnly(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	insertPrincipal(t, ctx, pool, "p-1", "t-1", "idp|p1", "ACTIVE")
	require.NoError(t, s.RecordAuthDenied(ctx, "p-1", "t-1", "bad_password", "corr-1"))

	_, err := pool.Exec(ctx, `UPDATE authentication_events SET outcome = 'SUCCESS'`)
	require.Error(t, err, "a recorded failure cannot be rewritten as a success")
	_, err = pool.Exec(ctx, `DELETE FROM authentication_events`)
	require.Error(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM principal_assurance_history`)
	require.NoError(t, err, "an empty delete touches no row, so no trigger fires")
}

func TestIC2_FactorChangesRecordAssuranceHistory(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	insertPrincipal(t, ctx, pool, "p-1", "t-1", "idp|p1", "ACTIVE")

	require.NoError(t, s.RecordMFADecision(ctx, "p-1", "t-1", "MFA_RESET", "ALLOWED", "totp_retired:lost_device", "corr-1"))
	require.NoError(t, s.RecordMFADecision(ctx, "p-1", "t-1", "MFA_RESET", "DENIED", "reset_requires_step_up", "corr-2"))

	hist, err := s.ListAssuranceHistoryPage(ctx, "t-1", "p-1", 0, 10)
	require.NoError(t, err)
	require.Len(t, hist, 1, "only an applied change is history")
	assert.Equal(t, "PASSWORD", hist[0].AssuranceLevel, "no factor remains")
	assert.Contains(t, hist[0].Reason, "MFA_RESET")
}

func TestIC2_PrincipalsPageFilterAndSearch(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	insertPrincipal(t, ctx, pool, "p-1", "t-1", "alice", "ACTIVE")
	insertPrincipal(t, ctx, pool, "p-2", "t-1", "bob", "ACTIVE")
	insertPrincipal(t, ctx, pool, "p-3", "t-1", "carol", "DISABLED")

	first, err := s.ListPrincipalsPage(ctx, "t-1", store.PrincipalListFilter{}, 0, 2)
	require.NoError(t, err)
	second, err := s.ListPrincipalsPage(ctx, "t-1", store.PrincipalListFilter{}, 2, 2)
	require.NoError(t, err)
	require.Len(t, first, 2)
	require.Len(t, second, 1)
	seen := map[string]bool{}
	for _, p := range append(first, second...) {
		assert.False(t, seen[p.PrincipalID], "pages never repeat a row")
		seen[p.PrincipalID] = true
	}

	active, err := s.ListPrincipalsPage(ctx, "t-1", store.PrincipalListFilter{Status: "ACTIVE"}, 0, 10)
	require.NoError(t, err)
	assert.Len(t, active, 2)

	found, err := s.ListPrincipalsPage(ctx, "t-1", store.PrincipalListFilter{Query: "CAROL@"}, 0, 10)
	require.NoError(t, err)
	require.Len(t, found, 1)
	assert.Equal(t, "p-3", found[0].PrincipalID)

	wild, err := s.ListPrincipalsPage(ctx, "t-1", store.PrincipalListFilter{Query: "%"}, 0, 10)
	require.NoError(t, err)
	assert.Empty(t, wild, "a % in the search box is a literal, not a wildcard")
}

func TestIC2_SessionsAndSupportLists(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	insertPrincipal(t, ctx, pool, "p-1", "t-1", "alice", "ACTIVE")
	require.NoError(t, s.InsertSessionContext(ctx, sessionFor("sc-live", "p-1", "t-1")))
	require.NoError(t, s.InsertSessionContext(ctx, sessionFor("sc-ended", "p-1", "t-1")))
	require.NoError(t, s.MarkSessionInvalidated(ctx, "sc-ended", "t-1", domain.InvalidationReasonAdminRevoke, time.Now().UTC()))

	live, err := s.ListSessionsPage(ctx, "t-1", store.SessionListFilter{State: store.SessionStateActive}, 0, 10)
	require.NoError(t, err)
	require.Len(t, live, 1)
	assert.Equal(t, "sc-live", live[0].SessionContextID)
	assert.Equal(t, "Test Principal", live[0].DisplayName)

	ended, err := s.ListSessionsPage(ctx, "t-1", store.SessionListFilter{State: store.SessionStateInvalidated}, 0, 10)
	require.NoError(t, err)
	require.Len(t, ended, 1)
	assert.Equal(t, string(domain.InvalidationReasonAdminRevoke), ended[0].InvalidationReason)

	_, err = s.ListSessionsPage(ctx, "t-1", store.SessionListFilter{State: "BOGUS"}, 0, 10)
	assert.ErrorIs(t, err, store.ErrInvalidFilter)

	support, err := s.ListSupportContextsPage(ctx, "t-1", store.SupportStatePending, 0, 10)
	require.NoError(t, err)
	assert.Empty(t, support)
}

// TestRLS_ListsNeverCrossTenant runs every console list as a NOBYPASSRLS role,
// the way the service connects on a managed host: row-level security alone
// must keep tenant B's rows out of tenant A's pages.
func TestRLS_ListsNeverCrossTenant(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	owner := store.New(pool, zap.NewNop())
	insertPrincipal(t, ctx, pool, "pa", "t-a", "alice", "ACTIVE")
	insertPrincipal(t, ctx, pool, "pb", "t-b", "bob", "ACTIVE")
	require.NoError(t, owner.InsertSessionContext(ctx, sessionFor("sc-b", "pb", "t-b")))
	require.NoError(t, owner.RecordAuthDenied(ctx, "pb", "t-b", "bad_password", "corr-b"))
	require.NoError(t, owner.RecordMFADecision(ctx, "pb", "t-b", "MFA_RESET", "ALLOWED", "x", "corr-b2"))

	for _, stmt := range []string{
		`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ic_rls_probe') THEN
		     CREATE ROLE ic_rls_probe NOLOGIN NOSUPERUSER NOBYPASSRLS; END IF; END $$`,
		`GRANT USAGE ON SCHEMA public TO ic_rls_probe`,
		`GRANT SELECT ON ALL TABLES IN SCHEMA public TO ic_rls_probe`,
	} {
		_, err := pool.Exec(ctx, stmt)
		require.NoError(t, err)
	}
	cfg := pool.Config().Copy()
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET ROLE ic_rls_probe")
		return err
	}
	probePool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(probePool.Close)
	probe := store.New(probePool, zap.NewNop())

	// The store's own tenant predicate is bypassed here on purpose: each read
	// below asks for tenant A while tenant B's rows exist, and RLS is the only
	// thing between them when the predicate is wrong.
	var leaked int
	require.NoError(t, probePool.QueryRow(ctx, `SELECT
	    (SELECT COUNT(*) FROM principals) + (SELECT COUNT(*) FROM session_contexts) +
	    (SELECT COUNT(*) FROM authentication_events) + (SELECT COUNT(*) FROM principal_assurance_history)`).Scan(&leaked))
	assert.Zero(t, leaked, "with no tenant set, the app role sees nothing")

	ps, err := probe.ListPrincipalsPage(ctx, "t-a", store.PrincipalListFilter{}, 0, 50)
	require.NoError(t, err)
	require.Len(t, ps, 1)
	assert.Equal(t, "pa", ps[0].PrincipalID)
	ss, err := probe.ListSessionsPage(ctx, "t-a", store.SessionListFilter{}, 0, 50)
	require.NoError(t, err)
	assert.Empty(t, ss)
	es, err := probe.ListAuthenticationEventsPage(ctx, "t-a", store.AuthEventFilter{}, 0, 50)
	require.NoError(t, err)
	assert.Empty(t, es)
	hs, err := probe.ListAssuranceHistoryPage(ctx, "t-a", "pb", 0, 50)
	require.NoError(t, err)
	assert.Empty(t, hs, "naming tenant B's principal from tenant A returns nothing")
	sum, err := probe.GetIdentitySummary(ctx, "t-a")
	require.NoError(t, err)
	assert.Equal(t, 1, sum.PrincipalsByStatus["ACTIVE"])
	assert.Zero(t, sum.FailedSignIns24h)
}

func TestIC2_SummaryCounts(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	insertPrincipal(t, ctx, pool, "p-1", "t-1", "alice", "ACTIVE")
	insertPrincipal(t, ctx, pool, "p-2", "t-1", "bob", "DISABLED")
	require.NoError(t, s.InsertSessionContext(ctx, sessionFor("sc-1", "p-1", "t-1")))
	require.NoError(t, s.RecordAuthDenied(ctx, "p-1", "t-1", "bad_password", "c1"))
	require.NoError(t, s.RecordMFADecision(ctx, "p-1", "t-1", "MFA_STEP_UP", "ALLOWED", "totp_verified", "c2"))

	sum, err := s.GetIdentitySummary(ctx, "t-1")
	require.NoError(t, err)
	assert.Equal(t, 1, sum.PrincipalsByStatus["ACTIVE"])
	assert.Equal(t, 1, sum.PrincipalsByStatus["DISABLED"])
	assert.Equal(t, 1, sum.ActiveSessions)
	assert.Equal(t, 1, sum.ActiveHumans)
	assert.Zero(t, sum.ActiveHumansWithMFA)
	assert.Equal(t, 1, sum.FailedSignIns24h)
	assert.Equal(t, 1, sum.Pipeline24h["CONTEXT_RESOLVED"])
	assert.Equal(t, 1, sum.Pipeline24h["STEPPED_UP"])
	assert.NotNil(t, sum.MFAByRole)
}

func TestORG01_MergedIDResolvesToSurvivor(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	insertPrincipal(t, ctx, pool, "p-surv", "t-1", "surv", "ACTIVE")
	insertPrincipal(t, ctx, pool, "p-dup", "t-1", "dup", "ACTIVE")

	res, err := s.MergeSubjects(ctx, "t-1", "p-surv", "p-dup", "p-admin", "duplicate HR import", "corr-1", 1)
	require.NoError(t, err)
	assert.Equal(t, "p-surv", res.SurvivorID)

	got, merged, err := s.ResolveSubjectID(ctx, "t-1", "p-dup")
	require.NoError(t, err)
	assert.True(t, merged)
	assert.Equal(t, "p-surv", got)
	self, merged, err := s.ResolveSubjectID(ctx, "t-1", "p-surv")
	require.NoError(t, err)
	assert.False(t, merged)
	assert.Equal(t, "p-surv", self)
	other, merged, err := s.ResolveSubjectID(ctx, "t-2", "p-dup")
	require.NoError(t, err)
	assert.False(t, merged, "an alias belongs to its tenant")
	assert.Equal(t, "p-dup", other)

	dup, err := s.GetPrincipalDetail(ctx, "t-1", "p-dup")
	require.NoError(t, err)
	assert.Equal(t, domain.PrincipalStatusDisabled, dup.Status)

	var outboxed int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM event_outbox WHERE event_type = 'identity.subject.merged'`).Scan(&outboxed))
	assert.Zero(t, outboxed, "the store enqueues exactly the records it is given")
}

func TestORG01_MergeNeverReusesID(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	insertPrincipal(t, ctx, pool, "p-a", "t-1", "a", "ACTIVE")
	insertPrincipal(t, ctx, pool, "p-b", "t-1", "b", "ACTIVE")
	insertPrincipal(t, ctx, pool, "p-c", "t-1", "c", "ACTIVE")
	_, err := s.MergeSubjects(ctx, "t-1", "p-a", "p-b", "p-admin", "dup", "c1", 1)
	require.NoError(t, err)

	// The retired id stays retired: it cannot be merged again, nor receive a merge.
	_, err = s.MergeSubjects(ctx, "t-1", "p-c", "p-b", "p-admin", "again", "c2", 2)
	assert.ErrorIs(t, err, store.ErrMergeConflict)
	_, err = s.MergeSubjects(ctx, "t-1", "p-b", "p-c", "p-admin", "into retired", "c3", 1)
	assert.ErrorIs(t, err, store.ErrMergeConflict)

	var rows int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM principals WHERE principal_id = 'p-b'`).Scan(&rows))
	assert.Equal(t, 1, rows, "the retired principal row is kept, so its id is never reissued")

	_, err = pool.Exec(ctx, `DELETE FROM identity_merges`)
	require.Error(t, err, "the alias table is append-only")
}

func TestORG01_MergeRefusesStaleOrMissing(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	insertPrincipal(t, ctx, pool, "p-a", "t-1", "a", "ACTIVE")
	insertPrincipal(t, ctx, pool, "p-b", "t-1", "b", "ACTIVE")

	_, err := s.MergeSubjects(ctx, "t-1", "p-a", "p-b", "p-admin", "dup", "c1", 7)
	assert.ErrorIs(t, err, store.ErrMergeStale)
	_, err = s.MergeSubjects(ctx, "t-1", "p-a", "p-missing", "p-admin", "dup", "c1", 1)
	assert.ErrorIs(t, err, store.ErrMergeNotFound)
	_, err = s.MergeSubjects(ctx, "t-2", "p-a", "p-b", "p-admin", "dup", "c1", 1)
	assert.True(t, errors.Is(err, store.ErrMergeNotFound), "another tenant cannot merge these subjects")

	b, err := s.GetPrincipalDetail(ctx, "t-1", "p-b")
	require.NoError(t, err)
	assert.Equal(t, domain.PrincipalStatusActive, b.Status, "a refused merge changes nothing")
}
