//go:build integration

// Real-Postgres tests for AR-02. Run (serially):
//
//	go test -tags=integration -count=1 -timeout=600s ./internal/ar02/...
//
// The embedded superuser bypasses RLS, so isolation tests prove the explicit
// tenant_id predicates, not the policies.
package ar02

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

	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/kit/kittest"
	"zoiko.io/accounts-receivable-svc/internal/taxdet"
)

var pool *pgxpool.Pool

func TestMain(m *testing.M) {
	var stop func()
	pool, stop = kittest.Start("ar02_test", 17500)
	// AR-03 stand-in table: the fake OrderCreator writes here inside the conversion transaction.
	if _, err := pool.Exec(context.Background(), `CREATE TABLE IF NOT EXISTS ar02_test_orders (
		order_id TEXT PRIMARY KEY, quote_ref TEXT NOT NULL UNIQUE, tenant_id UUID NOT NULL, net_total NUMERIC(20,4))`); err != nil {
		fmt.Println("test table:", err)
		stop()
		os.Exit(1)
	}
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
	tax    *fakeTax
	orders *fakeOrders
	offset time.Duration // simulated clock skew
}

func (e *env) now() time.Time { return time.Now().Add(e.offset).UTC() }

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, tenant: uuid.NewString(), entity: uuid.NewString(), prof: &fakeProfiles{}, tax: &fakeTax{det: okDetermination()}, orders: &fakeOrders{}}
	deps := testDeps(&fakeAuthz{grants: standardGrants()})
	deps.DB = kit.NewDB(pool)
	deps.Clock = e.now
	e.m = New(deps, e.prof, e.tax, e.orders)
	e.h = testRouter(e.m)
	return e
}

func (e *env) dayOf(n int) string { return day(e.now().AddDate(0, 0, n)) }

func (e *env) cmd(principal, name string, body map[string]any) (int, []byte) {
	code, b, _ := call(e.t, e.h, http.MethodPost, base+"commands/"+name, e.tenant, principal, "", body)
	return code, b
}

func (e *env) query(principal, name, qs string) (int, []byte) {
	code, b, _ := call(e.t, e.h, http.MethodGet, base+name+"?"+qs, e.tenant, principal, "", nil)
	return code, b
}

func (e *env) ok(code int, body []byte) *Quote {
	e.t.Helper()
	require.Less(e.t, code, 300, string(body))
	return asQuote(e.t, body)
}

// create makes a DRAFT quote (default: 2 x 500.00 + 1 x 250.50, 5% discount).
func (e *env) create(principal string, extra map[string]any) *Quote {
	e.t.Helper()
	b := createBody(e.entity, uuid.NewString())
	for k, v := range extra {
		b[k] = v
	}
	code, body := e.cmd(principal, "createQuote", b)
	require.Equal(e.t, http.StatusCreated, code, string(body))
	return asQuote(e.t, body)
}

func qbody(q *Quote, extra map[string]any) map[string]any {
	b := map[string]any{"quote_id": q.QuoteID, "expected_version": q.Version}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

// mk issues a command on a quote at its current version.
func (e *env) mk(principal, name string, q *Quote, extra map[string]any) (int, []byte) {
	return e.cmd(principal, name, qbody(q, extra))
}

func (e *env) step(principal, name string, q *Quote, extra map[string]any) *Quote {
	e.t.Helper()
	code, body := e.mk(principal, name, q, extra)
	return e.ok(code, body)
}

func (e *env) get(id string) *Quote {
	e.t.Helper()
	code, body := e.query(pNobody, "getQuote", "quote_id="+id)
	return e.ok(code, body)
}

func (e *env) submit(q *Quote) *Quote { return e.step(pPreparer, "submitQuoteForApproval", q, nil) }
func (e *env) approveAs(principal string, q *Quote) *Quote {
	return e.step(principal, "approveQuote", q, map[string]any{"version_no": q.CurrentVersionNo})
}
func (e *env) send(q *Quote) *Quote {
	return e.step(pPreparer, "sendQuote", q, map[string]any{"send_channel": "EMAIL", "recipient": "buyer@example.com"})
}

func acceptBody(q *Quote) map[string]any {
	return map[string]any{"version_no": q.CurrentVersionNo, "accepted_by": "Jane Customer", "acceptance_channel": "SIGNED_DOCUMENT", "evidence_id": "EV-ACC-1"}
}

func (e *env) accept(q *Quote) *Quote { return e.step(pPreparer, "recordAcceptance", q, acceptBody(q)) }

func (e *env) approved(extra map[string]any) *Quote {
	return e.approveAs(pApprover, e.submit(e.create(pPreparer, extra)))
}
func (e *env) sent(extra map[string]any) *Quote     { return e.send(e.approved(extra)) }
func (e *env) accepted(extra map[string]any) *Quote { return e.accept(e.sent(extra)) }

func (e *env) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	require.NoError(e.t, pool.QueryRow(context.Background(), q, args...).Scan(&n))
	return n
}

func (e *env) events(quoteID, eventType string) int {
	return e.count(`SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND aggregate_id = $2 AND event_type = $3`, e.tenant, quoteID, eventType)
}

func (e *env) audits(quoteID string) int {
	return e.count(`SELECT count(*) FROM ar_audit_events WHERE tenant_id = $1 AND aggregate_id = $2`, e.tenant, quoteID)
}

func (e *env) approvals(quoteID string) int {
	return e.count(`SELECT count(*) FROM ar02_quote_approvals WHERE tenant_id = $1 AND quote_id = $2`, e.tenant, quoteID)
}

func (e *env) orderRows() int {
	return e.count(`SELECT count(*) FROM ar02_test_orders WHERE tenant_id = $1`, e.tenant)
}

func line(desc string, qty, price string) map[string]any {
	return map[string]any{"description": desc, "product_ref": "SKU-X", "quantity": qty, "unit_price": price}
}

// ── lifecycle, evidence, events, non-posting ─────────────────────────────────

func TestLifecycle_EndToEnd(t *testing.T) {
	e := newEnv(t)
	q := e.create(pPreparer, map[string]any{"discount_percent": "5", "assumptions": "Excludes travel", "reason_code": "NEW_OPPORTUNITY", "evidence_id": "OPP-1"})
	require.Equal(t, StateDraft, q.State)
	require.Equal(t, 1, q.Version)
	require.Equal(t, 1, q.CurrentVersionNo)
	require.Equal(t, "Q-000001", q.QuoteNumber)
	require.Equal(t, pPreparer, q.CreatedBy)
	v := q.Current
	require.Len(t, v.Lines, 2)
	require.Equal(t, kit.MustAmount("1000"), v.Lines[0].LineNet)
	require.Equal(t, kit.MustAmount("1250.50"), v.Subtotal)
	require.Equal(t, kit.MustAmount("62.53"), v.DiscountAmount, "62.525 rounds half-up")
	require.Equal(t, kit.MustAmount("1187.97"), v.NetTotal)
	require.Equal(t, "PRICELIST-2026-Q4", v.PriceSourceRef)
	require.Len(t, v.ContentHash, 64)
	require.Equal(t, 1, e.events(q.QuoteID, EventQuoteCreated))

	// A second quote in the same entity gets the next number.
	require.Equal(t, "Q-000002", e.create(pPreparer, nil).QuoteNumber)

	// Illegal jumps are refused before the proper path is followed.
	code, body := e.mk(pPreparer, "sendQuote", q, nil)
	require.Equal(t, http.StatusConflict, code, string(body))
	require.Equal(t, kit.CodeInvalidTransition, errCode(t, body))
	code, body = e.mk(pBoss, "approveQuote", q, map[string]any{"version_no": 1})
	require.Equal(t, http.StatusConflict, code, "a draft cannot be approved before submission: "+string(body))
	require.Equal(t, kit.CodeInvalidTransition, errCode(t, body))

	q = e.submit(q)
	require.Equal(t, StatePendingApproval, q.State)
	require.Equal(t, pPreparer, q.SubmittedBy)
	q = e.approveAs(pApprover, q)
	require.Equal(t, StateApproved, q.State)
	require.Equal(t, pApprover, q.ApprovedBy)
	require.Equal(t, v.ContentHash, q.ApprovedContentHash)
	require.Equal(t, 1, q.ApprovedVersionNo)
	q = e.send(q)
	require.Equal(t, StateSent, q.State)
	require.NotNil(t, q.SentAt)
	q = e.accept(q)
	require.Equal(t, StateAccepted, q.State)
	require.Equal(t, "Jane Customer", q.AcceptedBy)
	require.Equal(t, "EV-ACC-1", q.AcceptanceEvidenceID)
	require.Equal(t, "SIGNED_DOCUMENT", q.AcceptanceChannel)

	require.Zero(t, e.orders.calls, "nothing is ordered before conversion")
	q = e.step(pConverter, "convertQuoteToOrder", q, nil)
	require.Equal(t, StateConverted, q.State)
	require.NotEmpty(t, q.ConvertedOrderID)
	require.Equal(t, 1, e.orders.calls)
	in := e.orders.last
	require.Equal(t, q.QuoteID, in.QuoteID)
	require.Equal(t, 1, in.VersionNo)
	require.Equal(t, v.ContentHash, in.ContentHash)
	require.Equal(t, kit.MustAmount("1187.97"), in.NetTotal)
	require.Len(t, in.Lines, 2)
	require.Equal(t, fmt.Sprintf("quote:%s:v1", q.QuoteID), in.Ref)
	require.Equal(t, q.CustomerPartyID, in.CustomerPartyID)
	require.Equal(t, "USD", in.Currency)

	// Evidence: one event per spec event, an audit + history row per command.
	for _, et := range []string{EventQuoteCreated, EventQuoteApproved, EventQuoteSent, EventQuoteAccepted, EventQuoteConverted} {
		require.Equal(t, 1, e.events(q.QuoteID, et), et)
	}
	require.Zero(t, e.events(q.QuoteID, EventQuoteRejected)+e.events(q.QuoteID, EventQuoteExpired))
	require.Equal(t, 6, e.audits(q.QuoteID)) // create, submit, approve, send, accept, convert
	code, body = e.query(pNobody, "getQuoteHistory", "quote_id="+q.QuoteID)
	require.Equal(t, http.StatusOK, code, string(body))
	var hs struct {
		Items     []HistoryEntry   `json:"items"`
		Versions  []VersionSummary `json:"versions"`
		Approvals []ApprovalRecord `json:"approvals"`
	}
	require.NoError(t, jsonDecode(body, &hs))
	require.Len(t, hs.Items, 6)
	cmds := []string{"createQuote", "submitQuoteForApproval", "approveQuote", "sendQuote", "recordAcceptance", "convertQuoteToOrder"}
	for i, it := range hs.Items {
		require.Equal(t, cmds[i], it.Command)
		require.Equal(t, i+1, it.QuoteVersion)
	}
	require.Equal(t, "OPP-1", hs.Items[0].EvidenceID)
	require.Equal(t, pApprover, hs.Items[2].ApproverID)
	require.Equal(t, "EV-ACC-1", hs.Items[4].EvidenceID)
	require.Equal(t, StateConverted, hs.Items[5].StateAfter)
	require.Len(t, hs.Versions, 1)
	require.Len(t, hs.Approvals, 1)
	require.Equal(t, pPreparer, hs.Approvals[0].PreparedBy)

	// The quote is NON-POSTING: no posting request, and only ar.quote.* events leave the module.
	require.Zero(t, e.count(`SELECT count(*) FROM ar_accounting_posting_requests WHERE tenant_id = $1`, e.tenant))
	require.Zero(t, e.count(`SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type NOT LIKE 'ar.quote.%'`, e.tenant))
	require.Zero(t, e.tax.calls, "no tax estimate was requested, so the tax service was never called")
	require.Equal(t, q.Version, e.get(q.QuoteID).Version)
}

func TestRejectAndExpiredLifecycles(t *testing.T) {
	e := newEnv(t)
	q := e.sent(nil)
	code, body := e.mk(pPreparer, "rejectQuote", q, nil)
	require.Equal(t, http.StatusBadRequest, code)
	require.Equal(t, kit.CodeReasonRequired, errCode(t, body))
	q = e.step(pPreparer, "rejectQuote", q, map[string]any{"reason_code": "PRICE_TOO_HIGH", "evidence_id": "MAIL-9"})
	require.Equal(t, StateRejected, q.State)
	require.Equal(t, "PRICE_TOO_HIGH", q.RejectionReason)
	require.Equal(t, 1, e.events(q.QuoteID, EventQuoteRejected))
	for _, name := range []string{"sendQuote", "submitQuoteForApproval", "convertQuoteToOrder"} {
		principal := pPreparer
		if name == "convertQuoteToOrder" {
			principal = pConverter
		}
		code, body = e.mk(principal, name, q, nil)
		require.Equal(t, http.StatusConflict, code, name+string(body))
		require.Equal(t, kit.CodeInvalidTransition, errCode(t, body), name)
	}
	code, _ = e.mk(pPreparer, "amendQuote", q, map[string]any{"terms": "x"})
	require.Equal(t, http.StatusConflict, code, "a rejected quote is final")

	// Reject from pending approval too.
	p := e.submit(e.create(pPreparer, nil))
	p = e.step(pPreparer, "rejectQuote", p, map[string]any{"reason_code": "WITHDRAWN"})
	require.Equal(t, StateRejected, p.State)
}

// ── Neg #1 (spec table) / Neg #5 (§18): approval cannot survive a material change ──

func TestNeg01_ApprovalNotReusedAfterMaterialPriceChange(t *testing.T) {
	e := newEnv(t)
	q := e.approved(nil)
	oldHash, oldNet := q.ApprovedContentHash, q.Current.NetTotal
	require.Equal(t, StateApproved, q.State)

	// A material price change creates version 2 and returns the quote to DRAFT.
	q = e.step(pPreparer, "amendQuote", q, map[string]any{"lines": []map[string]any{
		line("Platform licence", "2", "600.00"), line("Onboarding", "1", "250.50")}})
	require.Equal(t, StateDraft, q.State)
	require.Equal(t, 2, q.CurrentVersionNo)
	require.Empty(t, q.ApprovedContentHash, "the approval is cleared: it was bound to the old content")
	require.Zero(t, q.ApprovedVersionNo)
	require.NotEqual(t, oldHash, q.Current.ContentHash)
	require.NotEqual(t, oldNet, q.Current.NetTotal)
	require.Equal(t, 1, e.approvals(q.QuoteID), "the old approval row stays as history; it approves nothing now")

	// The old approval cannot be used to send...
	code, body := e.mk(pPreparer, "sendQuote", q, nil)
	require.Equal(t, http.StatusConflict, code, string(body))
	require.Equal(t, kit.CodeInvalidTransition, errCode(t, body))
	// ...nor re-asserted against the old version once resubmitted...
	q = e.submit(q)
	code, body = e.mk(pApprover, "approveQuote", q, map[string]any{"version_no": 1})
	require.Equal(t, http.StatusConflict, code, string(body))
	require.Equal(t, CodeApprovalInvalidated, errCode(t, body))
	code, body = e.mk(pApprover, "approveQuote", q, map[string]any{"version_no": 2, "content_hash": oldHash})
	require.Equal(t, http.StatusConflict, code, string(body))
	require.Equal(t, CodeApprovalInvalidated, errCode(t, body))
	require.Equal(t, StatePendingApproval, e.get(q.QuoteID).State)
	require.Equal(t, 1, e.approvals(q.QuoteID))

	// ...a fresh approval of version 2 is required, and binds to the new hash.
	q = e.approveAs(pApprover, q)
	require.Equal(t, 2, q.ApprovedVersionNo)
	require.Equal(t, q.Current.ContentHash, q.ApprovedContentHash)
	require.Equal(t, 2, e.approvals(q.QuoteID))
	require.Equal(t, 2, e.events(q.QuoteID, EventQuoteApproved))

	// Backstop: even if a stale approval were forced into the row (tampering / a bug),
	// sending it is refused because the approved hash is not the current content hash.
	r := e.approved(nil)
	oldR := r.ApprovedContentHash
	r = e.step(pPreparer, "amendQuote", r, map[string]any{"discount_percent": "7"})
	require.Equal(t, StateDraft, r.State)
	_, err := pool.Exec(context.Background(), `UPDATE ar02_quotes SET state = 'APPROVED', approved_version_no = 1, approved_content_hash = $2 WHERE quote_id = $1`, r.QuoteID, oldR)
	require.NoError(t, err)
	r = e.get(r.QuoteID)
	code, body = e.mk(pPreparer, "sendQuote", r, nil)
	require.Equal(t, http.StatusConflict, code, string(body))
	require.Equal(t, CodeApprovalInvalidated, errCode(t, body))
	require.Equal(t, StateApproved, e.get(r.QuoteID).State, "refusal changes nothing")
	require.Zero(t, e.events(r.QuoteID, EventQuoteSent))
	// An approval-dependent state with no approval is impossible in the data.
	_, err = pool.Exec(context.Background(), `UPDATE ar02_quotes SET approved_content_hash = NULL WHERE quote_id = $1`, r.QuoteID)
	require.ErrorContains(t, err, "ar02_state_requires_approval")
}

func TestNeg05_MaterialQuoteChangeAfterApprovalNeedsReapproval(t *testing.T) {
	e := newEnv(t)
	cases := map[string]map[string]any{
		"quantity":            {"lines": []map[string]any{line("Platform licence", "3", "500.00"), line("Onboarding", "1", "250.50")}},
		"unit price":          {"lines": []map[string]any{line("Platform licence", "2", "500.01"), line("Onboarding", "1", "250.50")}},
		"added line":          {"lines": []map[string]any{line("Platform licence", "2", "500.00"), line("Onboarding", "1", "250.50"), line("Extra", "1", "1")}},
		"discount":            {"discount_percent": "6"},
		"terms":               {"terms": "Net 90"},
		"price source":        {"price_source_ref": "PRICELIST-2026-Q5"},
		"validity extension":  {"validity_date": validityIn(60)},
		"currency":            {"currency": "EUR"},
		"tax estimate toggle": {"request_tax_estimate": true, "tax_params": map[string]any{"jurisdiction_id": "GB", "tax_category": "STANDARD", "supply_kind": "SERVICES", "supply_type": "B2B"}},
	}
	for name, change := range cases {
		for _, stage := range []string{"approved", "sent"} {
			var q *Quote
			if stage == "approved" {
				q = e.approved(nil)
			} else {
				q = e.sent(nil)
			}
			a := e.step(pPreparer, "amendQuote", q, change)
			require.Equal(t, StateDraft, a.State, name+"/"+stage)
			require.Equal(t, 2, a.CurrentVersionNo, name)
			require.Empty(t, a.ApprovedContentHash, name)
			require.Nil(t, a.SentAt, name)
			require.True(t, a.Current.MaterialChange, name)
			// It cannot go out, be accepted or converted without a new approval cycle.
			for _, c := range []struct {
				who, cmd string
				body     map[string]any
			}{{pPreparer, "sendQuote", nil}, {pPreparer, "recordAcceptance", acceptBody(a)}, {pConverter, "convertQuoteToOrder", nil}} {
				code, body := e.mk(c.who, c.cmd, a, c.body)
				require.Equal(t, http.StatusConflict, code, name+"/"+stage+" "+c.cmd+" "+string(body))
				require.Equal(t, kit.CodeInvalidTransition, errCode(t, body))
			}
			require.Equal(t, 0, e.events(a.QuoteID, EventQuoteAccepted), name)
		}
	}

	// Non-material changes keep the approval (and say so in history).
	q := e.approved(nil)
	oldHash := q.ApprovedContentHash
	q = e.step(pPreparer, "amendQuote", q, map[string]any{"assumptions": "Revised assumption"})
	require.Equal(t, StateApproved, q.State)
	require.Equal(t, 2, q.CurrentVersionNo)
	require.Equal(t, oldHash, q.ApprovedContentHash, "assumptions are outside the approval-binding hash")
	require.Equal(t, 2, q.ApprovedVersionNo, "approval carried forward to the new version")
	require.False(t, q.Current.MaterialChange)
	q = e.send(q)
	q = e.step(pPreparer, "amendQuote", q, map[string]any{"validity_date": validityIn(10)}) // shortening
	require.Equal(t, StateSent, q.State)
	require.Equal(t, 3, q.CurrentVersionNo)
	require.NotEqual(t, oldHash, q.ApprovedContentHash, "the carried approval now binds to the shortened-validity content")
	require.Equal(t, q.Current.ContentHash, q.ApprovedContentHash)

	// An amendment that changes nothing is refused; final states cannot be amended.
	code, body := e.mk(pPreparer, "amendQuote", q, map[string]any{"terms": q.Current.Terms})
	require.Equal(t, http.StatusBadRequest, code, string(body))
	acc := e.accept(q)
	code, body = e.mk(pPreparer, "amendQuote", acc, map[string]any{"terms": "x"})
	require.Equal(t, http.StatusConflict, code, string(body))
	require.Equal(t, kit.CodeInvalidTransition, errCode(t, body))

	// Currency change recomputes line nets exactly with the new currency's minor unit.
	j := e.create(pPreparer, map[string]any{"discount_percent": "5"})
	j = e.step(pPreparer, "amendQuote", j, map[string]any{"currency": "JPY"})
	require.Equal(t, "JPY", j.Currency)
	require.Equal(t, kit.MustAmount("251"), j.Current.Lines[1].LineNet, "250.50 JPY rounds half-up to 251")
	require.Equal(t, kit.MustAmount("1251"), j.Current.Subtotal)
	require.Equal(t, kit.MustAmount("63"), j.Current.DiscountAmount)
	require.Equal(t, kit.MustAmount("1188"), j.Current.NetTotal)
	// ...and the history records the invalidated approval.
	r := e.approved(map[string]any{"discount_percent": "5"})
	r = e.step(pPreparer, "amendQuote", r, map[string]any{"discount_percent": "8"})
	_, body = e.query(pNobody, "getQuoteHistory", "quote_id="+r.QuoteID)
	var hs struct {
		Items []HistoryEntry `json:"items"`
	}
	require.NoError(t, jsonDecode(body, &hs))
	last := hs.Items[len(hs.Items)-1]
	require.Equal(t, "amendQuote", last.Command)
	require.Equal(t, true, last.NewValues["approval_invalidated"])
	require.Equal(t, true, last.NewValues["material_change"])
	require.Equal(t, "5.0000", last.PriorValues["discount_percent"])
	require.Equal(t, "8.0000", last.NewValues["discount_percent"])
}

// ── Neg #2 (spec table) / Neg #4 (§18): expired quote converted ──────────────

func TestNeg02_ExpiredQuoteCannotBeConverted(t *testing.T) {
	e := newEnv(t)
	q := e.accepted(map[string]any{"validity_date": e.dayOf(5)})
	require.Equal(t, StateAccepted, q.State)

	e.offset = 10 * 24 * time.Hour // the validity has now lapsed
	code, body := e.mk(pConverter, "convertQuoteToOrder", q, nil)
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	require.Equal(t, kit.CodeQuoteExpired, errCode(t, body))
	after := e.get(q.QuoteID)
	require.Equal(t, StateAccepted, after.State, "the refusal preserves the safe state")
	require.Equal(t, q.Version, after.Version)
	require.Zero(t, e.orders.calls, "no order creation was attempted")
	require.Zero(t, e.orderRows())
	require.Zero(t, e.events(q.QuoteID, EventQuoteConverted))

	// An expired quote cannot be sent or accepted either.
	b := e.sent(map[string]any{"validity_date": e.dayOf(3)})
	c := e.approved(map[string]any{"validity_date": e.dayOf(3)})
	e.offset = 20 * 24 * time.Hour
	code, body = e.mk(pPreparer, "sendQuote", c, nil)
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	require.Equal(t, kit.CodeQuoteExpired, errCode(t, body))
	code, body = e.mk(pPreparer, "recordAcceptance", b, acceptBody(b))
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	require.Equal(t, kit.CodeQuoteExpired, errCode(t, body))
	code, body = e.mk(pPreparer, "amendQuote", b, map[string]any{"terms": "late edit"})
	require.Equal(t, http.StatusUnprocessableEntity, code, "amending an elapsed quote without extending it is QUOTE_EXPIRED: "+string(body))
	require.Equal(t, kit.CodeQuoteExpired, errCode(t, body))
	code, body = e.mk(pPreparer, "amendQuote", b, map[string]any{"validity_date": e.dayOf(-1)})
	require.Equal(t, http.StatusBadRequest, code, string(body))

	// ExpireQuote records the lapse (and only when the validity really elapsed).
	e.offset = 10 * 24 * time.Hour
	q = e.step(pPreparer, "expireQuote", q, nil)
	require.Equal(t, StateExpired, q.State)
	require.NotNil(t, q.ExpiredAt)
	require.Equal(t, 1, e.events(q.QuoteID, EventQuoteExpired))
	code, body = e.mk(pConverter, "convertQuoteToOrder", q, nil)
	require.Equal(t, http.StatusConflict, code, string(body))
	require.Equal(t, kit.CodeInvalidTransition, errCode(t, body))
	e.offset = 0
	fresh := e.sent(nil)
	code, body = e.mk(pPreparer, "expireQuote", fresh, nil)
	require.Equal(t, http.StatusUnprocessableEntity, code, "validity has not elapsed: "+string(body))
	require.Zero(t, e.events(fresh.QuoteID, EventQuoteExpired))
}

// ── Neg #3 (spec table): unauthorized discount approval ──────────────────────

func TestNeg03_UnauthorizedDiscountApproval(t *testing.T) {
	e := newEnv(t)
	disc := map[string]any{"discount_percent": "15"}

	// A plain approver (no discount authority) cannot approve a 15% discount.
	q := e.submit(e.create(pPreparer, disc))
	code, body := e.mk(pApprover, "approveQuote", q, map[string]any{"version_no": 1})
	require.Equal(t, http.StatusForbidden, code, string(body))
	require.Equal(t, kit.CodeForbidden, errCode(t, body))
	require.Equal(t, StatePendingApproval, e.get(q.QuoteID).State)
	require.Zero(t, e.approvals(q.QuoteID))

	// A principal who holds every authority still cannot approve their own discount.
	b := e.create(pBoss, disc)
	b = e.step(pBoss, "submitQuoteForApproval", b, nil)
	code, body = e.mk(pBoss, "approveQuote", b, map[string]any{"version_no": 1})
	require.Equal(t, http.StatusForbidden, code, string(body))
	require.Equal(t, kit.CodeSegregationOfDuties, errCode(t, body))
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar_audit_events WHERE tenant_id = $1 AND aggregate_id = $2 AND command = 'approveQuote.denied'`, e.tenant, b.QuoteID),
		"the refused self-approval leaves evidence")
	require.Equal(t, StatePendingApproval, e.get(b.QuoteID).State)
	require.Zero(t, e.events(b.QuoteID, EventQuoteApproved))
	// The submitter counts as preparer too: boss submits, someone else created.
	c := e.create(pPreparer, disc)
	c = e.step(pBoss, "submitQuoteForApproval", e.get(c.QuoteID), nil) // boss holds QUOTE_MANAGE
	code, body = e.mk(pBoss, "approveQuote", c, map[string]any{"version_no": 1})
	require.Equal(t, http.StatusForbidden, code, string(body))
	require.Equal(t, kit.CodeSegregationOfDuties, errCode(t, body))

	// A different principal holding the discount authority approves it.
	q = e.approveAs(pDiscApprover, q)
	require.Equal(t, StateApproved, q.State)
	var needed bool
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT requires_discount_authority FROM ar02_quote_approvals WHERE quote_id = $1`, q.QuoteID).Scan(&needed))
	require.True(t, needed)

	// Within the threshold (<= 10%) the ordinary approve action suffices, even for the preparer.
	at10 := e.submit(e.create(pPreparer, map[string]any{"discount_percent": "10"}))
	require.Equal(t, StateApproved, e.approveAs(pApprover, at10).State, "exactly 10% is within the preparer's own authority")
	self := e.step(pBoss, "submitQuoteForApproval", e.create(pBoss, map[string]any{"discount_percent": "9.9999"}), nil)
	require.Equal(t, StateApproved, e.approveAs(pBoss, self).State)
	just := e.submit(e.create(pPreparer, map[string]any{"discount_percent": "10.0001"}))
	code, _ = e.mk(pApprover, "approveQuote", just, map[string]any{"version_no": 1})
	require.Equal(t, http.StatusForbidden, code, "10.0001% is above the threshold")

	// Raising the discount after approval is a material change: back to DRAFT and the authority is needed again.
	r := e.approved(map[string]any{"discount_percent": "4"})
	r = e.step(pPreparer, "amendQuote", r, map[string]any{"discount_percent": "20"})
	require.Equal(t, StateDraft, r.State)
	r = e.submit(r)
	code, _ = e.mk(pApprover, "approveQuote", r, map[string]any{"version_no": 2})
	require.Equal(t, http.StatusForbidden, code)
	require.Equal(t, StateApproved, e.approveAs(pDiscApprover, r).State)

	// The data refuses it too: an approval above authority by the preparer cannot be stored.
	_, err := pool.Exec(context.Background(), `INSERT INTO ar02_quote_approvals (approval_id, tenant_id, legal_entity_id, quote_id, version_no, content_hash,
		discount_percent, requires_discount_authority, prepared_by, approved_by) VALUES ($1,$2,$3,$4,1,'h',15,true,'same','same')`,
		uuid.NewString(), e.tenant, e.entity, q.QuoteID)
	require.ErrorContains(t, err, "ar02_approval_sod")

	// No approve permission at all, or no authority to manage: 403.
	s := e.submit(e.create(pPreparer, nil))
	code, _ = e.mk(pNobody, "approveQuote", s, map[string]any{"version_no": 1})
	require.Equal(t, http.StatusForbidden, code)
	code, _ = e.mk(pApprover, "submitQuoteForApproval", e.create(pPreparer, nil), nil)
	require.Equal(t, http.StatusForbidden, code)
}

// ── Neg #4 (spec table): acceptance recorded without evidence ────────────────

func TestNeg04_AcceptanceRecordedWithoutEvidence(t *testing.T) {
	e := newEnv(t)
	q := e.sent(nil)
	for name, mutate := range map[string]func(map[string]any){
		"no evidence":    func(b map[string]any) { delete(b, "evidence_id") },
		"blank evidence": func(b map[string]any) { b["evidence_id"] = "  " },
	} {
		b := acceptBody(q)
		mutate(b)
		code, body := e.mk(pPreparer, "recordAcceptance", q, b)
		require.Equal(t, http.StatusBadRequest, code, name)
		require.Equal(t, kit.CodeEvidenceRequired, errCode(t, body), name)
	}
	after := e.get(q.QuoteID)
	require.Equal(t, StateSent, after.State)
	require.Equal(t, q.Version, after.Version)
	require.Zero(t, e.events(q.QuoteID, EventQuoteAccepted))

	// The data refuses a fabricated acceptance as well: evidence, signatory and channel are required.
	for _, set := range []string{
		`state = 'ACCEPTED'`,
		`state = 'ACCEPTED', accepted_by = 'x', acceptance_channel = 'EMAIL'`,
		`state = 'ACCEPTED', acceptance_evidence_id = 'E', acceptance_channel = 'EMAIL'`,
	} {
		_, err := pool.Exec(context.Background(), `UPDATE ar02_quotes SET `+set+` WHERE quote_id = $1`, q.QuoteID)
		require.ErrorContains(t, err, "ar02_acceptance_requires_evidence", set)
	}
	// An unauthorised principal cannot record it even with evidence.
	code, _ := e.mk(pNobody, "recordAcceptance", q, acceptBody(q))
	require.Equal(t, http.StatusForbidden, code)

	// Date sanity: before the send, or after the validity, is refused.
	b := acceptBody(q)
	b["accepted_at"] = e.now().AddDate(0, 0, -3).Format(time.RFC3339)
	code, body := e.mk(pPreparer, "recordAcceptance", q, b)
	require.Equal(t, http.StatusBadRequest, code, string(body))

	// With evidence it works and records who/how/evidence + emits the event with them.
	q = e.accept(q)
	require.Equal(t, StateAccepted, q.State)
	require.Equal(t, 1, e.events(q.QuoteID, EventQuoteAccepted))
	var evID string
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT payload->'payload'->>'evidence_id' FROM outbox_events WHERE aggregate_id = $1 AND event_type = $2`,
		q.QuoteID, EventQuoteAccepted).Scan(&evID))
	require.Equal(t, "EV-ACC-1", evID)

	// Acceptance names a version: one that is not the approved/sent content is superseded.
	s := e.sent(nil)
	s2 := e.step(pPreparer, "amendQuote", s, map[string]any{"validity_date": validityIn(10)}) // non-material shortening: new hash, approval carried
	require.Equal(t, StateSent, s2.State)
	b = acceptBody(s2)
	b["version_no"] = 1
	code, body = e.mk(pPreparer, "recordAcceptance", s2, b)
	require.Equal(t, http.StatusConflict, code, string(body))
	require.Equal(t, CodeVersionSuperseded, errCode(t, body))
	b["version_no"] = 99
	code, _ = e.mk(pPreparer, "recordAcceptance", s2, b)
	require.Equal(t, http.StatusNotFound, code)
}

// ── immutability, append-only evidence, schema invariants ────────────────────

func TestSupersededVersionsAreImmutableAndPreserved(t *testing.T) {
	e := newEnv(t)
	q := e.approved(map[string]any{"request_tax_estimate": true,
		"tax_params": map[string]any{"jurisdiction_id": "GB", "tax_category": "STANDARD", "supply_kind": "SERVICES", "supply_type": "B2B"}})
	require.NotNil(t, q.Current.TaxEstimate)
	v1hash, v1net := q.Current.ContentHash, q.Current.NetTotal
	q = e.step(pPreparer, "amendQuote", q, map[string]any{"lines": []map[string]any{line("Platform licence", "9", "500.00")}})
	require.Equal(t, 2, q.CurrentVersionNo)

	// Version 1 is still there, byte for byte, with its lines and hash.
	code, body := e.query(pNobody, "getQuoteVersion", "quote_id="+q.QuoteID+"&version_no=1")
	require.Equal(t, http.StatusOK, code, string(body))
	old := asQuote(t, body)
	require.Equal(t, 1, old.Current.VersionNo)
	require.Equal(t, v1hash, old.Current.ContentHash)
	require.Equal(t, v1net, old.Current.NetTotal)
	require.Len(t, old.Current.Lines, 2)
	require.Equal(t, kit.MustAmount("2"), old.Current.Lines[0].Quantity)
	require.NotNil(t, old.Current.TaxEstimate)
	code, body = e.query(pNobody, "getQuoteVersion", "quote_id="+q.QuoteID)
	require.Equal(t, 2, asQuote(t, body).Current.VersionNo, "default is the current version")
	code, _ = e.query(pNobody, "getQuoteVersion", "quote_id="+q.QuoteID+"&version_no=3")
	require.Equal(t, http.StatusNotFound, code)

	ctx := context.Background()
	for _, stmt := range []string{
		`UPDATE ar02_quote_versions SET net_total = 1 WHERE quote_id = $1 AND version_no = 1`,
		`UPDATE ar02_quote_versions SET content_hash = 'x' WHERE quote_id = $1`,
		`DELETE FROM ar02_quote_versions WHERE quote_id = $1 AND version_no = 1`,
		`UPDATE ar02_quote_lines SET unit_price = 1 WHERE quote_id = $1 AND version_no = 1`,
		`DELETE FROM ar02_quote_lines WHERE quote_id = $1`,
		`UPDATE ar02_quote_approvals SET approved_by = 'x' WHERE quote_id = $1`,
		`DELETE FROM ar02_quote_approvals WHERE quote_id = $1`,
		`UPDATE ar02_quote_tax_estimates SET tax_amount = 0 WHERE quote_id = $1`,
		`DELETE FROM ar02_quote_tax_estimates WHERE quote_id = $1`,
		`UPDATE ar02_quote_history SET command = 'x' WHERE quote_id = $1`,
		`DELETE FROM ar02_quote_history WHERE quote_id = $1`,
	} {
		_, err := pool.Exec(ctx, stmt, q.QuoteID)
		require.ErrorContains(t, err, "append-only", stmt)
	}
	// Versions are recorded strictly in sequence, with consistent totals.
	ins := func(vno int, net string) error {
		_, err := pool.Exec(ctx, `INSERT INTO ar02_quote_versions (quote_id, version_no, tenant_id, legal_entity_id, currency, validity_date, subtotal, discount_amount, net_total,
			content_hash, change_kind, created_by) VALUES ($1,$2,$3,$4,'USD',now()::date,10,0,$5::numeric,'h','AMEND','sql')`, q.QuoteID, vno, e.tenant, e.entity, net)
		return err
	}
	require.ErrorContains(t, ins(5, "10"), "AR02_VERSION_SEQUENCE")
	require.ErrorContains(t, ins(1, "10"), "AR02_VERSION_SEQUENCE")
	require.ErrorContains(t, ins(3, "9"), "ar02_totals_consistent")
	require.NoError(t, ins(3, "10"))
	// Content constraints.
	_, err := pool.Exec(ctx, `INSERT INTO ar02_quote_lines (quote_id, version_no, line_no, tenant_id, description, quantity, unit_price, line_net)
		VALUES ($1,3,9,$2,'x',0,1,0)`, q.QuoteID, e.tenant)
	require.Error(t, err, "a zero-quantity line is refused")
	_, err = pool.Exec(ctx, `UPDATE ar02_quotes SET state = 'CONVERTED' WHERE quote_id = $1`, q.QuoteID)
	require.Error(t, err, "a converted quote needs an order reference and acceptance")
	_, err = pool.Exec(ctx, `UPDATE ar02_quotes SET state = 'BOGUS' WHERE quote_id = $1`, q.QuoteID)
	require.Error(t, err)
}

// ── command plumbing: concurrency, idempotency, isolation, atomicity ─────────

func TestOptimisticConcurrency(t *testing.T) {
	e := newEnv(t)
	q := e.create(pPreparer, nil)
	b := qbody(q, nil)
	b["expected_version"] = q.Version - 1
	code, body := e.cmd(pPreparer, "submitQuoteForApproval", b)
	require.Equal(t, http.StatusConflict, code)
	require.Equal(t, kit.CodeStaleVersion, errCode(t, body))
	delete(b, "expected_version")
	code, body = e.cmd(pPreparer, "submitQuoteForApproval", b)
	require.Equal(t, http.StatusBadRequest, code)
	require.Equal(t, kit.CodeExpectedVersionMissing, errCode(t, body))
	require.Equal(t, StateDraft, e.get(q.QuoteID).State)

	// Racing identical approvals: exactly one wins, the rest are stale; one approval, one event.
	p := e.submit(q)
	var wg sync.WaitGroup
	codes := make([]int, 6)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _, _ = call(t, e.h, http.MethodPost, base+"commands/approveQuote", e.tenant, pApprover, "", qbody(p, map[string]any{"version_no": 1}))
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
	require.Equal(t, 1, e.approvals(p.QuoteID))
	require.Equal(t, 1, e.events(p.QuoteID, EventQuoteApproved))

	// Racing amendments at one version: one new version, never two version 2 rows.
	r := e.create(pPreparer, nil)
	codes = make([]int, 6)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _, _ = call(t, e.h, http.MethodPost, base+"commands/amendQuote", e.tenant, pPreparer, "", qbody(r, map[string]any{"terms": fmt.Sprintf("terms %d", i)}))
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
	require.Equal(t, 2, e.count(`SELECT count(*) FROM ar02_quote_versions WHERE quote_id = $1`, r.QuoteID))
}

func TestIdempotentReplayAndConversionLinkage(t *testing.T) {
	e := newEnv(t)
	body := createBody(e.entity, uuid.NewString())
	key := uuid.NewString()
	c1, b1, _ := call(t, e.h, http.MethodPost, base+"commands/createQuote", e.tenant, pPreparer, key, body)
	require.Equal(t, http.StatusCreated, c1, string(b1))
	c2, b2, h2 := call(t, e.h, http.MethodPost, base+"commands/createQuote", e.tenant, pPreparer, key, body)
	require.Equal(t, http.StatusCreated, c2)
	require.Equal(t, "true", h2.Get("Idempotent-Replay"))
	require.JSONEq(t, string(b1), string(b2))
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar02_quotes WHERE tenant_id = $1`, e.tenant))
	require.Equal(t, 1, e.count(`SELECT count(*) FROM outbox_events WHERE tenant_id = $1`, e.tenant))
	c3, b3, _ := call(t, e.h, http.MethodPost, base+"commands/createQuote", e.tenant, pPreparer, key, createBody(e.entity, uuid.NewString()))
	require.Equal(t, http.StatusUnprocessableEntity, c3)
	require.Equal(t, kit.CodeIdempotencyReused, errCode(t, b3))

	// A replayed state change does not apply twice.
	q := asQuote(t, b1)
	k2 := uuid.NewString()
	call(t, e.h, http.MethodPost, base+"commands/submitQuoteForApproval", e.tenant, pPreparer, k2, qbody(q, nil))
	c4, _, h4 := call(t, e.h, http.MethodPost, base+"commands/submitQuoteForApproval", e.tenant, pPreparer, k2, qbody(q, nil))
	require.Equal(t, http.StatusOK, c4)
	require.Equal(t, "true", h4.Get("Idempotent-Replay"))
	require.Equal(t, 2, e.get(q.QuoteID).Version)

	// Conversion is idempotent: a second convert returns the original linkage, never a second order.
	a := e.accepted(nil)
	conv := e.step(pConverter, "convertQuoteToOrder", a, nil)
	orderID := conv.ConvertedOrderID
	require.NotEmpty(t, orderID)
	again := e.step(pConverter, "convertQuoteToOrder", a, nil) // new Idempotency-Key, stale expected_version
	require.Equal(t, orderID, again.ConvertedOrderID)
	require.Equal(t, conv.Version, again.Version, "the replayed conversion changes nothing")
	again = e.step(pConverter, "convertQuoteToOrder", conv, nil) // current version
	require.Equal(t, orderID, again.ConvertedOrderID)
	require.Equal(t, 1, e.orders.calls, "the order creator ran exactly once")
	require.Equal(t, 1, e.orderRows())
	require.Equal(t, 1, e.events(conv.QuoteID, EventQuoteConverted))
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar_audit_events WHERE tenant_id = $1 AND aggregate_id = $2 AND command = 'convertQuoteToOrder'`, e.tenant, conv.QuoteID))

	// Concurrent conversions with different keys still produce one order.
	b := e.accepted(nil)
	var wg sync.WaitGroup
	codes := make([]int, 5)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _, _ = call(t, e.h, http.MethodPost, base+"commands/convertQuoteToOrder", e.tenant, pConverter, "", qbody(b, nil))
		}(i)
	}
	wg.Wait()
	for _, c := range codes {
		require.Equal(t, http.StatusOK, c)
	}
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar02_test_orders WHERE quote_ref = $1`, fmt.Sprintf("quote:%s:v1", b.QuoteID)))
	require.Equal(t, 1, e.events(b.QuoteID, EventQuoteConverted))
}

func TestAtomicity_FailedConversionLeavesNoOrderNoEvidence(t *testing.T) {
	e := newEnv(t)
	q := e.accepted(nil)
	beforeAudit := e.audits(q.QuoteID)

	// The order creator writes its row and then fails: the quote must not convert and the order must vanish.
	e.orders.err = errors.New("order service exploded")
	code, body := e.mk(pConverter, "convertQuoteToOrder", q, nil)
	require.Equal(t, http.StatusServiceUnavailable, code, string(body))
	require.Equal(t, kit.CodeDependencyUnavailable, errCode(t, body))
	require.Zero(t, e.orderRows(), "the order row rolled back with the quote conversion")
	after := e.get(q.QuoteID)
	require.Equal(t, StateAccepted, after.State)
	require.Equal(t, q.Version, after.Version)
	require.Zero(t, e.events(q.QuoteID, EventQuoteConverted))
	require.Equal(t, beforeAudit, e.audits(q.QuoteID))
	require.Equal(t, 1, e.orders.calls)

	// A failed command released its idempotency claim: the retry succeeds once the creator recovers.
	e.orders.err = nil
	conv := e.step(pConverter, "convertQuoteToOrder", q, nil)
	require.Equal(t, StateConverted, conv.State)
	require.Equal(t, 1, e.orderRows())

	// With AR-03 not wired at all: 503, state untouched, nothing else changed.
	r := e.accepted(nil)
	e.m.orders = nil
	code, body = e.mk(pConverter, "convertQuoteToOrder", r, nil)
	require.Equal(t, http.StatusServiceUnavailable, code, string(body))
	require.Contains(t, string(body), "AR-03 sales order service not wired")
	require.Equal(t, StateAccepted, e.get(r.QuoteID).State)
	require.Equal(t, r.Version, e.get(r.QuoteID).Version)
	e.m.orders = e.orders

	// Command-level atomicity of the evidence helpers: a rolled-back commit leaves nothing behind.
	s := e.create(pPreparer, nil)
	ctx := context.Background()
	c := kit.Caller{TenantID: e.tenant, PrincipalID: pPreparer, LegalEntityID: e.entity}
	beforeHist := e.count(`SELECT count(*) FROM ar02_quote_history WHERE quote_id = $1`, s.QuoteID)
	beforeAudit = e.audits(s.QuoteID)
	err := kit.NewDB(pool).WithTx(ctx, e.tenant, func(tx pgx.Tx) error {
		cur, err := getHeader(ctx, tx, e.tenant, s.QuoteID, true)
		require.NoError(t, err)
		nh := *cur
		nh.State = StatePendingApproval
		_, err = commit(ctx, tx, c, cur, &nh, hist{At: time.Now(), Command: "submitQuoteForApproval"}, func(h *Header) []kit.Event {
			return []kit.Event{{AggregateType: AggregateType, AggregateID: h.QuoteID, EventType: EventQuoteSent, Payload: map[string]any{}}}
		})
		require.NoError(t, err)
		return errors.New("simulated failure after the writes")
	})
	require.Error(t, err)
	require.Equal(t, beforeHist, e.count(`SELECT count(*) FROM ar02_quote_history WHERE quote_id = $1`, s.QuoteID))
	require.Equal(t, beforeAudit, e.audits(s.QuoteID))
	require.Zero(t, e.events(s.QuoteID, EventQuoteSent))
	require.Equal(t, StateDraft, e.get(s.QuoteID).State)

	// A failed create leaves no quote and does not burn a number.
	badBody := createBody(e.entity, uuid.NewString())
	e.prof.set(false, kit.Unprocessable(kit.CodeCustomerProfileInactive, "no"))
	code, _ = e.cmd(pPreparer, "createQuote", badBody)
	require.Equal(t, http.StatusUnprocessableEntity, code)
	e.prof.set(false, nil)
	n := e.create(pPreparer, nil)
	require.Equal(t, 4, e.count(`SELECT count(*) FROM ar02_quotes WHERE tenant_id = $1`, e.tenant), "quotes exist only for successful commands")
	require.Equal(t, "Q-000004", n.QuoteNumber, "a failed create does not burn a quote number")
}

func TestTenantAndEntityIsolation(t *testing.T) {
	e := newEnv(t)
	q := e.accepted(nil)

	other := *e
	other.tenant = uuid.NewString()
	for _, name := range []string{"getQuote", "getQuoteVersion", "getQuoteHistory", "getAvailableActions"} {
		code, body := other.query(pPreparer, name, "quote_id="+q.QuoteID)
		require.Equal(t, http.StatusNotFound, code, name)
		require.Equal(t, kit.CodeNotFound, errCode(t, body), name)
	}
	code, _ := other.query(pPreparer, "getQuote", "legal_entity_id="+e.entity+"&quote_number="+q.QuoteNumber)
	require.Equal(t, http.StatusNotFound, code)
	code, body := other.query(pPreparer, "listQuotes", "")
	require.Equal(t, http.StatusOK, code)
	var l struct {
		Items []ListItem `json:"items"`
	}
	require.NoError(t, jsonDecode(body, &l))
	require.Empty(t, l.Items, "another tenant sees nothing")
	for _, c := range []struct{ who, cmd string }{{pPreparer, "rejectQuote"}, {pConverter, "convertQuoteToOrder"}, {pPreparer, "sendQuote"}} {
		code, body = other.mk(c.who, c.cmd, q, map[string]any{"reason_code": "R"})
		require.Equal(t, http.StatusNotFound, code, c.cmd+string(body))
	}
	require.Equal(t, q.Version, e.get(q.QuoteID).Version)
	require.Zero(t, e.orders.calls)

	// A body entity that disagrees with the stored quote is refused.
	code, body = e.mk(pConverter, "convertQuoteToOrder", q, map[string]any{"legal_entity_id": uuid.NewString()})
	require.Equal(t, http.StatusForbidden, code, string(body))
	require.Equal(t, kit.CodeTenantScope, errCode(t, body))
	// Quote numbers are per tenant and entity: another tenant starts at 1 too.
	o := other.create(pPreparer, nil)
	require.Equal(t, "Q-000001", o.QuoteNumber)
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar02_quotes WHERE tenant_id = $1`, other.tenant))
}

// ── tax estimate: optional, estimate-only, blocks approval when unavailable ──

func TestTaxEstimate_OptionalAndFailClosedAtApproval(t *testing.T) {
	e := newEnv(t)
	taxParams := map[string]any{"jurisdiction_id": "GB", "tax_category": "STANDARD", "supply_kind": "SERVICES", "supply_type": "B2B"}
	q := e.submit(e.create(pPreparer, map[string]any{"request_tax_estimate": true, "tax_params": taxParams}))

	// Tax service down: approval is blocked (503), the quote stays pending, nothing is created.
	e.tax.err = taxdet.ErrUnavailable
	code, body := e.mk(pApprover, "approveQuote", q, map[string]any{"version_no": 1})
	require.Equal(t, http.StatusServiceUnavailable, code, string(body))
	require.Equal(t, kit.CodeDependencyUnavailable, errCode(t, body))
	require.Equal(t, StatePendingApproval, e.get(q.QuoteID).State)
	require.Zero(t, e.approvals(q.QuoteID))
	require.Zero(t, e.count(`SELECT count(*) FROM ar02_quote_tax_estimates WHERE quote_id = $1`, q.QuoteID))
	// A rejected request or an unusable determination is TAX_DETERMINATION_MISSING.
	e.tax.err = taxdet.ErrRejected
	code, body = e.mk(pApprover, "approveQuote", q, map[string]any{"version_no": 1})
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	require.Equal(t, kit.CodeTaxDeterminationMissing, errCode(t, body))
	e.tax.err = nil
	reversed := okDetermination()
	reversed.Status = "REVERSED"
	e.tax.det = reversed
	code, body = e.mk(pApprover, "approveQuote", q, map[string]any{"version_no": 1})
	require.Equal(t, kit.CodeTaxDeterminationMissing, errCode(t, body), string(body))
	// No tax client wired: also fails closed.
	e.m.tax = nil
	code, _ = e.mk(pApprover, "approveQuote", q, map[string]any{"version_no": 1})
	require.Equal(t, http.StatusServiceUnavailable, code)
	e.m.tax = e.tax
	e.tax.det = okDetermination()

	q = e.approveAs(pApprover, q)
	require.Equal(t, StateApproved, q.State)
	te := q.Current.TaxEstimate
	require.NotNil(t, te)
	require.Equal(t, "TD-1", te.DeterminationID)
	require.Equal(t, "SNAP-7", te.Provenance)
	require.True(t, te.IsEstimate, "stored as an estimate, never a tax document")
	require.Equal(t, kit.MustAmount("20"), te.TaxAmount)
	require.Equal(t, "AR", e.tax.last.SourceModule)
	require.Equal(t, q.Current.NetTotal, e.tax.last.GrossAmount)
	require.Equal(t, q.CustomerPartyID, e.tax.last.BuyerPartyID)
	require.Equal(t, e.entity, e.tax.last.LegalEntityID)
	require.Equal(t, "GB", e.tax.last.JurisdictionID)
	require.Equal(t, fmt.Sprintf("quote:%s:v1", q.QuoteID), e.tax.last.TransactionID)
	calls := e.tax.calls

	// A non-material amendment keeps the estimate (same price base) without a new call.
	q = e.step(pPreparer, "amendQuote", q, map[string]any{"assumptions": "clarified"})
	require.Equal(t, StateApproved, q.State)
	require.NotNil(t, q.Current.TaxEstimate)
	require.Equal(t, "TD-1", q.Current.TaxEstimate.DeterminationID)
	q = e.send(q)
	require.Equal(t, calls, e.tax.calls)
	// A material change needs a fresh approval and a fresh estimate.
	q = e.step(pPreparer, "amendQuote", q, map[string]any{"discount_percent": "3"})
	require.Nil(t, q.Current.TaxEstimate)
	q = e.approveAs(pApprover, e.submit(q))
	require.NotNil(t, q.Current.TaxEstimate)
	require.Equal(t, calls+1, e.tax.calls)

	// Whatever happened, a quote never produced a receivable or an accounting consequence.
	require.Zero(t, e.count(`SELECT count(*) FROM ar_accounting_posting_requests WHERE tenant_id = $1`, e.tenant))
	require.Zero(t, e.count(`SELECT count(*) FROM customer_invoices WHERE tenant_id::text = $1`, e.tenant))
}

// ── profile rules ────────────────────────────────────────────────────────────

func TestCustomerProfileRules(t *testing.T) {
	e := newEnv(t)
	inactive := kit.Unprocessable(kit.CodeCustomerProfileInactive, "financial profile is SUSPENDED")

	// No active profile: nothing is drafted.
	e.prof.set(false, inactive)
	code, body := e.cmd(pPreparer, "createQuote", createBody(e.entity, uuid.NewString()))
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	require.Equal(t, kit.CodeCustomerProfileInactive, errCode(t, body))
	require.Zero(t, e.count(`SELECT count(*) FROM ar02_quotes WHERE tenant_id = $1`, e.tenant))
	e.prof.set(false, errors.New("profile store down"))
	code, body = e.cmd(pPreparer, "createQuote", createBody(e.entity, uuid.NewString()))
	require.Equal(t, http.StatusServiceUnavailable, code, "an unreachable resolver fails closed: "+string(body))

	// A credit hold does NOT block drafting, approving, sending or accepting...
	e.prof.set(true, nil)
	q := e.accepted(nil)
	require.Equal(t, StateAccepted, q.State)
	// ...but it blocks conversion to an order.
	code, body = e.mk(pConverter, "convertQuoteToOrder", q, nil)
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	require.Equal(t, kit.CodeCreditHoldActive, errCode(t, body))
	require.Equal(t, StateAccepted, e.get(q.QuoteID).State)
	require.Zero(t, e.orderRows())
	e.prof.set(false, nil)
	require.Equal(t, StateConverted, e.step(pConverter, "convertQuoteToOrder", q, nil).State)

	// The customer going inactive mid-flight blocks submit, approve, send and convert.
	d := e.create(pPreparer, nil)
	e.prof.set(false, inactive)
	for _, c := range []struct {
		cmd  string
		body map[string]any
	}{{"submitQuoteForApproval", nil}, {"amendQuote", map[string]any{"terms": "x"}}} {
		code, body = e.mk(pPreparer, c.cmd, d, c.body)
		require.Equal(t, http.StatusUnprocessableEntity, code, c.cmd+string(body))
		require.Equal(t, kit.CodeCustomerProfileInactive, errCode(t, body))
	}
	e.prof.set(false, nil)
	p := e.submit(d)
	e.prof.set(false, inactive)
	code, body = e.mk(pApprover, "approveQuote", p, map[string]any{"version_no": 1})
	require.Equal(t, kit.CodeCustomerProfileInactive, errCode(t, body), string(body))
	e.prof.set(false, nil)
	a := e.approveAs(pApprover, p)
	e.prof.set(false, inactive)
	code, body = e.mk(pPreparer, "sendQuote", a, nil)
	require.Equal(t, kit.CodeCustomerProfileInactive, errCode(t, body), string(body))
	// Rejecting/expiring stay possible: protective actions never need an active profile.
	rj := e.step(pPreparer, "rejectQuote", a, map[string]any{"reason_code": "CUSTOMER_SUSPENDED"})
	require.Equal(t, StateRejected, rj.State)
	e.prof.set(false, nil)
	require.Positive(t, e.prof.calls)
}

// ── queries ──────────────────────────────────────────────────────────────────

func TestQueries_ListGetAndHistory(t *testing.T) {
	e := newEnv(t)
	party := uuid.NewString()
	mkQ := func(extra map[string]any) *Quote {
		b := createBody(e.entity, party)
		for k, v := range extra {
			b[k] = v
		}
		code, body := e.cmd(pPreparer, "createQuote", b)
		return e.ok(code, body)
	}
	q1 := mkQ(nil)
	q2 := e.submit(mkQ(map[string]any{"discount_percent": "2"}))
	q3 := mkQ(nil)
	other := e.create(pPreparer, nil) // a different customer

	list := func(qs string) []ListItem {
		code, body := e.query(pNobody, "listQuotes", qs)
		require.Equal(t, http.StatusOK, code, string(body))
		var l struct {
			Items []ListItem `json:"items"`
			Next  *int       `json:"next_offset"`
		}
		require.NoError(t, jsonDecode(body, &l))
		return l.Items
	}
	require.Len(t, list(""), 4)
	require.Len(t, list("customer_party_id="+party), 3)
	require.Len(t, list("customer_party_id="+party+"&state=DRAFT"), 2)
	got := list("customer_party_id=" + party + "&state=PENDING_APPROVAL")
	require.Len(t, got, 1)
	require.Equal(t, q2.QuoteID, got[0].QuoteID)
	require.Equal(t, kit.MustAmount("2"), got[0].DiscountPercent)
	require.Equal(t, q2.Current.NetTotal, got[0].NetTotal)
	require.Equal(t, q2.Current.ValidityDate, got[0].ValidityDate)
	require.Len(t, list("legal_entity_id="+uuid.NewString()), 0)
	require.Equal(t, other.QuoteID, list("limit=1")[0].QuoteID, "newest first")
	page2 := list("limit=2&offset=2")
	require.Len(t, page2, 2)
	code, body := e.query(pNobody, "listQuotes", "limit=100000")
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, string(body), `"limit":200`, "the page size is capped at 200")

	// Lookup by number, POST bodies, version listing.
	code, body = e.query(pNobody, "getQuote", "legal_entity_id="+e.entity+"&quote_number="+q1.QuoteNumber)
	require.Equal(t, http.StatusOK, code, string(body))
	require.Equal(t, q1.QuoteID, asQuote(t, body).QuoteID)
	code, body, _ = call(t, e.h, http.MethodPost, base+"getQuote", e.tenant, pNobody, "", map[string]any{"quote_id": q3.QuoteID})
	require.Equal(t, http.StatusOK, code, string(body))
	code, _ = e.query(pNobody, "getQuote", "")
	require.Equal(t, http.StatusBadRequest, code)
	code, _ = e.query(pNobody, "getQuote", "quote_id="+uuid.NewString())
	require.Equal(t, http.StatusNotFound, code)
	code, _ = e.query(pNobody, "getQuote", "quote_id=nope")
	require.Equal(t, http.StatusBadRequest, code)

	// History pagination: after_version paging; versions + approvals only on the first page.
	a := e.approveAs(pApprover, q2)
	a = e.send(a)
	code, body = e.query(pNobody, "getQuoteHistory", "quote_id="+a.QuoteID+"&limit=2")
	require.Equal(t, http.StatusOK, code)
	var h struct {
		Items []HistoryEntry `json:"items"`
		Next  int            `json:"next_after_version"`
		Vers  []VersionSummary
	}
	require.NoError(t, jsonDecode(body, &h))
	require.Len(t, h.Items, 2)
	require.Equal(t, 2, h.Next)
	code, body = e.query(pNobody, "getQuoteHistory", fmt.Sprintf("quote_id=%s&after_version=%d", a.QuoteID, h.Next))
	require.Equal(t, http.StatusOK, code)
	var h2 struct {
		Items    []HistoryEntry   `json:"items"`
		Versions []VersionSummary `json:"versions"`
	}
	require.NoError(t, jsonDecode(body, &h2))
	require.Len(t, h2.Items, 2)
	require.Equal(t, 3, h2.Items[0].QuoteVersion)
	require.Empty(t, h2.Versions)
}

func TestAvailableActions_DerivedFromStateAuthorityAndExpiry(t *testing.T) {
	e := newEnv(t)
	actions := func(principal string, q *Quote) map[string]actionState {
		code, body := e.query(principal, "getAvailableActions", "quote_id="+q.QuoteID)
		require.Equal(t, http.StatusOK, code, string(body))
		var out struct {
			Actions []actionState `json:"actions"`
		}
		require.NoError(t, jsonDecode(body, &out))
		m := map[string]actionState{}
		for _, a := range out.Actions {
			m[a.Action] = a
		}
		return m
	}
	q := e.create(pPreparer, map[string]any{"discount_percent": "15", "validity_date": e.dayOf(5)})
	a := actions(pPreparer, q)
	require.True(t, a["AmendQuote"].Available)
	require.True(t, a["SubmitQuoteForApproval"].Available)
	require.False(t, a["ApproveQuote"].Available, a["ApproveQuote"].Reason)
	require.False(t, a["SendQuote"].Available)
	require.False(t, a["RecordAcceptance"].Available)
	require.False(t, a["RejectQuote"].Available)
	require.False(t, a["ExpireQuote"].Available)
	require.False(t, a["ConvertQuoteToOrder"].Available)
	for name, x := range actions(pNobody, q) {
		require.False(t, x.Available, name)
	}
	require.False(t, actions(pApprover, q)["AmendQuote"].Available, "approver cannot manage")
	require.Contains(t, actions(pApprover, q)["AmendQuote"].Reason, ActionQuoteManage)

	// Pending approval with a 15% discount: SoD and authority are reflected per caller.
	b := e.step(pBoss, "submitQuoteForApproval", e.create(pBoss, map[string]any{"discount_percent": "15"}), nil)
	ap := actions(pBoss, b)["ApproveQuote"]
	require.False(t, ap.Available)
	require.Contains(t, ap.Reason, "different approver")
	require.True(t, actions(pDiscApprover, b)["ApproveQuote"].Available)
	require.False(t, actions(pApprover, b)["ApproveQuote"].Available, "lacks the discount authority")
	require.Contains(t, actions(pApprover, b)["ApproveQuote"].Reason, ActionQuoteDiscountApprove)
	require.True(t, actions(pPreparer, b)["RejectQuote"].Available)

	// Accepted: convert is available to a converter while the order service is wired and the quote is live.
	acc := e.accepted(map[string]any{"validity_date": e.dayOf(5)})
	require.True(t, actions(pConverter, acc)["ConvertQuoteToOrder"].Available)
	require.False(t, actions(pPreparer, acc)["ConvertQuoteToOrder"].Available, "needs the convert permission")
	require.False(t, actions(pPreparer, acc)["AmendQuote"].Available)
	e.m.orders = nil
	require.False(t, actions(pConverter, acc)["ConvertQuoteToOrder"].Available)
	e.m.orders = e.orders
	// After the validity lapses: convert is gone and expire appears.
	e.offset = 10 * 24 * time.Hour
	require.False(t, actions(pConverter, acc)["ConvertQuoteToOrder"].Available)
	require.Contains(t, actions(pConverter, acc)["ConvertQuoteToOrder"].Reason, "validity")
	require.True(t, actions(pPreparer, acc)["ExpireQuote"].Available)
	require.False(t, actions(pPreparer, q)["SubmitQuoteForApproval"].Available, "an elapsed draft cannot be submitted")
	e.offset = 0
	// Authorization outage fails closed.
	e.m.deps.Authz = &fakeAuthz{err: errors.New("authz down")}
	code, _ := e.query(pPreparer, "getAvailableActions", "quote_id="+q.QuoteID)
	require.Equal(t, http.StatusServiceUnavailable, code)
}
