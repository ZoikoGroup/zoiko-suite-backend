//go:build integration

// Real-Postgres tests for AR-03. Run (serially):
//
//	go test -tags=integration -count=1 -p 1 -timeout=600s ./internal/ar03/...
//
// The embedded superuser bypasses RLS, so isolation tests prove the explicit tenant_id
// predicates, not the policies.
package ar03

import (
	"context"
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

	"zoiko.io/accounts-receivable-svc/internal/ar02"
	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/kit/kittest"
)

var pool *pgxpool.Pool

func TestMain(m *testing.M) {
	var stop func()
	pool, stop = kittest.Start("ar03_test", 17550)
	code := m.Run()
	stop()
	os.Exit(code)
}

type env struct {
	t      *testing.T
	m      *Module
	h      http.Handler
	tenant string
	entity string
	prof   *fakeProfiles
	items  *fakeOpenItems
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, tenant: uuid.NewString(), entity: uuid.NewString(), prof: &fakeProfiles{}, items: &fakeOpenItems{}}
	deps := testDeps(&fakeAuthz{grants: standardGrants()})
	deps.DB = kit.NewDB(pool)
	e.m = New(deps, e.prof, nil) // AR-07 is not wired by default
	e.h = testRouter(e.m)
	return e
}

func (e *env) cmd(principal, name string, body map[string]any) (int, []byte) {
	code, b, _ := call(e.t, e.h, http.MethodPost, base+"commands/"+name, e.tenant, principal, "", body)
	return code, b
}

func (e *env) query(principal, name, qs string) (int, []byte) {
	code, b, _ := call(e.t, e.h, http.MethodGet, base+name+"?"+qs, e.tenant, principal, "", nil)
	return code, b
}

func (e *env) ok(code int, body []byte) *Order {
	e.t.Helper()
	require.Less(e.t, code, 300, string(body))
	return asOrder(e.t, body)
}

// create makes a DRAFT order (default: 2 x 500.00 + 1 x 250.50 = 1250.50 USD, no discount).
func (e *env) create(principal string, extra map[string]any) *Order {
	e.t.Helper()
	b := createBody(e.entity, uuid.NewString())
	for k, v := range extra {
		b[k] = v
	}
	code, body := e.cmd(principal, "createSalesOrder", b)
	require.Equal(e.t, http.StatusCreated, code, string(body))
	return asOrder(e.t, body)
}

func obody(o *Order, extra map[string]any) map[string]any {
	b := map[string]any{"order_id": o.OrderID, "expected_version": o.Version}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

func (e *env) mk(principal, name string, o *Order, extra map[string]any) (int, []byte) {
	return e.cmd(principal, name, obody(o, extra))
}

func (e *env) step(principal, name string, o *Order, extra map[string]any) *Order {
	e.t.Helper()
	code, body := e.mk(principal, name, o, extra)
	return e.ok(code, body)
}

func (e *env) get(id string) *Order {
	e.t.Helper()
	code, body := e.query(pNobody, "getSalesOrder", "order_id="+id)
	return e.ok(code, body)
}

func (e *env) submit(o *Order) *Order { return e.step(pCreator, "submitOrder", o, nil) }
func (e *env) approveAs(principal string, o *Order, extra map[string]any) *Order {
	b := map[string]any{"version_no": o.CurrentVersionNo}
	for k, v := range extra {
		b[k] = v
	}
	return e.step(principal, "approveOrder", o, b)
}
func (e *env) approved(extra map[string]any) *Order {
	return e.approveAs(pApprover, e.submit(e.create(pCreator, extra)), nil)
}
func (e *env) confirmed(extra map[string]any) *Order {
	return e.step(pConfirmer, "confirmOrder", e.approved(extra), nil)
}

func msBody(ref string, line int, qty, amt string) map[string]any {
	return map[string]any{"external_ref": ref, "line_no": line, "scheduled_qty": qty, "scheduled_amount": amt,
		"fulfillment_source": "LOGISTICS", "fulfillment_evidence_ref": "SHIP-" + ref}
}

// milestone records a billable milestone and returns the order and the milestone id.
func (e *env) milestone(o *Order, ref string, line int, qty, amt string) (*Order, string) {
	e.t.Helper()
	n := e.step(pCreator, "recordBillableMilestone", o, msBody(ref, line, qty, amt))
	for _, m := range n.Milestones {
		if m.ExternalRef == ref {
			return n, m.MilestoneID
		}
	}
	e.t.Fatalf("milestone %s not returned", ref)
	return nil, ""
}

func (e *env) caller() kit.Caller {
	return kit.Caller{TenantID: e.tenant, PrincipalID: "ar04-issuer", LegalEntityID: e.entity, CorrelationID: "corr-" + uuid.NewString()[:8]}
}

// invoice calls the kit.Orders port in its own transaction (what AR-04 does inside issuance).
func (e *env) invoice(orderID, msID, invoiceID string, net string) error {
	return e.m.deps.DB.WithTx(context.Background(), e.tenant, func(tx pgx.Tx) error {
		return e.m.RecordInvoiced(context.Background(), tx, e.caller(), orderID, msID, invoiceID, kit.MustAmount(net))
	})
}

func problemCode(t *testing.T, err error) string {
	t.Helper()
	var p *kit.Problem
	require.ErrorAs(t, err, &p, "%v", err)
	return p.Code
}

func (e *env) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	require.NoError(e.t, pool.QueryRow(context.Background(), q, args...).Scan(&n))
	return n
}

func (e *env) events(orderID, eventType string) int {
	return e.count(`SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND aggregate_id = $2 AND event_type = $3`, e.tenant, orderID, eventType)
}

func (e *env) audits(orderID string) int {
	return e.count(`SELECT count(*) FROM ar_audit_events WHERE tenant_id = $1 AND aggregate_id = $2`, e.tenant, orderID)
}

func (e *env) denials(orderID, command string) int {
	return e.count(`SELECT count(*) FROM ar_audit_events WHERE tenant_id = $1 AND aggregate_id = $2 AND command = $3`, e.tenant, orderID, command+".denied")
}

func (e *env) invoiced(orderID string, line int) (qty, amt string) {
	require.NoError(e.t, pool.QueryRow(context.Background(), `SELECT invoiced_qty::text, invoiced_amount::text FROM ar03_order_lines
		WHERE tenant_id = $1 AND order_id = $2 AND line_no = $3`, e.tenant, orderID, line).Scan(&qty, &amt))
	return
}

func line(no int, desc, qty, price string) map[string]any {
	return map[string]any{"line_no": no, "description": desc, "quantity": qty, "unit_price": price}
}

// ── lifecycle, evidence, events, commercial-source-only ──────────────────────

func TestLifecycle_EndToEnd(t *testing.T) {
	e := newEnv(t)
	o := e.create(pCreator, map[string]any{"reason_code": "NEW_ORDER", "evidence_id": "PO-DOC-1"})
	require.Equal(t, StateDraft, o.State)
	require.Equal(t, 1, o.Version)
	require.Equal(t, "SO-000001", o.OrderNumber)
	require.Equal(t, pCreator, o.CreatedBy)
	require.Equal(t, kit.MustAmount("1250.50"), o.NetTotal)
	require.Len(t, o.Lines, 2)
	require.Equal(t, kit.MustAmount("1000"), o.Lines[0].LineNet)
	require.Equal(t, kit.MustAmount("1000"), o.Lines[0].RemainingAmount)
	require.Equal(t, kit.MustAmount("2"), o.Lines[0].RemainingQty)
	require.Zero(t, o.Lines[0].InvoicedAmount)
	require.Equal(t, "PO-7781", o.CustomerPORef)
	require.Len(t, o.Current.Hash, 64)
	require.Equal(t, 1, e.events(o.OrderID, EventSalesOrderCreated))
	require.Equal(t, "SO-000002", e.create(pCreator, nil).OrderNumber)

	code, body := e.mk(pConfirmer, "confirmOrder", o, nil)
	require.Equal(t, http.StatusConflict, code, string(body))
	require.Equal(t, kit.CodeInvalidTransition, errCode(t, body))
	code, body = e.mk(pBoss, "approveOrder", o, map[string]any{"version_no": 1})
	require.Equal(t, http.StatusConflict, code, "a draft cannot be approved before submission: "+string(body))

	o = e.submit(o)
	require.Equal(t, StatePendingApproval, o.State)
	require.Equal(t, pCreator, o.SubmittedBy)
	o = e.approveAs(pApprover, o, nil)
	require.Equal(t, StateApproved, o.State)
	require.Equal(t, pApprover, o.ApprovedBy)
	require.Equal(t, o.Current.Hash, o.ApprovedHash)
	o = e.step(pConfirmer, "confirmOrder", o, nil)
	require.Equal(t, StateConfirmed, o.State)
	require.Equal(t, pConfirmer, o.ConfirmedBy)
	require.Equal(t, 1, e.events(o.OrderID, EventSalesOrderConfirmed))

	o, a := e.milestone(o, "FUL-A", 1, "1", "500")
	require.Equal(t, StateInFulfillment, o.State)
	o, b := e.milestone(o, "FUL-B", 1, "1", "500")
	o, c := e.milestone(o, "FUL-C", 2, "1", "250.50")
	require.Len(t, o.Milestones, 3)
	require.Equal(t, 3, e.events(o.OrderID, EventBillableMilestoneRecorded))
	require.Equal(t, "LOGISTICS", o.Milestones[0].FulfillmentSource)
	require.Equal(t, "SHIP-FUL-A", o.Milestones[0].FulfillmentEvidenceRef)

	code, body = e.query(pNobody, "getInvoiceEligibility", "order_id="+o.OrderID)
	require.Equal(t, http.StatusOK, code, string(body))
	var el struct {
		Items []eligibilityItem `json:"items"`
	}
	require.NoError(t, jsonDecode(body, &el))
	require.Len(t, el.Items, 3)
	for _, it := range el.Items {
		require.True(t, it.Eligible, it.Reason)
	}
	basis, err := e.m.InvoiceEligibility(context.Background(), e.tenant, o.OrderID, a)
	require.NoError(t, err)
	require.Equal(t, kit.MustAmount("500"), basis.EligibleNet)
	require.Equal(t, "USD", basis.Currency)
	require.Equal(t, o.CustomerPartyID, basis.CustomerParty)
	require.Equal(t, a, basis.MilestoneID)

	require.NoError(t, e.invoice(o.OrderID, a, "INV-1", "500"))
	g := e.get(o.OrderID)
	require.Equal(t, StatePartiallyBilled, g.State)
	require.Equal(t, kit.MustAmount("500"), g.Lines[0].InvoicedAmount)
	require.Equal(t, kit.MustAmount("1"), g.Lines[0].InvoicedQty)
	require.Equal(t, kit.MustAmount("500"), g.Lines[0].RemainingAmount)
	require.Equal(t, kit.MustAmount("500"), g.Billing.InvoicedNet)
	require.Equal(t, kit.MustAmount("750.50"), g.Billing.RemainingNet)
	require.Equal(t, 1, e.events(o.OrderID, EventSalesOrderPartiallyBilled))
	require.NoError(t, e.invoice(o.OrderID, b, "INV-2", "500"))
	require.Equal(t, 1, e.events(o.OrderID, EventSalesOrderPartiallyBilled), "the threshold is crossed once")
	require.Zero(t, e.events(o.OrderID, EventSalesOrderFullyBilled))
	require.NoError(t, e.invoice(o.OrderID, c, "INV-3", "250.50"))
	g = e.get(o.OrderID)
	require.Equal(t, StateFullyBilled, g.State)
	require.Zero(t, g.Billing.RemainingNet)
	require.True(t, g.Billing.FullyBilled)
	require.Equal(t, 1, e.events(o.OrderID, EventSalesOrderFullyBilled))
	code, body = e.query(pNobody, "getOrderBillingStatus", "order_id="+o.OrderID)
	require.Equal(t, http.StatusOK, code, string(body))
	require.Contains(t, string(body), `"unscheduled_net":"0.0000"`)

	g = e.step(pCreator, "closeOrder", g, nil)
	require.Equal(t, StateClosed, g.State)
	require.NotNil(t, g.ClosedAt)

	// Evidence: a history row and an audit row for every command, versions strictly sequential.
	code, body = e.query(pNobody, "getOrderHistory", "order_id="+o.OrderID)
	require.Equal(t, http.StatusOK, code, string(body))
	var hs struct {
		Items     []HistoryEntry   `json:"items"`
		Versions  []VersionSummary `json:"versions"`
		Approvals []ApprovalRecord `json:"approvals"`
	}
	require.NoError(t, jsonDecode(body, &hs))
	cmds := []string{"createSalesOrder", "submitOrder", "approveOrder", "confirmOrder", "recordBillableMilestone", "recordBillableMilestone",
		"recordBillableMilestone", "recordInvoiced", "recordInvoiced", "recordInvoiced", "closeOrder"}
	require.Len(t, hs.Items, len(cmds))
	for i, it := range hs.Items {
		require.Equal(t, cmds[i], it.Command)
		require.Equal(t, i+1, it.OrderVersion)
	}
	require.Equal(t, "PO-DOC-1", hs.Items[0].EvidenceID)
	require.Equal(t, pApprover, hs.Items[2].ApproverID)
	require.Equal(t, StateClosed, hs.Items[10].StateAfter)
	require.Len(t, hs.Versions, 1)
	require.Len(t, hs.Approvals, 1)
	require.Equal(t, len(cmds), e.audits(o.OrderID))

	// Commercial source only: no posting request and nothing but ar.sales_order.* events leaves AR-03.
	require.Zero(t, e.count(`SELECT count(*) FROM ar_accounting_posting_requests WHERE tenant_id = $1`, e.tenant))
	require.Zero(t, e.count(`SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type NOT LIKE 'ar.sales_order.%'`, e.tenant))
	require.Equal(t, g.Version, e.get(o.OrderID).Version)
}

// ── §18 #3 / spec table #1: credit control ───────────────────────────────────

func TestNegAR03_1_OrderExceedsCreditLimitWithoutApproval(t *testing.T) {
	e := newEnv(t)
	e.prof.set(false, kit.MustAmount("1000"), nil) // limit 1000 USD; the order is 1250.50
	o := e.approved(nil)
	require.Equal(t, StateApproved, o.State)

	// AR-07 not wired: only the order's own value is compared and exposure is reported unavailable.
	code, body := e.mk(pConfirmer, "confirmOrder", o, nil)
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	require.Equal(t, kit.CodeCreditHoldActive, errCode(t, body))
	require.Contains(t, string(body), "existing exposure is unavailable and was not assumed")
	after := e.get(o.OrderID)
	require.Equal(t, StateApproved, after.State)
	require.Equal(t, o.Version, after.Version, "the refusal preserves the safe state")
	require.Zero(t, e.events(o.OrderID, EventSalesOrderConfirmed))
	require.Equal(t, 1, e.denials(o.OrderID, "confirmOrder"), "the refused confirmation leaves evidence")
	var avail string
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT detail->'credit_check'->>'exposure_available' FROM ar_audit_events
		WHERE aggregate_id = $1 AND command = 'confirmOrder.denied'`, o.OrderID).Scan(&avail))
	require.Equal(t, "false", avail)

	// AR-07 wired: existing exposure + this order against the limit.
	e.m.openItems = e.items
	e.prof.set(false, kit.MustAmount("2000"), nil)
	e.items.exposure = kit.MustAmount("900")
	code, body = e.mk(pConfirmer, "confirmOrder", o, nil)
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	require.Equal(t, kit.CodeCreditHoldActive, errCode(t, body))
	require.Contains(t, string(body), "exposure 900.0000 plus this order 1250.5000 exceeds the credit limit 2000.0000")
	e.items.exposure = kit.MustAmount("700")
	confirmed := e.step(pConfirmer, "confirmOrder", o, nil)
	require.Equal(t, StateConfirmed, confirmed.State)
	var credit string
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT new_values->'credit_check'->>'exposure' FROM ar03_order_history
		WHERE order_id = $1 AND command = 'confirmOrder'`, o.OrderID).Scan(&credit))
	require.Equal(t, "700.0000", credit, "the evidence records the exposure that was compared")

	// Exposure unavailable (AR-07 down) fails closed.
	e.items.err = errors.New("ar07 down")
	p := e.approved(nil)
	code, body = e.mk(pConfirmer, "confirmOrder", p, nil)
	require.Equal(t, http.StatusServiceUnavailable, code, string(body))
	e.items.err = nil
	e.m.openItems = nil

	// An approved exception from a DIFFERENT principal holding the credit authority lets it through.
	e.prof.set(false, kit.MustAmount("1000"), nil)
	b := e.approved(map[string]any{}) // created and submitted by pCreator
	code, body = e.mk(pConfirmer, "confirmOrder", b, map[string]any{"credit_exception": map[string]any{"reason_code": "KEY_ACCOUNT", "evidence_id": "EV-1"}})
	require.Equal(t, http.StatusForbidden, code, "the confirmer holds no credit-exception authority: "+string(body))
	require.Equal(t, kit.CodeForbidden, errCode(t, body))
	code, body = e.mk(pCreditAuth, "confirmOrder", b, map[string]any{"credit_exception": map[string]any{"reason_code": "KEY_ACCOUNT"}})
	require.Equal(t, http.StatusBadRequest, code, string(body))
	require.Equal(t, kit.CodeEvidenceRequired, errCode(t, body))
	code, body = e.mk(pCreditAuth, "confirmOrder", b, map[string]any{"credit_exception": map[string]any{"evidence_id": "EV-1"}})
	require.Equal(t, http.StatusBadRequest, code, string(body))
	require.Equal(t, kit.CodeReasonRequired, errCode(t, body))
	require.Equal(t, StateApproved, e.get(b.OrderID).State)
	b = e.step(pCreditAuth, "confirmOrder", b, map[string]any{"credit_exception": map[string]any{"reason_code": "KEY_ACCOUNT", "evidence_id": "EV-1"}})
	require.Equal(t, StateConfirmed, b.State)
	require.NotEmpty(t, b.CreditExceptionID)
	var by, reason, evid string
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT approved_by, reason_code, evidence_id FROM ar03_credit_exceptions WHERE exception_id = $1`,
		b.CreditExceptionID).Scan(&by, &reason, &evid))
	require.Equal(t, []string{pCreditAuth, "KEY_ACCOUNT", "EV-1"}, []string{by, reason, evid})
	require.Equal(t, 1, e.events(b.OrderID, EventSalesOrderConfirmed))

	// The exception cannot be recorded by the order's own creator/submitter, even holding every authority.
	c := e.step(pBoss, "submitOrder", e.create(pBoss, nil), nil)
	c = e.approveAs(pApprover, c, nil)
	code, body = e.mk(pBoss, "confirmOrder", c, map[string]any{"credit_exception": map[string]any{"reason_code": "SELF", "evidence_id": "EV-2"}})
	require.Equal(t, http.StatusForbidden, code, string(body))
	require.Equal(t, kit.CodeSegregationOfDuties, errCode(t, body))
	require.Equal(t, 1, e.denials(c.OrderID, "confirmOrder"))
	require.Equal(t, StateApproved, e.get(c.OrderID).State)

	// Recorded at approval: the exception travels with the approved order, and confirmation then needs none.
	d := e.submit(e.create(pCreator, nil))
	code, body = e.mk(pApprover, "approveOrder", d, map[string]any{"version_no": 1, "credit_exception": map[string]any{"reason_code": "R", "evidence_id": "E"}})
	require.Equal(t, http.StatusForbidden, code, "approve alone is not the credit-exception authority: "+string(body))
	d = e.approveAs(pCreditAuth, d, map[string]any{"credit_exception": map[string]any{"reason_code": "R", "evidence_id": "E"}})
	require.NotEmpty(t, d.CreditExceptionID)
	require.Equal(t, StateConfirmed, e.step(pConfirmer, "confirmOrder", d, nil).State)

	// A change after the exception voids it: the exception was for the old content.
	f := e.submit(e.create(pCreator, nil))
	f = e.approveAs(pCreditAuth, f, map[string]any{"credit_exception": map[string]any{"reason_code": "R", "evidence_id": "E"}})
	f = e.step(pCreator, "amendSalesOrder", f, map[string]any{"terms": "Net 45"})
	require.Equal(t, StateDraft, f.State)
	require.Empty(t, f.CreditExceptionID)
	f = e.approveAs(pApprover, e.submit(f), nil)
	code, body = e.mk(pConfirmer, "confirmOrder", f, nil)
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	require.Equal(t, kit.CodeCreditHoldActive, errCode(t, body))

	// No limit configured and no hold: nothing blocks.
	e.prof.set(false, 0, nil)
	require.Equal(t, StateConfirmed, e.confirmed(nil).State)
}

func TestNeg03_CreditHoldOrderConfirmedWithoutApprovedException(t *testing.T) {
	e := newEnv(t)
	e.prof.set(true, 0, nil) // the customer is on credit hold; drafting and approval are still allowed
	o := e.approved(nil)

	code, body := e.mk(pConfirmer, "confirmOrder", o, nil)
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	require.Equal(t, kit.CodeCreditHoldActive, errCode(t, body))
	require.Equal(t, StateApproved, e.get(o.OrderID).State)
	require.Zero(t, e.events(o.OrderID, EventSalesOrderConfirmed))
	require.Equal(t, 1, e.denials(o.OrderID, "confirmOrder"))

	// An exception from the same principal who created it is refused; from an independent holder it is accepted.
	code, body = e.mk(pCreditAuth, "confirmOrder", o, map[string]any{"credit_exception": map[string]any{"reason_code": "PREPAID", "evidence_id": "PAY-1"}})
	require.Equal(t, http.StatusOK, code, string(body))
	require.Equal(t, StateConfirmed, asOrder(t, body).State)
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM ar03_credit_exceptions WHERE order_id = $1`, o.OrderID).Scan(&n))
	require.Equal(t, 1, n)

	// An order-level CREDIT hold blocks confirmation too, with the same stable class.
	e.prof.set(false, 0, nil)
	p := e.approved(nil)
	p = e.step(pHolder, "placeOrderHold", p, map[string]any{"hold_kind": HoldCredit, "reason_code": "OVERDUE", "evidence_id": "AR-9"})
	require.True(t, p.HoldActive)
	code, body = e.mk(pConfirmer, "confirmOrder", p, nil)
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	require.Equal(t, kit.CodeCreditHoldActive, errCode(t, body))
	q := e.approved(nil)
	q = e.step(pHolder, "placeOrderHold", q, map[string]any{"hold_kind": HoldCommercial, "reason_code": "R", "evidence_id": "E"})
	code, body = e.mk(pConfirmer, "confirmOrder", q, nil)
	require.Equal(t, http.StatusConflict, code, "a non-credit hold blocks confirmation as a state conflict: "+string(body))
}

// ── spec table #2: a billed line cannot be reduced in place ──────────────────

func TestNegAR03_2_BilledLineQuantityReducedInPlace(t *testing.T) {
	e := newEnv(t)
	o := e.confirmed(nil)
	o, a := e.milestone(o, "FUL-A", 1, "1", "500")
	require.NoError(t, e.invoice(o.OrderID, a, "INV-1", "500"))
	o = e.get(o.OrderID)
	require.Equal(t, StatePartiallyBilled, o.State)
	ctx := context.Background()

	// Through the API: reducing below what was invoiced is refused; the governed amendment needs reason and evidence.
	reduce := func(qty, price string) map[string]any {
		return map[string]any{"lines": []map[string]any{line(1, "Platform licence", qty, price), line(2, "Onboarding", "1", "250.50")},
			"reason_code": "CUSTOMER_REQUEST", "evidence_id": "CHG-1"}
	}
	code, body := e.mk(pCreator, "amendSalesOrder", o, reduce("0.5", "500.00"))
	require.Equal(t, http.StatusConflict, code, string(body))
	require.Equal(t, kit.CodeImmutable, errCode(t, body))
	code, body = e.mk(pCreator, "amendSalesOrder", o, reduce("2", "400.00"))
	require.Equal(t, http.StatusOK, code, "800 stays above the 500 invoiced: "+string(body))
	o = asOrder(t, body)
	code, body = e.mk(pCreator, "amendSalesOrder", o, reduce("1", "400.00"))
	require.Equal(t, http.StatusConflict, code, "400 is below the 500 invoiced: "+string(body))
	require.Equal(t, kit.CodeImmutable, errCode(t, body))
	code, body = e.mk(pCreator, "amendSalesOrder", o, reduce("3", "400.00"))
	require.Equal(t, http.StatusConflict, code, string(body))
	require.Equal(t, CodeAmendmentNotAllowed, errCode(t, body), "a confirmed order can only be reduced")
	b := reduce("2", "300.00")
	delete(b, "reason_code")
	code, body = e.mk(pCreator, "amendSalesOrder", o, b)
	require.Equal(t, http.StatusBadRequest, code, string(body))
	require.Equal(t, kit.CodeReasonRequired, errCode(t, body))
	b["reason_code"] = "R"
	delete(b, "evidence_id")
	code, body = e.mk(pCreator, "amendSalesOrder", o, b)
	require.Equal(t, http.StatusBadRequest, code, string(body))
	require.Equal(t, kit.CodeEvidenceRequired, errCode(t, body))
	code, body = e.mk(pCreator, "amendSalesOrder", o, map[string]any{"lines": []map[string]any{line(1, "Platform licence", "2", "400.00")},
		"reason_code": "R", "evidence_id": "E"})
	require.Equal(t, http.StatusConflict, code, "a confirmed order keeps its lines: "+string(body))
	require.Equal(t, CodeAmendmentNotAllowed, errCode(t, body))

	// The governed amendment recorded a new version; the billed position and the earlier version are untouched.
	require.Equal(t, 2, o.CurrentVersionNo)
	require.Equal(t, StatePartiallyBilled, o.State)
	require.Equal(t, kit.MustAmount("1050.50"), o.NetTotal)
	qty, amt := e.invoiced(o.OrderID, 1)
	require.Equal(t, []string{"1.0000", "500.0000"}, []string{qty, amt})
	_, body = e.query(pNobody, "getSalesOrder", "order_id="+o.OrderID+"&version_no=1")
	require.Equal(t, kit.MustAmount("1250.50"), asOrder(t, body).Current.NetTotal)
	require.Equal(t, 2, e.count(`SELECT count(*) FROM ar03_order_versions WHERE order_id = $1`, o.OrderID))

	// Recorded-but-unbilled milestones also pin the line.
	o, _ = e.milestone(o, "FUL-B", 1, "1", "300")
	code, body = e.mk(pCreator, "amendSalesOrder", o, reduce("1", "500.00"))
	require.Equal(t, http.StatusConflict, code, string(body))
	require.Contains(t, []string{kit.CodeImmutable, CodeAmendmentNotAllowed}, errCode(t, body))

	// The data refuses the same mutations without the API: no silent in-place change, no reduction, no deletion.
	_, err := pool.Exec(ctx, `UPDATE ar03_order_lines SET ordered_qty = 1, list_net = 500, line_net = 500 WHERE order_id = $1 AND line_no = 1`, o.OrderID)
	require.ErrorContains(t, err, "AR03_LINE_AMENDMENT_REQUIRED")
	err = kit.NewDB(pool).WithTx(ctx, e.tenant, func(tx pgx.Tx) error {
		if err := allowLineWrites(ctx, tx, o.OrderID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE ar03_order_lines SET ordered_qty = 0.5, list_net = 200, line_net = 200 WHERE order_id = $1 AND line_no = 1`, o.OrderID)
		return err
	})
	require.ErrorContains(t, err, "ar03_invoiced_within_ordered", "even a governed write cannot go below invoiced")
	_, err = pool.Exec(ctx, `UPDATE ar03_order_lines SET invoiced_qty = 0 WHERE order_id = $1 AND line_no = 1`, o.OrderID)
	require.ErrorContains(t, err, "AR03_INVOICED_NOT_REDUCIBLE")
	_, err = pool.Exec(ctx, `DELETE FROM ar03_order_lines WHERE order_id = $1 AND line_no = 1`, o.OrderID)
	require.ErrorContains(t, err, "AR03_BILLED_LINE_IMMUTABLE")
	qty, amt = e.invoiced(o.OrderID, 1)
	require.Equal(t, []string{"1.0000", "500.0000"}, []string{qty, amt})
}

// ── §18 #7 / spec table #3: a duplicate milestone cannot create duplicate eligibility ──

func TestNeg07_DuplicateBillableMilestoneCreatesNoDuplicateEligibility(t *testing.T) {
	e := newEnv(t)
	o := e.confirmed(nil)
	o, a := e.milestone(o, "FUL-A", 1, "1", "500")

	code, body := e.mk(pCreator, "recordBillableMilestone", o, msBody("FUL-A", 1, "1", "500"))
	require.Equal(t, http.StatusConflict, code, string(body))
	require.Equal(t, CodeDuplicateMilestone, errCode(t, body))
	code, body = e.mk(pCreator, "recordBillableMilestone", o, msBody("FUL-A", 2, "1", "100")) // same ref, different content
	require.Equal(t, http.StatusConflict, code, string(body))
	require.Equal(t, CodeDuplicateMilestone, errCode(t, body))
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar03_billing_milestones WHERE order_id = $1`, o.OrderID))
	require.Equal(t, 1, e.events(o.OrderID, EventBillableMilestoneRecorded))
	require.Equal(t, 2, e.denials(o.OrderID, "recordBillableMilestone"))
	require.Equal(t, o.Version, e.get(o.OrderID).Version)

	// The data refuses it as well, per (tenant, order); another order may reuse the reference.
	_, err := pool.Exec(context.Background(), `INSERT INTO ar03_billing_milestones (milestone_id, tenant_id, legal_entity_id, order_id, external_ref, line_no,
		scheduled_qty, scheduled_amount, state, fulfillment_source, fulfillment_evidence_ref, recorded_by)
		VALUES ($1,$2,$3,$4,'FUL-A',1,1,10,'RECORDED','SQL','X','sql')`, uuid.NewString(), e.tenant, e.entity, o.OrderID)
	require.ErrorContains(t, err, "uq_ar03_milestone_ref")
	other := e.confirmed(nil)
	e.milestone(other, "FUL-A", 1, "1", "500")

	// Concurrent deliveries of the same reference: exactly one row.
	p := e.confirmed(nil)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			call(t, e.h, http.MethodPost, base+"commands/recordBillableMilestone", e.tenant, pCreator, "", obody(p, msBody("RACE-1", 1, "1", "500")))
		}()
	}
	wg.Wait()
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar03_billing_milestones WHERE order_id = $1`, p.OrderID))

	// One milestone yields one invoice eligibility: after it is billed it is not billable again, and the ledger refuses a second row.
	require.NoError(t, e.invoice(o.OrderID, a, "INV-1", "500"))
	_, err = e.m.InvoiceEligibility(context.Background(), e.tenant, o.OrderID, a)
	require.Equal(t, kit.CodeOrderNotBillable, problemCode(t, err))
	err = e.invoice(o.OrderID, a, "INV-2", "500")
	require.Equal(t, kit.CodeOrderNotBillable, problemCode(t, err))
	_, err = pool.Exec(context.Background(), `INSERT INTO ar03_invoiced_ledger (ledger_id, tenant_id, legal_entity_id, order_id, milestone_id, invoice_id, net, recorded_by)
		VALUES ($1,$2,$3,$4,$5,'INV-9',500,'sql')`, uuid.NewString(), e.tenant, e.entity, o.OrderID, a)
	require.ErrorContains(t, err, "uq_ar03_ledger_milestone")
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar03_invoiced_ledger WHERE milestone_id = $1`, a))
	qty, amt := e.invoiced(o.OrderID, 1)
	require.Equal(t, []string{"1.0000", "500.0000"}, []string{qty, amt})
}

// ── spec table #4: a cancelled order is never invoiced ───────────────────────

func TestNegAR03_4_CancelledOrderInvoicedWithoutExplicitException(t *testing.T) {
	e := newEnv(t)
	o := e.confirmed(nil)
	o, a := e.milestone(o, "FUL-A", 1, "1", "500")
	o, b := e.milestone(o, "FUL-B", 1, "1", "500")
	require.NoError(t, e.invoice(o.OrderID, a, "INV-1", "500"))
	o = e.get(o.OrderID)

	code, body := e.mk(pCanceller, "cancelOrder", o, nil)
	require.Equal(t, http.StatusBadRequest, code, string(body))
	require.Equal(t, kit.CodeReasonRequired, errCode(t, body))
	code, _ = e.mk(pCreator, "cancelOrder", o, map[string]any{"reason_code": "R"})
	require.Equal(t, http.StatusForbidden, code, "cancel is its own permission")
	o = e.step(pCanceller, "cancelOrder", o, map[string]any{"reason_code": "CUSTOMER_WITHDREW", "evidence_id": "MAIL-1"})
	require.Equal(t, StateCancelled, o.State)
	require.Equal(t, "CUSTOMER_WITHDREW", o.CancelReason)
	require.Equal(t, 1, e.events(o.OrderID, EventSalesOrderCancelled))
	for _, m := range o.Milestones {
		if m.MilestoneID == b {
			require.Equal(t, MsVoided, m.State, "unbilled eligibility does not survive the cancellation")
		}
	}

	_, err := e.m.InvoiceEligibility(context.Background(), e.tenant, o.OrderID, b)
	require.Equal(t, kit.CodeOrderNotBillable, problemCode(t, err))
	require.Contains(t, err.Error(), "cancelled")
	err = e.invoice(o.OrderID, b, "INV-2", "500")
	require.Equal(t, kit.CodeOrderNotBillable, problemCode(t, err))
	qty, amt := e.invoiced(o.OrderID, 1)
	require.Equal(t, []string{"1.0000", "500.0000"}, []string{qty, amt})
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar03_invoiced_ledger WHERE order_id = $1`, o.OrderID))
	_, body = e.query(pNobody, "getInvoiceEligibility", "order_id="+o.OrderID+"&milestone_id="+b)
	require.Contains(t, string(body), `"eligible":false`)
	require.Contains(t, string(body), kit.CodeOrderNotBillable)

	// A cancelled order is frozen: no command and no write reaches it.
	for _, c := range []struct {
		who, name string
		body      map[string]any
	}{
		{pCanceller, "cancelOrder", map[string]any{"reason_code": "R"}},
		{pCreator, "closeOrder", map[string]any{"reason_code": "R"}},
		{pHolder, "placeOrderHold", map[string]any{"reason_code": "R", "evidence_id": "E", "hold_kind": "OTHER"}},
		{pCreator, "recordBillableMilestone", msBody("LATE", 1, "1", "1")},
	} {
		code, body = e.mk(c.who, c.name, o, c.body)
		require.Equal(t, http.StatusConflict, code, c.name+string(body))
		require.Equal(t, kit.CodeInvalidTransition, errCode(t, body), c.name)
	}
	_, err = pool.Exec(context.Background(), `UPDATE ar03_sales_orders SET customer_po_ref = 'x' WHERE order_id = $1`, o.OrderID)
	require.ErrorContains(t, err, "AR03_ORDER_TERMINAL")

	// Held, unconfirmed and unknown milestones are not billable either; releasing the hold restores eligibility.
	h := e.confirmed(nil)
	h, ms := e.milestone(h, "FUL-H", 1, "1", "500")
	h = e.step(pHolder, "placeOrderHold", h, map[string]any{"hold_kind": HoldCommercial, "reason_code": "DISPUTE", "evidence_id": "CASE-1"})
	_, err = e.m.InvoiceEligibility(context.Background(), e.tenant, h.OrderID, ms)
	require.Contains(t, err.Error(), "on hold")
	require.Equal(t, kit.CodeOrderNotBillable, problemCode(t, err))
	require.Equal(t, kit.CodeOrderNotBillable, problemCode(t, e.invoice(h.OrderID, ms, "INV-H", "500")))
	h = e.step(pHolder, "releaseOrderHold", h, map[string]any{"reason_code": "RESOLVED", "evidence_id": "CASE-1-CLOSED"})
	require.False(t, h.HoldActive)
	_, err = e.m.InvoiceEligibility(context.Background(), e.tenant, h.OrderID, ms)
	require.NoError(t, err)
	_, err = e.m.InvoiceEligibility(context.Background(), e.tenant, h.OrderID, uuid.NewString())
	require.Equal(t, kit.CodeOrderNotBillable, problemCode(t, err), "unknown milestone")
	_, err = e.m.InvoiceEligibility(context.Background(), e.tenant, h.OrderID, "")
	require.Equal(t, kit.CodeOrderNotBillable, problemCode(t, err))
	d := e.approved(nil)
	_, err = e.m.InvoiceEligibility(context.Background(), e.tenant, d.OrderID, uuid.NewString())
	require.Equal(t, kit.CodeOrderNotBillable, problemCode(t, err), "unconfirmed order")
	_, err = e.m.InvoiceEligibility(context.Background(), e.tenant, uuid.NewString(), uuid.NewString())
	require.Equal(t, kit.CodeNotFound, problemCode(t, err))
}

// ── §18 #6: billed beyond the remaining eligible quantity / amount ───────────

func TestNeg06_OrderBilledBeyondRemainingEligibleQuantityOrAmount(t *testing.T) {
	e := newEnv(t)
	o := e.confirmed(nil) // line 1: 2 x 500 = 1000
	o, a := e.milestone(o, "FUL-A", 1, "1", "600")

	code, body := e.mk(pCreator, "recordBillableMilestone", o, msBody("FUL-B", 1, "1", "500")) // 600 + 500 > 1000
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	require.Equal(t, kit.CodeOrderNotBillable, errCode(t, body))
	code, body = e.mk(pCreator, "recordBillableMilestone", o, msBody("FUL-B", 1, "2", "100")) // quantity 1 + 2 > 2
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	require.Equal(t, kit.CodeOrderNotBillable, errCode(t, body))
	code, body = e.mk(pCreator, "recordBillableMilestone", o, msBody("FUL-B", 9, "1", "10"))
	require.Equal(t, http.StatusBadRequest, code, "a line that does not exist: "+string(body))
	b := msBody("FUL-B", 1, "1", "10.001")
	code, body = e.mk(pCreator, "recordBillableMilestone", o, b)
	require.Equal(t, http.StatusBadRequest, code, "USD has two decimals: "+string(body))
	o, bID := e.milestone(o, "FUL-B", 1, "1", "400")

	// RecordInvoiced refuses anything but the milestone's exact eligible net.
	require.Equal(t, kit.CodeOrderNotBillable, problemCode(t, e.invoice(o.OrderID, a, "INV-1", "600.01")))
	require.Equal(t, kit.CodeOrderNotBillable, problemCode(t, e.invoice(o.OrderID, a, "INV-1", "599.99")))
	require.Equal(t, kit.CodeValidationFailed, problemCode(t, e.invoice(o.OrderID, a, "INV-1", "0")))
	qty, amt := e.invoiced(o.OrderID, 1)
	require.Equal(t, []string{"0.0000", "0.0000"}, []string{qty, amt})
	require.NoError(t, e.invoice(o.OrderID, a, "INV-1", "600"))
	require.NoError(t, e.invoice(o.OrderID, bID, "INV-2", "400"))
	qty, amt = e.invoiced(o.OrderID, 1)
	require.Equal(t, []string{"2.0000", "1000.0000"}, []string{qty, amt})
	o = e.get(o.OrderID)
	require.Zero(t, o.Lines[0].RemainingAmount)
	require.Zero(t, o.Lines[0].RemainingQty)

	// Fully billed line: nothing more can be scheduled against it.
	code, body = e.mk(pCreator, "recordBillableMilestone", o, msBody("FUL-C", 1, "0", "1"))
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	require.Equal(t, kit.CodeOrderNotBillable, errCode(t, body))

	// The data refuses over-billing even if the application were bypassed.
	ctx := context.Background()
	for _, set := range []string{`invoiced_amount = 1000.01`, `invoiced_qty = 3`} {
		_, err := pool.Exec(ctx, `UPDATE ar03_order_lines SET `+set+` WHERE order_id = $1 AND line_no = 1`, o.OrderID)
		require.ErrorContains(t, err, "ar03_invoiced_within_ordered", set)
	}
	_, err := pool.Exec(ctx, `UPDATE ar03_order_lines SET invoiced_amount = 5 WHERE order_id = $1 AND line_no = 2`, o.OrderID)
	require.NoError(t, err, "within the ordered amount is only a number; the rule is the ceiling")
	_, err = pool.Exec(ctx, `UPDATE ar03_order_lines SET invoiced_qty = 1.5 WHERE order_id = $1 AND line_no = 2`, o.OrderID)
	require.ErrorContains(t, err, "ar03_invoiced_within_ordered")
}

// ── segregation of duties ────────────────────────────────────────────────────

func TestSoD_ApprovalAboveCommercialAndCreditThreshold(t *testing.T) {
	e := newEnv(t)
	big := map[string]any{"lines": []map[string]any{{"description": "Programme", "quantity": "1", "unit_price": "150000"}}}

	// Above the commercial threshold the creator/submitter cannot approve, even holding every authority.
	o := e.step(pBoss, "submitOrder", e.create(pBoss, big), nil)
	code, body := e.mk(pBoss, "approveOrder", o, map[string]any{"version_no": 1})
	require.Equal(t, http.StatusForbidden, code, string(body))
	require.Equal(t, kit.CodeSegregationOfDuties, errCode(t, body))
	require.Equal(t, 1, e.denials(o.OrderID, "approveOrder"))
	require.Equal(t, StatePendingApproval, e.get(o.OrderID).State)
	require.Zero(t, e.count(`SELECT count(*) FROM ar03_order_approvals WHERE order_id = $1`, o.OrderID))
	// ...nor can the submitter when somebody else created it.
	c := e.create(pCreator, big)
	c = e.step(pBoss, "submitOrder", e.get(c.OrderID), nil)
	code, body = e.mk(pBoss, "approveOrder", c, map[string]any{"version_no": 1})
	require.Equal(t, http.StatusForbidden, code, string(body))
	require.Equal(t, kit.CodeSegregationOfDuties, errCode(t, body))
	// An independent approver with the approve action may.
	o = e.approveAs(pApprover, o, nil)
	require.Equal(t, StateApproved, o.State)
	var sod bool
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT sod_required FROM ar03_order_approvals WHERE order_id = $1`, o.OrderID).Scan(&sod))
	require.True(t, sod)
	// No approve action at all: plain 403.
	n := e.submit(e.create(pCreator, nil))
	code, body = e.mk(pNobody, "approveOrder", n, map[string]any{"version_no": 1})
	require.Equal(t, http.StatusForbidden, code)
	require.Equal(t, kit.CodeForbidden, errCode(t, body))

	// At or below the threshold the creator may approve their own order.
	atLimit := e.step(pBoss, "submitOrder", e.create(pBoss, map[string]any{"lines": []map[string]any{{"description": "x", "quantity": "1", "unit_price": "100000"}}}), nil)
	require.Equal(t, StateApproved, e.approveAs(pBoss, atLimit, nil).State, "exactly the threshold is within the creator's authority")
	small := e.step(pBoss, "submitOrder", e.create(pBoss, nil), nil)
	require.Equal(t, StateApproved, e.approveAs(pBoss, small, nil).State)

	// Credit threshold: more than half of the credit limit needs an independent approver.
	e.prof.set(false, kit.MustAmount("2000"), nil) // 1250.50 > 1000
	cr := e.step(pBoss, "submitOrder", e.create(pBoss, nil), nil)
	code, body = e.mk(pBoss, "approveOrder", cr, map[string]any{"version_no": 1})
	require.Equal(t, http.StatusForbidden, code, string(body))
	require.Equal(t, kit.CodeSegregationOfDuties, errCode(t, body))
	require.Equal(t, StateApproved, e.approveAs(pApprover, cr, nil).State)

	// The data enforces it too.
	_, err := pool.Exec(context.Background(), `INSERT INTO ar03_order_approvals (approval_id, tenant_id, legal_entity_id, order_id, version_no, content_hash, net_total,
		sod_required, created_by, submitted_by, approved_by) VALUES ($1,$2,$3,$4,1,'h',1,true,'same','x','same')`, uuid.NewString(), e.tenant, e.entity, o.OrderID)
	require.ErrorContains(t, err, "ar03_approval_sod")
	_, err = pool.Exec(context.Background(), `INSERT INTO ar03_credit_exceptions (exception_id, tenant_id, legal_entity_id, order_id, version_no, content_hash, order_net,
		reason_code, evidence_id, created_by, submitted_by, approved_by) VALUES ($1,$2,$3,$4,1,'h',1,'R','E','same','x','same')`, uuid.NewString(), e.tenant, e.entity, o.OrderID)
	require.ErrorContains(t, err, "ar03_exception_sod")
}

func TestSoD_CreditHoldReleaseNeedsIndependentAuthority(t *testing.T) {
	e := newEnv(t)
	o := e.confirmed(nil)
	hold := map[string]any{"hold_kind": HoldCredit, "reason_code": "OVERDUE_BALANCE", "evidence_id": "AR-77"}
	o = e.step(pCreditRel, "placeOrderHold", o, hold)
	require.True(t, o.HoldActive)
	require.Equal(t, HoldCredit, o.HoldKind)
	require.Equal(t, pCreditRel, o.HeldBy)
	require.Equal(t, 1, e.events(o.OrderID, EventSalesOrderHeld))
	code, body := e.mk(pCreditRel, "placeOrderHold", o, hold)
	require.Equal(t, http.StatusConflict, code, "already on hold: "+string(body))

	rel := map[string]any{"reason_code": "BALANCE_PAID", "evidence_id": "PAY-5"}
	// The placer cannot release their own credit hold, even holding the release authority.
	code, body = e.mk(pCreditRel, "releaseOrderHold", o, rel)
	require.Equal(t, http.StatusForbidden, code, string(body))
	require.Equal(t, kit.CodeSegregationOfDuties, errCode(t, body))
	require.Equal(t, 1, e.denials(o.OrderID, "releaseOrderHold"))
	// A different principal without the credit authority cannot either.
	code, body = e.mk(pHolder, "releaseOrderHold", o, rel)
	require.Equal(t, http.StatusForbidden, code, string(body))
	require.Equal(t, kit.CodeForbidden, errCode(t, body))
	require.True(t, e.get(o.OrderID).HoldActive)
	require.Zero(t, e.events(o.OrderID, EventSalesOrderReleased))
	// A different principal holding it can.
	o = e.step(pCreditAuth, "releaseOrderHold", o, rel)
	require.False(t, o.HoldActive)
	require.Empty(t, o.HoldKind)
	require.Equal(t, 1, e.events(o.OrderID, EventSalesOrderReleased))
	code, body = e.mk(pCreditAuth, "releaseOrderHold", o, rel)
	require.Equal(t, http.StatusConflict, code, "not on hold: "+string(body))

	// A non-credit hold: the placer may release it with the ordinary hold action.
	p := e.step(pHolder, "placeOrderHold", o, map[string]any{"hold_kind": HoldFulfillment, "reason_code": "R", "evidence_id": "E"})
	p = e.step(pHolder, "releaseOrderHold", p, rel)
	require.False(t, p.HoldActive)
	// Holds cannot be placed on drafts, and need a reason and evidence.
	d := e.create(pCreator, nil)
	code, _ = e.mk(pHolder, "placeOrderHold", d, hold)
	require.Equal(t, http.StatusConflict, code)
}

// ── RecordInvoiced: idempotency, races, atomicity ────────────────────────────

func TestRecordInvoiced_IdempotentPerMilestoneAndInvoice(t *testing.T) {
	e := newEnv(t)
	o := e.confirmed(nil)
	o, a := e.milestone(o, "FUL-A", 2, "1", "250.50")
	require.NoError(t, e.invoice(o.OrderID, a, "INV-1", "250.50"))
	v1 := e.get(o.OrderID).Version
	require.NoError(t, e.invoice(o.OrderID, a, "INV-1", "250.50"), "a replay is a no-op")
	require.NoError(t, e.invoice(o.OrderID, a, "INV-1", "250.50"))
	g := e.get(o.OrderID)
	require.Equal(t, v1, g.Version, "a replay changes nothing")
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar03_invoiced_ledger WHERE milestone_id = $1`, a))
	require.Equal(t, 1, e.events(o.OrderID, EventSalesOrderPartiallyBilled))
	qty, amt := e.invoiced(o.OrderID, 2)
	require.Equal(t, []string{"1.0000", "250.5000"}, []string{qty, amt})
	require.Equal(t, kit.CodeOrderNotBillable, problemCode(t, e.invoice(o.OrderID, a, "INV-2", "250.50")), "another invoice for a billed milestone is refused")
	require.Equal(t, MsInvoiced, g.Milestones[0].State)
	require.Equal(t, "INV-1", g.Milestones[0].InvoiceID)
	require.Equal(t, kit.MustAmount("250.50"), g.Milestones[0].InvoicedNet)

	// The recorded movement is evidence: history, audit and ledger carry the invoice.
	code, body := e.query(pNobody, "getOrderHistory", "order_id="+o.OrderID)
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, string(body), `"invoice_id":"INV-1"`)
	// Malformed input.
	require.Equal(t, kit.CodeValidationFailed, problemCode(t, e.invoice(o.OrderID, a, "", "250.50")))
	require.Equal(t, kit.CodeValidationFailed, problemCode(t, e.invoice("nope", a, "INV-3", "250.50")))
	require.Equal(t, kit.CodeOrderNotBillable, problemCode(t, e.invoice(o.OrderID, "", "INV-3", "250.50")))
	require.Equal(t, kit.CodeNotFound, problemCode(t, e.invoice(uuid.NewString(), a, "INV-3", "250.50")))
}

func TestRecordInvoiced_ConcurrentRaceForTheLastRemainingAmount(t *testing.T) {
	e := newEnv(t)
	o := e.confirmed(nil)
	o, a := e.milestone(o, "FUL-A", 2, "1", "250.50") // the last remaining amount of line 2

	// Many invoices race for the same milestone: exactly one wins, the rest are ORDER_NOT_BILLABLE.
	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = e.invoice(o.OrderID, a, fmt.Sprintf("INV-RACE-%d", i), "250.50")
		}(i)
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
			continue
		}
		require.Equal(t, kit.CodeOrderNotBillable, problemCode(t, err))
	}
	require.Equal(t, 1, wins, "exactly one invoice takes the last remaining amount")
	qty, amt := e.invoiced(o.OrderID, 2)
	require.Equal(t, []string{"1.0000", "250.5000"}, []string{qty, amt})
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar03_invoiced_ledger WHERE order_id = $1`, o.OrderID))

	// The same invoice delivered concurrently is applied once and every call succeeds.
	p := e.confirmed(nil)
	p, m1 := e.milestone(p, "FUL-A", 1, "1", "500")
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = e.invoice(p.OrderID, m1, "INV-SAME", "500")
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar03_invoiced_ledger WHERE order_id = $1`, p.OrderID))
	require.Equal(t, 1, e.events(p.OrderID, EventSalesOrderPartiallyBilled))

	// Two different milestones of one line racing for its two halves both land, and the thresholds fire once.
	q := e.confirmed(nil)
	q, qa := e.milestone(q, "FUL-A", 1, "1", "500")
	q, qb := e.milestone(q, "FUL-B", 1, "1", "500")
	q, qc := e.milestone(q, "FUL-C", 2, "1", "250.50")
	for i, ms := range []string{qa, qb, qc} {
		wg.Add(1)
		go func(i int, ms string, net string) {
			defer wg.Done()
			errs[i] = e.invoice(q.OrderID, ms, fmt.Sprintf("INV-Q-%d", i), net)
		}(i, ms, []string{"500", "500", "250.50"}[i])
	}
	wg.Wait()
	for _, err := range errs[:3] {
		require.NoError(t, err)
	}
	g := e.get(q.OrderID)
	require.Equal(t, StateFullyBilled, g.State)
	require.Zero(t, g.Billing.RemainingNet)
	require.Equal(t, 1, e.events(q.OrderID, EventSalesOrderFullyBilled))
	require.LessOrEqual(t, e.events(q.OrderID, EventSalesOrderPartiallyBilled), 1)
}

func TestRecordInvoiced_RollbackLeavesNothingBehind(t *testing.T) {
	e := newEnv(t)
	o := e.confirmed(nil)
	o, a := e.milestone(o, "FUL-A", 1, "1", "500")
	before := e.get(o.OrderID)
	hBefore := e.count(`SELECT count(*) FROM ar03_order_history WHERE order_id = $1`, o.OrderID)
	aBefore := e.audits(o.OrderID)

	err := kit.NewDB(pool).WithTx(context.Background(), e.tenant, func(tx pgx.Tx) error {
		if err := e.m.RecordInvoiced(context.Background(), tx, e.caller(), o.OrderID, a, "INV-1", kit.MustAmount("500")); err != nil {
			return err
		}
		return errors.New("the invoice issue failed after the order was updated")
	})
	require.Error(t, err)
	after := e.get(o.OrderID)
	require.Equal(t, before.Version, after.Version)
	require.Equal(t, before.State, after.State)
	qty, amt := e.invoiced(o.OrderID, 1)
	require.Equal(t, []string{"0.0000", "0.0000"}, []string{qty, amt}, "a failed outer transaction leaves invoiced untouched")
	require.Equal(t, MsRecorded, after.Milestones[0].State)
	require.Zero(t, e.count(`SELECT count(*) FROM ar03_invoiced_ledger WHERE order_id = $1`, o.OrderID))
	require.Equal(t, hBefore, e.count(`SELECT count(*) FROM ar03_order_history WHERE order_id = $1`, o.OrderID))
	require.Equal(t, aBefore, e.audits(o.OrderID))
	require.Zero(t, e.events(o.OrderID, EventSalesOrderPartiallyBilled))
	// ...and the same invoice then succeeds.
	require.NoError(t, e.invoice(o.OrderID, a, "INV-1", "500"))
	require.Equal(t, 1, e.events(o.OrderID, EventSalesOrderPartiallyBilled))
}

// ── ar02.OrderCreator ────────────────────────────────────────────────────────

func quoteInput(e *env, quoteID string) ar02.OrderFromQuote {
	return ar02.OrderFromQuote{QuoteID: quoteID, QuoteNumber: "Q-000001", VersionNo: 1, ContentHash: "h", LegalEntityID: e.entity,
		CustomerPartyID: uuid.NewString(), Currency: "USD", Subtotal: kit.MustAmount("1250.50"), DiscountPercent: kit.MustAmount("5"),
		DiscountAmount: kit.MustAmount("62.53"), NetTotal: kit.MustAmount("1187.97"), PriceSourceRef: "PRICELIST-2026-Q4", Terms: "Net 30",
		Ref: "quote:" + quoteID + ":v1",
		Lines: []ar02.OrderLine{
			{LineNo: 1, Description: "Platform licence", ProductRef: "SKU-LIC", Quantity: kit.MustAmount("2"), UnitPrice: kit.MustAmount("500"), LineNet: kit.MustAmount("1000")},
			{LineNo: 2, Description: "Onboarding", ProductRef: "SKU-ONB", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("250.50"), LineNet: kit.MustAmount("250.50")},
		}}
}

func (e *env) fromQuote(in ar02.OrderFromQuote) (string, error) {
	var id string
	err := kit.NewDB(pool).WithTx(context.Background(), e.tenant, func(tx pgx.Tx) error {
		var err error
		id, err = e.m.CreateFromQuote(context.Background(), tx, kit.Caller{TenantID: e.tenant, PrincipalID: pCreator, CorrelationID: "conv"}, in)
		return err
	})
	return id, err
}

func TestCreateFromQuote_IdempotentOnSourceRef(t *testing.T) {
	e := newEnv(t)
	in := quoteInput(e, uuid.NewString())
	id1, err := e.fromQuote(in)
	require.NoError(t, err)
	id2, err := e.fromQuote(in)
	require.NoError(t, err)
	require.Equal(t, id1, id2, "a second conversion returns the original order, never a second one")
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar03_sales_orders WHERE tenant_id = $1 AND source_ref = $2`, e.tenant, in.Ref))
	require.Equal(t, 1, e.events(id1, EventSalesOrderCreated))
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar03_order_history WHERE order_id = $1`, id1))

	o := e.get(id1)
	require.Equal(t, StateDraft, o.State)
	require.Equal(t, in.Ref, o.SourceQuoteRef)
	require.Equal(t, in.Ref, o.SourceRef)
	require.Equal(t, in.CustomerPartyID, o.CustomerPartyID)
	require.Equal(t, kit.MustAmount("1187.97"), o.NetTotal)
	require.Equal(t, kit.MustAmount("5"), o.DiscountPercent)
	require.Equal(t, "PRICELIST-2026-Q4", o.Current.PriceSourceRef)
	require.Equal(t, "Net 30", o.Current.Terms)
	require.Len(t, o.Lines, 2)
	require.Equal(t, kit.MustAmount("500"), o.Lines[0].UnitPrice)
	require.Equal(t, kit.MustAmount("2"), o.Lines[0].Quantity)
	sum := o.Lines[0].LineNet.Add(o.Lines[1].LineNet)
	require.Equal(t, kit.MustAmount("1187.97"), sum, "the discount is allocated to the lines exactly")
	require.Equal(t, kit.MustAmount("62.53"), o.Lines[0].DiscountAmount.Add(o.Lines[1].DiscountAmount))
	require.Equal(t, pCreator, o.CreatedBy)

	// A different version of the quote is a different source and creates its own order.
	in2 := in
	in2.Ref = "quote:" + in.QuoteID + ":v2"
	id3, err := e.fromQuote(in2)
	require.NoError(t, err)
	require.NotEqual(t, id1, id3)

	// Concurrent conversions of the same source produce one order.
	in3 := quoteInput(e, uuid.NewString())
	ids := make([]string, 6)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i], _ = e.fromQuote(in3)
		}(i)
	}
	wg.Wait()
	for _, id := range ids {
		require.Equal(t, ids[0], id)
		require.NotEmpty(t, id)
	}
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar03_sales_orders WHERE tenant_id = $1 AND source_ref = $2`, e.tenant, in3.Ref))

	// It rolls back with the caller's transaction: no order, and the number is not burned.
	in4 := quoteInput(e, uuid.NewString())
	err = kit.NewDB(pool).WithTx(context.Background(), e.tenant, func(tx pgx.Tx) error {
		if _, err := e.m.CreateFromQuote(context.Background(), tx, kit.Caller{TenantID: e.tenant, PrincipalID: pCreator}, in4); err != nil {
			return err
		}
		return errors.New("quote conversion failed after the order was created")
	})
	require.Error(t, err)
	require.Zero(t, e.count(`SELECT count(*) FROM ar03_sales_orders WHERE tenant_id = $1 AND source_ref = $2`, e.tenant, in4.Ref))
	next := e.create(pCreator, nil)
	require.Equal(t, "SO-000004", next.OrderNumber, "three committed orders exist: the rolled-back number was not burned")

	// Totals that do not match the lines are refused and create nothing.
	bad := quoteInput(e, uuid.NewString())
	bad.NetTotal = kit.MustAmount("1187.98")
	_, err = e.fromQuote(bad)
	require.Equal(t, kit.CodeValidationFailed, problemCode(t, err))
	bad = quoteInput(e, uuid.NewString())
	bad.Lines[0].LineNet = kit.MustAmount("999")
	_, err = e.fromQuote(bad)
	require.Equal(t, kit.CodeValidationFailed, problemCode(t, err))
	bad.Ref = ""
	_, err = e.fromQuote(bad)
	require.Equal(t, kit.CodeValidationFailed, problemCode(t, err))

	// The converted order continues through the normal lifecycle.
	e.prof.set(false, 0, nil)
	co := e.get(id1)
	co = e.approveAs(pApprover, e.submit(co), nil)
	require.Equal(t, StateConfirmed, e.step(pConfirmer, "confirmOrder", co, nil).State)
}

// ── governed amendments before confirmation, close, JPY/KWD ──────────────────

func TestAmendBeforeConfirmation_VoidsApprovalAndKeepsVersions(t *testing.T) {
	e := newEnv(t)
	o := e.approved(nil)
	oldHash := o.ApprovedHash
	o = e.step(pCreator, "amendSalesOrder", o, map[string]any{"lines": []map[string]any{{"description": "Platform licence", "quantity": "3", "unit_price": "500.00"}}})
	require.Equal(t, StateDraft, o.State)
	require.Equal(t, 2, o.CurrentVersionNo)
	require.Empty(t, o.ApprovedHash, "the approval was bound to the old content")
	require.Empty(t, o.SubmittedBy)
	require.NotEqual(t, oldHash, o.Current.Hash)
	require.Len(t, o.Lines, 1)
	require.Equal(t, kit.MustAmount("1500"), o.NetTotal)
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar03_order_approvals WHERE order_id = $1`, o.OrderID), "the old approval stays as history")

	o = e.submit(o)
	code, body := e.mk(pApprover, "approveOrder", o, map[string]any{"version_no": 1})
	require.Equal(t, http.StatusConflict, code, string(body))
	require.Equal(t, CodeApprovalInvalidated, errCode(t, body))
	code, body = e.mk(pApprover, "approveOrder", o, map[string]any{"version_no": 2, "content_hash": oldHash})
	require.Equal(t, CodeApprovalInvalidated, errCode(t, body))
	o = e.approveAs(pApprover, o, nil)
	require.Equal(t, 2, o.ApprovedVersionNo)
	require.Equal(t, 2, e.count(`SELECT count(*) FROM ar03_order_approvals WHERE order_id = $1`, o.OrderID))

	code, body = e.mk(pCreator, "amendSalesOrder", o, map[string]any{"terms": o.Current.Terms})
	require.Equal(t, http.StatusBadRequest, code, "an amendment that changes nothing is refused: "+string(body))
	code, body = e.mk(pCreator, "amendSalesOrder", o, map[string]any{"currency": "EUR"})
	require.Equal(t, http.StatusBadRequest, code, "currency never changes on an order: "+string(body))
	code, body = e.mk(pCreator, "amendSalesOrder", o, map[string]any{"price_source_ref": "has space"})
	require.Equal(t, http.StatusBadRequest, code)
	code, _ = e.mk(pApprover, "amendSalesOrder", o, map[string]any{"terms": "x"})
	require.Equal(t, http.StatusForbidden, code)

	// The history records the invalidated approval and the prior values.
	_, body = e.query(pNobody, "getOrderHistory", "order_id="+o.OrderID)
	var hs struct {
		Items []HistoryEntry `json:"items"`
	}
	require.NoError(t, jsonDecode(body, &hs))
	var amend HistoryEntry
	for _, it := range hs.Items {
		if it.Command == "amendSalesOrder" {
			amend = it
		}
	}
	require.Equal(t, true, amend.NewValues["approval_invalidated"])
	require.Equal(t, oldHash, amend.PriorValues["approved_content_hash"])
	require.Equal(t, "1250.5000", amend.PriorValues["net_total"])

	// Confirmed orders cannot be amended through the draft path; closed ones not at all.
	c := e.confirmed(nil)
	code, body = e.mk(pCreator, "amendSalesOrder", c, map[string]any{"terms": "new terms"})
	require.Equal(t, http.StatusBadRequest, code, "reason_code comes first: "+string(body))
	c = e.step(pCreator, "amendSalesOrder", c, map[string]any{"terms": "new terms", "customer_po_ref": "PO-9", "reason_code": "TERMS_CHANGE", "evidence_id": "MAIL-3"})
	require.Equal(t, StateConfirmed, c.State, "a non-monetary governed amendment keeps the confirmed state")
	require.Equal(t, 2, c.CurrentVersionNo)
	require.Equal(t, "PO-9", c.CustomerPORef)
	cl := e.step(pCreator, "closeOrder", e.get(e.confirmed(nil).OrderID), map[string]any{"reason_code": "SHORT_CLOSE"})
	code, _ = e.mk(pCreator, "amendSalesOrder", cl, map[string]any{"terms": "x", "reason_code": "R", "evidence_id": "E"})
	require.Equal(t, http.StatusConflict, code)
}

func TestCloseAndShortClose(t *testing.T) {
	e := newEnv(t)
	o := e.confirmed(nil)
	o, _ = e.milestone(o, "FUL-A", 1, "1", "500")
	code, body := e.mk(pCreator, "closeOrder", o, nil)
	require.Equal(t, http.StatusBadRequest, code, "a short-close needs a reason: "+string(body))
	require.Equal(t, kit.CodeReasonRequired, errCode(t, body))
	o = e.step(pHolder, "placeOrderHold", o, map[string]any{"hold_kind": HoldOther, "reason_code": "R", "evidence_id": "E"})
	o = e.step(pCreator, "closeOrder", o, map[string]any{"reason_code": "CUSTOMER_SCOPE_REDUCED", "evidence_id": "CR-1"})
	require.Equal(t, StateClosed, o.State)
	require.False(t, o.HoldActive)
	require.Equal(t, MsVoided, o.Milestones[0].State, "unbilled eligibility is voided")
	code, _ = e.mk(pCreator, "closeOrder", o, map[string]any{"reason_code": "R"})
	require.Equal(t, http.StatusConflict, code)
	// Draft and unapproved orders cannot be closed (they are cancelled).
	code, _ = e.mk(pCreator, "closeOrder", e.create(pCreator, nil), map[string]any{"reason_code": "R"})
	require.Equal(t, http.StatusConflict, code)
}

func TestJPYAndKWDOrders(t *testing.T) {
	e := newEnv(t)
	j := e.create(pCreator, map[string]any{"currency": "JPY", "discount_percent": "7.5", "lines": []map[string]any{
		{"description": "x", "quantity": "3", "unit_price": "333"}, {"description": "y", "quantity": "1", "unit_price": "1001"}}})
	require.Equal(t, kit.MustAmount("1850"), j.NetTotal)
	require.Equal(t, kit.MustAmount("1850"), j.Lines[0].LineNet.Add(j.Lines[1].LineNet))
	j = e.approveAs(pApprover, e.submit(j), nil)
	j = e.step(pConfirmer, "confirmOrder", j, nil)
	code, body := e.mk(pCreator, "recordBillableMilestone", j, msBody("J-1", 1, "1", "10.5"))
	require.Equal(t, http.StatusBadRequest, code, "JPY has no minor unit: "+string(body))
	j, m1 := e.milestone(j, "J-1", 1, "3", j.Lines[0].LineNet.String())
	require.NoError(t, e.invoice(j.OrderID, m1, "INV-J1", j.Lines[0].LineNet.String()))
	g := e.get(j.OrderID)
	require.Equal(t, StatePartiallyBilled, g.State)
	require.Equal(t, j.Lines[1].LineNet, g.Billing.RemainingNet)

	k := e.create(pCreator, map[string]any{"currency": "KWD", "lines": []map[string]any{{"description": "x", "quantity": "3", "unit_price": "1.333"}}})
	require.Equal(t, kit.MustAmount("3.999"), k.NetTotal)
	k = e.approveAs(pApprover, e.submit(k), nil)
	k = e.step(pConfirmer, "confirmOrder", k, nil)
	k, km := e.milestone(k, "K-1", 1, "3", "3.999")
	require.NoError(t, e.invoice(k.OrderID, km, "INV-K1", "3.999"))
	require.Equal(t, StateFullyBilled, e.get(k.OrderID).State)
}

// ── plumbing: concurrency, idempotency, isolation, atomicity, immutability ───

func TestOptimisticConcurrency(t *testing.T) {
	e := newEnv(t)
	o := e.create(pCreator, nil)
	b := obody(o, nil)
	b["expected_version"] = o.Version - 1
	code, body := e.cmd(pCreator, "submitOrder", b)
	require.Equal(t, http.StatusConflict, code)
	require.Equal(t, kit.CodeStaleVersion, errCode(t, body))
	delete(b, "expected_version")
	code, body = e.cmd(pCreator, "submitOrder", b)
	require.Equal(t, http.StatusBadRequest, code)
	require.Equal(t, kit.CodeExpectedVersionMissing, errCode(t, body))
	require.Equal(t, StateDraft, e.get(o.OrderID).State)

	// Racing identical approvals: exactly one wins, the rest are stale; one approval row.
	p := e.submit(o)
	var wg sync.WaitGroup
	codes := make([]int, 6)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _, _ = call(t, e.h, http.MethodPost, base+"commands/approveOrder", e.tenant, pApprover, "", obody(p, map[string]any{"version_no": 1}))
		}(i)
	}
	wg.Wait()
	wins := 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			wins++
		case http.StatusConflict:
		default:
			t.Fatalf("unexpected status %d", c)
		}
	}
	require.Equal(t, 1, wins)
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar03_order_approvals WHERE order_id = $1`, p.OrderID))

	// Racing amendments at one version: one new version, never two version 2 rows.
	r := e.create(pCreator, nil)
	codes = make([]int, 6)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _, _ = call(t, e.h, http.MethodPost, base+"commands/amendSalesOrder", e.tenant, pCreator, "", obody(r, map[string]any{"terms": fmt.Sprintf("terms %d", i)}))
		}(i)
	}
	wg.Wait()
	wins = 0
	for _, c := range codes {
		if c == http.StatusOK {
			wins++
		}
	}
	require.Equal(t, 1, wins)
	require.Equal(t, 2, e.count(`SELECT count(*) FROM ar03_order_versions WHERE order_id = $1`, r.OrderID))
}

func TestIdempotentReplay(t *testing.T) {
	e := newEnv(t)
	body := createBody(e.entity, uuid.NewString())
	key := uuid.NewString()
	c1, b1, _ := call(t, e.h, http.MethodPost, base+"commands/createSalesOrder", e.tenant, pCreator, key, body)
	require.Equal(t, http.StatusCreated, c1, string(b1))
	c2, b2, h2 := call(t, e.h, http.MethodPost, base+"commands/createSalesOrder", e.tenant, pCreator, key, body)
	require.Equal(t, http.StatusCreated, c2)
	require.Equal(t, "true", h2.Get("Idempotent-Replay"))
	require.JSONEq(t, string(b1), string(b2))
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar03_sales_orders WHERE tenant_id = $1`, e.tenant))
	require.Equal(t, 1, e.count(`SELECT count(*) FROM outbox_events WHERE tenant_id = $1`, e.tenant))
	c3, b3, _ := call(t, e.h, http.MethodPost, base+"commands/createSalesOrder", e.tenant, pCreator, key, createBody(e.entity, uuid.NewString()))
	require.Equal(t, http.StatusUnprocessableEntity, c3)
	require.Equal(t, kit.CodeIdempotencyReused, errCode(t, b3))

	o := asOrder(t, b1)
	k2 := uuid.NewString()
	call(t, e.h, http.MethodPost, base+"commands/submitOrder", e.tenant, pCreator, k2, obody(o, nil))
	c4, _, h4 := call(t, e.h, http.MethodPost, base+"commands/submitOrder", e.tenant, pCreator, k2, obody(o, nil))
	require.Equal(t, http.StatusOK, c4)
	require.Equal(t, "true", h4.Get("Idempotent-Replay"))
	require.Equal(t, 2, e.get(o.OrderID).Version, "a replayed state change does not apply twice")
}

func TestAtomicity_FailedCommandsLeaveNoEvidenceAndNoGaps(t *testing.T) {
	e := newEnv(t)
	s := e.create(pCreator, nil)
	ctx := context.Background()
	c := kit.Caller{TenantID: e.tenant, PrincipalID: pCreator, LegalEntityID: e.entity}
	beforeHist := e.count(`SELECT count(*) FROM ar03_order_history WHERE order_id = $1`, s.OrderID)
	beforeAudit := e.audits(s.OrderID)
	err := kit.NewDB(pool).WithTx(ctx, e.tenant, func(tx pgx.Tx) error {
		cur, err := getHeader(ctx, tx, e.tenant, s.OrderID, true)
		require.NoError(t, err)
		nh := *cur
		nh.State = StatePendingApproval
		_, err = commit(ctx, tx, c, cur, &nh, hist{At: time.Now(), Command: "submitOrder"}, func(h *Header) []kit.Event {
			return []kit.Event{{AggregateType: AggregateType, AggregateID: h.OrderID, EventType: EventSalesOrderHeld, Payload: map[string]any{}}}
		})
		require.NoError(t, err)
		return errors.New("simulated failure after the writes")
	})
	require.Error(t, err)
	require.Equal(t, beforeHist, e.count(`SELECT count(*) FROM ar03_order_history WHERE order_id = $1`, s.OrderID))
	require.Equal(t, beforeAudit, e.audits(s.OrderID))
	require.Zero(t, e.events(s.OrderID, EventSalesOrderHeld))
	require.Equal(t, StateDraft, e.get(s.OrderID).State)

	// A failed create (profile inactive) leaves no order and does not burn a number.
	e.prof.set(false, 0, kit.Unprocessable(kit.CodeCustomerProfileInactive, "no"))
	code, _ := e.cmd(pCreator, "createSalesOrder", createBody(e.entity, uuid.NewString()))
	require.Equal(t, http.StatusUnprocessableEntity, code)
	e.prof.set(false, 0, nil)
	n := e.create(pCreator, nil)
	require.Equal(t, 2, e.count(`SELECT count(*) FROM ar03_sales_orders WHERE tenant_id = $1`, e.tenant))
	require.Equal(t, "SO-000002", n.OrderNumber)
}

func TestTenantAndEntityIsolation(t *testing.T) {
	e := newEnv(t)
	o := e.confirmed(nil)
	o, ms := e.milestone(o, "FUL-A", 1, "1", "500")

	other := *e
	other.tenant = uuid.NewString()
	for _, name := range []string{"getSalesOrder", "getOrderBillingStatus", "getInvoiceEligibility", "getOrderHistory", "getAvailableActions"} {
		code, body := other.query(pCreator, name, "order_id="+o.OrderID)
		require.Equal(t, http.StatusNotFound, code, name)
		require.Equal(t, kit.CodeNotFound, errCode(t, body), name)
	}
	code, _ := other.query(pCreator, "getSalesOrder", "legal_entity_id="+e.entity+"&order_number="+o.OrderNumber)
	require.Equal(t, http.StatusNotFound, code)
	for _, c := range []struct {
		who, cmd string
		body     map[string]any
	}{
		{pCanceller, "cancelOrder", map[string]any{"reason_code": "R"}},
		{pCreator, "closeOrder", map[string]any{"reason_code": "R"}},
		{pHolder, "placeOrderHold", map[string]any{"reason_code": "R", "evidence_id": "E", "hold_kind": "OTHER"}},
	} {
		code, body := other.mk(c.who, c.cmd, o, c.body)
		require.Equal(t, http.StatusNotFound, code, c.cmd+string(body))
	}
	_, err := e.m.InvoiceEligibility(context.Background(), other.tenant, o.OrderID, ms)
	require.Equal(t, kit.CodeNotFound, problemCode(t, err), "cross-tenant looks identical to absent")
	err = other.m.deps.DB.WithTx(context.Background(), other.tenant, func(tx pgx.Tx) error {
		return other.m.RecordInvoiced(context.Background(), tx, other.caller(), o.OrderID, ms, "INV-X", kit.MustAmount("500"))
	})
	require.Equal(t, kit.CodeNotFound, problemCode(t, err))
	require.Equal(t, o.Version, e.get(o.OrderID).Version)

	// A body entity that disagrees with the stored order is refused.
	code, body := e.mk(pCreator, "closeOrder", o, map[string]any{"legal_entity_id": uuid.NewString(), "reason_code": "R"})
	require.Equal(t, http.StatusForbidden, code, string(body))
	require.Equal(t, kit.CodeTenantScope, errCode(t, body))
	// Order numbers are per tenant and entity.
	require.Equal(t, "SO-000001", other.create(pCreator, nil).OrderNumber)
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar03_sales_orders WHERE tenant_id = $1`, other.tenant))
}

func TestImmutabilityAndAppendOnlyEvidence(t *testing.T) {
	e := newEnv(t)
	o := e.confirmed(nil)
	o, ms := e.milestone(o, "FUL-A", 1, "1", "500")
	require.NoError(t, e.invoice(o.OrderID, ms, "INV-1", "500"))
	ctx := context.Background()
	for _, stmt := range []string{
		`UPDATE ar03_order_versions SET net_total = 1 WHERE order_id = $1`,
		`DELETE FROM ar03_order_versions WHERE order_id = $1`,
		`UPDATE ar03_order_approvals SET approved_by = 'x' WHERE order_id = $1`,
		`DELETE FROM ar03_order_approvals WHERE order_id = $1`,
		`UPDATE ar03_order_history SET command = 'x' WHERE order_id = $1`,
		`DELETE FROM ar03_order_history WHERE order_id = $1`,
		`UPDATE ar03_invoiced_ledger SET net = 1 WHERE order_id = $1`,
		`DELETE FROM ar03_invoiced_ledger WHERE order_id = $1`,
	} {
		_, err := pool.Exec(ctx, stmt, o.OrderID)
		require.ErrorContains(t, err, "append-only", stmt)
	}
	for stmt, want := range map[string]string{
		`DELETE FROM ar03_sales_orders WHERE order_id = $1`:                                      "AR03_ORDER_NOT_DELETABLE",
		`UPDATE ar03_sales_orders SET currency = 'EUR' WHERE order_id = $1`:                      "AR03_ORDER_IDENTITY_IMMUTABLE",
		`UPDATE ar03_sales_orders SET customer_party_id = gen_random_uuid() WHERE order_id = $1`: "AR03_ORDER_IDENTITY_IMMUTABLE",
		`UPDATE ar03_sales_orders SET state = 'BOGUS' WHERE order_id = $1`:                       "ar03_sales_orders_state_check",
		`UPDATE ar03_sales_orders SET approved_content_hash = NULL WHERE order_id = $1`:          "ar03_state_requires_approval",
		`UPDATE ar03_sales_orders SET hold_active = true WHERE order_id = $1`:                    "ar03_hold_requires_facts",
		`UPDATE ar03_sales_orders SET state = 'CANCELLED' WHERE order_id = $1`:                   "ar03_cancel_requires_reason",
		`DELETE FROM ar03_billing_milestones WHERE order_id = $1`:                                "AR03_MILESTONE_NOT_DELETABLE",
		`UPDATE ar03_billing_milestones SET state = 'RECORDED' WHERE order_id = $1`:              "AR03_MILESTONE_FINAL",
	} {
		_, err := pool.Exec(ctx, stmt, o.OrderID)
		require.ErrorContains(t, err, want, stmt)
	}
	// A recorded milestone only moves to INVOICED/VOIDED and keeps its facts.
	p := e.confirmed(nil)
	p, pm := e.milestone(p, "FUL-X", 1, "1", "500")
	_, err := pool.Exec(ctx, `UPDATE ar03_billing_milestones SET scheduled_amount = 1 WHERE milestone_id = $1`, pm)
	require.ErrorContains(t, err, "AR03_MILESTONE_IMMUTABLE")
	_, err = pool.Exec(ctx, `UPDATE ar03_billing_milestones SET state = 'INVOICED' WHERE milestone_id = $1`, pm)
	require.ErrorContains(t, err, "ar03_milestone_invoiced_facts")
	_, err = pool.Exec(ctx, `UPDATE ar03_billing_milestones SET state = 'RECORDED', fulfillment_evidence_ref = 'x' WHERE milestone_id = $1`, pm)
	require.ErrorContains(t, err, "AR03_MILESTONE_IMMUTABLE")
	// Fulfillment evidence is mandatory in the data.
	_, err = pool.Exec(ctx, `INSERT INTO ar03_billing_milestones (milestone_id, tenant_id, legal_entity_id, order_id, external_ref, line_no, scheduled_qty, scheduled_amount,
		state, fulfillment_source, fulfillment_evidence_ref, recorded_by) VALUES ($1,$2,$3,$4,'Z',1,1,1,'RECORDED','S','  ','sql')`, uuid.NewString(), e.tenant, e.entity, p.OrderID)
	require.Error(t, err)
	// Lines cannot be created outside a governed command, versions only in sequence.
	_, err = pool.Exec(ctx, `INSERT INTO ar03_order_lines (order_id, line_no, tenant_id, description, ordered_qty, unit_price, list_net, line_net)
		VALUES ($1,9,$2,'x',1,1,1,1)`, p.OrderID, e.tenant)
	require.ErrorContains(t, err, "AR03_LINE_AMENDMENT_REQUIRED")
	_, err = pool.Exec(ctx, `INSERT INTO ar03_order_versions (order_id, version_no, tenant_id, legal_entity_id, currency, subtotal, discount_amount, net_total, content_hash, lines,
		change_kind, created_by) VALUES ($1,7,$2,$3,'USD',10,0,10,'h','[]','AMEND','sql')`, p.OrderID, e.tenant, e.entity)
	require.ErrorContains(t, err, "AR03_VERSION_SEQUENCE")
	_, err = pool.Exec(ctx, `INSERT INTO ar03_order_versions (order_id, version_no, tenant_id, legal_entity_id, currency, subtotal, discount_amount, net_total, content_hash, lines,
		change_kind, created_by) VALUES ($1,2,$2,$3,'USD',10,0,9,'h','[]','AMEND','sql')`, p.OrderID, e.tenant, e.entity)
	require.ErrorContains(t, err, "ar03_totals_consistent")
	_, err = pool.Exec(ctx, `UPDATE ar03_order_lines SET line_net = list_net + 1 WHERE order_id = $1`, p.OrderID)
	require.Error(t, err)
}

// ── queries ──────────────────────────────────────────────────────────────────

func TestGetAvailableActionsIsDerivedFromStateSoDAndPermissions(t *testing.T) {
	e := newEnv(t)
	actions := func(principal string, o *Order) map[string]actionState {
		code, body := e.query(principal, "getAvailableActions", "order_id="+o.OrderID)
		require.Equal(t, http.StatusOK, code, string(body))
		var r struct {
			Actions []actionState `json:"actions"`
		}
		require.NoError(t, jsonDecode(body, &r))
		out := map[string]actionState{}
		for _, a := range r.Actions {
			out[a.Action] = a
		}
		return out
	}
	o := e.create(pCreator, nil)
	a := actions(pCreator, o)
	require.True(t, a["SubmitOrder"].Available)
	require.True(t, a["AmendSalesOrder"].Available)
	require.False(t, a["ApproveOrder"].Available)
	require.Equal(t, "order is not pending approval", a["ApproveOrder"].Reason)
	require.False(t, a["ConfirmOrder"].Available)
	require.False(t, a["RecordBillableMilestone"].Available)
	require.True(t, a["CancelOrder"].Permission[0] == ActionOrderCancel)
	require.False(t, a["CancelOrder"].Available, "legal in a draft, but pCreator lacks the cancel action")
	require.Equal(t, "caller lacks "+ActionOrderCancel, a["CancelOrder"].Reason)
	require.False(t, actions(pNobody, o)["SubmitOrder"].Available, "legal but not permitted")

	big := e.step(pBoss, "submitOrder", e.create(pBoss, map[string]any{"lines": []map[string]any{{"description": "x", "quantity": "1", "unit_price": "150000"}}}), nil)
	require.False(t, actions(pBoss, big)["ApproveOrder"].Available, "the submitter cannot approve above the threshold")
	require.True(t, actions(pApprover, big)["ApproveOrder"].Available)

	c := e.confirmed(nil)
	a = actions(pBoss, c)
	require.True(t, a["RecordBillableMilestone"].Available)
	require.True(t, a["PlaceOrderHold"].Available)
	require.False(t, a["ReleaseOrderHold"].Available)
	require.False(t, a["ConfirmOrder"].Available)
	c = e.step(pCreditRel, "placeOrderHold", c, map[string]any{"hold_kind": HoldCredit, "reason_code": "R", "evidence_id": "E"})
	require.False(t, actions(pCreditRel, c)["ReleaseOrderHold"].Available, "the placer of a credit hold cannot release it")
	require.True(t, actions(pCreditAuth, c)["ReleaseOrderHold"].Available)
	require.False(t, actions(pHolder, c)["ReleaseOrderHold"].Available, "the hold action alone is not the credit release authority")
	require.False(t, actions(pBoss, c)["PlaceOrderHold"].Available)
}

func TestGetOrderHistoryPagingAndLists(t *testing.T) {
	e := newEnv(t)
	o := e.confirmed(nil)
	o, _ = e.milestone(o, "FUL-A", 1, "1", "500")
	code, body := e.query(pNobody, "getOrderHistory", "order_id="+o.OrderID+"&limit=2")
	require.Equal(t, http.StatusOK, code, string(body))
	var p1 struct {
		Items []HistoryEntry `json:"items"`
		Next  int            `json:"next_after_version"`
	}
	require.NoError(t, jsonDecode(body, &p1))
	require.Len(t, p1.Items, 2)
	require.Equal(t, 2, p1.Next)
	code, body = e.query(pNobody, "getOrderHistory", fmt.Sprintf("order_id=%s&after_version=%d&limit=50", o.OrderID, p1.Next))
	require.Equal(t, http.StatusOK, code)
	var p2 struct {
		Items    []HistoryEntry   `json:"items"`
		Versions []VersionSummary `json:"versions"`
	}
	require.NoError(t, jsonDecode(body, &p2))
	require.Len(t, p2.Items, 3)
	require.Equal(t, 3, p2.Items[0].OrderVersion)
	require.Empty(t, p2.Versions, "versions and approvals ride only on the first page")
	// POST with a JSON body is the same query.
	code, body, _ = call(t, e.h, http.MethodPost, base+"getSalesOrder", e.tenant, pNobody, "", map[string]any{"order_id": o.OrderID})
	require.Equal(t, http.StatusOK, code, string(body))
	require.Equal(t, o.OrderID, asOrder(t, body).OrderID)
	code, _ = e.query(pNobody, "getSalesOrder", "legal_entity_id="+e.entity+"&order_number="+o.OrderNumber)
	require.Equal(t, http.StatusOK, code)
	code, _ = e.query(pNobody, "getSalesOrder", "")
	require.Equal(t, http.StatusBadRequest, code)
	code, _ = e.query(pNobody, "getSalesOrder", "order_id=zzz")
	require.Equal(t, http.StatusBadRequest, code)
}
