package context_test

import (
	"bytes"
	"context"
	"encoding/json"
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

// steppedUpRouter is lifecycleRouter with the second factor wired, so the
// commands that need a step-up read it from the caller's own session.
func steppedUpRouter(ls *lifecycle.Service, az identityctx.AuthzChecker, fx *mfaFixture) chi.Router {
	h := identityctx.NewHandler(nil, nil, nil, nil, az, zap.NewNop()).WithMFA(fx.svc).WithLifecycle(ls)
	r := chi.NewRouter()
	identityctx.RegisterRoutes(r, h)
	return r
}

func postAs(t *testing.T, r chi.Router, path, principalID, bearer, ifMatch string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", mfaTenant)
	req.Header.Set("X-Principal-Id", principalID)
	req.Header.Set("X-Correlation-ID", "test-correlation")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

type endCall struct{ tenantID, principalID string }

func mergeService(st *mockLifecycleStore, ended *[]endCall) *lifecycle.Service {
	end := func(_ context.Context, tenantID, principalID, _, _, _ string) (int, error) {
		*ended = append(*ended, endCall{tenantID, principalID})
		return 1, nil
	}
	return lifecycle.New(st, nil, lifecycle.Policy{MinLength: 12}, nil, end,
		lifecycle.Config{StepUpMaxAge: time.Minute}, nil, zap.NewNop())
}

func TestORG01_MergeRequiresStepUp(t *testing.T) {
	fx := newMFAFixture(t)
	var ended []endCall
	st := &mockLifecycleStore{}
	r := steppedUpRouter(mergeService(st, &ended), &permittingAuthz{}, fx)

	for name, bearer := range map[string]string{
		"password only": fx.session(admin, 10*time.Second, false),
		"stale step-up": fx.session(admin, 2*time.Minute, true),
	} {
		w := postAs(t, r, "/v1/subjects/P-SURVIVOR/merge", admin, bearer, `"v3"`,
			map[string]any{"duplicate_id": "P-DUP", "reason": "same person, two accounts"})
		assert.Equal(t, http.StatusUnauthorized, w.Code, name+": "+w.Body.String())
		assert.Contains(t, w.Body.String(), "step_up_required", name)
	}
	assert.Empty(t, ended, "a refused merge ends no session")
	assert.Zero(t, st.mergeVersion, "a refused merge never reaches the store")
}

func TestORG01_MergeIntoSelfRefused(t *testing.T) {
	fx := newMFAFixture(t)
	var ended []endCall
	r := steppedUpRouter(mergeService(&mockLifecycleStore{}, &ended), &permittingAuthz{}, fx)

	w := postAs(t, r, "/v1/subjects/P-SAME/merge", admin, fx.session(admin, 10*time.Second, true), `"v1"`,
		map[string]any{"duplicate_id": "P-SAME", "reason": "typo"})

	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

func TestORG01_MergeActorCannotBeEitherSubject(t *testing.T) {
	fx := newMFAFixture(t)
	var ended []endCall
	r := steppedUpRouter(mergeService(&mockLifecycleStore{}, &ended), &permittingAuthz{}, fx)

	w := postAs(t, r, "/v1/subjects/P-SURVIVOR/merge", admin, fx.session(admin, 10*time.Second, true), `"v1"`,
		map[string]any{"duplicate_id": admin, "reason": "merging myself away"})

	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
}

func TestORG01_MergeNeedsIfMatchAndReason(t *testing.T) {
	fx := newMFAFixture(t)
	var ended []endCall
	r := steppedUpRouter(mergeService(&mockLifecycleStore{}, &ended), &permittingAuthz{}, fx)
	bearer := fx.session(admin, 10*time.Second, true)

	w := postAs(t, r, "/v1/subjects/P-SURVIVOR/merge", admin, bearer, "",
		map[string]any{"duplicate_id": "P-DUP", "reason": "dup"})
	assert.Equal(t, http.StatusPreconditionRequired, w.Code, w.Body.String())

	w = postAs(t, r, "/v1/subjects/P-SURVIVOR/merge", admin, bearer, `"v2"`,
		map[string]any{"duplicate_id": "P-DUP", "reason": "  "})
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

func TestORG01_MergeStaleVersionIs412(t *testing.T) {
	fx := newMFAFixture(t)
	var ended []endCall
	st := &mockLifecycleStore{mergeErr: store.ErrMergeStale}
	r := steppedUpRouter(mergeService(st, &ended), &permittingAuthz{}, fx)

	w := postAs(t, r, "/v1/subjects/P-SURVIVOR/merge", admin, fx.session(admin, 10*time.Second, true), `"v2"`,
		map[string]any{"duplicate_id": "P-DUP", "reason": "duplicate HR import"})

	assert.Equal(t, http.StatusPreconditionFailed, w.Code, w.Body.String())
	assert.Equal(t, int64(2), st.mergeVersion, "the If-Match version reaches the store's locked compare")
	assert.Empty(t, ended)
}

func TestORG01_MergeEndsDuplicateSessions(t *testing.T) {
	fx := newMFAFixture(t)
	var ended []endCall
	r := steppedUpRouter(mergeService(&mockLifecycleStore{}, &ended), &permittingAuthz{}, fx)

	w := postAs(t, r, "/v1/subjects/P-SURVIVOR/merge", admin, fx.session(admin, 10*time.Second, true), `"v4"`,
		map[string]any{"duplicate_id": "P-DUP", "reason": "duplicate HR import"})

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, ended, 1)
	assert.Equal(t, endCall{mfaTenant, "P-DUP"}, ended[0], "every session of the duplicate ends; the survivor's do not")
}

func TestORG01_MergeNeedsTheMergeAction(t *testing.T) {
	fx := newMFAFixture(t)
	var ended []endCall
	r := steppedUpRouter(mergeService(&mockLifecycleStore{}, &ended), &denyingAuthz{}, fx)

	w := postAs(t, r, "/v1/subjects/P-SURVIVOR/merge", admin, fx.session(admin, 10*time.Second, true), `"v4"`,
		map[string]any{"duplicate_id": "P-DUP", "reason": "duplicate"})

	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Empty(t, ended)
}
