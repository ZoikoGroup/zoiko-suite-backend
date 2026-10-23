package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/connectivity-api-bridge-svc/internal/batch"
	"zoiko.io/connectivity-api-bridge-svc/internal/handler"
	"zoiko.io/connectivity-api-bridge-svc/internal/recon"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/connectivity-api-bridge-svc/internal/signals"
	"zoiko.io/connectivity-api-bridge-svc/internal/store"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
)

// These run reconciliation against REAL Postgres as an ordinary role: the comparison reads the real exchange, attempt,
// receipt and batch tables, the database refuses a non-independent run and holds the variance rules itself, and nothing
// reconciliation does changes any of the records it reads.

type rEnv struct {
	*bEnv
	rrepo *store.ReconRepo
	rsvc  *recon.Service
	c     *xicconn.Connection
	v     *registry.ContractVersion
}

var (
	rcMaker    = recon.Actor{PrincipalID: "ops-user", CorrelationID: "corr-rm"}
	rcDomain   = recon.Actor{PrincipalID: "domain-svc"}
	rcReviewer = recon.Actor{PrincipalID: "reviewer"}
)

func newREnv(t *testing.T) *rEnv {
	t.Helper()
	b := newBEnv(t)
	e := &rEnv{bEnv: b, rrepo: store.NewReconRepo(b.st)}
	e.rsvc = &recon.Service{Repo: e.rrepo, Conns: pgConns{b.gwEnv.svc}, Connectors: store.NewRegistryRepo(b.st), Live: store.NewXICConnRepo(b.st),
		Clock: func() time.Time { return b.now() }}
	e.v = b.fileContract(t, nil)
	e.c = b.sandbox(t, cTenantA, e.v, "acc-a", cRefOAuth, cRefHMAC)
	return e
}

func (e *rEnv) req(basis []string, ext *recon.External) recon.RunRequest {
	return recon.RunRequest{ConnectionID: e.c.ConnectionID, PeriodFrom: e.now().Add(-24 * time.Hour), PeriodTo: e.now(), Basis: basis, External: ext}
}

func (e *rEnv) run(t *testing.T, r recon.RunRequest) *recon.Run {
	t.Helper()
	run, existing, err := e.rsvc.RequestRun(tenantCtx(cTenantA), rcMaker, r)
	if err != nil || existing {
		t.Fatalf("%v %v", err, existing)
	}
	return run
}

func (e *rEnv) eval(t *testing.T, r *recon.Run) *recon.Run {
	t.Helper()
	got, err := e.rsvc.Evaluate(context.Background(), cTenantA, r.RunID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func (e *rEnv) variances(t *testing.T) []recon.Variance {
	t.Helper()
	v, err := e.rrepo.ListVariances(tenantCtx(cTenantA), recon.VarianceFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// attempt plants an outbound attempt (and optionally the provider's receipt for it) as the migration owner, bypassing the
// engines: reconciliation is shown against records it did not create.
func (e *rEnv) attempt(t *testing.T, id, state string, age time.Duration, receiptState string, needsReconcile bool) {
	t.Helper()
	ctx := context.Background()
	tx, err := e.admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	at := e.now().Add(-age).Add(-30 * time.Minute) // inside the period the tests ask about
	outcome, method, amb := any(nil), "", ""
	if state == "SETTLED" || state == "FAILED" {
		outcome, method = []byte(`{"status":"x"}`), "STATUS_QUERY"
	}
	if state == "UNKNOWN" {
		amb = "TIMEOUT"
	}
	submit := 0
	if state != "RESERVED" {
		submit = 1
	}
	if _, err := tx.Exec(ctx, `INSERT INTO xic_outbound_attempts (attempt_id, tenant_id, connection_id, connector_id, operation_id, contract_version_id, domain_request_ref, payload_hash,
		payload_size, state, ambiguity_reason, final_outcome, resolution_method, submit_count, last_submit_at, requested_by, created_at, updated_at)
		VALUES ($1::uuid, $2, $3::uuid, $4, 'payment-submit', $5::uuid, $6, repeat('a', 64), 10, $7, $8, $9, $10, $11, CASE WHEN $11 > 0 THEN $12::timestamptz END, 'fixture', $12, $12)`,
		id, cTenantA, e.c.ConnectionID, e.c.ConnectorID, e.c.ContractVersionID, "dr-"+id, state, amb, outcome, method, submit, at); err != nil {
		t.Fatal(err)
	}
	if receiptState != "" {
		st, reason := "APPLIED", ""
		var dk any = "dk-" + id
		if needsReconcile {
			st, reason, dk = "REJECTED", "LATE_CONTRADICTS_FINAL", nil
		}
		if _, err := tx.Exec(ctx, `INSERT INTO xic_callback_receipts (receipt_id, tenant_id, connection_id, connector_id, operation_id, contract_version_id, attempt_id, state, auth_result,
			reason_code, body_hash, body_size, target_state, needs_reconciliation, received_at, attempt_state_before, attempt_state_after, dedupe_key) VALUES (gen_random_uuid(), $1, $2::uuid, $3, 'payment-submit', $4::uuid, $5::uuid, $6,
			'OK', $7, repeat('b', 64), 5, $8, $9, $10, 'PENDING', $11, $12)`, cTenantA, e.c.ConnectionID, e.c.ConnectorID, e.c.ContractVersionID, id, st, reason, receiptState, needsReconcile, at, state, dk); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func uuidN(n int) string {
	return "00000000-0000-4000-8000-" + strings.Repeat("0", 11) + string(rune('0'+n%10))
}

// historyDigest fingerprints every record reconciliation reads, so a test can show a run did not change one.
func (e *rEnv) historyDigest(t *testing.T) string {
	t.Helper()
	var d string
	err := e.admin.QueryRow(context.Background(), `SELECT md5(
		COALESCE((SELECT string_agg(x::text, '|' ORDER BY exchange_id) FROM xic_inbound_exchanges x), '') || '#' ||
		COALESCE((SELECT string_agg(x::text, '|' ORDER BY attempt_id) FROM xic_outbound_attempts x), '') || '#' ||
		COALESCE((SELECT string_agg(x::text, '|' ORDER BY receipt_id) FROM xic_callback_receipts x), '') || '#' ||
		COALESCE((SELECT string_agg(x::text, '|' ORDER BY batch_id) FROM xic_batches x), '') || '#' ||
		COALESCE((SELECT string_agg(x::text, '|' ORDER BY row_id) FROM xic_batch_rows x), ''))`).Scan(&d)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func provPG(counts map[string]int) *recon.External {
	return &recon.External{Source: "PROVIDER_STATEMENT", Counts: counts}
}

// ---- schema ------------------------------------------------------------------------------------------------------

func TestReconPG_MigrationIsRerunnable(t *testing.T) {
	b := newBEnv(t)
	dir, names := migrationFiles(t)
	for _, n := range names {
		if strings.HasPrefix(n, "010") {
			raw, err := readFile(dir, n)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := b.admin.Exec(context.Background(), string(raw)); err != nil {
				t.Fatalf("010 applied a second time: %v", err)
			}
			return
		}
	}
	t.Fatal("no 010 migration")
}

// ---- the comparison reads the real tables ----------------------------------------------------------------------

func TestReconPG_CountsAndReferencesAreComparedWithWhatTheDatabaseHolds(t *testing.T) {
	e := newREnv(t)
	e.target(t)
	o := e.deliver(e.c, "B-R1", 5)
	e.settle(t, e.process(t, o.Batch))
	before := e.historyDigest(t)

	ok := e.eval(t, e.run(t, e.req([]string{"COUNT", "REFERENCE"}, &recon.External{Source: "PROVIDER_STATEMENT",
		Counts: map[string]int{"received": 5, "accepted": 5, "duplicate": 0}, References: []string{"B-R1"}})))
	if ok.State != recon.RunMatched || ok.VarianceCount != 0 || ok.EvaluatedAt == nil {
		t.Fatalf("%+v", ok)
	}
	// NP-45: the provider's count is higher than ours, and it names a batch we never received
	bad := e.eval(t, e.run(t, e.req([]string{"COUNT", "REFERENCE"}, &recon.External{Source: "PROVIDER_STATEMENT",
		Counts: map[string]int{"received": 7}, References: []string{"B-R1", "B-GHOST"}})))
	if bad.State != recon.RunVariance || bad.VarianceCount != 2 || bad.OpenVariances != 2 {
		t.Fatalf("%+v", bad)
	}
	types := map[string]recon.Variance{}
	for _, v := range e.variances(t) {
		types[v.Type] = v
	}
	cv, rv := types[recon.VarCount], types[recon.VarRefMissingInt]
	var cr, rr map[string]any
	_ = json.Unmarshal(cv.Refs, &cr)
	_ = json.Unmarshal(rv.Refs, &rr)
	if cr["external"].(float64) != 7 || cr["internal"].(float64) != 5 || !strings.Contains(string(rv.Refs), "B-GHOST") || strings.Contains(string(rv.Refs), "B-R1") {
		t.Fatalf("%v %v", cr, rr)
	}
	// reconciliation never rewrites history
	if after := e.historyDigest(t); after != before {
		t.Fatal("a run changed an exchange, attempt, receipt or batch")
	}
	var open int
	if err := e.admin.QueryRow(context.Background(), `SELECT count(*) FROM xic_reconciliation_variances WHERE state IN ('OPEN','ESCALATED')`).Scan(&open); err != nil || open != 2 {
		t.Fatalf("%d %v", open, err)
	}
}

func TestReconPG_AttemptsReceiptsAndBatchesAreReadAsTheyAre(t *testing.T) {
	e := newREnv(t)
	e.target(t)
	e.attempt(t, uuidN(1), "UNKNOWN", 72*time.Hour, "", false)
	e.attempt(t, uuidN(2), "SETTLED", 72*time.Hour, "FAILED", false)
	e.attempt(t, uuidN(3), "SETTLED", 72*time.Hour, "FAILED", true)
	e.attempt(t, uuidN(4), "SETTLED", 72*time.Hour, "SETTLED", false)
	e.attempt(t, uuidN(5), "PENDING", 72*time.Hour, "", false)
	o := e.deliver(e.c, "B-R2", 3)
	b := e.process(t, o.Batch)
	if b.State != batch.StateProcessing {
		t.Fatalf("%+v", b)
	}
	e.advance(48 * time.Hour) // the batch is long overdue now
	before := e.historyDigest(t)
	req := e.req([]string{"STATE", "EVIDENCE"}, nil)
	req.PeriodFrom, req.PeriodTo = e.now().Add(-200*time.Hour), e.now()
	run := e.eval(t, e.run(t, req))
	if run.State != recon.RunEscalated {
		t.Fatalf("%+v", run)
	}
	got := map[string]string{}
	for _, v := range e.variances(t) {
		var refs map[string]any
		_ = json.Unmarshal(v.Refs, &refs)
		key := v.Type
		if id, ok := refs["attempt_id"].(string); ok {
			key += ":" + id[len(id)-1:]
		}
		got[key] = v.Severity
	}
	for _, want := range []string{recon.VarUnknown + ":1", recon.VarState + ":2", recon.VarLateContradicts + ":3", recon.VarStalePending + ":5", recon.VarBatchIncomplete} {
		if got[want] == "" {
			t.Errorf("no %s in %v", want, got)
		}
	}
	if got[recon.VarState+":4"] != "" || len(got) != 5 {
		t.Fatalf("%v", got)
	}
	// NP-46: the unknown attempt is still UNKNOWN: nothing forced it to FAILED
	var st string
	if err := e.admin.QueryRow(context.Background(), `SELECT state FROM xic_outbound_attempts WHERE attempt_id=$1::uuid`, uuidN(1)).Scan(&st); err != nil || st != "UNKNOWN" {
		t.Fatalf("%s %v", st, err)
	}
	if after := e.historyDigest(t); after != before {
		t.Fatal("reconciliation changed history")
	}
}

func TestReconPG_AmountComparisonNeedsTheDomainsTotalsFromAnotherPerson(t *testing.T) {
	e := newREnv(t)
	r := e.run(t, e.req([]string{"AMOUNT"}, &recon.External{Source: "PROVIDER_API", TotalAmount: "2500.00", Currency: "GBP"}))
	if !r.AwaitingDomain {
		t.Fatal("waits for the domain")
	}
	if got := e.eval(t, r); got.State != recon.RunOpen {
		t.Fatalf("%+v", got)
	}
	if _, err := e.rsvc.SupplyDomainTotals(tenantCtx(cTenantA), rcMaker, r.RunID, recon.DomainTotals{TotalAmount: "2500.00", Currency: "GBP"}); !errors.Is(err, recon.ErrSelfAction) {
		t.Fatalf("%v", err)
	}
	if _, err := e.rsvc.SupplyDomainTotals(tenantCtx(cTenantA), rcDomain, r.RunID, recon.DomainTotals{TotalAmount: "2400.00", Currency: "GBP", Reference: "ledger"}); err != nil {
		t.Fatal(err)
	}
	if n, err := e.rsvc.DriveDue(context.Background(), 10); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	got, _ := e.rrepo.GetRun(tenantCtx(cTenantA), r.RunID)
	if got.State != recon.RunEscalated || got.DomainTotalsBy != "domain-svc" {
		t.Fatalf("%+v", got)
	}
	v := e.variances(t)[0]
	var refs map[string]string
	_ = json.Unmarshal(v.Refs, &refs)
	if v.Type != recon.VarAmount || refs["difference"] != "100" || refs["currency"] != "GBP" {
		t.Fatalf("%+v %v", v, refs)
	}
}

// ---- what the database holds on its own -------------------------------------------------------------------------

func TestReconPG_DatabaseRulesHoldWithoutTheEngine(t *testing.T) {
	e := newREnv(t)
	run := e.eval(t, e.run(t, e.req([]string{"COUNT"}, provPG(map[string]int{"received": 9}))))
	v := e.variances(t)[0]
	ins := func(basis string, ext string, source string, by string) error {
		return e.execAs(cTenantA, `INSERT INTO xic_reconciliation_runs (run_id, tenant_id, connection_id, connector_id, family, origin, period_from, period_to, basis, external_source, external,
			external_supplied_by, dedupe_key, requested_by) VALUES (gen_random_uuid(), $1, $2::uuid, 'bank-x', 'BANK_FEED', 'REQUESTED', now() - interval '2 days', now() - interval '1 day',
			$3::text[], $4, $5::jsonb, $6, md5(random()::text), 'x')`, cTenantA, e.c.ConnectionID, basis, source, nilIfEmpty(ext), by)
	}
	// NP-44: the database will not store a comparison of the platform with itself
	wantDBError(t, "no declaration", ins("{COUNT}", "", "", ""), "xic_rr_independent")
	wantDBError(t, "declared by the platform", ins("{COUNT}", `{"counts":{"received":1}}`, "INTERNAL", "someone"), "xic_rr_independent")
	wantDBError(t, "declared by nobody", ins("{AMOUNT}", `{"total_amount":"1"}`, "PROVIDER_API", ""), "xic_rr_independent")
	if err := ins("{STATE}", "", "", ""); err != nil {
		t.Fatalf("a state run needs no declaration: %v", err)
	}
	wantDBError(t, "unknown basis", ins("{VIBES}", "", "", ""), "xic_rr_basis")
	set := func(sql string, args ...any) error {
		return e.execAs(cTenantA, `UPDATE xic_reconciliation_runs SET `+sql+`, version=version+1 WHERE run_id=$1::uuid`, append([]any{run.RunID}, args...)...)
	}
	wantDBError(t, "identity", set(`origin='SCHEDULED'`), "fixed at request")
	wantDBError(t, "period", set(`period_to = period_to + interval '1 hour'`), "fixed at request")
	wantDBError(t, "basis", set(`basis = '{STATE}'`), "fixed at request")
	wantDBError(t, "declaration", set(`external = '{"counts":{"received":9}}'::jsonb`), "fixed at request")
	wantDBError(t, "requester", set(`requested_by = 'other'`), "fixed at request")
	wantDBError(t, "version must advance by one", e.execAs(cTenantA, `UPDATE xic_reconciliation_runs SET variance_count=5, version=version+3 WHERE run_id=$1::uuid`, run.RunID), "version")
	wantDBError(t, "back to open", set(`state='OPEN'`), "cannot go")
	wantDBError(t, "close with an open variance", set(`state='CLOSED', closed_by='x'`), "open variances")
	wantDBError(t, "same person on both sides", e.execAs(cTenantA, `UPDATE xic_reconciliation_runs SET domain_totals='{"total_amount":"1"}'::jsonb, domain_totals_by=external_supplied_by, version=version+1 WHERE run_id=$1::uuid`, run.RunID), "xic_rr_distinct_suppliers")
	wantDBError(t, "delete a run", e.execAs(cTenantA, `DELETE FROM xic_reconciliation_runs`), "never deleted")
	// variances: identity, evidence, one open per fingerprint, and the case path
	setv := func(sql string, args ...any) error {
		return e.execAs(cTenantA, `UPDATE xic_reconciliation_variances SET `+sql+`, version=version+1 WHERE variance_id=$1::uuid`, append([]any{v.VarianceID}, args...)...)
	}
	wantDBError(t, "variance type", setv(`variance_type='AMOUNT_MISMATCH'`), "fixed when it is opened")
	wantDBError(t, "variance evidence", setv(`refs='{}'::jsonb`), "fixed when it is opened")
	wantDBError(t, "variance run", setv(`run_id=last_run_id`+`, fingerprint=repeat('e', 64)`), "fixed when it is opened")
	wantDBError(t, "accepted without a case", setv(`state='ACCEPTED', resolved_by='r', resolution_note='fine'`), "xic_rv_accepted_reasoned")
	wantDBError(t, "accepted without a note", setv(`state='ACCEPTED', resolved_by='r', case_ref='C-1'`), "xic_rv_accepted_reasoned")
	wantDBError(t, "resolved without saying how", setv(`state='RESOLVED', resolved_by='r', resolved_at=now()`), "xic_rv_resolved_named")
	wantDBError(t, "repeat count falls", setv(`repeat_count=0`), "")
	wantDBError(t, "delete a variance", e.execAs(cTenantA, `DELETE FROM xic_reconciliation_variances`), "never deleted")
	wantDBError(t, "a second open variance with the same fingerprint", e.execAs(cTenantA, `INSERT INTO xic_reconciliation_variances (variance_id, tenant_id, run_id, last_run_id, connection_id,
		variance_type, severity, state, fingerprint, refs) SELECT gen_random_uuid(), tenant_id, run_id, run_id, connection_id, variance_type, severity, 'OPEN', fingerprint, refs
		FROM xic_reconciliation_variances WHERE variance_id=$1::uuid`, v.VarianceID), "xic_rv_open_once")
	wantDBError(t, "evidence rewrite", e.execAs(cTenantA, `UPDATE xic_reconciliation_events SET kind='X'`), "")
	wantDBError(t, "evidence delete", e.execAs(cTenantA, `DELETE FROM xic_reconciliation_events`), "")
	// settle it through the engine, then it is final in the database too
	if _, err := e.rsvc.Resolve(tenantCtx(cTenantA), rcReviewer, v.VarianceID, "PROVIDER_CONFIRMED", "provider re-sent the file", 0); err != nil {
		t.Fatal(err)
	}
	wantDBError(t, "a resolved variance is final", setv(`case_ref='late'`), "is final")
	if got, _ := e.rrepo.GetRun(tenantCtx(cTenantA), run.RunID); got.State != recon.RunClosed {
		t.Fatalf("the run should close with its last variance: %+v", got)
	}
	wantDBError(t, "a closed run is final", set(`close_note='x'`), "is final")
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func readFile(dir, name string) ([]byte, error) { return os.ReadFile(filepath.Join(dir, name)) }

// ---- repeats, concurrency, isolation, events ----------------------------------------------------------------------

func TestReconPG_RepeatFindingsCountAndASettledVarianceStartsAFreshOne(t *testing.T) {
	e := newREnv(t)
	first := e.eval(t, e.run(t, e.req([]string{"COUNT"}, provPG(map[string]int{"received": 4}))))
	req := e.req([]string{"COUNT"}, provPG(map[string]int{"received": 4}))
	req.PeriodTo = req.PeriodTo.Add(-time.Second)
	second := e.eval(t, e.run(t, req))
	vs := e.variances(t)
	if len(vs) != 1 || vs[0].RepeatCount != 2 || vs[0].RunID != first.RunID || vs[0].LastRunID != second.RunID {
		t.Fatalf("%+v", vs)
	}
	if _, err := e.rsvc.LinkCase(tenantCtx(cTenantA), rcReviewer, vs[0].VarianceID, "CASE-9", vs[0].Version); err != nil {
		t.Fatal(err)
	}
	acc, err := e.rsvc.Accept(tenantCtx(cTenantA), rcReviewer, vs[0].VarianceID, "", "provider test traffic", 0)
	if err != nil || acc.State != recon.VarAccepted || acc.CaseRef != "CASE-9" {
		t.Fatalf("%v %+v", err, acc)
	}
	// both runs that carried the variance close with it
	for _, id := range []string{first.RunID, second.RunID} {
		if r, _ := e.rrepo.GetRun(tenantCtx(cTenantA), id); r.State != recon.RunClosed || r.OpenVariances != 0 {
			t.Fatalf("%+v", r)
		}
	}
	req.PeriodTo = req.PeriodTo.Add(-time.Second)
	e.eval(t, e.run(t, req))
	if n := len(e.variances(t)); n != 2 {
		t.Fatalf("%d", n)
	}
}

func TestReconPG_ConcurrentWorkersEvaluateARunOnce(t *testing.T) {
	e := newREnv(t)
	r := e.run(t, e.req([]string{"COUNT"}, provPG(map[string]int{"received": 3, "accepted": 1})))
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = e.rsvc.Evaluate(context.Background(), cTenantA, r.RunID)
		}()
	}
	wg.Wait()
	if n := len(e.variances(t)); n != 2 {
		t.Fatalf("%d variances", n)
	}
	if n := e.countAs(cTenantA, `SELECT count(*) FROM xic_reconciliation_events WHERE kind='EVALUATED'`); n != 1 {
		t.Fatalf("%d evaluations", n)
	}
	if n := e.countAs(cTenantA, `SELECT count(*) FROM eventing_outbox WHERE event_type='com.zoikosuite.integration.reconciliation.completed'`); n != 1 {
		t.Fatalf("%d completed events", n)
	}
}

func TestReconPG_EventsCarryTheContractFieldsAndNoContent(t *testing.T) {
	e := newREnv(t)
	e.target(t)
	o := e.deliver(e.c, "B-EV", 2)
	e.settle(t, e.process(t, o.Batch))
	run := e.eval(t, e.run(t, e.req([]string{"COUNT"}, provPG(map[string]int{"received": 9}))))
	v := e.variances(t)[0]
	if _, err := e.rsvc.Resolve(tenantCtx(cTenantA), rcReviewer, v.VarianceID, "DOMAIN_CORRECTED", "ledger corrected", 0); err != nil {
		t.Fatal(err)
	}
	for _, typ := range recon.EventTypes {
		if typ == "integration.reconciliation.escalated" {
			continue
		}
		if n := e.countAs(cTenantA, `SELECT count(*) FROM eventing_outbox WHERE event_type=$1`, "com.zoikosuite."+typ); n < 1 {
			t.Errorf("no %s event", typ)
		}
	}
	// the variance event names the run, the variance, the connection and the type (§14.1); the check reads the real
	// envelope and carries a positive control
	for _, field := range []string{`"run_id"`, `"variance_id"`, `"connection_id"`, `"variance_type"`, run.RunID} {
		if n := e.countAs(cTenantA, `SELECT count(*) FROM eventing_outbox WHERE event_type='com.zoikosuite.integration.reconciliation.variance' AND convert_from(payload, 'UTF8') LIKE '%'||$1||'%'`, field); n != 1 {
			t.Errorf("the variance event lacks %s (%d)", field, n)
		}
	}
	for _, never := range []string{"B-EV-1000", "10.5", "GBP", pgHMAC} {
		if n := e.countAs(cTenantA, `SELECT count(*) FROM eventing_outbox WHERE event_type LIKE '%reconciliation%' AND convert_from(payload, 'UTF8') LIKE '%'||$1||'%'`, never); n != 0 {
			t.Errorf("%d reconciliation events carry %q", n, never)
		}
		if n := e.countAs(cTenantA, `SELECT count(*) FROM xic_reconciliation_events WHERE detail::text LIKE '%'||$1||'%'`, never); n != 0 {
			t.Errorf("%d reconciliation evidence rows carry %q", n, never)
		}
	}
}

func TestReconPG_TenantIsolation(t *testing.T) {
	e := newREnv(t)
	cB := e.sandbox(t, cTenantB, e.v, "acc-b", cRefOAuth2, cRefHMAC2)
	run := e.eval(t, e.run(t, e.req([]string{"COUNT"}, provPG(map[string]int{"received": 6}))))
	v := e.variances(t)[0]
	bctx := tenantCtx(cTenantB)
	if _, err := e.rrepo.GetRun(bctx, run.RunID); !errors.Is(err, recon.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if _, err := e.rrepo.GetVariance(bctx, v.VarianceID); !errors.Is(err, recon.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if _, err := e.rrepo.RunEvents(bctx, run.RunID); !errors.Is(err, recon.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if l, _ := e.rrepo.ListRuns(bctx, recon.RunFilter{}); len(l) != 0 {
		t.Fatal("listed another tenant's runs")
	}
	if l, _ := e.rrepo.ListVariances(bctx, recon.VarianceFilter{}); len(l) != 0 {
		t.Fatal("listed another tenant's variances")
	}
	if _, err := e.rsvc.Resolve(bctx, rcReviewer, v.VarianceID, "DOMAIN_CORRECTED", "note text", 0); !errors.Is(err, recon.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if _, err := e.rsvc.SupplyDomainTotals(bctx, rcDomain, run.RunID, recon.DomainTotals{TotalAmount: "1", Currency: "GBP"}); !errors.Is(err, recon.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	// NP-52: another tenant's connection in a reconcile request is refused and raised as a boundary signal
	before := e.countAs(cTenantB, `SELECT count(*) FROM eventing_outbox WHERE event_type='com.zoikosuite.integration.security.tenant-scope-mismatch'`)
	if _, _, err := e.rsvc.RequestRun(bctx, rcMaker, e.req([]string{"STATE"}, nil)); !errors.Is(err, recon.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if n := e.countAs(cTenantB, `SELECT count(*) FROM eventing_outbox WHERE event_type='com.zoikosuite.integration.security.tenant-scope-mismatch'`); n != before+1 {
		t.Fatalf("%d signals", n)
	}
	for _, tbl := range []string{"xic_reconciliation_runs", "xic_reconciliation_variances", "xic_reconciliation_events"} {
		if n := e.countAs(cTenantB, `SELECT count(*) FROM `+tbl); n != 0 {
			t.Errorf("tenant B sees %d %s rows", n, tbl)
		}
		if n := e.countAs("", `SELECT count(*) FROM `+tbl); n != 0 {
			t.Errorf("%d %s rows visible with no tenant", n, tbl)
		}
	}
	e.wantRefused(t, "update", cTenantB, `UPDATE xic_reconciliation_variances SET case_ref='x', version=version+1`)
	wantDBError(t, "forged run", e.execAs(cTenantB, `INSERT INTO xic_reconciliation_runs (run_id, tenant_id, connection_id, connector_id, family, origin, period_from, period_to, basis, dedupe_key, requested_by)
		VALUES (gen_random_uuid(), $1, $2::uuid, 'bank-x', 'BANK_FEED', 'REQUESTED', now() - interval '2 days', now() - interval '1 day', '{STATE}', md5('x'), 'x')`, cTenantA, e.c.ConnectionID), "row-level security")
	// tenant B's own connection reconciles on its own
	rb, _, err := e.rsvc.RequestRun(bctx, rcMaker, recon.RunRequest{ConnectionID: cB.ConnectionID, PeriodFrom: e.now().Add(-24 * time.Hour), PeriodTo: e.now().Add(-time.Minute), Basis: []string{"STATE"}})
	if err != nil || rb.TenantID != cTenantB {
		t.Fatalf("%v %+v", err, rb)
	}
	// the worker flag reads across tenants but cannot change anything
	if err := e.execAsWorker(`UPDATE xic_reconciliation_runs SET close_note='x', version=version+1`); err == nil {
		t.Fatal("the worker role changed a run")
	}
	refs, err := e.rrepo.DueRuns(context.Background(), time.Now().UTC().Add(time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	var sawB bool
	for _, d := range refs {
		if d.TenantID == cTenantB && d.ID == rb.RunID {
			sawB = true
		}
	}
	if !sawB {
		t.Fatalf("%+v", refs)
	}
}

// ---- scheduling, signals and metrics ------------------------------------------------------------------------------

func TestReconPG_ScheduledRunsAreCreatedOncePerPeriodAndDrivenByTheWorker(t *testing.T) {
	e := newREnv(t)
	e.target(t)
	n, err := e.rsvc.Schedule(context.Background())
	if err != nil || n < 1 {
		t.Fatalf("%d %v", n, err)
	}
	if again, _ := e.rsvc.Schedule(context.Background()); again != 0 {
		t.Fatalf("%d", again)
	}
	runs, _ := e.rrepo.ListRuns(tenantCtx(cTenantA), recon.RunFilter{})
	if len(runs) != 1 || runs[0].Origin != recon.OriginScheduled || runs[0].Family == "" || runs[0].ExternalSource != "" {
		t.Fatalf("%+v", runs)
	}
	if d, err := e.rsvc.DriveDue(context.Background(), 10); err != nil || d < 1 {
		t.Fatalf("%d %v", d, err)
	}
	got, _ := e.rrepo.GetRun(tenantCtx(cTenantA), runs[0].RunID)
	if got.State != recon.RunMatched {
		t.Fatalf("%+v", got)
	}
	if ls, _ := e.rrepo.LastScheduledEnd(context.Background(), cTenantA, e.c.ConnectionID); !ls.Equal(got.PeriodTo) {
		t.Fatalf("%v %v", ls, got.PeriodTo)
	}
}

func TestSignalsPG_TenantSnapshotPlatformGaugesAndMetricsEndpoint(t *testing.T) {
	e := newREnv(t)
	e.target(t)
	o := e.deliver(e.c, "B-S1", 4)
	e.settle(t, e.process(t, o.Batch))
	e.attempt(t, uuidN(1), "UNKNOWN", 72*time.Hour, "", false)
	e.eval(t, e.run(t, e.req([]string{"COUNT"}, provPG(map[string]int{"received": 9}))))
	e.eval(t, e.run(t, e.req([]string{"COUNT"}, provPG(map[string]int{"received": 4}))))
	src := store.NewSignalsRepo(e.st)
	since := time.Now().UTC().Add(-24 * time.Hour)
	s, err := src.Snapshot(tenantCtx(cTenantA), since)
	if err != nil {
		t.Fatal(err)
	}
	if s.Connections.ByStatus["ACTIVE"] != 1 || s.Inbound.Total != 4 || s.Inbound.ByState["HANDED_OFF"] != 4 || s.Batch.ByState["RECONCILED"] != 1 || s.Batch.RowsByState["ACCEPTED"] != 4 ||
		s.Outbound.OldestAmbiguousSeconds < 3600 || s.Reconciliation.OpenVariances != 1 || s.Reconciliation.RunsByState["MATCHED"] != 1 ||
		s.Reconciliation.RunsByState["VARIANCE"] != 1 || s.Reconciliation.MatchRate != 0.5 || s.Reconciliation.OpenBySeverity["LOW"]+s.Reconciliation.OpenBySeverity["MEDIUM"]+s.Reconciliation.OpenBySeverity["HIGH"] != 1 {
		t.Fatalf("%+v", s)
	}
	// another tenant's snapshot counts none of it
	sb, err := src.Snapshot(tenantCtx(cTenantB), since)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Inbound.Total != 0 || sb.Batch.ByState["RECONCILED"] != 0 || sb.Reconciliation.OpenVariances != 0 || sb.Outbound.OldestAmbiguousSeconds != 0 || len(sb.Connections.ByStatus) != 0 {
		t.Fatalf("%+v", sb)
	}
	if s.Security.Events == nil {
		t.Fatal("security events should be an object")
	}
	// the platform view spans tenants and names none
	ps, err := src.Platform(context.Background(), since)
	if err != nil {
		t.Fatal(err)
	}
	if ps.Inbound.Total != 4 || ps.Reconciliation.OpenVariances != 1 {
		t.Fatalf("%+v", ps)
	}
	m := signals.NewMetrics()
	m.Hook("batch")("batch_received", "HTTPS")
	m.Hook("batch")("batch_received", "HTTPS")
	m.Hook("inbound")("auth failed\"{x}", "R\n1")
	h := handler.NewMetrics(m, src, "scrape-token", time.Hour, zap.NewNop())
	get := func(tok string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/metrics", nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	if w := get(""); w.Code != 401 {
		t.Fatalf("%d", w.Code)
	}
	if w := get("wrong"); w.Code != 401 {
		t.Fatalf("%d", w.Code)
	}
	w := get("scrape-token")
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `xic_events_total{service="batch",kind="batch_received",reason="HTTPS"} 2`) || !strings.Contains(body, `xic_reconciliation_open_variances{severity=`) ||
		!strings.Contains(body, `xic_inbound_exchanges{state="HANDED_OFF"} 4`) || !strings.Contains(body, "xic_outbound_oldest_ambiguous_seconds") {
		t.Fatalf("%d\n%s", w.Code, body)
	}
	for _, never := range []string{cTenantA, cTenantB, e.c.ConnectionID, "\n1", `{x}`} {
		if strings.Contains(body, never) {
			t.Errorf("the metrics carry %q", never)
		}
	}
	// a handler built with no token never answers
	if w2 := func() *httptest.ResponseRecorder {
		hh := handler.NewMetrics(m, src, "", time.Hour, zap.NewNop())
		rr := httptest.NewRecorder()
		hh.ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", bytes.NewReader(nil)))
		return rr
	}(); w2.Code != 401 {
		t.Fatalf("%d", w2.Code)
	}
}
