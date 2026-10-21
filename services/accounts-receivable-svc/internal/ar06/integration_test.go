//go:build integration

// Real-Postgres tests for AR-06. Run (serially):
//
//	go test -tags=integration -count=1 -p 1 -timeout=600s ./internal/ar06/...
//
// The embedded superuser bypasses RLS, so isolation tests prove the explicit tenant_id predicates, not the policies.
// Spec section 18 scenarios carry their number (TestNeg17, TestNeg18, TestNeg35); the AR-06 table's four rows are
// TestRow1_CreditExceedsEligible (= TestNeg17*), TestRow2_StaleTaxRule, TestRow3_OriginalInvoiceNotEdited and TestRow4_SameCorrectionReplayedTwice.
package ar06

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sort"
	"sync"
	"testing"

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
	pool, stop = kittest.Start("ar06_test", 17750)
	if _, err := pool.Exec(context.Background(), `
		CREATE TABLE ar06_test_links (invoice_id text NOT NULL, correction_id text NOT NULL, kind text NOT NULL, PRIMARY KEY (invoice_id, correction_id));
		CREATE TABLE ar06_test_openitems (source_document_id text PRIMARY KEY, kind text NOT NULL, amount numeric NOT NULL, source_invoice_id text, due text);
		CREATE TABLE ar06_test_adjustments (open_item_id text NOT NULL, event_key text UNIQUE NOT NULL, kind text NOT NULL, amount numeric NOT NULL, source_ref_type text, source_ref_id text)`); err != nil {
		panic(err)
	}
	code := m.Run()
	stop()
	os.Exit(code)
}

type env struct {
	t       *testing.T
	m       *Module
	h       http.Handler
	tenant  string
	entity  string
	party   string
	inv     *fakeInvoices
	items   *fakeOpenItems
	periods *fakePeriods
	tax     *fakeTax
	src     *kit.IssuedInvoice
}

// newEnv wires every collaborator with a fake, with one ISSUED source invoice (gross 1500.60) and its open item. Each test gets its
// own tenant and entity so numbering and counters start fresh.
func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, tenant: uuid.NewString(), entity: uuid.NewString(), party: uuid.NewString(), inv: newFakeInvoices(), items: newFakeOpenItems(), periods: &fakePeriods{}, tax: newFakeTax()}
	e.inv.useDB = true
	deps := testDeps(&fakeAuthz{grants: standardGrants()})
	deps.DB = kit.NewDB(pool)
	e.m = New(deps, e.inv, e.items, e.tax, e.periods, nil)
	e.h = testRouter(e.m)
	e.src = e.addSource(sourceInvoice(e.entity, e.party))
	return e
}

func (e *env) addSource(src *kit.IssuedInvoice) *kit.IssuedInvoice {
	e.inv.put(e.tenant, src)
	e.items.putInvoiceItem(e.tenant, src.LegalEntityID, src.InvoiceID, src.Currency, src.Gross)
	return src
}

func (e *env) cmdAs(tenant, principal, name string, body map[string]any) (int, []byte) {
	code, b, _ := call(e.t, e.h, http.MethodPost, base+"commands/"+name, tenant, principal, "", body)
	return code, b
}

func (e *env) cmd(principal, name string, body map[string]any) (int, []byte) {
	return e.cmdAs(e.tenant, principal, name, body)
}

func (e *env) query(name, qs string) (int, []byte) {
	return e.queryAs(e.tenant, pNobody, name, qs)
}

func (e *env) queryAs(tenant, principal, name, qs string) (int, []byte) {
	code, b, _ := call(e.t, e.h, http.MethodGet, base+name+"?"+qs, tenant, principal, "", nil)
	return code, b
}

func (e *env) ok(code int, body []byte) *Correction {
	e.t.Helper()
	require.Less(e.t, code, 300, string(body))
	return asCorrection(e.t, body)
}

func cbody(c *Correction, extra map[string]any) map[string]any {
	b := map[string]any{"correction_id": c.CorrectionID, "expected_version": c.Version}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

func (e *env) create(principal string, body map[string]any) *Correction {
	e.t.Helper()
	code, b := e.cmd(principal, "createCorrectionDraft", body)
	require.Equal(e.t, http.StatusCreated, code, string(b))
	return asCorrection(e.t, b)
}

func (e *env) step(principal, name string, c *Correction, extra map[string]any) *Correction {
	e.t.Helper()
	code, b := e.cmd(principal, name, cbody(c, extra))
	return e.ok(code, b)
}

func (e *env) submit(c *Correction) *Correction {
	return e.step(pCreator, "submitCorrectionForApproval", c, nil)
}
func (e *env) approve(c *Correction) *Correction {
	return e.step(pApprover, "approveCorrection", c, map[string]any{"content_hash": c.ContentHash})
}
func (e *env) approvedFrom(body map[string]any) *Correction {
	return e.approve(e.submit(e.create(pCreator, body)))
}
func (e *env) approvedCredit(unitPrice string) *Correction {
	return e.approvedFrom(creditBody(e.entity, e.src, unitPrice, nil))
}
func (e *env) issueCredit(c *Correction) *Correction {
	return e.step(pIssuer, "issueCreditNote", c, nil)
}

func (e *env) get(id string) *Correction {
	e.t.Helper()
	code, b := e.query("getCorrection", "correction_id="+id)
	require.Equal(e.t, http.StatusOK, code, string(b))
	var out struct {
		Correction Correction `json:"correction"`
	}
	require.NoError(e.t, json.Unmarshal(b, &out))
	return &out.Correction
}

func (e *env) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	require.NoError(e.t, pool.QueryRow(context.Background(), q, args...).Scan(&n))
	return n
}

func (e *env) events(id, eventType string) int {
	return e.count(`SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND aggregate_id = $2 AND event_type = $3`, e.tenant, id, eventType)
}

func (e *env) postings(id string) int {
	return e.count(`SELECT count(*) FROM ar_accounting_posting_requests WHERE tenant_id = $1 AND source_event_id = $2`, e.tenant, id)
}

func (e *env) adjustments(id string) int {
	return e.count(`SELECT count(*) FROM ar06_test_adjustments WHERE source_ref_id = $1`, id)
}

func (e *env) links(id string) int {
	return e.count(`SELECT count(*) FROM ar06_test_links WHERE correction_id = $1`, id)
}

func (e *env) denials(id, command string) int {
	return e.count(`SELECT count(*) FROM ar_audit_events WHERE tenant_id = $1 AND aggregate_id = $2 AND command = $3`, e.tenant, id, command+".denied")
}

func (e *env) counter(series string) int {
	var n int
	err := pool.QueryRow(context.Background(), `SELECT next_number FROM ar06_number_counters WHERE tenant_id = $1 AND legal_entity_id = $2 AND series = $3`, e.tenant, e.entity, series).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0
	}
	require.NoError(e.t, err)
	return n
}

func (e *env) historyCmds(id string) []string {
	rows, err := pool.Query(context.Background(), `SELECT command FROM ar06_correction_history WHERE tenant_id = $1 AND correction_id = $2 ORDER BY correction_version`, e.tenant, id)
	require.NoError(e.t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		require.NoError(e.t, rows.Scan(&c))
		out = append(out, c)
	}
	return out
}

func (e *env) correctable(srcID string) map[string]any {
	e.t.Helper()
	code, b := e.query("getCorrectableAmount", "source_invoice_id="+srcID)
	require.Equal(e.t, http.StatusOK, code, string(b))
	var out struct {
		Eligibility map[string]any `json:"eligibility"`
	}
	require.NoError(e.t, json.Unmarshal(b, &out))
	return out.Eligibility
}

// nothingIssued asserts a refused issue left no number, adjustment, link, posting or issued event behind and the note unchanged.
func (e *env) nothingIssued(before *Correction, wantState State) {
	e.t.Helper()
	now := e.get(before.CorrectionID)
	require.Equal(e.t, wantState, now.State)
	require.Equal(e.t, before.Version, now.Version, "no state change")
	require.Empty(e.t, now.DocumentNumber)
	require.Zero(e.t, e.adjustments(before.CorrectionID), "no receivable adjustment")
	require.Zero(e.t, e.links(before.CorrectionID), "no link on the source invoice")
	require.Zero(e.t, e.postings(before.CorrectionID), "no accounting event")
	for _, ev := range []string{EventCreditNoteIssued, EventDebitNoteIssued, EventInvoiceCorrected, EventApplicationRequest} {
		require.Zero(e.t, e.events(before.CorrectionID, ev), ev)
	}
}

// ── happy paths ──────────────────────────────────────────────────────────────

func TestHappy_CreditNote_EndToEnd(t *testing.T) {
	e := newEnv(t)
	c := e.approvedCredit("400.00")
	require.Equal(t, StateApproved, c.State)
	require.Equal(t, kit.MustAmount("400.00"), c.NetTotal)
	require.Equal(t, kit.MustAmount("80.00"), c.TaxTotal, "tax comes from the tax domain, per tax group")
	require.Equal(t, kit.MustAmount("480.00"), c.GrossTotal)
	require.Len(t, c.TaxGroups, 1)
	require.Equal(t, "STANDARD", c.Lines[0].TaxCategory, "defaulted from the source line")

	code, body := e.cmd(pIssuer, "issueCreditNote", cbody(c, nil))
	n := e.ok(code, body)
	require.Equal(t, StateIssued, n.State)
	require.Equal(t, AppApplied, n.ApplicationState)
	require.Equal(t, "CN-000001", n.DocumentNumber)
	require.Equal(t, "CN", n.NumberingSeries)
	require.Equal(t, "QUEUED", n.AccountingState)
	require.Equal(t, "OI-"+e.src.InvoiceID, n.OpenItemID, "the credit is applied to the SOURCE invoice's open item")
	require.Equal(t, "2026-10-12", n.IssueDate)
	require.Len(t, n.IssuedContentHash, 64)

	// receivable consequence: an adjustment on the source open item, keyed <id>:issue
	var kind, key, refType string
	var amt kit.Amount
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT kind, event_key, source_ref_type, amount FROM ar06_test_adjustments WHERE source_ref_id = $1`, n.CorrectionID).Scan(&kind, &key, &refType, &amt))
	require.Equal(t, kit.AdjCreditNote, kind)
	require.Equal(t, n.CorrectionID+":issue", key)
	require.Equal(t, "CREDIT_NOTE", refType)
	require.Equal(t, kit.MustAmount("480.00"), amt)
	require.Equal(t, []string{e.src.InvoiceID + "|" + n.CorrectionID + "|CREDIT_NOTE"}, e.inv.linkLog())

	// accounting event on the ACC-04 queue, balanced, by mapping key
	var payload []byte
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT request_payload FROM ar_accounting_posting_requests WHERE tenant_id = $1 AND source_event_id = $2 AND source_type = 'CREDIT_NOTE'`,
		e.tenant, n.CorrectionID).Scan(&payload))
	var ev kit.PostingEvent
	require.NoError(t, json.Unmarshal(payload, &ev))
	require.Equal(t, "2026-10", ev.FiscalPeriod)
	got := map[string]float64{}
	var dr, cr float64
	for _, l := range ev.Lines {
		got[l.MappingKey] = l.DebitAmount - l.CreditAmount
		dr += l.DebitAmount
		cr += l.CreditAmount
	}
	require.Equal(t, map[string]float64{MappingCreditNoteRevenue: 400, MappingTaxOutput: 80, MappingReceivable: -480}, got)
	require.InDelta(t, dr, cr, 0.0001)

	for _, evt := range []string{EventDraftCreated, EventSubmitted, EventApproved, EventCreditNoteIssued, EventInvoiceCorrected, EventApplicationRequest} {
		require.Equal(t, 1, e.events(n.CorrectionID, evt), evt)
	}
	require.Equal(t, []string{"createCorrectionDraft", "submitCorrectionForApproval", "approveCorrection", "issueCreditNote"}, e.historyCmds(n.CorrectionID))

	// frozen representation, proven
	code, b := e.query("getCorrection", "correction_id="+n.CorrectionID)
	require.Equal(t, http.StatusOK, code)
	var q struct {
		Issued map[string]any `json:"issued"`
	}
	require.NoError(t, json.Unmarshal(b, &q))
	require.Equal(t, true, q.Issued["integrity_verified"])
	require.Equal(t, n.IssuedContentHash, q.Issued["issued_content_hash"])

	// correctable amount, chain, actions
	el := e.correctable(e.src.InvoiceID)
	require.Equal(t, "850.5000", el["remaining_correctable_net"])
	require.Equal(t, "1020.6000", el["remaining_correctable_gross"])
	code, b = e.query("getCorrectionChain", "source_invoice_id="+e.src.InvoiceID)
	require.Equal(t, http.StatusOK, code)
	var chain struct {
		Corrections []chainEntry `json:"corrections"`
	}
	require.NoError(t, json.Unmarshal(b, &chain))
	require.Len(t, chain.Corrections, 1)
	require.Equal(t, "CN-000001", chain.Corrections[0].DocumentNumber)
	code, b, _ = call(t, e.h, http.MethodGet, base+"getAvailableActions?correction_id="+n.CorrectionID, e.tenant, pLinker, "", nil)
	require.Equal(t, http.StatusOK, code)
	var acts struct {
		Actions []actionState `json:"actions"`
	}
	require.NoError(t, json.Unmarshal(b, &acts))
	avail := map[string]bool{}
	for _, a := range acts.Actions {
		avail[a.Action] = a.Available
	}
	require.True(t, avail["LinkReplacementInvoice"])
	require.False(t, avail["IssueCreditNote"] || avail["CancelUnissuedCorrection"] || avail["ApproveCorrection"])
}

func TestHappy_DebitNote(t *testing.T) {
	e := newEnv(t)
	c := e.approvedFrom(debitBody(e.entity, e.src, "100.00", nil))
	require.Equal(t, kit.MustAmount("120.00"), c.GrossTotal)
	code, body := e.cmd(pIssuer, "issueCreditNote", cbody(c, nil))
	require.Equal(t, http.StatusUnprocessableEntity, code, "a debit note cannot be issued through the credit-note command")
	e.nothingIssued(c, StateApproved)

	n := e.step(pIssuer, "issueDebitNote", c, nil)
	_ = body
	require.Equal(t, "DN-000001", n.DocumentNumber)
	require.Equal(t, "DN", n.NumberingSeries)
	require.Equal(t, "OI-"+n.CorrectionID, n.OpenItemID, "a debit note is its own receivable")
	var kind, due string
	var amt kit.Amount
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT kind, amount, due FROM ar06_test_openitems WHERE source_document_id = $1`, n.CorrectionID).Scan(&kind, &amt, &due))
	require.Equal(t, kit.DocDebitNote, kind)
	require.Equal(t, kit.MustAmount("120.00"), amt)
	require.Equal(t, "2026-11-11", due)
	require.Zero(t, e.adjustments(n.CorrectionID), "a debit note adjusts nothing")
	require.Equal(t, []string{e.src.InvoiceID + "|" + n.CorrectionID + "|DEBIT_NOTE"}, e.inv.linkLog())
	var payload []byte
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT request_payload FROM ar_accounting_posting_requests WHERE tenant_id = $1 AND source_event_id = $2 AND source_type = 'DEBIT_NOTE'`,
		e.tenant, n.CorrectionID).Scan(&payload))
	var ev kit.PostingEvent
	require.NoError(t, json.Unmarshal(payload, &ev))
	got := map[string]float64{}
	for _, l := range ev.Lines {
		got[l.MappingKey] = l.DebitAmount - l.CreditAmount
	}
	require.Equal(t, map[string]float64{MappingReceivable: 120, MappingRevenue: -100, MappingTaxOutput: -20}, got)
	require.Equal(t, 1, e.events(n.CorrectionID, EventDebitNoteIssued))
	require.Equal(t, "1250.5000", e.correctable(e.src.InvoiceID)["remaining_correctable_net"], "a debit note does not extend or consume the correctable amount")
}

// ── spec section 18 #17 / AR-06 row 1: credit exceeds eligible amount ────────

func TestNeg17_CreditExceedsEligibleAmount_Draft(t *testing.T) {
	e := newEnv(t)
	b := creditBody(e.entity, e.src, "700.00", map[string]any{"evidence_id": "EV-1"})
	b["lines"] = append(b["lines"].([]map[string]any), map[string]any{"source_line_no": 1, "description": "again", "quantity": "1", "unit_price": "700.00",
		"product_classification": "SOFTWARE", "supply_kind": "DIGITAL_SERVICES"})
	code, body := e.cmd(pCreator, "createCorrectionDraft", b) // net 1400.00 > source net 1250.50
	require.Equal(t, http.StatusUnprocessableEntity, code)
	require.Equal(t, kit.CodeCorrectionExceedsEligible, errCode(t, body))
	require.Zero(t, e.count(`SELECT count(*) FROM ar06_corrections WHERE tenant_id = $1`, e.tenant), "nothing was persisted")
}

func TestNeg17_CreditExceedsEligibleAmount_NetAndGrossAfterIssuedCredit(t *testing.T) {
	e := newEnv(t)
	first := e.issueCredit(e.approvedFrom(creditBody(e.entity, e.src, "1000.00", map[string]any{"evidence_id": "EV-1"}))) // remaining net 250.50, remaining gross 300.60
	require.Equal(t, "CN-000001", first.DocumentNumber)
	code, body := e.cmd(pCreator, "createCorrectionDraft", creditBody(e.entity, e.src, "250.51", nil))
	require.Equal(t, http.StatusUnprocessableEntity, code)
	require.Equal(t, kit.CodeCorrectionExceedsEligible, errCode(t, body), "net over the remaining net is refused at draft")

	// net fits (250.50) but the tax domain's answer (25%) pushes the gross (313.13) over the remaining gross (300.60): refused at submit
	d := e.create(pCreator, creditBody(e.entity, e.src, "250.50", nil))
	e.tax.set(func() { e.tax.ratePct = "25" })
	code, body = e.cmd(pCreator, "submitCorrectionForApproval", cbody(d, nil))
	require.Equal(t, http.StatusUnprocessableEntity, code)
	require.Equal(t, kit.CodeCorrectionExceedsEligible, errCode(t, body))
	require.Equal(t, StateDraft, e.get(d.CorrectionID).State)
	require.Equal(t, 2, e.counter(SeriesCredit), "no number was consumed by the refused credit")
}

func TestNeg17_CreditExceedsEligibleAmount_AtApproval(t *testing.T) {
	e := newEnv(t)
	c1 := e.submit(e.create(pCreator, creditBody(e.entity, e.src, "700.00", nil)))
	c2 := e.submit(e.create(pCreator, creditBody(e.entity, e.src, "700.00", nil))) // both fit while nothing is issued
	c1 = e.approve(c1)
	e.issueCredit(c1) // remaining net 550.50
	code, body := e.cmd(pApprover, "approveCorrection", cbody(c2, map[string]any{"content_hash": c2.ContentHash}))
	require.Equal(t, http.StatusUnprocessableEntity, code)
	require.Equal(t, kit.CodeCorrectionExceedsEligible, errCode(t, body))
	require.Equal(t, StatePendingApproval, e.get(c2.CorrectionID).State)
}

func TestNeg17_CreditExceedsEligibleAmount_AtIssue(t *testing.T) {
	e := newEnv(t)
	c1 := e.approvedCredit("700.00")
	c2 := e.approvedCredit("700.00")
	e.issueCredit(c1)
	code, body := e.cmd(pIssuer, "issueCreditNote", cbody(c2, nil))
	require.Equal(t, http.StatusUnprocessableEntity, code)
	require.Equal(t, kit.CodeCorrectionExceedsEligible, errCode(t, body))
	e.nothingIssued(c2, StateApproved)
	require.Equal(t, 2, e.counter(SeriesCredit), "only the first note took a number: the sequence has no gap")
	require.Equal(t, 1, e.adjustments(c1.CorrectionID))
	require.Zero(t, e.adjustments(c2.CorrectionID))
}

func TestNeg17_ConcurrentCreditsCannotJointlyExceed(t *testing.T) {
	e := newEnv(t)
	const n = 6
	cs := make([]*Correction, n)
	for i := range cs {
		cs[i] = e.approvedCredit("400.00") // net 400 gross 480 each: exactly three fit in net 1250.50 / gross 1500.60
	}
	codes := make([]int, n)
	bodies := make([][]byte, n)
	reqs := make([][]byte, n)
	for i, c := range cs {
		reqs[i], _ = json.Marshal(cbody(c, nil))
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range cs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			codes[i], bodies[i] = e.rawCmd(pIssuer, "issueCreditNote", reqs[i])
		}(i)
	}
	close(start)
	wg.Wait()
	okN, refused := 0, 0
	var numbers []string
	for i := range cs {
		switch codes[i] {
		case http.StatusOK:
			okN++
			numbers = append(numbers, asCorrection(t, bodies[i]).DocumentNumber)
		case http.StatusUnprocessableEntity:
			refused++
			require.Equal(t, kit.CodeCorrectionExceedsEligible, errCode(t, bodies[i]))
			e.nothingIssued(cs[i], StateApproved)
		default:
			t.Fatalf("unexpected %d: %s", codes[i], bodies[i])
		}
	}
	require.Equal(t, 3, okN, "exactly the permitted total succeeds")
	require.Equal(t, 3, refused)
	sort.Strings(numbers)
	require.Equal(t, []string{"CN-000001", "CN-000002", "CN-000003"}, numbers, "gapless, no duplicate")
	var sum kit.Amount
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT COALESCE(sum(amount),0) FROM ar06_test_adjustments WHERE open_item_id = $1`, "OI-"+e.src.InvoiceID).Scan(&sum))
	require.Equal(t, kit.MustAmount("1440.00"), sum)
	el := e.correctable(e.src.InvoiceID)
	require.Equal(t, "60.6000", el["remaining_correctable_gross"])
	require.Equal(t, "50.5000", el["remaining_correctable_net"])
}

func (e *env) rawCmd(principal, name string, raw []byte) (int, []byte) {
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	code, b, _ := call(e.t, e.h, http.MethodPost, base+"commands/"+name, e.tenant, principal, "", body)
	return code, b
}

// ── spec section 18 #18: correction against the wrong source invoice ─────────

func TestNeg18_CorrectionAgainstWrongSourceInvoice(t *testing.T) {
	e := newEnv(t)
	post := func(b map[string]any) (int, string) {
		code, body := e.cmd(pCreator, "createCorrectionDraft", b)
		return code, errCode(t, body)
	}
	// unknown / unissued source: AR-04 has no ISSUED invoice by that id
	code, c := post(creditBody(e.entity, e.src, "10.00", map[string]any{"source_invoice_id": uuid.NewString()}))
	require.Equal(t, http.StatusNotFound, code)
	require.Equal(t, kit.CodeNotFound, c)
	// another legal entity's invoice (same tenant)
	other := e.addSource(sourceInvoice(uuid.NewString(), e.party))
	code, c = post(creditBody(e.entity, other, "10.00", nil))
	require.Equal(t, http.StatusUnprocessableEntity, code)
	require.Equal(t, CodeCorrectionSourceInvalid, c)
	// another customer
	code, c = post(creditBody(e.entity, e.src, "10.00", map[string]any{"customer_party_id": uuid.NewString()}))
	require.Equal(t, http.StatusUnprocessableEntity, code)
	require.Equal(t, CodeCorrectionSourceInvalid, c)
	// another tenant's invoice
	foreign := sourceInvoice(e.entity, e.party)
	e.inv.put(uuid.NewString(), foreign)
	code, _ = post(creditBody(e.entity, foreign, "10.00", nil))
	require.Equal(t, http.StatusNotFound, code)
	require.Zero(t, e.count(`SELECT count(*) FROM ar06_corrections WHERE tenant_id = $1`, e.tenant), "no correction was recorded for any wrong source")

	// the source is re-verified at issue: if it is no longer an issued invoice the note is not issued
	approved := e.approvedCredit("100.00")
	e.inv.mu.Lock()
	delete(e.inv.issued, e.tenant+"/"+e.src.InvoiceID)
	e.inv.mu.Unlock()
	code, body := e.cmd(pIssuer, "issueCreditNote", cbody(approved, nil))
	require.Equal(t, http.StatusNotFound, code, string(body))
	e.nothingIssued(approved, StateApproved)
	// a credit with no open item on the source receivable is refused (and nothing is issued)
	e.addSource(e.src)
	e.items.mu.Lock()
	delete(e.items.items, e.tenant+"/"+e.src.InvoiceID)
	e.items.mu.Unlock()
	code, body = e.cmd(pIssuer, "issueCreditNote", cbody(approved, nil))
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	require.Equal(t, kit.CodeReceivableResidualInvalid, errCode(t, body))
	e.nothingIssued(approved, StateApproved)
}

// ── spec section 18 #35: accounting event missing for an issued correction ───

func TestNeg35_AccountingEventMissingIsVisible(t *testing.T) {
	e := newEnv(t)
	n := e.issueCredit(e.approvedCredit("100.00"))
	status := func() map[string]any {
		code, b := e.query("getCorrectionAccountingStatus", "correction_id="+n.CorrectionID)
		require.Equal(t, http.StatusOK, code, string(b))
		var out map[string]any
		require.NoError(t, json.Unmarshal(b, &out))
		return out
	}
	s := status()
	require.Equal(t, "PENDING", s["posting_status"])
	require.Equal(t, false, s["control_exception"])
	// the queue row disappears (the failure mode the control exists for)
	_, err := pool.Exec(context.Background(), `DELETE FROM ar_accounting_posting_requests WHERE tenant_id = $1 AND source_event_id = $2`, e.tenant, n.CorrectionID)
	require.NoError(t, err)
	s = status()
	require.Equal(t, AcctMissing, s["posting_status"])
	require.Equal(t, true, s["control_exception"])
	require.Equal(t, "ISSUED_NOTE_WITHOUT_POSTING_REQUEST", s["exception"])
	// an unissued note has nothing to post
	d := e.create(pCreator, creditBody(e.entity, e.src, "10.00", nil))
	code, b := e.query("getCorrectionAccountingStatus", "correction_id="+d.CorrectionID)
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, string(b), AcctNotApplicable)
}

func TestPostingListener_MirrorsOutcomeOntoNote(t *testing.T) {
	e := newEnv(t)
	n := e.issueCredit(e.approvedCredit("100.00"))
	ctx := context.Background()
	q := kit.QueuedPosting{TenantID: e.tenant, SourceType: kit.SourceCreditNote, SourceID: n.CorrectionID, SourceEventID: n.CorrectionID, CorrelationID: "corr"}
	require.NoError(t, e.m.OnPostingOutcome(ctx, q, kit.PostingOutcome{Status: kit.PostingPending}), "a pending outcome changes nothing")
	require.Equal(t, "QUEUED", e.get(n.CorrectionID).AccountingState)
	require.NoError(t, e.m.OnPostingOutcome(ctx, q, kit.PostingOutcome{Status: kit.PostingQuarantined, Error: "ambiguous mapping"}))
	g := e.get(n.CorrectionID)
	require.Equal(t, "QUARANTINED", g.AccountingState)
	require.Equal(t, "ambiguous mapping", g.AccountingError)
	require.Equal(t, n.Version, g.Version, "the mirror is not an operator-visible state change")
	require.NoError(t, e.m.OnPostingOutcome(ctx, q, kit.PostingOutcome{Status: kit.PostingPosted, JournalID: "J-1"}))
	g = e.get(n.CorrectionID)
	require.Equal(t, "POSTED", g.AccountingState)
	require.Equal(t, "J-1", g.AccountingJournalID)
	require.NoError(t, e.m.OnPostingOutcome(ctx, q, kit.PostingOutcome{Status: kit.PostingFailed, Error: "late contradiction"}))
	require.Equal(t, "POSTED", e.get(n.CorrectionID).AccountingState, "a POSTED note is never regressed")
	// debit notes route to the same listener; other source types are ignored; an unknown note is re-delivered
	require.NoError(t, e.m.OnPostingOutcome(ctx, kit.QueuedPosting{TenantID: e.tenant, SourceType: kit.SourceInvoice, SourceID: uuid.NewString()}, kit.PostingOutcome{Status: kit.PostingPosted}))
	require.Error(t, e.m.OnPostingOutcome(ctx, kit.QueuedPosting{TenantID: e.tenant, SourceType: kit.SourceDebitNote, SourceID: uuid.NewString()}, kit.PostingOutcome{Status: kit.PostingPosted}))
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar_audit_events WHERE tenant_id = $1 AND aggregate_id = $2 AND command = 'accountingOutcome' AND after_state->>'accounting_state' = 'POSTED'`, e.tenant, n.CorrectionID))
}

// ── AR-06 row 2: correction issued with a stale tax rule ─────────────────────

func TestRow2_StaleTaxRule(t *testing.T) {
	e := newEnv(t)
	c := e.approvedCredit("400.00")
	id := c.TaxGroups[0].DeterminationID
	e.tax.set(func() { e.tax.overrideTax[id] = kit.MustAmount("90.00") }) // the tax result changed after approval
	code, body := e.cmd(pIssuer, "issueCreditNote", cbody(c, nil))
	require.Equal(t, http.StatusConflict, code)
	require.Equal(t, kit.CodeInvoiceApprovalInvalidated, errCode(t, body))
	d := e.get(c.CorrectionID)
	require.Equal(t, StateDraft, d.State, "returned to draft")
	require.Empty(t, d.TaxGroups)
	require.Empty(t, d.ApprovedBy)
	require.Equal(t, kit.Amount(0), d.TaxTotal)
	require.Zero(t, e.counter(SeriesCredit), "no number was allocated")
	require.Zero(t, e.adjustments(c.CorrectionID))
	require.Zero(t, e.postings(c.CorrectionID))
	require.Zero(t, e.links(c.CorrectionID))
	require.Equal(t, 1, e.events(c.CorrectionID, EventApprovalInvalidated))
	require.Contains(t, e.historyCmds(c.CorrectionID), "invalidateApproval")
	// the old approval is useless; a re-submit re-determines tax, a new approval and issue then succeed
	d = e.submit(d)
	require.NotEqual(t, id, d.TaxGroups[0].DeterminationID)
	n := e.issueCredit(e.approve(d))
	require.Equal(t, "CN-000001", n.DocumentNumber)

	// a changed rule-pack provenance is equally stale
	c2 := e.approvedCredit("100.00")
	e.tax.set(func() { e.tax.store[c2.TaxGroups[0].DeterminationID].TaxLogicSnapshotID = "snap-2026.2" })
	code, body = e.cmd(pIssuer, "issueCreditNote", cbody(c2, nil))
	require.Equal(t, kit.CodeInvoiceApprovalInvalidated, errCode(t, body))
	require.Equal(t, http.StatusConflict, code)

	// a determination that no longer exists, or a tax service that is down, fails closed with the note unchanged
	c3 := e.approvedCredit("100.00")
	e.tax.set(func() { e.tax.notFound[c3.TaxGroups[0].DeterminationID] = true })
	code, body = e.cmd(pIssuer, "issueCreditNote", cbody(c3, nil))
	require.Equal(t, http.StatusUnprocessableEntity, code)
	require.Equal(t, kit.CodeTaxDeterminationMissing, errCode(t, body))
	e.nothingIssued(c3, StateApproved)
	e.tax.set(func() { e.tax.notFound = map[string]bool{}; e.tax.getErr = taxdet.ErrUnavailable })
	code, body = e.cmd(pIssuer, "issueCreditNote", cbody(c3, nil))
	require.Equal(t, http.StatusServiceUnavailable, code, string(body))
	e.nothingIssued(c3, StateApproved)

	// at submit: tax down => 503 and the draft stays a draft; a rejected request => TAX_DETERMINATION_MISSING
	d4 := e.create(pCreator, creditBody(e.entity, e.src, "10.00", nil))
	e.tax.set(func() { e.tax.detErr = taxdet.ErrUnavailable })
	code, _ = e.cmd(pCreator, "submitCorrectionForApproval", cbody(d4, nil))
	require.Equal(t, http.StatusServiceUnavailable, code)
	e.tax.set(func() { e.tax.detErr = taxdet.ErrRejected })
	code, body = e.cmd(pCreator, "submitCorrectionForApproval", cbody(d4, nil))
	require.Equal(t, http.StatusUnprocessableEntity, code)
	require.Equal(t, kit.CodeTaxDeterminationMissing, errCode(t, body))
	require.Equal(t, StateDraft, e.get(d4.CorrectionID).State)
}

// ── AR-06 row 3: original invoice edited instead of a note ───────────────────

func TestRow3_OriginalInvoiceNotEdited(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	invID := uuid.NewString()
	_, err := pool.Exec(ctx, `INSERT INTO ar04_invoices (invoice_id, tenant_id, legal_entity_id, customer_party_id, currency, lifecycle_state, accounting_state, invoice_number, numbering_series,
			sequence_no, supply_date, issue_date, due_date, net_total, tax_total, gross_total, content_hash, approved_content_hash, approved_by, approved_at, created_by, submitted_by,
			submitted_at, issued_by, issued_at, issued_canonical, issued_content_hash, open_item_id)
		VALUES ($1,$2,$3,$4,'USD','ISSUED','QUEUED','INV-ROW3','INV',1,'2026-10-01','2026-10-01','2026-10-31',1000,200,1200,repeat('a',64),repeat('a',64),'appr',now(),'maker','sub',now(),
			'iss',now(),'{}',repeat('b',64),'OI-1')`, invID, e.tenant, e.entity, e.party)
	require.NoError(t, err)

	// the issued invoice cannot be edited in place by anyone: the immutability trigger blocks it (DOCUMENT_IMMUTABLE)
	for _, sql := range []string{
		`UPDATE ar04_invoices SET gross_total = 1 WHERE invoice_id = $1`,
		`UPDATE ar04_invoices SET notes = 'edited' WHERE invoice_id = $1`,
		`UPDATE ar04_invoices SET invoice_number = 'INV-EDITED' WHERE invoice_id = $1`,
		`UPDATE ar04_invoices SET lifecycle_state = 'DRAFT' WHERE invoice_id = $1`,
		`DELETE FROM ar04_invoices WHERE invoice_id = $1`,
	} {
		_, err := pool.Exec(ctx, sql, invID)
		require.Error(t, err, sql)
		var p *kit.Problem
		require.True(t, errors.As(kit.MapPgErr(err), &p), sql)
		require.Equal(t, kit.CodeImmutable, p.Code, sql)
	}

	// a full credit-note flow against it leaves the original untouched
	src := &kit.IssuedInvoice{InvoiceID: invID, LegalEntityID: e.entity, CustomerPartyID: e.party, InvoiceNumber: "INV-ROW3", Currency: "USD", Net: kit.MustAmount("1000"),
		Tax: kit.MustAmount("200"), Gross: kit.MustAmount("1200"), DueDate: testNow.AddDate(0, 0, 19),
		Lines: []kit.IssuedInvoiceLine{{LineNumber: 1, Description: "x", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("1000"), Net: kit.MustAmount("1000"), Tax: kit.MustAmount("200"), TaxCode: "STANDARD"}}}
	e.addSource(src)
	snapshot := func() string {
		var s string
		require.NoError(t, pool.QueryRow(ctx, `SELECT row_to_json(i)::text FROM ar04_invoices i WHERE invoice_id = $1`, invID).Scan(&s))
		return s
	}
	before := snapshot()
	n := e.issueCredit(e.approvedFrom(creditBody(e.entity, src, "500.00", nil)))
	require.Equal(t, invID, n.SourceInvoiceID)
	require.Equal(t, before, snapshot(), "the source invoice row is byte-for-byte unchanged: corrections are new linked documents")

	// the issued NOTE is equally never edited in place
	for _, sql := range []string{
		`UPDATE ar06_corrections SET reason_code = 'EDITED' WHERE correction_id = $1`,
		`UPDATE ar06_corrections SET document_number = 'CN-EDITED' WHERE correction_id = $1`,
		`UPDATE ar06_corrections SET net_total = 1, gross_total = 1 WHERE correction_id = $1`,
		`UPDATE ar06_corrections SET issued_canonical = '{}' WHERE correction_id = $1`,
		`UPDATE ar06_corrections SET source_invoice_id = gen_random_uuid() WHERE correction_id = $1`,
		`UPDATE ar06_corrections SET state = 'DRAFT' WHERE correction_id = $1`,
		`DELETE FROM ar06_corrections WHERE correction_id = $1`,
		`UPDATE ar06_correction_lines SET net_amount = 1 WHERE correction_id = $1`,
		`DELETE FROM ar06_correction_lines WHERE correction_id = $1`,
		`UPDATE ar06_correction_tax_determinations SET tax_amount = 0 WHERE correction_id = $1`,
		`INSERT INTO ar06_correction_lines (correction_id, line_no, tenant_id, description, quantity, unit_price, net_amount) SELECT correction_id, 9, tenant_id, 'x', 1, 1, 1 FROM ar06_corrections WHERE correction_id = $1`,
		`UPDATE ar06_correction_history SET command = 'x' WHERE correction_id = $1`,
		`DELETE FROM ar06_correction_history WHERE correction_id = $1`,
	} {
		_, err := pool.Exec(ctx, sql, n.CorrectionID)
		require.Error(t, err, sql)
	}
	_, err = pool.Exec(ctx, `UPDATE ar06_corrections SET reason_code = 'EDITED' WHERE correction_id = $1`, n.CorrectionID)
	var p *kit.Problem
	require.True(t, errors.As(kit.MapPgErr(err), &p))
	require.Equal(t, kit.CodeImmutable, p.Code)
	// the permitted dimension writes still work
	_, err = pool.Exec(ctx, `UPDATE ar06_corrections SET accounting_state = 'POSTED', accounting_journal_id = 'J', version = version WHERE correction_id = $1`, n.CorrectionID)
	require.NoError(t, err)
	// command level: an issued note is not editable, cancellable or re-approvable
	code, body := e.cmd(pCreator, "cancelUnissuedCorrection", cbody(e.get(n.CorrectionID), map[string]any{"reason_code": "MISTAKE"}))
	require.Equal(t, http.StatusConflict, code)
	require.Equal(t, kit.CodeImmutable, errCode(t, body))
}

// ── AR-06 row 4: same correction replayed twice ──────────────────────────────

func TestRow4_SameCorrectionReplayedTwice(t *testing.T) {
	e := newEnv(t)
	// (a) the same create (same key, same body) returns the original, one record
	body := creditBody(e.entity, e.src, "100.00", map[string]any{"client_reference": "TICKET-77"})
	code1, b1, _ := call(t, e.h, http.MethodPost, base+"commands/createCorrectionDraft", e.tenant, pCreator, "key-create-1", body)
	code2, b2, h2 := call(t, e.h, http.MethodPost, base+"commands/createCorrectionDraft", e.tenant, pCreator, "key-create-1", body)
	require.Equal(t, http.StatusCreated, code1)
	require.Equal(t, http.StatusCreated, code2)
	require.Equal(t, "true", h2.Get("Idempotent-Replay"))
	require.Equal(t, asCorrection(t, b1).CorrectionID, asCorrection(t, b2).CorrectionID)
	// same key, different body => reuse refused
	other := creditBody(e.entity, e.src, "200.00", map[string]any{"client_reference": "TICKET-77"})
	code, b, _ := call(t, e.h, http.MethodPost, base+"commands/createCorrectionDraft", e.tenant, pCreator, "key-create-1", other)
	require.Equal(t, http.StatusUnprocessableEntity, code)
	require.Equal(t, kit.CodeIdempotencyReused, errCode(t, b))
	// (c) the same correction under a NEW key (a client retry that lost its key) is a duplicate
	code, b, _ = call(t, e.h, http.MethodPost, base+"commands/createCorrectionDraft", e.tenant, pCreator, "key-create-2", body)
	require.Equal(t, http.StatusConflict, code)
	require.Equal(t, CodeCorrectionDuplicate, errCode(t, b))
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar06_corrections WHERE tenant_id = $1`, e.tenant))

	// (b) issue: same key replays the stored answer; a different key is CORRECTION_ALREADY_ISSUED; one number, one adjustment, one posting
	c := e.approve(e.submit(asCorrection(t, b1)))
	issueBody := cbody(c, nil)
	code1, b1, _ = call(t, e.h, http.MethodPost, base+"commands/issueCreditNote", e.tenant, pIssuer, "key-issue-1", issueBody)
	code2, b2, h2 = call(t, e.h, http.MethodPost, base+"commands/issueCreditNote", e.tenant, pIssuer, "key-issue-1", issueBody)
	require.Equal(t, http.StatusOK, code1, string(b1))
	require.Equal(t, http.StatusOK, code2)
	require.Equal(t, "true", h2.Get("Idempotent-Replay"))
	require.Equal(t, asCorrection(t, b1).DocumentNumber, asCorrection(t, b2).DocumentNumber)
	code, b, _ = call(t, e.h, http.MethodPost, base+"commands/issueCreditNote", e.tenant, pIssuer, "key-issue-2", issueBody)
	require.Equal(t, http.StatusConflict, code)
	require.Equal(t, CodeCorrectionAlreadyIssued, errCode(t, b))
	code, b, _ = call(t, e.h, http.MethodPost, base+"commands/issueCreditNote", e.tenant, pIssuer, "key-issue-3", cbody(e.get(c.CorrectionID), nil))
	require.Equal(t, CodeCorrectionAlreadyIssued, errCode(t, b), "even with the current version")
	require.Equal(t, 2, e.counter(SeriesCredit), "a single number was allocated")
	require.Equal(t, 1, e.adjustments(c.CorrectionID))
	require.Equal(t, 1, e.postings(c.CorrectionID))
	require.Equal(t, 1, e.events(c.CorrectionID, EventCreditNoteIssued))
	require.Equal(t, 1, e.links(c.CorrectionID))
	// the port itself is idempotent on the event key: replaying the adjustment applies nothing twice
	require.NoError(t, e.m.deps.DB.WithTx(context.Background(), e.tenant, func(tx pgx.Tx) error {
		_, err := e.items.ApplyAdjustment(context.Background(), tx, kit.Caller{TenantID: e.tenant}, kit.OpenItemAdjustment{OpenItemID: "OI-" + e.src.InvoiceID, Kind: kit.AdjCreditNote,
			Amount: c.GrossTotal, EventKey: c.CorrectionID + ":issue", SourceRefType: "CREDIT_NOTE", SourceRefID: c.CorrectionID})
		return err
	}))
	require.Equal(t, 1, e.adjustments(c.CorrectionID))

	// a cancelled correction frees its client reference
	d := e.create(pCreator, creditBody(e.entity, e.src, "10.00", map[string]any{"client_reference": "TICKET-78"}))
	e.step(pCreator, "cancelUnissuedCorrection", d, map[string]any{"reason_code": "DUPLICATE"})
	e.create(pCreator, creditBody(e.entity, e.src, "10.00", map[string]any{"client_reference": "TICKET-78"}))
}

// ── atomicity: any failing step leaves nothing behind ────────────────────────

func TestIssueAtomicity_FailureLeavesNothing(t *testing.T) {
	e := newEnv(t)
	c := e.approvedCredit("100.00")
	cases := []struct {
		name   string
		inject func()
		clear  func()
		status int
	}{
		{"period closed", func() { e.periods.setErr(kit.Conflict(kit.CodePeriodClosed, "closed")) }, func() { e.periods.setErr(nil) }, http.StatusConflict},
		{"period service down", func() { e.periods.setErr(kit.Unavailable("down")) }, func() { e.periods.setErr(nil) }, http.StatusServiceUnavailable},
		{"receivable creation fails", func() { e.items.setErr(errors.New("db down")) }, func() { e.items.setErr(nil) }, http.StatusServiceUnavailable},
		{"receivable refuses the credit", func() { e.items.setErr(kit.Unprocessable(kit.CodeReceivableResidualInvalid, "residual")) }, func() { e.items.setErr(nil) }, http.StatusUnprocessableEntity},
		// the link is the LAST write before the posting: the number, the adjustment and the state update all happened already and roll back
		{"source link fails", func() { e.inv.setLinkErr(errors.New("ar04 down")) }, func() { e.inv.setLinkErr(nil) }, http.StatusServiceUnavailable},
	}
	for _, k := range cases {
		k.inject()
		code, body := e.cmd(pIssuer, "issueCreditNote", cbody(c, nil))
		k.clear()
		require.Equal(t, k.status, code, k.name+": "+string(body))
		e.nothingIssued(c, StateApproved)
		require.Zero(t, e.counter(SeriesCredit), k.name+": the legal number is returned")
	}
	// an unwired AR-07 refuses before anything happens
	m2 := New(e.m.deps, e.inv, nil, e.tax, e.periods, nil)
	code, _, _ := call(t, testRouter(m2), http.MethodPost, base+"commands/issueCreditNote", e.tenant, pIssuer, "", cbody(c, nil))
	require.Equal(t, http.StatusServiceUnavailable, code)
	e.nothingIssued(c, StateApproved)
	// and the retry after recovery takes number 1: no gap
	n := e.issueCredit(c)
	require.Equal(t, "CN-000001", n.DocumentNumber)
	require.Equal(t, 1, e.adjustments(c.CorrectionID))
}

// ── separation of duties ─────────────────────────────────────────────────────

func TestSoD_ApproverIsNotPreparer(t *testing.T) {
	e := newEnv(t)
	c := e.submit(e.create(pBoss, creditBody(e.entity, e.src, "100.00", nil)))
	code, body := e.cmd(pBoss, "approveCorrection", cbody(c, map[string]any{"content_hash": c.ContentHash}))
	require.Equal(t, http.StatusForbidden, code)
	require.Equal(t, kit.CodeSegregationOfDuties, errCode(t, body))
	require.Equal(t, 1, e.denials(c.CorrectionID, "approveCorrection"), "the refusal is audited")
	require.Equal(t, StatePendingApproval, e.get(c.CorrectionID).State)
	// the submitter (not the creator) is also barred
	c2 := e.create(pCreator, creditBody(e.entity, e.src, "100.00", nil))
	c2 = e.step(pBoss, "submitCorrectionForApproval", c2, nil)
	code, body = e.cmd(pBoss, "approveCorrection", cbody(c2, map[string]any{"content_hash": c2.ContentHash}))
	require.Equal(t, kit.CodeSegregationOfDuties, errCode(t, body))
	require.Equal(t, http.StatusForbidden, code)
	// the database refuses it too
	_, err := pool.Exec(context.Background(), `UPDATE ar06_corrections SET state = 'APPROVED', approved_content_hash = content_hash, approved_by = created_by, approved_at = now() WHERE correction_id = $1`, c.CorrectionID)
	require.Error(t, err)
	// a different approver succeeds; availability is derived from SoD
	code, b, _ := call(t, e.h, http.MethodGet, base+"getAvailableActions?correction_id="+c.CorrectionID, e.tenant, pBoss, "", nil)
	require.Equal(t, http.StatusOK, code)
	var acts struct {
		Actions []actionState `json:"actions"`
	}
	require.NoError(t, json.Unmarshal(b, &acts))
	for _, a := range acts.Actions {
		if a.Action == "ApproveCorrection" {
			require.False(t, a.Available)
			require.Contains(t, a.Reason, "neither created nor submitted")
		}
	}
	e.approve(c)
}

func TestSoD_MaterialCorrectionIssuerIsNotApprover(t *testing.T) {
	e := newEnv(t)
	big := sourceInvoice(e.entity, e.party)
	big.Lines = []kit.IssuedInvoiceLine{{LineNumber: 1, Description: "Big", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("40000"), Net: kit.MustAmount("40000.00"), Tax: kit.MustAmount("8000.00"), TaxCode: "STANDARD"}}
	big.Net, big.Tax, big.Gross = kit.MustAmount("40000.00"), kit.MustAmount("8000.00"), kit.MustAmount("48000.00")
	e.addSource(big)
	mk := func(price string, evidence bool) *Correction {
		extra := map[string]any{}
		if evidence {
			extra["evidence_id"] = "EV-1"
		}
		c := e.submit(e.create(pCreator, creditBody(e.entity, big, price, extra)))
		return e.step(pBoss, "approveCorrection", c, map[string]any{"content_hash": c.ContentHash})
	}
	m := mk("10000.00", true) // gross 12000 > 10000: material
	require.True(t, IsMaterial(m.GrossTotal))
	code, body := e.cmd(pBoss, "issueCreditNote", cbody(m, nil))
	require.Equal(t, http.StatusForbidden, code)
	require.Equal(t, kit.CodeSegregationOfDuties, errCode(t, body))
	require.Equal(t, 1, e.denials(m.CorrectionID, "issueCreditNote"))
	e.nothingIssued(m, StateApproved)
	require.Equal(t, "CN-000001", e.step(pIssuer, "issueCreditNote", m, nil).DocumentNumber)
	// below the threshold the approver may also issue
	s := mk("100.00", false)
	require.False(t, IsMaterial(s.GrossTotal))
	require.Equal(t, StateIssued, e.step(pBoss, "issueCreditNote", s, nil).State)
}

func TestEvidenceAndReasonRequired(t *testing.T) {
	e := newEnv(t)
	big := sourceInvoice(e.entity, e.party)
	big.Lines[0].Net = kit.MustAmount("5000.00")
	e.addSource(big)
	code, body := e.cmd(pCreator, "createCorrectionDraft", creditBody(e.entity, big, "1500.00", nil))
	require.Equal(t, http.StatusBadRequest, code)
	require.Equal(t, kit.CodeEvidenceRequired, errCode(t, body))
	// gross above the threshold even though the net is not: refused at submit
	d := e.create(pCreator, creditBody(e.entity, e.src, "950.00", nil)) // net 950 (no evidence needed), gross 1140 > 1000
	code, body = e.cmd(pCreator, "submitCorrectionForApproval", cbody(d, nil))
	require.Equal(t, http.StatusBadRequest, code)
	require.Equal(t, kit.CodeEvidenceRequired, errCode(t, body))
	// with evidence it passes; a debit note needs none
	e.submit(e.create(pCreator, creditBody(e.entity, e.src, "950.00", map[string]any{"evidence_id": "EV-2"})))
	e.submit(e.create(pCreator, debitBody(e.entity, e.src, "5000.00", nil)))
	// reason is mandatory for create and cancel
	b := creditBody(e.entity, e.src, "10.00", nil)
	delete(b, "reason_code")
	code, body = e.cmd(pCreator, "createCorrectionDraft", b)
	require.Equal(t, kit.CodeReasonRequired, errCode(t, body))
	require.Equal(t, http.StatusBadRequest, code)
	code, body = e.cmd(pCreator, "cancelUnissuedCorrection", cbody(d, nil))
	require.Equal(t, kit.CodeReasonRequired, errCode(t, body))
	require.Equal(t, http.StatusBadRequest, code)
}

// ── lifecycle, concurrency, cancel ───────────────────────────────────────────

func TestOptimisticConcurrencyAndTransitions(t *testing.T) {
	e := newEnv(t)
	d := e.create(pCreator, creditBody(e.entity, e.src, "100.00", nil))
	code, body := e.cmd(pCreator, "submitCorrectionForApproval", map[string]any{"correction_id": d.CorrectionID})
	require.Equal(t, http.StatusBadRequest, code)
	require.Equal(t, kit.CodeExpectedVersionMissing, errCode(t, body))
	code, body = e.cmd(pCreator, "submitCorrectionForApproval", map[string]any{"correction_id": d.CorrectionID, "expected_version": d.Version + 5})
	require.Equal(t, http.StatusConflict, code)
	require.Equal(t, kit.CodeStaleVersion, errCode(t, body))
	// illegal transitions
	code, body = e.cmd(pApprover, "approveCorrection", cbody(d, map[string]any{"content_hash": d.ContentHash}))
	require.Equal(t, http.StatusConflict, code)
	require.Equal(t, kit.CodeInvalidTransition, errCode(t, body))
	code, body = e.cmd(pIssuer, "issueCreditNote", cbody(d, nil))
	require.Equal(t, http.StatusConflict, code)
	require.Equal(t, kit.CodeInvalidTransition, errCode(t, body))
	// the approval is bound to the content the approver saw
	s := e.submit(d)
	code, body = e.cmd(pApprover, "approveCorrection", cbody(s, map[string]any{"content_hash": "deadbeef"}))
	require.Equal(t, http.StatusConflict, code)
	require.Equal(t, kit.CodeInvoiceApprovalInvalidated, errCode(t, body))
	// authorization is per action
	for _, c := range []struct {
		who, cmd string
		extra    map[string]any
	}{{pCreator, "approveCorrection", map[string]any{"content_hash": s.ContentHash}}, {pApprover, "issueCreditNote", nil},
		{pIssuer, "cancelUnissuedCorrection", map[string]any{"reason_code": "X"}}, {pNobody, "submitCorrectionForApproval", nil}} {
		code, _ = e.cmd(c.who, c.cmd, cbody(s, c.extra))
		require.Equal(t, http.StatusForbidden, code, c.who+" "+c.cmd)
	}
	// a body entity that disagrees with the stored one is refused
	code, _ = e.cmd(pCreator, "cancelUnissuedCorrection", cbody(s, map[string]any{"reason_code": "X", "legal_entity_id": uuid.NewString()}))
	require.Equal(t, http.StatusForbidden, code)
}

func TestCancelUnissuedCorrection_ConsumesNothing(t *testing.T) {
	e := newEnv(t)
	for _, stage := range []string{"draft", "pending", "approved"} {
		c := e.create(pCreator, creditBody(e.entity, e.src, "400.00", nil))
		if stage != "draft" {
			c = e.submit(c)
		}
		if stage == "approved" {
			c = e.approve(c)
		}
		x := e.step(pCreator, "cancelUnissuedCorrection", c, map[string]any{"reason_code": "WRONG_CUSTOMER"})
		require.Equal(t, StateCancelled, x.State, stage)
		require.Equal(t, "1250.5000", e.correctable(e.src.InvoiceID)["remaining_correctable_net"], stage)
		code, body := e.cmd(pIssuer, "issueCreditNote", cbody(x, nil))
		require.Equal(t, http.StatusConflict, code)
		require.Equal(t, kit.CodeInvalidTransition, errCode(t, body))
		_, err := pool.Exec(context.Background(), `UPDATE ar06_corrections SET reason_code = 'x' WHERE correction_id = $1`, x.CorrectionID)
		require.Error(t, err, "a cancelled correction is frozen")
		require.Equal(t, 1, e.events(x.CorrectionID, EventCancelled))
	}
	require.Zero(t, e.counter(SeriesCredit))
}

// ── numbering ────────────────────────────────────────────────────────────────

func TestNumbering_SeriesAreIndependentAndProfileDriven(t *testing.T) {
	e := newEnv(t)
	cn1 := e.issueCredit(e.approvedCredit("100.00"))
	dn1 := e.step(pIssuer, "issueDebitNote", e.approvedFrom(debitBody(e.entity, e.src, "50.00", nil)), nil)
	cn2 := e.issueCredit(e.approvedCredit("100.00"))
	require.Equal(t, []string{"CN-000001", "DN-000001", "CN-000002"}, []string{cn1.DocumentNumber, dn1.DocumentNumber, cn2.DocumentNumber})
	// another tenant starts its own sequence
	e2 := newEnv(t)
	require.Equal(t, "CN-000001", e2.issueCredit(e2.approvedCredit("100.00")).DocumentNumber)
	// a jurisdiction-controlled profile changes the format, not the gaplessness
	_, err := pool.Exec(context.Background(), `INSERT INTO ar06_numbering_profiles (tenant_id, legal_entity_id, series, prefix, number_width, created_by) VALUES ($1,$2,'CN','CRN/',4,'ops')`, e2.tenant, e2.entity)
	require.NoError(t, err)
	require.Equal(t, "CRN/0002", e2.issueCredit(e2.approvedCredit("100.00")).DocumentNumber)
	// duplicate numbers are impossible in the data
	_, err = pool.Exec(context.Background(), `UPDATE ar06_number_counters SET next_number = 1 WHERE tenant_id = $1`, e.tenant)
	require.Error(t, err, "a legal sequence never moves backwards")
	_, err = pool.Exec(context.Background(), `DELETE FROM ar06_number_counters WHERE tenant_id = $1`, e.tenant)
	require.Error(t, err, "a legal sequence is never deleted")
}

// ── replacement invoice / correction chain ───────────────────────────────────

func TestLinkReplacementInvoice(t *testing.T) {
	e := newEnv(t)
	b := creditBody(e.entity, e.src, "1000.00", map[string]any{"evidence_id": "EV-9"})
	b["lines"] = append(b["lines"].([]map[string]any), map[string]any{"source_line_no": 2, "description": "Onboarding", "quantity": "1", "unit_price": "250.50",
		"product_classification": "SOFTWARE", "supply_kind": "DIGITAL_SERVICES"})
	full := e.issueCredit(e.approvedFrom(b))
	require.Equal(t, e.src.Gross, full.GrossTotal, "the credit covers the whole source gross")
	require.Equal(t, "0.0000", e.correctable(e.src.InvoiceID)["remaining_correctable_gross"])

	rep := e.addSource(sourceInvoice(e.entity, e.party))
	rep.InvoiceNumber = "INV-000002"
	lb := cbody(full, map[string]any{"replacement_invoice_id": rep.InvoiceID, "reason_code": "REISSUE"})
	code, body := e.cmd(pLinker, "linkReplacementInvoice", lb)
	l := e.ok(code, body)
	require.Equal(t, full.Version+1, l.Version)
	require.Equal(t, full.IssuedContentHash, l.IssuedContentHash, "the issued note is not edited by the link")
	require.Contains(t, e.inv.linkLog(), e.src.InvoiceID+"|"+full.CorrectionID+":replacement|FULL_CORRECTION")
	// replay at the new version records nothing twice
	lb2 := cbody(l, map[string]any{"replacement_invoice_id": rep.InvoiceID, "reason_code": "REISSUE"})
	code, body = e.cmd(pLinker, "linkReplacementInvoice", lb2)
	require.Equal(t, l.Version, e.ok(code, body).Version)
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar06_replacement_links WHERE correction_id = $1`, full.CorrectionID))
	chainCode, chainBody := e.query("getCorrectionChain", "correction_id="+full.CorrectionID)
	require.Equal(t, http.StatusOK, chainCode)
	require.Contains(t, string(chainBody), rep.InvoiceID)
	// the chain is append-only
	_, err := pool.Exec(context.Background(), `DELETE FROM ar06_replacement_links WHERE correction_id = $1`, full.CorrectionID)
	require.Error(t, err)

	// refusals: a stale version, a replacement of another customer, a draft, the source itself, a debit note
	code, _ = e.cmd(pLinker, "linkReplacementInvoice", lb)
	require.Equal(t, http.StatusConflict, code)
	foreign := e.addSource(sourceInvoice(e.entity, uuid.NewString()))
	code, body = e.cmd(pLinker, "linkReplacementInvoice", cbody(l, map[string]any{"replacement_invoice_id": foreign.InvoiceID, "reason_code": "REISSUE"}))
	require.Equal(t, CodeCorrectionSourceInvalid, errCode(t, body))
	require.Equal(t, http.StatusUnprocessableEntity, code)
	_, body = e.cmd(pLinker, "linkReplacementInvoice", cbody(l, map[string]any{"replacement_invoice_id": e.src.InvoiceID, "reason_code": "REISSUE"}))
	require.Equal(t, CodeCorrectionSourceInvalid, errCode(t, body))
	d := e.create(pCreator, debitBody(e.entity, e.src, "1.00", nil))
	code, _ = e.cmd(pLinker, "linkReplacementInvoice", cbody(d, map[string]any{"replacement_invoice_id": rep.InvoiceID, "reason_code": "REISSUE"}))
	require.Equal(t, http.StatusConflict, code)
	code, _ = e.cmd(pCreator, "linkReplacementInvoice", lb2)
	require.Equal(t, http.StatusForbidden, code)

	// a partial credit is chained but does not mark the source fully corrected
	e2 := newEnv(t)
	part := e2.issueCredit(e2.approvedCredit("100.00"))
	rep2 := e2.addSource(sourceInvoice(e2.entity, e2.party))
	e2.ok(e2.cmd(pLinker, "linkReplacementInvoice", cbody(part, map[string]any{"replacement_invoice_id": rep2.InvoiceID, "reason_code": "REISSUE"})))
	for _, l := range e2.inv.linkLog() {
		require.NotContains(t, l, "FULL_CORRECTION")
	}
}

// ── e-invoice ────────────────────────────────────────────────────────────────

func TestEInvoiceSource(t *testing.T) {
	e := newEnv(t)
	e.m.einvoice = fakeEInvoice{required: true}
	d := e.approvedCredit("100.00")
	ctx := context.Background()
	credit := e.m.EInvoiceSource(KindCredit)
	_, err := credit.Get(ctx, e.tenant, d.CorrectionID)
	require.Error(t, err, "an unissued note can never be transmitted")
	n := e.issueCredit(d)
	require.Equal(t, EInvRequired, n.EInvoiceState)
	doc, err := credit.Get(ctx, e.tenant, n.CorrectionID)
	require.NoError(t, err)
	require.Equal(t, "CREDIT_NOTE", doc.DocumentType)
	require.Equal(t, "CN-000001", doc.Number)
	require.Equal(t, n.GrossTotal, doc.Gross)
	require.Equal(t, n.IssuedContentHash, doc.ContentHash)
	require.Len(t, doc.Lines, 1)
	require.NotEmpty(t, doc.TaxDeterminationID)
	_, err = e.m.EInvoiceSource(KindDebit).Get(ctx, e.tenant, n.CorrectionID)
	require.Error(t, err, "a credit note is not a debit note")
	_, err = credit.Get(ctx, uuid.NewString(), n.CorrectionID)
	require.Error(t, err, "another tenant sees nothing")

	c := kit.Caller{TenantID: e.tenant, PrincipalID: "system:ar05"}
	require.NoError(t, e.m.deps.DB.WithTx(ctx, e.tenant, func(tx pgx.Tx) error { return credit.SetEInvoiceState(ctx, tx, c, n.CorrectionID, "ACCEPTED") }))
	g := e.get(n.CorrectionID)
	require.Equal(t, "ACCEPTED", g.EInvoiceState)
	require.Equal(t, n.IssuedContentHash, g.IssuedContentHash)
	require.Equal(t, n.Version+1, g.Version)
	require.Error(t, e.m.deps.DB.WithTx(ctx, e.tenant, func(tx pgx.Tx) error { return credit.SetEInvoiceState(ctx, tx, c, n.CorrectionID, "BOGUS") }))
	require.NoError(t, e.m.deps.DB.WithTx(ctx, e.tenant, func(tx pgx.Tx) error { return credit.SetEInvoiceState(ctx, tx, c, n.CorrectionID, "ACCEPTED") }), "idempotent")
	require.Equal(t, g.Version, e.get(n.CorrectionID).Version)
	// the requirement lookup failing closes the issue
	e.m.einvoice = fakeEInvoice{err: errors.New("jurisdiction down")}
	x := e.approvedCredit("10.00")
	code, _ := e.cmd(pIssuer, "issueCreditNote", cbody(x, nil))
	require.Equal(t, http.StatusServiceUnavailable, code)
	e.nothingIssued(x, StateApproved)
}

// ── tenant isolation ─────────────────────────────────────────────────────────

func TestTenantIsolation(t *testing.T) {
	e := newEnv(t)
	n := e.issueCredit(e.approvedCredit("100.00"))
	d := e.create(pCreator, creditBody(e.entity, e.src, "10.00", nil))
	other := uuid.NewString()
	for _, q := range []string{"getCorrection", "getAvailableActions", "getCorrectionAccountingStatus"} {
		code, body := e.queryAs(other, pCreator, q, "correction_id="+n.CorrectionID)
		require.Equal(t, http.StatusNotFound, code, q)
		require.Equal(t, kit.CodeNotFound, errCode(t, body), q)
	}
	code, _ := e.queryAs(other, pCreator, "getCorrectionChain", "correction_id="+n.CorrectionID)
	require.Equal(t, http.StatusNotFound, code)
	code, body := e.queryAs(other, pCreator, "getCorrectionChain", "source_invoice_id="+e.src.InvoiceID)
	require.Equal(t, http.StatusOK, code)
	require.NotContains(t, string(body), n.CorrectionID, "another tenant's chain is empty")
	code, _ = e.queryAs(other, pCreator, "getCorrectableAmount", "source_invoice_id="+e.src.InvoiceID)
	require.Equal(t, http.StatusNotFound, code)
	for _, c := range []struct {
		who, cmd string
		extra    map[string]any
	}{{pCreator, "submitCorrectionForApproval", nil}, {pApprover, "approveCorrection", map[string]any{"content_hash": d.ContentHash}}, {pIssuer, "issueCreditNote", nil},
		{pCreator, "cancelUnissuedCorrection", map[string]any{"reason_code": "X"}}, {pLinker, "linkReplacementInvoice", map[string]any{"reason_code": "X", "replacement_invoice_id": uuid.NewString()}}} {
		code, _ := e.cmdAs(other, c.who, c.cmd, cbody(d, c.extra))
		require.Equal(t, http.StatusNotFound, code, c.cmd)
	}
	// and nothing changed for the owner
	require.Equal(t, StateDraft, e.get(d.CorrectionID).State)
	require.Equal(t, d.Version, e.get(d.CorrectionID).Version)
	// another tenant's own credit does not count against this tenant's source
	e2 := newEnv(t)
	e2.issueCredit(e2.approvedCredit("100.00"))
	require.Equal(t, "1150.5000", e.correctable(e.src.InvoiceID)["remaining_correctable_net"], "only this tenant's own issued credit (100.00) counts")
}

func TestGetAvailableActions_DerivedFromState(t *testing.T) {
	e := newEnv(t)
	actions := func(c *Correction, who string) map[string]actionState {
		code, b, _ := call(t, e.h, http.MethodGet, base+"getAvailableActions?correction_id="+c.CorrectionID, e.tenant, who, "", nil)
		require.Equal(t, http.StatusOK, code, string(b))
		var out struct {
			Actions []actionState `json:"actions"`
		}
		require.NoError(t, json.Unmarshal(b, &out))
		m := map[string]actionState{}
		for _, a := range out.Actions {
			m[a.Action] = a
		}
		return m
	}
	d := e.create(pCreator, creditBody(e.entity, e.src, "100.00", nil))
	a := actions(d, pCreator)
	require.True(t, a["SubmitCorrectionForApproval"].Available)
	require.True(t, a["CancelUnissuedCorrection"].Available)
	require.False(t, a["ApproveCorrection"].Available)
	require.False(t, actions(d, pNobody)["SubmitCorrectionForApproval"].Available, "no permission")
	p := e.submit(d)
	require.False(t, actions(p, pCreator)["ApproveCorrection"].Available, "the preparer cannot approve")
	require.True(t, actions(p, pApprover)["ApproveCorrection"].Available)
	ap := e.approve(p)
	require.True(t, actions(ap, pIssuer)["IssueCreditNote"].Available)
	require.False(t, actions(ap, pIssuer)["IssueDebitNote"].Available)
	require.False(t, actions(ap, pApprover)["IssueCreditNote"].Available, "no issue permission")
	e.issueCredit(ap)
}
