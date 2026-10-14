package handler_test

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

	"zoiko.io/leave-absence-svc/internal/domain"
	"zoiko.io/leave-absence-svc/internal/handler"
	"zoiko.io/leave-absence-svc/internal/middleware"
	"zoiko.io/leave-absence-svc/internal/store"
	"zoiko.io/leave-absence-svc/internal/workerctx"
)

// ── stubs ────────────────────────────────────────────────────────────────────

type stubWFPStore struct {
	sources []domain.SourceLeave
	batch   *domain.TimeLeaveBatch

	importedLines []domain.TimeLeaveLine
	importCalls   int
	importErr     error
	importResult  *store.BatchResult
	built         []domain.OutboxEvent

	freezeErr, supersedeErr, quarantineErr error
	supersedeCalls                         int
	supersedeLines                         []domain.TimeLeaveLine
	supersedePolicy                        string
	issues                                 []domain.SourceIssue
}

func (s *stubWFPStore) ListApprovedLeaveForPeriod(context.Context, string, string, string) ([]domain.SourceLeave, error) {
	return s.sources, nil
}

func (s *stubWFPStore) ImportTimeLeaveBatch(_ context.Context, b *domain.TimeLeaveBatch, lines []domain.TimeLeaveLine, mk store.EventBuilder) (*store.BatchResult, error) {
	s.importCalls++
	s.importedLines = lines
	if s.importErr != nil {
		return nil, s.importErr
	}
	res := s.importResult
	if res == nil {
		b.State, b.Version = domain.BatchAccepted, 1
		res = &store.BatchResult{Batch: b, Lines: lines, Created: true}
	}
	if mk != nil {
		evs, err := mk(store.BatchChange{Kind: store.ChangeImport, Batch: res.Batch, Lines: res.Lines})
		if err != nil {
			return nil, err
		}
		s.built = evs
	}
	return res, nil
}

func (s *stubWFPStore) SupersedeTimeLeaveBatch(_ context.Context, _ string, _ int, nb *domain.TimeLeaveBatch, lines []domain.TimeLeaveLine, reason string, mk store.EventBuilder) (*store.BatchResult, error) {
	s.supersedeCalls++
	s.supersedeLines, s.supersedePolicy = lines, nb.OnInvalid
	if s.supersedeErr != nil {
		return nil, s.supersedeErr
	}
	nb.State, nb.Version = domain.BatchAccepted, 1
	if mk != nil {
		evs, err := mk(store.BatchChange{Kind: store.ChangeSupersede, Batch: nb, Lines: lines, Superseded: s.batch, Reason: reason})
		if err != nil {
			return nil, err
		}
		s.built = evs
	}
	return &store.BatchResult{Batch: nb, Lines: lines, Created: true}, nil
}

func (s *stubWFPStore) ValidateTimeLeaveBatch(context.Context, string) ([]domain.SourceIssue, error) {
	return s.issues, nil
}

func (s *stubWFPStore) FreezeTimeLeaveBatch(_ context.Context, _ string, _ int, actor string, mk store.EventBuilder) (*domain.TimeLeaveBatch, error) {
	if s.freezeErr != nil {
		return nil, s.freezeErr
	}
	b := *s.batch
	b.State, b.FrozenBy = domain.BatchFrozen, &actor
	if mk != nil {
		evs, err := mk(store.BatchChange{Kind: store.ChangeFreeze, Batch: &b})
		if err != nil {
			return nil, err
		}
		s.built = evs
	}
	return &b, nil
}

func (s *stubWFPStore) QuarantineLine(_ context.Context, _, lineID string, _ int, mk store.EventBuilder) (*domain.TimeLeaveBatch, *domain.TimeLeaveLine, error) {
	if s.quarantineErr != nil {
		return nil, nil, s.quarantineErr
	}
	reason := domain.ReasonManual
	l := &domain.TimeLeaveLine{LineID: lineID, WorkerID: "w-1", State: domain.LineQuarantined, QuarantineReason: &reason}
	if mk != nil {
		evs, err := mk(store.BatchChange{Kind: store.ChangeQuarantine, Batch: s.batch, Lines: []domain.TimeLeaveLine{*l}})
		if err != nil {
			return nil, nil, err
		}
		s.built = evs
	}
	return s.batch, l, nil
}

func (s *stubWFPStore) GetTimeLeaveBatch(context.Context, string) (*domain.TimeLeaveBatch, error) {
	if s.batch == nil {
		return nil, domain.ErrBatchNotFound
	}
	return s.batch, nil
}

func (s *stubWFPStore) ListTimeLeaveBatches(context.Context, string, string, string) ([]domain.TimeLeaveBatch, error) {
	return nil, nil
}

func (s *stubWFPStore) ListBatchLines(context.Context, string, string, string) ([]domain.TimeLeaveLine, error) {
	return nil, nil
}

func (s *stubWFPStore) GetWorkerImportedUnits(_ context.Context, _, w string) (*store.WorkerUnits, error) {
	return &store.WorkerUnits{WorkerID: w, TotalUnits: "8.00"}, nil
}

type stubWorkers struct {
	results map[string]workerctx.Result
	err     error
	calls   []string
	caller  workerctx.Caller
	asOf    string
}

func (w *stubWorkers) Validate(_ context.Context, c workerctx.Caller, worker, _, asOf string) (workerctx.Result, error) {
	w.calls = append(w.calls, worker)
	w.caller, w.asOf = c, asOf
	if w.err != nil {
		return workerctx.Result{}, w.err
	}
	if r, ok := w.results[worker]; ok {
		return r, nil
	}
	return workerctx.Result{Valid: true}, nil
}

// recordingAuthZ records the entity and action each check was made against.
type recordingAuthZ struct {
	err      error
	entities []string
	actions  []string
}

func (a *recordingAuthZ) CheckAllowed(_ context.Context, _, entity, action string) error {
	a.entities, a.actions = append(a.entities, entity), append(a.actions, action)
	return a.err
}

// ── harness ──────────────────────────────────────────────────────────────────

const wfpBase = "/v1/wfp/time-leave-batches"

func newWFPRouter(st *stubWFPStore, az handler.AuthZClient, w *stubWorkers) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(rw, req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc")))
		})
	})
	handler.RegisterWFPRoutes(r, handler.NewWFP(st, az, w, zap.NewNop()))
	return r
}

func wfpDo(r chi.Router, method, path string, body any, principal, ifMatch string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func errCode(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var b map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &b))
	s, _ := b["error_code"].(string)
	return s
}

func importBody() map[string]any {
	return map[string]any{"legal_entity_id": "le-1", "payroll_period_id": "2026-09", "period_start": "2026-09-01", "period_end": "2026-09-30"}
}

func leave(id, worker, hours string) domain.SourceLeave {
	by, at := "mgr-1", time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	return domain.SourceLeave{
		RequestID: id, WorkerID: worker, LeaveTypeID: "t-1", LeaveTypeCode: "VACATION", IsPaid: true,
		StartDate: "2026-09-10", EndDate: "2026-09-10", Units: hours, Status: "APPROVED", ApprovedBy: &by, ApprovedAt: &at,
	}
}

func liveBatch() *domain.TimeLeaveBatch {
	return &domain.TimeLeaveBatch{
		BatchID: "b-1", LegalEntityID: "le-ENTITY-OF-BATCH", PayrollPeriodID: "2026-09", PeriodStart: "2026-09-01",
		PeriodEnd: "2026-09-30", OnInvalid: domain.OnInvalidRejectBatch, State: domain.BatchAccepted, Version: 1,
	}
}

// ── identity & authorization ─────────────────────────────────────────────────

func TestWFP02_IdentityAuthorizationAndForgedTenant(t *testing.T) {
	st, w := &stubWFPStore{}, &stubWorkers{}
	r := newWFPRouter(st, &stubAuthZ{}, w)
	assert.Equal(t, http.StatusUnauthorized, wfpDo(r, "POST", wfpBase, importBody(), "", "").Code)

	denied := newWFPRouter(st, &stubAuthZ{err: domain.ErrAuthorizationDenied}, w)
	assert.Equal(t, http.StatusForbidden, wfpDo(denied, "POST", wfpBase, importBody(), "p-1", "").Code)
	down := newWFPRouter(st, &stubAuthZ{err: domain.ErrAuthzServiceUnavailable}, w)
	assert.Equal(t, http.StatusServiceUnavailable, wfpDo(down, "POST", wfpBase, importBody(), "p-1", "").Code, "fail closed")

	b := importBody()
	b["tenant_id"] = "someone-elses"
	rr := wfpDo(r, "POST", wfpBase, b, "p-1", "")
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Equal(t, "invalid_json", errCode(t, rr))
	assert.Zero(t, st.importCalls)
}

func TestWFP02_BatchActionsAreAuthorizedAgainstTheBatchsOwnEntity(t *testing.T) {
	st := &stubWFPStore{batch: liveBatch()}
	az := &recordingAuthZ{}
	r := newWFPRouter(st, az, &stubWorkers{})

	require.Equal(t, http.StatusOK, wfpDo(r, "GET", wfpBase+"/b-1", nil, "p-1", "").Code)
	require.Equal(t, http.StatusOK, wfpDo(r, "POST", wfpBase+"/b-1/freeze", map[string]any{"expected_version": 1}, "p-1", "").Code)
	assert.Equal(t, []string{"le-ENTITY-OF-BATCH", "le-ENTITY-OF-BATCH"}, az.entities,
		"scope comes from the stored batch, never from a header the caller chose")
	assert.Equal(t, []string{"WFP_TIME_LEAVE_VIEW", "WFP_TIME_LEAVE_FREEZE"}, az.actions)

	assert.Equal(t, http.StatusNotFound, wfpDo(newWFPRouter(&stubWFPStore{}, az, &stubWorkers{}), "GET", wfpBase+"/b-1", nil, "p-1", "").Code)
}

// ── import ───────────────────────────────────────────────────────────────────

func TestWFP02_ImportAsksWorkerContextOncePerWorkerAndQuarantinesInvalid(t *testing.T) {
	st := &stubWFPStore{sources: []domain.SourceLeave{leave("r-1", "w-ok", "8.00"), leave("r-2", "w-ok", "4.00"), leave("r-3", "w-bad", "8.00")}}
	w := &stubWorkers{results: map[string]workerctx.Result{"w-bad": {Valid: false, Reasons: []string{"NO_ACTIVE_PROFILE"}}}}
	r := newWFPRouter(st, &stubAuthZ{}, w)

	body := importBody()
	body["on_invalid"] = "QUARANTINE_LINES"
	rr := wfpDo(r, "POST", wfpBase, body, "p-1", "")
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())

	assert.ElementsMatch(t, []string{"w-ok", "w-bad"}, w.calls, "one worker-context call per distinct worker")
	assert.Equal(t, "2026-09-30", w.asOf, "validated as of the period end")
	assert.Equal(t, "tenant-abc", w.caller.TenantID, "the authenticated tenant is forwarded")
	assert.Equal(t, "p-1", w.caller.PrincipalID)
	assert.Equal(t, "le-1", w.caller.LegalEntityID)

	byID := map[string]domain.TimeLeaveLine{}
	for _, l := range st.importedLines {
		byID[l.SourceLineID] = l
	}
	require.Len(t, byID, 3)
	assert.Equal(t, domain.LineAccepted, byID["r-1"].State)
	assert.Equal(t, domain.LineAccepted, byID["r-2"].State)
	assert.Equal(t, domain.LineQuarantined, byID["r-3"].State)
	assert.Equal(t, "NO_ACTIVE_PROFILE", *byID["r-3"].QuarantineReason)
	assert.Equal(t, "8.00", byID["r-1"].Units, "hours pass through as the exact decimal text")
	assert.Equal(t, domain.UnitHours, byID["r-1"].UnitOfMeasure)

	var out struct {
		Quarantined []domain.TimeLeaveLine `json:"quarantined_lines"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	require.Len(t, out.Quarantined, 1)
	assert.Equal(t, "r-3", out.Quarantined[0].SourceLineID)
}

func TestWFP02_ImportFailsClosedWhenWorkerContextUnavailable(t *testing.T) {
	st := &stubWFPStore{sources: []domain.SourceLeave{leave("r-1", "w-1", "8.00")}}
	w := &stubWorkers{err: domain.ErrWorkerContextUnavailable}
	rr := wfpDo(newWFPRouter(st, &stubAuthZ{}, w), "POST", wfpBase, importBody(), "p-1", "")
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	assert.Equal(t, "worker_context_unavailable", errCode(t, rr))
	assert.Zero(t, st.importCalls, "a batch is never built on a guess about who may be paid")
}

func TestWFP02_ImportDefaultsToRejectBatchAndReportsOutcomes(t *testing.T) {
	st := &stubWFPStore{sources: []domain.SourceLeave{leave("r-1", "w-1", "8.00")}}
	r := newWFPRouter(st, &stubAuthZ{}, &stubWorkers{})

	// Rejected batch: 422 with the exceptions, never a silent partial success.
	b := liveBatch()
	b.State = domain.BatchRejected
	st.importResult = &store.BatchResult{Batch: b, Created: true, Lines: []domain.TimeLeaveLine{
		{SourceLineID: "r-9", State: domain.LineQuarantined},
	}}
	rr := wfpDo(r, "POST", wfpBase, importBody(), "p-1", "")
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code)
	assert.Equal(t, "BATCH_REJECTED", errCode(t, rr))

	// Replay of an identical population: 200.
	b2 := liveBatch()
	st.importResult = &store.BatchResult{Batch: b2, Created: false}
	assert.Equal(t, http.StatusOK, wfpDo(r, "POST", wfpBase, importBody(), "p-1", "").Code)

	// A different population while one is live: 409 pointing at supersede.
	st.importResult, st.importErr = nil, domain.ErrLiveBatchExists
	rr = wfpDo(r, "POST", wfpBase, importBody(), "p-1", "")
	assert.Equal(t, http.StatusConflict, rr.Code)
	assert.Equal(t, "LIVE_BATCH_EXISTS", errCode(t, rr))

	// Validation.
	bad := importBody()
	bad["period_end"] = "2026-08-01"
	assert.Equal(t, http.StatusBadRequest, wfpDo(r, "POST", wfpBase, bad, "p-1", "").Code)
	bad = importBody()
	bad["on_invalid"] = "IGNORE"
	assert.Equal(t, http.StatusBadRequest, wfpDo(r, "POST", wfpBase, bad, "p-1", "").Code)
}

func TestWFP02_ImportEventsCarryNoLeaveDetail(t *testing.T) {
	st := &stubWFPStore{sources: []domain.SourceLeave{leave("r-1", "w-bad", "8.00")}}
	w := &stubWorkers{results: map[string]workerctx.Result{"w-bad": {Valid: false, Reasons: []string{"WORKER_NOT_FOUND"}}}}
	r := newWFPRouter(st, &stubAuthZ{}, w)
	body := importBody()
	body["on_invalid"] = "QUARANTINE_LINES"
	require.Equal(t, http.StatusCreated, wfpDo(r, "POST", wfpBase, body, "p-1", "").Code)

	var types []string
	for _, ev := range st.built {
		types = append(types, ev.EventType)
		assert.Equal(t, "tenant-abc", ev.TenantID)
		assert.NotContains(t, string(ev.Payload), "VACATION", "no leave type or reason travels on the bus")
	}
	assert.Equal(t, []string{
		domain.EventBatchReceived, domain.EventLineQuarantined, domain.EventBatchAccepted,
	}, types)
}

// ── freeze / supersede / quarantine ──────────────────────────────────────────

func TestWFP02_FreezeRequiresVersionAndReportsSourceChanges(t *testing.T) {
	st := &stubWFPStore{batch: liveBatch()}
	r := newWFPRouter(st, &stubAuthZ{}, &stubWorkers{})

	assert.Equal(t, http.StatusBadRequest, wfpDo(r, "POST", wfpBase+"/b-1/freeze", map[string]any{}, "p-1", "").Code)
	assert.Equal(t, http.StatusOK, wfpDo(r, "POST", wfpBase+"/b-1/freeze", map[string]any{}, "p-1", `"1"`).Code, "If-Match is honoured")
	require.Len(t, st.built, 1)
	assert.Equal(t, domain.EventBatchFrozen, st.built[0].EventType)

	st.freezeErr = &store.SourceChangedError{Issues: []domain.SourceIssue{{SourceLineID: "r-1", Issue: domain.IssueSourceNotApproved}}}
	rr := wfpDo(r, "POST", wfpBase+"/b-1/freeze", map[string]any{"expected_version": 1}, "p-1", "")
	assert.Equal(t, http.StatusConflict, rr.Code)
	assert.Equal(t, "SOURCE_CHANGED", errCode(t, rr))
	assert.Contains(t, rr.Body.String(), domain.IssueSourceNotApproved, "the differences are reported")

	for err, want := range map[error]struct {
		status int
		code   string
	}{
		domain.ErrBatchVersionConflict: {http.StatusConflict, "VERSION_CONFLICT"},
		domain.ErrBatchNotAccepted:     {http.StatusConflict, "INVALID_STATE"},
	} {
		st.freezeErr = err
		rr := wfpDo(r, "POST", wfpBase+"/b-1/freeze", map[string]any{"expected_version": 1}, "p-1", "")
		assert.Equal(t, want.status, rr.Code)
		assert.Equal(t, want.code, errCode(t, rr))
	}
}

func TestWFP02_SupersedeRebuildsFromCurrentSourceUnderTheOldPolicy(t *testing.T) {
	st := &stubWFPStore{batch: liveBatch(), sources: []domain.SourceLeave{leave("r-1", "w-1", "8.00")}}
	w := &stubWorkers{}
	r := newWFPRouter(st, &stubAuthZ{}, w)

	assert.Equal(t, http.StatusBadRequest, wfpDo(r, "POST", wfpBase+"/b-1/supersede", map[string]any{"expected_version": 1}, "p-1", "").Code, "a reason is required")
	assert.Equal(t, http.StatusBadRequest, wfpDo(r, "POST", wfpBase+"/b-1/supersede", map[string]any{"reason": "x"}, "p-1", "").Code, "a version is required")
	assert.Equal(t, http.StatusBadRequest, wfpDo(r, "POST", wfpBase+"/b-1/supersede", map[string]any{"expected_version": 1, "reason": "x", "on_invalid": "IGNORE"}, "p-1", "").Code)

	rr := wfpDo(r, "POST", wfpBase+"/b-1/supersede", map[string]any{"expected_version": 1, "reason": "late leave"}, "p-1", "")
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	assert.Equal(t, 1, st.supersedeCalls)
	assert.Equal(t, domain.OnInvalidRejectBatch, st.supersedePolicy, "inherits the batch's policy")
	assert.Equal(t, "2026-09-30", w.asOf, "the old batch's own period is re-snapshotted")
	require.Len(t, st.supersedeLines, 1)
	var types []string
	for _, ev := range st.built {
		types = append(types, ev.EventType)
	}
	assert.Equal(t, []string{domain.EventBatchSuperseded, domain.EventBatchReceived, domain.EventBatchAccepted}, types)

	rr = wfpDo(r, "POST", wfpBase+"/b-1/supersede", map[string]any{"expected_version": 1, "reason": "x", "on_invalid": "QUARANTINE_LINES"}, "p-1", "")
	require.Equal(t, http.StatusCreated, rr.Code)
	assert.Equal(t, domain.OnInvalidQuarantineLines, st.supersedePolicy, "an explicit policy overrides")

	st.supersedeErr = domain.ErrSupersedeWouldReject
	rr = wfpDo(r, "POST", wfpBase+"/b-1/supersede", map[string]any{"expected_version": 1, "reason": "x"}, "p-1", "")
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code)
	assert.Equal(t, "REPLACEMENT_WOULD_BE_REJECTED", errCode(t, rr))

	st.supersedeErr = domain.ErrBatchNotSupersedable
	assert.Equal(t, http.StatusConflict, wfpDo(r, "POST", wfpBase+"/b-1/supersede", map[string]any{"expected_version": 1, "reason": "x"}, "p-1", "").Code)
}

func TestWFP02_QuarantineLineRequiresReasonAndVersion(t *testing.T) {
	st := &stubWFPStore{batch: liveBatch()}
	r := newWFPRouter(st, &stubAuthZ{}, &stubWorkers{})
	path := wfpBase + "/b-1/lines/l-1/quarantine"

	assert.Equal(t, http.StatusBadRequest, wfpDo(r, "POST", path, map[string]any{"reason": "dup"}, "p-1", "").Code)
	assert.Equal(t, http.StatusBadRequest, wfpDo(r, "POST", path, map[string]any{"expected_version": 1}, "p-1", "").Code)
	assert.Equal(t, http.StatusOK, wfpDo(r, "POST", path, map[string]any{"expected_version": 1, "reason": "dup"}, "p-1", "").Code)
	require.Len(t, st.built, 1)
	assert.Equal(t, domain.EventLineQuarantined, st.built[0].EventType)

	st.quarantineErr = domain.ErrLineNotAccepted
	assert.Equal(t, http.StatusConflict, wfpDo(r, "POST", path, map[string]any{"expected_version": 1, "reason": "dup"}, "p-1", "").Code)
}

// ── queries ──────────────────────────────────────────────────────────────────

func TestWFP02_Queries(t *testing.T) {
	st := &stubWFPStore{batch: liveBatch()}
	r := newWFPRouter(st, &stubAuthZ{}, &stubWorkers{})

	rr := wfpDo(r, "GET", wfpBase+"/b-1/validate", nil, "p-1", "")
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), `"valid":true`)
	st.issues = []domain.SourceIssue{{SourceLineID: "r-1", Issue: domain.IssueSourceChanged}}
	rr = wfpDo(r, "GET", wfpBase+"/b-1/validate", nil, "p-1", "")
	assert.Contains(t, rr.Body.String(), `"valid":false`)

	assert.Equal(t, http.StatusOK, wfpDo(r, "GET", wfpBase+"/b-1/exceptions", nil, "p-1", "").Code)
	assert.Equal(t, http.StatusOK, wfpDo(r, "GET", wfpBase+"/b-1/control-totals", nil, "p-1", "").Code)
	assert.Equal(t, http.StatusOK, wfpDo(r, "GET", wfpBase+"/b-1/workers/w-1", nil, "p-1", "").Code)

	assert.Equal(t, http.StatusBadRequest, wfpDo(r, "GET", wfpBase, nil, "p-1", "").Code, "list needs a legal_entity_id to authorize against")
	assert.Equal(t, http.StatusOK, wfpDo(r, "GET", wfpBase+"?legal_entity_id=le-1", nil, "p-1", "").Code)
}
