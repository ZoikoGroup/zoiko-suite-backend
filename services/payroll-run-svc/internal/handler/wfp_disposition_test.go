package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/handler"
	"zoiko.io/payroll-run-svc/internal/middleware"
	"zoiko.io/payroll-run-svc/internal/retention"
)

type stubRetention struct {
	res    *retention.Resolution
	err    error
	calls  int
	class  string
	entity string
	caller retention.Caller
}

func (s *stubRetention) Resolve(_ context.Context, c retention.Caller, class, _, entityRef string) (*retention.Resolution, error) {
	s.calls++
	s.class, s.entity, s.caller = class, entityRef, c
	return s.res, s.err
}

const dispBase = "/v1/payroll/wfp/disposition-check"

func newDispRouter(rr handler.RetentionResolver, az handler.AuthZClient) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc")))
		})
	})
	handler.RegisterWFPDispositionRoutes(r, handler.NewWFPDisposition(rr, az, zap.NewNop()))
	return r
}

func clearPolicy() *retention.Resolution {
	return &retention.Resolution{Policy: &retention.Policy{ID: "pol-1", MinRetentionDays: 365}}
}

func decode(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(body, &m))
	return m
}

func TestWFP041_ARecordPastRetentionWithNoHoldMayBeDisposedOf(t *testing.T) {
	rr := &stubRetention{res: clearPolicy()}
	r := newDispRouter(rr, &stubAuthZ{})
	w := inDo(r, "GET", dispBase+"?record_class=WFP_PAYROLL_RESULT&record_created_at=2015-01-01", nil, inCaller)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	out := decode(t, w.Body.Bytes())
	assert.Equal(t, true, out["allowed"])
	assert.Equal(t, "tenant-abc", rr.caller.TenantID, "the verified tenant is forwarded, never one the caller names")
	assert.Equal(t, "le-1", rr.entity, "the entity defaults to the authorized one")
}

func TestWFP041_ALegalHoldBlocksDisposition(t *testing.T) {
	rr := &stubRetention{res: &retention.Resolution{Blocked: true, Hold: &retention.Hold{ID: "h-9", Authority: "court order"},
		Policy: &retention.Policy{ID: "pol-1", MinRetentionDays: 365}}}
	w := inDo(newDispRouter(rr, &stubAuthZ{}), "GET", dispBase+"?record_class=WFP_LIABILITY&record_created_at=2015-01-01", nil, inCaller)
	require.Equal(t, http.StatusOK, w.Code)
	out := decode(t, w.Body.Bytes())
	assert.Equal(t, false, out["allowed"])
	assert.Equal(t, "h-9", out["legal_hold_id"])
	assert.Contains(t, out["reasons"], domain.DispositionHoldActive)
}

func TestWFP041_RecentRecordsAndMissingFactsAreRefused(t *testing.T) {
	rr := &stubRetention{res: clearPolicy()}
	r := newDispRouter(rr, &stubAuthZ{})
	w := inDo(r, "GET", dispBase+"?record_class=WFP_PAYROLL_RESULT&record_created_at=2999-01-01", nil, inCaller)
	assert.Contains(t, decode(t, w.Body.Bytes())["reasons"], domain.DispositionRetentionNotOver)
	w = inDo(r, "GET", dispBase+"?record_class=WFP_PAYROLL_RESULT", nil, inCaller)
	out := decode(t, w.Body.Bytes())
	assert.Equal(t, false, out["allowed"], "without the record date retention cannot be judged")
	assert.Contains(t, out["reasons"], domain.DispositionRetentionNotKnown)

	rr.res = &retention.Resolution{}
	w = inDo(r, "GET", dispBase+"?record_class=WFP_PAYROLL_RESULT&record_created_at=2015-01-01", nil, inCaller)
	assert.Contains(t, decode(t, w.Body.Bytes())["reasons"], domain.DispositionNoPolicy)
}

func TestWFP041_WhenTheRegistryCannotAnswerNothingIsAllowed(t *testing.T) {
	rr := &stubRetention{err: domain.ErrRetentionUnavailable}
	w := inDo(newDispRouter(rr, &stubAuthZ{}), "GET", dispBase+"?record_class=WFP_PAYROLL_RESULT&record_created_at=2015-01-01", nil, inCaller)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	out := decode(t, w.Body.Bytes())
	assert.Equal(t, false, out["allowed"])
	assert.Equal(t, "retention_unavailable", out["error_code"])
}

func TestWFP041_IdentityAuthorizationAndInputAreChecked(t *testing.T) {
	rr := &stubRetention{res: clearPolicy()}
	r := newDispRouter(rr, &stubAuthZ{})
	url := dispBase + "?record_class=WFP_PAYROLL_RESULT&record_created_at=2015-01-01"
	assert.Equal(t, http.StatusUnauthorized, inDo(r, "GET", url, nil, caller2{}).Code)
	assert.Equal(t, http.StatusBadRequest, inDo(r, "GET", url, nil, caller2{principal: "p-1"}).Code, "an entity is required")
	assert.Equal(t, http.StatusBadRequest, inDo(r, "GET", dispBase+"?record_class=AUDIT_LOG", nil, inCaller).Code, "only WFP classes")
	assert.Equal(t, http.StatusBadRequest, inDo(r, "GET", dispBase+"?record_class=WFP_LIABILITY&record_created_at=yesterday", nil, inCaller).Code)
	assert.Zero(t, rr.calls, "nothing reached the registry")

	assert.Equal(t, http.StatusForbidden, inDo(newDispRouter(rr, &stubAuthZ{err: domain.ErrAuthorizationDenied}), "GET", url, nil, inCaller).Code)
	assert.Equal(t, http.StatusServiceUnavailable, inDo(newDispRouter(rr, &stubAuthZ{err: domain.ErrAuthzServiceUnavailable}), "GET", url, nil, inCaller).Code)
	assert.Zero(t, rr.calls)
}
