package context_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	identityctx "zoiko.io/identity-context-svc/internal/context"
	"zoiko.io/identity-context-svc/internal/lifecycle"
	"zoiko.io/identity-context-svc/internal/store"
)

// POST /v1/identities/{survivorID}/merge (ORG-01 MergeDuplicateIdentity).

type fakeMerger struct {
	m       *store.IdentityMerge
	created bool
	err     error

	calls                                                 int
	tenant, actor, survivor, duplicate, reason, corrIDGot string
}

func (f *fakeMerger) MergeDuplicate(_ context.Context, tenantID string, actor lifecycle.Actor, survivorID, duplicateID, reason string, duplicateVersion int64, corr string) (*store.IdentityMerge, bool, error) {
	f.calls++
	f.tenant, f.actor, f.survivor, f.duplicate, f.reason, f.corrIDGot = tenantID, actor.PrincipalID, survivorID, duplicateID, reason, corr
	return f.m, f.created, f.err
}

func mergeRouter(m identityctx.IdentityMerger, authz identityctx.AuthzChecker, fx *mfaFixture) (http.Handler, *permittingAuthz) {
	pa := &permittingAuthz{}
	if authz == nil {
		authz = pa
	}
	h := identityctx.NewHandler(nil, nil, nil, nil, authz, zap.NewNop()).WithMFA(fx.svc).WithIdentityMerger(m)
	r := chi.NewRouter()
	identityctx.RegisterRoutes(r, h)
	return r, pa
}

func postMerge(router http.Handler, body string, ifMatch string, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/identities/p-2/merge", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", mfaTenant)
	req.Header.Set("X-Principal-Id", admin)
	req.Header.Set("X-Correlation-ID", "corr-merge")
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

const mergeBody = `{"duplicate_id":"p-1","reason":"same person"}`

func TestMergeDuplicateIdentity_MadeIs201AndReplayIs200(t *testing.T) {
	fx := newMFAFixture(t)
	f := &fakeMerger{m: &store.IdentityMerge{MergeID: "m-1", SurvivorID: "p-2", DuplicateID: "p-1"}, created: true}
	router, authz := mergeRouter(f, nil, fx)

	bearer := fx.session(admin, time.Minute, true)
	w := postMerge(router, mergeBody, `W/"v1"`, bearer)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"merge_id":"m-1"`)
	assert.Equal(t, []string{identityctx.ActionIdentityMerge}, authz.seen)
	assert.Equal(t, mfaTenant, f.tenant)
	assert.Equal(t, admin, f.actor, "the actor is the verified caller")
	assert.Equal(t, "p-2", f.survivor)
	assert.Equal(t, "p-1", f.duplicate)
	assert.Equal(t, "same person", f.reason)
	assert.Equal(t, "corr-merge", f.corrIDGot)

	f.created = false
	assert.Equal(t, http.StatusOK, postMerge(router, mergeBody, `W/"v1"`, bearer).Code, "an identical replay is not a second merge")
}

func TestMergeDuplicateIdentity_AlwaysNeedsTheGrant(t *testing.T) {
	fx := newMFAFixture(t)
	f := &fakeMerger{m: &store.IdentityMerge{}, created: true}
	router, _ := mergeRouter(f, denyingAuthz{}, fx)
	bearer := fx.session(admin, time.Minute, true)
	assert.Equal(t, http.StatusForbidden, postMerge(router, mergeBody, `W/"v1"`, bearer).Code)
	assert.Zero(t, f.calls)

	router, _ = mergeRouter(f, outageAuthz{}, fx)
	assert.Equal(t, http.StatusServiceUnavailable, postMerge(router, mergeBody, `W/"v1"`, bearer).Code)
	assert.Zero(t, f.calls)
}

func TestMergeDuplicateIdentity_UnconfiguredIs501(t *testing.T) {
	fx := newMFAFixture(t)
	router, _ := mergeRouter(nil, nil, fx)
	bearer := fx.session(admin, time.Minute, true)
	assert.Equal(t, http.StatusNotImplemented, postMerge(router, mergeBody, `W/"v1"`, bearer).Code)
}

func TestMergeDuplicateIdentity_BadBodyIs400(t *testing.T) {
	fx := newMFAFixture(t)
	f := &fakeMerger{m: &store.IdentityMerge{}}
	router, _ := mergeRouter(f, nil, fx)
	bearer := fx.session(admin, time.Minute, true)
	for _, body := range []string{``, `not json`, `{"duplicate_id":"p-1","reason":"x","survivor_id":"p-9"}`} {
		assert.Equal(t, http.StatusBadRequest, postMerge(router, body, `W/"v1"`, bearer).Code, body)
	}
	assert.Zero(t, f.calls, "an unknown field cannot name a different survivor")
}

func TestMergeDuplicateIdentity_MapsRefusals(t *testing.T) {
	fx := newMFAFixture(t)
	bearer := fx.session(admin, time.Minute, true)
	for _, tc := range []struct {
		err  error
		want int
	}{
		{fmt.Errorf("%w: a reason is required", lifecycle.ErrInvalid), http.StatusBadRequest},
		{fmt.Errorf("%w: you cannot merge your own identity", lifecycle.ErrForbidden), http.StatusForbidden},
		{fmt.Errorf("%w: duplicate p-1", lifecycle.ErrNotFound), http.StatusNotFound},
		{fmt.Errorf("%w: already merged", lifecycle.ErrConflict), http.StatusConflict},
		{errors.New("db down"), http.StatusInternalServerError},
	} {
		router, _ := mergeRouter(&fakeMerger{err: tc.err}, nil, fx)
		w := postMerge(router, mergeBody, `W/"v1"`, bearer)
		assert.Equal(t, tc.want, w.Code, tc.err.Error())
		assert.NotContains(t, w.Body.String(), "db down", "an internal error is never returned")
	}
}
