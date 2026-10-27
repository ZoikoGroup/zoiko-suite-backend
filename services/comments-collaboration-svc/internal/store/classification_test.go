package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"zoiko.io/comments-collaboration-svc/internal/domain"
	"zoiko.io/comments-collaboration-svc/internal/store"
)

func clsParams(tenant, object, classification string) domain.AddCommentParams {
	return domain.AddCommentParams{
		TenantID: tenant, LegalEntityID: "le-1", LinkedObjectType: "DOCUMENT", LinkedObjectID: object,
		Classification: classification, Body: "text", CreatedByPrincipalID: "p1",
	}
}

func TestPgStore_Classification_AcceptedPersistedAndReturned(t *testing.T) {
	s := store.NewPgStore(requireTestDB(t))
	for i, tier := range []string{"PUBLIC", "INTERNAL", "CONFIDENTIAL", "RESTRICTED"} {
		obj := "doc-tier-" + tier
		comment, thread, err := s.AddComment(ctx(), clsParams("default", obj, tier))
		require.NoError(t, err, tier)
		require.Equal(t, tier, thread.Classification)
		require.Equal(t, tier, comment.Classification, "the comment inherits the thread's classification")

		gotThread, err := s.GetThread(ctx(), thread.ThreadID)
		require.NoError(t, err)
		require.Equal(t, tier, gotThread.Classification, "persisted on the thread (case %d)", i)
		gotComment, _, err := s.GetComment(ctx(), "default", comment.CommentID)
		require.NoError(t, err)
		require.Equal(t, tier, gotComment.Classification, "persisted on the comment")
		list, err := s.ListComments(ctx(), "default", thread.ThreadID)
		require.NoError(t, err)
		require.Len(t, list, 1)
		require.Equal(t, tier, list[0].Classification, "returned by ListComments")
	}
}

func TestPgStore_Classification_RequiredToCreateAThread(t *testing.T) {
	s := store.NewPgStore(requireTestDB(t))
	_, _, err := s.AddComment(ctx(), clsParams("default", "doc-nocls", ""))
	require.ErrorIs(t, err, domain.ErrClassificationRequired)

	// Nothing was written — the refused call left no thread behind, so a
	// correct retry creates it fresh.
	_, thread, err := s.AddComment(ctx(), clsParams("default", "doc-nocls", "INTERNAL"))
	require.NoError(t, err)
	list, err := s.ListComments(ctx(), "default", thread.ThreadID)
	require.NoError(t, err)
	require.Len(t, list, 1)
}

func TestPgStore_Classification_InvalidValueRefused(t *testing.T) {
	s := store.NewPgStore(requireTestDB(t))
	for _, bad := range []string{"SECRET", "internal", "TOP-SECRET", " INTERNAL"} {
		_, _, err := s.AddComment(ctx(), clsParams("default", "doc-bad", bad))
		require.ErrorIs(t, err, domain.ErrClassificationInvalid, bad)
	}
}

func TestPgStore_Classification_InheritedByLaterComments(t *testing.T) {
	s := store.NewPgStore(requireTestDB(t))
	_, thread, err := s.AddComment(ctx(), clsParams("default", "doc-inh", "CONFIDENTIAL"))
	require.NoError(t, err)

	// Omitted: inherits the thread's.
	c2, t2, err := s.AddComment(ctx(), clsParams("default", "doc-inh", ""))
	require.NoError(t, err)
	require.Equal(t, thread.ThreadID, t2.ThreadID)
	require.Equal(t, "CONFIDENTIAL", c2.Classification)

	// Restated identically: accepted.
	c3, _, err := s.AddComment(ctx(), clsParams("default", "doc-inh", "CONFIDENTIAL"))
	require.NoError(t, err)
	require.Equal(t, "CONFIDENTIAL", c3.Classification)

	// A different value is refused — a comment cannot be classified below (or
	// above) the thread it joins — and writes nothing.
	_, _, err = s.AddComment(ctx(), clsParams("default", "doc-inh", "PUBLIC"))
	require.ErrorIs(t, err, domain.ErrClassificationMismatch)
	list, err := s.ListComments(ctx(), "default", thread.ThreadID)
	require.NoError(t, err)
	require.Len(t, list, 3)
	for _, c := range list {
		require.Equal(t, "CONFIDENTIAL", c.Classification)
	}
}

func TestPgStore_Classification_SurvivesEditModerateAndDelete(t *testing.T) {
	s := store.NewPgStore(requireTestDB(t))
	c, _, err := s.AddComment(ctx(), clsParams("default", "doc-life", "RESTRICTED"))
	require.NoError(t, err)

	edited, err := s.EditComment(ctx(), domain.EditCommentParams{CommentID: c.CommentID, TenantID: "default", ActorPrincipalID: "p1", Body: "v2"})
	require.NoError(t, err)
	require.Equal(t, "RESTRICTED", edited.Classification)
	hist, err := s.GetCommentHistory(ctx(), "default", c.CommentID)
	require.NoError(t, err)
	require.Len(t, hist, 2, "edit history is untouched by classification")

	moderated, err := s.Moderate(ctx(), domain.ModerateParams{CommentID: c.CommentID, TenantID: "default", ModeratorPrincipalID: "m", Action: "HIDE", Reason: "r"})
	require.NoError(t, err)
	require.Equal(t, "RESTRICTED", moderated.Classification)

	deleted, err := s.DeleteComment(ctx(), domain.DeleteCommentParams{CommentID: c.CommentID, TenantID: "default", ActorPrincipalID: "p1", Reason: "r"})
	require.NoError(t, err)
	require.Equal(t, "RESTRICTED", deleted.Classification)
}

func TestPgStore_Classification_IdempotentReplayReturnsIt(t *testing.T) {
	s := store.NewPgStore(requireTestDB(t))
	p := clsParams("default", "doc-idem-cls", "INTERNAL")
	p.Idem = idem("p1", "AddComment", "cls-key", "h")
	first, _, err := s.AddComment(ctx(), p)
	require.NoError(t, err)

	p2 := clsParams("default", "doc-idem-cls", "INTERNAL")
	p2.Idem = idem("p1", "AddComment", "cls-key", "h")
	replay, thread, err := s.AddComment(ctx(), p2)
	require.NoError(t, err)
	require.True(t, p2.Idem.Replayed)
	require.Equal(t, first.CommentID, replay.CommentID)
	require.Equal(t, "INTERNAL", replay.Classification)
	require.Equal(t, "INTERNAL", thread.Classification)
}

func TestPgStore_Classification_IsTenantScoped(t *testing.T) {
	admin := requireTestDB(t)
	s := store.NewPgStore(requireRLSPool(t, admin))

	// The same linked object in two tenants has two independent threads, each
	// with its own classification.
	_, ta, err := s.AddComment(ctx(), clsParams("default", "shared-doc", "PUBLIC"))
	require.NoError(t, err)
	_, tb, err := s.AddComment(ctxFor("tenant-b"), clsParams("tenant-b", "shared-doc", "RESTRICTED"))
	require.NoError(t, err)
	require.NotEqual(t, ta.ThreadID, tb.ThreadID)
	require.Equal(t, "PUBLIC", ta.Classification)
	require.Equal(t, "RESTRICTED", tb.Classification)

	// Tenant B cannot read tenant A's thread or learn its classification.
	_, err = s.GetThread(ctxFor("tenant-b"), ta.ThreadID)
	require.ErrorIs(t, err, domain.ErrThreadNotFound)
	// And a tenant-B comment restating A's classification is judged against
	// B's own thread, not A's.
	_, _, err = s.AddComment(ctxFor("tenant-b"), clsParams("tenant-b", "shared-doc", "PUBLIC"))
	require.ErrorIs(t, err, domain.ErrClassificationMismatch)
}

func TestPgStore_Classification_LegacyThreadsAreNotFabricated(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	// A thread written before classification existed: no value recorded.
	conn, err := pool.Acquire(context.Background())
	require.NoError(t, err)
	defer conn.Release()
	_, err = conn.Exec(context.Background(), `SELECT set_config('app.tenant_id', 'default', false)`)
	require.NoError(t, err)
	_, err = conn.Exec(context.Background(), `
		INSERT INTO comment_threads (thread_id, tenant_id, legal_entity_id, linked_object_type, linked_object_id, created_by)
		VALUES ('thread-legacy', 'default', 'le-1', 'DOCUMENT', 'doc-legacy', 'p0')`)
	require.NoError(t, err)

	got, err := s.GetThread(ctx(), "thread-legacy")
	require.NoError(t, err)
	require.Empty(t, got.Classification, "no classification is invented for a legacy thread")

	// A comment that omits one inherits "not recorded".
	c, _, err := s.AddComment(ctx(), clsParams("default", "doc-legacy", ""))
	require.NoError(t, err)
	require.Empty(t, c.Classification)

	// Asserting one on a legacy thread is not silently accepted.
	_, _, err = s.AddComment(ctx(), clsParams("default", "doc-legacy", "INTERNAL"))
	require.ErrorIs(t, err, domain.ErrClassificationMismatch)
}

func TestPgStore_Classification_DatabaseRejectsAnUnknownTier(t *testing.T) {
	pool := requireTestDB(t)
	conn, err := pool.Acquire(context.Background())
	require.NoError(t, err)
	defer conn.Release()
	_, err = conn.Exec(context.Background(), `SELECT set_config('app.tenant_id', 'default', false)`)
	require.NoError(t, err)
	_, err = conn.Exec(context.Background(), `
		INSERT INTO comment_threads (thread_id, tenant_id, legal_entity_id, linked_object_type, linked_object_id, created_by, classification)
		VALUES ('thread-bad', 'default', 'le-1', 'DOCUMENT', 'doc-bad', 'p0', 'TOP-SECRET')`)
	require.Error(t, err, "the CHECK constraint is the last line of defence behind the service validation")
}
