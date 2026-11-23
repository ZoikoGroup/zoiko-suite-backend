package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/comments-collaboration-svc/internal/domain"
	"zoiko.io/comments-collaboration-svc/internal/store"
)

const ccPrefix = "com.zoikosuite.business."

// Each BIZ-09 event is written in the same transaction as the change, to the shared eventing
// outbox; payload schemas are checked for every row by verifyEventContracts. The payload
// carries identifiers only, never the comment body.
func TestCommentEvents_WrittenWithEachChange(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool, store.WithEventRegion("local"))

	c, thread, err := s.AddComment(ctx(), domain.AddCommentParams{
		TenantID: "default", LegalEntityID: "le-ev", LinkedObjectType: "DOCUMENT", LinkedObjectID: "doc-ev", Classification: "RESTRICTED",
		Body: "the secret body text", CreatedByPrincipalID: "author-1", CorrelationID: "corr-add",
	})
	require.NoError(t, err)
	_, err = s.EditComment(ctx(), domain.EditCommentParams{CommentID: c.CommentID, TenantID: "default", ActorPrincipalID: "author-1", Body: "revised secret", CorrelationID: "corr-edit"})
	require.NoError(t, err)
	_, err = s.Mention(ctx(), domain.MentionParams{CommentID: c.CommentID, TenantID: "default", MentionedPrincipalID: "sarah", VisibilityGranted: true, ActorPrincipalID: "author-1", CorrelationID: "corr-mention"})
	require.NoError(t, err)
	_, err = s.Moderate(ctx(), domain.ModerateParams{CommentID: c.CommentID, TenantID: "default", ModeratorPrincipalID: "mod-1", Action: "HIDE", Reason: "abusive", CorrelationID: "corr-mod"})
	require.NoError(t, err)
	_, err = s.ResolveThread(ctx(), domain.ResolveThreadParams{ThreadID: thread.ThreadID, TenantID: "default", ActorPrincipalID: "mod-1", ResolutionNote: "done", CorrelationID: "corr-res"})
	require.NoError(t, err)

	types := map[string]int{}
	rows, err := pool.Query(context.Background(), `SELECT event_type, count(*) FROM eventing_outbox GROUP BY 1`)
	require.NoError(t, err)
	for rows.Next() {
		var ty string
		var n int
		require.NoError(t, rows.Scan(&ty, &n))
		types[ty] = n
	}
	rows.Close()
	// Only the five spec events are written (the earlier commands write none beyond them).
	assert.Equal(t, map[string]int{
		ccPrefix + "comment.added": 1, ccPrefix + "comment.edited": 1, ccPrefix + "comment.moderated": 1,
		ccPrefix + "mention.created": 1, ccPrefix + "thread.resolved": 1,
	}, types)

	// Correlation and actor are carried; the body never is; the thread's RESTRICTED class sets the envelope's.
	var all string
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT string_agg(convert_from(payload, 'UTF8'), ' ') FROM eventing_outbox`).Scan(&all))
	for _, want := range []string{"corr-add", "corr-edit", "corr-mention", "corr-mod", "corr-res", "mod-1", "sarah", "restricted"} {
		assert.Contains(t, all, want)
	}
	assert.NotContains(t, all, "secret", "an event must never carry the comment body")
}

// A replayed request (same Idempotency-Key) returns the original result and announces nothing
// a second time.
func TestCommentEvents_IdempotentReplayWritesNoSecondEvent(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool, store.WithEventRegion("local"))
	p := addParams("default", "doc-replay", "hello", idem("author-1", "AddComment", "k-1", "h-1"))
	first, _, err := s.AddComment(ctx(), p)
	require.NoError(t, err)
	p2 := addParams("default", "doc-replay", "hello", idem("author-1", "AddComment", "k-1", "h-1"))
	again, _, err := s.AddComment(ctx(), p2)
	require.NoError(t, err)
	require.Equal(t, first.CommentID, again.CommentID)
	require.True(t, p2.Idem.Replayed)

	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM eventing_outbox`).Scan(&n))
	require.Equal(t, 1, n, "the replay must not write a second comment.added")
}

// A store with no event region cannot build the envelope, so the write is refused and rolled
// back: no thread, no comment, no event.
func TestCommentEvents_MissingRegionRollsTheWriteBack(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool) // no WithEventRegion

	_, _, err := s.AddComment(ctx(), addParams("default", "doc-noregion", "x", nil))
	require.Error(t, err)
	var threads, comments, events int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM comment_threads`).Scan(&threads))
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM comments`).Scan(&comments))
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM eventing_outbox`).Scan(&events))
	assert.Zero(t, threads)
	assert.Zero(t, comments)
	assert.Zero(t, events)

	// An edit that cannot write its event leaves the comment unedited.
	good := store.NewPgStore(pool, store.WithEventRegion("local"))
	c, _, err := good.AddComment(ctx(), addParams("default", "doc-noregion2", "original", nil))
	require.NoError(t, err)
	_, err = s.EditComment(ctx(), domain.EditCommentParams{CommentID: c.CommentID, TenantID: "default", ActorPrincipalID: "reviewer-1", Body: "changed"})
	require.Error(t, err)
	hist, err := good.GetCommentHistory(ctx(), "default", c.CommentID)
	require.NoError(t, err)
	require.Len(t, hist, 1, "a failed event must roll the new version back")
}
