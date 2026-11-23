package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"zoiko.io/comments-collaboration-svc/internal/domain"
	"zoiko.io/comments-collaboration-svc/internal/store"
)

// A deleted comment is redacted from the ordinary read: ListComments no longer returns its
// body. The original text stays in the append-only history (the evidence), and the comment
// row records who deleted it and why.
func TestPgStore_ListComments_RedactsADeletedCommentsBody(t *testing.T) {
	s := store.NewPgStore(requireTestDB(t), store.WithEventRegion("local"))
	c, thread, err := s.AddComment(ctx(), addParams("default", "doc-redact", "secret text", nil))
	require.NoError(t, err)
	keep, _, err := s.AddComment(ctx(), addParams("default", "doc-redact", "visible text", nil))
	require.NoError(t, err)

	_, err = s.DeleteComment(ctx(), domain.DeleteCommentParams{CommentID: c.CommentID, TenantID: "default", ActorPrincipalID: "p1", Reason: "mistake"})
	require.NoError(t, err)

	list, err := s.ListComments(ctx(), "default", thread.ThreadID)
	require.NoError(t, err)
	require.Len(t, list, 2)
	for _, got := range list {
		switch got.CommentID {
		case c.CommentID:
			require.Equal(t, domain.CommentStatusDeletedRedacted, got.Status)
			require.Empty(t, got.CurrentBody, "a deleted comment's body must not be returned by the ordinary read")
			require.Equal(t, "mistake", got.DeletionReason)
		case keep.CommentID:
			require.Equal(t, "visible text", got.CurrentBody)
		}
	}

	hist, err := s.GetCommentHistory(ctx(), "default", c.CommentID)
	require.NoError(t, err)
	require.Len(t, hist, 1)
	require.Equal(t, "secret text", hist[0].Body, "the original is preserved in the history")
}

// BIZ-09 tenant isolation for the write paths, as a NOSUPERUSER NOBYPASSRLS role: tenant B
// cannot edit, delete, mention, react to, moderate, resolve or attach to tenant A's thread
// and comment, A's data is unchanged, and a raw count under B's context sees no rows.
func TestPgStore_Writes_AreTenantIsolated(t *testing.T) {
	admin := requireTestDB(t)
	s := store.NewPgStore(requireRLSPool(t, admin), store.WithEventRegion("local"))
	c, thread, err := s.AddComment(ctx(), addParams("default", "doc-iso", "mine", nil))
	require.NoError(t, err)
	other := ctxFor("other-tenant")

	_, err = s.EditComment(other, domain.EditCommentParams{CommentID: c.CommentID, TenantID: "other-tenant", ActorPrincipalID: "x", Body: "hijack"})
	require.ErrorIs(t, err, domain.ErrCommentNotFound)
	_, err = s.DeleteComment(other, domain.DeleteCommentParams{CommentID: c.CommentID, TenantID: "other-tenant", ActorPrincipalID: "x", Reason: "x"})
	require.ErrorIs(t, err, domain.ErrCommentNotFound)
	_, err = s.Mention(other, domain.MentionParams{CommentID: c.CommentID, TenantID: "other-tenant", MentionedPrincipalID: "x", VisibilityGranted: true})
	require.Error(t, err)
	_, err = s.React(other, domain.ReactParams{CommentID: c.CommentID, TenantID: "other-tenant", PrincipalID: "x", ReactionType: "THUMBS_UP"})
	require.Error(t, err)
	_, err = s.Moderate(other, domain.ModerateParams{CommentID: c.CommentID, TenantID: "other-tenant", ModeratorPrincipalID: "x", Action: "HIDE", Reason: "x"})
	require.ErrorIs(t, err, domain.ErrCommentNotFound)
	_, err = s.ResolveThread(other, domain.ResolveThreadParams{ThreadID: thread.ThreadID, TenantID: "other-tenant", ActorPrincipalID: "x"})
	require.ErrorIs(t, err, domain.ErrThreadNotFound)
	_, err = s.AttachReference(other, domain.AttachReferenceParams{CommentID: c.CommentID, TenantID: "other-tenant", LinkedObjectID: "doc", AttachedByPrincipalID: "x"})
	require.Error(t, err)

	// A's data is unchanged.
	got, _, err := s.GetComment(ctx(), "default", c.CommentID)
	require.NoError(t, err)
	require.Equal(t, domain.CommentStatusActive, got.Status)
	gotThread, err := s.GetThread(ctx(), thread.ThreadID)
	require.NoError(t, err)
	require.Equal(t, domain.ThreadStatusActive, gotThread.Status)
	hist, err := s.GetCommentHistory(ctx(), "default", c.CommentID)
	require.NoError(t, err)
	require.Len(t, hist, 1)

	// Raw probe: no tenant predicate, B's context.
	pool := requireRLSPool(t, admin)
	tx, err := pool.Begin(context.Background())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.Background()) }()
	_, err = tx.Exec(context.Background(), `SELECT set_config('app.tenant_id', 'other-tenant', true)`)
	require.NoError(t, err)
	for _, table := range []string{"comment_threads", "comments", "comment_versions", "mentions", "reactions", "comment_attachments", "moderation_trail"} {
		var n int
		require.NoError(t, tx.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&n))
		require.Zero(t, n, "RLS leak: tenant B context sees rows in %s", table)
	}
}
