package store_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"zoiko.io/comments-collaboration-svc/internal/domain"
	svcmiddleware "zoiko.io/comments-collaboration-svc/internal/middleware"
	"zoiko.io/comments-collaboration-svc/internal/store"
)

// requireTestDB connects to a real Postgres named by TEST_DATABASE_URL and
// replays every migration from a clean slate — same gating/discovery
// pattern used by every other service in this platform.
func requireTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping real-Postgres integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, err = pool.Exec(context.Background(), `
		DROP TABLE IF EXISTS moderation_trail;
		DROP TABLE IF EXISTS comment_attachments;
		DROP TABLE IF EXISTS reactions;
		DROP TABLE IF EXISTS mentions;
		DROP TABLE IF EXISTS comment_versions;
		DROP TABLE IF EXISTS comments;
		DROP TABLE IF EXISTS comment_threads;
	`)
	require.NoError(t, err)

	_, filename, _, _ := runtime.Caller(0)
	migDir := filepath.Join(filepath.Dir(filename), "..", "..", "deployments", "migrations")
	migrations, err := filepath.Glob(filepath.Join(migDir, "*.up.sql"))
	require.NoError(t, err)
	require.NotEmpty(t, migrations, "no migrations found in %s", migDir)
	sort.Strings(migrations)
	for _, path := range migrations {
		sql, err := os.ReadFile(path)
		require.NoError(t, err, "reading migration %s", filepath.Base(path))
		_, err = pool.Exec(context.Background(), string(sql))
		require.NoError(t, err, "applying migration %s", filepath.Base(path))
	}
	return pool
}

// ctx carries tenant "default" — middleware.TenantFromContext resolves an
// unset context to "", so every test explicitly scopes to a real tenant.
func ctx() context.Context {
	return svcmiddleware.WithTenant(context.Background(), "default")
}

func TestPgStore_AddComment_CreatesThreadAndFirstComment(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	comment, thread, err := s.AddComment(ctx(), domain.AddCommentParams{
		TenantID: "default", LegalEntityID: "le-1",
		LinkedObjectType: "DOCUMENT", LinkedObjectID: "doc-1",
		Body: "Why does clause 4.2 say Net-60?", CreatedByPrincipalID: "reviewer-1",
	})
	require.NoError(t, err)
	require.Equal(t, domain.ThreadStatusActive, thread.Status)
	require.Equal(t, domain.CommentStatusActive, comment.Status)
	require.Equal(t, "Why does clause 4.2 say Net-60?", comment.CurrentBody)

	got, err := s.GetThread(ctx(), thread.ThreadID)
	require.NoError(t, err)
	require.Equal(t, thread.ThreadID, got.ThreadID)
}

func TestPgStore_AddComment_SecondCommentSameObjectReusesThread(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	_, thread1, err := s.AddComment(ctx(), domain.AddCommentParams{
		TenantID: "default", LegalEntityID: "le-1", LinkedObjectType: "DOCUMENT", LinkedObjectID: "doc-2",
		Body: "first", CreatedByPrincipalID: "reviewer-1",
	})
	require.NoError(t, err)

	_, thread2, err := s.AddComment(ctx(), domain.AddCommentParams{
		TenantID: "default", LegalEntityID: "le-1", LinkedObjectType: "DOCUMENT", LinkedObjectID: "doc-2",
		Body: "second", CreatedByPrincipalID: "reviewer-2",
	})
	require.NoError(t, err)
	require.Equal(t, thread1.ThreadID, thread2.ThreadID, "a second comment on the same object must land in the same thread")

	comments, err := s.ListComments(ctx(), "default", thread1.ThreadID)
	require.NoError(t, err)
	require.Len(t, comments, 2)
}

func TestPgStore_AddComment_ReplyMustBelongToSameThread(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	parent, _, err := s.AddComment(ctx(), domain.AddCommentParams{
		TenantID: "default", LegalEntityID: "le-1", LinkedObjectType: "DOCUMENT", LinkedObjectID: "doc-3",
		Body: "parent", CreatedByPrincipalID: "reviewer-1",
	})
	require.NoError(t, err)

	other, _, err := s.AddComment(ctx(), domain.AddCommentParams{
		TenantID: "default", LegalEntityID: "le-1", LinkedObjectType: "DOCUMENT", LinkedObjectID: "doc-4",
		Body: "unrelated", CreatedByPrincipalID: "reviewer-1",
	})
	require.NoError(t, err)

	// A reply nesting under a comment in an unrelated thread is refused —
	// the negative control against replies silently crossing discussions.
	_, _, err = s.AddComment(ctx(), domain.AddCommentParams{
		TenantID: "default", LegalEntityID: "le-1", LinkedObjectType: "DOCUMENT", LinkedObjectID: "doc-4",
		ParentCommentID: parent.CommentID, Body: "cross-thread reply", CreatedByPrincipalID: "reviewer-2",
	})
	require.ErrorIs(t, err, domain.ErrParentCommentNotInThread)
	_ = other
}

func TestPgStore_AddComment_ReopensResolvedThread(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	_, thread, err := s.AddComment(ctx(), domain.AddCommentParams{
		TenantID: "default", LegalEntityID: "le-1", LinkedObjectType: "TASK", LinkedObjectID: "task-1",
		Body: "first", CreatedByPrincipalID: "reviewer-1",
	})
	require.NoError(t, err)

	// Resolve it directly via SQL (ResolveThread itself is Wave 2) to set
	// up the reopen scenario. A raw pool.Exec has no session-level tenant
	// context, so FORCE RLS would make the row invisible (silently
	// matching zero rows) without first setting app.tenant_id on the
	// acquired connection.
	conn, err := pool.Acquire(context.Background())
	require.NoError(t, err)
	_, err = conn.Exec(context.Background(), `SELECT set_config('app.tenant_id', 'default', false)`)
	require.NoError(t, err)
	tag, err := conn.Exec(context.Background(), `UPDATE comment_threads SET status='RESOLVED', resolved_by='reviewer-1', resolved_at=now() WHERE thread_id=$1`, thread.ThreadID)
	require.NoError(t, err)
	require.Equal(t, int64(1), tag.RowsAffected())
	conn.Release()

	_, reopened, err := s.AddComment(ctx(), domain.AddCommentParams{
		TenantID: "default", LegalEntityID: "le-1", LinkedObjectType: "TASK", LinkedObjectID: "task-1",
		Body: "new activity", CreatedByPrincipalID: "reviewer-2",
	})
	require.NoError(t, err)
	require.Equal(t, thread.ThreadID, reopened.ThreadID)
	require.Equal(t, domain.ThreadStatusActive, reopened.Status, "new discussion must reopen a resolved thread")
}

func TestPgStore_AddComment_EmptyBody_Rejected(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	_, _, err := s.AddComment(ctx(), domain.AddCommentParams{
		TenantID: "default", LegalEntityID: "le-1", LinkedObjectType: "DOCUMENT", LinkedObjectID: "doc-5",
		Body: "", CreatedByPrincipalID: "reviewer-1",
	})
	require.ErrorIs(t, err, domain.ErrEmptyBody)
}

func TestPgStore_EditComment_CreatesNewVersionPreservesHistory(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	comment, _, err := s.AddComment(ctx(), domain.AddCommentParams{
		TenantID: "default", LegalEntityID: "le-1", LinkedObjectType: "DOCUMENT", LinkedObjectID: "doc-6",
		Body: "Net-60 seems wrong", CreatedByPrincipalID: "reviewer-1",
	})
	require.NoError(t, err)

	edited, err := s.EditComment(ctx(), domain.EditCommentParams{
		CommentID: comment.CommentID, TenantID: "default", ActorPrincipalID: "reviewer-1", Body: "Net-30 seems wrong (typo fix)",
	})
	require.NoError(t, err)
	require.Equal(t, domain.CommentStatusEdited, edited.Status)
	require.Equal(t, "Net-30 seems wrong (typo fix)", edited.CurrentBody)

	history, err := s.GetCommentHistory(ctx(), "default", comment.CommentID)
	require.NoError(t, err)
	require.Len(t, history, 2, "both the original and edited body must remain queryable")
	require.Equal(t, "Net-60 seems wrong", history[0].Body)
	require.Equal(t, "Net-30 seems wrong (typo fix)", history[1].Body)
}

func TestPgStore_EditComment_UnknownComment_ReturnsNotFound(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	_, err := s.EditComment(ctx(), domain.EditCommentParams{CommentID: "comment-does-not-exist", TenantID: "default", ActorPrincipalID: "reviewer-1", Body: "x"})
	require.ErrorIs(t, err, domain.ErrCommentNotFound)
}

// TestPgStore_CommentVersions_AreAppendOnly is the negative control on
// migration 000001's trigger — a raw UPDATE or DELETE against
// comment_versions is refused outright, proving "original history
// preserved" is real, not just documented intent.
func TestPgStore_CommentVersions_AreAppendOnly(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	comment, _, err := s.AddComment(ctx(), domain.AddCommentParams{
		TenantID: "default", LegalEntityID: "le-1", LinkedObjectType: "DOCUMENT", LinkedObjectID: "doc-7",
		Body: "original", CreatedByPrincipalID: "reviewer-1",
	})
	require.NoError(t, err)

	// A raw pool.Exec has no session-level tenant context, so FORCE RLS
	// would make the row invisible (silently matching zero rows, no
	// error) rather than exercising the trigger this test actually wants
	// to prove. Set app.tenant_id on an acquired connection first so the
	// row is visible and the append-only trigger is what fires.
	conn, err := pool.Acquire(context.Background())
	require.NoError(t, err)
	defer conn.Release()
	_, err = conn.Exec(context.Background(), `SELECT set_config('app.tenant_id', 'default', false)`)
	require.NoError(t, err)

	_, err = conn.Exec(context.Background(), `UPDATE comment_versions SET body='tampered' WHERE comment_id=$1`, comment.CommentID)
	require.Error(t, err, "expected the append-only trigger to reject a direct mutation")

	_, err = conn.Exec(context.Background(), `DELETE FROM comment_versions WHERE comment_id=$1`, comment.CommentID)
	require.Error(t, err, "expected the append-only trigger to reject a direct delete")
}

func TestPgStore_ListComments_OrdersByCreatedAt(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	_, thread, err := s.AddComment(ctx(), domain.AddCommentParams{
		TenantID: "default", LegalEntityID: "le-1", LinkedObjectType: "DOCUMENT", LinkedObjectID: "doc-8",
		Body: "first", CreatedByPrincipalID: "reviewer-1",
	})
	require.NoError(t, err)
	_, _, err = s.AddComment(ctx(), domain.AddCommentParams{
		TenantID: "default", LegalEntityID: "le-1", LinkedObjectType: "DOCUMENT", LinkedObjectID: "doc-8",
		Body: "second", CreatedByPrincipalID: "reviewer-2",
	})
	require.NoError(t, err)

	list, err := s.ListComments(ctx(), "default", thread.ThreadID)
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, "first", list[0].CurrentBody)
	require.Equal(t, "second", list[1].CurrentBody)
}

func TestPgStore_GetThread_UnknownThread_ReturnsNotFound(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	_, err := s.GetThread(ctx(), "thread-does-not-exist")
	require.ErrorIs(t, err, domain.ErrThreadNotFound)
}

func TestPgStore_DeleteComment_ThenRejectsDoubleDelete(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	comment, _, err := s.AddComment(ctx(), domain.AddCommentParams{
		TenantID: "default", LegalEntityID: "le-1", LinkedObjectType: "DOCUMENT", LinkedObjectID: "doc-9",
		Body: "spam", CreatedByPrincipalID: "reviewer-1",
	})
	require.NoError(t, err)

	deleted, err := s.DeleteComment(ctx(), domain.DeleteCommentParams{CommentID: comment.CommentID, TenantID: "default", ActorPrincipalID: "moderator-1", Reason: "spam"})
	require.NoError(t, err)
	require.Equal(t, domain.CommentStatusDeletedRedacted, deleted.Status)
	require.Equal(t, "spam", deleted.DeletionReason)

	_, err = s.DeleteComment(ctx(), domain.DeleteCommentParams{CommentID: comment.CommentID, TenantID: "default", ActorPrincipalID: "moderator-1", Reason: "again"})
	require.ErrorIs(t, err, domain.ErrCommentInvalidState)
}

func TestPgStore_Mention_ThenGetMentions(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	comment, _, err := s.AddComment(ctx(), domain.AddCommentParams{
		TenantID: "default", LegalEntityID: "le-1", LinkedObjectType: "DOCUMENT", LinkedObjectID: "doc-10",
		Body: "hello", CreatedByPrincipalID: "reviewer-1",
	})
	require.NoError(t, err)

	m, err := s.Mention(ctx(), domain.MentionParams{CommentID: comment.CommentID, TenantID: "default", MentionedPrincipalID: "sarah", VisibilityGranted: true})
	require.NoError(t, err)
	require.True(t, m.VisibilityGranted)

	mentions, err := s.GetMentions(ctx(), "default", comment.CommentID)
	require.NoError(t, err)
	require.Len(t, mentions, 1)
	require.Equal(t, "sarah", mentions[0].MentionedPrincipalID)
}

// TestPgStore_Mentions_AreAppendOnly is the negative control on migration
// 000001's trigger for the mentions table.
func TestPgStore_Mentions_AreAppendOnly(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	comment, _, err := s.AddComment(ctx(), domain.AddCommentParams{
		TenantID: "default", LegalEntityID: "le-1", LinkedObjectType: "DOCUMENT", LinkedObjectID: "doc-11",
		Body: "hello", CreatedByPrincipalID: "reviewer-1",
	})
	require.NoError(t, err)
	m, err := s.Mention(ctx(), domain.MentionParams{CommentID: comment.CommentID, TenantID: "default", MentionedPrincipalID: "sarah", VisibilityGranted: true})
	require.NoError(t, err)

	conn, err := pool.Acquire(context.Background())
	require.NoError(t, err)
	defer conn.Release()
	_, err = conn.Exec(context.Background(), `SELECT set_config('app.tenant_id', 'default', false)`)
	require.NoError(t, err)

	_, err = conn.Exec(context.Background(), `UPDATE mentions SET visibility_granted=false WHERE mention_id=$1`, m.MentionID)
	require.Error(t, err, "expected the append-only trigger to reject a direct mutation")
}

func TestPgStore_React_TogglesOnAndOff(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	comment, _, err := s.AddComment(ctx(), domain.AddCommentParams{
		TenantID: "default", LegalEntityID: "le-1", LinkedObjectType: "DOCUMENT", LinkedObjectID: "doc-12",
		Body: "hello", CreatedByPrincipalID: "reviewer-1",
	})
	require.NoError(t, err)

	added, err := s.React(ctx(), domain.ReactParams{CommentID: comment.CommentID, TenantID: "default", PrincipalID: "reviewer-2", ReactionType: "THUMBS_UP"})
	require.NoError(t, err)
	require.True(t, added)

	removed, err := s.React(ctx(), domain.ReactParams{CommentID: comment.CommentID, TenantID: "default", PrincipalID: "reviewer-2", ReactionType: "THUMBS_UP"})
	require.NoError(t, err)
	require.False(t, removed)
}

func TestPgStore_Moderate_RequiresReasonAndRecordsTrail(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	comment, _, err := s.AddComment(ctx(), domain.AddCommentParams{
		TenantID: "default", LegalEntityID: "le-1", LinkedObjectType: "DOCUMENT", LinkedObjectID: "doc-13",
		Body: "inappropriate", CreatedByPrincipalID: "reviewer-1",
	})
	require.NoError(t, err)

	_, err = s.Moderate(ctx(), domain.ModerateParams{CommentID: comment.CommentID, TenantID: "default", ModeratorPrincipalID: "moderator-1", Action: "REDACT", Reason: ""})
	require.ErrorIs(t, err, domain.ErrReasonRequired)

	moderated, err := s.Moderate(ctx(), domain.ModerateParams{CommentID: comment.CommentID, TenantID: "default", ModeratorPrincipalID: "moderator-1", Action: "REDACT", Reason: "policy violation"})
	require.NoError(t, err)
	require.Equal(t, domain.CommentStatusModerated, moderated.Status)

	trail, err := s.GetModerationTrail(ctx(), "default", comment.CommentID)
	require.NoError(t, err)
	require.Len(t, trail, 1)
	require.Equal(t, "policy violation", trail[0].Reason)
}

// TestPgStore_ModerationTrail_AreAppendOnly is the negative control
// proving moderation evidence cannot be silently rewritten (doc's own
// T46).
func TestPgStore_ModerationTrail_AreAppendOnly(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	comment, _, err := s.AddComment(ctx(), domain.AddCommentParams{
		TenantID: "default", LegalEntityID: "le-1", LinkedObjectType: "DOCUMENT", LinkedObjectID: "doc-14",
		Body: "inappropriate", CreatedByPrincipalID: "reviewer-1",
	})
	require.NoError(t, err)
	_, err = s.Moderate(ctx(), domain.ModerateParams{CommentID: comment.CommentID, TenantID: "default", ModeratorPrincipalID: "moderator-1", Action: "REDACT", Reason: "policy violation"})
	require.NoError(t, err)

	trail, err := s.GetModerationTrail(ctx(), "default", comment.CommentID)
	require.NoError(t, err)
	require.Len(t, trail, 1)

	conn, err := pool.Acquire(context.Background())
	require.NoError(t, err)
	defer conn.Release()
	_, err = conn.Exec(context.Background(), `SELECT set_config('app.tenant_id', 'default', false)`)
	require.NoError(t, err)

	_, err = conn.Exec(context.Background(), `UPDATE moderation_trail SET reason='tampered' WHERE moderation_id=$1`, trail[0].ModerationID)
	require.Error(t, err, "expected the append-only trigger to reject a direct mutation")
}

func TestPgStore_ResolveThread_ThenRejectsDoubleResolve(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	_, thread, err := s.AddComment(ctx(), domain.AddCommentParams{
		TenantID: "default", LegalEntityID: "le-1", LinkedObjectType: "DOCUMENT", LinkedObjectID: "doc-15",
		Body: "hello", CreatedByPrincipalID: "reviewer-1",
	})
	require.NoError(t, err)

	resolved, err := s.ResolveThread(ctx(), domain.ResolveThreadParams{ThreadID: thread.ThreadID, TenantID: "default", ActorPrincipalID: "reviewer-1", ResolutionNote: "answered"})
	require.NoError(t, err)
	require.Equal(t, domain.ThreadStatusResolved, resolved.Status)

	_, err = s.ResolveThread(ctx(), domain.ResolveThreadParams{ThreadID: thread.ThreadID, TenantID: "default", ActorPrincipalID: "reviewer-1", ResolutionNote: "again"})
	require.ErrorIs(t, err, domain.ErrThreadInvalidState)
}

func TestPgStore_AttachReference_ThenRejectsOnDeletedComment(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	comment, _, err := s.AddComment(ctx(), domain.AddCommentParams{
		TenantID: "default", LegalEntityID: "le-1", LinkedObjectType: "DOCUMENT", LinkedObjectID: "doc-16",
		Body: "see attached quote", CreatedByPrincipalID: "reviewer-1",
	})
	require.NoError(t, err)

	attachment, err := s.AttachReference(ctx(), domain.AttachReferenceParams{CommentID: comment.CommentID, TenantID: "default", LinkedObjectID: "quote-1", AttachedByPrincipalID: "reviewer-1"})
	require.NoError(t, err)
	require.Equal(t, "DOCUMENT", attachment.LinkedObjectType)

	_, err = s.DeleteComment(ctx(), domain.DeleteCommentParams{CommentID: comment.CommentID, TenantID: "default", ActorPrincipalID: "moderator-1", Reason: "spam"})
	require.NoError(t, err)

	_, err = s.AttachReference(ctx(), domain.AttachReferenceParams{CommentID: comment.CommentID, TenantID: "default", LinkedObjectID: "quote-2", AttachedByPrincipalID: "reviewer-1"})
	require.ErrorIs(t, err, domain.ErrCommentInvalidState)
}

// TestPgStore_CommentAttachments_AreAppendOnly is the negative control
// on migration 000001's trigger for comment_attachments.
func TestPgStore_CommentAttachments_AreAppendOnly(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	comment, _, err := s.AddComment(ctx(), domain.AddCommentParams{
		TenantID: "default", LegalEntityID: "le-1", LinkedObjectType: "DOCUMENT", LinkedObjectID: "doc-17",
		Body: "see attached quote", CreatedByPrincipalID: "reviewer-1",
	})
	require.NoError(t, err)
	attachment, err := s.AttachReference(ctx(), domain.AttachReferenceParams{CommentID: comment.CommentID, TenantID: "default", LinkedObjectID: "quote-1", AttachedByPrincipalID: "reviewer-1"})
	require.NoError(t, err)

	conn, err := pool.Acquire(context.Background())
	require.NoError(t, err)
	defer conn.Release()
	_, err = conn.Exec(context.Background(), `SELECT set_config('app.tenant_id', 'default', false)`)
	require.NoError(t, err)

	_, err = conn.Exec(context.Background(), `DELETE FROM comment_attachments WHERE attachment_id=$1`, attachment.AttachmentID)
	require.Error(t, err, "expected the append-only trigger to reject a direct delete")
}
