//go:build integration

// Real-Postgres tests for AR-07. Run (serially):
//
//	go test -tags=integration -count=1 -p 1 -timeout=600s ./internal/ar07/...
//
// The embedded superuser bypasses RLS, so isolation tests prove the explicit tenant_id predicates, not the policies.
// Spec section 18 scenarios carry their number: TestNeg19 (negative residual), TestNeg20 (manual balance overwrite),
// TestNeg21 (AR matches GL while open items are duplicated / missing).
package ar07

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/kit/kittest"
)

var pool *pgxpool.Pool

func TestMain(m *testing.M) {
	var stop func()
	pool, stop = kittest.Start("ar07_test", 17700)
	code := m.Run()
	stop()
	os.Exit(code)
}

var t0 = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

type env struct {
	t      *testing.T
	m      *Module
	h      http.Handler
	tenant string
	entity string
	party  string
	clock  *testClock
	inv    *fakeInvoices
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, tenant: uuid.NewString(), entity: uuid.NewString(), party: uuid.NewString(), clock: &testClock{t: t0}, inv: &fakeInvoices{}}
	grants := standardGrants()
	grants["resolver"] = map[string]bool{ActionDisputeManage: true}
	deps := testDeps(&fakeAuthz{grants: grants}, e.clock.Now)
	deps.DB = kit.NewDB(pool)
	e.m = New(deps)
	e.m.SetInvoices(e.inv)
	e.h = testRouter(e.m)
	return e
}

func (e *env) caller(principal string) kit.Caller {
	return kit.Caller{TenantID: e.tenant, PrincipalID: principal, CorrelationID: "corr-" + uuid.NewString(), LegalEntityID: e.entity}
}

func (e *env) tx(fn func(tx pgx.Tx) error) error {
	return e.m.deps.DB.WithTx(context.Background(), e.tenant, fn)
}

func doc(party, amount, currency, issue, due string) kit.OpenItemFromDocument {
	i, _ := time.Parse("2006-01-02", issue)
	d, _ := time.Parse("2006-01-02", due)
	return kit.OpenItemFromDocument{DocumentKind: kit.DocInvoice, SourceDocumentID: uuid.NewString(), DocumentNumber: "INV-" + uuid.NewString()[:6],
		CustomerPartyID: party, Currency: currency, OriginalAmount: kit.MustAmount(amount), IssueDate: i, DueDate: d}
}

// create opens an item through the port (as AR-04 does inside the issuing tx).
func (e *env) create(amount, currency, issue, due string) (kit.OpenItemRef, kit.OpenItemFromDocument) {
	e.t.Helper()
	in := doc(e.party, amount, currency, issue, due)
	var ref kit.OpenItemRef
	require.NoError(e.t, e.tx(func(tx pgx.Tx) (err error) {
		ref, err = e.m.CreateFromDocument(context.Background(), tx, e.caller(pBoss), in)
		return
	}))
	return ref, in
}

func (e *env) adjust(principal, itemID, kind, amount, key string) (*kit.OpenItemView, error) {
	var v *kit.OpenItemView
	err := e.tx(func(tx pgx.Tx) (err error) {
		v, err = e.m.ApplyAdjustment(context.Background(), tx, e.caller(principal), kit.OpenItemAdjustment{OpenItemID: itemID, Kind: kind, Amount: kit.MustAmount(amount),
			SourceRefType: kind, SourceRefID: "ref-" + key, EventKey: key, Reason: "test"})
		return
	})
	return v, err
}

func (e *env) mustAdjust(itemID, kind, amount string) *kit.OpenItemView {
	e.t.Helper()
	v, err := e.adjust(pBoss, itemID, kind, amount, uuid.NewString())
	require.NoError(e.t, err)
	return v
}

func (e *env) cmd(principal, name string, body map[string]any) (int, []byte) {
	if _, ok := body["legal_entity_id"]; !ok {
		body["legal_entity_id"] = e.entity
	}
	return call(e.t, e.h, http.MethodPost, base+"commands/"+name, e.tenant, principal, "", body)
}

func (e *env) query(path string) (int, []byte) {
	return call(e.t, e.h, http.MethodGet, base+path, e.tenant, "reader", "", nil)
}

func problemCode(t *testing.T, err error) string {
	t.Helper()
	var p *kit.Problem
	require.ErrorAs(t, err, &p, "%v", err)
	return p.Code
}

func count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}

func (e *env) ledgerRows(itemID string) int {
	return count(e.t, `SELECT count(*) FROM ar07_adjustments WHERE tenant_id = $1 AND open_item_id = $2`, e.tenant, itemID)
}

func (e *env) events(itemID, typ string) int {
	return count(e.t, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND aggregate_id = $2 AND event_type = $3`, e.tenant, itemID, typ)
}

func (e *env) allEvents(itemID string) int {
	return count(e.t, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND aggregate_id = $2`, e.tenant, itemID)
}

func (e *env) audits(itemID string) int {
	return count(e.t, `SELECT count(*) FROM ar_audit_events WHERE tenant_id = $1 AND aggregate_id = $2`, e.tenant, itemID)
}

func (e *env) item(id string) *OpenItem {
	e.t.Helper()
	var o *OpenItem
	require.NoError(e.t, e.m.read(context.Background(), e.tenant, func(tx pgx.Tx) (err error) { o, err = getItem(context.Background(), tx, e.tenant, id, false); return }))
	return o
}

func decode[T any](t *testing.T, b []byte) T {
	t.Helper()
	var v T
	require.NoError(t, json.Unmarshal(b, &v), string(b))
	return v
}

// ---- lifecycle, events, audit, dimension mirroring ----

func TestLifecycle_EventsAuditAndInvoiceMirror(t *testing.T) {
	e := newEnv(t)
	ref, in := e.create("100.00", "USD", "2026-10-01", "2026-10-31")
	id := ref.OpenItemID
	require.Equal(t, 1, ref.Version)
	require.Equal(t, 1, e.events(id, EvOpenItemCreated))
	o := e.item(id)
	require.Equal(t, StateOpen, o.State)
	require.Equal(t, kit.MustAmount("100"), o.Residual)

	v := e.mustAdjust(id, kit.AdjCashApplication, "40.00")
	require.Equal(t, StatePartiallySettl, v.State)
	require.Equal(t, "60.0000", v.Residual.String())
	s, _ := e.inv.last()
	require.Equal(t, in.SourceDocumentID+":PARTIALLY_PAID", s)

	v = e.mustAdjust(id, kit.AdjCashUnapply, "40.00") // reopens
	require.Equal(t, StateOpen, v.State)
	s, _ = e.inv.last()
	require.Equal(t, in.SourceDocumentID+":UNPAID", s)

	v = e.mustAdjust(id, kit.AdjCashApplication, "100.00")
	require.Equal(t, StateSettled, v.State)
	s, _ = e.inv.last()
	require.Equal(t, in.SourceDocumentID+":PAID", s)
	require.Equal(t, 1, e.events(id, EvPartiallySettled))
	require.Equal(t, 1, e.events(id, EvSettled))
	require.Equal(t, 1, e.events(id, EvReopened))
	require.Equal(t, 4, e.audits(id)) // create + three adjustments
}

func TestMirror_CreditNoteWriteOffAndDispute(t *testing.T) {
	e := newEnv(t)
	a, inA := e.create("100.00", "USD", "2026-10-01", "2026-10-31")
	e.mustAdjust(a.OpenItemID, kit.AdjCreditNote, "100.00")
	s, _ := e.inv.last()
	require.Equal(t, inA.SourceDocumentID+":CREDITED", s)
	require.Equal(t, StateSettled, e.item(a.OpenItemID).State)

	b, inB := e.create("80.00", "USD", "2026-10-01", "2026-10-31")
	e.mustAdjust(b.OpenItemID, kit.AdjWriteOff, "80.00")
	s, _ = e.inv.last()
	require.Equal(t, inB.SourceDocumentID+":WRITTEN_OFF", s)
	require.Equal(t, StateWrittenOff, e.item(b.OpenItemID).State)
	require.Equal(t, 1, e.events(b.OpenItemID, EvWrittenOff))
	// terminal: no further adjustment
	_, err := e.adjust(pBoss, b.OpenItemID, kit.AdjCashUnapply, "10.00", "late")
	require.Equal(t, kit.CodeInvalidTransition, problemCode(t, err))

	c, inC := e.create("50.00", "USD", "2026-10-01", "2026-10-31")
	code, body := e.cmd(pDispute, "openDispute", map[string]any{"open_item_id": c.OpenItemID, "expected_version": 1, "reason_code": "WRONG_QTY", "evidence_id": "EV-1"})
	require.Equal(t, http.StatusOK, code, string(body))
	_, d := e.inv.last()
	require.Equal(t, inC.SourceDocumentID+":DISPUTED", d)
	code, body = e.cmd("resolver", "resolveDispute", map[string]any{"open_item_id": c.OpenItemID, "expected_version": 2, "resolution_code": "REJECTED", "evidence_id": "EV-2"})
	require.Equal(t, http.StatusOK, code, string(body))
	_, d = e.inv.last()
	require.Equal(t, inC.SourceDocumentID+":NONE", d)

	// a debit-note receivable is not mirrored onto an invoice dimension
	before := len(e.inv.settlement)
	dn := doc(e.party, "20.00", "USD", "2026-10-02", "2026-11-01")
	dn.DocumentKind = kit.DocDebitNote
	require.NoError(t, e.tx(func(tx pgx.Tx) error {
		_, err := e.m.CreateFromDocument(context.Background(), tx, e.caller(pBoss), dn)
		return err
	}))
	v, err := e.m.GetBySourceDocument(context.Background(), e.tenant, dn.SourceDocumentID)
	require.NoError(t, err)
	e.mustAdjust(v.OpenItemID, kit.AdjCashApplication, "20.00")
	require.Equal(t, before, len(e.inv.settlement))
}

// ---- section 18 #19: residual becomes negative ----

func TestNeg19_NegativeResidualBlocked(t *testing.T) {
	e := newEnv(t)
	ref, _ := e.create("100.00", "USD", "2026-10-01", "2026-10-31")
	id := ref.OpenItemID
	e.mustAdjust(id, kit.AdjCashApplication, "60.00")
	eventsBefore, auditsBefore := e.allEvents(id), e.audits(id)

	_, err := e.adjust(pBoss, id, kit.AdjCashApplication, "60.01", "overshoot")
	require.Equal(t, kit.CodeReceivableResidualInvalid, problemCode(t, err))
	_, err = e.adjust(pBoss, id, kit.AdjCreditNote, "100.00", "credit-too-big")
	require.Equal(t, kit.CodeReceivableResidualInvalid, problemCode(t, err))
	_, err = e.adjust(pBoss, id, kit.AdjCashUnapply, "60.01", "above-original") // 40 + 60.01 > 100
	require.Equal(t, kit.CodeReceivableResidualInvalid, problemCode(t, err))

	require.Equal(t, "40.0000", e.item(id).Residual.String(), "the residual is untouched")
	require.Equal(t, 1, e.ledgerRows(id), "no ledger row for a refused adjustment")
	require.Equal(t, eventsBefore, e.allEvents(id), "no event for a refused adjustment")
	require.Equal(t, auditsBefore, e.audits(id))
	require.Equal(t, 0, count(t, `SELECT count(*) FROM ar07_adjustments WHERE event_key IN ('overshoot','credit-too-big','above-original')`))

	// the data layer refuses it as well (a bug in the service could not create it)
	_, err = pool.Exec(context.Background(), `UPDATE ar07_open_items SET residual_amount = -1 WHERE open_item_id = $1`, id)
	require.Error(t, err)
	_, err = pool.Exec(context.Background(), `INSERT INTO ar07_adjustments (adjustment_id, tenant_id, legal_entity_id, open_item_id, kind, amount, signed_effect, balance_before,
		balance_after, source_ref_type, source_ref_id, event_key, actor_id, version_after) VALUES (gen_random_uuid(),$1,$2,$3,'CASH_APPLICATION',50,-50,40,-10,'X','y','neg-direct','sql',9)`,
		e.tenant, e.entity, id)
	require.Error(t, err)
}

// ---- section 18 #20: manual balance overwrite ----

func TestNeg20_ManualBalanceOverwriteRefused(t *testing.T) {
	e := newEnv(t)
	ref, _ := e.create("100.00", "USD", "2026-10-01", "2026-10-31")
	id := ref.OpenItemID

	// HTTP: no field sets a balance
	code, body := e.cmd(pAdjuster, "applyReceivableAdjustment", map[string]any{"open_item_id": id, "expected_version": 1, "kind": "CORRECTION", "amount": "10.00",
		"reason_code": "X", "evidence_id": "E", "owning_document_type": "CORRECTION_CASE", "owning_document_id": "CC-1", "residual_amount": "0.00"})
	require.Equal(t, http.StatusBadRequest, code, string(body))
	// HTTP: only CORRECTION; cash / write-off / refund / credit arrive only via the port
	for _, k := range []string{kit.AdjCashApplication, kit.AdjWriteOff, kit.AdjRefund, kit.AdjCreditNote, kit.AdjCashUnapply} {
		code, body = e.cmd(pAdjuster, "applyReceivableAdjustment", map[string]any{"open_item_id": id, "expected_version": 1, "kind": k, "amount": "10.00",
			"reason_code": "X", "evidence_id": "E", "owning_document_type": "CASH_APPLICATION", "owning_document_id": "CA-1"})
		require.Equal(t, http.StatusUnprocessableEntity, code, k+string(body))
	}
	// HTTP: privileged action only
	code, _ = e.cmd(pDispute, "applyReceivableAdjustment", map[string]any{"open_item_id": id, "expected_version": 1, "kind": "CORRECTION", "amount": "10.00"})
	require.Equal(t, http.StatusForbidden, code)
	// HTTP: free-form (no owning document) refused
	code, body = e.cmd(pAdjuster, "applyReceivableAdjustment", map[string]any{"open_item_id": id, "expected_version": 1, "kind": "CORRECTION", "amount": "10.00",
		"reason_code": "X", "evidence_id": "E"})
	require.Equal(t, http.StatusBadRequest, code, string(body))
	require.Equal(t, "100.0000", e.item(id).Residual.String())
	require.Equal(t, 0, e.ledgerRows(id))

	// SQL: a direct residual edit, an orphan ledger row and a ledger rewrite are all refused
	_, err := pool.Exec(context.Background(), `UPDATE ar07_open_items SET residual_amount = 1 WHERE open_item_id = $1`, id)
	require.ErrorContains(t, err, "AR07_BALANCE_NOT_EDITABLE")
	_, err = pool.Exec(context.Background(), `UPDATE ar07_open_items SET original_amount = 1, residual_amount = 1 WHERE open_item_id = $1`, id)
	require.ErrorContains(t, err, "AR07_OPEN_ITEM_IMMUTABLE")
	_, err = pool.Exec(context.Background(), `DELETE FROM ar07_open_items WHERE open_item_id = $1`, id)
	require.Error(t, err)
	_, err = pool.Exec(context.Background(), `INSERT INTO ar07_adjustments (adjustment_id, tenant_id, legal_entity_id, open_item_id, kind, amount, signed_effect, balance_before,
		balance_after, source_ref_type, source_ref_id, event_key, actor_id, version_after) VALUES (gen_random_uuid(),$1,$2,$3,'CASH_APPLICATION',10,-10,55,45,'X','y','sql-chain','sql',2)`,
		e.tenant, e.entity, id)
	require.ErrorContains(t, err, "AR07_LEDGER_CHAIN_BROKEN")
	// a forged but chain-consistent ledger row without the matching item update cannot move the residual either
	_, err = pool.Exec(context.Background(), `INSERT INTO ar07_adjustments (adjustment_id, tenant_id, legal_entity_id, open_item_id, kind, amount, signed_effect, balance_before,
		balance_after, source_ref_type, source_ref_id, event_key, actor_id, version_after) VALUES (gen_random_uuid(),$1,$2,$3,'CASH_APPLICATION',10,-10,100,90,'X','y','sql-ok','sql',2)`,
		e.tenant, e.entity, id)
	require.NoError(t, err) // the ledger is append-only evidence; it still does not change the item
	require.Equal(t, "100.0000", e.item(id).Residual.String())
	// ...and the control exception surfaces on the reconstruction (never plugged)
	code, body = e.query("getBalanceAsOf?as_of=2026-12-31&open_item_id=" + id)
	require.Equal(t, http.StatusConflict, code, string(body))
	require.Equal(t, kit.CodeARGLReconciliationBreak, errCode(t, body))

	// the legitimate privileged path: a CORRECTION that names its owning document
	e2 := newEnv(t)
	ref2, _ := e2.create("100.00", "USD", "2026-10-01", "2026-10-31")
	good := map[string]any{"open_item_id": ref2.OpenItemID, "expected_version": 1, "kind": "CORRECTION", "amount": "100.00", "reason_code": "BILLING_ERROR",
		"evidence_id": "EV-9", "owning_document_type": "CORRECTION_CASE", "owning_document_id": "CC-9"}
	code, body = e2.cmd(pAdjuster, "applyReceivableAdjustment", good)
	require.Equal(t, http.StatusOK, code, string(body))
	o := decode[OpenItem](t, body)
	require.Equal(t, StateCorrected, o.State)
	require.True(t, o.Residual.IsZero())
	require.Equal(t, 1, e2.ledgerRows(ref2.OpenItemID))
	// the same owning document replayed (new Idempotency-Key) is a no-op, and a stale expected_version on a replay still is
	code, _ = e2.cmd(pAdjuster, "applyReceivableAdjustment", good)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, 1, e2.ledgerRows(ref2.OpenItemID))
	// a different correction document against the corrected (terminal) item is refused
	good["owning_document_id"] = "CC-10"
	good["expected_version"] = 2
	code, body = e2.cmd(pAdjuster, "applyReceivableAdjustment", good)
	require.Equal(t, http.StatusConflict, code, string(body))
	// the ledger itself cannot be edited
	_, err = pool.Exec(context.Background(), `UPDATE ar07_adjustments SET amount = 1 WHERE open_item_id = $1`, ref2.OpenItemID)
	require.Error(t, err)
	_, err = pool.Exec(context.Background(), `DELETE FROM ar07_adjustments WHERE open_item_id = $1`, ref2.OpenItemID)
	require.Error(t, err)
}

// ---- section 18 #21: AR matches GL while open items are duplicated or missing ----

func TestNeg21_DuplicateAndMissingOpenItems(t *testing.T) {
	e := newEnv(t)
	a, inA := e.create("100.00", "USD", "2026-10-01", "2026-10-31")
	_, inB := e.create("100.00", "USD", "2026-10-01", "2026-10-31")

	// replay of the same document returns the same open item; a second row is impossible
	var again kit.OpenItemRef
	require.NoError(t, e.tx(func(tx pgx.Tx) (err error) {
		again, err = e.m.CreateFromDocument(context.Background(), tx, e.caller(pBoss), inA)
		return
	}))
	require.Equal(t, a.OpenItemID, again.OpenItemID)
	require.Equal(t, 1, count(t, `SELECT count(*) FROM ar07_open_items WHERE tenant_id = $1 AND source_document_id = $2`, e.tenant, inA.SourceDocumentID))
	require.Equal(t, 1, e.events(a.OpenItemID, EvOpenItemCreated), "a replay emits nothing")
	// same document, different content: conflict, not a second item
	other := inA
	other.OriginalAmount = kit.MustAmount("250.00")
	err := e.tx(func(tx pgx.Tx) error {
		_, err := e.m.CreateFromDocument(context.Background(), tx, e.caller(pBoss), other)
		return err
	})
	require.Equal(t, http.StatusConflict, mustProblem(t, err).Status)
	// SQL: the unique constraint is the backstop
	_, err = pool.Exec(context.Background(), `INSERT INTO ar07_open_items (open_item_id, tenant_id, legal_entity_id, document_kind, source_document_id, customer_party_id, currency,
		original_amount, residual_amount, issue_date, due_date, created_by) VALUES (gen_random_uuid(),$1,$2,'INVOICE',$3,$4,'USD',100,100,'2026-10-01','2026-10-31','sql')`,
		e.tenant, e.entity, inA.SourceDocumentID, e.party)
	require.True(t, kit.IsUniqueViolation(err, "uq_ar07_source_document"), "%v", err)
	// SQL: no free-form opening balance
	_, err = pool.Exec(context.Background(), `INSERT INTO ar07_open_items (open_item_id, tenant_id, legal_entity_id, document_kind, source_document_id, customer_party_id, currency,
		original_amount, residual_amount, issue_date, due_date, created_by) VALUES (gen_random_uuid(),$1,$2,'INVOICE',gen_random_uuid(),$3,'USD',100,10,'2026-10-01','2026-10-31','sql')`,
		e.tenant, e.entity, e.party)
	require.Error(t, err)
	// a credit note is not a receivable of its own
	cn := doc(e.party, "10.00", "USD", "2026-10-02", "2026-10-31")
	cn.DocumentKind = kit.DocCreditNote
	err = e.tx(func(tx pgx.Tx) error {
		_, err := e.m.CreateFromDocument(context.Background(), tx, e.caller(pBoss), cn)
		return err
	})
	require.Equal(t, http.StatusBadRequest, mustProblem(t, err).Status)

	ctx := context.Background()
	// complete: issued docs == open items (count, sum, per document)
	rep, err := e.m.Completeness(ctx, e.tenant, e.entity, []ExpectedDocument{{inA.SourceDocumentID, "USD", inA.OriginalAmount}, {inB.SourceDocumentID, "USD", inB.OriginalAmount}})
	require.NoError(t, err)
	require.True(t, rep.Balanced, "%+v", rep)
	require.NoError(t, rep.Err())
	require.Equal(t, 2, rep.OpenItemRows)

	// the headline total is equal (200 == 200) yet a document is missing and another has the wrong amount: still a break
	missing := uuid.NewString()
	rep, err = e.m.Completeness(ctx, e.tenant, e.entity, []ExpectedDocument{{inA.SourceDocumentID, "USD", kit.MustAmount("200.00")}, {missing, "USD", kit.MustAmount("0.0001")}})
	require.NoError(t, err)
	require.False(t, rep.Balanced)
	require.Equal(t, kit.CodeARGLReconciliationBreak, problemCode(t, rep.Err()))
	require.Equal(t, []string{missing}, rep.Missing)
	require.Equal(t, []string{inB.SourceDocumentID}, rep.Unexpected)
	require.Equal(t, []string{inA.SourceDocumentID}, rep.AmountMismatch)

	// an issued document with NO open item is detected
	third := uuid.NewString()
	rep, _ = e.m.Completeness(ctx, e.tenant, e.entity, []ExpectedDocument{{inA.SourceDocumentID, "USD", inA.OriginalAmount}, {inB.SourceDocumentID, "USD", inB.OriginalAmount}, {third, "USD", kit.MustAmount("5.00")}})
	require.False(t, rep.Balanced)
	require.Equal(t, []string{third}, rep.Missing)
	// a duplicate in the issued list is reported too
	rep, _ = e.m.Completeness(ctx, e.tenant, e.entity, []ExpectedDocument{{inA.SourceDocumentID, "USD", inA.OriginalAmount}, {inA.SourceDocumentID, "USD", inA.OriginalAmount}, {inB.SourceDocumentID, "USD", inB.OriginalAmount}})
	require.False(t, rep.Balanced)
	require.Equal(t, []string{inA.SourceDocumentID}, rep.DuplicateDocuments)
}

func mustProblem(t *testing.T, err error) *kit.Problem {
	t.Helper()
	var p *kit.Problem
	require.ErrorAs(t, err, &p, "%v", err)
	return p
}

// ---- idempotency, concurrency, atomicity ----

func TestAdjustment_ReplayIsNoOp(t *testing.T) {
	e := newEnv(t)
	ref, _ := e.create("100.00", "USD", "2026-10-01", "2026-10-31")
	id := ref.OpenItemID
	v1, err := e.adjust(pBoss, id, kit.AdjCashApplication, "30.00", "evt-1")
	require.NoError(t, err)
	events, audits := e.allEvents(id), e.audits(id)
	v2, err := e.adjust(pBoss, id, kit.AdjCashApplication, "30.00", "evt-1")
	require.NoError(t, err)
	require.Equal(t, v1.Version, v2.Version)
	require.Equal(t, v1.Residual, v2.Residual)
	require.Equal(t, "70.0000", v2.Residual.String())
	require.Equal(t, 1, e.ledgerRows(id))
	require.Equal(t, events, e.allEvents(id))
	require.Equal(t, audits, e.audits(id))
	require.Equal(t, 1, len(e.inv.settlement))
	// same key, different amount / kind: refused, never silently applied
	_, err = e.adjust(pBoss, id, kit.AdjCashApplication, "31.00", "evt-1")
	require.Equal(t, kit.CodeIdempotencyReused, problemCode(t, err))
	_, err = e.adjust(pBoss, id, kit.AdjWriteOff, "30.00", "evt-1")
	require.Equal(t, kit.CodeIdempotencyReused, problemCode(t, err))
	// same key on another item: refused
	ref2, _ := e.create("100.00", "USD", "2026-10-01", "2026-10-31")
	_, err = e.adjust(pBoss, ref2.OpenItemID, kit.AdjCashApplication, "30.00", "evt-1")
	require.Equal(t, kit.CodeIdempotencyReused, problemCode(t, err))
	require.Equal(t, "100.0000", e.item(ref2.OpenItemID).Residual.String())
	// the key is per tenant: another tenant may reuse it
	e3 := newEnv(t)
	ref3, _ := e3.create("100.00", "USD", "2026-10-01", "2026-10-31")
	_, err = e3.adjust(pBoss, ref3.OpenItemID, kit.AdjCashApplication, "30.00", "evt-1")
	require.NoError(t, err)
}

func TestConcurrent_LastResidualOneWins(t *testing.T) {
	e := newEnv(t)
	ref, _ := e.create("100.00", "USD", "2026-10-01", "2026-10-31")
	id := ref.OpenItemID
	const n = 10
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = e.adjust(pBoss, id, kit.AdjCashApplication, "100.00", fmt.Sprintf("race-%d", i))
		}(i)
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
			continue
		}
		require.Equal(t, kit.CodeReceivableResidualInvalid, problemCode(t, err))
	}
	require.Equal(t, 1, wins, "exactly one adjustment takes the last residual")
	require.True(t, e.item(id).Residual.IsZero())
	require.Equal(t, 1, e.ledgerRows(id))
	require.Equal(t, 1, e.events(id, EvSettled))
	require.Equal(t, 1, len(e.inv.settlement))
}

func TestConcurrent_SameEventKeyAppliesOnce(t *testing.T) {
	e := newEnv(t)
	ref, _ := e.create("100.00", "USD", "2026-10-01", "2026-10-31")
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = e.adjust(pBoss, ref.OpenItemID, kit.AdjCashApplication, "10.00", "same-key")
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, 1, e.ledgerRows(ref.OpenItemID))
	require.Equal(t, "90.0000", e.item(ref.OpenItemID).Residual.String())
}

func TestRollbackAtomicity(t *testing.T) {
	e := newEnv(t)
	ref, _ := e.create("100.00", "USD", "2026-10-01", "2026-10-31")
	id := ref.OpenItemID
	events, audits := e.allEvents(id), e.audits(id)

	// the caller's transaction fails after the adjustment: nothing remains
	boom := errors.New("boom")
	err := e.tx(func(tx pgx.Tx) error {
		if _, err := e.m.ApplyAdjustment(context.Background(), tx, e.caller(pBoss), kit.OpenItemAdjustment{OpenItemID: id, Kind: kit.AdjCashApplication, Amount: kit.MustAmount("25.00"),
			SourceRefType: "CASH_APPLICATION", SourceRefID: "r", EventKey: "rb-1"}); err != nil {
			return err
		}
		return boom
	})
	require.ErrorIs(t, err, boom)
	require.Equal(t, "100.0000", e.item(id).Residual.String())
	require.Equal(t, 0, e.ledgerRows(id))
	require.Equal(t, events, e.allEvents(id))
	require.Equal(t, audits, e.audits(id))

	// the invoice dimension write fails: the whole adjustment is rolled back (no half-applied settlement)
	e.inv.setErr(kit.Unavailable("ar04 down"))
	_, err = e.adjust(pBoss, id, kit.AdjCashApplication, "25.00", "rb-2")
	require.Equal(t, kit.CodeDependencyUnavailable, problemCode(t, err))
	require.Equal(t, "100.0000", e.item(id).Residual.String())
	require.Equal(t, 0, e.ledgerRows(id))
	require.Equal(t, events, e.allEvents(id))
	e.inv.setErr(nil)
	_, err = e.adjust(pBoss, id, kit.AdjCashApplication, "25.00", "rb-2") // the same event can then be applied
	require.NoError(t, err)

	// a creation rolled back with the caller's transaction leaves no open item
	in := doc(e.party, "10.00", "USD", "2026-10-01", "2026-10-31")
	err = e.tx(func(tx pgx.Tx) error {
		if _, err := e.m.CreateFromDocument(context.Background(), tx, e.caller(pBoss), in); err != nil {
			return err
		}
		return boom
	})
	require.ErrorIs(t, err, boom)
	require.Equal(t, 0, count(t, `SELECT count(*) FROM ar07_open_items WHERE source_document_id = $1`, in.SourceDocumentID))
	require.Equal(t, 0, count(t, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = $2 AND payload::text LIKE '%'||$3||'%'`, e.tenant, EvOpenItemCreated, in.SourceDocumentID))
}

// ---- as-of reconstruction ----

func TestBalanceAsOf_ReconstructedFromLedger(t *testing.T) {
	e := newEnv(t)
	ref, _ := e.create("100.00", "USD", "2026-10-01", "2026-10-31")
	id := ref.OpenItemID
	e.clock.Set(time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC))
	e.mustAdjust(id, kit.AdjCashApplication, "30.00")
	e.clock.Set(time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC))
	e.mustAdjust(id, kit.AdjCreditNote, "20.00")
	e.clock.Set(time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC))
	e.mustAdjust(id, kit.AdjCashUnapply, "10.00")

	bal := func(asOf string) string {
		code, body := e.query("getBalanceAsOf?open_item_id=" + id + "&as_of=" + asOf)
		require.Equal(t, http.StatusOK, code, string(body))
		r := decode[struct {
			Items []asOfItem `json:"items"`
		}](t, body)
		require.Len(t, r.Items, 1)
		return r.Items[0].Balance.String()
	}
	require.Equal(t, "100.0000", bal("2026-10-04"))
	require.Equal(t, "100.0000", bal("2026-10-05T11:59:59Z"))
	require.Equal(t, "70.0000", bal("2026-10-05T12:00:00Z"))
	require.Equal(t, "70.0000", bal("2026-10-05"))
	require.Equal(t, "70.0000", bal("2026-10-09"))
	require.Equal(t, "50.0000", bal("2026-10-10"))
	require.Equal(t, "60.0000", bal("2026-10-12"))
	require.Equal(t, "60.0000", bal("2027-01-01"))
	require.Equal(t, "60.0000", e.item(id).Residual.String(), "the stored residual equals the ledger reconstruction")
	// before the document existed
	code, _ := e.query("getBalanceAsOf?open_item_id=" + id + "&as_of=2026-09-30")
	require.Equal(t, http.StatusNotFound, code)

	// customer level, per currency
	e.create("40.00", "EUR", "2026-10-01", "2026-10-31")
	code, body := e.query("getBalanceAsOf?as_of=2026-10-04&legal_entity_id=" + e.entity + "&customer_party_id=" + e.party)
	require.Equal(t, http.StatusOK, code, string(body))
	r := decode[struct {
		Balances []struct {
			Currency string     `json:"currency"`
			Balance  kit.Amount `json:"balance"`
		} `json:"balances"`
	}](t, body)
	require.Len(t, r.Balances, 2)
	require.Equal(t, "EUR", r.Balances[0].Currency)
	require.Equal(t, "40.0000", r.Balances[0].Balance.String())
	require.Equal(t, "100.0000", r.Balances[1].Balance.String())
}

// ---- aging ----

type agingResp struct {
	AsOf       string          `json:"as_of"`
	Basis      map[string]any  `json:"basis"`
	Currencies []agingCurrency `json:"currencies"`
}

func bucketAmt(cl *agingClass, bucket string) string {
	if cl == nil {
		return "none"
	}
	for _, b := range cl.Buckets {
		if b.Bucket == bucket {
			return b.Amount.String()
		}
	}
	return "none"
}

func TestAging_BucketsAsOfExplicitDate(t *testing.T) {
	e := newEnv(t)
	cur, _ := e.create("100.00", "USD", "2026-05-01", "2026-10-20") // current on 10-12
	b, _ := e.create("100.00", "USD", "2026-05-01", "2026-10-05")   // 7 days past due
	c, _ := e.create("30.00", "USD", "2026-05-01", "2026-08-01")    // 72 days: disputed
	d, _ := e.create("20.00", "USD", "2026-05-01", "2026-06-01")    // 133 days: held
	e.create("10.00", "USD", "2026-05-01", "2026-09-01")            // 41 days
	e.create("55.00", "EUR", "2026-05-01", "2026-09-10")            // 32 days EUR
	_ = cur
	e.clock.Set(time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC))
	e.mustAdjust(b.OpenItemID, kit.AdjCashApplication, "40.00") // paid on 10-02
	code, body := e.cmd(pDispute, "openDispute", map[string]any{"open_item_id": c.OpenItemID, "expected_version": 1, "reason_code": "R", "evidence_id": "E"})
	require.Equal(t, http.StatusOK, code, string(body))
	code, body = e.cmd(pHold, "placeCollectionHold", map[string]any{"open_item_id": d.OpenItemID, "expected_version": 1, "reason_code": "R"})
	require.Equal(t, http.StatusOK, code, string(body))

	q := "getAging?legal_entity_id=" + e.entity + "&customer_party_id=" + e.party + "&as_of="
	code, body = e.query(q + "2026-10-12")
	require.Equal(t, http.StatusOK, code, string(body))
	r := decode[agingResp](t, body)
	require.Equal(t, "2026-10-12", r.AsOf)
	require.Equal(t, "2026-10-12", r.Basis["as_of"])
	require.NotEmpty(t, r.Basis["day_count"])
	require.Len(t, r.Currencies, 2)
	eur, usd := r.Currencies[0], r.Currencies[1]
	require.Equal(t, "EUR", eur.Currency)
	require.Equal(t, "31-60", eur.Classes["normal"].Buckets[0].Bucket)
	require.Equal(t, "USD", usd.Currency)
	require.Equal(t, "100.0000", bucketAmt(usd.Classes["normal"], "current"))
	require.Equal(t, "60.0000", bucketAmt(usd.Classes["normal"], "1-30"))
	require.Equal(t, "10.0000", bucketAmt(usd.Classes["normal"], "31-60"))
	require.Equal(t, "30.0000", bucketAmt(usd.Classes["disputed"], "61-90"), "disputed items are reported separately")
	require.Equal(t, "20.0000", bucketAmt(usd.Classes["held"], "90+"), "held items are reported separately")
	require.Equal(t, "none", bucketAmt(usd.Classes["normal"], "61-90"))
	require.Equal(t, "220.0000", usd.Total.String())
	require.Equal(t, 5, usd.Count)

	// an earlier date reproduces the earlier picture: the 10-02 payment had not happened on 09-15, nothing is past due yet
	code, body = e.query(q + "2026-09-15&currency=USD")
	require.Equal(t, http.StatusOK, code, string(body))
	r = decode[agingResp](t, body)
	require.Len(t, r.Currencies, 1)
	nm := r.Currencies[0].Classes
	require.Equal(t, "200.0000", bucketAmt(nm["normal"], "current"))
	require.Equal(t, "10.0000", bucketAmt(nm["normal"], "1-30"))
	require.Equal(t, "30.0000", bucketAmt(nm["disputed"], "31-60"))
	require.Equal(t, "20.0000", bucketAmt(nm["held"], "90+"))

	// the as_of date is mandatory; the calendar must match the one the items are aged on (#2 of the AR-07 table)
	code, _ = e.query("getAging?legal_entity_id=" + e.entity)
	require.Equal(t, http.StatusBadRequest, code)
	code, body = e.query(q + "2026-10-12&business_calendar_id=US_FEDERAL")
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	code, _ = e.query(q + "2026-10-12&business_calendar_id=" + DefaultCalendar)
	require.Equal(t, http.StatusOK, code)
}

// ---- customer receivables, port reads ----

func TestCustomerReceivablesAndPortReads(t *testing.T) {
	e := newEnv(t)
	a, _ := e.create("100.00", "USD", "2026-10-01", "2026-10-20")
	b, _ := e.create("50.00", "USD", "2026-10-01", "2026-10-10")
	e.create("70.00", "EUR", "2026-10-01", "2026-10-10")
	e.mustAdjust(a.OpenItemID, kit.AdjCashApplication, "25.00")
	e.cmd(pDispute, "openDispute", map[string]any{"open_item_id": b.OpenItemID, "expected_version": 1, "reason_code": "R", "evidence_id": "E"})
	ctx := context.Background()

	exp, err := e.m.Exposure(ctx, e.tenant, e.entity, e.party, "USD")
	require.NoError(t, err)
	require.Equal(t, "125.0000", exp.String(), "disputed residual still counts as exposure")
	_, err = e.m.Exposure(ctx, e.tenant, e.entity, e.party, "")
	require.Error(t, err)
	// An active dispute blocks collection contact (kit.OpenItems contract; spec
	// §18 #27) even without a collection hold.
	blocked, err := e.m.CollectionBlocked(ctx, e.tenant, e.entity, e.party)
	require.NoError(t, err)
	require.True(t, blocked)

	code, body := e.query("getCustomerReceivables?legal_entity_id=" + e.entity + "&customer_party_id=" + e.party)
	require.Equal(t, http.StatusOK, code, string(body))
	r := decode[struct {
		Summaries []customerSummary `json:"summaries"`
		OpenItems []OpenItem        `json:"open_items"`
	}](t, body)
	require.Len(t, r.Summaries, 2)
	require.Equal(t, "EUR", r.Summaries[0].Currency)
	usd := r.Summaries[1]
	require.Equal(t, 2, usd.OpenItemCount)
	require.Equal(t, "150.0000", usd.OriginalAmount.String())
	require.Equal(t, "125.0000", usd.Residual.String())
	require.Equal(t, "50.0000", usd.DisputedResidual.String())
	require.Len(t, r.OpenItems, 3)

	list, err := e.m.ListOpenForCustomer(ctx, e.tenant, e.entity, e.party, "USD")
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, b.OpenItemID, list[0].OpenItemID, "oldest due first")
	require.True(t, list[1].Residual == kit.MustAmount("75"))
	all, _ := e.m.ListOpenForCustomer(ctx, e.tenant, e.entity, e.party, "")
	require.Len(t, all, 3)

	// pagination
	code, body = e.query("getCustomerReceivables?limit=2&legal_entity_id=" + e.entity + "&customer_party_id=" + e.party)
	require.Equal(t, http.StatusOK, code)
	p1 := decode[struct {
		OpenItems  []OpenItem `json:"open_items"`
		NextCursor string     `json:"next_cursor"`
	}](t, body)
	require.Len(t, p1.OpenItems, 2)
	require.NotEmpty(t, p1.NextCursor)
}

// ---- HTTP command flow ----

func TestCommands_FlagsVersionsAndClose(t *testing.T) {
	e := newEnv(t)
	in := doc(e.party, "100.00", "USD", "2026-10-01", "2026-10-31")
	create := map[string]any{"document_kind": "INVOICE", "source_document_id": in.SourceDocumentID, "document_number": "INV-9", "customer_party_id": e.party,
		"currency": "USD", "original_amount": "100.00", "issue_date": "2026-10-01", "due_date": "2026-10-31"}
	code, body := e.cmd(pManager, "createOpenItemFromDocument", create)
	require.Equal(t, http.StatusCreated, code, string(body))
	o := decode[OpenItem](t, body)
	id := o.OpenItemID
	code, body = e.cmd(pManager, "createOpenItemFromDocument", create)
	require.Equal(t, http.StatusOK, code, string(body))
	require.Equal(t, id, decode[OpenItem](t, body).OpenItemID)
	code, _ = e.cmd(pNobody, "createOpenItemFromDocument", create)
	require.Equal(t, http.StatusForbidden, code)

	// dispute
	dispute := map[string]any{"open_item_id": id, "expected_version": 1, "reason_code": "PRICE", "evidence_id": "EV-1"}
	code, body = e.cmd(pDispute, "openDispute", dispute)
	require.Equal(t, http.StatusOK, code, string(body))
	o = decode[OpenItem](t, body)
	require.True(t, o.Disputed)
	require.Equal(t, 2, o.Version)
	require.Equal(t, StateOpen, o.State, "the dispute flag is orthogonal to the state")
	require.Equal(t, 1, e.events(id, EvDisputed))
	code, body = e.cmd(pDispute, "openDispute", dispute) // stale
	require.Equal(t, http.StatusConflict, code)
	require.Equal(t, kit.CodeStaleVersion, errCode(t, body))
	dispute["expected_version"] = 2
	code, body = e.cmd(pDispute, "openDispute", dispute) // already disputed
	require.Equal(t, kit.CodeInvalidTransition, errCode(t, body), string(body))
	code, _ = e.cmd(pHold, "openDispute", dispute)
	require.Equal(t, http.StatusForbidden, code)
	// settlement continues while disputed; close is refused until the dispute is resolved and the residual is zero
	e.mustAdjust(id, kit.AdjCashApplication, "100.00")
	code, body = e.cmd(pManager, "closeOpenItem", map[string]any{"open_item_id": id, "expected_version": 3})
	require.Equal(t, http.StatusConflict, code, string(body))
	code, body = e.cmd("resolver", "resolveDispute", map[string]any{"open_item_id": id, "expected_version": 3, "resolution_code": "UPHELD", "evidence_id": "EV-2"})
	require.Equal(t, http.StatusOK, code, string(body))
	require.False(t, decode[OpenItem](t, body).Disputed)
	code, body = e.cmd("resolver", "resolveDispute", map[string]any{"open_item_id": id, "expected_version": 4, "resolution_code": "UPHELD", "evidence_id": "EV-2"})
	require.Equal(t, kit.CodeInvalidTransition, errCode(t, body), string(body))

	// a settled item cannot be disputed or held
	code, body = e.cmd(pHold, "placeCollectionHold", map[string]any{"open_item_id": id, "expected_version": 4, "reason_code": "R"})
	require.Equal(t, http.StatusConflict, code, string(body))

	// close: residual must be zero; version is mandatory
	code, body = e.cmd(pManager, "closeOpenItem", map[string]any{"open_item_id": id})
	require.Equal(t, kit.CodeExpectedVersionMissing, errCode(t, body))
	code, body = e.cmd(pManager, "closeOpenItem", map[string]any{"open_item_id": id, "expected_version": 4})
	require.Equal(t, http.StatusOK, code, string(body))
	require.NotNil(t, decode[OpenItem](t, body).ClosedAt)
	require.Equal(t, 1, e.events(id, EvClosed))
	_, err := e.adjust(pBoss, id, kit.AdjCashUnapply, "10.00", "after-close")
	require.Equal(t, kit.CodeInvalidTransition, problemCode(t, err))
	// SQL: a closed item is frozen
	_, err = pool.Exec(context.Background(), `UPDATE ar07_open_items SET closed_at = NULL WHERE open_item_id = $1`, id)
	require.ErrorContains(t, err, "AR07_OPEN_ITEM_CLOSED")

	// hold + a positive residual cannot be closed
	o2, _ := e.create("60.00", "USD", "2026-10-01", "2026-10-31")
	code, body = e.cmd(pHold, "placeCollectionHold", map[string]any{"open_item_id": o2.OpenItemID, "expected_version": 1, "reason_code": "PAYMENT_PLAN"})
	require.Equal(t, http.StatusOK, code, string(body))
	require.True(t, decode[OpenItem](t, body).CollectionHold)
	blocked, err := e.m.CollectionBlocked(context.Background(), e.tenant, e.entity, e.party)
	require.NoError(t, err)
	require.True(t, blocked)
	other, err := e.m.CollectionBlocked(context.Background(), e.tenant, e.entity, uuid.NewString())
	require.NoError(t, err)
	require.False(t, other)
	code, _ = e.cmd(pHold, "placeCollectionHold", map[string]any{"open_item_id": o2.OpenItemID, "expected_version": 2, "reason_code": "R"})
	require.Equal(t, http.StatusConflict, code)
	code, body = e.cmd(pManager, "closeOpenItem", map[string]any{"open_item_id": o2.OpenItemID, "expected_version": 2})
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	require.Equal(t, kit.CodeReceivableResidualInvalid, errCode(t, body))
	code, body = e.cmd(pHold, "releaseCollectionHold", map[string]any{"open_item_id": o2.OpenItemID, "expected_version": 2, "reason_code": "PAID_PLAN"})
	require.Equal(t, http.StatusOK, code, string(body))
	blocked, _ = e.m.CollectionBlocked(context.Background(), e.tenant, e.entity, e.party)
	require.False(t, blocked)
	require.Equal(t, 1, e.events(o2.OpenItemID, EvHoldPlaced))
	require.Equal(t, 1, e.events(o2.OpenItemID, EvHoldReleased))

	// history and available actions
	code, body = e.query("getOpenItemHistory?open_item_id=" + o2.OpenItemID)
	require.Equal(t, http.StatusOK, code, string(body))
	h := decode[struct {
		History []HistoryEvent `json:"history"`
	}](t, body)
	require.Len(t, h.History, 3) // CREATED, HOLD_PLACED, HOLD_RELEASED
	require.Equal(t, "HOLD_PLACED", h.History[1].Event)

	acts := func(principal, itemID string) map[string]bool {
		code, body := call(t, e.h, http.MethodGet, base+"getAvailableActions?open_item_id="+itemID, e.tenant, principal, "", nil)
		require.Equal(t, http.StatusOK, code, string(body))
		out := map[string]bool{}
		for _, a := range decode[struct {
			Actions []actionInfo `json:"actions"`
		}](t, body).Actions {
			out[a.Action] = a.Available
		}
		return out
	}
	live := acts(pBoss, o2.OpenItemID)
	require.True(t, live["OpenDispute"] && live["PlaceCollectionHold"] && live["ApplyReceivableAdjustment"])
	require.False(t, live["ResolveDispute"] || live["ReleaseCollectionHold"] || live["CloseOpenItem"])
	require.False(t, acts(pNobody, o2.OpenItemID)["OpenDispute"], "derived from permissions, not hard-coded")
	done := acts(pBoss, id)
	for k, v := range done {
		require.False(t, v, k)
	}
}

// ---- segregation of duties ----

func TestSoD_DisputeResolverCannotApproveWriteOff(t *testing.T) {
	e := newEnv(t)
	ref, _ := e.create("100.00", "USD", "2026-10-01", "2026-10-31")
	id := ref.OpenItemID
	ctx := context.Background()
	op, rs, err := e.m.DisputeParticipants(ctx, e.tenant, id)
	require.NoError(t, err)
	require.Empty(t, op+rs)

	code, body := e.cmd(pDispute, "openDispute", map[string]any{"open_item_id": id, "expected_version": 1, "reason_code": "R", "evidence_id": "E"})
	require.Equal(t, http.StatusOK, code, string(body))
	op, rs, _ = e.m.DisputeParticipants(ctx, e.tenant, id)
	require.Equal(t, pDispute, op)
	require.Empty(t, rs)
	code, body = e.cmd("resolver", "resolveDispute", map[string]any{"open_item_id": id, "expected_version": 2, "resolution_code": "UPHELD", "evidence_id": "E2"})
	require.Equal(t, http.StatusOK, code, string(body))
	op, rs, _ = e.m.DisputeParticipants(ctx, e.tenant, id)
	require.Equal(t, pDispute, op)
	require.Equal(t, "resolver", rs)

	_, err = e.adjust("resolver", id, kit.AdjWriteOff, "100.00", "wo-1")
	require.Equal(t, kit.CodeSegregationOfDuties, problemCode(t, err))
	require.Equal(t, 0, e.ledgerRows(id))
	v, err := e.adjust("approver-2", id, kit.AdjWriteOff, "100.00", "wo-2")
	require.NoError(t, err)
	require.Equal(t, StateWrittenOff, v.State)
	// a non-write-off adjustment by the resolver is not restricted
	ref2, _ := e.create("10.00", "USD", "2026-10-01", "2026-10-31")
	_, err = e.adjust("resolver", ref2.OpenItemID, kit.AdjCashApplication, "1.00", "c-1")
	require.NoError(t, err)
}

// ---- tenant isolation ----

func TestTenantIsolation(t *testing.T) {
	a, b := newEnv(t), newEnv(t)
	ref, _ := a.create("100.00", "USD", "2026-10-01", "2026-10-31")
	id := ref.OpenItemID
	ctx := context.Background()

	_, err := b.m.GetByID(ctx, b.tenant, id)
	require.Equal(t, kit.CodeNotFound, problemCode(t, err))
	_, err = b.adjust(pBoss, id, kit.AdjCashApplication, "10.00", "x-tenant")
	require.Equal(t, kit.CodeNotFound, problemCode(t, err))
	code, body := b.query("getOpenItem?open_item_id=" + id)
	require.Equal(t, http.StatusNotFound, code, string(body))
	code, _ = b.query("getOpenItemHistory?open_item_id=" + id)
	require.Equal(t, http.StatusNotFound, code)
	code, _ = b.query("getBalanceAsOf?as_of=2026-12-01&open_item_id=" + id)
	require.Equal(t, http.StatusNotFound, code)
	code, body = b.cmd(pDispute, "openDispute", map[string]any{"open_item_id": id, "expected_version": 1, "reason_code": "R", "evidence_id": "E"})
	require.Equal(t, http.StatusNotFound, code, string(body))
	// the same tenant but a different legal entity in the body never reveals the item
	code, _ = a.cmd(pDispute, "openDispute", map[string]any{"legal_entity_id": uuid.NewString(), "open_item_id": id, "expected_version": 1, "reason_code": "R", "evidence_id": "E"})
	require.Equal(t, http.StatusNotFound, code)
	exp, _ := b.m.Exposure(ctx, b.tenant, a.entity, a.party, "USD")
	require.True(t, exp.IsZero())
	list, _ := b.m.ListOpenForCustomer(ctx, b.tenant, a.entity, a.party, "")
	require.Empty(t, list)
	blocked, _ := b.m.CollectionBlocked(ctx, b.tenant, a.entity, a.party)
	require.False(t, blocked)
	_, rs, err := b.m.DisputeParticipants(ctx, b.tenant, id)
	require.NoError(t, err)
	require.Empty(t, rs)
	pop, err := b.m.ControlPopulation(ctx, domain.ControlPopulationQuery{TenantID: b.tenant, LegalEntityID: a.entity, Limit: 10})
	require.NoError(t, err)
	require.Empty(t, pop.Records)
	require.Equal(t, "100.0000", a.item(id).Residual.String())
	// a foreign adjustment cannot reuse a tenant's event key to collide with it
	_, err = b.adjust(pBoss, ref.OpenItemID, kit.AdjCashApplication, "10.00", "k")
	require.Error(t, err)
}

// ---- control population ----

func TestControlPopulation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var ids []string
	for i := 0; i < 5; i++ {
		ref, _ := e.create(fmt.Sprintf("%d00.00", i+1), "USD", "2026-10-01", "2026-10-31")
		ids = append(ids, ref.OpenItemID)
	}
	e.create("9.00", "EUR", "2026-10-01", "2026-10-31")
	// a different entity of the same tenant is not in scope
	oe := *e
	oe.entity = uuid.NewString()
	oe.create("1000.00", "USD", "2026-10-01", "2026-10-31")

	e.clock.Set(time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC))
	e.mustAdjust(ids[0], kit.AdjCashApplication, "100.00") // settled: out of the population
	e.mustAdjust(ids[1], kit.AdjCashApplication, "50.00")  // partial: 150

	page1, err := e.m.ControlPopulation(ctx, domain.ControlPopulationQuery{TenantID: e.tenant, LegalEntityID: e.entity, Limit: 2})
	require.NoError(t, err)
	require.Len(t, page1.Records, 2)
	require.NotEmpty(t, page1.NextCursor)
	require.Equal(t, int64(5), page1.DeclaredTotals.RowCount) // 4 USD + 1 EUR
	require.Equal(t, "1350.0000", page1.DeclaredTotals.Totals["USD"])
	require.Equal(t, "9.0000", page1.DeclaredTotals.Totals["EUR"])
	var all []domain.ControlRecord
	all = append(all, page1.Records...)
	cursor := page1.NextCursor
	for cursor != "" {
		p, err := e.m.ControlPopulation(ctx, domain.ControlPopulationQuery{TenantID: e.tenant, LegalEntityID: e.entity, Limit: 2, AfterRecordID: cursor})
		require.NoError(t, err)
		require.Equal(t, page1.Watermark, p.Watermark, "the watermark is identical on every page")
		require.Equal(t, page1.DeclaredTotals, p.DeclaredTotals)
		all = append(all, p.Records...)
		cursor = p.NextCursor
	}
	require.Len(t, all, 5)
	seen := map[string]domain.ControlRecord{}
	for i, r := range all {
		seen[r.RecordID] = r
		if i > 0 {
			require.Less(t, all[i-1].RecordID, r.RecordID, "keyset order")
		}
	}
	require.NotContains(t, seen, ids[0])
	require.Equal(t, "150.0000", seen[ids[1]].Amount)
	require.Equal(t, "PARTIALLY_SETTLED", seen[ids[1]].Attributes["status"])
	require.Equal(t, "200.0000", seen[ids[1]].Attributes["invoice_amount"])

	// cut-off before the payments: the settled item is back at its full amount, and the watermark differs
	cut := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	before, err := e.m.ControlPopulation(ctx, domain.ControlPopulationQuery{TenantID: e.tenant, LegalEntityID: e.entity, CutOff: &cut, Limit: 50})
	require.NoError(t, err)
	require.Equal(t, int64(6), before.DeclaredTotals.RowCount)
	require.Equal(t, "1500.0000", before.DeclaredTotals.Totals["USD"])
	require.NotEqual(t, page1.Watermark, before.Watermark)
	// a data change moves the watermark
	e.mustAdjust(ids[2], kit.AdjCashApplication, "1.00")
	after, _ := e.m.ControlPopulation(ctx, domain.ControlPopulationQuery{TenantID: e.tenant, LegalEntityID: e.entity, Limit: 2})
	require.NotEqual(t, page1.Watermark, after.Watermark)
}
