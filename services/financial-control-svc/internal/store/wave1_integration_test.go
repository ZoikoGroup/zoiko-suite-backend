//go:build integration

package store_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/financial-control-svc/internal/domain"
	"zoiko.io/financial-control-svc/internal/engine"
	"zoiko.io/financial-control-svc/internal/source"
	"zoiko.io/financial-control-svc/internal/store"
)

// fakeSources serves the population contract for two systems ("ar", "gl").
type fakeSources struct {
	noPeriod map[string]bool // "system/population" -> the real endpoint rejects period_id as an unknown param
	queries  map[string]url.Values
	mu       sync.Mutex
	data     map[string][]domain.PopulationRecord // "ar/invoices" -> records
	wm       map[string]string
	down     map[string]bool
	requests []http.Header
}

func newFakeSources() *fakeSources {
	return &fakeSources{data: map[string][]domain.PopulationRecord{}, wm: map[string]string{}, down: map[string]bool{}, queries: map[string]url.Values{}, noPeriod: map[string]bool{}}
}

func (f *fakeSources) set(system, pop, watermark string, recs ...domain.PopulationRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[system+"/"+pop] = recs
	f.wm[system+"/"+pop] = watermark
}

func (f *fakeSources) server(system string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, r.Header.Clone())
		f.queries[system] = r.URL.Query()
		pop := r.URL.Path[len("/v1/control-populations/"):]
		key := system + "/" + pop
		if f.noPeriod[key] && r.URL.Query().Has("period_id") {
			http.Error(w, "unknown query parameter period_id", http.StatusBadRequest)
			return
		}
		if f.down[system] {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var recs []domain.PopulationRecord
		ok := false
		for _, cand := range []string{key + "?side=" + r.URL.Query().Get("side"), key + "?measure=" + r.URL.Query().Get("measure"), key + "?leg=" + r.URL.Query().Get("leg"), key} {
			if v, found := f.data[cand]; found {
				recs, ok = v, true
				key = cand
				break
			}
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		off, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		end := off + limit
		resp := map[string]any{"watermark": f.wm[key]}
		if end < len(recs) {
			resp["next_cursor"] = strconv.Itoa(end)
		} else {
			end = len(recs)
		}
		resp["records"] = recs[off:end]
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func r(id, ref, amt string) domain.PopulationRecord {
	return domain.PopulationRecord{RecordID: id, Reference: ref, Amount: amt, Currency: "USD", Date: "2026-09-01"}
}

type e2e struct {
	t       *testing.T
	tenant  string
	entity  string
	src     *fakeSources
	exec    *engine.Executor
	def     *domain.ControlDefinition
	cleanup []func()
}

func newE2E(t *testing.T) *e2e {
	t.Helper()
	x := &e2e{t: t, tenant: uuid.NewString(), entity: uuid.NewString(), src: newFakeSources()}
	ar, gl := x.src.server("ar"), x.src.server("gl")
	t.Cleanup(ar.Close)
	t.Cleanup(gl.Close)
	x.exec = engine.New(testStore, source.NewHTTPFetcher(map[string]string{"ar": ar.URL, "gl": gl.URL}, nil), zap.NewNop())

	req := newDef("FIN-CTRL-001")
	req.SourceSpec = json.RawMessage(`{"system":"ar","population":"invoices"}`)
	req.TargetSpec = json.RawMessage(`{"system":"gl","population":"ar-postings"}`)
	d, err := testStore.CreateDefinition(ctx, x.tenant, "maker", "c", req)
	require.NoError(t, err)
	_, err = testStore.ApproveRuleVersion(ctx, x.tenant, d.ControlDefinitionID, 1, "checker")
	require.NoError(t, err)
	x.def = d
	return x
}

// start creates a run and executes it through the real pipeline.
func (x *e2e) start(key string) (*domain.ControlRun, *domain.ControlRun) {
	x.t.Helper()
	run, _, err := testStore.CreateRun(ctx, x.tenant, "prep", "corr", "create-"+key, runReq(x.def.ControlDefinitionID, x.entity))
	require.NoError(x.t, err)
	return run, x.execute(run, key)
}

func (x *e2e) execute(run *domain.ControlRun, key string) *domain.ControlRun {
	x.t.Helper()
	started, replay, err := testStore.BeginExecution(ctx, x.tenant, run.RunID, key, "prep", "corr", store.SkipVersionCheck)
	require.NoError(x.t, err)
	require.False(x.t, replay)
	require.Equal(x.t, domain.LifecyclePreparing, started.LifecycleState)
	final, err := x.exec.Execute(ctx, engine.Input{TenantID: x.tenant, RunID: run.RunID, Actor: "prep", CorrelationID: "corr"})
	require.NoError(x.t, err)
	return final
}

func (x *e2e) events(runID string) map[string]int {
	rows, err := testPool.Query(ctx, `SELECT event_type, count(*) FROM outbox_events
		WHERE tenant_id = $1 AND (aggregate_id = $2::text OR aggregate_id IN (SELECT exception_id::text FROM control_exceptions WHERE run_id::text = $2::text))
		GROUP BY event_type`, x.tenant, runID)
	require.NoError(x.t, err)
	defer rows.Close()
	m := map[string]int{}
	for rows.Next() {
		var et string
		var n int
		require.NoError(x.t, rows.Scan(&et, &n))
		m[et] = n
	}
	return m
}

func TestE2E_CleanRunPassesWithFrozenPopulationAndVerifiedEvidence(t *testing.T) {
	x := newE2E(t)
	x.src.set("ar", "invoices", "ar-wm-1", r("a1", "INV-1", "100.00"), r("a2", "INV-2", "50.50"))
	x.src.set("gl", "ar-postings", "gl-wm-1", r("g1", "INV-1", "100"), r("g2", "INV-2", "50.5"))

	_, final := x.start("k1")
	assert.Equal(t, domain.LifecycleReadyToCertify, final.LifecycleState)
	assert.Equal(t, domain.ResultPass, final.ResultState)
	assert.Equal(t, domain.CertPending, final.CertificationState, "key control still needs a certifier")
	assert.NotNil(t, final.StartedAt)

	snaps, err := testStore.ListPopulationSnapshots(ctx, x.tenant, final.RunID)
	require.NoError(t, err)
	require.Len(t, snaps, 2)
	assert.Equal(t, domain.SideA, snaps[0].Side)
	assert.Equal(t, "ar-wm-1", snaps[0].Watermark)
	assert.Equal(t, "gl-wm-1", snaps[1].Watermark)
	assert.Equal(t, 2, snaps[0].RowCount)
	assert.Equal(t, "150.5", snaps[0].Totals[0].Total)
	want, _ := domain.HashPopulation([]domain.PopulationRecord{r("a1", "INV-1", "100.00"), r("a2", "INV-2", "50.50")})
	assert.Equal(t, want, snaps[0].PopulationHash, "the stored hash is reproducible from the source records")

	recs, err := testStore.ListPopulationRecords(ctx, x.tenant, final.RunID, domain.SideA, 0, 1)
	require.NoError(t, err)
	require.Len(t, recs, 1, "keyset paging")
	more, err := testStore.ListPopulationRecords(ctx, x.tenant, final.RunID, domain.SideA, recs[0].Seq, 10)
	require.NoError(t, err)
	require.Len(t, more, 1)
	assert.NotEqual(t, recs[0].Record.RecordID, more[0].Record.RecordID)

	matches, err := testStore.ListMatchResults(ctx, x.tenant, final.RunID)
	require.NoError(t, err)
	assert.Len(t, matches, 2)

	pkg, err := testStore.GetLatestEvidence(ctx, x.tenant, final.RunID)
	require.NoError(t, err)
	require.NotNil(t, pkg.Verified)
	assert.True(t, *pkg.Verified, "sealed evidence verifies after a round trip through JSONB")
	var c domain.EvidenceContent
	require.NoError(t, json.Unmarshal(pkg.Content, &c))
	assert.Equal(t, snaps[0].PopulationHash, c.Populations[0].PopulationHash)
	assert.Equal(t, x.def.ControlCode, c.Definition.ControlCode)
	assert.Equal(t, 1, c.Definition.RuleVersion)

	ev := x.events(final.RunID)
	for _, want := range []string{"control.run.created", "control.population.frozen", "control.execution.completed", "control.run.ready_for_certification"} {
		assert.Equal(t, 1, ev[want], want)
	}
	assert.Zero(t, ev["control.exception.opened"])

	// The source received the VERIFIED tenant/principal so it applies its own isolation.
	require.NotEmpty(t, x.src.requests)
	assert.Equal(t, x.tenant, x.src.requests[0].Get("X-Tenant-Id"))
	assert.Equal(t, "prep", x.src.requests[0].Get("X-Principal-Id"))
}

func TestE2E_MismatchCreatesOwnedDatedExceptionsAndAssignmentWorkflow(t *testing.T) {
	x := newE2E(t)
	x.src.set("ar", "invoices", "wm", r("a1", "INV-1", "100"), r("a2", "INV-2", "40"), r("a3", "INV-3", "7"))
	x.src.set("gl", "ar-postings", "wm", r("g1", "INV-1", "99"), r("g2", "INV-2", "40"))

	run, final := x.start("k1")
	assert.Equal(t, domain.ResultFail, final.ResultState)
	assert.Equal(t, domain.LifecycleExceptionReview, final.LifecycleState)

	list, err := testStore.ListExceptions(ctx, x.tenant, run.RunID, store.ListExceptionsFilter{})
	require.NoError(t, err)
	require.Len(t, list, 2, "one amount mismatch (INV-1) and one missing downstream (INV-3)")
	byReason := map[string]domain.ControlException{}
	for _, e := range list {
		byReason[e.ReasonCode] = e
		assert.Equal(t, "AR_CONTROLLER", e.OwnerRole, "Invariant 7: never ownerless")
		assert.Nil(t, e.OwnerPrincipal)
		assert.Equal(t, domain.ExOpen, e.State)
		assert.True(t, e.DueAt.After(e.CreatedAt), "always has a due date")
		assert.Equal(t, x.entity, e.LegalEntityID)
	}
	assert.Equal(t, "1", byReason[domain.ReasonAmountMismatch].Exposure)
	assert.Equal(t, "7", byReason[domain.ReasonMissingDownstream].Exposure)
	assert.Equal(t, 2, x.events(run.RunID)["control.exception.opened"])

	// Filters.
	open, err := testStore.ListExceptions(ctx, x.tenant, run.RunID, store.ListExceptionsFilter{State: "OPEN", Limit: 1})
	require.NoError(t, err)
	assert.Len(t, open, 1)
	none, err := testStore.ListExceptions(ctx, x.tenant, run.RunID, store.ListExceptionsFilter{State: "CLOSED"})
	require.NoError(t, err)
	assert.Empty(t, none)

	// Assign: stale ETag refused, SLA cannot be extended, tightening allowed, history appended.
	ex := byReason[domain.ReasonMissingDownstream]
	req := domain.AssignExceptionRequest{OwnerPrincipalID: "bob", Reason: "AR lead"}
	_, err = testStore.AssignException(ctx, x.tenant, ex.ExceptionID, "alice", "c", 99, req)
	assert.ErrorIs(t, err, domain.ErrConflict)

	late := req
	late.DueDate = "2099-01-01"
	_, err = testStore.AssignException(ctx, x.tenant, ex.ExceptionID, "alice", "c", ex.Version, late)
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "an SLA extension is a governed waiver, not an assignment side effect")

	tight := req
	tight.DueDate = ex.DueAt.AddDate(0, 0, -2).Format("2006-01-02")
	got, err := testStore.AssignException(ctx, x.tenant, ex.ExceptionID, "alice", "c", ex.Version, tight)
	require.NoError(t, err)
	assert.Equal(t, domain.ExAssigned, got.State)
	require.NotNil(t, got.OwnerPrincipal)
	assert.Equal(t, "bob", *got.OwnerPrincipal)
	assert.Equal(t, ex.Version+1, got.Version)
	assert.True(t, got.DueAt.Before(ex.DueAt))

	re, err := testStore.AssignException(ctx, x.tenant, ex.ExceptionID, "alice", "c", got.Version,
		domain.AssignExceptionRequest{OwnerPrincipalID: "carol", Reason: "cover"})
	require.NoError(t, err, "reassignment is allowed (ASSIGNED -> ASSIGNED)")
	assert.Equal(t, "carol", *re.OwnerPrincipal)

	var trans int
	require.NoError(t, testPool.QueryRow(ctx, `SELECT count(*) FROM exception_transitions WHERE exception_id = $1`, ex.ExceptionID).Scan(&trans))
	assert.Equal(t, 3, trans, "opened + assigned + reassigned")
	assert.Equal(t, 2, x.events(run.RunID)["control.exception.assigned"])

	// The finding on an exception can never be edited; it can never be deleted to make a control pass.
	_, err = testPool.Exec(ctx, `UPDATE control_exceptions SET exposure = 0 WHERE exception_id = $1`, ex.ExceptionID)
	assert.Error(t, err, "exposure is part of the immutable finding")
	_, err = testPool.Exec(ctx, `UPDATE control_exceptions SET reason_code = 'OK' WHERE exception_id = $1`, ex.ExceptionID)
	assert.Error(t, err)
	_, err = testPool.Exec(ctx, `DELETE FROM control_exceptions WHERE exception_id = $1`, ex.ExceptionID)
	assert.Error(t, err, "manual deletion of an exception is prohibited")
	// And past OPEN an exception must have a named owner.
	_, err = testPool.Exec(ctx, `UPDATE control_exceptions SET state = 'INVESTIGATING', owner_principal_id = NULL WHERE exception_id = $1`, byReason[domain.ReasonAmountMismatch].ExceptionID)
	assert.Error(t, err)
}

// Scenario 09: a source outage is Indeterminate/Failed, never Pass, and leaves nothing half-frozen.
func TestE2E_SourceOutageFailsRunIndeterminate(t *testing.T) {
	x := newE2E(t)
	x.src.set("ar", "invoices", "wm", r("a1", "INV-1", "1"))
	x.src.set("gl", "ar-postings", "wm", r("g1", "INV-1", "1"))
	x.src.down["gl"] = true

	run, final := x.start("k1")
	assert.Equal(t, domain.LifecycleFailed, final.LifecycleState)
	assert.Equal(t, domain.ResultIndeterminate, final.ResultState)
	assert.NotNil(t, final.CompletedAt)

	snaps, err := testStore.ListPopulationSnapshots(ctx, x.tenant, run.RunID)
	require.NoError(t, err)
	assert.Empty(t, snaps, "a partial fetch freezes nothing")
	_, err = testStore.GetLatestEvidence(ctx, x.tenant, run.RunID)
	assert.ErrorIs(t, err, domain.ErrNotFound, "a failed control seals no result evidence")
	assert.Equal(t, 1, x.events(run.RunID)["control.run.failed"])

	// The reason is durable in the transition history.
	trs, err := testStore.ListTransitions(ctx, x.tenant, run.RunID)
	require.NoError(t, err)
	last := trs[len(trs)-1]
	assert.Contains(t, last.Reason, "side B population could not be built")
	// A failed run is terminal.
	_, err = testStore.AdvanceRun(ctx, x.tenant, run.RunID, store.AdvanceParams{ExpectedVersion: store.SkipVersionCheck, ToLifecycle: domain.LifecycleExecuting, Actor: "x", CorrelationID: "c"})
	assert.ErrorIs(t, err, domain.ErrInvalidTransition)
}

func TestE2E_UnwatermarkedSourceFailsRunEvenIfDataLooksFine(t *testing.T) {
	x := newE2E(t)
	x.src.set("ar", "invoices", "", r("a1", "INV-1", "1")) // no watermark => not reproducible
	x.src.set("gl", "ar-postings", "wm", r("g1", "INV-1", "1"))
	_, final := x.start("k1")
	assert.Equal(t, domain.ResultIndeterminate, final.ResultState)
}

func TestE2E_ExecuteIsIdempotentAndExactlyOnce(t *testing.T) {
	x := newE2E(t)
	x.src.set("ar", "invoices", "wm", r("a1", "INV-1", "1"))
	x.src.set("gl", "ar-postings", "wm", r("g1", "INV-1", "1"))
	run, final := x.start("exec-key")
	require.Equal(t, domain.ResultPass, final.ResultState)

	// Replay with the same key: no second execution, current state returned.
	again, replay, err := testStore.BeginExecution(ctx, x.tenant, run.RunID, "exec-key", "prep", "c", store.SkipVersionCheck)
	require.NoError(t, err)
	assert.True(t, replay)
	assert.Equal(t, domain.LifecycleReadyToCertify, again.LifecycleState)

	// A NEW key against a run that already executed is an invalid transition, not a second run.
	_, _, err = testStore.BeginExecution(ctx, x.tenant, run.RunID, "other-key", "prep", "c", store.SkipVersionCheck)
	assert.ErrorIs(t, err, domain.ErrInvalidTransition)

	// The same key for a different run is a conflict.
	other, _, err := testStore.CreateRun(ctx, x.tenant, "prep", "c", "create-other", runReq(x.def.ControlDefinitionID, x.entity))
	require.NoError(t, err)
	_, _, err = testStore.BeginExecution(ctx, x.tenant, other.RunID, "exec-key", "prep", "c", store.SkipVersionCheck)
	assert.ErrorIs(t, err, domain.ErrConflict)

	var frozen, evidence int
	require.NoError(t, testPool.QueryRow(ctx, `SELECT count(*) FROM population_snapshots WHERE run_id = $1`, run.RunID).Scan(&frozen))
	require.NoError(t, testPool.QueryRow(ctx, `SELECT count(*) FROM evidence_packages WHERE run_id = $1`, run.RunID).Scan(&evidence))
	assert.Equal(t, 2, frozen)
	assert.Equal(t, 1, evidence)
}

func TestE2E_BeginExecutionHonoursExpectedVersion(t *testing.T) {
	x := newE2E(t)
	run, _, err := testStore.CreateRun(ctx, x.tenant, "prep", "c", "k", runReq(x.def.ControlDefinitionID, x.entity))
	require.NoError(t, err)
	_, _, err = testStore.BeginExecution(ctx, x.tenant, run.RunID, "e", "prep", "c", run.Version+5)
	assert.ErrorIs(t, err, domain.ErrConflict, "stale ETag refused")
	var used int
	require.NoError(t, testPool.QueryRow(ctx, `SELECT count(*) FROM command_idempotency WHERE run_id = $1`, run.RunID).Scan(&used))
	assert.Zero(t, used, "a refused start does not consume the idempotency key")
}

// Scenario 05 (population half): frozen populations cannot be silently mutated,
// and later data produces a NEW run with a different fingerprint.
func TestE2E_FrozenPopulationIsImmutable_LateDataMakesANewRun(t *testing.T) {
	x := newE2E(t)
	x.src.set("ar", "invoices", "wm-1", r("a1", "INV-1", "10"))
	x.src.set("gl", "ar-postings", "wm-1", r("g1", "INV-1", "10"))
	run1, _ := x.start("k1")
	snaps1, err := testStore.ListPopulationSnapshots(ctx, x.tenant, run1.RunID)
	require.NoError(t, err)

	for name, q := range map[string]string{
		"update record":   `UPDATE population_records SET amount = 999 WHERE population_id = $1`,
		"delete record":   `DELETE FROM population_records WHERE population_id = $1`,
		"update snapshot": `UPDATE population_snapshots SET row_count = 0 WHERE population_id = $1`,
		"delete snapshot": `DELETE FROM population_snapshots WHERE population_id = $1`,
		"rewrite hash":    `UPDATE population_snapshots SET population_hash = 'sha256:` + fmt.Sprintf("%064d", 0) + `' WHERE population_id = $1`,
		"delete match":    `DELETE FROM match_results WHERE run_id = (SELECT run_id FROM population_snapshots WHERE population_id = $1)`,
	} {
		_, err := testPool.Exec(ctx, q, snaps1[0].PopulationID)
		assert.Error(t, err, name)
	}

	// A late invoice arrives at the source. The certified/frozen run is untouched; a new run sees it.
	x.src.set("ar", "invoices", "wm-2", r("a1", "INV-1", "10"), r("a9", "INV-9", "5"))
	run2, final2 := x.start("k2")
	assert.Equal(t, domain.ResultFail, final2.ResultState)
	snaps2, err := testStore.ListPopulationSnapshots(ctx, x.tenant, run2.RunID)
	require.NoError(t, err)
	assert.NotEqual(t, snaps1[0].PopulationHash, snaps2[0].PopulationHash)
	assert.Equal(t, "wm-2", snaps2[0].Watermark)
	reread, err := testStore.ListPopulationSnapshots(ctx, x.tenant, run1.RunID)
	require.NoError(t, err)
	assert.Equal(t, snaps1[0].PopulationHash, reread[0].PopulationHash, "run 1's frozen population never changed")
	assert.Equal(t, "wm-1", reread[0].Watermark)
}

func TestE2E_DuplicateRecordIDDeliveryIsStoredAndFlagged(t *testing.T) {
	x := newE2E(t)
	x.src.set("ar", "invoices", "wm", r("a1", "INV-1", "10"), r("a1", "INV-1", "10")) // same id twice
	x.src.set("gl", "ar-postings", "wm", r("g1", "INV-1", "10"))
	run, final := x.start("k1")
	assert.Equal(t, domain.ResultFail, final.ResultState)
	snaps, err := testStore.ListPopulationSnapshots(ctx, x.tenant, run.RunID)
	require.NoError(t, err)
	assert.Equal(t, 2, snaps[0].RowCount, "both deliveries are recorded so the duplicate is provable")
	list, err := testStore.ListExceptions(ctx, x.tenant, run.RunID, store.ListExceptionsFilter{})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, domain.ReasonDuplicateID, list[0].ReasonCode)
}

func TestE2E_EvidenceTamperingIsDetectedOnRead(t *testing.T) {
	x := newE2E(t)
	x.src.set("ar", "invoices", "wm", r("a1", "INV-1", "10"))
	x.src.set("gl", "ar-postings", "wm", r("g1", "INV-1", "10"))
	run, _ := x.start("k1")

	// The application path cannot edit sealed evidence at all.
	_, err := testPool.Exec(ctx, `UPDATE evidence_packages SET content = '{"forged":true}' WHERE run_id = $1`, run.RunID)
	assert.Error(t, err)
	_, err = testPool.Exec(ctx, `DELETE FROM evidence_packages WHERE run_id = $1`, run.RunID)
	assert.Error(t, err)

	// A privileged actor who bypasses the trigger still cannot hide it: verification recomputes the digest.
	_, err = testPool.Exec(ctx, `ALTER TABLE evidence_packages DISABLE TRIGGER trg_immutable_evidence`)
	require.NoError(t, err)
	defer func() {
		_, _ = testPool.Exec(ctx, `ALTER TABLE evidence_packages ENABLE TRIGGER trg_immutable_evidence`)
	}()
	_, err = testPool.Exec(ctx, `UPDATE evidence_packages SET content = jsonb_set(content, '{execution_evidence,matched_count}', '99') WHERE run_id = $1`, run.RunID)
	require.NoError(t, err)

	pkg, err := testStore.GetLatestEvidence(ctx, x.tenant, run.RunID)
	require.NoError(t, err)
	require.NotNil(t, pkg.Verified)
	assert.False(t, *pkg.Verified, "tampered evidence is reported as failing integrity")
}

func TestE2E_TenantIsolationOnWave1Data(t *testing.T) {
	x := newE2E(t)
	x.src.set("ar", "invoices", "wm", r("a1", "INV-1", "10"), r("a2", "INV-2", "5"))
	x.src.set("gl", "ar-postings", "wm", r("g1", "INV-1", "10"))
	run, _ := x.start("k1")
	list, err := testStore.ListExceptions(ctx, x.tenant, run.RunID, store.ListExceptionsFilter{})
	require.NoError(t, err)
	require.Len(t, list, 1)

	other := uuid.NewString() // scenario 28: a different tenant asks for this run's data
	snaps, err := testStore.ListPopulationSnapshots(ctx, other, run.RunID)
	require.NoError(t, err)
	assert.Empty(t, snaps)
	recs, err := testStore.ListPopulationRecords(ctx, other, run.RunID, domain.SideA, 0, 100)
	require.NoError(t, err)
	assert.Empty(t, recs)
	exs, err := testStore.ListExceptions(ctx, other, run.RunID, store.ListExceptionsFilter{})
	require.NoError(t, err)
	assert.Empty(t, exs)
	_, err = testStore.GetException(ctx, other, list[0].ExceptionID)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = testStore.AssignException(ctx, other, list[0].ExceptionID, "mallory", "c", 1,
		domain.AssignExceptionRequest{OwnerPrincipalID: "mallory", Reason: "steal"})
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = testStore.GetLatestEvidence(ctx, other, run.RunID)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = testStore.GetExecutionContext(ctx, other, run.RunID)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, _, err = testStore.BeginExecution(ctx, other, run.RunID, "steal", "mallory", "c", store.SkipVersionCheck)
	assert.ErrorIs(t, err, domain.ErrNotFound, "another tenant cannot execute this run")

	// The victim's exception is untouched.
	still, err := testStore.GetException(ctx, x.tenant, list[0].ExceptionID)
	require.NoError(t, err)
	assert.Equal(t, domain.ExOpen, still.State)
}

// Tolerance pinned on the run is what the engine used: a wider policy created
// AFTER the run was created does not change its outcome (scenario 04, end to end).
func TestE2E_PinnedToleranceGovernsTheRun(t *testing.T) {
	x := newE2E(t)
	x.src.set("ar", "invoices", "wm", r("a1", "INV-1", "10.00"))
	x.src.set("gl", "ar-postings", "wm", r("g1", "INV-1", "9.60"))

	_, err := testStore.CreateTolerancePolicy(ctx, x.tenant, "maker", domain.CreateTolerancePolicyRequest{
		LegalEntityID: x.entity, Metric: "FIN-CTRL-001", AbsoluteTolerance: "0.01", Currency: "USD",
		Rationale: "cent rounding", EffectiveFrom: "2026-01-01", ApprovedBy: "owner"})
	require.NoError(t, err)
	run, _, err := testStore.CreateRun(ctx, x.tenant, "prep", "c", "create", runReq(x.def.ControlDefinitionID, x.entity))
	require.NoError(t, err)
	require.NotNil(t, run.ToleranceID)

	// After the run exists, someone creates a much wider tolerance. It cannot rescue this run.
	_, err = testStore.CreateTolerancePolicy(ctx, x.tenant, "maker", domain.CreateTolerancePolicyRequest{
		LegalEntityID: x.entity, Metric: "FIN-CTRL-001", AbsoluteTolerance: "100", Currency: "USD",
		Rationale: "widened after the fact", EffectiveFrom: "2026-01-01", ApprovedBy: "owner"})
	require.NoError(t, err)

	final := x.execute(run, "k1")
	assert.Equal(t, domain.ResultFail, final.ResultState, "0.40 exceeds the pinned 0.01; the later 100 tolerance does not apply")
	pkg, err := testStore.GetLatestEvidence(ctx, x.tenant, run.RunID)
	require.NoError(t, err)
	var c domain.EvidenceContent
	require.NoError(t, json.Unmarshal(pkg.Content, &c))
	assert.Equal(t, *run.ToleranceID, c.Execution.ToleranceID)
	assert.Equal(t, "0.01", c.Execution.AbsoluteTolerance)
}

func TestE2E_SeverityFromPinnedMateriality(t *testing.T) {
	x := newE2E(t)
	_, err := testStore.CreateMaterialityPolicy(ctx, x.tenant, "maker", domain.CreateMaterialityPolicyRequest{
		LegalEntityID: x.entity, ReportingBasis: "US_GAAP", AmountThreshold: "1000", AggregateThreshold: "5000",
		Currency: "USD", EffectiveFrom: "2026-01-01", ApprovedBy: "cfo"})
	require.NoError(t, err)
	x.src.set("ar", "invoices", "wm", r("a1", "I-1", "5000"), r("a2", "I-2", "2"))
	x.src.set("gl", "ar-postings", "wm")
	run, _ := x.start("k1")
	list, err := testStore.ListExceptions(ctx, x.tenant, run.RunID, store.ListExceptionsFilter{})
	require.NoError(t, err)
	sev := map[string]domain.Severity{}
	for _, e := range list {
		sev[e.RecordIDs[0]] = e.Severity
	}
	assert.Equal(t, domain.SeverityHigh, sev["a1"], "5000 >= 1000")
	assert.Equal(t, domain.SeverityMedium, sev["a2"], "missing records never fall below MEDIUM")
}

func TestE2E_LargePopulationRoundTrip(t *testing.T) {
	x := newE2E(t)
	var a, b []domain.PopulationRecord
	for i := 0; i < 2600; i++ { // > 2 fetch pages and > 5 insert batches
		a = append(a, r(fmt.Sprintf("a%05d", i), fmt.Sprintf("INV-%d", i), "1.25"))
		b = append(b, r(fmt.Sprintf("g%05d", i), fmt.Sprintf("INV-%d", i), "1.25"))
	}
	x.src.set("ar", "invoices", "wm", a...)
	x.src.set("gl", "ar-postings", "wm", b...)
	run, final := x.start("k1")
	assert.Equal(t, domain.ResultPass, final.ResultState)
	snaps, err := testStore.ListPopulationSnapshots(ctx, x.tenant, run.RunID)
	require.NoError(t, err)
	assert.Equal(t, 2600, snaps[0].RowCount)
	assert.Equal(t, "3250", snaps[0].Totals[0].Total)
	matches, err := testStore.ListMatchResults(ctx, x.tenant, run.RunID)
	require.NoError(t, err)
	assert.Len(t, matches, 2600)
}

// lastQuery returns the query string of the most recent request the named system received.
func (f *fakeSources) lastQuery(system string) url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.queries[system]
}
