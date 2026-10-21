//go:build integration

// Real-Postgres tests for AR-09, driving the REAL AR-07 module as the receivable source of truth. Run (serially):
//
//	go test -tags=integration -count=1 -p 1 -timeout=600s ./internal/ar09/...
//
// The embedded superuser bypasses RLS, so isolation tests prove the explicit tenant_id predicates, not the policies.
// Spec section 18 scenarios carry their number: TestNeg27 (dunning while dispute/hold), TestNeg28 (promise history
// overwritten), TestNeg29 (collector self-approves write-off: there is no such authority).
package ar09

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"zoiko.io/accounts-receivable-svc/internal/ar07"
	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/kit/kittest"
	svcmiddleware "zoiko.io/accounts-receivable-svc/internal/middleware"
)

var pool *pgxpool.Pool

func TestMain(m *testing.M) {
	var stop func()
	pool, stop = kittest.Start("ar09_test", 18100)
	code := m.Run()
	stop()
	os.Exit(code)
}

type env struct {
	t      *testing.T
	m      *Module
	h      http.Handler
	a7     *ar07.Module
	a7h    http.Handler
	tenant string
	entity string
	party  string
	clock  *testClock
	del    *FakeDeliverer
	prof   *fakeProfiles
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, tenant: uuid.NewString(), entity: uuid.NewString(), party: uuid.NewString(), clock: &testClock{t: testNow},
		del: &FakeDeliverer{}, prof: &fakeProfiles{}}
	deps := testDeps(&fakeAuthz{grants: standardGrants()}, e.clock.Now)
	deps.DB = kit.NewDB(pool)
	e.a7 = ar07.New(deps)
	e.m = New(deps, e.a7, e.prof, e.del)
	e.h = testRouter(e.m)
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	e.a7.Register(r)
	e.a7h = r
	return e
}

func (e *env) caller(principal string) kit.Caller {
	return kit.Caller{TenantID: e.tenant, PrincipalID: principal, CorrelationID: "corr-" + uuid.NewString(), LegalEntityID: e.entity}
}

func (e *env) tx(fn func(tx pgx.Tx) error) error {
	return e.m.deps.DB.WithTx(context.Background(), e.tenant, fn)
}

func (e *env) cmd(principal, name string, body map[string]any) (int, []byte) {
	if _, ok := body["legal_entity_id"]; !ok {
		body["legal_entity_id"] = e.entity
	}
	return call(e.t, e.h, http.MethodPost, base+"commands/"+name, e.tenant, principal, "", body)
}

func (e *env) ok(principal, name string, body map[string]any) map[string]any {
	e.t.Helper()
	code, raw := e.cmd(principal, name, body)
	require.Contains(e.t, []int{200, 201}, code, string(raw))
	return decode(e.t, raw)
}

func decode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m), string(raw))
	return m
}

func (e *env) get(name, query string) (int, []byte) {
	return call(e.t, e.h, http.MethodGet, base+name+"?"+query, e.tenant, "reader", "", nil)
}

func (e *env) publish(mut func(*Policy)) {
	e.t.Helper()
	p := testPolicy()
	if mut != nil {
		mut(&p)
	}
	code, raw := call(e.t, e.h, http.MethodPost, base+"commands/publishDunningPolicy", e.tenant, pPolicy, "", map[string]any{
		"legal_entity_id": e.entity, "dunning_profile": p.DunningProfile, "effective_from": p.EffectiveFrom, "stages": p.Stages,
		"blackout_weekdays": p.BlackoutWeekdays, "blackout_dates": p.BlackoutDates, "max_notices_per_window": p.MaxNoticesPerWindow,
		"window_days": p.WindowDays, "promise_grace_days": p.PromiseGraceDays})
	require.Equal(e.t, http.StatusCreated, code, string(raw))
}

// item creates an AR-07 open item for the env's customer (as AR-04 would inside the issuing tx).
func (e *env) item(amount, due string) string {
	e.t.Helper()
	d, _ := time.Parse("2006-01-02", due)
	var ref kit.OpenItemRef
	require.NoError(e.t, e.tx(func(tx pgx.Tx) (err error) {
		ref, err = e.a7.CreateFromDocument(context.Background(), tx, e.caller("seed"), kit.OpenItemFromDocument{DocumentKind: kit.DocInvoice,
			SourceDocumentID: uuid.NewString(), DocumentNumber: "INV-" + uuid.NewString()[:6], CustomerPartyID: e.party, Currency: "USD",
			OriginalAmount: kit.MustAmount(amount), IssueDate: d.AddDate(0, 0, -30), DueDate: d})
		return
	}))
	return ref.OpenItemID
}

func (e *env) a7adjust(itemID, kind, amount string) *kit.OpenItemView {
	e.t.Helper()
	var v *kit.OpenItemView
	require.NoError(e.t, e.tx(func(tx pgx.Tx) (err error) {
		v, err = e.a7.ApplyAdjustment(context.Background(), tx, e.caller("seed"), kit.OpenItemAdjustment{OpenItemID: itemID, Kind: kind,
			Amount: kit.MustAmount(amount), SourceRefType: kind, SourceRefID: "ref-" + uuid.NewString(), EventKey: uuid.NewString(), Reason: "test"})
		return
	}))
	return v
}

func (e *env) view(itemID string) *kit.OpenItemView {
	e.t.Helper()
	v, err := e.a7.GetByID(context.Background(), e.tenant, itemID)
	require.NoError(e.t, err)
	return v
}

func (e *env) a7cmd(name string, body map[string]any) {
	e.t.Helper()
	body["legal_entity_id"] = e.entity
	code, raw := call(e.t, e.a7h, http.MethodPost, "/internal/v1/ar07/commands/"+name, e.tenant, "ar07dispute", "", body)
	require.Equal(e.t, http.StatusOK, code, string(raw))
}

func (e *env) dispute(itemID string) {
	e.a7cmd("openDispute", map[string]any{"open_item_id": itemID, "expected_version": e.view(itemID).Version, "reason_code": "WRONG_AMOUNT", "evidence_id": "ev-1"})
}

func (e *env) resolveDispute(itemID string) {
	e.a7cmd("resolveDispute", map[string]any{"open_item_id": itemID, "expected_version": e.view(itemID).Version, "resolution_code": "REJECTED", "evidence_id": "ev-2"})
}

func (e *env) a7hold(itemID string) {
	e.a7cmd("placeCollectionHold", map[string]any{"open_item_id": itemID, "expected_version": e.view(itemID).Version, "reason_code": "CUSTOMER_REQUEST"})
}

// setup publishes the policy, creates one 500 USD item due 2026-10-06 (6 days past due on testNow) and opens the case.
func setup(t *testing.T, mut func(*Policy)) (*env, string, string) {
	t.Helper()
	e := newEnv(t)
	e.publish(mut)
	item := e.item("500", "2026-10-06")
	out := e.ok(pCollector, "openCollectionCase", map[string]any{"customer_party_id": e.party, "currency": "USD"})
	return e, out["case_id"].(string), item
}

func (e *env) ver(caseID string) int {
	e.t.Helper()
	code, raw := e.get("getCollectionCase", "case_id="+caseID)
	require.Equal(e.t, http.StatusOK, code, string(raw))
	return int(decode(e.t, raw)["case"].(map[string]any)["version"].(float64))
}

func (e *env) caseOf(caseID string) map[string]any {
	e.t.Helper()
	code, raw := e.get("getCollectionCase", "case_id="+caseID)
	require.Equal(e.t, http.StatusOK, code, string(raw))
	return decode(e.t, raw)["case"].(map[string]any)
}

func (e *env) advance(caseID string) (int, []byte) {
	return e.cmd(pCollector, "advanceDunningStage", map[string]any{"case_id": caseID, "expected_version": e.ver(caseID)})
}

func (e *env) send(caseID string) (int, []byte) {
	return e.cmd(pCollector, "sendDunningNotice", map[string]any{"case_id": caseID, "expected_version": e.ver(caseID)})
}

func count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}

func (e *env) actions(caseID, result string) int {
	return count(e.t, `SELECT count(*) FROM ar09_dunning_actions WHERE tenant_id = $1 AND case_id = $2 AND delivery_result = $3`, e.tenant, caseID, result)
}

func (e *env) events(caseID, typ string) int {
	return count(e.t, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND aggregate_id = $2 AND event_type = $3`, e.tenant, caseID, typ)
}

func (e *env) lastSuppression(caseID string) string {
	var s string
	require.NoError(e.t, pool.QueryRow(context.Background(), `SELECT COALESCE((SELECT suppression_reason FROM ar09_dunning_actions WHERE tenant_id = $1 AND case_id = $2
		AND delivery_result = 'SUPPRESSED' ORDER BY seq DESC LIMIT 1),'')`, e.tenant, caseID).Scan(&s))
	return s
}

func (e *env) runDue(at time.Time) int {
	e.t.Helper()
	n, err := e.m.runDue(context.Background(), at, e.tenant)
	require.NoError(e.t, err)
	return n
}

// ── open / advance / send ────────────────────────────────────────────────────

func TestOpenAdvanceSendHappyPath(t *testing.T) {
	e, cid, item := setup(t, nil)
	cs := e.caseOf(cid)
	require.Equal(t, "ELIGIBLE", cs["state"])
	require.Equal(t, float64(1), cs["version"])
	snap := cs["aging_snapshot"].(map[string]any)
	require.Equal(t, "500.0000", snap["total_residual"])
	require.Equal(t, 1, e.events(cid, EvCaseOpened))
	require.Equal(t, 1, count(t, `SELECT count(*) FROM ar09_case_items WHERE tenant_id = $1 AND case_id = $2 AND open_item_id = $3`, e.tenant, cid, item))

	// one live case per customer/currency
	code, raw := e.cmd(pCollector, "openCollectionCase", map[string]any{"customer_party_id": e.party, "currency": "USD"})
	require.Equal(t, 409, code, string(raw))

	// stage 1 is due (6 >= 5); stage 2 is not.
	code, raw = e.advance(cid)
	require.Equal(t, 200, code, string(raw))
	require.Equal(t, "ACTIVE", e.caseOf(cid)["state"])
	require.Equal(t, float64(1), e.caseOf(cid)["dunning_stage"])
	code, raw = e.advance(cid)
	require.Equal(t, 409, code, "no further stage due: "+string(raw))
	require.Equal(t, kit.CodeInvalidTransition, errCode(t, raw))

	code, raw = e.send(cid)
	require.Equal(t, 200, code, string(raw))
	act := decode(t, raw)["action"].(map[string]any)
	require.Equal(t, "SENT", act["delivery_result"])
	require.Len(t, act["notice_artifact_hash"], 64)
	require.NotNil(t, act["aging_snapshot"])
	require.Len(t, e.del.Sent(), 1)
	require.Equal(t, 1, e.events(cid, EvNoticeSent))

	// one notice per stage
	code, raw = e.send(cid)
	require.Equal(t, 409, code, string(raw))
	require.Len(t, e.del.Sent(), 1)

	// stage / version enforcement
	code, raw = e.cmd(pCollector, "sendDunningNotice", map[string]any{"case_id": cid, "expected_version": 1})
	require.Equal(t, 409, code)
	require.Equal(t, kit.CodeStaleVersion, errCode(t, raw))
	code, raw = e.cmd(pCollector, "sendDunningNotice", map[string]any{"case_id": cid})
	require.Equal(t, 400, code)
	require.Equal(t, kit.CodeExpectedVersionMissing, errCode(t, raw))

	// receivable truth untouched by all of the above
	require.Equal(t, kit.MustAmount("500"), e.view(item).Residual)
	require.Equal(t, 0, count(t, `SELECT count(*) FROM ar07_adjustments WHERE tenant_id = $1`, e.tenant))
	require.Equal(t, 0, count(t, `SELECT count(*) FROM ar_accounting_posting_requests WHERE tenant_id = $1`, e.tenant))
	require.GreaterOrEqual(t, count(t, `SELECT count(*) FROM ar_audit_events WHERE tenant_id = $1 AND aggregate_id = $2`, e.tenant, cid), 3)

	// queries
	code, raw = e.get("getDunningSchedule", "case_id="+cid)
	require.Equal(t, 200, code, string(raw))
	require.Len(t, decode(t, raw)["stages"], 3)
	code, raw = e.get("getCustomerCollectionHistory", "customer_party_id="+e.party)
	require.Equal(t, 200, code, string(raw))
	require.Len(t, decode(t, raw)["cases"], 1)
	require.Len(t, decode(t, raw)["actions"], 1)
}

func TestOpenCaseValidation(t *testing.T) {
	e := newEnv(t)
	e.publish(nil)
	// no overdue receivable yet
	e.item("100", "2026-12-01")
	code, raw := e.cmd(pCollector, "openCollectionCase", map[string]any{"customer_party_id": e.party, "currency": "USD"})
	require.Equal(t, 422, code, string(raw))
	// a not-overdue item cannot be named
	cur := e.item("100", "2026-12-02")
	code, _ = e.cmd(pCollector, "openCollectionCase", map[string]any{"customer_party_id": e.party, "currency": "USD", "open_item_ids": []string{cur}})
	require.Equal(t, 422, code)
	// profile inactive blocks opening
	e.item("100", "2026-10-01")
	e.prof.setInactive(true)
	code, raw = e.cmd(pCollector, "openCollectionCase", map[string]any{"customer_party_id": e.party, "currency": "USD"})
	require.Equal(t, 409, code, string(raw))
	require.Equal(t, kit.CodeCustomerProfileInactive, errCode(t, raw))
	// no policy for the profile
	e.prof.setInactive(false)
	e.prof.profile = "UNKNOWN"
	code, _ = e.cmd(pCollector, "openCollectionCase", map[string]any{"customer_party_id": e.party, "currency": "USD"})
	require.Equal(t, 422, code)
}

func TestOpenCaseIdempotentReplayAndAtomicity(t *testing.T) {
	e := newEnv(t)
	e.publish(nil)
	e.item("500", "2026-10-06")
	body := map[string]any{"legal_entity_id": e.entity, "customer_party_id": e.party, "currency": "USD"}
	key := uuid.NewString()
	code1, raw1 := call(t, e.h, http.MethodPost, base+"commands/openCollectionCase", e.tenant, pCollector, key, body)
	code2, raw2 := call(t, e.h, http.MethodPost, base+"commands/openCollectionCase", e.tenant, pCollector, key, body)
	require.Equal(t, 201, code1)
	require.Equal(t, 201, code2)
	require.JSONEq(t, string(raw1), string(raw2))
	require.Equal(t, 1, count(t, `SELECT count(*) FROM ar09_collection_cases WHERE tenant_id = $1`, e.tenant))
	// a failed open (no policy) leaves no case, history or outbox row
	e2 := newEnv(t)
	e2.item("500", "2026-10-06")
	code, _ := e2.cmd(pCollector, "openCollectionCase", map[string]any{"customer_party_id": e2.party, "currency": "USD"})
	require.Equal(t, 422, code)
	require.Equal(t, 0, count(t, `SELECT count(*) FROM ar09_collection_cases WHERE tenant_id = $1`, e2.tenant))
	require.Equal(t, 0, count(t, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type LIKE 'ar.collection.%'`, e2.tenant))
}

func TestPolicyVersionsAreImmutableAndOrdered(t *testing.T) {
	e := newEnv(t)
	e.publish(nil)
	// the same effective date again overlaps the previous version
	code, raw := call(t, e.h, http.MethodPost, base+"commands/publishDunningPolicy", e.tenant, pPolicy, "", map[string]any{
		"legal_entity_id": e.entity, "dunning_profile": "STD", "effective_from": "2026-01-01", "stages": testPolicy().Stages})
	require.Equal(t, 422, code, string(raw))
	e.publish(func(p *Policy) { p.EffectiveFrom = "2026-06-01"; p.Stages = p.Stages[:1] })
	require.Equal(t, 2, count(t, `SELECT max(version) FROM ar09_dunning_policies WHERE tenant_id = $1`, e.tenant))
	_, err := pool.Exec(context.Background(), `UPDATE ar09_dunning_policies SET promise_grace_days = 9 WHERE tenant_id = $1`, e.tenant)
	require.Error(t, err)
	_, err = pool.Exec(context.Background(), `DELETE FROM ar09_dunning_policies WHERE tenant_id = $1`, e.tenant)
	require.Error(t, err)
}

// ── #27 dunning during dispute / hold ────────────────────────────────────────

func TestNeg27_DunningSuppressedWhileDisputeOrHold(t *testing.T) {
	type variant struct {
		name   string
		mut    func(*Policy)
		cond   func(e *env, cid, item string)
		reason string
	}
	variants := []variant{
		{"AR-07 dispute", nil, func(e *env, _, item string) { e.dispute(item) }, SuppItemDisputeOrHold},
		{"AR-07 item collection hold", nil, func(e *env, _, item string) { e.a7hold(item) }, SuppItemDisputeOrHold},
		{"AR-09 case hold", nil, func(e *env, cid, _ string) {
			e.ok(pHold, "placeCollectionHold", map[string]any{"case_id": cid, "expected_version": e.ver(cid), "reason_code": "CUSTOMER_REQUEST"})
		}, SuppCaseHold},
		{"customer profile inactive", nil, func(e *env, _, _ string) { e.prof.setInactive(true) }, SuppProfileInactive},
		{"promise-to-pay grace", nil, func(e *env, cid, _ string) {
			e.ok(pCollector, "recordPromiseToPay", map[string]any{"case_id": cid, "expected_version": e.ver(cid), "promised_date": "2026-10-20", "promised_amount": "100"})
		}, SuppPromiseGrace},
		{"contact blackout", func(p *Policy) { p.BlackoutWeekdays = []int{2} }, func(e *env, _, _ string) { e.clock.Set(testNow.AddDate(0, 0, 1)) }, SuppContactBlackout},
	}
	for _, v := range variants {
		for _, mode := range []string{"advance", "send"} {
			t.Run(v.name+"/"+mode, func(t *testing.T) {
				e, cid, item := setup(t, v.mut)
				if mode == "send" {
					code, raw := e.advance(cid)
					require.Equal(t, 200, code, string(raw))
				}
				v.cond(e, cid, item)
				var code int
				var raw []byte
				if mode == "advance" {
					code, raw = e.advance(cid)
				} else {
					code, raw = e.send(cid)
				}
				require.Equal(t, http.StatusConflict, code, string(raw))
				require.Equal(t, kit.CodeCollectionHoldActive, errCode(t, raw))
				require.Empty(t, e.del.Sent(), "no notice may reach the customer")
				require.Equal(t, 0, e.actions(cid, ResultSent)+e.actions(cid, ResultDelivered)+e.actions(cid, ResultFailed))
				require.Equal(t, v.reason, e.lastSuppression(cid), "the refusal is recorded as evidence")
				require.Equal(t, 1, e.actions(cid, ResultSuppressed))
				// the same refusal is not re-recorded on every retry
				if mode == "advance" {
					e.advance(cid)
				} else {
					e.send(cid)
				}
				require.Equal(t, 1, e.actions(cid, ResultSuppressed))
				// the receivable is untouched
				require.Equal(t, kit.MustAmount("500"), e.view(item).Residual)
				require.Equal(t, 0, count(t, `SELECT count(*) FROM ar07_adjustments WHERE tenant_id = $1`, e.tenant))
			})
		}
	}
}

func TestNeg27_SchedulerObeysDisputeAndResumesAfter(t *testing.T) {
	e, cid, item := setup(t, nil)
	e.dispute(item)
	require.Equal(t, 1, e.runDue(testNow), "state synced from the AR-07 dispute")
	require.Equal(t, "DISPUTED", e.caseOf(cid)["state"])
	require.Empty(t, e.del.Sent())
	require.Equal(t, 0, e.actions(cid, ResultSent))
	require.Equal(t, SuppItemDisputeOrHold, e.lastSuppression(cid))
	e.runDue(testNow) // idempotent: nothing new
	require.Equal(t, 1, e.actions(cid, ResultSuppressed))

	e.resolveDispute(item)
	require.Equal(t, 1, e.actions(cid, ResultSuppressed))
	e.runDue(testNow)
	require.Equal(t, "ACTIVE", e.caseOf(cid)["state"])
	require.Len(t, e.del.Sent(), 1, "once the dispute is cleared the due stage is sent")
}

func TestNeg27_ContactPolicyRateLimit(t *testing.T) {
	e, cid, _ := setup(t, func(p *Policy) { p.MaxNoticesPerWindow, p.WindowDays = 1, 14 })
	require.Equal(t, 1, e.runDue(testNow)) // stage 1 advance + send
	require.Len(t, e.del.Sent(), 1)
	// dpd 17: stage 2 is due, but the customer already received one notice inside the 14-day window
	later := testNow.AddDate(0, 0, 11)
	e.clock.Set(later)
	e.runDue(later)
	require.Len(t, e.del.Sent(), 1)
	require.Equal(t, SuppRateLimited, e.lastSuppression(cid))
	require.Equal(t, float64(1), e.caseOf(cid)["dunning_stage"])
	// a command is refused the same way
	code, raw := e.advance(cid)
	require.Equal(t, 409, code)
	require.Equal(t, kit.CodeCollectionHoldActive, errCode(t, raw))
	// window passes: allowed again
	e.clock.Set(testNow.AddDate(0, 0, 19))
	e.runDue(testNow.AddDate(0, 0, 19))
	require.Len(t, e.del.Sent(), 2)
	require.Equal(t, float64(2), e.caseOf(cid)["dunning_stage"])
}

// ── scheduler ────────────────────────────────────────────────────────────────

func TestRunDueIdempotentPerStageAndConcurrent(t *testing.T) {
	e, cid, _ := setup(t, nil)
	var wg sync.WaitGroup
	var mu sync.Mutex
	total := 0
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := e.m.runDue(context.Background(), testNow, e.tenant)
			require.NoError(t, err)
			mu.Lock()
			total += n
			mu.Unlock()
		}()
	}
	wg.Wait()
	require.GreaterOrEqual(t, total, 1)
	require.Len(t, e.del.Sent(), 1, "concurrent runs produce ONE notice for the stage")
	require.Equal(t, 1, e.actions(cid, ResultSent))
	// replay: nothing more
	require.Equal(t, 0, e.runDue(testNow))
	require.Len(t, e.del.Sent(), 1)
	// next stage, once, however many times it runs
	later := testNow.AddDate(0, 0, 10) // 16 dpd
	e.clock.Set(later)
	e.runDue(later)
	e.runDue(later)
	require.Len(t, e.del.Sent(), 2)
	require.Equal(t, float64(2), e.caseOf(cid)["dunning_stage"])
	require.Equal(t, 2, count(t, `SELECT count(*) FROM ar09_dunning_actions WHERE tenant_id = $1 AND case_id = $2 AND delivery_result = 'SENT'`, e.tenant, cid))
	// the unique index is the backstop: a second SENT row for a stage is refused by the database
	_, err := pool.Exec(context.Background(), `INSERT INTO ar09_dunning_actions (action_id, tenant_id, legal_entity_id, case_id, customer_party_id, stage_no, channel, template_key,
		delivery_result, notice_artifact_hash, actor_id) SELECT gen_random_uuid(), tenant_id, legal_entity_id, case_id, customer_party_id, 1, 'EMAIL', 'DUN1', 'SENT',
		repeat('a',64), 'x' FROM ar09_collection_cases WHERE case_id = $1`, cid)
	require.Error(t, err)
}

func TestRunDueLapsedPromiseReturnsToActive(t *testing.T) {
	e, cid, _ := setup(t, nil)
	e.ok(pCollector, "recordPromiseToPay", map[string]any{"case_id": cid, "expected_version": e.ver(cid), "promised_date": "2026-10-14", "promised_amount": "500"})
	require.Equal(t, 0, e.runDue(testNow), "inside the promise: nothing happens")
	require.Equal(t, "PROMISE_TO_PAY", e.caseOf(cid)["state"])
	late := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC) // promise 14 + 2 grace = 16 < 20
	e.clock.Set(late)
	require.GreaterOrEqual(t, e.runDue(late), 1)
	require.Equal(t, "ACTIVE", e.caseOf(cid)["state"])
	require.Len(t, e.del.Sent(), 1)
}

// ── #28 promise history ──────────────────────────────────────────────────────

func TestNeg28_PromiseHistoryAppendOnly(t *testing.T) {
	e, cid, _ := setup(t, nil)
	r1 := e.ok(pCollector, "recordPromiseToPay", map[string]any{"case_id": cid, "expected_version": e.ver(cid), "promised_date": "2026-10-20", "promised_amount": "200"})
	p1 := r1["promise"].(map[string]any)
	require.Equal(t, float64(1), p1["version_no"])
	require.Equal(t, "PROMISE_TO_PAY", e.caseOf(cid)["state"])

	// a changed promise without a reason is refused
	code, raw := e.cmd(pCollector, "recordPromiseToPay", map[string]any{"case_id": cid, "expected_version": e.ver(cid), "promised_date": "2026-10-25", "promised_amount": "200"})
	require.Equal(t, 400, code, string(raw))
	require.Equal(t, kit.CodeReasonRequired, errCode(t, raw))
	// unchanged is refused
	code, _ = e.cmd(pCollector, "recordPromiseToPay", map[string]any{"case_id": cid, "expected_version": e.ver(cid), "promised_date": "2026-10-20", "promised_amount": "200", "reason_code": "X"})
	require.Equal(t, 409, code)
	// a promise cannot be in the past or exceed what AR-07 shows outstanding
	code, _ = e.cmd(pCollector, "recordPromiseToPay", map[string]any{"case_id": cid, "expected_version": e.ver(cid), "promised_date": "2026-10-01", "promised_amount": "200", "reason_code": "X"})
	require.Equal(t, 422, code)
	code, _ = e.cmd(pCollector, "recordPromiseToPay", map[string]any{"case_id": cid, "expected_version": e.ver(cid), "promised_date": "2026-10-25", "promised_amount": "900", "reason_code": "X"})
	require.Equal(t, 422, code)

	// with a reason: a NEW version superseding the old one; the old row is untouched
	r2 := e.ok(pCollector, "recordPromiseToPay", map[string]any{"case_id": cid, "expected_version": e.ver(cid), "promised_date": "2026-10-25", "promised_amount": "250", "reason_code": "CUSTOMER_REQUEST"})
	p2 := r2["promise"].(map[string]any)
	require.Equal(t, float64(2), p2["version_no"])
	require.Equal(t, p1["promise_id"], p2["supersedes"])
	require.Equal(t, 2, count(t, `SELECT count(*) FROM ar09_promises WHERE tenant_id = $1 AND case_id = $2`, e.tenant, cid))
	var d string
	var amt kit.Amount
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT to_char(promised_date,'YYYY-MM-DD'), promised_amount FROM ar09_promises WHERE promise_id = $1`, p1["promise_id"]).Scan(&d, &amt))
	require.Equal(t, "2026-10-20", d)
	require.Equal(t, kit.MustAmount("200"), amt)
	require.Equal(t, 2, e.events(cid, EvPromiseRecorded))

	// the database refuses every way of rewriting history
	ctx := context.Background()
	_, err := pool.Exec(ctx, `UPDATE ar09_promises SET promised_date = '2026-12-31' WHERE promise_id = $1`, p1["promise_id"])
	require.Error(t, err, "UPDATE in place")
	_, err = pool.Exec(ctx, `DELETE FROM ar09_promises WHERE promise_id = $1`, p1["promise_id"])
	require.Error(t, err, "DELETE")
	ins := `INSERT INTO ar09_promises (promise_id, tenant_id, legal_entity_id, case_id, version_no, promised_date, promised_amount, currency, supersedes, reason_code, recorded_by)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, '2026-11-01', 10, 'USD', $5, $6, 'x')`
	_, err = pool.Exec(ctx, ins, e.tenant, e.entity, cid, 3, nil, "R")
	require.Error(t, err, "a changed promise that supersedes nothing")
	_, err = pool.Exec(ctx, ins, e.tenant, e.entity, cid, 3, p1["promise_id"], "R")
	require.Error(t, err, "superseding a version that was already superseded")
	_, err = pool.Exec(ctx, ins, e.tenant, e.entity, cid, 3, p2["promise_id"], nil)
	require.Error(t, err, "a new version without a reason")
	_, err = pool.Exec(ctx, ins, e.tenant, e.entity, cid, 5, p2["promise_id"], "R")
	require.Error(t, err, "skipping a version number")
	_, err = pool.Exec(ctx, ins, e.tenant, e.entity, cid, 3, p2["promise_id"], "R")
	require.NoError(t, err, "the legitimate next version is accepted")

	// GetPromisesDue shows only the latest version of each live case, with the AR-07 outstanding read live
	code, raw = e.get("getPromisesDue", "as_of=2026-12-31")
	require.Equal(t, 200, code, string(raw))
	rows := decode(t, raw)["promises"].([]any)
	require.Len(t, rows, 1)
	require.Equal(t, "500.0000", rows[0].(map[string]any)["outstanding_now"])
	code, raw = e.get("getPromisesDue", "as_of=2026-10-01")
	require.Len(t, decode(t, raw)["promises"], 0)
}

// ── #29 collector has no write-off authority ─────────────────────────────────

func TestNeg29_CollectorCannotWriteOffOrApprove(t *testing.T) {
	e, cid, item := setup(t, nil)
	// no route in AR-09 for any financial action
	for _, name := range []string{"approveWriteOff", "requestWriteOff", "writeOff", "applyReceivableAdjustment", "applyCash", "issueRefund", "createCreditNote"} {
		code, _ := e.cmd(pCollector, name, map[string]any{"case_id": cid, "open_item_id": item})
		require.Contains(t, []int{404, 405}, code, name)
	}
	// the collector's grants do not let it touch AR-07's adjustment / write-off routes either
	code, raw := call(t, e.a7h, http.MethodPost, "/internal/v1/ar07/commands/applyReceivableAdjustment", e.tenant, pCollector, "", map[string]any{
		"legal_entity_id": e.entity, "open_item_id": item, "expected_version": 1, "kind": "WRITE_OFF", "amount": "500", "reason_code": "X", "evidence_id": "e",
		"owning_document_type": "WRITE_OFF", "owning_document_id": "w1"})
	require.Equal(t, 403, code, string(raw))
	// a "WRITE_OFF" outcome smuggled into the close command is not an outcome
	code, raw = e.cmd(pManager, "closeCollectionCase", map[string]any{"case_id": cid, "expected_version": e.ver(cid), "outcome": "WRITTEN_OFF", "reason_code": "X"})
	require.Equal(t, 400, code, string(raw))
	// the owner cannot walk away from collectable debt on their own
	code, raw = e.cmd(pCollector, "closeCollectionCase", map[string]any{"case_id": cid, "expected_version": e.ver(cid), "outcome": "ABANDONED", "reason_code": "UNCOLLECTABLE"})
	require.Equal(t, 403, code, string(raw))
	require.Equal(t, kit.CodeSegregationOfDuties, errCode(t, raw))
	require.Equal(t, "ELIGIBLE", e.caseOf(cid)["state"])
	// another manager may close administratively - still no balance effect
	e.ok(pManager, "closeCollectionCase", map[string]any{"case_id": cid, "expected_version": e.ver(cid), "outcome": "ABANDONED", "reason_code": "UNCOLLECTABLE", "reason": "agency recall"})
	require.Equal(t, "CLOSED", e.caseOf(cid)["state"])
	v := e.view(item)
	require.Equal(t, kit.MustAmount("500"), v.Residual)
	require.Equal(t, "OPEN", v.State, "closing a collection case does not touch the receivable")
	require.Equal(t, 0, count(t, `SELECT count(*) FROM ar07_adjustments WHERE tenant_id = $1`, e.tenant))
	// a closed case is frozen
	_, err := pool.Exec(context.Background(), `UPDATE ar09_collection_cases SET state = 'ACTIVE' WHERE case_id = $1`, cid)
	require.Error(t, err)
}

func TestEscalationRecordsReferralOnly(t *testing.T) {
	e, cid, item := setup(t, nil)
	body := func(extra map[string]any) map[string]any {
		b := map[string]any{"case_id": cid, "expected_version": e.ver(cid), "reason_code": "THRESHOLD", "referral_reason": "refer to legal workflow"}
		for k, v := range extra {
			b[k] = v
		}
		return b
	}
	code, _ := e.cmd(pEscalate, "escalateCollectionCase", body(nil))
	require.Equal(t, 409, code, "before dunning started")
	_, raw := e.advance(cid)
	_ = raw
	code, raw = e.cmd(pCollector, "escalateCollectionCase", body(nil))
	require.Equal(t, 403, code, "collector lacks the escalate permission: "+string(raw))
	// stage 1 has no threshold: escalation allowed from ACTIVE
	out := e.ok(pEscalate, "escalateCollectionCase", body(nil))
	require.Equal(t, "ESCALATED", out["state"])
	require.Equal(t, "refer to legal workflow", out["referral_reason"])
	require.Equal(t, 1, e.events(cid, EvEscalated))
	require.Equal(t, kit.MustAmount("500"), e.view(item).Residual)
	require.Equal(t, 0, count(t, `SELECT count(*) FROM ar_accounting_posting_requests WHERE tenant_id = $1`, e.tenant))
	// an escalated case is not dunned further
	code, _ = e.send(cid)
	require.Equal(t, 409, code)
	require.Empty(t, e.del.Sent())
}

func TestEscalationThresholdAndDisputeBlock(t *testing.T) {
	e := newEnv(t)
	e.publish(func(p *Policy) { p.Stages[0].EscalationThreshold = kit.MustAmount("1000") })
	item := e.item("500", "2026-10-06")
	cid := e.ok(pCollector, "openCollectionCase", map[string]any{"customer_party_id": e.party, "currency": "USD"})["case_id"].(string)
	e.advance(cid)
	b := map[string]any{"case_id": cid, "expected_version": e.ver(cid), "reason_code": "T", "referral_reason": "x"}
	code, raw := e.cmd(pEscalate, "escalateCollectionCase", b)
	require.Equal(t, 422, code, "below the stage's escalation threshold: "+string(raw))
	e2, cid2, item2 := setup(t, nil)
	e2.advance(cid2)
	e2.dispute(item2)
	code, raw = e2.cmd(pEscalate, "escalateCollectionCase", map[string]any{"case_id": cid2, "expected_version": e2.ver(cid2), "reason_code": "T", "referral_reason": "x"})
	require.Equal(t, 409, code, string(raw))
	require.Equal(t, kit.CodeCollectionHoldActive, errCode(t, raw))
	_ = item
}

// ── delivery ─────────────────────────────────────────────────────────────────

func TestDeliveryFailureLeavesBalancesUnchanged(t *testing.T) {
	e, cid, item := setup(t, nil)
	e.del.SetErr(ErrDeliveryNotConfigured)
	e.advance(cid)
	code, raw := e.send(cid)
	require.Equal(t, 200, code, string(raw))
	act := decode(t, raw)["action"].(map[string]any)
	require.Equal(t, "DELIVERY_FAILED", act["delivery_result"])
	require.NotEmpty(t, act["failure_detail"])
	require.Len(t, act["notice_artifact_hash"], 64, "the notice artifact is recorded even when delivery fails")
	require.Equal(t, 1, e.actions(cid, ResultFailed))
	require.Equal(t, 0, e.events(cid, EvNoticeSent), "no 'sent' event for a failed delivery")
	require.Equal(t, kit.MustAmount("500"), e.view(item).Residual)
	require.Equal(t, 0, count(t, `SELECT count(*) FROM ar07_adjustments WHERE tenant_id = $1`, e.tenant))
	require.Equal(t, 0, count(t, `SELECT count(*) FROM ar_accounting_posting_requests WHERE tenant_id = $1`, e.tenant))
	// the stage can be retried once the channel works; then exactly one SENT
	e.del.SetErr(nil)
	code, raw = e.send(cid)
	require.Equal(t, 200, code, string(raw))
	require.Equal(t, "SENT", decode(t, raw)["action"].(map[string]any)["delivery_result"])
	require.Equal(t, 1, e.actions(cid, ResultSent))
}

func TestUnconfiguredDelivererRecordsFailureNeverSuccess(t *testing.T) {
	e := newEnv(t)
	e.m.deliverer = UnconfiguredDeliverer{}
	e.publish(nil)
	item := e.item("500", "2026-10-06")
	cid := e.ok(pCollector, "openCollectionCase", map[string]any{"customer_party_id": e.party, "currency": "USD"})["case_id"].(string)
	e.runDue(testNow)
	require.Equal(t, 1, e.actions(cid, ResultFailed))
	require.Equal(t, 0, e.actions(cid, ResultSent)+e.actions(cid, ResultDelivered))
	require.Equal(t, kit.MustAmount("500"), e.view(item).Residual)
}

func TestDeliveredReceiptIsNotPayment(t *testing.T) {
	e, cid, item := setup(t, nil)
	e.del.Confirmed = true
	e.advance(cid)
	code, raw := e.send(cid)
	require.Equal(t, 200, code, string(raw))
	require.Equal(t, "DELIVERED", decode(t, raw)["action"].(map[string]any)["delivery_result"])
	v := e.view(item)
	require.Equal(t, kit.MustAmount("500"), v.Residual)
	require.Equal(t, "OPEN", v.State)
	// and the case does not resolve on a confirmed delivery
	code, raw = e.cmd(pCollector, "closeCollectionCase", map[string]any{"case_id": cid, "expected_version": e.ver(cid), "outcome": "RESOLVED"})
	require.Equal(t, 409, code, string(raw))
	require.Equal(t, "ACTIVE", e.caseOf(cid)["state"])
}

// ── hold / resume ────────────────────────────────────────────────────────────

func TestResumeAfterHold(t *testing.T) {
	e, cid, _ := setup(t, nil)
	e.advance(cid)
	out := e.ok(pHold, "placeCollectionHold", map[string]any{"case_id": cid, "expected_version": e.ver(cid), "reason_code": "CUSTOMER_REQUEST", "evidence_id": "ev"})
	require.Equal(t, "HELD", out["case"].(map[string]any)["state"])
	require.Equal(t, 1, e.events(cid, EvHoldPlaced))
	// a second hold is refused
	code, _ := e.cmd(pHold, "placeCollectionHold", map[string]any{"case_id": cid, "expected_version": e.ver(cid), "reason_code": "X"})
	require.Equal(t, 409, code)
	code, raw := e.send(cid)
	require.Equal(t, 409, code)
	require.Equal(t, kit.CodeCollectionHoldActive, errCode(t, raw))
	// the scheduler leaves a held case alone
	require.Equal(t, 0, e.runDue(testNow))
	require.Empty(t, e.del.Sent())
	// SoD: the placer cannot release
	code, raw = e.cmd(pHold, "resumeCollections", map[string]any{"case_id": cid, "expected_version": e.ver(cid), "reason_code": "DONE"})
	require.Equal(t, 403, code, string(raw))
	require.Equal(t, kit.CodeSegregationOfDuties, errCode(t, raw))
	// another principal releases; the case returns to its prior state and sends again
	res := e.ok(pHold2, "resumeCollections", map[string]any{"case_id": cid, "expected_version": e.ver(cid), "reason_code": "DONE"})
	require.Equal(t, "ACTIVE", res["state"])
	require.Equal(t, 1, count(t, `SELECT count(*) FROM ar09_holds WHERE tenant_id = $1 AND case_id = $2 AND released_by = $3`, e.tenant, cid, pHold2))
	code, raw = e.send(cid)
	require.Equal(t, 200, code, string(raw))
	require.Len(t, e.del.Sent(), 1)
	// resume of a case that is not held
	code, _ = e.cmd(pHold2, "resumeCollections", map[string]any{"case_id": cid, "expected_version": e.ver(cid), "reason_code": "DONE"})
	require.Equal(t, 409, code)
	// hold rows only ever record their release, once
	_, err := pool.Exec(context.Background(), `UPDATE ar09_holds SET reason_code = 'OTHER' WHERE case_id = $1`, cid)
	require.Error(t, err)
	_, err = pool.Exec(context.Background(), `UPDATE ar09_holds SET released_by = 'z' WHERE case_id = $1`, cid)
	require.Error(t, err)
}

// ── resolution is read live from AR-07 ───────────────────────────────────────

func TestCaseResolvesOnlyFromLiveAR07State(t *testing.T) {
	e, cid, item := setup(t, nil)
	e.advance(cid)
	code, raw := e.cmd(pCollector, "closeCollectionCase", map[string]any{"case_id": cid, "expected_version": e.ver(cid), "outcome": "RESOLVED"})
	require.Equal(t, 409, code, "the item is still owed: "+string(raw))
	// a part payment is not a resolution
	e.a7adjust(item, kit.AdjCashApplication, "200")
	code, _ = e.cmd(pCollector, "closeCollectionCase", map[string]any{"case_id": cid, "expected_version": e.ver(cid), "outcome": "RESOLVED"})
	require.Equal(t, 409, code)
	// the case reads the new residual live: the notice (and snapshot) reflect 300, not the 500 captured at open
	code, raw = e.send(cid)
	require.Equal(t, 200, code, string(raw))
	require.Equal(t, "300.0000", decode(t, raw)["action"].(map[string]any)["aging_snapshot"].(map[string]any)["total_residual"])
	require.Equal(t, "500.0000", e.caseOf(cid)["aging_snapshot"].(map[string]any)["total_residual"], "the snapshot at open is evidence, never updated")
	// AR-07 settles it (the owner of the balance): now the case resolves
	e.a7adjust(item, kit.AdjCashApplication, "300")
	out := e.ok(pCollector, "closeCollectionCase", map[string]any{"case_id": cid, "expected_version": e.ver(cid), "outcome": "RESOLVED"})
	require.Equal(t, "RESOLVED", out["state"])
	require.Equal(t, 1, e.events(cid, EvCaseResolved))
	require.NotNil(t, out["resolution"])
	require.Equal(t, 1, count(t, `SELECT count(*) FROM ar07_adjustments WHERE tenant_id = $1 AND kind = 'CASH_APPLICATION' AND actor_id = 'seed' AND amount = 300`, e.tenant))
	require.Equal(t, 2, count(t, `SELECT count(*) FROM ar07_adjustments WHERE tenant_id = $1`, e.tenant), "only the two AR-07 test adjustments exist; AR-09 wrote none")
	// terminal: nothing further
	code, _ = e.send(cid)
	require.Equal(t, 409, code)
}

func TestSchedulerAutoResolvesSettledCaseAndWrittenOffCounts(t *testing.T) {
	e, cid, item := setup(t, nil)
	e.a7adjust(item, kit.AdjWriteOff, "500") // AR-10's job in production; here the owner-of-balance port does it
	require.Equal(t, 1, e.runDue(testNow))
	cs := e.caseOf(cid)
	require.Equal(t, "RESOLVED", cs["state"])
	require.Equal(t, 1, e.events(cid, EvCaseResolved))
	require.Empty(t, e.del.Sent())
	// a new overdue item for the same customer can open a fresh case
	e.item("50", "2026-10-01")
	e.ok(pCollector, "openCollectionCase", map[string]any{"customer_party_id": e.party, "currency": "USD"})
}

// ── isolation, entity scope, available actions ───────────────────────────────

func TestTenantIsolation(t *testing.T) {
	e, cid, _ := setup(t, nil)
	other := newEnv(t)
	// the other tenant sees nothing: reads and commands look like "absent"
	code, _ := call(t, other.h, http.MethodGet, base+"getCollectionCase?case_id="+cid, other.tenant, "reader", "", nil)
	require.Equal(t, 404, code)
	for _, q := range []string{"getDunningSchedule?case_id=" + cid, "getAvailableActions?case_id=" + cid} {
		code, _ = call(t, other.h, http.MethodGet, base+q, other.tenant, "reader", "", nil)
		require.Equal(t, 404, code, q)
	}
	code, raw := call(t, other.h, http.MethodGet, base+"getCustomerCollectionHistory?customer_party_id="+e.party, other.tenant, "reader", "", nil)
	require.Equal(t, 200, code)
	require.Len(t, decode(t, raw)["cases"], 0)
	require.Len(t, decode(t, raw)["actions"], 0)
	code, raw = other.cmd(pCollector, "sendDunningNotice", map[string]any{"case_id": cid, "expected_version": 1})
	require.Equal(t, 404, code, string(raw))
	code, _ = other.cmd(pHold, "placeCollectionHold", map[string]any{"case_id": cid, "expected_version": 1, "reason_code": "X"})
	require.Equal(t, 404, code)
	// same tenant, different legal entity: also absent
	code, _ = call(t, e.h, http.MethodPost, base+"commands/sendDunningNotice", e.tenant, pCollector, "", map[string]any{
		"legal_entity_id": uuid.NewString(), "case_id": cid, "expected_version": 1})
	require.Equal(t, 404, code)
	require.Equal(t, 0, other.actions(cid, ResultSuppressed)+other.actions(cid, ResultSent))
}

func TestGetAvailableActionsDerivedFromStateAndPermissions(t *testing.T) {
	e, cid, _ := setup(t, nil)
	actions := func(principal string) (state string, avail, denied []string) {
		code, raw := call(t, e.h, http.MethodGet, base+"getAvailableActions?case_id="+cid, e.tenant, principal, "", nil)
		require.Equal(t, 200, code, string(raw))
		m := decode(t, raw)
		for _, a := range m["available"].([]any) {
			avail = append(avail, a.(string))
		}
		for _, a := range m["denied_by_permission"].([]any) {
			denied = append(denied, a.(string))
		}
		return m["state"].(string), avail, denied
	}
	st, avail, _ := actions(pCollector)
	require.Equal(t, "ELIGIBLE", st)
	require.Contains(t, avail, "AdvanceDunningStage")
	require.NotContains(t, avail, "SendDunningNotice", "no stage reached yet")
	require.NotContains(t, avail, "ResumeCollections")
	require.NotContains(t, avail, "PlaceCollectionHold", "the collector lacks hold permission")
	_, _, denied := actions(pCollector)
	require.Contains(t, denied, "PlaceCollectionHold")
	e.advance(cid)
	_, avail, _ = actions(pManager)
	require.Contains(t, avail, "SendDunningNotice")
	require.Contains(t, avail, "EscalateCollectionCase")
	e.send(cid)
	_, avail, _ = actions(pManager)
	require.NotContains(t, avail, "SendDunningNotice", "this stage's notice is already sent")
	e.ok(pHold, "placeCollectionHold", map[string]any{"case_id": cid, "expected_version": e.ver(cid), "reason_code": "X"})
	st, avail, _ = actions(pHold2)
	require.Equal(t, "HELD", st)
	require.Equal(t, []string{"ResumeCollections"}, avail)
}

func TestSchemaGuards(t *testing.T) {
	e, cid, _ := setup(t, nil)
	e.advance(cid)
	e.send(cid)
	ctx := context.Background()
	for name, sql := range map[string]string{
		"action UPDATE":       `UPDATE ar09_dunning_actions SET channel = 'SMS' WHERE case_id = $1`,
		"action DELETE":       `DELETE FROM ar09_dunning_actions WHERE case_id = $1`,
		"history UPDATE":      `UPDATE ar09_history SET event = 'x' WHERE case_id = $1`,
		"history DELETE":      `DELETE FROM ar09_history WHERE case_id = $1`,
		"snapshot rewrite":    `UPDATE ar09_collection_cases SET aging_snapshot = '{}'::jsonb WHERE case_id = $1`,
		"customer rewrite":    `UPDATE ar09_collection_cases SET customer_party_id = gen_random_uuid() WHERE case_id = $1`,
		"case DELETE":         `DELETE FROM ar09_collection_cases WHERE case_id = $1`,
		"bad state":           `UPDATE ar09_collection_cases SET state = 'OVERDUE' WHERE case_id = $1`,
		"notice w/o artifact": `INSERT INTO ar09_dunning_actions (action_id, tenant_id, legal_entity_id, case_id, customer_party_id, stage_no, channel, template_key, delivery_result, actor_id) SELECT gen_random_uuid(), tenant_id, legal_entity_id, case_id, customer_party_id, 2, 'EMAIL', 'T', 'SENT', 'x' FROM ar09_collection_cases WHERE case_id = $1`,
		"suppression w/o why": `INSERT INTO ar09_dunning_actions (action_id, tenant_id, legal_entity_id, case_id, customer_party_id, stage_no, channel, template_key, delivery_result, actor_id) SELECT gen_random_uuid(), tenant_id, legal_entity_id, case_id, customer_party_id, 2, 'EMAIL', 'T', 'SUPPRESSED', 'x' FROM ar09_collection_cases WHERE case_id = $1`,
		"second live case":    `INSERT INTO ar09_collection_cases (case_id, tenant_id, legal_entity_id, customer_party_id, currency, owner_principal, dunning_profile, policy_id, aging_snapshot, created_by) SELECT gen_random_uuid(), tenant_id, legal_entity_id, customer_party_id, currency, 'x', dunning_profile, policy_id, aging_snapshot, 'x' FROM ar09_collection_cases WHERE case_id = $1`,
		"promise amount <= 0": `INSERT INTO ar09_promises (promise_id, tenant_id, legal_entity_id, case_id, version_no, promised_date, promised_amount, currency, recorded_by) SELECT gen_random_uuid(), tenant_id, legal_entity_id, case_id, 1, '2026-11-01', 0, 'USD', 'x' FROM ar09_collection_cases WHERE case_id = $1`,
	} {
		_, err := pool.Exec(ctx, sql, cid)
		require.Error(t, err, name)
	}
}
