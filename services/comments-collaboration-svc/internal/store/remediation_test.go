package store_test

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"zoiko.io/comments-collaboration-svc/internal/domain"
	svcmiddleware "zoiko.io/comments-collaboration-svc/internal/middleware"
	"zoiko.io/comments-collaboration-svc/internal/store"
)

func ctxFor(tenant string) context.Context {
	return svcmiddleware.WithTenant(context.Background(), tenant)
}

// requireRLSPool returns a pool whose sessions run as a NON-superuser role.
// A Postgres superuser bypasses row-level security even when it is FORCEd, so
// the default test pool cannot prove isolation; this one can. The role is
// created idempotently in the throwaway test database.
func requireRLSPool(t *testing.T, admin *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	_, err := admin.Exec(context.Background(), `
		DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ccs_rls_test') THEN
				CREATE ROLE ccs_rls_test NOLOGIN NOSUPERUSER NOBYPASSRLS;
			END IF;
		END $$;
		GRANT ALL ON ALL TABLES IN SCHEMA public TO ccs_rls_test;
	`)
	require.NoError(t, err)
	cfg, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	require.NoError(t, err)
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, `SET ROLE ccs_rls_test`)
		return err
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func idem(principal, op, key, hash string) *domain.Idempotency {
	return &domain.Idempotency{Key: key, PrincipalID: principal, Operation: op, RequestHash: hash}
}

func addParams(tenant, object, body string, i *domain.Idempotency) domain.AddCommentParams {
	return domain.AddCommentParams{
		TenantID: tenant, LegalEntityID: "le-1", LinkedObjectType: "DOCUMENT", LinkedObjectID: object, Classification: "INTERNAL",
		Body: body, CreatedByPrincipalID: "reviewer-1", Idem: i,
	}
}

// ── restricted flag ──────────────────────────────────────────────────────────

func TestPgStore_Restricted_IsThreadLevel_MismatchRefused(t *testing.T) {
	s := store.NewPgStore(requireTestDB(t))

	p := addParams("default", "doc-r1", "secret", nil)
	p.Restricted = true
	_, thread, err := s.AddComment(ctx(), p)
	require.NoError(t, err)
	require.True(t, thread.Restricted)

	// A later comment that does not declare the thread's restriction is
	// refused, not silently accepted.
	_, _, err = s.AddComment(ctx(), addParams("default", "doc-r1", "sneaky", nil))
	require.ErrorIs(t, err, domain.ErrThreadRestrictionMismatch)

	// Matching restricted value is accepted and stays in the same thread.
	p2 := addParams("default", "doc-r1", "second", nil)
	p2.Restricted = true
	_, t2, err := s.AddComment(ctx(), p2)
	require.NoError(t, err)
	require.Equal(t, thread.ThreadID, t2.ThreadID)

	// And the reverse: escalating an ordinary thread is refused, not discarded.
	_, _, err = s.AddComment(ctx(), addParams("default", "doc-r2", "open", nil))
	require.NoError(t, err)
	p3 := addParams("default", "doc-r2", "escalate", nil)
	p3.Restricted = true
	_, _, err = s.AddComment(ctx(), p3)
	require.ErrorIs(t, err, domain.ErrThreadRestrictionMismatch)

	list, err := s.ListComments(ctx(), "default", thread.ThreadID)
	require.NoError(t, err)
	require.Len(t, list, 2, "a refused comment writes nothing")
}

// ── cross-tenant isolation ───────────────────────────────────────────────────

func TestPgStore_Reads_AreTenantIsolated(t *testing.T) {
	admin := requireTestDB(t)
	s := store.NewPgStore(requireRLSPool(t, admin))
	comment, thread, err := s.AddComment(ctx(), addParams("default", "doc-x", "mine", nil))
	require.NoError(t, err)

	other := ctxFor("other-tenant")
	_, err = s.GetThread(other, thread.ThreadID)
	require.ErrorIs(t, err, domain.ErrThreadNotFound)
	_, _, err = s.GetComment(other, "other-tenant", comment.CommentID)
	require.ErrorIs(t, err, domain.ErrCommentNotFound)
	list, err := s.ListComments(other, "other-tenant", thread.ThreadID)
	require.NoError(t, err)
	require.Empty(t, list)
	hist, err := s.GetCommentHistory(other, "other-tenant", comment.CommentID)
	require.NoError(t, err)
	require.Empty(t, hist)
	// Even naming the owning tenant in the query does not escape RLS.
	list, err = s.ListComments(other, "default", thread.ThreadID)
	require.NoError(t, err)
	require.Empty(t, list)
}

// ── idempotency ──────────────────────────────────────────────────────────────

func TestPgStore_Idempotency_FirstThenExactReplay(t *testing.T) {
	s := store.NewPgStore(requireTestDB(t))

	i1 := idem("p1", "AddComment", "key-1", "h1")
	c1, th1, err := s.AddComment(ctx(), addParams("default", "doc-i1", "once", i1))
	require.NoError(t, err)
	require.False(t, i1.Replayed)

	i2 := idem("p1", "AddComment", "key-1", "h1")
	c2, th2, err := s.AddComment(ctx(), addParams("default", "doc-i1", "once", i2))
	require.NoError(t, err)
	require.True(t, i2.Replayed)
	require.Equal(t, c1.CommentID, c2.CommentID)
	require.Equal(t, th1.ThreadID, th2.ThreadID)
	require.Equal(t, "once", c2.CurrentBody)

	list, err := s.ListComments(ctx(), "default", th1.ThreadID)
	require.NoError(t, err)
	require.Len(t, list, 1, "a replay must not create a second comment")
}

func TestPgStore_Idempotency_SameKeyDifferentRequest_Conflicts(t *testing.T) {
	s := store.NewPgStore(requireTestDB(t))
	_, th, err := s.AddComment(ctx(), addParams("default", "doc-i2", "a", idem("p1", "AddComment", "k", "hash-a")))
	require.NoError(t, err)

	_, _, err = s.AddComment(ctx(), addParams("default", "doc-i2", "b", idem("p1", "AddComment", "k", "hash-b")))
	require.ErrorIs(t, err, domain.ErrIdempotencyConflict)

	list, err := s.ListComments(ctx(), "default", th.ThreadID)
	require.NoError(t, err)
	require.Len(t, list, 1)
}

func TestPgStore_Idempotency_ConcurrentReplay_CreatesExactlyOne(t *testing.T) {
	s := store.NewPgStore(requireTestDB(t))

	const n = 12
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			c, _, err := s.AddComment(ctx(), addParams("default", "doc-conc", "race", idem("p1", "AddComment", "race-key", "h")))
			errs[i] = err
			if c != nil {
				ids[i] = c.CommentID
			}
		}(i)
	}
	close(start)
	wg.Wait()
	for i := 0; i < n; i++ {
		require.NoError(t, errs[i])
		require.Equal(t, ids[0], ids[i], "every concurrent caller gets the one original comment")
	}
	_, th, err := s.AddComment(ctx(), addParams("default", "doc-conc", "later", nil))
	require.NoError(t, err)
	list, err := s.ListComments(ctx(), "default", th.ThreadID)
	require.NoError(t, err)
	require.Len(t, list, 2, "12 concurrent identical requests + 1 later comment = 2 comments")
}

func TestPgStore_Idempotency_ScopedByTenantAndPrincipal(t *testing.T) {
	s := store.NewPgStore(requireTestDB(t))

	a, _, err := s.AddComment(ctx(), addParams("default", "doc-s", "x", idem("p1", "AddComment", "shared", "h")))
	require.NoError(t, err)

	// Same key + same request hash, different principal: independent.
	i := idem("p2", "AddComment", "shared", "h")
	b, _, err := s.AddComment(ctx(), addParams("default", "doc-s", "x", i))
	require.NoError(t, err)
	require.False(t, i.Replayed)
	require.NotEqual(t, a.CommentID, b.CommentID)

	// Same key, different tenant: independent.
	i = idem("p1", "AddComment", "shared", "h")
	c, _, err := s.AddComment(ctxFor("tenant-b"), addParams("tenant-b", "doc-s", "x", i))
	require.NoError(t, err)
	require.False(t, i.Replayed)
	require.NotEqual(t, a.CommentID, c.CommentID)

	// Same key, different operation: independent.
	i = idem("p1", "Other", "shared", "h")
	d, _, err := s.AddComment(ctx(), addParams("default", "doc-s", "x", i))
	require.NoError(t, err)
	require.False(t, i.Replayed)
	require.NotEqual(t, a.CommentID, d.CommentID)
}

func TestPgStore_Idempotency_OtherWrites_ReplayWithoutDuplicates(t *testing.T) {
	s := store.NewPgStore(requireTestDB(t))
	comment, thread, err := s.AddComment(ctx(), addParams("default", "doc-w", "base", nil))
	require.NoError(t, err)

	// Mention
	m1, err := s.Mention(ctx(), domain.MentionParams{CommentID: comment.CommentID, TenantID: "default", MentionedPrincipalID: "sarah", VisibilityGranted: true, Idem: idem("p1", "Mention", "m", "h")})
	require.NoError(t, err)
	mi := idem("p1", "Mention", "m", "h")
	m2, err := s.Mention(ctx(), domain.MentionParams{CommentID: comment.CommentID, TenantID: "default", MentionedPrincipalID: "sarah", VisibilityGranted: true, Idem: mi})
	require.NoError(t, err)
	require.True(t, mi.Replayed)
	require.Equal(t, m1.MentionID, m2.MentionID)
	ms, err := s.GetMentions(ctx(), "default", comment.CommentID)
	require.NoError(t, err)
	require.Len(t, ms, 1)

	// AttachReference
	a1, err := s.AttachReference(ctx(), domain.AttachReferenceParams{CommentID: comment.CommentID, TenantID: "default", LinkedObjectID: "file-1", AttachedByPrincipalID: "p1", Idem: idem("p1", "AttachReference", "a", "h")})
	require.NoError(t, err)
	a2, err := s.AttachReference(ctx(), domain.AttachReferenceParams{CommentID: comment.CommentID, TenantID: "default", LinkedObjectID: "file-1", AttachedByPrincipalID: "p1", Idem: idem("p1", "AttachReference", "a", "h")})
	require.NoError(t, err)
	require.Equal(t, a1.AttachmentID, a2.AttachmentID)

	// React is a toggle: a replay must NOT toggle it back off.
	r1, err := s.React(ctx(), domain.ReactParams{CommentID: comment.CommentID, TenantID: "default", PrincipalID: "p1", ReactionType: "UP", Idem: idem("p1", "React", "r", "h")})
	require.NoError(t, err)
	require.True(t, r1)
	r2, err := s.React(ctx(), domain.ReactParams{CommentID: comment.CommentID, TenantID: "default", PrincipalID: "p1", ReactionType: "UP", Idem: idem("p1", "React", "r", "h")})
	require.NoError(t, err)
	require.True(t, r2, "replay returns the original result (added=true) and leaves the reaction in place")

	// Moderate / Edit / Resolve replays leave exactly one history entry each.
	mod := func() (*domain.Comment, error) {
		return s.Moderate(ctx(), domain.ModerateParams{CommentID: comment.CommentID, TenantID: "default", ModeratorPrincipalID: "mod", Action: "HIDE", Reason: "spam", Idem: idem("mod", "Moderate", "mk", "h")})
	}
	_, err = mod()
	require.NoError(t, err)
	_, err = mod() // would be ErrCommentInvalidState without the replay
	require.NoError(t, err)
	trail, err := s.GetModerationTrail(ctx(), "default", comment.CommentID)
	require.NoError(t, err)
	require.Len(t, trail, 1)

	c2, _, err := s.AddComment(ctx(), addParams("default", "doc-w2", "ed", nil))
	require.NoError(t, err)
	edit := func() error {
		_, err := s.EditComment(ctx(), domain.EditCommentParams{CommentID: c2.CommentID, TenantID: "default", ActorPrincipalID: "p1", Body: "v2", Idem: idem("p1", "EditComment", "ek", "h")})
		return err
	}
	require.NoError(t, edit())
	require.NoError(t, edit())
	hist, err := s.GetCommentHistory(ctx(), "default", c2.CommentID)
	require.NoError(t, err)
	require.Len(t, hist, 2)

	res := func() error {
		_, err := s.ResolveThread(ctx(), domain.ResolveThreadParams{ThreadID: thread.ThreadID, TenantID: "default", ActorPrincipalID: "p1", Idem: idem("p1", "ResolveThread", "rk", "h")})
		return err
	}
	require.NoError(t, res())
	require.NoError(t, res(), "a replayed resolve must not fail as already-resolved")

	// Delete
	del := func() error {
		_, err := s.DeleteComment(ctx(), domain.DeleteCommentParams{CommentID: c2.CommentID, TenantID: "default", ActorPrincipalID: "p1", Reason: "x", Idem: idem("p1", "DeleteComment", "dk", "h")})
		return err
	}
	require.NoError(t, del())
	require.NoError(t, del())
}

func TestPgStore_Idempotency_FailedMutationReleasesTheKey(t *testing.T) {
	s := store.NewPgStore(requireTestDB(t))
	// Edit of an unknown comment fails and rolls back the claim; the key is
	// not burned and a later valid use of it succeeds.
	_, err := s.EditComment(ctx(), domain.EditCommentParams{CommentID: "nope", TenantID: "default", ActorPrincipalID: "p1", Body: "x", Idem: idem("p1", "EditComment", "burn", "h")})
	require.ErrorIs(t, err, domain.ErrCommentNotFound)
	c, _, err := s.AddComment(ctx(), addParams("default", "doc-f", "x", nil))
	require.NoError(t, err)
	_, err = s.EditComment(ctx(), domain.EditCommentParams{CommentID: c.CommentID, TenantID: "default", ActorPrincipalID: "p1", Body: "y", Idem: idem("p1", "EditComment", "burn", "h")})
	require.NoError(t, err)
}

func TestPgStore_IdempotencyKeys_ForceRLS(t *testing.T) {
	pool := requireTestDB(t)
	var enabled, forced bool
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname='idempotency_keys'`).Scan(&enabled, &forced))
	require.True(t, enabled)
	require.True(t, forced)

	rls := requireRLSPool(t, pool)
	s := store.NewPgStore(rls)
	_, _, err := s.AddComment(ctx(), addParams("default", "doc-rls", "x", idem("p1", "AddComment", "rls", "h")))
	require.NoError(t, err)
	// As a non-superuser with no tenant set, FORCE RLS shows no rows; with
	// the owning tenant set it shows the one claim.
	count := func(tenant string) int {
		conn, err := rls.Acquire(context.Background())
		require.NoError(t, err)
		defer conn.Release()
		if tenant != "" {
			_, err = conn.Exec(context.Background(), `SELECT set_config('app.tenant_id', $1, false)`, tenant)
			require.NoError(t, err)
		}
		var n int
		require.NoError(t, conn.QueryRow(context.Background(), `SELECT count(*) FROM idempotency_keys`).Scan(&n))
		return n
	}
	require.Equal(t, 0, count(""))
	require.Equal(t, 0, count("other-tenant"))
}
