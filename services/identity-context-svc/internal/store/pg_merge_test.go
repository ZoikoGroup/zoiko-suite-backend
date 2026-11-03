// MergeDuplicateIdentity against a real Postgres (TEST_DATABASE_URL, like pg_store_test.go).
package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/federation"
	"zoiko.io/identity-context-svc/internal/outbox"
	"zoiko.io/identity-context-svc/internal/store"
)

func mergeRig(t *testing.T) *fedRig {
	t.Helper()
	r := newFedRig(t)
	insertPrincipal(t, context.Background(), r.admin, "p-3", "t-1", "fed|p3", "ACTIVE")
	return r
}

func (r *fedRig) link(t *testing.T, conn, subject, principal string) *federation.Link {
	t.Helper()
	l, err := r.s.CreateLink(context.Background(), federation.Link{TenantID: "t-1", ConnectionID: conn, Subject: subject,
		PrincipalID: principal, Source: federation.SourceAdmin, LinkedBy: "admin"}, "admin", "corr")
	require.NoError(t, err)
	return l
}

func eventFn(m store.IdentityMerge) ([]outbox.Record, error) {
	return []outbox.Record{{EventID: "evt-merge-" + m.MergeID, EventType: "identity.subject.merged", TenantID: m.TenantID,
		PartitionKey: m.SurvivorID, Payload: []byte(`{"merge_id":"` + m.MergeID + `"}`)}}, nil
}

func (r *fedRig) merge(survivor, duplicate string) (*store.IdentityMerge, bool, error) {
	return r.s.MergeIdentities(context.Background(), "t-1", survivor, duplicate, "same person", "admin-9", "corr-m", eventFn)
}

func TestMerge_MovesLinksDisablesDuplicateAndKeepsItsRow(t *testing.T) {
	ctx := context.Background()
	r := mergeRig(t)
	r.active(t, "t-1", "c-a", "https://a.example.com")
	r.active(t, "t-1", "c-b", "https://b.example.com")
	moved := r.link(t, "c-b", "dup-b", "p-1")   // survivor holds no link at b: it moves
	retired := r.link(t, "c-a", "dup-a", "p-1") // survivor already holds one at a: it is retired
	r.link(t, "c-a", "sur-a", "p-2")

	m, created, err := r.merge("p-2", "p-1")
	require.NoError(t, err)
	require.True(t, created)
	assert.Equal(t, "p-2", m.SurvivorID)
	assert.Equal(t, "p-1", m.DuplicateID)
	assert.Equal(t, "admin-9", m.MergedBy)
	got := map[string]string{}
	for _, o := range m.LinkOutcomes {
		got[o.LinkID] = o.Action
	}
	assert.Equal(t, map[string]string{moved.LinkID: store.LinkMoved, retired.LinkID: store.LinkRetired}, got)

	l, err := r.s.GetLink(ctx, "t-1", moved.LinkID)
	require.NoError(t, err)
	assert.Equal(t, "p-2", l.PrincipalID, "the link now belongs to the survivor")
	assert.Equal(t, federation.LinkActive, l.Status)
	l, err = r.s.GetLink(ctx, "t-1", retired.LinkID)
	require.NoError(t, err)
	assert.Equal(t, "p-1", l.PrincipalID, "history is kept: a retired link stays on the duplicate")
	assert.Equal(t, "DEPROVISIONED", l.Status)

	dup, err := r.s.GetSubjectIdentity(ctx, "t-1", "p-1")
	require.NoError(t, err)
	require.NotNil(t, dup, "the duplicate keeps its row, so its id can never be issued again")
	assert.Equal(t, domain.PrincipalStatusDisabled, dup.Status)
	assert.Equal(t, "p-2", dup.MergedInto)
	sur, err := r.s.GetSubjectIdentity(ctx, "t-1", "p-2")
	require.NoError(t, err)
	assert.Empty(t, sur.MergedInto)
	assert.Equal(t, 2, sur.ActiveLinkCount)

	_, err = r.admin.Exec(ctx, `INSERT INTO principals (principal_id, tenant_id, principal_type, identity_provider_subject, email, display_name, status)
		VALUES ('p-1','t-1','HUMAN','new|subject','n@zoiko.io','New','ACTIVE')`)
	assert.Error(t, err, "a merged-away id is never reused")

	assert.Equal(t, 1, r.count(t, `SELECT count(*) FROM event_outbox WHERE event_type='identity.subject.merged'`))
	assert.Equal(t, 1, r.count(t, `SELECT count(*) FROM access_decision_log WHERE action_type='IDENTITY_MERGED_AWAY' AND principal_id='p-1'`))
	assert.Equal(t, 1, r.count(t, `SELECT count(*) FROM access_decision_log WHERE action_type='IDENTITY_MERGED_IN' AND principal_id='p-2'`))
}

func TestMerge_IsIdempotentPerDuplicate(t *testing.T) {
	r := mergeRig(t)
	first, created, err := r.merge("p-2", "p-1")
	require.NoError(t, err)
	require.True(t, created)

	again, created, err := r.merge("p-2", "p-1")
	require.NoError(t, err)
	assert.False(t, created, "the replay makes no second merge")
	assert.Equal(t, first.MergeID, again.MergeID)
	assert.Equal(t, 1, r.count(t, `SELECT count(*) FROM principal_merges`))
	assert.Equal(t, 1, r.count(t, `SELECT count(*) FROM event_outbox WHERE event_type='identity.subject.merged'`), "and no second event")

	_, _, err = r.merge("p-3", "p-1")
	assert.ErrorIs(t, err, store.ErrMergeConflict, "a duplicate is merged into one survivor only")
}

func TestMerge_RefusalsChangeNothing(t *testing.T) {
	ctx := context.Background()
	r := mergeRig(t)

	_, _, err := r.merge("p-2", "nobody")
	assert.ErrorIs(t, err, domain.ErrPrincipalNotFound)
	_, _, err = r.merge("nobody", "p-1")
	assert.ErrorIs(t, err, domain.ErrPrincipalNotFound)
	_, _, err = r.merge("p-9", "p-1")
	assert.ErrorIs(t, err, domain.ErrPrincipalNotFound, "an identity of another tenant is not mergeable")

	_, err = r.admin.Exec(ctx, `UPDATE principals SET status = 'DISABLED' WHERE principal_id = 'p-3'`)
	require.NoError(t, err)
	_, _, err = r.merge("p-3", "p-1")
	assert.ErrorIs(t, err, store.ErrMergeConflict, "a disabled survivor")

	_, _, err = r.merge("p-2", "p-1")
	require.NoError(t, err)
	_, _, err = r.merge("p-1", "p-2")
	assert.ErrorIs(t, err, store.ErrMergeConflict, "merging away the survivor of an earlier merge")
	insertPrincipal(t, ctx, r.admin, "p-4", "t-1", "fed|p4", "ACTIVE")
	_, _, err = r.merge("p-1", "p-4")
	assert.ErrorIs(t, err, store.ErrMergeConflict, "a merged-away identity cannot survive another merge")
	_, _, err = r.merge("p-4", "p-2")
	assert.ErrorIs(t, err, store.ErrMergeConflict, "a survivor cannot itself be merged away: merges do not chain")

	assert.Equal(t, 1, r.count(t, `SELECT count(*) FROM principal_merges`))
	assert.Equal(t, 1, r.count(t, `SELECT count(*) FROM event_outbox WHERE event_type='identity.subject.merged'`))
	assert.Equal(t, 1, r.count(t, `SELECT count(*) FROM principals WHERE principal_id='p-4' AND status='ACTIVE'`),
		"a refused merge leaves the duplicate alone")
}

func TestMerge_RecordIsAppendOnlyIndependentAndTenantScoped(t *testing.T) {
	ctx := context.Background()
	r := mergeRig(t)
	_, _, err := r.merge("p-2", "p-1")
	require.NoError(t, err)

	insertPrincipal(t, ctx, r.admin, "p-4", "t-1", "fed|p4", "ACTIVE")
	_, err = r.admin.Exec(ctx, `UPDATE principal_merges SET reason = 'edited'`)
	assert.Error(t, err)
	_, err = r.admin.Exec(ctx, `DELETE FROM principal_merges`)
	assert.Error(t, err, "a merge is never undone by deleting its record")

	_, err = r.admin.Exec(ctx, `INSERT INTO principal_merges (merge_id, tenant_id, survivor_id, duplicate_id, reason, merged_by)
		VALUES ('m-self','t-1','p-3','p-3','x','admin')`)
	assert.Error(t, err, "an identity cannot be merged into itself")
	_, err = r.admin.Exec(ctx, `INSERT INTO principal_merges (merge_id, tenant_id, survivor_id, duplicate_id, reason, merged_by)
		VALUES ('m-own','t-1','p-3','p-4','x','p-3')`)
	assert.Error(t, err, "the merging administrator is neither identity")
	_, err = r.admin.Exec(ctx, `INSERT INTO principal_merges (merge_id, tenant_id, survivor_id, duplicate_id, reason, merged_by)
		VALUES ('m-blank','t-1','p-3','p-4','   ','admin')`)
	assert.Error(t, err, "a merge states why")

	into, err := r.s.MergedInto(ctx, "t-2", "p-1")
	require.NoError(t, err)
	assert.Empty(t, into, "another tenant sees no merge")
	into, err = r.s.MergedInto(ctx, "t-1", "p-1")
	require.NoError(t, err)
	assert.Equal(t, "p-2", into)
}
