//go:build integration

package arctl

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

func runID(m map[string]any) string { return m["run_id"].(string) }

// #21: AR total equals the GL (200 = 200) while B has no open item and a stray open item C exists: the run must break.
func TestNeg21_TotalsMatchGLButOpenItemsMissingAndUnexpected(t *testing.T) {
	e := newEnv(t)
	a := e.invoice("100.00", "USD", true)
	b := e.invoice("100.00", "USD", false) // issued, receivable never created
	stray := uuid.NewString()
	e.openItem(stray, "100.00", "USD")
	e.postAll(a, b)
	e.gl.set(usd("200.00")) // headline GL = headline AR (A's open item 100 + B at gross 100)
	run := e.mustRun("2026-09-30", nil)

	require.Equal(t, ResultBreak, run["result"], "matching headline totals must not hide missing/unexpected open items")
	fs := e.findings(runID(run))
	require.ElementsMatch(t, []string{KindMissingOpenItem, KindUnexpectedOpenItem}, kindsOf(fs), "and no GL difference: the totals do tie")
	for _, f := range fs {
		require.Equal(t, kit.CodeARGLReconciliationBreak, f["code"])
		switch f["kind"] {
		case KindMissingOpenItem:
			require.Equal(t, b, f["subject_id"])
		case KindUnexpectedOpenItem:
			require.Equal(t, stray, f["subject_id"])
		}
	}
	// the same population is visible through the control link evidence
	code, link := e.get("getARControlLink?run_id=" + runID(run))
	require.Equal(t, 200, code)
	require.Equal(t, true, link["gl_reconciliation"].(map[string]any)["exact_comparison"])
	require.EqualValues(t, 2, link["findings"].(map[string]any)["blocking"])
}

// A duplicated issued document (the expected list carries it twice) is a finding even though totals can be made to agree.
func TestNeg21_DuplicateDocumentFails(t *testing.T) {
	e := newEnv(t)
	a := e.invoice("100.00", "USD", true)
	e.postAll(a)
	e.gl.set(usd("100.00"))
	require.Equal(t, ResultPass, e.mustRun("2026-09-30", nil)["result"], "baseline is clean")
	// a second run on a doc source that lists A twice
	e.m.src.Documents = dupDocs{PgSources{DB: e.db}}
	run := e.mustRun("2026-09-30", nil)
	require.Equal(t, ResultBreak, run["result"])
	require.Contains(t, kindsOf(e.findings(runID(run))), KindDuplicateOpenItem)
}

type dupDocs struct{ PgSources }

func (d dupDocs) ExpectedOpenItemDocuments(ctx context.Context, tenantID, legalEntityID string) ([]ExpectedDoc, error) {
	docs, err := d.PgSources.ExpectedOpenItemDocuments(ctx, tenantID, legalEntityID)
	return append(docs, docs...), err
}

// #35: accounting event missing / failed / quarantined / stuck for issued invoice, credit note.
func TestNeg35_AccountingEventMissingForIssuedDocuments(t *testing.T) {
	e := newEnv(t)
	ok := e.invoice("10.00", "USD", true)
	missing := e.invoice("20.00", "USD", true)
	failed := e.invoice("30.00", "USD", true)
	quar := e.invoice("40.00", "USD", true)
	stuck := e.invoice("50.00", "USD", true)
	young := e.invoice("60.00", "USD", true)
	cn := e.note("CREDIT_NOTE", "5.00", "USD")
	e.queue("INVOICE", ok, kit.PostingPosted, time.Hour)
	e.queue("INVOICE", failed, kit.PostingFailed, time.Hour)
	e.queue("INVOICE", quar, kit.PostingQuarantined, time.Hour)
	e.queue("INVOICE", stuck, kit.PostingPending, time.Hour)
	e.queue("INVOICE", young, kit.PostingPending, time.Minute)
	legacyMissing := e.legacy("70.00", "ISSUED")
	e.gl.set(usd("0")) // not under test
	run := e.mustRun("2026-09-30", nil)
	require.Equal(t, ResultBreak, run["result"])

	got := map[string]string{}
	for _, f := range e.findings(runID(run)) {
		if f["category"] == CatAccounting {
			got[f["subject_id"].(string)] = f["kind"].(string) + "/" + f["severity"].(string)
		}
	}
	require.Equal(t, map[string]string{
		missing:       KindPostingMissing + "/BREAK",
		failed:        KindPostingFailed + "/BREAK",
		quar:          KindPostingQuarantined + "/BREAK",
		stuck:         KindPostingPending + "/BREAK",
		young:         KindPostingInFlight + "/INFO",
		cn:            KindPostingMissing + "/BREAK",
		legacyMissing: KindPostingMissing + "/BREAK",
	}, got)
	// the ARControlLink carries the posting refs
	_, link := e.get("getARControlLink?run_id=" + runID(run))
	ae := link["accounting_events"].(map[string]any)
	require.EqualValues(t, 1, ae["posted"])
	require.Len(t, ae["not_posted_refs"], 7)
}

// #36: a tolerance change cannot alter a stored run, hide its break, or rewrite it; a rerun is a NEW linked run.
func TestNeg36_ToleranceChangeCannotHideStoredBreak(t *testing.T) {
	e := newEnv(t)
	a := e.invoice("100.00", "USD", true)
	e.postAll(a)
	e.gl.set(usd("99.99")) // one cent short in the GL
	r1 := e.mustRun("2026-09-30", map[string]any{"tolerance_policy_id": "TOL-AR-1", "tolerance_policy_version": "1"})
	require.Equal(t, ResultBreak, r1["result"])
	fs1 := e.findings(runID(r1))
	require.Equal(t, []string{KindGLDifference}, kindsOf(fs1))
	require.Equal(t, "100.0000", fs1[0]["ar_amount"])
	require.Equal(t, "99.9900", fs1[0]["gl_amount"])
	require.Equal(t, "0.0100", fs1[0]["difference"])

	_, before := e.get("getReconciliationRun?run_id=" + runID(r1) + "&verify=true")

	// the owner of the tolerance policy widens it; AR is asked to rerun with the new version
	r2 := e.mustRun("2026-09-30", map[string]any{"tolerance_policy_id": "TOL-AR-1", "tolerance_policy_version": "2-wide"})
	require.NotEqual(t, runID(r1), runID(r2))
	require.Equal(t, runID(r1), r2["supersedes_run_id"], "a rerun is a new run linked by supersedes")
	require.Equal(t, ResultBreak, r2["result"], "a wider tolerance does not turn the difference into a pass in AR")
	require.Equal(t, []string{KindGLDifference}, kindsOf(e.findings(runID(r2))))

	// the stored first run is exactly what it was
	_, after := e.get("getReconciliationRun?run_id=" + runID(r1) + "&verify=true")
	run1 := func(m map[string]any) map[string]any {
		r := m["run"].(map[string]any)
		delete(r, "superseded_by_run_id") // derived link, not stored on the row
		return r
	}
	require.Equal(t, run1(before), run1(after))
	require.Equal(t, "1", run1(after)["tolerance_policy"].(map[string]any)["version"])
	require.Equal(t, true, after["reproducible"], "%v", after["reproducibility_detail"])
	_, h := e.get("getReconciliationRun?run_id=" + runID(r1))
	require.Equal(t, runID(r2), h["run"].(map[string]any)["superseded_by_run_id"])

	// direct tampering is refused by the database
	for _, sql := range []string{
		`UPDATE arctl_runs SET tolerance_policy_version = '99' WHERE run_id = $1`,
		`UPDATE arctl_runs SET result = 'PASS' WHERE run_id = $1`,
		`DELETE FROM arctl_findings WHERE run_id = $1`,
		`UPDATE arctl_evidence SET content = '{}'::jsonb WHERE run_id = $1`,
		`DELETE FROM arctl_evidence WHERE run_id = $1`,
		`DELETE FROM arctl_runs WHERE run_id = $1`,
	} {
		_, err := pool.Exec(context.Background(), sql, runID(r1))
		require.Error(t, err, sql)
		require.Contains(t, err.Error(), "append-only", sql)
	}
	// a rerun must supersede the CURRENT run
	code, out := e.run("runner", "2026-09-30", map[string]any{"supersedes_run_id": runID(r1)})
	require.Equal(t, http.StatusConflict, code, "%v", out)
	require.Equal(t, kit.CodeStaleVersion, out["code"])
	require.Equal(t, 2, rowCount(t, `SELECT count(*) FROM arctl_runs WHERE tenant_id = $1`, e.tenant))
}

// If the GL cannot be read the run is recorded UNVERIFIED, never PASS.
func TestGLUnreachableIsUnverifiedNotPass(t *testing.T) {
	e := newEnv(t)
	a := e.invoice("100.00", "USD", true)
	e.postAll(a)
	e.gl.mu.Lock()
	e.gl.down = true
	e.gl.mu.Unlock()
	run := e.mustRun("2026-09-30", nil)
	require.Equal(t, ResultUnverified, run["result"])
	require.Equal(t, []string{KindGLUnverified}, kindsOf(e.findings(runID(run))))
	_, link := e.get("getARControlLink?run_id=" + runID(run))
	require.Equal(t, true, link["gl_reconciliation"].(map[string]any)["unverified"])

	// a hard error from the GL source behaves the same way
	e.gl.mu.Lock()
	e.gl.err = errors_New("boom")
	e.gl.mu.Unlock()
	run2 := e.mustRun("2026-09-30", nil)
	require.Equal(t, ResultUnverified, run2["result"])

	// GL readable and tying out: PASS
	e.gl.mu.Lock()
	e.gl.err = nil
	e.gl.mu.Unlock()
	e.gl.set(usd("100.00"))
	require.Equal(t, ResultPass, e.mustRun("2026-09-30", nil)["result"])
}

type constErr string

func (c constErr) Error() string { return string(c) }
func errors_New(s string) error  { return constErr(s) }

func TestRunImmutableAndRunnableChain(t *testing.T) {
	e := newEnv(t)
	a := e.invoice("100.00", "USD", true)
	e.postAll(a)
	e.gl.set(usd("100.00"))
	r1 := e.mustRun("2026-09-30", nil)
	for _, sql := range []string{
		`UPDATE arctl_findings SET severity = 'INFO'`, `DELETE FROM arctl_acknowledgements`, `UPDATE arctl_acknowledgements SET note = 'x'`,
	} {
		_, err := pool.Exec(context.Background(), sql+` WHERE tenant_id = $1`, e.tenant)
		if err != nil {
			require.Contains(t, err.Error(), "append-only")
		}
	}
	// a first run per (entity, as-of) is unique and a run is superseded at most once
	_, err := pool.Exec(context.Background(), `INSERT INTO arctl_runs (run_id, tenant_id, legal_entity_id, as_of_date, cutoff_date, result, inputs_hash, summary, run_by, computed_at)
		VALUES (gen_random_uuid(),$1,$2,'2026-09-30','2026-09-30','PASS','h','{}','x',now())`, e.tenant, e.entity)
	require.Error(t, err)
	require.True(t, kit.IsUniqueViolation(err, "uq_arctl_first_run"), "%v", err)
	_, err = pool.Exec(context.Background(), `INSERT INTO arctl_runs (run_id, tenant_id, legal_entity_id, as_of_date, cutoff_date, result, supersedes_run_id, inputs_hash, summary, run_by, computed_at)
		VALUES (gen_random_uuid(),$1,$2,'2026-09-30','2026-09-30','PASS',$3,'h','{}','x',now())`, e.tenant, e.entity, runID(r1))
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(), `INSERT INTO arctl_runs (run_id, tenant_id, legal_entity_id, as_of_date, cutoff_date, result, supersedes_run_id, inputs_hash, summary, run_by, computed_at)
		VALUES (gen_random_uuid(),$1,$2,'2026-09-30','2026-09-30','PASS',$3,'h','{}','x',now())`, e.tenant, e.entity, runID(r1))
	require.True(t, kit.IsUniqueViolation(err, "uq_arctl_superseded_once"), "%v", err)
}

func TestIdempotentReplayAndEvents(t *testing.T) {
	e := newEnv(t)
	a := e.invoice("100.00", "USD", true)
	e.postAll(a)
	e.gl.set(usd("90.00"))
	body := map[string]any{"legal_entity_id": e.entity, "as_of_date": "2026-09-30"}
	c1, b1 := e.call(http.MethodPost, base+"commands/runARGLReconciliation", e.tenant, "runner", "key-1", body)
	c2, b2 := e.call(http.MethodPost, base+"commands/runARGLReconciliation", e.tenant, "runner", "key-1", body)
	require.Equal(t, http.StatusCreated, c1)
	require.Equal(t, c1, c2)
	require.JSONEq(t, string(b1), string(b2), "a replay returns the stored response")
	require.Equal(t, 1, rowCount(t, `SELECT count(*) FROM arctl_runs WHERE tenant_id = $1`, e.tenant))
	require.Equal(t, 1, rowCount(t, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = $2`, e.tenant, EventRunCompleted))
	require.Equal(t, 1, rowCount(t, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = $2`, e.tenant, EventBreakDetected))
	require.Equal(t, 1, rowCount(t, `SELECT count(*) FROM ar_audit_events WHERE tenant_id = $1 AND command = 'runARGLReconciliation'`, e.tenant))
	// same key, different body: refused
	body["as_of_date"] = "2026-08-31"
	c3, _ := e.call(http.MethodPost, base+"commands/runARGLReconciliation", e.tenant, "runner", "key-1", body)
	require.Equal(t, http.StatusUnprocessableEntity, c3)
	// no key: refused
	req := map[string]any{"legal_entity_id": e.entity, "as_of_date": "2026-09-30"}
	cNoKey, _ := e.call(http.MethodPost, base+"commands/runARGLReconciliation", e.tenant, "runner", "", req)
	require.Equal(t, http.StatusCreated, cNoKey, "the helper adds a fresh key")
	// a clean run emits no break event
	breaks := rowCount(t, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = $2`, e.tenant, EventBreakDetected)
	e.gl.set(usd("0")) // nothing was issued by 2026-08-31
	require.Equal(t, ResultPass, e.mustRun("2026-08-31", nil)["result"])
	require.Equal(t, breaks, rowCount(t, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = $2`, e.tenant, EventBreakDetected))
}

func TestAuthzAndNotAuthorised(t *testing.T) {
	e := newEnv(t)
	code, _ := e.run("nobody", "2026-09-30", nil)
	require.Equal(t, http.StatusForbidden, code)
	require.Equal(t, 0, rowCount(t, `SELECT count(*) FROM arctl_runs WHERE tenant_id = $1`, e.tenant))
}

func TestTenantAndEntityIsolation(t *testing.T) {
	a := newEnv(t)
	inv := a.invoice("100.00", "USD", true)
	a.postAll(inv)
	a.gl.set(usd("50.00"))
	run := a.mustRun("2026-09-30", nil)
	fs := a.findings(runID(run))
	require.NotEmpty(t, fs)

	// another tenant sees nothing, and absent looks identical to foreign
	other := uuid.NewString()
	for _, p := range []string{"getReconciliationRun?run_id=" + runID(run), "getFindings?run_id=" + runID(run), "getARControlLink?run_id=" + runID(run)} {
		code, _ := a.call(http.MethodGet, base+p, other, "reader", "", nil)
		require.Equal(t, http.StatusNotFound, code, p)
		code, _ = a.call(http.MethodGet, base+p[:len(p)-len(runID(run))]+uuid.NewString(), a.tenant, "reader", "", nil)
		require.Equal(t, http.StatusNotFound, code, p)
	}
	code, b := a.call(http.MethodGet, base+"listReconciliationRuns", other, "reader", "", nil)
	require.Equal(t, 200, code)
	require.JSONEq(t, `{"runs":[],"limit":50,"offset":0}`, string(b))
	// acknowledging another tenant's finding is a 404
	code, _ = a.call(http.MethodPost, base+"commands/acknowledgeFinding", other, "acker", "", map[string]any{"finding_id": fs[0]["finding_id"], "reason_code": "R", "evidence_id": "E"})
	require.Equal(t, http.StatusNotFound, code)

	// another legal entity of the SAME tenant: its run sees none of entity A's documents, queue rows or findings
	entityB := uuid.NewString()
	b2 := &env{t: t, m: a.m, h: a.h, ar07m: a.ar07m, gl: a.gl, tenant: a.tenant, entity: entityB, party: a.party, db: a.db}
	a.gl.set(map[string]kit.Amount{})
	rb := b2.mustRun("2026-09-30", nil)
	require.Equal(t, ResultPass, rb["result"])
	require.Empty(t, b2.findings(runID(rb)))
	_, list := a.get("listReconciliationRuns?legal_entity_id=" + entityB)
	require.Len(t, list["runs"], 1)
	_, list = a.get("listReconciliationRuns")
	require.Len(t, list["runs"], 2)
	// the exception roll-up is per entity as well
	_, ex := b2.get("getControlExceptions?legal_entity_id=" + entityB)
	require.EqualValues(t, 0, ex["total"])
}

func TestAcknowledgeNeverClearsAndSeparatesDuties(t *testing.T) {
	e := newEnv(t)
	a := e.invoice("100.00", "USD", true)
	e.postAll(a)
	e.gl.set(usd("1.00"))
	run := e.mustRun("2026-09-30", nil)
	f := e.findings(runID(run))[0]
	ack := func(principal string, body map[string]any) (int, map[string]any) {
		code, b := e.call(http.MethodPost, base+"commands/acknowledgeFinding", e.tenant, principal, "", body)
		var out map[string]any
		_ = json.Unmarshal(b, &out)
		return code, out
	}
	good := map[string]any{"finding_id": f["finding_id"], "reason_code": "TIMING", "evidence_id": "EV-1", "note": "known cut-off item"}
	code, out := ack("runner", good)
	require.Equal(t, http.StatusForbidden, code, "the principal that ran the reconciliation cannot acknowledge it")
	require.Equal(t, kit.CodeSegregationOfDuties, out["code"])
	code, _ = ack("acker", map[string]any{"finding_id": f["finding_id"], "evidence_id": "EV-1"})
	require.Equal(t, 400, code, "reason required")
	code, _ = ack("acker", map[string]any{"finding_id": f["finding_id"], "reason_code": "R"})
	require.Equal(t, 400, code, "evidence required")
	code, _ = ack("nobody", good)
	require.Equal(t, http.StatusForbidden, code)
	code, out = ack("acker", good)
	require.Equal(t, http.StatusCreated, code, "%v", out)
	require.Equal(t, false, out["finding_changed"])

	after := e.findings(runID(run))[0]
	require.Equal(t, true, after["acknowledged"])
	require.Len(t, after["acknowledgements"], 1)
	for _, k := range []string{"kind", "severity", "code", "ar_amount", "gl_amount", "difference"} {
		require.Equal(t, f[k], after[k], "acknowledgement must not change %s", k)
	}
	_, r := e.get("getReconciliationRun?run_id=" + runID(run))
	require.Equal(t, ResultBreak, r["run"].(map[string]any)["result"], "the run still reads BREAK")
	_, link := e.get("getARControlLink?run_id=" + runID(run))
	require.EqualValues(t, 1, link["findings"].(map[string]any)["blocking"], "an acknowledged break still counts")
	require.EqualValues(t, 1, link["findings"].(map[string]any)["acknowledged"])
	require.Equal(t, 1, rowCount(t, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = $2`, e.tenant, EventFindingAcked))
}

func TestReproducibleAndHistoricalReconstruction(t *testing.T) {
	e := newEnv(t)
	a := e.invoice("100.00", "USD", true)
	e.postAll(a)
	e.gl.set(usd("100.00"))
	r1 := e.mustRun("2026-09-30", nil)
	require.Equal(t, ResultPass, r1["result"])
	// the world changes afterwards: a new issued invoice with no posting, GL moves
	b := e.invoice("40.00", "USD", false)
	_ = b
	e.gl.set(usd("7.00"))
	r2 := e.mustRun("2026-09-30", nil)
	require.Equal(t, ResultBreak, r2["result"])

	// run 1 reads back exactly as computed then
	code, got := e.get("getReconciliationRun?run_id=" + runID(r1) + "&verify=true&include_inputs=true")
	require.Equal(t, 200, code)
	require.Equal(t, true, got["reproducible"], "%v", got["reproducibility_detail"])
	require.Equal(t, ResultPass, got["run"].(map[string]any)["result"])
	require.Empty(t, e.findings(runID(r1)))
	var gl map[string]any
	for _, ev := range got["evidence"].([]any) {
		if ev.(map[string]any)["kind"] == EvGLRead {
			gl = ev.(map[string]any)["content"].(map[string]any)
		}
	}
	require.Equal(t, "100.0000", gl["totals"].(map[string]any)["USD"])
	require.Equal(t, 6, len(got["evidence"].([]any)))
	code, got2 := e.get("getReconciliationRun?run_id=" + runID(r2) + "&verify=true")
	require.Equal(t, 200, code)
	require.Equal(t, true, got2["reproducible"])
	// as-of lookup resolves the head of the chain
	_, link := e.get("getARControlLink?legal_entity_id=" + e.entity + "&as_of_date=2026-09-30")
	require.Equal(t, runID(r2), link["run_id"])
}

func TestControlExceptionsRollup(t *testing.T) {
	e := newEnv(t)
	_ = e.invoice("10.00", "USD", true) // issued, no posting request
	_ = e.note("DEBIT_NOTE", "5.00", "USD")
	e.receipt("100.00", "0")
	_, err := pool.Exec(context.Background(), `UPDATE ar08_receipts SET blocked = true, control_exception = 'NO_FX_RATE' WHERE tenant_id = $1`, e.tenant)
	require.NoError(t, err)
	code, out := e.get("getControlExceptions?legal_entity_id=" + e.entity)
	require.Equal(t, 200, code, "%v", out)
	by := out["count_by_source"].(map[string]any)
	require.EqualValues(t, 1, by["AR04"])
	require.EqualValues(t, 1, by["AR06"])
	require.EqualValues(t, 1, by["AR08"])
	code, _ = e.get("getControlExceptions")
	require.Equal(t, 400, code)
}
