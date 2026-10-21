//go:build integration

// Real-Postgres tests for AR-10 (the real AR-07 open items and AR-08 receipts/holds are used; the payment path, destination
// verification and period gate are fakes that COUNT external effects). Run (serially):
//
//	go test -tags=integration -count=1 -p 1 -timeout=600s ./internal/ar10/...
//
// The embedded superuser bypasses RLS, so isolation tests prove the explicit tenant_id predicates, not the policies.
// Spec section 18 scenarios carry their number: TestNeg29 (collector self-approves write-off), TestNeg30 (refund destination changed
// after approval), TestNeg31 (provider timeout triggers duplicate refund), TestNeg32 (refund settles but AR remains open without
// exception), TestNeg33 (write-off above delegated limit), TestNeg34 (write-off tax implication ignored), TestNeg35 (accounting
// event missing).
package ar10

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
	"zoiko.io/accounts-receivable-svc/internal/ar08"
	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/kit/kittest"
	svcmiddleware "zoiko.io/accounts-receivable-svc/internal/middleware"
)

var pool *pgxpool.Pool

func TestMain(m *testing.M) {
	var stop func()
	pool, stop = kittest.Start("ar10_test", 18000)
	code := m.Run()
	stop()
	os.Exit(code)
}

const (
	pResolver = "resolver" // resolves AR-07 disputes and holds write-off tier 3
	pDisputer = "disputer" // opens AR-07 disputes
	pHolder   = "holder"   // places AR-07 collection holds
)

// fakeBank stands in for Banking (banking-connector-svc) for AR-08 receipt registration.
type fakeBank struct {
	mu   sync.Mutex
	txns map[string]*ar08.BankTransaction
}

func (f *fakeBank) put(tenant, amount string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := uuid.NewString()
	f.txns[id] = &ar08.BankTransaction{TransactionID: id, TenantID: tenant, StatementLineID: uuid.NewString(), Currency: "USD", Status: ar08.BankNormalized,
		Counterparty: "ACME PAYER", Amount: kit.MustAmount(amount), TransactionDate: time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)}
	return id
}

func (f *fakeBank) Get(_ context.Context, _ kit.Caller, id string) (*ar08.BankTransaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.txns[id]
	if !ok {
		return nil, ar08.ErrBankTxnNotFound
	}
	c := *t
	return &c, nil
}

// failingItems runs the real adjustment and THEN fails: proves the adjustment rolls back with the command.
type failingItems struct{ kit.OpenItems }

func (f failingItems) ApplyAdjustment(ctx context.Context, tx pgx.Tx, c kit.Caller, in kit.OpenItemAdjustment) (*kit.OpenItemView, error) {
	if _, err := f.OpenItems.ApplyAdjustment(ctx, tx, c, in); err != nil {
		return nil, err
	}
	return nil, errors.New("boom after the adjustment")
}

// flakyReceipts runs the real consume and THEN fails.
type flakyReceipts struct {
	ReceiptCashPort
	failConsume bool
	hold        string // when set, UnappliedHoldStatus reports this status
}

func (f flakyReceipts) ConsumeUnapplied(ctx context.Context, tx pgx.Tx, c kit.Caller, owner string) error {
	if err := f.ReceiptCashPort.ConsumeUnapplied(ctx, tx, c, owner); err != nil {
		return err
	}
	if f.failConsume {
		return errors.New("boom after the consume")
	}
	return nil
}

func (f flakyReceipts) UnappliedHoldStatus(ctx context.Context, tenantID, owner string) (string, bool, error) {
	if f.hold != "" {
		return f.hold, true, nil
	}
	return f.ReceiptCashPort.UnappliedHoldStatus(ctx, tenantID, owner)
}

type env struct {
	t              *testing.T
	m              *Module
	m7             *ar07.Module
	m8             *ar08.Module
	h, h7, h8      http.Handler
	tenant, entity string
	party          string
	gw             *fakeGateway
	dest           *fakeDest
	per            *fakePeriods
	bank           *fakeBank
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, tenant: uuid.NewString(), entity: uuid.NewString(), party: uuid.NewString(), gw: newFakeGateway(), dest: newFakeDest(), per: &fakePeriods{},
		bank: &fakeBank{txns: map[string]*ar08.BankTransaction{}}}
	g := standardGrants()
	g[pResolver] = map[string]bool{ActionWriteOffApproveT3: true}
	deps := testDeps(&fakeAuthz{grants: g})
	deps.DB = kit.NewDB(pool)

	g7 := map[string]map[string]bool{
		"boss":     {ar07.ActionReceivableManage: true, ar07.ActionDisputeManage: true, ar07.ActionHoldManage: true, ar08.ActionReceiptRegister: true},
		pDisputer:  {ar07.ActionDisputeManage: true},
		pResolver:  {ar07.ActionDisputeManage: true},
		pHolder:    {ar07.ActionHoldManage: true},
		pCollector: {ar07.ActionHoldManage: true, ar07.ActionDisputeManage: true},
	}
	deps7 := testDeps(&fakeAuthz{grants: g7})
	deps7.DB = kit.NewDB(pool)
	e.m7 = ar07.New(deps7)
	e.m8 = ar08.New(deps7, e.bank, e.m7, e.m7, e.per)
	e.m = New(deps, e.m7, e.per, e.m8, e.gw, e.dest, e.m7, nil)
	e.h = testRouter(e.m)
	r7 := chi.NewRouter()
	r7.Use(svcmiddleware.TenantContext())
	e.m7.Register(r7)
	e.h7 = r7
	r8 := chi.NewRouter()
	r8.Use(svcmiddleware.TenantContext())
	e.m8.Register(r8)
	e.h8 = r8
	e.dest.put("dest-1", e.party, "USD", "fp-1", "ACTIVE")
	return e
}

func (e *env) caller() kit.Caller {
	return kit.Caller{TenantID: e.tenant, PrincipalID: "boss", CorrelationID: "corr-" + uuid.NewString(), LegalEntityID: e.entity}
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

func (e *env) get(path, principal, tenant string) (int, []byte) {
	code, b, _ := call(e.t, e.h, "GET", base+path, tenant, principal, "", nil)
	return code, b
}

func (e *env) n(sql string, args ...any) int {
	e.t.Helper()
	var n int
	require.NoError(e.t, pool.QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}

func (e *env) events(eventType string) int {
	return e.n(`SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = $2`, e.tenant, eventType)
}

func (e *env) audits(aggID, command string) int {
	return e.n(`SELECT count(*) FROM ar_audit_events WHERE tenant_id = $1 AND aggregate_id = $2 AND command = $3`, e.tenant, aggID, command)
}

func (e *env) posting(sourceEventID string) (kit.PostingEvent, bool) {
	e.t.Helper()
	var payload []byte
	err := pool.QueryRow(context.Background(), `SELECT request_payload FROM ar_accounting_posting_requests WHERE tenant_id = $1 AND source_event_id = $2`, e.tenant, sourceEventID).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return kit.PostingEvent{}, false
	}
	require.NoError(e.t, err)
	var ev kit.PostingEvent
	require.NoError(e.t, json.Unmarshal(payload, &ev))
	return ev, true
}

// ── fixtures ─────────────────────────────────────────────────────────────────

func (e *env) item(entity, amount string) string {
	e.t.Helper()
	in := kit.OpenItemFromDocument{DocumentKind: kit.DocInvoice, SourceDocumentID: uuid.NewString(), DocumentNumber: "INV-" + uuid.NewString()[:6], CustomerPartyID: e.party,
		Currency: "USD", OriginalAmount: kit.MustAmount(amount), IssueDate: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), DueDate: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)}
	c := e.caller()
	c.LegalEntityID = entity
	var ref kit.OpenItemRef
	require.NoError(e.t, e.tx(func(tx pgx.Tx) (err error) {
		ref, err = e.m7.CreateFromDocument(context.Background(), tx, c, in)
		return
	}))
	return ref.OpenItemID
}

func (e *env) itemView(id string) *kit.OpenItemView {
	e.t.Helper()
	v, err := e.m7.GetByID(context.Background(), e.tenant, id)
	require.NoError(e.t, err)
	return v
}

func (e *env) receipt(amount string) string {
	e.t.Helper()
	id := e.bank.put(e.tenant, amount)
	code, b, _ := call(e.t, e.h8, "POST", "/internal/v1/ar08/commands/registerConfirmedReceipt", e.tenant, "boss", "", map[string]any{"legal_entity_id": e.entity,
		"bank_transaction_id": id, "customer_party_id": e.party})
	require.Equal(e.t, http.StatusCreated, code, string(b))
	var r struct {
		ReceiptID string `json:"receipt_id"`
	}
	require.NoError(e.t, json.Unmarshal(b, &r))
	return r.ReceiptID
}

func (e *env) cash(receiptID string) *ar08.ReceiptCash {
	e.t.Helper()
	c, err := e.m8.GetReceiptCash(context.Background(), e.tenant, receiptID)
	require.NoError(e.t, err)
	return c
}

func (e *env) holdStatus(refundID string) string {
	e.t.Helper()
	st, found, err := e.m8.UnappliedHoldStatus(context.Background(), e.tenant, "REFUND:"+refundID)
	require.NoError(e.t, err)
	require.True(e.t, found)
	return st
}

func decodeRefund(t *testing.T, b []byte) *Refund {
	t.Helper()
	var r Refund
	require.NoError(t, json.Unmarshal(b, &r), string(b))
	return &r
}

func (e *env) refund(id string) *Refund {
	e.t.Helper()
	var r *Refund
	require.NoError(e.t, e.tx(func(tx pgx.Tx) (err error) { r, err = getRefund(context.Background(), tx, e.tenant, id, false); return }))
	return r
}

func (e *env) newRefund(receiptID, amount string) *Refund {
	e.t.Helper()
	return decodeRefund(e.t, e.must(http.StatusCreated, "createRefundRequest", pRequester, map[string]any{"legal_entity_id": e.entity, "receipt_id": receiptID, "amount": amount,
		"destination_ref": "dest-1", "payer_account_ref": "payer-1", "reason_code": "OVERPAYMENT", "evidence_id": "ev-1"}))
}

func (e *env) ver(id string) int { return e.refund(id).Version }

func (e *env) approveRefund(id string) {
	e.t.Helper()
	e.must(http.StatusOK, "approveRefund", pApprover, map[string]any{"refund_id": id, "expected_version": e.ver(id)})
}

func (e *env) approvedRefund(amount string) *Refund {
	e.t.Helper()
	r := e.newRefund(e.receipt("500.00"), amount)
	e.approveRefund(r.RefundID)
	return e.refund(r.RefundID)
}

func (e *env) submit(id string) (int, []byte) {
	return e.post("submitRefundForPayment", pReleaser, map[string]any{"refund_id": id, "expected_version": e.ver(id)})
}

func (e *env) reconcile(id string) (int, []byte) {
	return e.post("reconcileRefundState", pReleaser, map[string]any{"refund_id": id, "expected_version": e.ver(id)})
}

// settled drives a refund all the way to SETTLED through the fake payment path.
func (e *env) settled(r *Refund) *Refund {
	e.t.Helper()
	code, b := e.submit(r.RefundID)
	require.Equal(e.t, http.StatusOK, code, string(b))
	e.gw.setAllPayments(PaySettled)
	code, b = e.reconcile(r.RefundID)
	require.Equal(e.t, http.StatusOK, code, string(b))
	out := e.refund(r.RefundID)
	require.Equal(e.t, RfSettled, out.State)
	return out
}

// ── write-off fixtures ───────────────────────────────────────────────────────

func decodeWO(t *testing.T, b []byte) *WriteOff {
	t.Helper()
	var w WriteOff
	require.NoError(t, json.Unmarshal(b, &w), string(b))
	return &w
}

func (e *env) writeOff(w string) *WriteOff {
	e.t.Helper()
	var out *WriteOff
	require.NoError(e.t, e.tx(func(tx pgx.Tx) (err error) {
		out, err = getWriteOff(context.Background(), tx, e.tenant, w, false)
		return
	}))
	return out
}

func (e *env) newWriteOff(itemID, amount string, over map[string]any) *WriteOff {
	e.t.Helper()
	b := map[string]any{"legal_entity_id": e.entity, "open_item_id": itemID, "amount": amount, "reason_category": "BAD_DEBT", "reason_code": "UNCOLLECTIBLE", "evidence_id": "ev-wo"}
	for k, v := range over {
		b[k] = v
	}
	return decodeWO(e.t, e.must(http.StatusCreated, "createWriteOffRequest", pRequester, b))
}

func (e *env) woApprove(id, principal string) (int, []byte) {
	return e.post("approveWriteOff", principal, map[string]any{"write_off_id": id, "expected_version": e.writeOff(id).Version})
}

func (e *env) woTax(id, treatment, relief string) (int, []byte) {
	return e.post("resolveWriteOffTaxTreatment", pTaxReview, map[string]any{"write_off_id": id, "expected_version": e.writeOff(id).Version, "tax_treatment": treatment,
		"tax_relief_amount": relief, "evidence_id": "tax-ev-1"})
}

func (e *env) woCommit(id string) (int, []byte) {
	return e.post("commitWriteOff", pCommitter, map[string]any{"write_off_id": id, "expected_version": e.writeOff(id).Version})
}

// approvedWriteOff: item + write-off + tax decided (NONE_REQUIRED) + approved by tier 3.
func (e *env) approvedWriteOff(itemAmount, amount string) (itemID string, w *WriteOff) {
	e.t.Helper()
	itemID = e.item(e.entity, itemAmount)
	w = e.newWriteOff(itemID, amount, nil)
	code, b := e.woTax(w.WriteOffID, TaxNoneRequired, "0")
	require.Equal(e.t, http.StatusOK, code, string(b))
	code, b = e.woApprove(w.WriteOffID, pTier3)
	require.Equal(e.t, http.StatusOK, code, string(b))
	return itemID, e.writeOff(w.WriteOffID)
}

// ═══ refund: happy path, evidence, reads ═════════════════════════════════════

func TestRefund_EndToEnd(t *testing.T) {
	e := newEnv(t)
	rc := e.receipt("500.00")
	require.Equal(t, kit.MustAmount("500"), e.cash(rc).Available)

	r := e.newRefund(rc, "200.00")
	require.Equal(t, RfPendingApproval, r.State)
	require.Equal(t, kit.MustAmount("300"), e.cash(rc).Available, "the refunded cash is reserved: it cannot be applied or refunded twice")
	require.Equal(t, "RESERVED", e.holdStatus(r.RefundID))
	require.Equal(t, 1, e.events(EvRefundRequested))

	e.approveRefund(r.RefundID)
	require.Equal(t, RfApproved, e.refund(r.RefundID).State)

	code, b := e.submit(r.RefundID)
	require.Equal(t, http.StatusOK, code, string(b))
	got := e.refund(r.RefundID)
	require.Equal(t, RfSubmitted, got.State)
	require.NotEmpty(t, got.ExternalAttemptID, "the durable external attempt id is recorded")
	require.NotEmpty(t, got.PaymentStatusID, "payment tracking is registered with payment-status-svc")
	require.Equal(t, 1, e.events(EvRefundSubmitted))
	p, s, rg := e.gw.counts()
	require.Equal(t, []int{1, 1, 1}, []int{p, s, rg})
	require.Equal(t, 0, e.n(`SELECT count(*) FROM ar_accounting_posting_requests WHERE tenant_id = $1 AND source_type = 'REFUND'`, e.tenant), "no accounting before settlement")

	// payment-status-svc says SETTLED
	e.gw.setAllPayments(PaySettled)
	code, b = e.reconcile(r.RefundID)
	require.Equal(t, http.StatusOK, code, string(b))
	got = e.refund(r.RefundID)
	require.Equal(t, RfSettled, got.State)
	require.Equal(t, "CONSUMED", e.holdStatus(r.RefundID))
	require.Equal(t, kit.MustAmount("300"), e.cash(rc).Available)
	require.Equal(t, 1, e.events(EvRefundSettled))
	ev, found := e.posting(r.RefundID)
	require.True(t, found)
	require.Equal(t, "2026-10", ev.FiscalPeriod)
	require.Len(t, ev.Lines, 2)
	require.Equal(t, KeyUnappliedCash, ev.Lines[0].MappingKey)
	require.InDelta(t, 200.0, ev.Lines[0].DebitAmount, 0.0001)
	require.Equal(t, KeyCashReceipt, ev.Lines[1].MappingKey)
	require.InDelta(t, 200.0, ev.Lines[1].CreditAmount, 0.0001)
	require.Equal(t, "PENDING", got.AccountingState)

	// reads
	code, b = e.get("getRefund?refund_id="+r.RefundID, "anyone", e.tenant)
	require.Equal(t, http.StatusOK, code, string(b))
	var view struct {
		Refund     Refund             `json:"refund"`
		Exceptions []ControlException `json:"control_exceptions"`
	}
	require.NoError(t, json.Unmarshal(b, &view))
	require.Equal(t, RfSettled, view.Refund.State)
	require.Empty(t, view.Exceptions, "a settled, consumed, posted-pending refund has no control exception")
	code, b = e.get("getRefundPaymentStatus?refund_id="+r.RefundID+"&live=true", "anyone", e.tenant)
	require.Equal(t, http.StatusOK, code, string(b))
	require.Contains(t, string(b), `"payment_status":"SETTLED"`)
	code, b = e.get("getRefundWriteOffHistory?refund_id="+r.RefundID, "anyone", e.tenant)
	require.Equal(t, http.StatusOK, code, string(b))
	var hist struct {
		History []HistoryEntry `json:"history"`
	}
	require.NoError(t, json.Unmarshal(b, &hist))
	var states []string
	for _, h := range hist.History {
		states = append(states, h.ToState)
	}
	require.Equal(t, []string{RfPendingApproval, RfApproved, RfSubmitted, RfSubmitted, RfSettled}, states[:5], "append-only lifecycle, in order")
	require.Equal(t, 1, e.audits(r.RefundID, "approveRefund"))
	require.Equal(t, 1, e.audits(r.RefundID, "refundSettled"))

	// the ACC-04 outcome is mirrored by the posting listener
	require.NoError(t, e.m.OnPostingOutcome(context.Background(), kit.QueuedPosting{TenantID: e.tenant, SourceType: kit.SourceRefund, SourceID: r.RefundID, SourceEventID: r.RefundID},
		kit.PostingOutcome{Status: kit.PostingPosted, JournalID: "J-1", ExecutionID: "X-1"}))
	got = e.refund(r.RefundID)
	require.Equal(t, "POSTED", got.AccountingState)
	require.Equal(t, "J-1", got.JournalID)
	require.Error(t, e.m.OnPostingOutcome(context.Background(), kit.QueuedPosting{TenantID: e.tenant, SourceType: kit.SourceRefund, SourceID: uuid.NewString()},
		kit.PostingOutcome{Status: kit.PostingPosted}), "an unknown source is re-delivered, not swallowed")
	require.NoError(t, e.m.OnPostingOutcome(context.Background(), kit.QueuedPosting{TenantID: e.tenant, SourceType: kit.SourceInvoice, SourceID: "x"}, kit.PostingOutcome{Status: kit.PostingPosted}))
}

func TestRefund_SoDAndCreateValidation(t *testing.T) {
	e := newEnv(t)
	rc := e.receipt("500.00")
	// the maker holds every refund role: SoD (not roles) stops self-service
	r := decodeRefund(t, e.must(http.StatusCreated, "createRefundRequest", pRefundAll, map[string]any{"legal_entity_id": e.entity, "receipt_id": rc, "amount": "100",
		"destination_ref": "dest-1", "payer_account_ref": "payer-1", "reason_code": "OVERPAYMENT", "evidence_id": "ev-1"}))
	e.refused(http.StatusForbidden, "SEGREGATION_OF_DUTIES", "approveRefund", pRefundAll, map[string]any{"refund_id": r.RefundID, "expected_version": r.Version})
	require.Equal(t, 1, e.audits(r.RefundID, "approveRefund.denied"), "the refusal is audited")
	require.Equal(t, RfPendingApproval, e.refund(r.RefundID).State)
	e.must(http.StatusOK, "approveRefund", pApprover, map[string]any{"refund_id": r.RefundID, "expected_version": r.Version})
	e.refused(http.StatusForbidden, "SEGREGATION_OF_DUTIES", "submitRefundForPayment", pRefundAll, map[string]any{"refund_id": r.RefundID, "expected_version": e.ver(r.RefundID)})
	require.Equal(t, 0, e.gw.prepares, "a refused release never reaches the payment path")
	require.Equal(t, RfApproved, e.refund(r.RefundID).State)
	// approving needs the approve authority; stale versions and missing versions are refused
	e.refused(http.StatusForbidden, "FORBIDDEN", "approveRefund", pRequester, map[string]any{"refund_id": r.RefundID, "expected_version": 1})
	e.refused(http.StatusBadRequest, "EXPECTED_VERSION_REQUIRED", "submitRefundForPayment", pReleaser, map[string]any{"refund_id": r.RefundID})
	e.refused(http.StatusConflict, "STALE_VERSION", "submitRefundForPayment", pReleaser, map[string]any{"refund_id": r.RefundID, "expected_version": 1})

	// create validation against the server-resolved context
	body := func(over map[string]any) map[string]any {
		b := map[string]any{"legal_entity_id": e.entity, "receipt_id": e.receipt("100.00"), "amount": "50", "destination_ref": "dest-1", "payer_account_ref": "payer-1",
			"reason_code": "OVERPAYMENT", "evidence_id": "ev-1"}
		for k, v := range over {
			b[k] = v
		}
		return b
	}
	e.refused(http.StatusUnprocessableEntity, "APPLICATION_OUT_OF_BALANCE", "createRefundRequest", pRequester, body(map[string]any{"amount": "100.01"}))
	e.refused(http.StatusNotFound, "NOT_FOUND", "createRefundRequest", pRequester, body(map[string]any{"receipt_id": uuid.NewString()}))
	e.refused(http.StatusNotFound, "NOT_FOUND", "createRefundRequest", pRequester, body(map[string]any{"destination_ref": "unknown-dest"}))
	e.dest.put("dest-susp", e.party, "USD", "fp-s", "SUSPENDED")
	e.refused(http.StatusUnprocessableEntity, "VALIDATION_FAILED", "createRefundRequest", pRequester, body(map[string]any{"destination_ref": "dest-susp"}))
	e.dest.put("dest-other", uuid.NewString(), "USD", "fp-o", "ACTIVE")
	e.refused(http.StatusUnprocessableEntity, "VALIDATION_FAILED", "createRefundRequest", pRequester, body(map[string]any{"destination_ref": "dest-other"}))
	e.refused(http.StatusForbidden, "TENANT_SCOPE_VIOLATION", "createRefundRequest", pRequester, body(map[string]any{"legal_entity_id": uuid.NewString()}))
	require.Equal(t, 1, e.n(`SELECT count(*) FROM ar10_refunds WHERE tenant_id = $1`, e.tenant), "no refused request left a row or a reservation")
}

// ═══ #30 destination changed after approval ══════════════════════════════════

func TestNeg30_RefundDestinationChangedAfterApproval(t *testing.T) {
	e := newEnv(t)
	r := e.approvedRefund("200.00")
	approvedHash := e.refund(r.RefundID).ApprovedHash
	require.NotEmpty(t, approvedHash)

	// the verified destination's fingerprint changes at the owner after approval
	e.dest.mutate("dest-1", func(d *DestinationInfo) { d.Fingerprint = "fp-CHANGED" })
	e.refused(http.StatusConflict, "REFUND_DESTINATION_CHANGED", "submitRefundForPayment", pReleaser, map[string]any{"refund_id": r.RefundID, "expected_version": e.ver(r.RefundID)})
	got := e.refund(r.RefundID)
	require.Equal(t, RfPendingApproval, got.State, "the approval is invalidated")
	require.Empty(t, got.ApprovedHash)
	require.Empty(t, got.ApprovedBy)
	require.Equal(t, 0, e.gw.prepares+e.gw.submits, "no payment instruction was created")
	require.Equal(t, "RESERVED", e.holdStatus(r.RefundID), "the reservation is intact")
	require.Equal(t, 1, e.events(EvRefundInvalidated))
	require.Equal(t, 1, e.audits(r.RefundID, "submitRefundForPayment.denied"), "evidence of the refusal")
	// it cannot be submitted without a fresh approval
	e.refused(http.StatusConflict, "INVALID_STATE_TRANSITION", "submitRefundForPayment", pReleaser, map[string]any{"refund_id": r.RefundID, "expected_version": e.ver(r.RefundID)})
	// approving against a destination that no longer matches the request is refused too
	e.refused(http.StatusConflict, "REFUND_DESTINATION_CHANGED", "approveRefund", pApprover, map[string]any{"refund_id": r.RefundID, "expected_version": e.ver(r.RefundID)})

	// the legitimate path: change the destination (re-verified), which re-binds the request, then approve again
	e.dest.put("dest-2", e.party, "USD", "fp-2", "ACTIVE")
	e.must(http.StatusOK, "changeRefundDestination", pRequester, map[string]any{"refund_id": r.RefundID, "expected_version": e.ver(r.RefundID), "destination_ref": "dest-2",
		"reason_code": "CUSTOMER_REQUEST", "evidence_id": "ev-2"})
	got = e.refund(r.RefundID)
	require.Equal(t, "dest-2", got.DestinationRef)
	require.NotEqual(t, r.BindingHash, got.BindingHash)
	e.approveRefund(r.RefundID)
	code, b := e.submit(r.RefundID)
	require.Equal(t, http.StatusOK, code, string(b))
	require.Equal(t, 1, e.gw.submits)

	// changing the destination of an APPROVED refund invalidates that approval
	r2 := e.approvedRefund("100.00")
	e.must(http.StatusOK, "changeRefundDestination", pRequester, map[string]any{"refund_id": r2.RefundID, "expected_version": e.ver(r2.RefundID), "destination_ref": "dest-2",
		"reason_code": "CUSTOMER_REQUEST", "evidence_id": "ev-2"})
	require.Equal(t, RfPendingApproval, e.refund(r2.RefundID).State)
	require.Empty(t, e.refund(r2.RefundID).ApprovedBy)
	// once submitted the destination can no longer change (command and schema)
	e.refused(http.StatusConflict, "INVALID_STATE_TRANSITION", "changeRefundDestination", pRequester, map[string]any{"refund_id": r.RefundID, "expected_version": e.ver(r.RefundID),
		"destination_ref": "dest-1", "reason_code": "x", "evidence_id": "y"})
	_, err := pool.Exec(context.Background(), `UPDATE ar10_refunds SET destination_ref = 'evil' WHERE refund_id = $1`, r.RefundID)
	require.Error(t, err, "the schema freezes the destination of a submitted refund")
	require.Equal(t, http.StatusConflict, mapStatus(err))
}

func mapStatus(err error) int {
	var p *kit.Problem
	if errors.As(kit.MapPgErr(err), &p) {
		return p.Status
	}
	return 0
}

func TestRefund_ApprovedContentIsImmutableInSchema(t *testing.T) {
	e := newEnv(t)
	r := e.approvedRefund("100.00")
	_, err := pool.Exec(context.Background(), `UPDATE ar10_refunds SET destination_ref = 'evil' WHERE refund_id = $1`, r.RefundID)
	require.Error(t, err)
	require.Equal(t, http.StatusConflict, mapStatus(err))
	_, err = pool.Exec(context.Background(), `UPDATE ar10_refunds SET amount = amount + 1 WHERE refund_id = $1`, r.RefundID)
	require.Error(t, err)
	_, err = pool.Exec(context.Background(), `DELETE FROM ar10_refunds WHERE refund_id = $1`, r.RefundID)
	require.Error(t, err)
	_, err = pool.Exec(context.Background(), `UPDATE ar10_refunds SET approved_by = requested_by WHERE refund_id = $1`, r.RefundID)
	require.Error(t, err, "SoD is a table constraint too")
	_, err = pool.Exec(context.Background(), `UPDATE ar10_history SET command = 'x' WHERE aggregate_id = $1`, r.RefundID)
	require.Error(t, err, "history is append-only")
	// a final refund cannot be revived
	e.must(http.StatusOK, "cancelUnsubmittedRefund", pRequester, map[string]any{"refund_id": r.RefundID, "expected_version": e.ver(r.RefundID), "reason_code": "CUSTOMER_WITHDREW"})
	_, err = pool.Exec(context.Background(), `UPDATE ar10_refunds SET state = 'APPROVED' WHERE refund_id = $1`, r.RefundID)
	require.Error(t, err)
}

// ═══ #31 provider timeout / duplicate refund ═════════════════════════════════

func TestNeg31_ProviderTimeoutNeverTriggersDuplicateRefund(t *testing.T) {
	e := newEnv(t)
	r := e.approvedRefund("200.00")
	e.gw.mode = "timeout"
	code, b := e.submit(r.RefundID)
	require.Equal(t, http.StatusAccepted, code, string(b))
	require.Equal(t, "REFUND_STATE_UNKNOWN", errCode(t, b))
	got := e.refund(r.RefundID)
	require.Equal(t, RfUnknown, got.State, "a timeout is UNKNOWN, never failed")
	require.NotEmpty(t, got.ExternalAttemptID)
	require.Equal(t, "RESERVED", e.holdStatus(r.RefundID), "the cash stays reserved while the outcome is unknown")
	require.Equal(t, 1, e.events(EvRefundStateUnknown))
	require.Equal(t, 0, e.events(EvRefundFailed))

	// a retry (new Idempotency-Key) is refused: no new payment ID, no second submission
	e.refused(http.StatusConflict, "REFUND_STATE_UNKNOWN", "submitRefundForPayment", pReleaser, map[string]any{"refund_id": r.RefundID, "expected_version": e.ver(r.RefundID)})
	e.refused(http.StatusConflict, "INVALID_STATE_TRANSITION", "cancelUnsubmittedRefund", pRequester, map[string]any{"refund_id": r.RefundID, "expected_version": e.ver(r.RefundID), "reason_code": "x"})
	// reconcile while the provider is still ambiguous: stays UNKNOWN, still no resubmission
	code, b = e.reconcile(r.RefundID)
	require.Equal(t, http.StatusAccepted, code, string(b))
	require.Equal(t, RfUnknown, e.refund(r.RefundID).State)
	// the UNKNOWN refund is a visible control exception
	exc, err := e.m.ControlExceptions(context.Background(), e.tenant, e.entity)
	require.NoError(t, err)
	require.Len(t, exc, 1)
	require.Equal(t, "REFUND_STATE_UNKNOWN", exc[0].Kind)
	p, s, _ := e.gw.counts()
	require.Equal(t, []int{1, 1}, []int{p, s}, "exactly one attempt and one submission so far")

	// the adapter operator resolves the ambiguity: the provider did receive it
	e.gw.resolve(got.ExternalAttemptID, AttemptSubmitted, "prov-1")
	code, b = e.reconcile(r.RefundID)
	require.Equal(t, http.StatusOK, code, string(b))
	require.Equal(t, RfPending, e.refund(r.RefundID).State)
	require.NotEmpty(t, e.refund(r.RefundID).PaymentStatusID)
	e.gw.setAllPayments(PaySettled)
	code, b = e.reconcile(r.RefundID)
	require.Equal(t, http.StatusOK, code, string(b))
	require.Equal(t, RfSettled, e.refund(r.RefundID).State)
	p, s, rg := e.gw.counts()
	require.Equal(t, []int{1, 1, 1}, []int{p, s, rg}, "NO duplicate external effect: one attempt, one submission, one tracking record")
	require.Equal(t, 1, e.n(`SELECT count(*) FROM ar_accounting_posting_requests WHERE tenant_id = $1 AND source_type = 'REFUND'`, e.tenant), "one accounting event")
	// repeated reconcile of a settled refund changes nothing
	code, _ = e.reconcile(r.RefundID)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, 1, e.events(EvRefundSettled))
}

func TestRefund_AmbiguousVariantsAreUnknownNotFailed(t *testing.T) {
	for name, setup := range map[string]func(g *fakeGateway){
		"submit transport error": func(g *fakeGateway) { g.subErr = ErrGatewayUnavailable },
		"adapter swallowed":      func(g *fakeGateway) { g.mode = "timeout" },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			r := e.approvedRefund("50.00")
			setup(e.gw)
			code, b := e.submit(r.RefundID)
			require.Equal(t, http.StatusAccepted, code, string(b))
			require.Equal(t, RfUnknown, e.refund(r.RefundID).State)
			require.Equal(t, "RESERVED", e.holdStatus(r.RefundID))
		})
	}
}

func TestRefund_PrepareFailureIsSafeAndRetryable(t *testing.T) {
	e := newEnv(t)
	r := e.approvedRefund("50.00")
	e.gw.prepErr = ErrGatewayUnavailable
	e.refused(http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "submitRefundForPayment", pReleaser, map[string]any{"refund_id": r.RefundID, "expected_version": e.ver(r.RefundID)})
	require.Equal(t, RfApproved, e.refund(r.RefundID).State, "nothing was sent: back to APPROVED, retryable")
	require.Equal(t, 0, e.gw.submits)
	e.gw.prepErr = &GatewayRejected{Status: 422, Msg: "payer account is not ACTIVE and ownership-verified"}
	e.refused(http.StatusUnprocessableEntity, "VALIDATION_FAILED", "submitRefundForPayment", pReleaser, map[string]any{"refund_id": r.RefundID, "expected_version": e.ver(r.RefundID)})
	require.Equal(t, RfApproved, e.refund(r.RefundID).State)
	e.gw.prepErr = nil
	code, b := e.submit(r.RefundID)
	require.Equal(t, http.StatusOK, code, string(b))
	require.Equal(t, RfSubmitted, e.refund(r.RefundID).State)
	require.Equal(t, 1, e.gw.submits)
}

func TestRefund_NotWiredFailsClosedWithoutStateChange(t *testing.T) {
	e := newEnv(t)
	r := e.approvedRefund("50.00")
	gw := e.m.gateway
	e.m.gateway = nil
	e.refused(http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "submitRefundForPayment", pReleaser, map[string]any{"refund_id": r.RefundID, "expected_version": e.ver(r.RefundID)})
	require.Equal(t, RfApproved, e.refund(r.RefundID).State)
	e.m.gateway = gw
	e.m.dest = nil
	e.refused(http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "approveRefund", pApprover, map[string]any{"refund_id": r.RefundID, "expected_version": e.ver(r.RefundID)})
	e.m.dest = e.dest
}

// ═══ reservation release, cancel, replay, concurrency ════════════════════════

func TestRefund_FailureAndCancelReleaseTheReservation(t *testing.T) {
	e := newEnv(t)
	rc := e.receipt("500.00")
	r := e.newRefund(rc, "300.00")
	require.Equal(t, kit.MustAmount("200"), e.cash(rc).Available)
	e.approveRefund(r.RefundID)
	e.gw.mode = "reject"
	code, b := e.submit(r.RefundID)
	require.Equal(t, http.StatusOK, code, string(b))
	got := e.refund(r.RefundID)
	require.Equal(t, RfFailed, got.State)
	require.Contains(t, got.FailureReason, "REJECTED_BEFORE_SUBMISSION")
	require.Equal(t, "RELEASED", e.holdStatus(r.RefundID))
	require.Equal(t, kit.MustAmount("500"), e.cash(rc).Available, "the cash is available again")
	require.Equal(t, 1, e.events(EvRefundFailed))
	require.Equal(t, 0, e.n(`SELECT count(*) FROM ar_accounting_posting_requests WHERE tenant_id = $1 AND source_type = 'REFUND'`, e.tenant))
	// a failed refund is final; a settled-looking payment cannot revive it
	e.refused(http.StatusConflict, "INVALID_STATE_TRANSITION", "reconcileRefundState", pReleaser, map[string]any{"refund_id": r.RefundID, "expected_version": e.ver(r.RefundID)})

	// payment-status-svc REJECTED after submission also releases
	r2 := e.newRefund(rc, "100.00")
	e.approveRefund(r2.RefundID)
	e.gw.mode = "ok"
	code, b = e.submit(r2.RefundID)
	require.Equal(t, http.StatusOK, code, string(b))
	e.gw.setAllPayments(PayRejected)
	code, b = e.reconcile(r2.RefundID)
	require.Equal(t, http.StatusOK, code, string(b))
	require.Equal(t, RfFailed, e.refund(r2.RefundID).State)
	require.Equal(t, "RELEASED", e.holdStatus(r2.RefundID))
	require.Equal(t, kit.MustAmount("500"), e.cash(rc).Available)

	// cancel before submit releases; cancel after submit is refused
	r3 := e.newRefund(rc, "100.00")
	e.must(http.StatusOK, "cancelUnsubmittedRefund", pRequester, map[string]any{"refund_id": r3.RefundID, "expected_version": r3.Version, "reason_code": "CUSTOMER_WITHDREW"})
	require.Equal(t, RfCancelled, e.refund(r3.RefundID).State)
	require.Equal(t, "RELEASED", e.holdStatus(r3.RefundID))
	e.refused(http.StatusBadRequest, "REASON_REQUIRED", "cancelUnsubmittedRefund", pRequester, map[string]any{"refund_id": r3.RefundID, "expected_version": 1})
	r4 := e.approvedRefund("10.00")
	e.submit(r4.RefundID)
	e.refused(http.StatusConflict, "INVALID_STATE_TRANSITION", "cancelUnsubmittedRefund", pRequester, map[string]any{"refund_id": r4.RefundID, "expected_version": e.ver(r4.RefundID), "reason_code": "x"})
}

func TestRefund_IdempotentReplayAndRepeatSubmit(t *testing.T) {
	e := newEnv(t)
	r := e.approvedRefund("100.00")
	body := map[string]any{"refund_id": r.RefundID, "expected_version": e.ver(r.RefundID)}
	key := uuid.NewString()
	code, b1, _ := call(t, e.h, "POST", base+"commands/submitRefundForPayment", e.tenant, pReleaser, key, body)
	require.Equal(t, http.StatusOK, code, string(b1))
	code, b2, hdr := call(t, e.h, "POST", base+"commands/submitRefundForPayment", e.tenant, pReleaser, key, body)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "true", hdr.Get("Idempotent-Replay"))
	require.JSONEq(t, string(b1), string(b2))
	// the same key for a different request is refused
	code, b, _ := call(t, e.h, "POST", base+"commands/submitRefundForPayment", e.tenant, pReleaser, key, map[string]any{"refund_id": r.RefundID, "expected_version": 99})
	require.Equal(t, http.StatusUnprocessableEntity, code, string(b))
	// a repeat with a NEW key is a no-op on an already released refund
	code, b = e.submit(r.RefundID)
	require.Equal(t, http.StatusOK, code, string(b))
	p, s, _ := e.gw.counts()
	require.Equal(t, []int{1, 1}, []int{p, s})
	// create replays too
	rc := e.receipt("100.00")
	cb := map[string]any{"legal_entity_id": e.entity, "receipt_id": rc, "amount": "40", "destination_ref": "dest-1", "payer_account_ref": "payer-1", "reason_code": "X", "evidence_id": "E"}
	k2 := uuid.NewString()
	code, _, _ = call(t, e.h, "POST", base+"commands/createRefundRequest", e.tenant, pRequester, k2, cb)
	require.Equal(t, http.StatusCreated, code)
	code, _, hdr = call(t, e.h, "POST", base+"commands/createRefundRequest", e.tenant, pRequester, k2, cb)
	require.Equal(t, http.StatusCreated, code)
	require.Equal(t, "true", hdr.Get("Idempotent-Replay"))
	require.Equal(t, 1, e.n(`SELECT count(*) FROM ar10_refunds WHERE tenant_id = $1 AND receipt_id = $2`, e.tenant, rc))
	require.Equal(t, kit.MustAmount("60"), e.cash(rc).Available)
}

func TestRefund_ConcurrentSubmitsHaveExactlyOneExternalEffect(t *testing.T) {
	e := newEnv(t)
	r := e.approvedRefund("100.00")
	v := e.ver(r.RefundID)
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = e.post("submitRefundForPayment", pReleaser, map[string]any{"refund_id": r.RefundID, "expected_version": v})
		}(i)
	}
	wg.Wait()
	for _, c := range codes {
		require.True(t, c == http.StatusOK || c == http.StatusAccepted || c == http.StatusConflict, "%v", codes)
	}
	p, s, _ := e.gw.counts()
	require.Equal(t, 1, p, "one durable attempt")
	require.Equal(t, 1, s, "one submission to the provider")
	require.Equal(t, 1, e.n(`SELECT count(*) FROM ar10_refunds WHERE tenant_id = $1 AND external_attempt_id IS NOT NULL`, e.tenant))
	require.Equal(t, 1, e.events(EvRefundSubmitted))
}

func TestRefund_ConcurrentRequestsCannotOverReserveTheCash(t *testing.T) {
	e := newEnv(t)
	rc := e.receipt("100.00")
	var wg sync.WaitGroup
	codes := make([]int, 5)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = e.post("createRefundRequest", pRequester, map[string]any{"legal_entity_id": e.entity, "receipt_id": rc, "amount": "60", "destination_ref": "dest-1",
				"payer_account_ref": "payer-1", "reason_code": "X", "evidence_id": "E"})
		}(i)
	}
	wg.Wait()
	created := 0
	for _, c := range codes {
		if c == http.StatusCreated {
			created++
		} else {
			require.Equal(t, http.StatusUnprocessableEntity, c)
		}
	}
	require.Equal(t, 1, created, "only one of five concurrent refunds of 60 fits in 100")
	require.Equal(t, kit.MustAmount("40"), e.cash(rc).Available)
}

func TestRefund_AtomicRollbackOfSettlement(t *testing.T) {
	e := newEnv(t)
	r := e.approvedRefund("100.00")
	code, b := e.submit(r.RefundID)
	require.Equal(t, http.StatusOK, code, string(b))
	e.gw.setAllPayments(PaySettled)
	e.m.receipts = flakyReceipts{ReceiptCashPort: e.m8, failConsume: true}
	code, b = e.reconcile(r.RefundID)
	require.Equal(t, http.StatusServiceUnavailable, code, string(b))
	got := e.refund(r.RefundID)
	require.Equal(t, RfSubmitted, got.State, "the failed settlement left the refund as it was")
	require.Equal(t, "RESERVED", e.holdStatus(r.RefundID), "the consume rolled back with it")
	require.Equal(t, 0, e.n(`SELECT count(*) FROM ar_accounting_posting_requests WHERE tenant_id = $1 AND source_type = 'REFUND'`, e.tenant))
	require.Equal(t, 0, e.events(EvRefundSettled))
	e.m.receipts = e.m8
	code, b = e.reconcile(r.RefundID)
	require.Equal(t, http.StatusOK, code, string(b))
	require.Equal(t, RfSettled, e.refund(r.RefundID).State)
}

func TestRefund_ReturnedAfterSettlementIsAControlException(t *testing.T) {
	e := newEnv(t)
	r := e.settled(e.approvedRefund("100.00"))
	e.gw.setAllPayments(PayReturned)
	code, b := e.reconcile(r.RefundID)
	require.Equal(t, http.StatusOK, code, string(b))
	got := e.refund(r.RefundID)
	require.Equal(t, RfSettled, got.State)
	require.Equal(t, "REFUND_RETURNED_AFTER_SETTLEMENT", got.ControlException)
	exc, err := e.m.ControlExceptions(context.Background(), e.tenant, e.entity)
	require.NoError(t, err)
	require.Len(t, exc, 1)
}

// ═══ ar08 interplay ══════════════════════════════════════════════════════════

func TestHold_SchemaRefusesApplyingReservedCash(t *testing.T) {
	e := newEnv(t)
	rc := e.receipt("500.00")
	e.newRefund(rc, "300.00")
	_, err := pool.Exec(context.Background(), `UPDATE ar08_receipts SET applied_amount = 500, unapplied_amount = 0, state = 'FULLY_APPLIED' WHERE receipt_id = $1`, rc)
	require.Error(t, err)
	require.Contains(t, err.Error(), "AR08_RECEIPT_CASH_RESERVED")
	_, err = pool.Exec(context.Background(), `UPDATE ar08_receipts SET applied_amount = 200, unapplied_amount = 300, state = 'PARTIALLY_APPLIED' WHERE receipt_id = $1`, rc)
	require.NoError(t, err, "unreserved cash can still be applied")
	// a reservation cannot exceed what is unapplied, and releasing / consuming is one-way
	var rf *Refund
	require.NoError(t, e.tx(func(tx pgx.Tx) error {
		_, err := e.m8.ReserveUnapplied(context.Background(), tx, e.caller(), rc, "REFUND:"+uuid.NewString(), kit.MustAmount("1"))
		require.Error(t, err)
		require.Equal(t, "APPLICATION_OUT_OF_BALANCE", err.(*kit.Problem).Code)
		return nil
	}))
	_ = rf
	_, err = pool.Exec(context.Background(), `UPDATE ar08_receipt_holds SET status = 'CONSUMED' WHERE receipt_id = $1`, rc)
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(), `UPDATE ar08_receipt_holds SET status = 'RELEASED' WHERE receipt_id = $1`, rc)
	require.Error(t, err, "a consumed hold is final")
}

// ═══ #32 refund settles but AR remains open without exception ════════════════

func TestNeg32_SettledRefundWithoutARConsequenceIsAVisibleException(t *testing.T) {
	e := newEnv(t)
	r := e.settled(e.approvedRefund("100.00"))
	exc, err := e.m.ControlExceptions(context.Background(), e.tenant, e.entity)
	require.NoError(t, err)
	require.Empty(t, exc, "a consistent settled refund has no exception")

	// (a) the reservation was not consumed: AR still carries the cash as unapplied
	e.m.receipts = flakyReceipts{ReceiptCashPort: e.m8, hold: "RESERVED"}
	code, b := e.get("getRefund?refund_id="+r.RefundID, "anyone", e.tenant)
	require.Equal(t, http.StatusOK, code, string(b))
	require.Contains(t, string(b), "AR_CONSEQUENCE_MISSING")
	exc, err = e.m.ControlExceptions(context.Background(), e.tenant, e.entity)
	require.NoError(t, err)
	require.Len(t, exc, 1)
	require.Equal(t, "AR_CONSEQUENCE_MISSING", exc[0].Kind)
	e.m.receipts = e.m8

	// (b) the accounting event is missing
	_, err = pool.Exec(context.Background(), `DELETE FROM ar_accounting_posting_requests WHERE tenant_id = $1 AND source_event_id = $2`, e.tenant, r.RefundID)
	require.NoError(t, err)
	code, b = e.get("getControlExceptions?legal_entity_id="+e.entity, "anyone", e.tenant)
	require.Equal(t, http.StatusOK, code, string(b))
	require.Contains(t, string(b), "NO_POSTING")
	require.Equal(t, 0, e.n(`SELECT count(*) FROM ar_accounting_posting_requests WHERE tenant_id = $1`, e.tenant))
}

// ═══ #35 accounting event missing / failed ═══════════════════════════════════

func TestNeg35_AccountingEventMissingOrFailedIsVisible(t *testing.T) {
	e := newEnv(t)
	// refund: the posting listener mirrors FAILED / QUARANTINED, and the exception stays visible until resolved
	r := e.settled(e.approvedRefund("100.00"))
	require.NoError(t, e.m.OnPostingOutcome(context.Background(), kit.QueuedPosting{TenantID: e.tenant, SourceType: kit.SourceRefund, SourceID: r.RefundID, SourceEventID: r.RefundID},
		kit.PostingOutcome{Status: kit.PostingQuarantined, Error: "no mapping for AR_CASH_RECEIPT"}))
	got := e.refund(r.RefundID)
	require.Equal(t, "QUARANTINED", got.AccountingState)
	require.Contains(t, got.AccountingError, "AR_CASH_RECEIPT")
	exc, err := e.m.ControlExceptions(context.Background(), e.tenant, e.entity)
	require.NoError(t, err)
	require.Len(t, exc, 1)
	require.Equal(t, "POSTING_FAILED", exc[0].Kind)
	// a PENDING outcome (retry later) is not recorded as terminal
	require.NoError(t, e.m.OnPostingOutcome(context.Background(), kit.QueuedPosting{TenantID: e.tenant, SourceType: kit.SourceRefund, SourceID: r.RefundID}, kit.PostingOutcome{Status: kit.PostingPending}))
	require.Equal(t, "QUARANTINED", e.refund(r.RefundID).AccountingState)

	// write-off: committed => exactly one posting; failure is mirrored and visible
	_, w := e.approvedWriteOff("1000.00", "400.00")
	code, b := e.woCommit(w.WriteOffID)
	require.Equal(t, http.StatusOK, code, string(b))
	_, found := e.posting(w.WriteOffID)
	require.True(t, found)
	require.NoError(t, e.m.OnPostingOutcome(context.Background(), kit.QueuedPosting{TenantID: e.tenant, SourceType: kit.SourceWriteOff, SourceID: w.WriteOffID, SourceEventID: w.WriteOffID},
		kit.PostingOutcome{Status: kit.PostingFailed, Error: "period locked"}))
	require.Equal(t, "FAILED", e.writeOff(w.WriteOffID).AccountingState)
	code, b = e.get("getWriteOff?write_off_id="+w.WriteOffID, "anyone", e.tenant)
	require.Equal(t, http.StatusOK, code, string(b))
	require.Contains(t, string(b), "POSTING_FAILED")
	_, err = pool.Exec(context.Background(), `DELETE FROM ar_accounting_posting_requests WHERE tenant_id = $1 AND source_event_id = $2`, e.tenant, w.WriteOffID)
	require.NoError(t, err)
	code, b = e.get("getWriteOff?write_off_id="+w.WriteOffID, "anyone", e.tenant)
	require.Equal(t, http.StatusOK, code, string(b))
	require.Contains(t, string(b), "NO_POSTING", "a committed write-off with no accounting event is a visible control exception")
}

// ═══ write-off ═══════════════════════════════════════════════════════════════

func TestWriteOff_EndToEnd(t *testing.T) {
	e := newEnv(t)
	itemID := e.item(e.entity, "1000.00")
	w := e.newWriteOff(itemID, "1000.00", nil)
	require.Equal(t, WoPendingApproval, w.State)
	require.Equal(t, 2, w.RequiredTier)
	require.Equal(t, TaxReviewNeeded, w.TaxTreatment, "the tax treatment starts unresolved")
	require.Equal(t, 1, e.events(EvWriteOffRequested))

	code, b := e.woTax(w.WriteOffID, TaxReliefClaimed, "166.67")
	require.Equal(t, http.StatusOK, code, string(b))
	code, b = e.woApprove(w.WriteOffID, pTier3)
	require.Equal(t, http.StatusOK, code, string(b))
	got := e.writeOff(w.WriteOffID)
	require.Equal(t, WoApproved, got.State)
	require.Equal(t, pTier3, got.ApprovedBy)
	require.Equal(t, 3, got.ApprovedTier)
	require.Equal(t, 1, e.events(EvWriteOffApproved))

	code, b = e.woCommit(w.WriteOffID)
	require.Equal(t, http.StatusOK, code, string(b))
	got = e.writeOff(w.WriteOffID)
	require.Equal(t, WoCommitted, got.State)
	require.Equal(t, "2026-10", got.FiscalPeriod)
	require.Equal(t, "PENDING", got.AccountingState)
	v := e.itemView(itemID)
	require.Equal(t, kit.Amount(0), v.Residual)
	require.Equal(t, "WRITTEN_OFF", v.State, "AR-07 drove the residual and state")
	require.Equal(t, 1, e.n(`SELECT count(*) FROM ar07_adjustments WHERE tenant_id = $1 AND source_ref_type = 'WRITE_OFF' AND source_ref_id = $2`, e.tenant, w.WriteOffID))
	require.Equal(t, 1, e.events(EvReceivableWritten))
	ev, found := e.posting(w.WriteOffID)
	require.True(t, found)
	require.Len(t, ev.Lines, 3)
	require.Equal(t, KeyBadDebt, ev.Lines[0].MappingKey)
	require.InDelta(t, 833.33, ev.Lines[0].DebitAmount, 0.0001)
	require.Equal(t, KeyTaxOutput, ev.Lines[1].MappingKey)
	require.InDelta(t, 166.67, ev.Lines[1].DebitAmount, 0.0001)
	require.Equal(t, KeyReceivable, ev.Lines[2].MappingKey)
	require.InDelta(t, 1000.0, ev.Lines[2].CreditAmount, 0.0001)

	// a repeat commit is a no-op; a committed write-off is frozen in the schema
	code, _ = e.woCommit(w.WriteOffID)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, 1, e.events(EvReceivableWritten))
	_, err := pool.Exec(context.Background(), `UPDATE ar10_writeoffs SET tax_treatment = 'NONE_REQUIRED', tax_relief_amount = 0 WHERE writeoff_id = $1`, w.WriteOffID)
	require.Error(t, err)
	require.Equal(t, http.StatusConflict, mapStatus(err))
	_, err = pool.Exec(context.Background(), `UPDATE ar10_writeoffs SET amount = 1 WHERE writeoff_id = $1`, w.WriteOffID)
	require.Error(t, err)

	// reads
	code, b = e.get("getRefundWriteOffHistory?write_off_id="+w.WriteOffID, "anyone", e.tenant)
	require.Equal(t, http.StatusOK, code, string(b))
	require.Contains(t, string(b), "commitWriteOff")
	code, b = e.get("getAvailableActions?write_off_id="+w.WriteOffID, pCommitter, e.tenant)
	require.Equal(t, http.StatusOK, code, string(b))
	require.JSONEq(t, `{"actions":[]}`, string(b), "nothing is available on a committed write-off")
}

func TestWriteOff_RequestValidation(t *testing.T) {
	e := newEnv(t)
	itemID := e.item(e.entity, "500.00")
	body := func(over map[string]any) map[string]any {
		b := map[string]any{"legal_entity_id": e.entity, "open_item_id": itemID, "amount": "100", "reason_category": "BAD_DEBT", "reason_code": "X", "evidence_id": "E"}
		for k, v := range over {
			b[k] = v
		}
		return b
	}
	e.refused(http.StatusUnprocessableEntity, "RECEIVABLE_RESIDUAL_INVALID", "createWriteOffRequest", pRequester, body(map[string]any{"amount": "500.01"}))
	e.refused(http.StatusNotFound, "NOT_FOUND", "createWriteOffRequest", pRequester, body(map[string]any{"open_item_id": uuid.NewString()}))
	e.refused(http.StatusForbidden, "TENANT_SCOPE_VIOLATION", "createWriteOffRequest", pRequester, body(map[string]any{"legal_entity_id": uuid.NewString()}))
	other := e.item(uuid.NewString(), "500.00")
	e.refused(http.StatusForbidden, "TENANT_SCOPE_VIOLATION", "createWriteOffRequest", pRequester, body(map[string]any{"open_item_id": other}))
	require.Equal(t, 0, e.n(`SELECT count(*) FROM ar10_writeoffs WHERE tenant_id = $1`, e.tenant))

	w := e.newWriteOff(itemID, "100", nil)
	e.refused(http.StatusConflict, "INVALID_STATE_TRANSITION", "createWriteOffRequest", pRequester, body(nil)) // one in-flight write-off per item
	// a disputed item cannot be written off
	disputed := e.item(e.entity, "500.00")
	code, b, _ := call(t, e.h7, "POST", "/internal/v1/ar07/commands/openDispute", e.tenant, pDisputer, "", map[string]any{"legal_entity_id": e.entity, "open_item_id": disputed,
		"expected_version": e.itemView(disputed).Version, "reason_code": "PRICE", "evidence_id": "ev"})
	require.Equal(t, 2, code/100, string(b))
	e.refused(http.StatusConflict, "INVALID_STATE_TRANSITION", "createWriteOffRequest", pRequester, body(map[string]any{"open_item_id": disputed}))

	// committing needs the approval first, and refuses a stale version
	e.refused(http.StatusConflict, "INVALID_STATE_TRANSITION", "commitWriteOff", pCommitter, map[string]any{"write_off_id": w.WriteOffID, "expected_version": w.Version})
	e.refused(http.StatusBadRequest, "EXPECTED_VERSION_REQUIRED", "approveWriteOff", pTier3, map[string]any{"write_off_id": w.WriteOffID})
	e.refused(http.StatusConflict, "STALE_VERSION", "approveWriteOff", pTier3, map[string]any{"write_off_id": w.WriteOffID, "expected_version": 99})
	// the tax reviewer is never the requester
	e.refused(http.StatusForbidden, "SEGREGATION_OF_DUTIES", "resolveWriteOffTaxTreatment", pWOAll, map[string]any{"write_off_id": e.newWriteOffBy(pWOAll, e.item(e.entity, "300.00")),
		"expected_version": 1, "tax_treatment": TaxNoneRequired, "evidence_id": "t"})
}

func (e *env) newWriteOffBy(principal, itemID string) string {
	e.t.Helper()
	b := e.must(http.StatusCreated, "createWriteOffRequest", principal, map[string]any{"legal_entity_id": e.entity, "open_item_id": itemID, "amount": "100", "reason_category": "BAD_DEBT",
		"reason_code": "X", "evidence_id": "E"})
	return decodeWO(e.t, b).WriteOffID
}

// ═══ #29 collector / requester self-approval ═════════════════════════════════

func TestNeg29_CollectorSelfApprovesWriteOff(t *testing.T) {
	e := newEnv(t)
	// the requester holds approval authority themselves: SoD still refuses the self-approval (audited)
	itemID := e.item(e.entity, "3000.00")
	w := decodeWO(t, e.must(http.StatusCreated, "createWriteOffRequest", pWOAll, map[string]any{"legal_entity_id": e.entity, "open_item_id": itemID, "amount": "2000",
		"reason_category": "BAD_DEBT", "reason_code": "X", "evidence_id": "E"}))
	code, b := e.woApprove(w.WriteOffID, pWOAll)
	require.Equal(t, http.StatusForbidden, code, string(b))
	require.Equal(t, "SEGREGATION_OF_DUTIES", errCode(t, b))
	require.Equal(t, 1, e.audits(w.WriteOffID, "approveWriteOff.denied"))
	require.Equal(t, WoPendingApproval, e.writeOff(w.WriteOffID).State)
	require.Equal(t, 0, e.events(EvWriteOffApproved))

	// the collector who placed the collection hold on the item cannot approve a MATERIAL write-off of it
	item2 := e.item(e.entity, "3000.00")
	code, b, _ = call(t, e.h7, "POST", "/internal/v1/ar07/commands/placeCollectionHold", e.tenant, pCollector, "", map[string]any{"legal_entity_id": e.entity, "open_item_id": item2,
		"expected_version": e.itemView(item2).Version, "reason_code": "COLLECTING", "evidence_id": "e"})
	require.Equal(t, 2, code/100, string(b))
	w2 := e.newWriteOff(item2, "2000", nil)
	code, b = e.woApprove(w2.WriteOffID, pCollector)
	require.Equal(t, http.StatusForbidden, code, string(b))
	require.Equal(t, "SEGREGATION_OF_DUTIES", errCode(t, b))
	require.Contains(t, string(b), "collection hold owner")
	require.Equal(t, 1, e.audits(w2.WriteOffID, "approveWriteOff.denied"))
	// ... while someone else with the authority can, and an immaterial write-off has no such restriction
	code, b = e.woApprove(w2.WriteOffID, pTier3)
	require.Equal(t, http.StatusOK, code, string(b))
	item3 := e.item(e.entity, "300.00")
	code, b, _ = call(t, e.h7, "POST", "/internal/v1/ar07/commands/placeCollectionHold", e.tenant, pCollector, "", map[string]any{"legal_entity_id": e.entity, "open_item_id": item3,
		"expected_version": e.itemView(item3).Version, "reason_code": "COLLECTING", "evidence_id": "e"})
	require.Equal(t, 2, code/100, string(b))
	w3 := e.newWriteOff(item3, "200", nil)
	code, b = e.woApprove(w3.WriteOffID, pCollector)
	require.Equal(t, http.StatusOK, code, string(b))

	// the principal who resolved a dispute on the item cannot approve a material write-off
	item4 := e.item(e.entity, "3000.00")
	code, b, _ = call(t, e.h7, "POST", "/internal/v1/ar07/commands/openDispute", e.tenant, pDisputer, "", map[string]any{"legal_entity_id": e.entity, "open_item_id": item4,
		"expected_version": e.itemView(item4).Version, "reason_code": "PRICE", "evidence_id": "ev"})
	require.Equal(t, 2, code/100, string(b))
	code, b, _ = call(t, e.h7, "POST", "/internal/v1/ar07/commands/resolveDispute", e.tenant, pResolver, "", map[string]any{"legal_entity_id": e.entity, "open_item_id": item4,
		"expected_version": e.itemView(item4).Version, "resolution_code": "REJECTED", "reason_code": "PRICE_OK", "evidence_id": "ev2"})
	require.Equal(t, 2, code/100, string(b))
	w4 := e.newWriteOff(item4, "2000", nil)
	code, b = e.woApprove(w4.WriteOffID, pResolver)
	require.Equal(t, http.StatusForbidden, code, string(b))
	require.Equal(t, "SEGREGATION_OF_DUTIES", errCode(t, b))
	require.Contains(t, string(b), "dispute resolver")
	require.Equal(t, WoPendingApproval, e.writeOff(w4.WriteOffID).State)
}

// ═══ #33 write-off above delegated limit ═════════════════════════════════════

func TestNeg33_WriteOffAboveDelegatedLimit(t *testing.T) {
	e := newEnv(t)
	itemID := e.item(e.entity, "9000.00")
	w := e.newWriteOff(itemID, "6000", nil)
	require.Equal(t, 3, w.RequiredTier, "6000 is above the tier-2 limit of 5000")
	for _, p := range []string{pTier1, pTier2} {
		code, b := e.woApprove(w.WriteOffID, p)
		require.Equal(t, http.StatusForbidden, code, string(b))
		require.Equal(t, "WRITEOFF_APPROVAL_REQUIRED", errCode(t, b), p)
	}
	require.Equal(t, 2, e.audits(w.WriteOffID, "approveWriteOff.denied"), "each refusal is audited")
	// no approval authority at all is plain FORBIDDEN, still audited
	code, b := e.woApprove(w.WriteOffID, pNobody)
	require.Equal(t, http.StatusForbidden, code, string(b))
	require.Equal(t, "FORBIDDEN", errCode(t, b))
	require.Equal(t, WoPendingApproval, e.writeOff(w.WriteOffID).State)
	require.Equal(t, kit.MustAmount("9000"), e.itemView(itemID).Residual)
	code, b = e.woApprove(w.WriteOffID, pTier3)
	require.Equal(t, http.StatusOK, code, string(b))

	// the highest tier covers everything; tier 1 covers its own limit
	big := e.newWriteOff(e.item(e.entity, "90000.00"), "60000", nil)
	require.Equal(t, 4, big.RequiredTier)
	code, _ = e.woApprove(big.WriteOffID, pTier3)
	require.Equal(t, http.StatusForbidden, code)
	code, b = e.woApprove(big.WriteOffID, pTier4)
	require.Equal(t, http.StatusOK, code, string(b))
	small := e.newWriteOff(e.item(e.entity, "500.00"), "500", nil)
	code, b = e.woApprove(small.WriteOffID, pTier1)
	require.Equal(t, http.StatusOK, code, string(b))

	// GetAvailableActions is derived from the caller's authority and SoD, never hard-coded
	w2 := e.newWriteOff(e.item(e.entity, "9000.00"), "6000", nil)
	for principal, want := range map[string]bool{pTier1: false, pTier3: true, pRequester: false} {
		code, b := e.get("getAvailableActions?write_off_id="+w2.WriteOffID, principal, e.tenant)
		require.Equal(t, http.StatusOK, code, string(b))
		var v struct {
			Actions []actionView `json:"actions"`
		}
		require.NoError(t, json.Unmarshal(b, &v))
		var found bool
		for _, a := range v.Actions {
			if a.Command == "approveWriteOff" {
				found = true
				require.Equal(t, want, a.Allowed, principal+": "+a.Blocked)
			}
		}
		require.True(t, found)
	}
}

// ═══ #34 write-off tax implication ignored ═══════════════════════════════════

func TestNeg34_WriteOffTaxImplicationCannotBeIgnored(t *testing.T) {
	e := newEnv(t)
	itemID := e.item(e.entity, "1000.00")
	w := e.newWriteOff(itemID, "1000", nil)
	require.Equal(t, TaxReviewNeeded, w.TaxTreatment)
	code, b := e.woApprove(w.WriteOffID, pTier3)
	require.Equal(t, http.StatusOK, code, string(b))
	code, b = e.woCommit(w.WriteOffID)
	require.Equal(t, http.StatusUnprocessableEntity, code, string(b))
	require.Equal(t, "TAX_DETERMINATION_MISSING", errCode(t, b), "commit is blocked while the tax treatment is unresolved")
	require.Equal(t, WoApproved, e.writeOff(w.WriteOffID).State)
	require.Equal(t, kit.MustAmount("1000"), e.itemView(itemID).Residual, "no residual change")
	require.Equal(t, 0, e.n(`SELECT count(*) FROM ar_accounting_posting_requests WHERE tenant_id = $1 AND source_type = 'WRITE_OFF'`, e.tenant), "no accounting")
	require.Equal(t, 1, e.audits(w.WriteOffID, "commitWriteOff.denied"))
	// the schema agrees: a write-off cannot be COMMITTED with an unresolved treatment
	_, err := pool.Exec(context.Background(), `UPDATE ar10_writeoffs SET state = 'COMMITTED' WHERE writeoff_id = $1`, w.WriteOffID)
	require.Error(t, err)

	// the decision needs evidence and the tax-review authority; deciding invalidates the approval (it is bound to the treatment)
	e.refused(http.StatusBadRequest, "EVIDENCE_REQUIRED", "resolveWriteOffTaxTreatment", pTaxReview, map[string]any{"write_off_id": w.WriteOffID, "expected_version": e.writeOff(w.WriteOffID).Version,
		"tax_treatment": TaxNoneRequired})
	e.refused(http.StatusForbidden, "FORBIDDEN", "resolveWriteOffTaxTreatment", pTier3, map[string]any{"write_off_id": w.WriteOffID, "expected_version": e.writeOff(w.WriteOffID).Version,
		"tax_treatment": TaxNoneRequired, "evidence_id": "t"})
	e.refused(http.StatusBadRequest, "VALIDATION_FAILED", "resolveWriteOffTaxTreatment", pTaxReview, map[string]any{"write_off_id": w.WriteOffID, "expected_version": e.writeOff(w.WriteOffID).Version,
		"tax_treatment": TaxReliefClaimed, "tax_relief_amount": "1001", "evidence_id": "t"})
	code, b = e.woTax(w.WriteOffID, TaxReliefClaimed, "166.67")
	require.Equal(t, http.StatusOK, code, string(b))
	got := e.writeOff(w.WriteOffID)
	require.Equal(t, WoPendingApproval, got.State, "the approval was invalidated")
	require.Empty(t, got.ApprovedHash)
	require.Equal(t, pTaxReview, got.TaxReviewer)
	require.Equal(t, "tax-ev-1", got.TaxEvidenceID)
	require.Equal(t, 1, e.events(EvWriteOffInvalid))
	code, b = e.woApprove(w.WriteOffID, pTier3)
	require.Equal(t, http.StatusOK, code, string(b))
	code, b = e.woCommit(w.WriteOffID)
	require.Equal(t, http.StatusOK, code, string(b))
	ev, found := e.posting(w.WriteOffID)
	require.True(t, found)
	require.Len(t, ev.Lines, 3, "the relief is posted to output tax")

	// changing the treatment of an approved write-off through the schema is refused outright
	w2 := e.newWriteOff(e.item(e.entity, "500.00"), "100", map[string]any{})
	e.woTax(w2.WriteOffID, TaxNoneRequired, "0")
	e.woApprove(w2.WriteOffID, pTier1)
	_, err = pool.Exec(context.Background(), `UPDATE ar10_writeoffs SET tax_treatment = 'RELIEF_CLAIMED', tax_relief_amount = 10 WHERE writeoff_id = $1`, w2.WriteOffID)
	require.Error(t, err)
}

// ═══ commit: atomicity, gates, concurrency ═══════════════════════════════════

func TestWriteOff_CommitAtomicRollbackAndGates(t *testing.T) {
	e := newEnv(t)
	itemID, w := e.approvedWriteOff("1000.00", "400.00")

	// the adjustment succeeds and then the command fails: everything rolls back together
	items := e.m.items
	e.m.items = failingItems{items}
	code, b := e.woCommit(w.WriteOffID)
	require.Equal(t, http.StatusServiceUnavailable, code, string(b))
	e.m.items = items
	require.Equal(t, WoApproved, e.writeOff(w.WriteOffID).State)
	require.Equal(t, kit.MustAmount("1000"), e.itemView(itemID).Residual, "the AR-07 adjustment rolled back")
	require.Equal(t, 0, e.n(`SELECT count(*) FROM ar07_adjustments WHERE tenant_id = $1 AND source_ref_type = 'WRITE_OFF'`, e.tenant))
	require.Equal(t, 0, e.n(`SELECT count(*) FROM ar_accounting_posting_requests WHERE tenant_id = $1 AND source_type = 'WRITE_OFF'`, e.tenant))
	require.Equal(t, 0, e.events(EvReceivableWritten))

	// closed period: PERIOD_CLOSED, nothing changes
	e.per.setErr(kit.Conflict(kit.CodePeriodClosed, "closed"))
	e.refused(http.StatusConflict, "PERIOD_CLOSED", "commitWriteOff", pCommitter, map[string]any{"write_off_id": w.WriteOffID, "expected_version": e.writeOff(w.WriteOffID).Version})
	e.per.setErr(nil)
	// not wired: 503 without a state change
	per := e.m.periods
	e.m.periods = nil
	e.refused(http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "commitWriteOff", pCommitter, map[string]any{"write_off_id": w.WriteOffID, "expected_version": e.writeOff(w.WriteOffID).Version})
	e.m.periods = per
	e.m.items = nil
	e.refused(http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "commitWriteOff", pCommitter, map[string]any{"write_off_id": w.WriteOffID, "expected_version": e.writeOff(w.WriteOffID).Version})
	e.m.items = items
	require.Equal(t, WoApproved, e.writeOff(w.WriteOffID).State)
	// the committer needs the commit action
	e.refused(http.StatusForbidden, "FORBIDDEN", "commitWriteOff", pTier3, map[string]any{"write_off_id": w.WriteOffID, "expected_version": e.writeOff(w.WriteOffID).Version})

	// the residual moved elsewhere (cash arrived): the write-off no longer fits and AR-07 refuses
	other := e.item(e.entity, "100.00")
	w2 := e.newWriteOff(other, "100", nil)
	e.woTax(w2.WriteOffID, TaxNoneRequired, "0")
	e.woApprove(w2.WriteOffID, pTier1)
	require.NoError(t, e.tx(func(tx pgx.Tx) error {
		_, err := e.m7.ApplyAdjustment(context.Background(), tx, e.caller(), kit.OpenItemAdjustment{OpenItemID: other, Kind: kit.AdjCashApplication, Amount: kit.MustAmount("60"),
			SourceRefType: "CASH_APPLICATION", SourceRefID: uuid.NewString(), EventKey: uuid.NewString(), Reason: "test"})
		return err
	}))
	e.refused(http.StatusUnprocessableEntity, "RECEIVABLE_RESIDUAL_INVALID", "commitWriteOff", pCommitter, map[string]any{"write_off_id": w2.WriteOffID, "expected_version": e.writeOff(w2.WriteOffID).Version})
	require.Equal(t, WoApproved, e.writeOff(w2.WriteOffID).State)
}

func TestWriteOff_ConcurrentCommitsHaveExactlyOneEffect(t *testing.T) {
	e := newEnv(t)
	itemID, w := e.approvedWriteOff("1000.00", "400.00")
	v := e.writeOff(w.WriteOffID).Version
	var wg sync.WaitGroup
	codes := make([]int, 6)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = e.post("commitWriteOff", pCommitter, map[string]any{"write_off_id": w.WriteOffID, "expected_version": v})
		}(i)
	}
	wg.Wait()
	for _, c := range codes {
		require.True(t, c == http.StatusOK || c == http.StatusConflict, "%v", codes)
	}
	require.Equal(t, WoCommitted, e.writeOff(w.WriteOffID).State)
	require.Equal(t, 1, e.n(`SELECT count(*) FROM ar07_adjustments WHERE tenant_id = $1 AND source_ref_type = 'WRITE_OFF'`, e.tenant), "one adjustment")
	require.Equal(t, 1, e.n(`SELECT count(*) FROM ar_accounting_posting_requests WHERE tenant_id = $1 AND source_type = 'WRITE_OFF'`, e.tenant), "one accounting event")
	require.Equal(t, 1, e.events(EvReceivableWritten))
	require.Equal(t, kit.MustAmount("600"), e.itemView(itemID).Residual, "written off exactly once")
	require.Equal(t, "PARTIALLY_SETTLED", e.itemView(itemID).State)
}

// ═══ tenant isolation ════════════════════════════════════════════════════════

func TestTenantIsolation(t *testing.T) {
	e := newEnv(t)
	r := e.approvedRefund("100.00")
	_, w := e.approvedWriteOff("1000.00", "400.00")
	other := uuid.NewString()
	for _, path := range []string{"getRefund?refund_id=" + r.RefundID, "getRefundPaymentStatus?refund_id=" + r.RefundID, "getWriteOff?write_off_id=" + w.WriteOffID,
		"getAvailableActions?refund_id=" + r.RefundID, "getAvailableActions?write_off_id=" + w.WriteOffID} {
		code, b := e.get(path, "anyone", other)
		require.Equal(t, http.StatusNotFound, code, path+" "+string(b))
	}
	code, b := e.get("getRefundWriteOffHistory?refund_id="+r.RefundID, "anyone", other)
	require.Equal(t, http.StatusOK, code)
	require.JSONEq(t, `{"history":[]}`, string(b), "another tenant sees no history")
	// commands against another tenant's ids look exactly like absent ones
	code, b, _ = call(t, e.h, "POST", base+"commands/submitRefundForPayment", other, pReleaser, "", map[string]any{"refund_id": r.RefundID, "expected_version": 2})
	require.Equal(t, http.StatusNotFound, code, string(b))
	code, b, _ = call(t, e.h, "POST", base+"commands/commitWriteOff", other, pCommitter, "", map[string]any{"write_off_id": w.WriteOffID, "expected_version": 3})
	require.Equal(t, http.StatusNotFound, code, string(b))
	exc, err := e.m.ControlExceptions(context.Background(), other, e.entity)
	require.NoError(t, err)
	require.Empty(t, exc)
	// the posting listener is tenant scoped too
	require.Error(t, e.m.OnPostingOutcome(context.Background(), kit.QueuedPosting{TenantID: other, SourceType: kit.SourceRefund, SourceID: r.RefundID}, kit.PostingOutcome{Status: kit.PostingPosted}))
	require.Equal(t, RfApproved, e.refund(r.RefundID).State)
}

func TestAvailableActions_RefundIsDerivedFromStateAndPermissions(t *testing.T) {
	e := newEnv(t)
	r := e.newRefund(e.receipt("500.00"), "100")
	actions := func(principal string) map[string]bool {
		code, b := e.get("getAvailableActions?refund_id="+r.RefundID, principal, e.tenant)
		require.Equal(t, http.StatusOK, code, string(b))
		var v struct {
			Actions []actionView `json:"actions"`
		}
		require.NoError(t, json.Unmarshal(b, &v))
		out := map[string]bool{}
		for _, a := range v.Actions {
			out[a.Command] = a.Allowed
		}
		return out
	}
	require.Equal(t, map[string]bool{"changeRefundDestination": true, "cancelUnsubmittedRefund": true, "approveRefund": false}, actions(pRequester))
	require.True(t, actions(pApprover)["approveRefund"])
	require.False(t, actions(pNobody)["approveRefund"])
	e.approveRefund(r.RefundID)
	require.NotContains(t, actions(pApprover), "approveRefund")
	require.True(t, actions(pReleaser)["submitRefundForPayment"])
	require.False(t, actions(pRequester)["submitRefundForPayment"], "the maker cannot release their own refund")
	e.submit(r.RefundID)
	require.Equal(t, map[string]bool{"reconcileRefundState": true}, actions(pReleaser))
}
