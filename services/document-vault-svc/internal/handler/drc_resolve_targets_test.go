package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/document-vault-svc/internal/domain"
	"zoiko.io/document-vault-svc/internal/handler"
)

// POST /v1/legal-holds/{id}:resolve-targets (ZS-SVC-S-001 §9.2) over the real handler.

type resolveStub struct {
	handler.RetentionStore
	calls  []domain.ResolveLegalHoldTargetsParams
	result *domain.HoldResolution
	err    error
}

func (s *resolveStub) ResolveLegalHoldTargets(_ context.Context, p domain.ResolveLegalHoldTargetsParams) (*domain.HoldResolution, error) {
	if s.err != nil {
		return nil, s.err
	}
	s.calls = append(s.calls, p)
	if s.result != nil {
		return s.result, nil
	}
	res := &domain.HoldResolution{ResolutionID: "snap-1", HoldID: p.HoldID, HoldStatus: domain.LegalHoldActive, Complete: true,
		Resolved: []domain.ResolvedTarget{}, Unresolved: []domain.UnresolvedTarget{}}
	for _, id := range p.RecordIDs {
		res.Resolved = append(res.Resolved, domain.ResolvedTarget{RecordID: id, Outcome: domain.TargetAdded, Applied: true})
	}
	return res, nil
}

func resolveRouter(t *testing.T, az *entityActionAuthz, st *resolveStub, identity bool) http.Handler {
	return drcRouter(t, newStubStore(), az, nil, nil, st, identity)
}

func TestResolveTargets_ReturnsTheSnapshot(t *testing.T) {
	st := &resolveStub{}
	az := &entityActionAuthz{}
	w := call(resolveRouter(t, az, st, true), http.MethodPost, "/v1/legal-holds/hold-7:resolve-targets",
		`{"record_ids":["rec-1"," rec-2 ","rec-1",""]}`, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, st.calls, 1)
	assert.Equal(t, "hold-7", st.calls[0].HoldID, "the id before ':resolve-targets' is the hold id")
	assert.Equal(t, []string{"rec-1", "rec-2"}, st.calls[0].RecordIDs, "trimmed, de-duplicated, blanks dropped")
	assert.Equal(t, testPrincipal, st.calls[0].ResolvedByPrincipalID)
	assert.Equal(t, []string{"|LEGAL_HOLD_MANAGE"}, az.asked, "the same grant as the other hold-management commands")
	var m map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &m))
	assert.Equal(t, "snap-1", m["resolution_id"])
	assert.Equal(t, true, m["complete"])
	assert.Len(t, m["resolved_targets"], 2)
	assert.Empty(t, m["unresolved_targets"])
}

func TestResolveTargets_PartialFailureIsExposed(t *testing.T) {
	st := &resolveStub{result: &domain.HoldResolution{ResolutionID: "snap-2", HoldID: "hold-7", HoldStatus: domain.LegalHoldActive, Complete: false,
		Resolved:   []domain.ResolvedTarget{{RecordID: "rec-1", Outcome: domain.TargetAdded, Applied: true}},
		Unresolved: []domain.UnresolvedTarget{{RecordID: "rec-x", Reason: domain.UnresolvedRecordNotFound}}}}
	w := call(resolveRouter(t, &entityActionAuthz{}, st, true), http.MethodPost, "/v1/legal-holds/hold-7:resolve-targets", `{"record_ids":["rec-1","rec-x"]}`, nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"complete":false`, "the hold is not presented as complete")
	assert.Contains(t, w.Body.String(), `"reason":"RECORD_NOT_FOUND"`)
	assert.Contains(t, w.Body.String(), "rec-x")
}

func TestResolveTargets_ValidatesAndFailsClosed(t *testing.T) {
	st := &resolveStub{}
	r := resolveRouter(t, &entityActionAuthz{}, st, true)
	path := "/v1/legal-holds/hold-7:resolve-targets"
	assert.Equal(t, http.StatusBadRequest, call(r, http.MethodPost, path, `{}`, nil).Code)
	assert.Equal(t, http.StatusBadRequest, call(r, http.MethodPost, path, `{"record_ids":[]}`, nil).Code)
	assert.Equal(t, http.StatusBadRequest, call(r, http.MethodPost, path, `{"record_ids":["   "]}`, nil).Code)
	assert.Equal(t, http.StatusBadRequest, call(r, http.MethodPost, path, `{"record_ids":["a"],"custodians":["bob"]}`, nil).Code, "unsupported scope selectors are refused, not ignored")
	many := make([]string, 501)
	for i := range many {
		many[i] = fmt.Sprintf("rec-%d", i)
	}
	b, _ := json.Marshal(map[string]any{"record_ids": many})
	w := call(r, http.MethodPost, path, string(b), nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "too_many_records")
	assert.Empty(t, st.calls)

	st = &resolveStub{}
	assert.Equal(t, http.StatusForbidden, call(resolveRouter(t, &entityActionAuthz{denyAll: true}, st, true), http.MethodPost, path, `{"record_ids":["a"]}`, nil).Code)
	assert.Equal(t, http.StatusServiceUnavailable, call(resolveRouter(t, &entityActionAuthz{err: domain.ErrAuthzServiceUnavailable}, st, true), http.MethodPost, path, `{"record_ids":["a"]}`, nil).Code)
	assert.Equal(t, http.StatusUnauthorized, call(resolveRouter(t, &entityActionAuthz{}, st, false), http.MethodPost, path, `{"record_ids":["a"]}`, nil).Code)
	assert.Empty(t, st.calls, "nothing is resolved for a caller who is not authorized")
}

func TestResolveTargets_StoreErrorsKeepTheirStatus(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"hold not found (or another tenant's)": {domain.ErrLegalHoldNotFound, http.StatusNotFound},
		"hold released":                        {domain.ErrLegalHoldReleased, http.StatusConflict},
	} {
		st := &resolveStub{err: tc.err}
		w := call(resolveRouter(t, &entityActionAuthz{}, st, true), http.MethodPost, "/v1/legal-holds/hold-7:resolve-targets", `{"record_ids":["a"]}`, nil)
		assert.Equal(t, tc.want, w.Code, name)
	}
}
