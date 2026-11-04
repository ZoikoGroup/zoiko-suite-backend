package lifecycle_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/lifecycle"
	"zoiko.io/identity-context-svc/internal/outbox"
	"zoiko.io/identity-context-svc/internal/store"
)

// MergeDuplicate: what the service refuses before the store is asked, how it maps the store's answers, and that
// sessions end only for a merge that was actually made.

type mergeStore struct {
	lifecycle.Store
	created bool
	err     error
	calls   int
	recs    []outbox.Record
}

func (s *mergeStore) MergeIdentities(_ context.Context, tenantID, survivorID, duplicateID, reason, actor, _ string,
	build func(m store.IdentityMerge) ([]outbox.Record, error)) (*store.IdentityMerge, bool, error) {
	s.calls++
	if s.err != nil {
		return nil, false, s.err
	}
	m := store.IdentityMerge{MergeID: "m-1", TenantID: tenantID, SurvivorID: survivorID, DuplicateID: duplicateID, Reason: reason, MergedBy: actor}
	recs, err := build(m)
	s.recs = recs
	return &m, s.created, err
}

func mergeSvc(st *mergeStore, ended *[]enderCall) *lifecycle.Service {
	end := func(_ context.Context, tenantID, principalID, reason, actor, _ string) (int, error) {
		*ended = append(*ended, enderCall{tenantID, principalID, reason, actor})
		return 1, nil
	}
	return lifecycle.New(st, nil, lifecycle.Policy{MinLength: 12}, nil, end, lifecycle.Config{}, nil, nil)
}

func TestMerge_RefusesBeforeTheStore(t *testing.T) {
	for _, tc := range []struct {
		name                               string
		actor, survivor, duplicate, reason string
		want                               error
	}{
		{"no duplicate", "admin", "p-2", "", "same person", lifecycle.ErrInvalid},
		{"no survivor", "admin", "", "p-1", "same person", lifecycle.ErrInvalid},
		{"itself", "admin", "p-1", "p-1", "same person", lifecycle.ErrInvalid},
		{"no reason", "admin", "p-2", "p-1", "  ", lifecycle.ErrInvalid},
		{"actor is the duplicate", "p-1", "p-2", "p-1", "same person", lifecycle.ErrForbidden},
		{"actor is the survivor", "p-2", "p-2", "p-1", "same person", lifecycle.ErrForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &mergeStore{created: true}
			var ended []enderCall
			_, _, err := mergeSvc(st, &ended).MergeDuplicate(context.Background(),
				lifecycle.Actor{PrincipalID: tc.actor, TenantID: "t-1"}, tc.survivor, tc.duplicate, tc.reason)
			assert.ErrorIs(t, err, tc.want)
			assert.Zero(t, st.calls, "nothing reaches the store")
			assert.Empty(t, ended)
		})
	}
}

func TestMerge_EndsTheDuplicatesSessionsAndEmitsTheEvent(t *testing.T) {
	st := &mergeStore{created: true}
	var ended []enderCall
	m, created, err := mergeSvc(st, &ended).MergeDuplicate(context.Background(),
		lifecycle.Actor{PrincipalID: "admin", TenantID: "t-1", CorrelationID: "corr"}, "p-2", "p-1", "same person")
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, "p-2", m.SurvivorID)
	require.Len(t, ended, 1)
	assert.Equal(t, enderCall{"t-1", "p-1", domain.SessionEndMerged, "admin"}, ended[0], "the duplicate's sessions end, not the survivor's")
	assert.Equal(t, []string{"identity.subject.merged"}, eventTypes(t, st.recs))
}

func TestMerge_ReplayEndsNothing(t *testing.T) {
	st := &mergeStore{created: false}
	var ended []enderCall
	_, created, err := mergeSvc(st, &ended).MergeDuplicate(context.Background(),
		lifecycle.Actor{PrincipalID: "admin", TenantID: "t-1"}, "p-2", "p-1", "same person")
	require.NoError(t, err)
	assert.False(t, created)
	assert.Empty(t, ended)
}

func TestMerge_MapsTheStoresAnswers(t *testing.T) {
	for _, tc := range []struct{ in, want error }{
		{store.ErrMergeConflict, lifecycle.ErrConflict},
		{domain.ErrPrincipalNotFound, lifecycle.ErrNotFound},
	} {
		var ended []enderCall
		_, _, err := mergeSvc(&mergeStore{err: tc.in}, &ended).MergeDuplicate(context.Background(),
			lifecycle.Actor{PrincipalID: "admin", TenantID: "t-1"}, "p-2", "p-1", "same person")
		assert.ErrorIs(t, err, tc.want)
		assert.Empty(t, ended)
	}
	_, _, err := mergeSvc(&mergeStore{err: errors.New("db down")}, new([]enderCall)).MergeDuplicate(context.Background(),
		lifecycle.Actor{PrincipalID: "admin", TenantID: "t-1"}, "p-2", "p-1", "same person")
	assert.Error(t, err)
	assert.NotErrorIs(t, err, lifecycle.ErrConflict)
}
