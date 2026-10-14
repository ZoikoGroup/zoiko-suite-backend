package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/handler"
	"zoiko.io/payroll-run-svc/internal/middleware"
	"zoiko.io/payroll-run-svc/internal/store"
	"zoiko.io/payroll-run-svc/internal/workerctx"
)

// ── stubs ────────────────────────────────────────────────────────────────────

type stubResultStore struct {
	profiles map[string]*domain.SourceTrustProfile
	manifest *domain.PayrollRuleManifest
	batches  map[string]*domain.InputBatch
	runs     map[string]*domain.PayrollRun
	slips    map[string][]domain.PaySlip
	ref      *domain.PayrollRunRef
	result   *domain.PayrollResult

	received    []store.ReceiveResultInput
	receiveErr  error
	forced      *store.ReceiveResultOutput
	builtEvents []domain.OutboxEvent

	latestVersion  int
	correctionSeen map[string]int // payrollRunID -> version already registered
	diffs          []domain.ResultReason
	workers        []domain.PayrollResultWorker
	err            error
}

func newResultStore() *stubResultStore {
	active := &domain.SourceTrustProfile{SourceSystemID: "ext", Status: domain.TrustActive, AllowedCapabilities: []string{domain.CapPayrollResult}}
	return &stubResultStore{
		profiles: map[string]*domain.SourceTrustProfile{"ext": active, "zoiko-payroll": {SourceSystemID: "zoiko-payroll", Status: domain.TrustActive, AllowedCapabilities: []string{domain.CapPayrollResult}}},
		manifest: &domain.PayrollRuleManifest{ManifestID: "11111111-1111-1111-1111-111111111111", SourceSystemID: "ext", EffectiveFrom: "2026-01-01"},
		batches:  map[string]*domain.InputBatch{}, runs: map[string]*domain.PayrollRun{}, slips: map[string][]domain.PaySlip{},
		correctionSeen: map[string]int{},
	}
}

func (s *stubResultStore) GetSourceTrustProfileBySource(_ context.Context, src string) (*domain.SourceTrustProfile, error) {
	if p, ok := s.profiles[src]; ok {
		return p, nil
	}
	return nil, domain.ErrTrustProfileNotFound
}

func (s *stubResultStore) GetRuleManifest(context.Context, string) (*domain.PayrollRuleManifest, error) {
	if s.manifest == nil {
		return nil, domain.ErrManifestNotFound
	}
	return s.manifest, nil
}

func (s *stubResultStore) GetInputBatch(_ context.Context, id string) (*domain.InputBatch, error) {
	if b, ok := s.batches[id]; ok {
		return b, nil
	}
	return nil, domain.ErrInputBatchNotFound
}

func (s *stubResultStore) GetPayrollRun(_ context.Context, id string) (*domain.PayrollRun, error) {
	if r, ok := s.runs[id]; ok {
		return r, nil
	}
	return nil, domain.ErrPayrollRunNotFound
}

func (s *stubResultStore) GetPaySlipsByRun(_ context.Context, id string) ([]domain.PaySlip, error) {
	return s.slips[id], nil
}

func (s *stubResultStore) ReceivePayrollResult(_ context.Context, in store.ReceiveResultInput, mk store.ResultEventBuilder) (*store.ReceiveResultOutput, error) {
	s.received = append(s.received, in)
	if s.receiveErr != nil {
		return nil, s.receiveErr
	}
	if s.forced != nil {
		return s.forced, nil
	}
	ref := in.Ref
	res := &domain.PayrollResult{
		ResultID: uuid.NewString(), RunRefID: ref.RunRefID, Outcome: domain.ResultAccepted, Currency: in.Request.Currency,
		WorkerCount: in.Evaluation.Totals.WorkerCount, ResultFingerprint: in.Evaluation.Fingerprint, EvidenceFlags: in.Request.EvidenceFlags,
		RejectionReasons: in.Evaluation.Reasons,
	}
	ref.AcceptanceState = domain.AcceptanceAccepted
	if !in.Evaluation.Accepted() {
		res.Outcome, ref.AcceptanceState = domain.ResultRejected, domain.AcceptanceRejected
	}
	var superseded *domain.PayrollRunRef
	if in.Ref.SourceRunVersion > 1 {
		superseded = &domain.PayrollRunRef{RunRefID: "prev", LegalEntityID: ref.LegalEntityID, SourceSystemID: ref.SourceSystemID, SourceRunID: ref.SourceRunID, SourceRunVersion: ref.SourceRunVersion - 1}
	}
	if mk != nil {
		evs, err := mk(store.ResultChange{Ref: &ref, Result: res, Registered: true, Superseded: superseded})
		if err != nil {
			return nil, err
		}
		s.builtEvents = evs
	}
	return &store.ReceiveResultOutput{Ref: &ref, Result: res, Created: true}, nil
}

func (s *stubResultStore) GetPayrollRunRef(context.Context, string) (*domain.PayrollRunRef, error) {
	if s.ref == nil {
		return nil, domain.ErrRunRefNotFound
	}
	return s.ref, nil
}

func (s *stubResultStore) GetPayrollResultByRef(context.Context, string) (*domain.PayrollResult, error) {
	if s.result == nil {
		return nil, domain.ErrResultNotFound
	}
	return s.result, nil
}

func (s *stubResultStore) ListResultWorkers(context.Context, string) ([]domain.PayrollResultWorker, error) {
	return s.workers, nil
}

func (s *stubResultStore) ListResultComponents(context.Context, string, string) ([]domain.PayrollResultComponent, error) {
	return []domain.PayrollResultComponent{{Code: "BASE", Kind: "EARNING", Amount: "1.0000"}}, nil
}

func (s *stubResultStore) CorrectionChains(context.Context, string) ([]domain.PayrollCorrectionChain, []domain.PayrollCorrectionChain, error) {
	return []domain.PayrollCorrectionChain{{Reason: "later fix"}}, nil, nil
}

func (s *stubResultStore) LatestRunRefVersion(context.Context, string, string, string) (int, error) {
	return s.latestVersion, nil
}

func (s *stubResultStore) RunRefVersionForPayrollRun(_ context.Context, _, _, _, runID string) (int, bool, error) {
	v, ok := s.correctionSeen[runID]
	return v, ok, nil
}

func (s *stubResultStore) RecomputeResultTotals(context.Context, string) ([]domain.ResultReason, error) {
	return s.diffs, s.err
}

type stubResultWorkers struct {
	mu     sync.Mutex
	calls  []string
	asOf   string
	caller workerctx.Caller
	byID   map[string]domain.WorkerContext
	err    error
}

func (w *stubResultWorkers) Validate(_ context.Context, c workerctx.Caller, id, _, asOf string) (domain.WorkerContext, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, id)
	w.asOf, w.caller = asOf, c
	if w.err != nil {
		return domain.WorkerContext{}, w.err
	}
	if c, ok := w.byID[id]; ok {
		return c, nil
	}
	return domain.WorkerContext{Valid: true, PayCurrencies: []string{"GBP"}}, nil
}

// ── harness ──────────────────────────────────────────────────────────────────

const resBase = "/v1/payroll/wfp/results"

func newResultRouter(st *stubResultStore, az handler.AuthZClient, w *stubResultWorkers) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(rw, req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc")))
		})
	})
	handler.RegisterWFPResultRoutes(r, handler.NewWFPResult(st, az, w, zap.NewNop()))
	return r
}

func resDo(r chi.Router, method, path string, body any, principal string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func resCode(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var b map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &b))
	s, _ := b["error_code"].(string)
	return s
}

func submitBody(worker string) map[string]any {
	return map[string]any{
		"legal_entity_id": "le-1", "source_system_id": "ext", "source_run_id": "RUN-1", "source_run_version": 1, "run_kind": "REGULAR",
		"payroll_period_id": "2026-09", "pay_date": "2026-09-30", "rule_manifest_id": "11111111-1111-1111-1111-111111111111",
		"source_run_status": "FINAL", "source_approval_ref": "approval-1", "calculation_ref": "engine-1", "currency": "GBP",
		"workers": []map[string]any{{
			"worker_id": worker, "currency": "GBP", "gross": "1000", "taxable_pay": "1000", "employee_deductions": "200",
			"employer_liabilities": "50", "net_pay": "800",
			"components": []map[string]any{
				{"code": "BASE", "kind": "EARNING", "amount": "1000"}, {"code": "TAX", "kind": "EMPLOYEE_TAX", "amount": "200"},
				{"code": "NI", "kind": "EMPLOYER_SOCIAL", "amount": "50"},
			},
		}},
		"run_totals": map[string]any{"worker_count": 1, "gross": "1000", "taxable_pay": "1000", "employee_deductions": "200", "employer_liabilities": "50", "net_pay": "800"},
	}
}

// ── identity & authorization ─────────────────────────────────────────────────

func TestWFP04_IdentityAuthorizationAndForgedTenant(t *testing.T) {
	st, w := newResultStore(), &stubResultWorkers{}
	wk := uuid.NewString()
	r := newResultRouter(st, &stubAuthZ{}, w)
	assert.Equal(t, http.StatusUnauthorized, resDo(r, "POST", resBase, submitBody(wk), "").Code)

	denied := newResultRouter(st, &stubAuthZ{err: domain.ErrAuthorizationDenied}, w)
	assert.Equal(t, http.StatusForbidden, resDo(denied, "POST", resBase, submitBody(wk), "p-1").Code)
	down := newResultRouter(st, &stubAuthZ{err: domain.ErrAuthzServiceUnavailable}, w)
	assert.Equal(t, http.StatusServiceUnavailable, resDo(down, "POST", resBase, submitBody(wk), "p-1").Code, "fail closed")

	b := submitBody(wk)
	b["tenant_id"] = "someone-elses"
	rr := resDo(r, "POST", resBase, b, "p-1")
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Equal(t, "invalid_json", resCode(t, rr))
	assert.Empty(t, st.received)
	assert.Empty(t, w.calls, "no worker lookups for a refused request")
}

func TestWFP04_ReadsAreAuthorizedAgainstTheRunReferencesOwnEntity(t *testing.T) {
	st, w := newResultStore(), &stubResultWorkers{}
	st.ref = &domain.PayrollRunRef{RunRefID: "r-1", LegalEntityID: "le-OF-REF", AcceptanceState: domain.AcceptanceAccepted}
	st.result = &domain.PayrollResult{ResultID: "x", Outcome: domain.ResultAccepted}
	az := &entityAuthZ{}
	r := newResultRouter(st, az, w)

	require.Equal(t, http.StatusOK, resDo(r, "GET", resBase+"/r-1", nil, "p-1").Code)
	require.Equal(t, http.StatusOK, resDo(r, "GET", resBase+"/r-1/control-totals", nil, "p-1").Code)
	assert.Equal(t, []string{"le-OF-REF", "le-OF-REF"}, az.entities)
	assert.Equal(t, []string{"WFP_RESULT_VIEW", "WFP_RESULT_VIEW"}, az.actions)

	st.ref = nil
	assert.Equal(t, http.StatusNotFound, resDo(r, "GET", resBase+"/r-1", nil, "p-1").Code)
}

// ── gates ────────────────────────────────────────────────────────────────────

func TestWFP04_GatesRefuseWithoutStoringAnything(t *testing.T) {
	wk := uuid.NewString()
	cases := map[string]struct {
		setup  func(*stubResultStore)
		status int
		code   string
	}{
		"no trust profile":          {func(s *stubResultStore) { delete(s.profiles, "ext") }, http.StatusUnprocessableEntity, "SOURCE_NOT_TRUSTED"},
		"suspended":                 {func(s *stubResultStore) { s.profiles["ext"].Status = domain.TrustSuspended }, http.StatusUnprocessableEntity, "SOURCE_NOT_TRUSTED"},
		"wrong capability":          {func(s *stubResultStore) { s.profiles["ext"].AllowedCapabilities = []string{domain.CapTimeLeave} }, http.StatusUnprocessableEntity, "SOURCE_NOT_TRUSTED"},
		"no manifest":               {func(s *stubResultStore) { s.manifest = nil }, http.StatusNotFound, "not_found"},
		"other source's manifest":   {func(s *stubResultStore) { s.manifest.SourceSystemID = "someone-else" }, http.StatusUnprocessableEntity, "RULE_MANIFEST_SOURCE_MISMATCH"},
		"future-effective manifest": {func(s *stubResultStore) { s.manifest.EffectiveFrom = "2026-10-01" }, http.StatusUnprocessableEntity, "RULE_MANIFEST_NOT_EFFECTIVE"},
		"expired manifest":          {func(s *stubResultStore) { to := "2026-09-30"; s.manifest.EffectiveTo = &to }, http.StatusUnprocessableEntity, "RULE_MANIFEST_NOT_EFFECTIVE"},
	}
	for name, c := range cases {
		st, w := newResultStore(), &stubResultWorkers{}
		c.setup(st)
		rr := resDo(newResultRouter(st, &stubAuthZ{}, w), "POST", resBase, submitBody(wk), "p-1")
		assert.Equal(t, c.status, rr.Code, name)
		assert.Equal(t, c.code, resCode(t, rr), name)
		assert.Empty(t, st.received, name)
		assert.Empty(t, w.calls, "%s: no worker lookups before the gates pass", name)
	}
}

func TestWFP04_UnreachableWorkerServiceFailsClosed(t *testing.T) {
	st, w := newResultStore(), &stubResultWorkers{err: domain.ErrSourceUnavailable}
	rr := resDo(newResultRouter(st, &stubAuthZ{}, w), "POST", resBase, submitBody(uuid.NewString()), "p-1")
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	assert.Equal(t, "worker_context_unavailable", resCode(t, rr))
	assert.Empty(t, st.received, "an unverifiable worker is never assumed valid")
}

func TestWFP04_StructuralErrorsAre400AndStoreNothing(t *testing.T) {
	st, w := newResultStore(), &stubResultWorkers{}
	r := newResultRouter(st, &stubAuthZ{}, w)
	wk := uuid.NewString()
	for name, mutate := range map[string]func(map[string]any){
		"bad currency":   func(b map[string]any) { b["currency"] = "pounds" },
		"bad status":     func(b map[string]any) { b["source_run_status"] = "DONE" },
		"float amount":   func(b map[string]any) { b["workers"].([]map[string]any)[0]["gross"] = "1000.12345" },
		"missing worker": func(b map[string]any) { b["workers"].([]map[string]any)[0]["worker_id"] = "x" },
		"bad kind": func(b map[string]any) {
			b["workers"].([]map[string]any)[0]["components"].([]map[string]any)[0]["kind"] = "BONUS"
		},
	} {
		b := submitBody(wk)
		mutate(b)
		assert.Equal(t, http.StatusBadRequest, resDo(r, "POST", resBase, b, "p-1").Code, name)
	}
	assert.Empty(t, st.received)
}

// ── outcomes ─────────────────────────────────────────────────────────────────

func TestWFP04_AcceptedResultIs201AndCarriesTheAuthenticatedIdentity(t *testing.T) {
	st, w := newResultStore(), &stubResultWorkers{}
	wk := uuid.NewString()
	r := newResultRouter(st, &stubAuthZ{}, w)

	rr := resDo(r, "POST", resBase, submitBody(wk), "p-1")
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	require.Len(t, st.received, 1)
	in := st.received[0]
	assert.True(t, in.Evaluation.Accepted(), "%v", in.Evaluation.Reasons)
	assert.Equal(t, "p-1", in.ReceivedBy)
	assert.Equal(t, domain.RunIdentityFingerprint("tenant-abc", in.Request.RunRef()), in.Ref.IdentityFingerprint,
		"the run identity is bound to the AUTHENTICATED tenant")
	assert.Equal(t, domain.ResultFingerprint("tenant-abc", in.Request), in.Evaluation.Fingerprint)

	assert.Equal(t, []string{wk}, w.calls, "one worker-context call per distinct worker")
	assert.Equal(t, "2026-09-30", w.asOf, "workers are validated as of the pay date")
	assert.Equal(t, "tenant-abc", w.caller.TenantID)
	assert.Equal(t, "p-1", w.caller.PrincipalID)
	assert.Equal(t, "le-1", w.caller.LegalEntityID)
}

func TestWFP04_FailedControlIs422WithTheReasonsAndStillStoresTheEvidence(t *testing.T) {
	st, w := newResultStore(), &stubResultWorkers{}
	wk := uuid.NewString()
	b := submitBody(wk)
	b["workers"].([]map[string]any)[0]["net_pay"] = "799.99"
	b["run_totals"].(map[string]any)["net_pay"] = "799.99"

	rr := resDo(newResultRouter(st, &stubAuthZ{}, w), "POST", resBase, b, "p-1")
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code)
	assert.Equal(t, "RESULT_REJECTED", resCode(t, rr))
	assert.Contains(t, rr.Body.String(), domain.RsNetPayMismatch)
	require.Len(t, st.received, 1, "a rejected result is stored as evidence, not discarded")
	assert.False(t, st.received[0].Evaluation.Accepted())
}

func TestWFP04_UnknownOrMismatchedWorkersAreRejectedByTheControls(t *testing.T) {
	st := newResultStore()
	wk := uuid.NewString()
	w := &stubResultWorkers{byID: map[string]domain.WorkerContext{wk: {Valid: false, Reason: "CROSS_ENTITY_WORKER"}}}
	rr := resDo(newResultRouter(st, &stubAuthZ{}, w), "POST", resBase, submitBody(wk), "p-1")
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code)
	assert.Contains(t, rr.Body.String(), "CROSS_ENTITY_WORKER", "no posting or payment for a worker mapped to the wrong entity")

	st2 := newResultStore()
	w2 := &stubResultWorkers{byID: map[string]domain.WorkerContext{wk: {Valid: true, PayCurrencies: []string{"EUR"}}}}
	rr = resDo(newResultRouter(st2, &stubAuthZ{}, w2), "POST", resBase, submitBody(wk), "p-1")
	assert.Contains(t, rr.Body.String(), domain.RsCurrencyMismatch)
}

func TestWFP04_InputBatchMustBeFrozenAndMatch(t *testing.T) {
	wk := uuid.NewString()
	batchID := uuid.NewString()
	for name, c := range map[string]struct {
		batch *domain.InputBatch
		want  string
	}{
		"unknown":      {nil, "NOT_FOUND"},
		"other entity": {&domain.InputBatch{LegalEntityID: "le-2", PayrollPeriodID: "2026-09", State: domain.InputBatchFrozen}, "ENTITY_MISMATCH"},
		"other period": {&domain.InputBatch{LegalEntityID: "le-1", PayrollPeriodID: "2026-08", State: domain.InputBatchFrozen}, "PERIOD_MISMATCH"},
		"not frozen":   {&domain.InputBatch{LegalEntityID: "le-1", PayrollPeriodID: "2026-09", State: domain.InputBatchReady}, "NOT_FROZEN:READY"},
		"frozen":       {&domain.InputBatch{LegalEntityID: "le-1", PayrollPeriodID: "2026-09", State: domain.InputBatchFrozen}, ""},
		"consumed":     {&domain.InputBatch{LegalEntityID: "le-1", PayrollPeriodID: "2026-09", State: domain.InputBatchConsumed}, ""},
	} {
		st := newResultStore()
		if c.batch != nil {
			st.batches[batchID] = c.batch
		}
		b := submitBody(wk)
		b["input_batch_id"] = batchID
		rr := resDo(newResultRouter(st, &stubAuthZ{}, &stubResultWorkers{}), "POST", resBase, b, "p-1")
		if c.want == "" {
			assert.Equal(t, http.StatusCreated, rr.Code, name)
			continue
		}
		assert.Equal(t, http.StatusUnprocessableEntity, rr.Code, name)
		assert.Contains(t, rr.Body.String(), domain.RsInputBatchInvalid, name)
		assert.Contains(t, rr.Body.String(), c.want, name)
	}
}

func TestWFP04_StoreOutcomesMapToStatuses(t *testing.T) {
	wk := uuid.NewString()
	for name, c := range map[string]struct {
		err    error
		status int
		code   string
	}{
		"fingerprint conflict": {domain.ErrResultFingerprintConflict, http.StatusConflict, "RESULT_FINGERPRINT_CONFLICT"},
		"identity conflict":    {domain.ErrRunIdentityConflict, http.StatusConflict, "RUN_IDENTITY_CONFLICT"},
		"prior version":        {domain.ErrPriorVersionMissing, http.StatusConflict, "PRIOR_VERSION_MISSING"},
	} {
		st := newResultStore()
		st.receiveErr = c.err
		rr := resDo(newResultRouter(st, &stubAuthZ{}, &stubResultWorkers{}), "POST", resBase, submitBody(wk), "p-1")
		assert.Equal(t, c.status, rr.Code, name)
		assert.Equal(t, c.code, resCode(t, rr), name)
	}

	// A replay (the identical result already on record) is 200, not a second 201.
	st := newResultStore()
	ref := &domain.PayrollRunRef{RunRefID: "r", AcceptanceState: domain.AcceptanceAccepted}
	st.forced = &store.ReceiveResultOutput{Ref: ref, Result: &domain.PayrollResult{Outcome: domain.ResultAccepted}, Created: false}
	assert.Equal(t, http.StatusOK, resDo(newResultRouter(st, &stubAuthZ{}, &stubResultWorkers{}), "POST", resBase, submitBody(wk), "p-1").Code)
}

func TestWFP04_EventsCarryNoAmountsOrWorkerDetail(t *testing.T) {
	st, w := newResultStore(), &stubResultWorkers{}
	wk := uuid.NewString()
	b := submitBody(wk)
	b["source_run_version"] = 2 // the stub then reports a superseded previous version
	b["run_kind"] = "CORRECTION"
	require.Equal(t, http.StatusCreated, resDo(newResultRouter(st, &stubAuthZ{}, w), "POST", resBase, b, "p-1").Code)

	var types []string
	for _, ev := range st.builtEvents {
		types = append(types, ev.EventType)
		assert.Equal(t, "tenant-abc", ev.TenantID)
		assert.Equal(t, "ext|RUN-1", ev.ObjectID, "keyed by source run so every version stays in order")
		for _, secret := range []string{"1000.0000", "800.0000", "gross", "net_pay", wk} {
			assert.NotContains(t, string(ev.Payload), secret, "no amount or worker id travels on the bus")
		}
	}
	assert.Equal(t, []string{domain.EventPayrollResultReceived, domain.EventPayrollResultAccepted, domain.EventPayrollResultSuperseded}, types)
}

// ── Zoiko path ───────────────────────────────────────────────────────────────

func zoikoFixture() (*stubResultStore, *domain.PayrollRun) {
	st := newResultStore()
	st.manifest.SourceSystemID = "zoiko-payroll"
	hash := "snap"
	run := &domain.PayrollRun{
		RunID: uuid.NewString(), LegalEntityID: "le-1", Status: "COMPLETED", PayDate: "2026-09-30", EmployeeCount: 1,
		TotalGrossPay: 1000, TotalNetPay: 800, TotalTaxDeductions: 200, SnapshotHash: &hash,
	}
	st.runs[run.RunID] = run
	st.slips[run.RunID] = []domain.PaySlip{{EmployeeID: uuid.NewString(), GrossPay: 1000, TaxWithheld: 200, NetPay: 800, TaxableAmount: 1000, Currency: "GBP"}}
	return st, run
}

func fromRunBody(run *domain.PayrollRun) map[string]any {
	return map[string]any{"payroll_run_id": run.RunID, "payroll_period_id": "2026-09", "rule_manifest_id": "11111111-1111-1111-1111-111111111111"}
}

func TestWFP04_FromRunPromotesACompletedRun(t *testing.T) {
	st, run := zoikoFixture()
	az := &entityAuthZ{}
	rr := resDo(newResultRouter(st, az, &stubResultWorkers{}), "POST", resBase+"/from-run", fromRunBody(run), "p-1")
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())

	require.Len(t, st.received, 1)
	req := st.received[0].Request
	assert.Equal(t, domain.SourceZoikoPayroll, req.SourceSystemID)
	assert.Equal(t, run.RunID, req.SourceRunID)
	assert.Equal(t, 1, req.SourceRunVersion)
	assert.Equal(t, "REGULAR", req.RunKind)
	assert.Equal(t, "finalized:snap", req.SourceApprovalRef)
	assert.Contains(t, req.EvidenceFlags, domain.FlagPlaceholderTaxRate)
	assert.True(t, st.received[0].Evaluation.Accepted(), "%v", st.received[0].Evaluation.Reasons)
	assert.Equal(t, []string{"le-1"}, az.entities, "authorized against the RUN's legal entity")
	assert.Equal(t, []string{"WFP_RESULT_SUBMIT"}, az.actions)
	assert.Equal(t, run.RunID, *st.received[0].Ref.PayrollRunID, "the platform run is linked to its result")
}

func TestWFP04_FromRunRefusesWhatIsNotAPayrollResult(t *testing.T) {
	st, run := zoikoFixture()
	r := newResultRouter(st, &stubAuthZ{}, &stubResultWorkers{})

	run.Status = "CALCULATED"
	rr := resDo(r, "POST", resBase+"/from-run", fromRunBody(run), "p-1")
	assert.Equal(t, http.StatusConflict, rr.Code)
	assert.Equal(t, "RUN_NOT_FINAL", resCode(t, rr))

	run.Status, run.IsShadowRun = "COMPLETED", true
	rr = resDo(r, "POST", resBase+"/from-run", fromRunBody(run), "p-1")
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code)
	assert.Equal(t, "SHADOW_RUN_NOT_PAYABLE", resCode(t, rr))

	run.IsShadowRun = false
	b := fromRunBody(run)
	b["payroll_run_id"] = uuid.NewString()
	rr = resDo(r, "POST", resBase+"/from-run", b, "p-1")
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code)
	assert.Equal(t, "RUN_NOT_FOUND", resCode(t, rr))

	assert.Empty(t, st.received)
	for name, mutate := range map[string]func(map[string]any){
		"run id":      func(b map[string]any) { b["payroll_run_id"] = "x" },
		"manifest id": func(b map[string]any) { b["rule_manifest_id"] = "x" },
		"period":      func(b map[string]any) { b["payroll_period_id"] = " " },
		"corrects id": func(b map[string]any) { b["corrects_payroll_run_id"] = "x" },
		"no reason":   func(b map[string]any) { b["corrects_payroll_run_id"] = uuid.NewString() },
	} {
		b := fromRunBody(run)
		mutate(b)
		assert.Equal(t, http.StatusBadRequest, resDo(r, "POST", resBase+"/from-run", b, "p-1").Code, name)
	}
}

func TestWFP04_FromRunStillNeedsTrustAndAManifestForZoikoPayroll(t *testing.T) {
	st, run := zoikoFixture()
	delete(st.profiles, "zoiko-payroll")
	rr := resDo(newResultRouter(st, &stubAuthZ{}, &stubResultWorkers{}), "POST", resBase+"/from-run", fromRunBody(run), "p-1")
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code)
	assert.Equal(t, "SOURCE_NOT_TRUSTED", resCode(t, rr), "Zoiko Payroll is a source like any other (WFP-D-01)")

	st2, run2 := zoikoFixture()
	st2.manifest = nil
	rr = resDo(newResultRouter(st2, &stubAuthZ{}, &stubResultWorkers{}), "POST", resBase+"/from-run", fromRunBody(run2), "p-1")
	assert.Equal(t, http.StatusNotFound, rr.Code, "no manifest, no Zoiko result")
	assert.Empty(t, st2.received)
}

func TestWFP04_ACorrectionBecomesTheNextVersionOfTheOriginalRunAndIsReplaySafe(t *testing.T) {
	st, corr := zoikoFixture()
	orig := &domain.PayrollRun{RunID: uuid.NewString(), LegalEntityID: "le-1", Status: "COMPLETED"}
	st.runs[orig.RunID] = orig
	st.latestVersion = 1
	r := newResultRouter(st, &stubAuthZ{}, &stubResultWorkers{})
	b := fromRunBody(corr)
	b["corrects_payroll_run_id"] = orig.RunID
	b["reason"] = "retro increase"

	require.Equal(t, http.StatusCreated, resDo(r, "POST", resBase+"/from-run", b, "p-1").Code)
	req := st.received[0].Request
	assert.Equal(t, orig.RunID, req.SourceRunID, "the correction is a version of the ORIGINAL source run")
	assert.Equal(t, 2, req.SourceRunVersion)
	assert.Equal(t, "CORRECTION", req.RunKind)
	assert.Equal(t, corr.RunID, *req.PayrollRunID)
	assert.Equal(t, "retro increase", st.received[0].SupersedeReason)

	// Promoting the same correction run again reuses the version it already has.
	st.correctionSeen[corr.RunID] = 2
	st.latestVersion = 2
	require.Equal(t, http.StatusCreated, resDo(r, "POST", resBase+"/from-run", b, "p-1").Code)
	assert.Equal(t, 2, st.received[1].Request.SourceRunVersion, "a retried correction is a replay, not version 3")

	// Nothing registered for the original: nothing to correct.
	st2, corr2 := zoikoFixture()
	orig2 := &domain.PayrollRun{RunID: uuid.NewString(), LegalEntityID: "le-1", Status: "COMPLETED"}
	st2.runs[orig2.RunID] = orig2
	b2 := fromRunBody(corr2)
	b2["corrects_payroll_run_id"], b2["reason"] = orig2.RunID, "x"
	rr := resDo(newResultRouter(st2, &stubAuthZ{}, &stubResultWorkers{}), "POST", resBase+"/from-run", b2, "p-1")
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code)
	assert.Equal(t, "CORRECTION_BASE_MISSING", resCode(t, rr))

	// Corrections cannot cross legal entities.
	st3, corr3 := zoikoFixture()
	other := &domain.PayrollRun{RunID: uuid.NewString(), LegalEntityID: "le-OTHER", Status: "COMPLETED"}
	st3.runs[other.RunID] = other
	b3 := fromRunBody(corr3)
	b3["corrects_payroll_run_id"], b3["reason"] = other.RunID, "x"
	rr = resDo(newResultRouter(st3, &stubAuthZ{}, &stubResultWorkers{}), "POST", resBase+"/from-run", b3, "p-1")
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code)
	assert.Equal(t, "CORRECTION_ENTITY_MISMATCH", resCode(t, rr))
}

// ── queries ──────────────────────────────────────────────────────────────────

func TestWFP04_Queries(t *testing.T) {
	st, w := newResultStore(), &stubResultWorkers{}
	wk := uuid.NewString()
	st.ref = &domain.PayrollRunRef{RunRefID: "r-1", LegalEntityID: "le-1", RuleManifestID: "m-1", AcceptanceState: domain.AcceptanceAccepted}
	batch := uuid.NewString()
	st.result = &domain.PayrollResult{ResultID: "x", Outcome: domain.ResultAccepted, Currency: "GBP", TotalGross: "1000.0000", InputBatchID: &batch,
		DeclaredTotals: domain.ResultTotalsInput{Gross: "1000.0000"}}
	st.workers = []domain.PayrollResultWorker{{WorkerID: wk, Gross: "1000.0000", NetPay: "800.0000"}}
	r := newResultRouter(st, &stubAuthZ{}, w)

	rr := resDo(r, "GET", resBase+"/r-1/control-totals", nil, "p-1")
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), `"computed"`)
	assert.Contains(t, rr.Body.String(), `"declared"`)

	assert.Equal(t, http.StatusOK, resDo(r, "GET", resBase+"/r-1/workers", nil, "p-1").Code)
	rr = resDo(r, "GET", resBase+"/r-1/workers/"+wk, nil, "p-1")
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), `"components"`)
	assert.Equal(t, http.StatusNotFound, resDo(r, "GET", resBase+"/r-1/workers/"+uuid.NewString(), nil, "p-1").Code)
	assert.Equal(t, http.StatusBadRequest, resDo(r, "GET", resBase+"/r-1/workers/nope", nil, "p-1").Code)
	assert.Equal(t, http.StatusOK, resDo(r, "GET", resBase+"/r-1/components", nil, "p-1").Code)
	assert.Equal(t, http.StatusBadRequest, resDo(r, "GET", resBase+"/r-1/components?worker_id=nope", nil, "p-1").Code)

	rr = resDo(r, "GET", resBase+"/r-1/revalidate-totals", nil, "p-1")
	assert.Contains(t, rr.Body.String(), `"valid":true`)
	st.diffs = []domain.ResultReason{{Code: domain.RsRunTotalMismatch}}
	rr = resDo(r, "GET", resBase+"/r-1/revalidate-totals", nil, "p-1")
	assert.Contains(t, rr.Body.String(), `"valid":false`)

	rr = resDo(r, "GET", resBase+"/r-1/evidence", nil, "p-1")
	require.Equal(t, http.StatusOK, rr.Code)
	for _, part := range []string{`"rule_manifest"`, `"result"`, `"correction_chain"`, `"later fix"`} {
		assert.Contains(t, rr.Body.String(), part)
	}
}
