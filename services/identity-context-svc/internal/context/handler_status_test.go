package context_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	identityctx "zoiko.io/identity-context-svc/internal/context"
	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/lifecycle"
)

// PUT /v1/principals/{id}/status goes through the governed status command
// (Group 1 re-audit F-3, 19 Oct 2026). It used to update the row and the
// decision log only: no event left the service and no session ended.

type recordingStatusChanger struct {
	calls                                        int
	tenantID, actorID, principalID, reason, corr string
	status                                       domain.PrincipalStatus
	err                                          error
}

func (r *recordingStatusChanger) ChangeStatus(_ context.Context, tenantID, actorID, principalID string,
	status domain.PrincipalStatus, reason, corr string) error {
	r.calls++
	r.tenantID, r.actorID, r.principalID, r.status, r.reason, r.corr = tenantID, actorID, principalID, status, reason, corr
	return r.err
}

func statusRouter(changer identityctx.StatusChanger) http.Handler {
	store := &mockPrincipalStore{principal: &domain.Principal{PrincipalID: "p-1", TenantID: "tenant-a", RecordVersion: 1}}
	h := identityctx.NewHandler(nil, nil, nil, store, &permittingAuthz{}, zap.NewNop())
	if changer != nil {
		h.WithStatusChanger(changer)
	}
	r := chi.NewRouter()
	identityctx.RegisterRoutes(r, h)
	return r
}

func putStatus(t *testing.T, router http.Handler, status domain.PrincipalStatus) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(domain.UpdateStatusRequest{Status: status, Reason: "left the company"})
	req := httptest.NewRequest(http.MethodPut, "/v1/principals/p-1/status", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", "tenant-a")
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Correlation-ID", "corr-status")
	req.Header.Set("If-Match", `"v1"`)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func TestUpdatePrincipalStatus_GoesThroughTheGovernedCommand(t *testing.T) {
	changer := &recordingStatusChanger{}
	w := putStatus(t, statusRouter(changer), domain.PrincipalStatusDisabled)

	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	require.Equal(t, 1, changer.calls)
	assert.Equal(t, "tenant-a", changer.tenantID)
	assert.Equal(t, "admin-1", changer.actorID, "the actor is the verified caller")
	assert.Equal(t, "p-1", changer.principalID)
	assert.Equal(t, domain.PrincipalStatusDisabled, changer.status)
	assert.Equal(t, "left the company", changer.reason)
	assert.Equal(t, "corr-status", changer.corr)
}

// An unconfigured status command is unavailable, never a silent row update.
func TestUpdatePrincipalStatus_UnconfiguredIs501(t *testing.T) {
	w := putStatus(t, statusRouter(nil), domain.PrincipalStatusDisabled)
	assert.Equal(t, http.StatusNotImplemented, w.Code)
}

func TestUpdatePrincipalStatus_MapsLifecycleRefusals(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{fmt.Errorf("%w: you cannot disable your own account", lifecycle.ErrForbidden), http.StatusForbidden},
		{fmt.Errorf("%w: status must be ACTIVE, SUSPENDED or DISABLED", lifecycle.ErrInvalid), http.StatusBadRequest},
		{fmt.Errorf("store down"), http.StatusInternalServerError},
	} {
		w := putStatus(t, statusRouter(&recordingStatusChanger{err: tc.err}), domain.PrincipalStatusDisabled)
		assert.Equal(t, tc.want, w.Code, tc.err.Error())
	}
}
