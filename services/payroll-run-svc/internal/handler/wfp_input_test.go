package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/handler"
	"zoiko.io/payroll-run-svc/internal/inputpull"
	"zoiko.io/payroll-run-svc/internal/middleware"
	"zoiko.io/payroll-run-svc/internal/store"
)

// ── stubs ────────────────────────────────────────────────────────────────────

type stubInputStore struct {
	mappings map[string]domain.InputCodeMapping
	batch    *domain.InputBatch
	stored   []domain.InputLine
	liveBon  map[string]bool

	mappingResolvedAsOf string
	created             []domain.InputLine
	createCalls         int
	createErr           error
	createResult        *store.InputBatchResult
	built               []domain.OutboxEvent

	freezeCalls, supersedeCalls, cancelCalls, consumeCalls int
	supersedeLines                                         []domain.InputLine
	supersedePolicy                                        string
	err                                                    error
	mappingErr                                             error
	registered                                             []*domain.InputCodeMapping
}

func (s *stubInputStore) RegisterInputCodeMapping(_ context.Context, m *domain.InputCodeMapping, _ store.EventBuilder[domain.InputCodeMapping]) (bool, error) {
	if s.mappingErr != nil {
		return false, s.mappingErr
	}
	s.registered = append(s.registered, m)
	m.MappingVersion = len(s.registered)
	return true, nil
}

func (s *stubInputStore) ListInputCodeMappings(context.Context, string, string) ([]domain.InputCodeMapping, error) {
	return nil, nil
}

func (s *stubInputStore) ResolveInputCodeMappings(_ context.Context, asOf string) (map[string]domain.InputCodeMapping, error) {
	s.mappingResolvedAsOf = asOf
	return s.mappings, nil
}

func (s *stubInputStore) CreateInputBatch(_ context.Context, b *domain.InputBatch, lines []domain.InputLine, mk store.InputEventBuilder) (*store.InputBatchResult, error) {
	s.createCalls++
	s.created = lines
	if s.createErr != nil {
		return nil, s.createErr
	}
	res := s.createResult
	if res == nil {
		b.State, b.Version = domain.InputBatchReady, 2
		res = &store.InputBatchResult{Batch: b, Lines: lines, Created: true}
	}
	if mk != nil {
		evs, err := mk(store.InputChange{Kind: store.InputChangeCreate, Batch: res.Batch, Lines: res.Lines})
		if err != nil {
			return nil, err
		}
		s.built = evs
	}
	return res, nil
}

func (s *stubInputStore) SupersedeInputBatch(_ context.Context, _ string, _ int, nb *domain.InputBatch, lines []domain.InputLine, reason string, mk store.InputEventBuilder) (*store.InputBatchResult, error) {
	s.supersedeCalls++
	s.supersedeLines, s.supersedePolicy = lines, nb.OnInvalid
	if s.err != nil {
		return nil, s.err
	}
	nb.State, nb.Version = domain.InputBatchReady, 2
	if mk != nil {
		evs, err := mk(store.InputChange{Kind: store.InputChangeSupersede, Batch: nb, Lines: lines, Superseded: s.batch, Reason: reason})
		if err != nil {
			return nil, err
		}
		s.built = evs
	}
	return &store.InputBatchResult{Batch: nb, Lines: lines, Created: true}, nil
}

func (s *stubInputStore) transition(kind store.InputChangeKind, mk store.InputEventBuilder, state string) (*domain.InputBatch, error) {
	if s.err != nil {
		return nil, s.err
	}
	b := *s.batch
	b.State = state
	if mk != nil {
		evs, err := mk(store.InputChange{Kind: kind, Batch: &b})
		if err != nil {
			return nil, err
		}
		s.built = evs
	}
	return &b, nil
}

func (s *stubInputStore) FreezeInputBatch(_ context.Context, _ string, _ int, _ string, mk store.InputEventBuilder) (*domain.InputBatch, error) {
	s.freezeCalls++
	return s.transition(store.InputChangeFreeze, mk, domain.InputBatchFrozen)
}

func (s *stubInputStore) CancelInputBatch(_ context.Context, _ string, _ int, _ string, mk store.InputEventBuilder) (*domain.InputBatch, error) {
	s.cancelCalls++
	return s.transition(store.InputChangeCancel, mk, domain.InputBatchCancelled)
}

func (s *stubInputStore) ConsumeInputBatch(_ context.Context, _ string, _ int, _ string, mk store.InputEventBuilder) (*domain.InputBatch, error) {
	s.consumeCalls++
	return s.transition(store.InputChangeConsume, mk, domain.InputBatchConsumed)
}

func (s *stubInputStore) GetInputBatch(context.Context, string) (*domain.InputBatch, error) {
	if s.batch == nil {
		return nil, domain.ErrInputBatchNotFound
	}
	return s.batch, nil
}

func (s *stubInputStore) ListInputBatches(context.Context, string, string, string) ([]domain.InputBatch, error) {
	return nil, nil
}

func (s *stubInputStore) ListInputLines(_ context.Context, _, state, _ string) ([]domain.InputLine, error) {
	var out []domain.InputLine
	for _, l := range s.stored {
		if state == "" || l.State == state {
			out = append(out, l)
		}
	}
	return out, nil
}

func (s *stubInputStore) ListInputControlTotals(context.Context, string) ([]domain.InputControlTotal, error) {
	return []domain.InputControlTotal{{Category: "EARNING", Currency: "GBP", LineCount: 1, TotalAmount: "1.0000"}}, nil
}

func (s *stubInputStore) ListLiveBonusSourceLines(context.Context, string, string) (map[string]bool, error) {
	return s.liveBon, nil
}

type stubPuller struct {
	sources []domain.WorkerSource
	err     error
	calls   int
	last    inputpull.Request
}

func (p *stubPuller) Pull(_ context.Context, r inputpull.Request) ([]domain.WorkerSource, error) {
	p.calls++
	p.last = r
	return p.sources, p.err
}

type stubRuns struct {
	run *domain.PayrollRun
	err error
}

func (r *stubRuns) GetPayrollRun(context.Context, string) (*domain.PayrollRun, error) {
	return r.run, r.err
}

type entityAuthZ struct {
	err      error
	entities []string
	actions  []string
}

func (a *entityAuthZ) CheckAllowed(_ context.Context, _, entity, action string) error {
	a.entities, a.actions = append(a.entities, entity), append(a.actions, action)
	return a.err
}

// ── harness ──────────────────────────────────────────────────────────────────

const inBase = "/v1/payroll/wfp/input-batches"

func newInputRouter(st *stubInputStore, az handler.AuthZClient, p *stubPuller, runs *stubRuns) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc")))
		})
	})
	handler.RegisterWFPInputRoutes(r, handler.NewWFPInput(st, az, p, runs, zap.NewNop()))
	return r
}

type caller2 struct{ principal, entity, ifMatch string }

var inCaller = caller2{principal: "p-1", entity: "le-1"}

func inDo(r chi.Router, method, path string, body any, c caller2) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if c.principal != "" {
		req.Header.Set("X-Principal-Id", c.principal)
	}
	if c.entity != "" {
		req.Header.Set("X-Legal-Entity-Id", c.entity)
	}
	if c.ifMatch != "" {
		req.Header.Set("If-Match", c.ifMatch)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func inCode(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var b map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &b))
	s, _ := b["error_code"].(string)
	return s
}

func testMappings() map[string]domain.InputCodeMapping {
	mk := func(kind, code, payroll, cat, tax string) domain.InputCodeMapping {
		return domain.InputCodeMapping{MappingID: "m-" + kind + code, SourceKind: kind, SourceCode: code, MappingVersion: 1,
			PayrollCode: payroll, Category: cat, TaxTreatment: tax, Active: true}
	}
	out := map[string]domain.InputCodeMapping{}
	for _, m := range []domain.InputCodeMapping{
		mk("BASE_SALARY", "BASE", "P-BASE", "EARNING", "TAXABLE"),
		mk("COMP_BONUS", "ANNUAL", "P-BONUS", "EARNING", "TAXABLE"),
	} {
		out[domain.MappingKey(m.SourceKind, m.SourceCode)] = m
	}
	return out
}

func srcWorker(id string, base float64) domain.WorkerSource {
	by := "mgr-1"
	return domain.WorkerSource{
		WorkerID: id, Context: domain.WorkerContext{Valid: true, PayCurrencies: []string{"GBP"}},
		Base:    &domain.SourceBase{ContractID: "c-1", Amount: base, Currency: "GBP", PayFrequency: "MONTHLY"},
		Bonuses: []domain.SourceBonus{{GrantID: "g-1", BonusType: "ANNUAL", Amount: 500, Currency: "GBP", GrantDate: "2026-09-15", ApprovedBy: &by}},
	}
}

func createBody() map[string]any {
	return map[string]any{"legal_entity_id": "le-1", "payroll_period_id": "2026-09", "period_start": "2026-09-01", "period_end": "2026-09-30"}
}

func readyBatch() *domain.InputBatch {
	return &domain.InputBatch{
		BatchID: "b-1", LegalEntityID: "le-OF-BATCH", PayrollPeriodID: "2026-09", PeriodStart: "2026-09-01", PeriodEnd: "2026-09-30",
		OnInvalid: domain.OnInvalidRejectBatch, State: domain.InputBatchReady, Version: 2,
	}
}

func newStubs() (*stubInputStore, *stubPuller, *stubRuns) {
	w := uuid.NewString()
	return &stubInputStore{mappings: testMappings()}, &stubPuller{sources: []domain.WorkerSource{srcWorker(w, 3000)}}, &stubRuns{}
}

// ── identity & authorization ─────────────────────────────────────────────────

func TestWFP03_IdentityAuthorizationAndForgedTenant(t *testing.T) {
	st, p, runs := newStubs()
	r := newInputRouter(st, &stubAuthZ{}, p, runs)
	assert.Equal(t, http.StatusUnauthorized, inDo(r, "POST", inBase, createBody(), caller2{}).Code)

	denied := newInputRouter(st, &stubAuthZ{err: domain.ErrAuthorizationDenied}, p, runs)
	assert.Equal(t, http.StatusForbidden, inDo(denied, "POST", inBase, createBody(), inCaller).Code)
	down := newInputRouter(st, &stubAuthZ{err: domain.ErrAuthzServiceUnavailable}, p, runs)
	assert.Equal(t, http.StatusServiceUnavailable, inDo(down, "POST", inBase, createBody(), inCaller).Code, "fail closed")
	assert.Zero(t, p.calls, "nothing is pulled for an unauthorized caller")

	b := createBody()
	b["tenant_id"] = "someone-elses"
	rr := inDo(r, "POST", inBase, b, inCaller)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Equal(t, "invalid_json", inCode(t, rr))
}

func TestWFP03_BatchActionsAreAuthorizedAgainstTheBatchsOwnEntity(t *testing.T) {
	st, p, runs := newStubs()
	st.batch = readyBatch()
	az := &entityAuthZ{}
	r := newInputRouter(st, az, p, runs)

	require.Equal(t, http.StatusOK, inDo(r, "GET", inBase+"/b-1", nil, caller2{principal: "p-1"}).Code, "no entity header needed")
	require.Equal(t, http.StatusOK, inDo(r, "GET", inBase+"/b-1/control-totals", nil, caller2{principal: "p-1"}).Code)
	assert.Equal(t, []string{"le-OF-BATCH", "le-OF-BATCH"}, az.entities, "scope comes from the stored batch")
	assert.Equal(t, []string{"WFP_INPUT_VIEW", "WFP_INPUT_VIEW"}, az.actions)

	st.batch = nil
	assert.Equal(t, http.StatusNotFound, inDo(r, "GET", inBase+"/b-1", nil, inCaller).Code)
}

// ── create ───────────────────────────────────────────────────────────────────

func TestWFP03_CreateBuildsFromSourcesUnderTheMappingInForceOnPeriodEnd(t *testing.T) {
	st, p, runs := newStubs()
	st.liveBon = map[string]bool{"BONUS:held-elsewhere": true}
	r := newInputRouter(st, &stubAuthZ{}, p, runs)

	rr := inDo(r, "POST", inBase, createBody(), inCaller)
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())

	assert.Equal(t, "2026-09-30", st.mappingResolvedAsOf, "mappings are resolved as of the period end, not today")
	assert.Equal(t, "tenant-abc", p.last.TenantID, "the authenticated tenant is what is pulled for")
	assert.Equal(t, "p-1", p.last.PrincipalID)
	assert.Equal(t, "le-1", p.last.LegalEntityID)
	assert.Equal(t, "2026-09-01", p.last.PeriodStart)
	assert.Equal(t, st.liveBon, p.last.ExcludeBonuses, "bonuses held by another period are passed to the pull")

	require.Len(t, st.created, 2)
	for _, l := range st.created {
		assert.Equal(t, domain.InputLineAccepted, l.State)
		assert.NotNil(t, l.MappingID)
	}
	var out struct {
		Batch domain.InputBatch `json:"batch"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	assert.Equal(t, domain.OnInvalidRejectBatch, out.Batch.OnInvalid, "the conservative policy is the default")
}

func TestWFP03_CreateOutcomes(t *testing.T) {
	st, p, runs := newStubs()
	r := newInputRouter(st, &stubAuthZ{}, p, runs)

	// Rejected under REJECT_BATCH: 422 with the exceptions.
	b := readyBatch()
	b.State = domain.InputBatchRejected
	st.createResult = &store.InputBatchResult{Batch: b, Created: true, Lines: []domain.InputLine{
		{SourceLineID: "x", State: domain.InputLineQuarantined, Quarantine: strp("UNMAPPED_CODE")},
	}}
	rr := inDo(r, "POST", inBase, createBody(), inCaller)
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code)
	assert.Equal(t, "BATCH_REJECTED", inCode(t, rr))
	assert.Contains(t, rr.Body.String(), "UNMAPPED_CODE")

	// Replay of an identical population: 200.
	st.createResult = &store.InputBatchResult{Batch: readyBatch(), Created: false}
	assert.Equal(t, http.StatusOK, inDo(r, "POST", inBase, createBody(), inCaller).Code)

	// A different population while one is live: 409.
	st.createResult, st.createErr = nil, domain.ErrInputLiveBatchExists
	rr = inDo(r, "POST", inBase, createBody(), inCaller)
	assert.Equal(t, http.StatusConflict, rr.Code)
	assert.Equal(t, "LIVE_BATCH_EXISTS", inCode(t, rr))

	// Validation.
	for name, mutate := range map[string]func(map[string]any){
		"reversed period": func(m map[string]any) { m["period_end"] = "2026-08-01" },
		"bad policy":      func(m map[string]any) { m["on_invalid"] = "IGNORE" },
		"no entity":       func(m map[string]any) { m["legal_entity_id"] = "" },
	} {
		body := createBody()
		mutate(body)
		assert.Equal(t, http.StatusBadRequest, inDo(r, "POST", inBase, body, inCaller).Code, name)
	}
}

func strp(s string) *string { return &s }

func TestWFP03_CreateFailsClosedWhenASourceIsUnavailable(t *testing.T) {
	st, p, runs := newStubs()
	p.err = domain.ErrSourceUnavailable
	r := newInputRouter(st, &stubAuthZ{}, p, runs)
	rr := inDo(r, "POST", inBase, createBody(), inCaller)
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	assert.Equal(t, "input_source_unavailable", inCode(t, rr))
	assert.Zero(t, st.createCalls, "no batch is built on a partial pull")

	p.err = domain.ErrTooManyWorkers
	rr = inDo(r, "POST", inBase, createBody(), inCaller)
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code)
	assert.Equal(t, "ENTITY_TOO_LARGE_FOR_SYNC_BATCH", inCode(t, rr))
}

func TestWFP03_EventsCarryNoAmountsOrPersonalDetail(t *testing.T) {
	st, p, runs := newStubs()
	// One worker with an unmapped component so a quarantine event is produced.
	w := srcWorker(uuid.NewString(), 3000)
	w.Components = []domain.SourceComponent{{StructureID: "s-1", Code: "MYSTERY", Name: "Secret Allowance", Type: "EARNING", IsTaxable: true, Amount: 987.65, Currency: "GBP"}}
	p.sources = []domain.WorkerSource{w}
	r := newInputRouter(st, &stubAuthZ{}, p, runs)

	body := createBody()
	body["on_invalid"] = "QUARANTINE_LINES"
	require.Equal(t, http.StatusCreated, inDo(r, "POST", inBase, body, inCaller).Code)

	var types []string
	for _, ev := range st.built {
		types = append(types, ev.EventType)
		assert.Equal(t, "tenant-abc", ev.TenantID)
		for _, secret := range []string{"987.65", "Secret Allowance", "3000.0000", "500.0000"} {
			assert.NotContains(t, string(ev.Payload), secret, "no amount or label travels on the bus")
		}
	}
	assert.Equal(t, []string{domain.EventInputCreated, domain.EventInputLineQuarantined, domain.EventInputBatchValidated}, types)
}

// ── freeze / validate ────────────────────────────────────────────────────────

// freezeFixture stores exactly what a fresh pull would produce, so there is no drift.
func freezeFixture() (*stubInputStore, *stubPuller, *stubRuns) {
	st, p, runs := newStubs()
	st.batch = readyBatch()
	st.stored = domain.BuildInputLines(p.sources, st.mappings, "2026-09-30")
	return st, p, runs
}

func TestWFP03_FreezeRequiresVersionStateAndAMatchingSource(t *testing.T) {
	st, p, runs := freezeFixture()
	r := newInputRouter(st, &stubAuthZ{}, p, runs)

	assert.Equal(t, http.StatusBadRequest, inDo(r, "POST", inBase+"/b-1/freeze", map[string]any{}, inCaller).Code, "a version is required")

	rr := inDo(r, "POST", inBase+"/b-1/freeze", map[string]any{"expected_version": 2}, inCaller)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, 1, st.freezeCalls)
	require.Len(t, st.built, 1)
	assert.Equal(t, domain.EventPayrollInputBatchFrozen, st.built[0].EventType)
	assert.Equal(t, "le-OF-BATCH", p.last.LegalEntityID, "the re-pull is for the batch's own entity")

	// If-Match is honoured.
	viaHeader := inCaller
	viaHeader.ifMatch = `"2"`
	assert.Equal(t, http.StatusOK, inDo(r, "POST", inBase+"/b-1/freeze", map[string]any{}, viaHeader).Code)

	// Source drift: the population no longer matches; freeze is refused, nothing changes.
	st2, p2, runs2 := freezeFixture()
	p2.sources[0].Base.Amount = 3100
	r2 := newInputRouter(st2, &stubAuthZ{}, p2, runs2)
	rr = inDo(r2, "POST", inBase+"/b-1/freeze", map[string]any{"expected_version": 2}, inCaller)
	assert.Equal(t, http.StatusConflict, rr.Code)
	assert.Equal(t, "SOURCE_CHANGED", inCode(t, rr))
	assert.Contains(t, rr.Body.String(), domain.InputIssueSourceChanged)
	assert.Zero(t, st2.freezeCalls)

	// Not READY: refused without pulling anything.
	st3, p3, runs3 := freezeFixture()
	st3.batch.State = domain.InputBatchFrozen
	r3 := newInputRouter(st3, &stubAuthZ{}, p3, runs3)
	rr = inDo(r3, "POST", inBase+"/b-1/freeze", map[string]any{"expected_version": 2}, inCaller)
	assert.Equal(t, http.StatusConflict, rr.Code)
	assert.Equal(t, "INVALID_STATE", inCode(t, rr))
	assert.Zero(t, p3.calls)

	// A source outage is not "no drift".
	st4, p4, runs4 := freezeFixture()
	p4.err = domain.ErrSourceUnavailable
	rr = inDo(newInputRouter(st4, &stubAuthZ{}, p4, runs4), "POST", inBase+"/b-1/freeze", map[string]any{"expected_version": 2}, inCaller)
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	assert.Zero(t, st4.freezeCalls)

	// A stale version surfaces as 409 VERSION_CONFLICT.
	st5, p5, runs5 := freezeFixture()
	st5.err = domain.ErrInputBatchVersion
	rr = inDo(newInputRouter(st5, &stubAuthZ{}, p5, runs5), "POST", inBase+"/b-1/freeze", map[string]any{"expected_version": 1}, inCaller)
	assert.Equal(t, "VERSION_CONFLICT", inCode(t, rr))
}

func TestWFP03_ValidateReportsDriftAndChangesNothing(t *testing.T) {
	st, p, runs := freezeFixture()
	r := newInputRouter(st, &stubAuthZ{}, p, runs)
	rr := inDo(r, "GET", inBase+"/b-1/validate", nil, inCaller)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), `"valid":true`)

	p.sources[0].Bonuses = nil // a bonus disappeared at the source
	rr = inDo(r, "GET", inBase+"/b-1/validate", nil, inCaller)
	assert.Contains(t, rr.Body.String(), `"valid":false`)
	assert.Contains(t, rr.Body.String(), domain.InputIssueSourceMissing)
	assert.Zero(t, st.freezeCalls+st.supersedeCalls+st.cancelCalls+st.consumeCalls)
}

// ── supersede / cancel / consume ─────────────────────────────────────────────

func TestWFP03_SupersedeRebuildsUnderTheOldPolicy(t *testing.T) {
	st, p, runs := freezeFixture()
	r := newInputRouter(st, &stubAuthZ{}, p, runs)

	assert.Equal(t, http.StatusBadRequest, inDo(r, "POST", inBase+"/b-1/supersede", map[string]any{"expected_version": 2}, inCaller).Code, "reason required")
	assert.Equal(t, http.StatusBadRequest, inDo(r, "POST", inBase+"/b-1/supersede", map[string]any{"reason": "x"}, inCaller).Code, "version required")
	assert.Equal(t, http.StatusBadRequest, inDo(r, "POST", inBase+"/b-1/supersede", map[string]any{"expected_version": 2, "reason": "x", "on_invalid": "IGNORE"}, inCaller).Code)

	p.sources[0].Base.Amount = 3300
	rr := inDo(r, "POST", inBase+"/b-1/supersede", map[string]any{"expected_version": 2, "reason": "salary revised"}, inCaller)
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	assert.Equal(t, domain.OnInvalidRejectBatch, st.supersedePolicy)
	var base domain.InputLine
	for _, l := range st.supersedeLines {
		if l.SourceKind == domain.SourceBaseSalary {
			base = l
		}
	}
	assert.Equal(t, "3300.0000", base.Amount, "rebuilt from the CURRENT source")
	var types []string
	for _, ev := range st.built {
		types = append(types, ev.EventType)
	}
	assert.Equal(t, []string{domain.EventInputSuperseded, domain.EventInputCreated, domain.EventInputBatchValidated}, types)

	rr = inDo(r, "POST", inBase+"/b-1/supersede", map[string]any{"expected_version": 2, "reason": "x", "on_invalid": "QUARANTINE_LINES"}, inCaller)
	require.Equal(t, http.StatusCreated, rr.Code)
	assert.Equal(t, domain.OnInvalidQuarantineLines, st.supersedePolicy)

	st.err = domain.ErrInputSupersedeWouldReject
	rr = inDo(r, "POST", inBase+"/b-1/supersede", map[string]any{"expected_version": 2, "reason": "x"}, inCaller)
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code)
	assert.Equal(t, "REPLACEMENT_WOULD_BE_REJECTED", inCode(t, rr))

	// A consumed batch is not superseded here; no pull is wasted finding that out.
	st2, p2, runs2 := freezeFixture()
	st2.batch.State = domain.InputBatchConsumed
	rr = inDo(newInputRouter(st2, &stubAuthZ{}, p2, runs2), "POST", inBase+"/b-1/supersede", map[string]any{"expected_version": 2, "reason": "x"}, inCaller)
	assert.Equal(t, http.StatusConflict, rr.Code)
	assert.Zero(t, p2.calls)
	assert.Zero(t, st2.supersedeCalls)
}

func TestWFP03_CancelRequiresAReason(t *testing.T) {
	st, p, runs := freezeFixture()
	r := newInputRouter(st, &stubAuthZ{}, p, runs)
	assert.Equal(t, http.StatusBadRequest, inDo(r, "POST", inBase+"/b-1/cancel", map[string]any{"expected_version": 2}, inCaller).Code)
	assert.Equal(t, http.StatusOK, inDo(r, "POST", inBase+"/b-1/cancel", map[string]any{"expected_version": 2, "reason": "wrong period"}, inCaller).Code)
	require.Len(t, st.built, 1)
	assert.Equal(t, domain.EventInputBatchCancelled, st.built[0].EventType)
}

func TestWFP03_ConsumeTiesABatchToTheRunForTheSameEntityAndPeriod(t *testing.T) {
	st, p, runs := freezeFixture()
	st.batch.State = domain.InputBatchFrozen
	run := uuid.NewString()
	r := newInputRouter(st, &stubAuthZ{}, p, runs)
	body := func() map[string]any { return map[string]any{"expected_version": 2, "payroll_run_id": run} }

	assert.Equal(t, http.StatusBadRequest, inDo(r, "POST", inBase+"/b-1/consume", map[string]any{"expected_version": 2}, inCaller).Code, "a run id is required")
	assert.Equal(t, http.StatusBadRequest, inDo(r, "POST", inBase+"/b-1/consume", map[string]any{"expected_version": 2, "payroll_run_id": "nope"}, inCaller).Code)

	runs.err = domain.ErrPayrollRunNotFound
	rr := inDo(r, "POST", inBase+"/b-1/consume", body(), inCaller)
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code)
	assert.Equal(t, "RUN_NOT_FOUND", inCode(t, rr))
	runs.err = nil

	for name, run := range map[string]*domain.PayrollRun{
		"other entity": {LegalEntityID: "le-OTHER", PayPeriodStart: "2026-09-01", PayPeriodEnd: "2026-09-30"},
		"other period": {LegalEntityID: "le-OF-BATCH", PayPeriodStart: "2026-10-01", PayPeriodEnd: "2026-10-31"},
	} {
		runs.run = run
		rr := inDo(r, "POST", inBase+"/b-1/consume", body(), inCaller)
		assert.Equal(t, http.StatusUnprocessableEntity, rr.Code, name)
		assert.Equal(t, "RUN_PERIOD_MISMATCH", inCode(t, rr), name)
	}
	assert.Zero(t, st.consumeCalls)

	runs.run = &domain.PayrollRun{LegalEntityID: "le-OF-BATCH", PayPeriodStart: "2026-09-01", PayPeriodEnd: "2026-09-30"}
	require.Equal(t, http.StatusOK, inDo(r, "POST", inBase+"/b-1/consume", body(), inCaller).Code)
	assert.Equal(t, 1, st.consumeCalls)
	assert.Equal(t, domain.EventInputBatchConsumed, st.built[0].EventType)

	st.err = domain.ErrRunAlreadyLinked
	rr = inDo(r, "POST", inBase+"/b-1/consume", body(), inCaller)
	assert.Equal(t, http.StatusConflict, rr.Code)
	assert.Equal(t, "RUN_ALREADY_LINKED", inCode(t, rr))
}

// ── queries ──────────────────────────────────────────────────────────────────

func TestWFP03_Queries(t *testing.T) {
	st, p, runs := freezeFixture()
	r := newInputRouter(st, &stubAuthZ{}, p, runs)

	rr := inDo(r, "GET", inBase+"/b-1/control-totals", nil, inCaller)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), `"totals"`)
	assert.Equal(t, http.StatusOK, inDo(r, "GET", inBase+"/b-1/exceptions", nil, inCaller).Code)

	w := uuid.NewString()
	assert.Equal(t, http.StatusOK, inDo(r, "GET", inBase+"/b-1/workers/"+w, nil, inCaller).Code)
	assert.Equal(t, http.StatusBadRequest, inDo(r, "GET", inBase+"/b-1/workers/not-a-uuid", nil, inCaller).Code)

	assert.Equal(t, http.StatusBadRequest, inDo(r, "GET", inBase, nil, inCaller).Code, "list needs a legal_entity_id to authorize against")
	assert.Equal(t, http.StatusOK, inDo(r, "GET", inBase+"?legal_entity_id=le-1", nil, inCaller).Code)
}

// ── mappings ─────────────────────────────────────────────────────────────────

const mapBase = "/v1/payroll/wfp/input-code-mappings"

func mapBody() map[string]any {
	return map[string]any{
		"source_kind": "COMP_COMPONENT", "source_code": "HRA", "payroll_code": "P-HRA", "category": "EARNING",
		"tax_treatment": "TAXABLE", "effective_from": "2026-01-01",
	}
}

func TestWFP03_MappingRegistration(t *testing.T) {
	st, p, runs := newStubs()
	r := newInputRouter(st, &stubAuthZ{}, p, runs)

	assert.Equal(t, http.StatusUnauthorized, inDo(r, "POST", mapBase, mapBody(), caller2{entity: "le-1"}).Code)
	assert.Equal(t, http.StatusBadRequest, inDo(r, "POST", mapBase, mapBody(), caller2{principal: "p-1"}).Code, "a scope entity is required")

	rr := inDo(r, "POST", mapBase, mapBody(), inCaller)
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	require.Len(t, st.registered, 1)
	assert.True(t, st.registered[0].Active, "active by default")
	assert.Equal(t, "p-1", st.registered[0].RegisteredBy)

	off := mapBody()
	off["active"] = false
	require.Equal(t, http.StatusCreated, inDo(r, "POST", mapBase, off, inCaller).Code)
	assert.False(t, st.registered[1].Active)

	st.mappingErr = domain.ErrMappingRegression
	rr = inDo(r, "POST", mapBase, mapBody(), inCaller)
	assert.Equal(t, http.StatusConflict, rr.Code)
	assert.Equal(t, "MAPPING_EFFECTIVE_DATE_REGRESSION", inCode(t, rr))

	bad := mapBody()
	bad["tax_treatment"] = "MAYBE"
	assert.Equal(t, http.StatusBadRequest, inDo(r, "POST", mapBase, bad, inCaller).Code)
	bad = mapBody()
	bad["source_kind"] = "PAYSLIP"
	assert.Equal(t, http.StatusBadRequest, inDo(r, "POST", mapBase, bad, inCaller).Code)
}

func TestWFP03_MappingResolveRequiresADate(t *testing.T) {
	st, p, runs := newStubs()
	r := newInputRouter(st, &stubAuthZ{}, p, runs)

	assert.Equal(t, http.StatusBadRequest, inDo(r, "GET", mapBase+"/resolve?source_kind=BASE_SALARY&source_code=BASE", nil, inCaller).Code, "no 'current' by omission")
	rr := inDo(r, "GET", mapBase+"/resolve?source_kind=BASE_SALARY&source_code=BASE&as_of=2026-09-30", nil, inCaller)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.True(t, strings.Contains(rr.Body.String(), "P-BASE"))
	assert.Equal(t, http.StatusNotFound, inDo(r, "GET", mapBase+"/resolve?source_kind=BASE_SALARY&source_code=NOPE&as_of=2026-09-30", nil, inCaller).Code)
	assert.Equal(t, http.StatusOK, inDo(r, "GET", mapBase, nil, inCaller).Code)
}
