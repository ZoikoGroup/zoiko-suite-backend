//go:build integration

// Real-Postgres tests for AR-08 (the real AR-07 module is the open-item owner). Run (serially):
//
//	go test -tags=integration -count=1 -p 1 -timeout=600s ./internal/ar08/...
//
// The embedded superuser bypasses RLS, so isolation tests prove the explicit tenant_id predicates, not the policies.
// Spec section 18 scenarios carry their number: TestNeg22 (unconfirmed payment applied), TestNeg23 (same receipt applied twice),
// TestNeg24 (application does not reconcile to the receipt), TestNeg25 (manual match above authority), TestNeg26 (application
// across a tenant / legal-entity boundary).
package ar08

import (
	"context"
	"encoding/json"
	"errors"
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
	pool, stop = kittest.Start("ar08_test", 17800)
	code := m.Run()
	stop()
	os.Exit(code)
}

const (
	pClerkUnap  = "clerkunap"  // manage + unapply (to prove the applier cannot unapply)
	pClerkReap  = "clerkreap"  // manage + reapply
	pDisputer   = "disputer"   // AR-07 dispute management
	pClerkOther = "clerkother" // a second applier
)

type env struct {
	t      *testing.T
	m      *Module
	m7     *ar07.Module
	h, h7  http.Handler
	tenant string
	entity string
	party  string
	src    *fakeSource
	per    *fakePeriods
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, tenant: uuid.NewString(), entity: uuid.NewString(), party: uuid.NewString(), src: newFakeSource(), per: &fakePeriods{}}
	g := standardGrants()
	g[pClerkUnap] = map[string]bool{ActionApplicationManage: true, ActionUnapply: true}
	g[pClerkReap] = map[string]bool{ActionApplicationManage: true, ActionReapply: true}
	g[pDisputer] = map[string]bool{ar07.ActionDisputeManage: true}
	g[pClerkOther] = map[string]bool{ActionApplicationManage: true}
	deps := testDeps(&fakeAuthz{grants: g})
	deps.DB = kit.NewDB(pool)
	e.m7 = ar07.New(deps)
	e.m = New(deps, e.src, e.m7, e.m7, e.per)
	e.h = testRouter(e.m)
	r7 := chi.NewRouter()
	r7.Use(svcmiddleware.TenantContext())
	e.m7.Register(r7)
	e.h7 = r7
	return e
}

func (e *env) caller(entity string) kit.Caller {
	return kit.Caller{TenantID: e.tenant, PrincipalID: "boss", CorrelationID: "corr-" + uuid.NewString(), LegalEntityID: entity}
}

func (e *env) tx(fn func(tx pgx.Tx) error) error {
	return e.m.deps.DB.WithTx(context.Background(), e.tenant, fn)
}

func (e *env) post(op, principal string, body map[string]any) (int, []byte) {
	code, b, _ := call(e.t, e.h, "POST", base+"commands/"+op, e.tenant, principal, "", body)
	return code, b
}

func (e *env) must(status int, op, principal string, body map[string]any) []byte {
	e.t.Helper()
	code, b := e.post(op, principal, body)
	require.Equal(e.t, status, code, string(b))
	return b
}

func (e *env) refused(status int, wantCode, op, principal string, body map[string]any) {
	e.t.Helper()
	code, b := e.post(op, principal, body)
	require.Equal(e.t, status, code, string(b))
	require.Equal(e.t, wantCode, errCode(e.t, b), string(b))
}

func (e *env) get(path, principal string, tenant string) (int, []byte) {
	code, b, _ := call(e.t, e.h, "GET", base+path, tenant, principal, "", nil)
	return code, b
}

func (e *env) n(sql string, args ...any) int {
	e.t.Helper()
	var n int
	require.NoError(e.t, pool.QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}

func (e *env) item(entity, party, amount, currency, due string) (itemID, docID string) {
	e.t.Helper()
	dd, _ := time.Parse("2006-01-02", due)
	in := kit.OpenItemFromDocument{DocumentKind: kit.DocInvoice, SourceDocumentID: uuid.NewString(), DocumentNumber: "INV-" + uuid.NewString()[:6], CustomerPartyID: party,
		Currency: currency, OriginalAmount: kit.MustAmount(amount), IssueDate: dd.AddDate(0, 0, -30), DueDate: dd}
	var ref kit.OpenItemRef
	require.NoError(e.t, e.tx(func(tx pgx.Tx) (err error) {
		ref, err = e.m7.CreateFromDocument(context.Background(), tx, e.caller(entity), in)
		return
	}))
	return ref.OpenItemID, in.SourceDocumentID
}

func (e *env) residual(itemID string) kit.Amount {
	e.t.Helper()
	v, err := e.m7.GetByID(context.Background(), e.tenant, itemID)
	require.NoError(e.t, err)
	return v.Residual
}

func (e *env) register(amount, remittance string) *Receipt {
	e.t.Helper()
	id := uuid.NewString()
	e.src.put(e.tenant, id, amount, "USD", BankNormalized)
	b := e.must(http.StatusCreated, "registerConfirmedReceipt", pRegistrar, map[string]any{"legal_entity_id": e.entity, "bank_transaction_id": id,
		"customer_party_id": e.party, "remittance_text": remittance})
	var r Receipt
	require.NoError(e.t, json.Unmarshal(b, &r))
	return &r
}

func (e *env) batch(rs ...*Receipt) *Batch {
	e.t.Helper()
	ids := []string{}
	for _, r := range rs {
		ids = append(ids, r.ReceiptID)
	}
	b := e.must(http.StatusCreated, "createApplicationBatch", pClerk, map[string]any{"legal_entity_id": e.entity, "currency": "USD", "reference": "run-1", "receipt_ids": ids})
	var out Batch
	require.NoError(e.t, json.Unmarshal(b, &out))
	return &out
}

func (e *env) receipt(id string) *Receipt {
	e.t.Helper()
	var r *Receipt
	require.NoError(e.t, e.tx(func(tx pgx.Tx) (err error) {
		r, err = getReceipt(context.Background(), tx, e.tenant, id, false)
		return
	}))
	return r
}

func (e *env) batchRow(id string) *Batch {
	e.t.Helper()
	var b *Batch
	require.NoError(e.t, e.tx(func(tx pgx.Tx) (err error) { b, err = getBatch(context.Background(), tx, e.tenant, id, false); return }))
	return b
}

func (e *env) generate(b *Batch, r *Receipt) map[string]any {
	e.t.Helper()
	out := e.must(http.StatusOK, "generateMatchCandidates", pClerk, map[string]any{"batch_id": b.BatchID, "receipt_id": r.ReceiptID})
	var m map[string]any
	require.NoError(e.t, json.Unmarshal(out, &m))
	return m
}

func (e *env) autoApply(principal string, b *Batch, r *Receipt) (int, []byte) {
	return e.post("applyReceipt", principal, map[string]any{"batch_id": b.BatchID, "receipt_id": r.ReceiptID, "expected_version": r.Version, "mode": "AUTO"})
}

func decodeApplied(t *testing.T, b []byte) applyResult {
	t.Helper()
	var r applyResult
	require.NoError(t, json.Unmarshal(b, &r), string(b))
	return r
}

// ready: one open item, a receipt equal to it that quotes its document id, a batch, and generated candidates.
func (e *env) ready(amount string) (itemID string, r *Receipt, b *Batch) {
	e.t.Helper()
	itemID, doc := e.item(e.entity, e.party, amount, "USD", "2026-10-20")
	r = e.register(amount, "Payment for "+doc)
	b = e.batch(r)
	e.generate(b, r)
	return
}

func (e *env) counts(receiptID string) (apps, lines, postings, adjustments int) {
	apps = e.n(`SELECT count(*) FROM ar08_applications WHERE tenant_id = $1 AND receipt_id = $2`, e.tenant, receiptID)
	lines = e.n(`SELECT count(*) FROM ar08_application_lines l JOIN ar08_applications a ON a.application_id = l.application_id WHERE a.tenant_id = $1 AND a.receipt_id = $2`, e.tenant, receiptID)
	postings = e.n(`SELECT count(*) FROM ar_accounting_posting_requests WHERE tenant_id = $1 AND source_type IN ('CASH_APPLICATION','CASH_UNAPPLICATION')`, e.tenant)
	adjustments = e.n(`SELECT count(*) FROM ar07_adjustments WHERE tenant_id = $1 AND source_ref_type = 'CASH_APPLICATION'`, e.tenant)
	return
}

func (e *env) events(eventType string) int {
	return e.n(`SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = $2`, e.tenant, eventType)
}

// ── happy paths ──────────────────────────────────────────────────────────────

func TestAutoApply_EndToEnd(t *testing.T) {
	e := newEnv(t)
	itemID, r, b := e.ready("500.00")
	require.Equal(t, BatchProposed, e.batchRow(b.BatchID).State)
	code, body := e.autoApply(pClerk, b, r)
	require.Equal(t, http.StatusCreated, code, string(body))
	res := decodeApplied(t, body)
	require.False(t, res.Replayed)
	require.Equal(t, RcptFull, res.Receipt.State)
	require.Equal(t, kit.MustAmount("500"), res.Application.AppliedAmount)
	require.Equal(t, kit.Amount(0), e.residual(itemID))
	apps, lines, postings, adj := e.counts(r.ReceiptID)
	require.Equal(t, []int{1, 1, 1, 1}, []int{apps, lines, postings, adj})
	require.Equal(t, 1, e.events(EvApplied))
	require.Equal(t, BatchFull, e.batchRow(b.BatchID).State)

	// the posting is balanced: Dr cash receipt = Cr receivable (no unapplied remainder)
	var payload []byte
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT request_payload FROM ar_accounting_posting_requests WHERE tenant_id = $1 AND source_event_id = $2`, e.tenant, res.Application.ApplicationID).Scan(&payload))
	var ev kit.PostingEvent
	require.NoError(t, json.Unmarshal(payload, &ev))
	require.Len(t, ev.Lines, 2)
	require.Equal(t, KeyCashReceipt, ev.Lines[0].MappingKey)
	require.Equal(t, KeyReceivable, ev.Lines[1].MappingKey)
	require.Equal(t, "2026-10", ev.FiscalPeriod)

	// reads
	code, body = e.get("getApplication?receipt_id="+r.ReceiptID, pClerk, e.tenant)
	require.Equal(t, http.StatusOK, code, string(body))
	var view struct {
		Accounting map[string]accountingView `json:"accounting"`
		Exceptions []string                  `json:"control_exceptions"`
	}
	require.NoError(t, json.Unmarshal(body, &view))
	require.True(t, view.Accounting["apply"].PostingFound)
	require.Empty(t, view.Exceptions)
	code, body = e.get("getUnappliedCash", pClerk, e.tenant)
	require.Equal(t, http.StatusOK, code)
	require.NotContains(t, string(body), r.ReceiptID)
}

func TestAutoApply_PartialKeepsUnappliedCashAndPostsIt(t *testing.T) {
	e := newEnv(t)
	itemID, doc := e.item(e.entity, e.party, "500.00", "USD", "2026-10-20")
	r := e.register("800.00", "Payment for "+doc)
	b := e.batch(r)
	e.generate(b, r)
	code, body := e.autoApply(pClerk, b, r)
	require.Equal(t, http.StatusCreated, code, string(body))
	res := decodeApplied(t, body)
	require.Equal(t, RcptPartial, res.Receipt.State)
	require.Equal(t, kit.MustAmount("300"), res.Receipt.UnappliedAmount)
	require.Equal(t, kit.Amount(0), e.residual(itemID))
	require.Equal(t, BatchPartial, e.batchRow(b.BatchID).State)
	var payload []byte
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT request_payload FROM ar_accounting_posting_requests WHERE tenant_id = $1 AND source_event_id = $2`, e.tenant, res.Application.ApplicationID).Scan(&payload))
	var ev kit.PostingEvent
	require.NoError(t, json.Unmarshal(payload, &ev))
	require.Len(t, ev.Lines, 3)
	require.Equal(t, KeyUnappliedCash, ev.Lines[2].MappingKey)
	require.InDelta(t, 300.0, ev.Lines[2].CreditAmount, 0.0001)
	code, body = e.get("getUnappliedCash", pClerk, e.tenant)
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, string(body), r.ReceiptID)
}

func TestCandidates_PersistedDeterministicAndDisputeFlagged(t *testing.T) {
	e := newEnv(t)
	a, docA := e.item(e.entity, e.party, "500.00", "USD", "2026-10-20")
	b2, _ := e.item(e.entity, e.party, "500.00", "USD", "2026-10-25")
	r := e.register("500.00", "ref "+docA)
	bt := e.batch(r)
	g1 := e.generate(bt, r)
	g2 := e.generate(bt, r)
	c1, _ := json.Marshal(g1["candidates"])
	c2, _ := json.Marshal(g2["candidates"])
	require.JSONEq(t, string(c1), string(c2), "same inputs give the same ranked candidates")
	require.EqualValues(t, 2, g2["generation"])
	require.Equal(t, 4, e.n(`SELECT count(*) FROM ar08_candidates WHERE tenant_id = $1 AND receipt_id = $2`, e.tenant, r.ReceiptID), "both generations persisted as evidence")
	cands := g1["candidates"].([]any)
	require.Equal(t, a, cands[0].(map[string]any)["open_item_id"])
	_ = b2

	// dispute the best candidate in AR-07: it is reported, and never auto-applied
	require.Equal(t, 1, e.residualVersion(a))
	code, body, _ := call(t, e.h7, "POST", "/internal/v1/ar07/commands/openDispute", e.tenant, pDisputer, "", map[string]any{"legal_entity_id": e.entity, "open_item_id": a,
		"expected_version": 1, "reason_code": "PRICE", "evidence_id": "ev-1"})
	require.Equal(t, 2, code/100, string(body))
	g3 := e.generate(bt, r)
	top := g3["candidates"].([]any)
	var found bool
	for _, c := range top {
		if c.(map[string]any)["open_item_id"] == a {
			found = true
			require.Equal(t, true, c.(map[string]any)["disputed"])
		}
	}
	require.True(t, found)
	e.refused(http.StatusUnprocessableEntity, CodeManualApprovalRequired, "applyReceipt", pClerk, map[string]any{"batch_id": bt.BatchID, "receipt_id": r.ReceiptID, "expected_version": 1, "mode": "AUTO"})
	require.Equal(t, kit.MustAmount("500"), e.residual(a))
}

func (e *env) residualVersion(itemID string) int {
	v, err := e.m7.GetByID(context.Background(), e.tenant, itemID)
	require.NoError(e.t, err)
	return v.Version
}

// ── #22 ──────────────────────────────────────────────────────────────────────

func TestNeg22_UnconfirmedPaymentApplied(t *testing.T) {
	e := newEnv(t)
	// registration refuses every non-confirmed Banking state and leaves no receipt
	for status, id := range map[string]string{BankQuarantined: uuid.NewString(), BankSuperseded: uuid.NewString()} {
		e.src.put(e.tenant, id, "100.00", "USD", status)
		e.refused(http.StatusUnprocessableEntity, "SETTLEMENT_NOT_CONFIRMED", "registerConfirmedReceipt", pRegistrar, map[string]any{"legal_entity_id": e.entity, "bank_transaction_id": id})
	}
	e.refused(http.StatusUnprocessableEntity, "SETTLEMENT_NOT_CONFIRMED", "registerConfirmedReceipt", pRegistrar, map[string]any{"legal_entity_id": e.entity, "bank_transaction_id": uuid.NewString()})
	e.src.setErr(ErrSourceUnavailable)
	e.refused(http.StatusServiceUnavailable, "SETTLEMENT_NOT_CONFIRMED", "registerConfirmedReceipt", pRegistrar, map[string]any{"legal_entity_id": e.entity, "bank_transaction_id": uuid.NewString()})
	e.src.setErr(nil)
	require.Equal(t, 0, e.n(`SELECT count(*) FROM ar08_receipts WHERE tenant_id = $1`, e.tenant))

	// a receipt Banking later supersedes cannot be applied
	itemID, r, b := e.ready("500.00")
	e.src.setStatus(r.BankTransactionID, BankSuperseded)
	code, body := e.autoApply(pClerk, b, r)
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	require.Equal(t, "SETTLEMENT_NOT_CONFIRMED", errCode(t, body))
	// ... nor when Banking cannot be asked
	e.src.setStatus(r.BankTransactionID, BankNormalized)
	e.src.setErr(ErrSourceUnavailable)
	code, body = e.autoApply(pClerk, b, r)
	require.Equal(t, http.StatusServiceUnavailable, code)
	require.Equal(t, "SETTLEMENT_NOT_CONFIRMED", errCode(t, body))
	e.src.setErr(nil)
	apps, _, postings, adj := e.counts(r.ReceiptID)
	require.Equal(t, []int{0, 0, 0}, []int{apps, postings, adj})
	require.Equal(t, kit.MustAmount("500"), e.residual(itemID))
}

func TestSupersededBankingTransaction_ReverifyFlagsAndBlocksNeverMovesMoney(t *testing.T) {
	e := newEnv(t)
	itemID, r, b := e.ready("500.00")
	code, body := e.autoApply(pClerk, b, r)
	require.Equal(t, http.StatusCreated, code, string(body))
	b = e.batchRow(b.BatchID)

	e.src.setStatus(r.BankTransactionID, BankSuperseded)
	out := e.must(http.StatusOK, "reconcileApplicationBatch", pClerk, map[string]any{"batch_id": b.BatchID, "expected_version": b.Version})
	var rec Reconciliation
	require.NoError(t, json.Unmarshal(out, &rec))
	require.Equal(t, "EXCEPTION", rec.Status)
	require.Equal(t, "SETTLEMENT_SUPERSEDED", rec.Findings[0].Code)
	cur := e.receipt(r.ReceiptID)
	require.True(t, cur.Blocked)
	require.Equal(t, "SETTLEMENT_SUPERSEDED", cur.ControlException)
	// money is exactly as it was: still applied, residual still settled, no reversal queued
	require.Equal(t, RcptFull, cur.State)
	require.Equal(t, kit.Amount(0), e.residual(itemID))
	require.Equal(t, 0, e.n(`SELECT count(*) FROM ar_accounting_posting_requests WHERE tenant_id = $1 AND source_type = 'CASH_UNAPPLICATION'`, e.tenant))
	require.Equal(t, 1, e.n(`SELECT count(*) FROM ar_audit_events WHERE tenant_id = $1 AND command = 'settlementControlException'`, e.tenant))
	// the exception is visible on the read side
	_, body = e.get("getApplication?receipt_id="+r.ReceiptID, pClerk, e.tenant)
	require.Contains(t, string(body), "SETTLEMENT_SUPERSEDED")
	// only a governed unapply may change the money; afterwards the blocked receipt takes no new application
	e.must(http.StatusOK, "unapplyReceipt", pUnapplier, map[string]any{"receipt_id": r.ReceiptID, "expected_version": cur.Version, "reason_code": "SETTLEMENT_SUPERSEDED", "evidence_id": "e"})
	require.Equal(t, kit.MustAmount("500"), e.residual(itemID))
	e.refused(http.StatusUnprocessableEntity, "SETTLEMENT_NOT_CONFIRMED", "reapplyReceipt", pClerkReap, map[string]any{"receipt_id": r.ReceiptID, "expected_version": cur.Version + 1, "reason_code": "X",
		"evidence_id": "e", "lines": []map[string]any{{"open_item_id": itemID, "amount": "500.00"}}})
}

func TestReconcile_QuarantinedBeforeApplyBlocksApplication(t *testing.T) {
	e := newEnv(t)
	_, r, b := e.ready("500.00")
	e.src.setStatus(r.BankTransactionID, BankQuarantined)
	out := e.must(http.StatusOK, "reconcileApplicationBatch", pClerk, map[string]any{"batch_id": b.BatchID, "expected_version": e.batchRow(b.BatchID).Version})
	var rec Reconciliation
	require.NoError(t, json.Unmarshal(out, &rec))
	require.Equal(t, "SETTLEMENT_QUARANTINED", rec.Findings[0].Code)
	e.src.setStatus(r.BankTransactionID, BankNormalized) // even if Banking recovers, the exception must be cleared by a governed process, not silently
	code, body := e.autoApply(pClerk, b, e.receipt(r.ReceiptID))
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	require.Equal(t, "SETTLEMENT_NOT_CONFIRMED", errCode(t, body))
}

// ── #23 ──────────────────────────────────────────────────────────────────────

func TestNeg23_SameReceiptAppliedTwice(t *testing.T) {
	e := newEnv(t)
	// the same Banking transaction cannot become two receipts
	id := uuid.NewString()
	e.src.put(e.tenant, id, "100.00", "USD", BankNormalized)
	body := map[string]any{"legal_entity_id": e.entity, "bank_transaction_id": id}
	e.must(http.StatusCreated, "registerConfirmedReceipt", pRegistrar, body)
	e.refused(http.StatusConflict, "CASH_APPLICATION_DUPLICATE", "registerConfirmedReceipt", pRegistrar, body)
	require.Equal(t, 1, e.n(`SELECT count(*) FROM ar08_receipts WHERE tenant_id = $1`, e.tenant))

	itemID, r, b := e.ready("500.00")
	code, first := e.autoApply(pClerk, b, r)
	require.Equal(t, http.StatusCreated, code, string(first))
	// a retry under a NEW idempotency key (timeout, no idempotency) is recognised, not re-applied
	code, again := e.autoApply(pClerk, b, r)
	require.Equal(t, http.StatusOK, code, string(again))
	res := decodeApplied(t, again)
	require.True(t, res.Replayed)
	require.Equal(t, decodeApplied(t, first).Application.ApplicationID, res.Application.ApplicationID)
	apps, lines, postings, adj := e.counts(r.ReceiptID)
	require.Equal(t, []int{1, 1, 1, 1}, []int{apps, lines, postings, adj})
	require.Equal(t, kit.Amount(0), e.residual(itemID))

	// a DIFFERENT application of the same receipt is a duplicate
	e.refused(http.StatusConflict, "CASH_APPLICATION_DUPLICATE", "applyReceipt", pClerk, map[string]any{"batch_id": b.BatchID, "receipt_id": r.ReceiptID, "expected_version": 2, "mode": "MANUAL",
		"reason_code": "X", "lines": []map[string]any{{"open_item_id": itemID, "amount": "1.00"}}})
	// an applied receipt cannot join another batch, and an unapplied one cannot belong to two
	e.refused(http.StatusConflict, "INVALID_STATE_TRANSITION", "createApplicationBatch", pClerk, map[string]any{"legal_entity_id": e.entity, "currency": "USD", "receipt_ids": []string{r.ReceiptID}})
	r3 := e.register("10.00", "")
	e.batch(r3)
	e.refused(http.StatusConflict, "CASH_APPLICATION_DUPLICATE", "createApplicationBatch", pClerk, map[string]any{"legal_entity_id": e.entity, "currency": "USD", "receipt_ids": []string{r3.ReceiptID}})
}

func TestIdempotentReplay_SameKeyReturnsStoredResponse(t *testing.T) {
	e := newEnv(t)
	itemID, r, b := e.ready("500.00")
	body := map[string]any{"batch_id": b.BatchID, "receipt_id": r.ReceiptID, "expected_version": 1, "mode": "AUTO"}
	code, first, _ := call(t, e.h, "POST", base+"commands/applyReceipt", e.tenant, pClerk, "key-1", body)
	require.Equal(t, http.StatusCreated, code, string(first))
	code, second, hdr := call(t, e.h, "POST", base+"commands/applyReceipt", e.tenant, pClerk, "key-1", body)
	require.Equal(t, http.StatusCreated, code)
	require.Equal(t, "true", hdr.Get("Idempotent-Replay"))
	require.JSONEq(t, string(first), string(second))
	body["mode"] = "MANUAL"
	code, third, _ := call(t, e.h, "POST", base+"commands/applyReceipt", e.tenant, pClerk, "key-1", body)
	require.Equal(t, http.StatusUnprocessableEntity, code, string(third))
	require.Equal(t, "IDEMPOTENCY_KEY_REUSED", errCode(t, third))
	apps, _, _, adj := e.counts(r.ReceiptID)
	require.Equal(t, []int{1, 1}, []int{apps, adj})
	require.Equal(t, kit.Amount(0), e.residual(itemID))
}

func TestConcurrentIdenticalApplications_ExactlyOneConsequence(t *testing.T) {
	e := newEnv(t)
	itemID, r, b := e.ready("500.00")
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = e.autoApply(pClerk, b, r)
		}(i)
	}
	wg.Wait()
	created := 0
	for _, c := range codes {
		require.Contains(t, []int{http.StatusCreated, http.StatusOK}, c)
		if c == http.StatusCreated {
			created++
		}
	}
	require.Equal(t, 1, created, "exactly one request applied; the rest were recognised retries")
	apps, lines, postings, adj := e.counts(r.ReceiptID)
	require.Equal(t, []int{1, 1, 1, 1}, []int{apps, lines, postings, adj})
	require.Equal(t, kit.Amount(0), e.residual(itemID))
}

func (e *env) planFor(r *Receipt, b *Batch, itemID, amount string, mode string) *plan {
	lines := []Line{{OpenItemID: itemID, Amount: kit.MustAmount(amount)}}
	SortLines(lines)
	s, err := ValidateLines(r, lines, 0, "", nil)
	require.NoError(e.t, err)
	return &plan{receiptID: r.ReceiptID, batchID: b.BatchID, mode: mode, hash: RequestHash(b.BatchID, r.ReceiptID, mode, lines, 0, ""), lines: lines, split: s, customer: e.party,
		expected: 1, periodKey: "2026-10"}
}

func TestConcurrentApplications_OneReceiptOneWinner(t *testing.T) {
	e := newEnv(t)
	r := e.register("100.00", "")
	b := e.batch(r)
	var items []string
	for i := 0; i < 5; i++ {
		id, _ := e.item(e.entity, e.party, "100.00", "USD", "2026-10-20")
		items = append(items, id)
	}
	errs := make([]error, len(items))
	var wg sync.WaitGroup
	for i, id := range items {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			_, _, _, errs[i] = e.m.execute(context.Background(), e.caller(e.entity), e.planFor(r, b, id, "100.00", ModeManual))
		}(i, id)
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
			continue
		}
		var p *kit.Problem
		require.True(t, errors.As(err, &p), err)
		require.Contains(t, []string{"CASH_APPLICATION_DUPLICATE", "STALE_VERSION"}, p.Code)
	}
	require.Equal(t, 1, wins)
	apps, _, _, adj := e.counts(r.ReceiptID)
	require.Equal(t, []int{1, 1}, []int{apps, adj})
	settled := 0
	for _, id := range items {
		if e.residual(id).IsZero() {
			settled++
		}
	}
	require.Equal(t, 1, settled, "only the winner's item was settled")
}

func TestConcurrentApplications_LastResidualOneWinner(t *testing.T) {
	e := newEnv(t)
	itemID, _ := e.item(e.entity, e.party, "100.00", "USD", "2026-10-20")
	var rs []*Receipt
	var bs []*Batch
	for i := 0; i < 4; i++ {
		r := e.register("100.00", "")
		rs, bs = append(rs, r), append(bs, e.batch(r))
	}
	errs := make([]error, len(rs))
	var wg sync.WaitGroup
	for i := range rs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, _, errs[i] = e.m.execute(context.Background(), e.caller(e.entity), e.planFor(rs[i], bs[i], itemID, "100.00", ModeManual))
		}(i)
	}
	wg.Wait()
	wins := 0
	for i, err := range errs {
		if err == nil {
			wins++
			continue
		}
		var p *kit.Problem
		require.True(t, errors.As(err, &p), err)
		require.Equal(t, "RECEIVABLE_RESIDUAL_INVALID", p.Code)
		apps, _, _, _ := e.counts(rs[i].ReceiptID)
		require.Equal(t, 0, apps, "the loser left nothing behind")
		require.Equal(t, RcptUnapplied, e.receipt(rs[i].ReceiptID).State)
	}
	require.Equal(t, 1, wins)
	require.Equal(t, kit.Amount(0), e.residual(itemID))
	require.Equal(t, 1, e.n(`SELECT count(*) FROM ar07_adjustments WHERE tenant_id = $1 AND kind = 'CASH_APPLICATION'`, e.tenant))
}

// ── #24 ──────────────────────────────────────────────────────────────────────

func TestNeg24_ApplicationDoesNotReconcileToReceipt(t *testing.T) {
	e := newEnv(t)
	itemID, _ := e.item(e.entity, e.party, "900.00", "USD", "2026-10-20")
	r := e.register("100.00", "")
	b := e.batch(r)
	manual := func(lines []map[string]any, extra map[string]any) map[string]any {
		m := map[string]any{"batch_id": b.BatchID, "receipt_id": r.ReceiptID, "expected_version": 1, "mode": "MANUAL", "reason_code": "X", "lines": lines}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	l := func(a string) []map[string]any { return []map[string]any{{"open_item_id": itemID, "amount": a}} }
	e.refused(http.StatusUnprocessableEntity, "APPLICATION_OUT_OF_BALANCE", "applyReceipt", pClerk, manual(l("100.01"), nil))
	e.refused(http.StatusUnprocessableEntity, "APPLICATION_OUT_OF_BALANCE", "applyReceipt", pClerk, manual(l("60.00"), map[string]any{"unapplied_amount": "39.00"}))
	e.refused(http.StatusUnprocessableEntity, "APPLICATION_OUT_OF_BALANCE", "applyReceipt", pClerk, manual(l("60.00"), map[string]any{"difference_amount": "60.00", "difference_kind": "BANK_FEE", "evidence_id": "e"}))
	require.Equal(t, 0, e.n(`SELECT count(*) FROM ar08_manual_matches WHERE tenant_id = $1`, e.tenant))
	require.Equal(t, 0, e.n(`SELECT count(*) FROM ar08_applications WHERE tenant_id = $1`, e.tenant))

	// the database refuses an unreconciled receipt or application even if the application layer were bypassed
	_, err := pool.Exec(context.Background(), `UPDATE ar08_receipts SET applied_amount = applied_amount + 1, unapplied_amount = unapplied_amount WHERE receipt_id = $1`, r.ReceiptID)
	require.ErrorContains(t, err, "ar08_receipt_reconciles")
	_, err = pool.Exec(context.Background(), `UPDATE ar08_receipts SET unapplied_amount = 0 WHERE receipt_id = $1`, r.ReceiptID)
	require.ErrorContains(t, err, "ar08_receipt_reconciles")
	require.Equal(t, kit.MustAmount("100"), e.receipt(r.ReceiptID).UnappliedAmount)
}

func TestAuthorizedDifference_ReconcilesAndPostsToDifferenceAccount(t *testing.T) {
	e := newEnv(t)
	itemID, _ := e.item(e.entity, e.party, "95.00", "USD", "2026-10-20")
	r := e.register("100.00", "")
	b := e.batch(r)
	out := e.must(http.StatusAccepted, "applyReceipt", pClerk, map[string]any{"batch_id": b.BatchID, "receipt_id": r.ReceiptID, "expected_version": 1, "mode": "MANUAL", "reason_code": "BANK_FEE",
		"evidence_id": "ev-fee", "difference_amount": "5.00", "difference_kind": "BANK_FEE", "unapplied_amount": "0.00", "lines": []map[string]any{{"open_item_id": itemID, "amount": "95.00"}}})
	var prop struct{ Match ManualMatch }
	require.NoError(t, json.Unmarshal(out, &prop))
	res := decodeApplied(t, e.must(http.StatusCreated, "approveManualMatch", pApprover, map[string]any{"match_id": prop.Match.MatchID, "expected_version": 1}))
	require.Equal(t, RcptFull, res.Receipt.State)
	require.Equal(t, kit.MustAmount("5"), res.Receipt.DifferenceAmount)
	require.Equal(t, res.Receipt.Amount, res.Receipt.AppliedAmount+res.Receipt.UnappliedAmount+res.Receipt.DifferenceAmount)
	var payload []byte
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT request_payload FROM ar_accounting_posting_requests WHERE tenant_id = $1 AND source_event_id = $2`, e.tenant, res.Application.ApplicationID).Scan(&payload))
	var ev kit.PostingEvent
	require.NoError(t, json.Unmarshal(payload, &ev))
	require.Equal(t, KeyBankFee, ev.Lines[2].MappingKey)
	// a manual difference without evidence is refused
	r2 := e.register("100.00", "")
	e.batch(r2)
	item3, _ := e.item(e.entity, e.party, "100.00", "USD", "2026-10-22")
	code, b2 := e.post("applyReceipt", pClerk, map[string]any{"batch_id": e.batchOf(r2), "receipt_id": r2.ReceiptID, "expected_version": 1, "mode": "MANUAL", "reason_code": "X",
		"difference_amount": "5.00", "difference_kind": "BANK_FEE", "lines": []map[string]any{{"open_item_id": item3, "amount": "10.00"}}})
	require.Equal(t, http.StatusBadRequest, code, string(b2))
	require.Equal(t, "EVIDENCE_REQUIRED", errCode(t, b2))
}

func (e *env) batchOf(r *Receipt) string {
	var id string
	require.NoError(e.t, e.tx(func(tx pgx.Tx) (err error) {
		id, err = batchOfReceipt(context.Background(), tx, e.tenant, r.ReceiptID)
		return
	}))
	return id
}

// ── #25 ──────────────────────────────────────────────────────────────────────

func TestNeg25_ManualMatchExceedsAuthority(t *testing.T) {
	e := newEnv(t)
	itemID, _ := e.item(e.entity, e.party, "500.00", "USD", "2026-10-20")
	rid := uuid.NewString()
	e.src.put(e.tenant, rid, "500.00", "USD", BankNormalized)
	var r Receipt // registered by a principal who ALSO holds the approve action
	require.NoError(t, json.Unmarshal(e.must(http.StatusCreated, "registerConfirmedReceipt", pRegAppr, map[string]any{"legal_entity_id": e.entity, "bank_transaction_id": rid, "customer_party_id": e.party}), &r))
	b := e.batch(&r)
	lines := []map[string]any{{"open_item_id": itemID, "amount": "500.00"}}
	propose := func(principal string) []byte {
		return e.must(http.StatusAccepted, "applyReceipt", principal, map[string]any{"batch_id": b.BatchID, "receipt_id": r.ReceiptID, "expected_version": 1, "mode": "MANUAL", "reason_code": "REMITTANCE_ADVICE", "lines": lines})
	}
	var prop struct{ Match ManualMatch }
	require.NoError(t, json.Unmarshal(propose(pClerkAppr), &prop))
	// a proposal moves no money
	require.Equal(t, kit.MustAmount("500"), e.residual(itemID))
	require.Equal(t, 0, e.n(`SELECT count(*) FROM ar08_applications WHERE tenant_id = $1`, e.tenant))
	require.Equal(t, BatchProposed, e.batchRow(b.BatchID).State)
	approve := map[string]any{"match_id": prop.Match.MatchID, "expected_version": 1}

	e.refused(http.StatusForbidden, "FORBIDDEN", "approveManualMatch", pClerk, approve)                 // manage authority is not approve authority
	e.refused(http.StatusForbidden, "SEGREGATION_OF_DUTIES", "approveManualMatch", pClerkAppr, approve) // the proposer cannot approve their own match
	e.refused(http.StatusForbidden, "SEGREGATION_OF_DUTIES", "approveManualMatch", pRegAppr, approve)   // the receipt's registrant gains no override
	e.refused(http.StatusForbidden, "FORBIDDEN", "approveManualMatch", pNobody, approve)
	e.refused(http.StatusBadRequest, "EXPECTED_VERSION_REQUIRED", "approveManualMatch", pApprover, map[string]any{"match_id": prop.Match.MatchID})
	e.refused(http.StatusConflict, "STALE_VERSION", "approveManualMatch", pApprover, map[string]any{"match_id": prop.Match.MatchID, "expected_version": 7})
	require.Equal(t, kit.MustAmount("500"), e.residual(itemID))
	// a second proposal while one is pending is refused
	code, body := e.post("applyReceipt", pClerk, map[string]any{"batch_id": b.BatchID, "receipt_id": r.ReceiptID, "expected_version": 1, "mode": "MANUAL", "reason_code": "X", "lines": lines})
	require.Equal(t, http.StatusConflict, code, string(body))

	res := decodeApplied(t, e.must(http.StatusCreated, "approveManualMatch", pApprover, approve))
	require.Equal(t, pApprover, res.Application.ApprovedBy)
	require.Equal(t, pClerkAppr, res.Application.AppliedBy, "the proposer is the applier; the approver is recorded separately")
	require.Equal(t, kit.Amount(0), e.residual(itemID))
	require.Equal(t, MatchApplied, e.matchStatus(prop.Match.MatchID))
	// the proposer / approver cannot later unapply it either
	e.refused(http.StatusForbidden, "FORBIDDEN", "unapplyReceipt", pApprover, map[string]any{"receipt_id": r.ReceiptID, "expected_version": 2, "reason_code": "X", "evidence_id": "e"})
}

func TestNeg25_HighValueAndRelatedPartyNeedIndependentApproval(t *testing.T) {
	e := newEnv(t)
	itemID, doc := e.item(e.entity, e.party, "20000.00", "USD", "2026-10-20")
	r := e.register("20000.00", "ref "+doc)
	b := e.batch(r)
	e.generate(b, r)
	e.refused(http.StatusUnprocessableEntity, CodeManualApprovalRequired, "applyReceipt", pClerk, map[string]any{"batch_id": b.BatchID, "receipt_id": r.ReceiptID, "expected_version": 1, "mode": "AUTO"})
	require.Equal(t, kit.MustAmount("20000"), e.residual(itemID))
	// a high-value manual match needs evidence
	e.refused(http.StatusBadRequest, "EVIDENCE_REQUIRED", "applyReceipt", pClerk, map[string]any{"batch_id": b.BatchID, "receipt_id": r.ReceiptID, "expected_version": 1, "mode": "MANUAL",
		"reason_code": "X", "lines": []map[string]any{{"open_item_id": itemID, "amount": "20000.00"}}})

	// related-party receipt: small amount, perfect match, still needs approval
	id := uuid.NewString()
	e.src.put(e.tenant, id, "50.00", "USD", BankNormalized)
	it2, doc2 := e.item(e.entity, e.party, "50.00", "USD", "2026-10-21")
	var rp Receipt
	require.NoError(t, json.Unmarshal(e.must(http.StatusCreated, "registerConfirmedReceipt", pRegistrar, map[string]any{"legal_entity_id": e.entity, "bank_transaction_id": id,
		"customer_party_id": e.party, "remittance_text": doc2, "related_party": true}), &rp))
	b2 := e.batch(&rp)
	e.generate(b2, &rp)
	e.refused(http.StatusUnprocessableEntity, CodeManualApprovalRequired, "applyReceipt", pClerk, map[string]any{"batch_id": b2.BatchID, "receipt_id": rp.ReceiptID, "expected_version": 1, "mode": "AUTO"})
	require.Equal(t, kit.MustAmount("50"), e.residual(it2))
}

// ── #26 ──────────────────────────────────────────────────────────────────────

func TestNeg26_TenantAndEntityBoundary(t *testing.T) {
	e := newEnv(t)
	otherEntity := uuid.NewString()
	_, r, b := e.ready("500.00")
	foreignItem, _ := e.item(otherEntity, e.party, "500.00", "USD", "2026-10-20") // same tenant, different legal entity

	// a body entity that disagrees with the receipt's entity is refused
	e.refused(http.StatusForbidden, "TENANT_SCOPE_VIOLATION", "applyReceipt", pClerk, map[string]any{"legal_entity_id": otherEntity, "batch_id": b.BatchID, "receipt_id": r.ReceiptID, "expected_version": 1, "mode": "AUTO"})
	// an open item of another legal entity is refused
	e.refused(http.StatusForbidden, "TENANT_SCOPE_VIOLATION", "applyReceipt", pClerk, map[string]any{"batch_id": b.BatchID, "receipt_id": r.ReceiptID, "expected_version": 1, "mode": "MANUAL",
		"reason_code": "X", "lines": []map[string]any{{"open_item_id": foreignItem, "amount": "500.00"}}})
	// a receipt of another entity cannot join this entity's batch
	e.refused(http.StatusForbidden, "TENANT_SCOPE_VIOLATION", "createApplicationBatch", pClerk, map[string]any{"legal_entity_id": otherEntity, "currency": "USD", "receipt_ids": []string{r.ReceiptID}})
	require.Equal(t, kit.MustAmount("500"), e.residual(foreignItem))

	// another tenant sees nothing: receipts and items look absent
	t2 := newEnv(t)
	code, _ := call2(t, e, t2.tenant, "applyReceipt", pClerk, map[string]any{"batch_id": b.BatchID, "receipt_id": r.ReceiptID, "expected_version": 1, "mode": "AUTO"})
	require.Equal(t, http.StatusNotFound, code)
	code, _ = e.get("getApplication?receipt_id="+r.ReceiptID, pClerk, t2.tenant)
	require.Equal(t, http.StatusNotFound, code)
	code, _ = e.get("getMatchCandidates?receipt_id="+r.ReceiptID, pClerk, t2.tenant)
	require.Equal(t, http.StatusNotFound, code)
	code, body := e.get("getUnappliedCash", pClerk, t2.tenant)
	require.Equal(t, http.StatusOK, code)
	require.NotContains(t, string(body), r.ReceiptID)
	code, _ = e.get("getApplicationReconciliation?batch_id="+b.BatchID, pClerk, t2.tenant)
	require.Equal(t, http.StatusNotFound, code)
	// a foreign tenant's open item inside this tenant's receipt: not found
	t2item, _ := t2.item(t2.entity, t2.party, "500.00", "USD", "2026-10-20")
	e.refused(http.StatusNotFound, "NOT_FOUND", "applyReceipt", pClerk, map[string]any{"batch_id": b.BatchID, "receipt_id": r.ReceiptID, "expected_version": 1, "mode": "MANUAL",
		"reason_code": "X", "lines": []map[string]any{{"open_item_id": t2item, "amount": "500.00"}}})
}

func call2(t *testing.T, e *env, tenant, op, principal string, body map[string]any) (int, []byte) {
	code, b, _ := call(t, e.h, "POST", base+"commands/"+op, tenant, principal, "", body)
	return code, b
}

func TestCrossCurrencyIsNotImplemented(t *testing.T) {
	e := newEnv(t)
	eur, _ := e.item(e.entity, e.party, "100.00", "EUR", "2026-10-20")
	r := e.register("100.00", "")
	b := e.batch(r)
	code, body := e.post("applyReceipt", pClerk, map[string]any{"batch_id": b.BatchID, "receipt_id": r.ReceiptID, "expected_version": 1, "mode": "MANUAL", "reason_code": "X",
		"lines": []map[string]any{{"open_item_id": eur, "amount": "100.00"}}})
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	require.Equal(t, "VALIDATION_FAILED", errCode(t, body))
	require.Contains(t, string(body), "cross-currency")
	require.Equal(t, kit.MustAmount("100"), e.residual(eur))
	e.refused(http.StatusUnprocessableEntity, "VALIDATION_FAILED", "createApplicationBatch", pClerk, map[string]any{"legal_entity_id": e.entity, "currency": "EUR", "receipt_ids": []string{e.register("10.00", "").ReceiptID}})
}

// ── atomicity ────────────────────────────────────────────────────────────────

type flakyItems struct {
	kit.OpenItems
	failAfter int
	n         int
}

func (f *flakyItems) ApplyAdjustment(ctx context.Context, tx pgx.Tx, c kit.Caller, in kit.OpenItemAdjustment) (*kit.OpenItemView, error) {
	f.n++
	if f.n > f.failAfter {
		return nil, errors.New("injected AR-07 failure")
	}
	return f.OpenItems.ApplyAdjustment(ctx, tx, c, in)
}

func TestAtomicRollback_FailingOpenItemsLeavesNothing(t *testing.T) {
	e := newEnv(t)
	i1, _ := e.item(e.entity, e.party, "100.00", "USD", "2026-10-20")
	i2, _ := e.item(e.entity, e.party, "100.00", "USD", "2026-10-21")
	r := e.register("200.00", "")
	b := e.batch(r)
	out := e.must(http.StatusAccepted, "applyReceipt", pClerk, map[string]any{"batch_id": b.BatchID, "receipt_id": r.ReceiptID, "expected_version": 1, "mode": "MANUAL", "reason_code": "X",
		"lines": []map[string]any{{"open_item_id": i1, "amount": "100.00"}, {"open_item_id": i2, "amount": "100.00"}}})
	var prop struct{ Match ManualMatch }
	require.NoError(t, json.Unmarshal(out, &prop))

	real := e.m.items
	e.m.items = &flakyItems{OpenItems: real, failAfter: 1} // the first line's adjustment succeeds inside the tx, the second fails
	code, body := e.post("approveManualMatch", pApprover, map[string]any{"match_id": prop.Match.MatchID, "expected_version": 1})
	require.Equal(t, http.StatusServiceUnavailable, code, string(body))
	e.m.items = real

	apps, lines, postings, adj := e.counts(r.ReceiptID)
	require.Equal(t, []int{0, 0, 0, 0}, []int{apps, lines, postings, adj})
	require.Equal(t, 0, e.events(EvApplied))
	require.Equal(t, kit.MustAmount("100"), e.residual(i1), "the first line's adjustment was rolled back with the rest")
	require.Equal(t, kit.MustAmount("100"), e.residual(i2))
	cur := e.receipt(r.ReceiptID)
	require.Equal(t, 1, cur.Version)
	require.Equal(t, RcptUnapplied, cur.State)
	require.Equal(t, MatchPending, e.matchStatus(prop.Match.MatchID))
	// the retry (infrastructure recovered) now succeeds, once
	e.must(http.StatusCreated, "approveManualMatch", pApprover, map[string]any{"match_id": prop.Match.MatchID, "expected_version": 1})
	apps, _, _, adj = e.counts(r.ReceiptID)
	require.Equal(t, []int{1, 2}, []int{apps, adj})
}

func (e *env) matchStatus(id string) string {
	var s string
	require.NoError(e.t, pool.QueryRow(context.Background(), `SELECT status FROM ar08_manual_matches WHERE tenant_id = $1 AND match_id = $2`, e.tenant, id).Scan(&s))
	return s
}

func TestPeriodClosedRefusesApplicationAndChangesNothing(t *testing.T) {
	e := newEnv(t)
	itemID, r, b := e.ready("500.00")
	e.per.setErr(kit.Conflict(kit.CodePeriodClosed, "closed"))
	code, body := e.autoApply(pClerk, b, r)
	require.Equal(t, http.StatusConflict, code, string(body))
	require.Equal(t, "PERIOD_CLOSED", errCode(t, body))
	apps, _, postings, _ := e.counts(r.ReceiptID)
	require.Equal(t, []int{0, 0}, []int{apps, postings})
	require.Equal(t, kit.MustAmount("500"), e.residual(itemID))
}

// ── unapply / reapply ────────────────────────────────────────────────────────

func TestUnapplyReapply_GovernedReversal(t *testing.T) {
	e := newEnv(t)
	itemID, r, b := e.ready("500.00")
	code, body := e.autoApply(pClerkUnap, b, r) // applied by a principal who also holds the unapply action
	require.Equal(t, http.StatusCreated, code, string(body))
	app := decodeApplied(t, body).Application
	unapply := func(principal string, version int, reason, evidence string) (int, []byte) {
		return e.post("unapplyReceipt", principal, map[string]any{"receipt_id": r.ReceiptID, "expected_version": version, "reason_code": reason, "evidence_id": evidence})
	}
	// authority + governance
	code, body = unapply(pClerk, 2, "CORRECTION", "ev")
	require.Equal(t, http.StatusForbidden, code, string(body))
	require.Equal(t, "FORBIDDEN", errCode(t, body), "unapply is restricted")
	code, body = unapply(pClerkUnap, 2, "CORRECTION", "ev")
	require.Equal(t, "SEGREGATION_OF_DUTIES", errCode(t, body), "the applier cannot unapply their own application")
	code, body = unapply(pUnapplier, 2, "", "ev")
	require.Equal(t, "REASON_REQUIRED", errCode(t, body))
	code, body = unapply(pUnapplier, 2, "CORRECTION", "")
	require.Equal(t, "EVIDENCE_REQUIRED", errCode(t, body))
	code, body = unapply(pUnapplier, 9, "CORRECTION", "ev")
	require.Equal(t, "STALE_VERSION", errCode(t, body))
	e.per.setErr(kit.Conflict(kit.CodePeriodClosed, "closed"))
	code, body = unapply(pUnapplier, 2, "CORRECTION", "ev")
	require.Equal(t, http.StatusConflict, code, string(body))
	require.Equal(t, "PERIOD_CLOSED", errCode(t, body))
	e.per.setErr(nil)
	require.Equal(t, kit.Amount(0), e.residual(itemID), "nothing changed by the refusals")
	// a failed forward posting must be resolved before it is reversed
	_, err := pool.Exec(context.Background(), `UPDATE ar08_applications SET accounting_state = 'FAILED' WHERE application_id = $1`, app.ApplicationID)
	require.NoError(t, err)
	code, body = unapply(pUnapplier, 2, "CORRECTION", "ev")
	require.Equal(t, "INVALID_STATE_TRANSITION", errCode(t, body))
	_, err = pool.Exec(context.Background(), `UPDATE ar08_applications SET accounting_state = 'POSTED' WHERE application_id = $1`, app.ApplicationID)
	require.NoError(t, err)

	code, body = unapply(pUnapplier, 2, "CORRECTION", "ev")
	require.Equal(t, http.StatusOK, code, string(body))
	res := decodeApplied(t, body)
	require.Equal(t, AppUnapplied, res.Application.Status)
	require.Equal(t, RcptUnapplied, res.Receipt.State)
	require.Equal(t, kit.MustAmount("500"), res.Receipt.UnappliedAmount)
	require.Equal(t, kit.MustAmount("500"), e.residual(itemID), "the residual is restored through a CASH_UNAPPLICATION adjustment")
	require.Equal(t, 1, e.n(`SELECT count(*) FROM ar07_adjustments WHERE tenant_id = $1 AND kind = 'CASH_UNAPPLICATION'`, e.tenant))
	require.Equal(t, 1, e.n(`SELECT count(*) FROM ar_accounting_posting_requests WHERE tenant_id = $1 AND source_event_id = $2`, e.tenant, app.ApplicationID+":unapply"))
	require.Equal(t, 1, e.events(EvUnapplied))
	require.Equal(t, 1, e.events(EvUnappliedByCorrection))
	var payload []byte
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT request_payload FROM ar_accounting_posting_requests WHERE tenant_id = $1 AND source_event_id = $2`, e.tenant, app.ApplicationID+":unapply").Scan(&payload))
	var ev kit.PostingEvent
	require.NoError(t, json.Unmarshal(payload, &ev))
	require.InDelta(t, 500.0, ev.Lines[0].CreditAmount, 0.0001, "the reversal credits the cash receipt")
	require.InDelta(t, 500.0, ev.Lines[1].DebitAmount, 0.0001, "and debits the receivable")
	require.Equal(t, BatchProposed, e.batchRow(b.BatchID).State)
	// no second unapply
	code, body = unapply(pUnapplier, 3, "CORRECTION", "ev")
	require.Equal(t, "INVALID_STATE_TRANSITION", errCode(t, body))

	// reapply: restricted, by someone other than the original applier, creating a NEW application
	reapply := func(principal string) (int, []byte) {
		return e.post("reapplyReceipt", principal, map[string]any{"receipt_id": r.ReceiptID, "expected_version": 3, "reason_code": "CORRECTED_ALLOCATION", "evidence_id": "ev-2",
			"lines": []map[string]any{{"open_item_id": itemID, "amount": "500.00"}}})
	}
	code, body = reapply(pClerk)
	require.Equal(t, "FORBIDDEN", errCode(t, body))
	code, body = reapply(pClerkReap)
	require.Equal(t, http.StatusCreated, code, string(body))
	re := decodeApplied(t, body)
	require.Equal(t, ModeReapply, re.Application.Mode)
	require.Equal(t, app.ApplicationID, re.Application.Reapplies)
	require.NotEqual(t, app.ApplicationID, re.Application.ApplicationID)
	require.Equal(t, kit.Amount(0), e.residual(itemID))
	require.Equal(t, 2, e.n(`SELECT count(*) FROM ar08_applications WHERE tenant_id = $1 AND receipt_id = $2`, e.tenant, r.ReceiptID))
	require.Equal(t, 1, e.n(`SELECT count(*) FROM ar08_applications WHERE tenant_id = $1 AND receipt_id = $2 AND status = 'APPLIED'`, e.tenant, r.ReceiptID))
	require.Equal(t, BatchFull, e.batchRow(b.BatchID).State)
	// reapply by the maker of the reversed application is refused (SoD)
	code, body = e.post("unapplyReceipt", pUnapplier, map[string]any{"receipt_id": r.ReceiptID, "expected_version": 4, "reason_code": "CORRECTION", "evidence_id": "ev"})
	require.Equal(t, http.StatusOK, code, string(body))
	code, body = e.post("reapplyReceipt", pClerkReap, map[string]any{"receipt_id": r.ReceiptID, "expected_version": 5, "reason_code": "X", "evidence_id": "ev",
		"lines": []map[string]any{{"open_item_id": itemID, "amount": "500.00"}}})
	require.Equal(t, http.StatusForbidden, code, string(body))
	require.Equal(t, "SEGREGATION_OF_DUTIES", errCode(t, body))
}

// ── reconciliation ───────────────────────────────────────────────────────────

func TestReconcileApplicationBatch_TotalsAndExceptions(t *testing.T) {
	e := newEnv(t)
	i1, doc1 := e.item(e.entity, e.party, "500.00", "USD", "2026-10-20")
	r1 := e.register("500.00", doc1)
	_, doc2 := e.item(e.entity, e.party, "300.00", "USD", "2026-10-21")
	r2 := e.register("400.00", doc2) // 300 applied, 100 unapplied
	b := e.batch(r1, r2)
	e.generate(b, r1)
	e.generate(b, r2)
	require.Equal(t, http.StatusCreated, func() int { c, _ := e.autoApply(pClerk, b, r1); return c }())
	require.Equal(t, http.StatusCreated, func() int { c, _ := e.autoApply(pClerk, b, r2); return c }())
	require.Equal(t, BatchPartial, e.batchRow(b.BatchID).State, "one receipt still carries unapplied cash")
	cur := e.batchRow(b.BatchID)

	e.refused(http.StatusBadRequest, "EXPECTED_VERSION_REQUIRED", "reconcileApplicationBatch", pClerk, map[string]any{"batch_id": b.BatchID})
	e.refused(http.StatusConflict, "STALE_VERSION", "reconcileApplicationBatch", pClerk, map[string]any{"batch_id": b.BatchID, "expected_version": 99})
	out := e.must(http.StatusOK, "reconcileApplicationBatch", pClerk, map[string]any{"batch_id": b.BatchID, "expected_version": cur.Version})
	var rec Reconciliation
	require.NoError(t, json.Unmarshal(out, &rec))
	require.Equal(t, "RECONCILED", rec.Status, string(out))
	require.Empty(t, rec.Findings)
	require.Equal(t, kit.MustAmount("900"), rec.ReceiptsTotal)
	require.Equal(t, kit.MustAmount("800"), rec.AppliedTotal)
	require.Equal(t, kit.MustAmount("100"), rec.UnappliedTotal)
	require.Equal(t, rec.ReceiptsTotal, rec.AppliedTotal+rec.UnappliedTotal+rec.DifferenceTotal)
	require.Equal(t, BatchReconciled, e.batchRow(b.BatchID).State)
	require.Equal(t, 1, e.events(EvReconciled))
	require.Equal(t, 1, e.n(`SELECT count(*) FROM ar08_reconciliations WHERE tenant_id = $1 AND status = 'RECONCILED'`, e.tenant))
	code, body := e.get("getApplicationReconciliation?batch_id="+b.BatchID, pClerk, e.tenant)
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, string(body), `"balanced":true`)

	// the reconciliation record is append-only
	_, err := pool.Exec(context.Background(), `UPDATE ar08_reconciliations SET status = 'EXCEPTION' WHERE tenant_id = $1`, e.tenant)
	require.Error(t, err)

	// an application without its accounting posting is a visible control exception, never plugged
	_, err = pool.Exec(context.Background(), `DELETE FROM ar_accounting_posting_requests WHERE tenant_id = $1 AND source_type = 'CASH_APPLICATION'`, e.tenant)
	require.NoError(t, err)
	cur = e.batchRow(b.BatchID)
	out = e.must(http.StatusOK, "reconcileApplicationBatch", pClerk, map[string]any{"batch_id": b.BatchID, "expected_version": cur.Version})
	require.NoError(t, json.Unmarshal(out, &rec))
	require.Equal(t, "EXCEPTION", rec.Status)
	require.Equal(t, "NO_POSTING", rec.Findings[0].Code)
	require.Equal(t, BatchPartial, e.batchRow(b.BatchID).State, "an unreconciled batch is no longer RECONCILED")
	_, body = e.get("getApplication?receipt_id="+r1.ReceiptID, pClerk, e.tenant)
	require.Contains(t, string(body), "NO_POSTING")
	_ = i1
}

func TestReconcile_ApplyAfterReconciliationReopensBatch(t *testing.T) {
	e := newEnv(t)
	_, r, b := e.ready("500.00")
	e.autoApply(pClerk, b, r)
	out := e.must(http.StatusOK, "reconcileApplicationBatch", pClerk, map[string]any{"batch_id": b.BatchID, "expected_version": e.batchRow(b.BatchID).Version})
	require.Contains(t, string(out), `"RECONCILED"`)
	require.Equal(t, BatchReconciled, e.batchRow(b.BatchID).State)
	e.must(http.StatusOK, "unapplyReceipt", pUnapplier, map[string]any{"receipt_id": r.ReceiptID, "expected_version": 2, "reason_code": "CUSTOMER_REFUND", "evidence_id": "e"})
	require.NotEqual(t, BatchReconciled, e.batchRow(b.BatchID).State)
	require.Equal(t, 0, e.events(EvUnappliedByCorrection), "only reason CORRECTION raises CashUnappliedByCorrection")
}

// ── immutability, listener, reads ────────────────────────────────────────────

func TestImmutability_TriggersRefuseRewrites(t *testing.T) {
	e := newEnv(t)
	_, r, b := e.ready("500.00")
	_, body := e.autoApply(pClerk, b, r)
	app := decodeApplied(t, body).Application
	ctx := context.Background()
	for name, sql := range map[string]string{
		"application amount":  `UPDATE ar08_applications SET applied_amount = 1 WHERE application_id = '` + app.ApplicationID + `'`,
		"application applier": `UPDATE ar08_applications SET applied_by = 'someone' WHERE application_id = '` + app.ApplicationID + `'`,
		"application delete":  `DELETE FROM ar08_applications WHERE application_id = '` + app.ApplicationID + `'`,
		"line update":         `UPDATE ar08_application_lines SET amount = 1 WHERE application_id = '` + app.ApplicationID + `'`,
		"line delete":         `DELETE FROM ar08_application_lines WHERE application_id = '` + app.ApplicationID + `'`,
		"receipt amount":      `UPDATE ar08_receipts SET amount = 1, unapplied_amount = 1 WHERE receipt_id = '` + r.ReceiptID + `'`,
		"receipt bank txn":    `UPDATE ar08_receipts SET bank_transaction_id = gen_random_uuid() WHERE receipt_id = '` + r.ReceiptID + `'`,
		"receipt delete":      `DELETE FROM ar08_receipts WHERE receipt_id = '` + r.ReceiptID + `'`,
		"candidate update":    `UPDATE ar08_candidates SET score = 0 WHERE receipt_id = '` + r.ReceiptID + `'`,
	} {
		_, err := pool.Exec(ctx, sql)
		require.Error(t, err, name)
		require.Error(t, kit.MapPgErr(err), name)
	}
	// a reversed application can never be reinstated
	e.must(http.StatusOK, "unapplyReceipt", pUnapplier, map[string]any{"receipt_id": r.ReceiptID, "expected_version": 2, "reason_code": "X", "evidence_id": "e"})
	_, err := pool.Exec(ctx, `UPDATE ar08_applications SET status = 'APPLIED' WHERE application_id = $1`, app.ApplicationID)
	require.Error(t, err)
	var p *kit.Problem
	require.ErrorAs(t, kit.MapPgErr(err), &p)
	require.Equal(t, "DOCUMENT_IMMUTABLE", p.Code)
}

func TestPostingListener_MirrorsOutcomeAndVisibleException(t *testing.T) {
	e := newEnv(t)
	_, r, b := e.ready("500.00")
	_, body := e.autoApply(pClerk, b, r)
	app := decodeApplied(t, body).Application
	q := kit.QueuedPosting{TenantID: e.tenant, SourceType: kit.SourceCashApplication, SourceID: app.ApplicationID, SourceEventID: app.ApplicationID, CorrelationID: "c"}
	ctx := context.Background()
	require.NoError(t, e.m.OnPostingOutcome(ctx, q, kit.PostingOutcome{Status: kit.PostingPending}))
	require.NoError(t, e.m.OnPostingOutcome(ctx, q, kit.PostingOutcome{Status: kit.PostingPosted, JournalID: "J-1", ExecutionID: "X-1"}))
	_, got := e.get("getApplication?application_id="+app.ApplicationID, pClerk, e.tenant)
	require.Contains(t, string(got), `"journal_id":"J-1"`)
	require.Contains(t, string(got), `"accounting_state":"POSTED"`)
	require.NoError(t, e.m.OnPostingOutcome(ctx, q, kit.PostingOutcome{Status: kit.PostingQuarantined, Error: "no mapping"}))
	_, got = e.get("getApplication?application_id="+app.ApplicationID, pClerk, e.tenant)
	require.Contains(t, string(got), `"accounting_state":"QUARANTINED"`)
	require.Contains(t, string(got), "no mapping")
	require.NoError(t, e.m.OnPostingOutcome(ctx, q, kit.PostingOutcome{Status: kit.PostingPosted, JournalID: "J-1"}))
	// unapply outcome lands on the unapply dimension
	e.must(http.StatusOK, "unapplyReceipt", pUnapplier, map[string]any{"receipt_id": r.ReceiptID, "expected_version": 2, "reason_code": "X", "evidence_id": "e"})
	uq := kit.QueuedPosting{TenantID: e.tenant, SourceType: kit.SourceCashUnapply, SourceID: app.ApplicationID, SourceEventID: app.ApplicationID + ":unapply"}
	require.NoError(t, e.m.OnPostingOutcome(ctx, uq, kit.PostingOutcome{Status: kit.PostingPosted, JournalID: "J-2"}))
	_, got = e.get("getApplication?application_id="+app.ApplicationID, pClerk, e.tenant)
	require.Contains(t, string(got), `"unapply_journal_id":"J-2"`)
	// an unknown application is re-delivered (error), another source type is ignored
	require.Error(t, e.m.OnPostingOutcome(ctx, kit.QueuedPosting{TenantID: e.tenant, SourceType: kit.SourceCashApplication, SourceID: uuid.NewString()}, kit.PostingOutcome{Status: kit.PostingPosted}))
	require.NoError(t, e.m.OnPostingOutcome(ctx, kit.QueuedPosting{TenantID: e.tenant, SourceType: kit.SourceInvoice, SourceID: "x"}, kit.PostingOutcome{Status: kit.PostingPosted}))
}

func TestGetAvailableActions_DerivedFromStateAndSoD(t *testing.T) {
	e := newEnv(t)
	itemID, r, b := e.ready("500.00")
	actions := func(principal string) map[string]actionState {
		code, body := e.get("getAvailableActions?receipt_id="+r.ReceiptID, principal, e.tenant)
		require.Equal(t, http.StatusOK, code, string(body))
		var out struct{ Actions []actionState }
		require.NoError(t, json.Unmarshal(body, &out))
		m := map[string]actionState{}
		for _, a := range out.Actions {
			m[a.Action] = a
		}
		return m
	}
	a := actions(pClerk)
	require.True(t, a["ApplyReceipt"].Available)
	require.False(t, a["UnapplyReceipt"].Available, "nothing to unapply yet")
	require.False(t, a["ApproveManualMatch"].Available)
	require.False(t, actions(pNobody)["ApplyReceipt"].Available, "legal but not permitted")
	require.Contains(t, actions(pNobody)["ApplyReceipt"].Reason, "lacks")
	code, body := e.autoApply(pClerkUnap, b, r)
	require.Equal(t, http.StatusCreated, code, string(body))
	a = actions(pClerk)
	require.False(t, a["ApplyReceipt"].Available, "already applied")
	require.True(t, actions(pUnapplier)["UnapplyReceipt"].Available)
	require.False(t, actions(pClerkUnap)["UnapplyReceipt"].Available, "the applier cannot unapply")
	_ = itemID
}
