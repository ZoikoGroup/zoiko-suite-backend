//go:build integration

// Real-Postgres tests for AR-04. Run (serially):
//
//	go test -tags=integration -count=1 -p 1 -timeout=600s ./internal/ar04/...
//
// The embedded superuser bypasses RLS, so isolation tests prove the explicit tenant_id
// predicates, not the policies. Spec section 18 scenarios carry their number: TestNeg08 ... TestNeg35;
// the AR-04 table's four rows (replayed issue, tax changed after approval, number collision, delivery
// failure) are TestNeg11, TestNeg09, TestNeg10 and TestNeg13.
package ar04

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/kit/kittest"
	"zoiko.io/accounts-receivable-svc/internal/taxdet"
)

var pool *pgxpool.Pool

func TestMain(m *testing.M) {
	var stop func()
	pool, stop = kittest.Start("ar04_test", 17600)
	if _, err := pool.Exec(context.Background(), `CREATE TABLE ar04_test_openitems (source_document_id text NOT NULL);
		CREATE TABLE ar04_test_orders_log (invoice_id text NOT NULL, entry text NOT NULL)`); err != nil {
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
	prof    *fakeProfiles
	items   *fakeOpenItems
	orders  *fakeOrders
	periods *fakePeriods
	tax     *fakeTax
}

// newEnv wires every collaborator with a fake (AR-07, tax, period gate wired; AR-03 only when a test
// installs it). Each test gets its own tenant and entity so numbering and counters start fresh.
func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, tenant: uuid.NewString(), entity: uuid.NewString(), party: uuid.NewString(), prof: &fakeProfiles{}, items: &fakeOpenItems{},
		periods: &fakePeriods{}, tax: newFakeTax()}
	deps := testDeps(&fakeAuthz{grants: standardGrants()})
	deps.DB = kit.NewDB(pool)
	e.m = New(deps, e.prof, nil, e.items, e.tax, e.periods, nil)
	e.h = testRouter(e.m)
	return e
}

func (e *env) cmdAs(tenant, principal, name string, body map[string]any) (int, []byte) {
	code, b, _ := call(e.t, e.h, http.MethodPost, base+"commands/"+name, tenant, principal, "", body)
	return code, b
}

func (e *env) cmd(principal, name string, body map[string]any) (int, []byte) {
	return e.cmdAs(e.tenant, principal, name, body)
}

func (e *env) query(principal, name, qs string) (int, []byte) {
	code, b, _ := call(e.t, e.h, http.MethodGet, base+name+"?"+qs, e.tenant, principal, "", nil)
	return code, b
}

func (e *env) ok(code int, body []byte) *Invoice {
	e.t.Helper()
	require.Less(e.t, code, 300, string(body))
	return asInvoice(e.t, body)
}

// create makes a DRAFT invoice (default: 2 x 500.00 + 1 x 250.50 = 1250.50 USD net, two tax groups).
func (e *env) create(principal string, extra map[string]any) *Invoice {
	e.t.Helper()
	b := createBody(e.entity, e.party)
	for k, v := range extra {
		b[k] = v
	}
	code, body := e.cmd(principal, "createInvoiceDraft", b)
	require.Equal(e.t, http.StatusCreated, code, string(body))
	return asInvoice(e.t, body)
}

func ibody(inv *Invoice, extra map[string]any) map[string]any {
	b := map[string]any{"invoice_id": inv.InvoiceID, "expected_version": inv.Version}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

func (e *env) mk(principal, name string, inv *Invoice, extra map[string]any) (int, []byte) {
	return e.cmd(principal, name, ibody(inv, extra))
}

func (e *env) step(principal, name string, inv *Invoice, extra map[string]any) *Invoice {
	e.t.Helper()
	code, body := e.mk(principal, name, inv, extra)
	return e.ok(code, body)
}

func (e *env) get(id string) *Invoice {
	e.t.Helper()
	code, body := e.query(pNobody, "getInvoice", "invoice_id="+id)
	return e.ok(code, body)
}

func (e *env) submit(inv *Invoice) *Invoice {
	return e.step(pCreator, "submitInvoiceForApproval", inv, nil)
}

func (e *env) approveAs(principal string, inv *Invoice) *Invoice {
	return e.step(principal, "approveInvoice", inv, map[string]any{"content_hash": inv.ContentHash})
}

func (e *env) approved(extra map[string]any) *Invoice {
	return e.approveAs(pApprover, e.submit(e.create(pCreator, extra)))
}

func (e *env) issue(principal string, inv *Invoice) *Invoice {
	return e.step(principal, "issueInvoice", inv, nil)
}

func (e *env) issued() *Invoice { return e.issue(pIssuer, e.approved(nil)) }

func (e *env) caller() kit.Caller {
	return kit.Caller{TenantID: e.tenant, PrincipalID: "ar-peer", LegalEntityID: e.entity, CorrelationID: "corr-" + uuid.NewString()[:8]}
}

func (e *env) tx(fn func(tx pgx.Tx) error) error {
	return e.m.deps.DB.WithTx(context.Background(), e.tenant, fn)
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

func (e *env) audits(id string) int {
	return e.count(`SELECT count(*) FROM ar_audit_events WHERE tenant_id = $1 AND aggregate_id = $2`, e.tenant, id)
}

func (e *env) denials(id, command string) int {
	return e.count(`SELECT count(*) FROM ar_audit_events WHERE tenant_id = $1 AND aggregate_id = $2 AND command = $3`, e.tenant, id, command+".denied")
}

func (e *env) postings(id string) int {
	return e.count(`SELECT count(*) FROM ar_accounting_posting_requests WHERE tenant_id = $1 AND source_event_id = $2`, e.tenant, id)
}

func (e *env) historyCmds(id string) []string {
	rows, err := pool.Query(context.Background(), `SELECT command FROM ar04_invoice_history WHERE tenant_id = $1 AND invoice_id = $2 ORDER BY invoice_version`, e.tenant, id)
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

// counter returns the entity's next_number, or 0 when no number was ever allocated.
func (e *env) counter() int {
	var n int
	err := pool.QueryRow(context.Background(), `SELECT next_number FROM ar04_number_counters WHERE tenant_id = $1 AND legal_entity_id = $2 AND series = $3`,
		e.tenant, e.entity, DefaultSeries).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0
	}
	require.NoError(e.t, err)
	return n
}

// row returns the invoice row as JSON (every column), for "nothing else changed" comparisons.
func (e *env) row(id string) map[string]any {
	e.t.Helper()
	var raw []byte
	require.NoError(e.t, pool.QueryRow(context.Background(), `SELECT to_jsonb(i) FROM ar04_invoices i WHERE invoice_id = $1`, id).Scan(&raw))
	var m map[string]any
	require.NoError(e.t, json.Unmarshal(raw, &m))
	return m
}

func (e *env) exec(q string, args ...any) error {
	_, err := pool.Exec(context.Background(), q, args...)
	return err
}

func pgMsg(err error) (code, msg string) {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code, pe.Message
	}
	return "", ""
}

// requireRefused asserts a direct SQL statement was refused by the database with the given SQLSTATE
// and (optional) message fragment.
func requireRefused(t *testing.T, err error, sqlstate, fragment string) {
	t.Helper()
	require.Error(t, err)
	code, msg := pgMsg(err)
	require.Equal(t, sqlstate, code, err.Error())
	require.Contains(t, msg, fragment)
}

func lineBody(desc, qty, price, class, kind string) map[string]any {
	return map[string]any{"description": desc, "quantity": qty, "unit_price": price, "tax_category": "STANDARD", "product_classification": class, "supply_kind": kind}
}

func mustAmount(s string) kit.Amount { return kit.MustAmount(s) }

// ── lifecycle, evidence, events, accounting ──────────────────────────────────

func TestLifecycle_EndToEnd(t *testing.T) {
	e := newEnv(t)
	inv := e.create(pCreator, map[string]any{"reason_code": "NEW_BILLING", "evidence_id": "CONTRACT-1"})
	require.Equal(t, StateDraft, inv.State)
	require.Equal(t, 1, inv.Version)
	require.Equal(t, pCreator, inv.CreatedBy)
	require.Equal(t, InitialDimensions(), inv.Dimensions)
	require.Equal(t, "NOT_QUEUED", inv.AccountingState)
	require.Empty(t, inv.InvoiceNumber)
	require.Equal(t, mustAmount("1250.50"), inv.NetTotal)
	require.Zero(t, inv.TaxTotal)
	require.Equal(t, mustAmount("1250.50"), inv.GrossTotal)
	require.Len(t, inv.Lines, 2)
	require.Len(t, inv.ContentHash, 64)
	require.Empty(t, inv.TaxGroups)
	require.Equal(t, 1, e.events(inv.InvoiceID, EventInvoiceDraftCreated))

	// An approval cannot precede submission; issue cannot precede approval.
	code, body := e.mk(pBoss, "approveInvoice", inv, map[string]any{"content_hash": inv.ContentHash})
	require.Equal(t, http.StatusConflict, code, string(body))
	require.Equal(t, kit.CodeInvalidTransition, errCode(t, body))
	code, body = e.mk(pBoss, "issueInvoice", inv, nil)
	require.Equal(t, http.StatusConflict, code, string(body))
	require.Equal(t, kit.CodeInvalidTransition, errCode(t, body))

	// Submit: one tax determination per tax group, obtained from the TAX domain and stored with provenance.
	inv = e.submit(inv)
	require.Equal(t, StatePendingApproval, inv.State)
	require.Equal(t, pCreator, inv.SubmittedBy)
	require.Len(t, inv.TaxGroups, 2)
	require.Len(t, e.tax.requests, 2)
	g1, g2 := inv.TaxGroups[0], inv.TaxGroups[1] // CONSULTING/SERVICES, SOFTWARE/DIGITAL_SERVICES
	require.Equal(t, "CONSULTING", g1.ProductClassification)
	require.Equal(t, mustAmount("250.50"), g1.Net)
	require.Equal(t, mustAmount("50.10"), g1.Tax)
	require.Equal(t, mustAmount("1000"), g2.Net)
	require.Equal(t, mustAmount("200.00"), g2.Tax)
	for _, g := range inv.TaxGroups {
		require.NotEmpty(t, g.DeterminationID)
		require.Equal(t, "snap-2026.1", g.Provenance, "the exact rule-pack provenance is stored")
		require.Equal(t, "CALCULATED", g.Status)
	}
	require.Equal(t, mustAmount("250.10"), inv.TaxTotal)
	require.Equal(t, mustAmount("1500.60"), inv.GrossTotal)
	require.Equal(t, inv.NetTotal.Add(inv.TaxTotal), inv.GrossTotal)
	var lineNet, lineTax kit.Amount
	for _, l := range inv.Lines {
		lineNet, lineTax = lineNet.Add(l.Net), lineTax.Add(l.Tax)
	}
	require.Equal(t, inv.NetTotal, lineNet)
	require.Equal(t, inv.TaxTotal, lineTax)
	for _, r := range e.tax.requests {
		require.Equal(t, e.entity, r.SellerPartyID, "the seller is the verified legal entity")
		require.Equal(t, e.party, r.BuyerPartyID, "the buyer is the customer")
		require.Equal(t, "2026-10-10", r.SupplyDate)
		require.Equal(t, "GB", r.JurisdictionID)
		require.Equal(t, "B2B", r.SupplyType)
		require.Contains(t, r.TransactionID, "ar04:"+inv.InvoiceID)
	}

	inv = e.approveAs(pApprover, inv)
	require.Equal(t, StateApproved, inv.State)
	require.Equal(t, pApprover, inv.ApprovedBy)
	require.Equal(t, inv.ContentHash, inv.ApprovedHash)
	require.Equal(t, 1, e.events(inv.InvoiceID, EventInvoiceApproved))

	require.Zero(t, e.counter(), "no legal number exists before issue")
	inv = e.issue(pIssuer, inv)
	require.Equal(t, StateIssued, inv.State)
	require.Equal(t, 4, inv.Version)
	require.Equal(t, "INV-000001", inv.InvoiceNumber)
	require.Equal(t, DefaultSeries, inv.NumberingSeries)
	require.EqualValues(t, 1, inv.SequenceNo)
	require.Equal(t, "2026-10-12", inv.IssueDate)
	require.Equal(t, "2026-11-11", inv.DueDate, "issue date + 30 payment-terms days")
	require.Equal(t, "NET30", inv.PaymentTermsCode)
	require.Equal(t, "QUEUED", inv.AccountingState)
	require.Equal(t, EInvNotRequired, inv.EInvoice)
	require.Equal(t, pIssuer, inv.IssuedBy)
	require.Len(t, inv.IssuedContentHash, 64)
	require.Equal(t, "OI-"+inv.InvoiceID, inv.OpenItemID)
	require.Equal(t, 2, e.counter(), "the counter holds the NEXT number")

	// The receivable was created from the issued document (inside the issue transaction).
	require.Len(t, e.items.created, 1)
	oi := e.items.created[0]
	require.Equal(t, kit.DocInvoice, oi.DocumentKind)
	require.Equal(t, inv.InvoiceID, oi.SourceDocumentID)
	require.Equal(t, "INV-000001", oi.DocumentNumber)
	require.Equal(t, mustAmount("1500.60"), oi.OriginalAmount)
	require.Equal(t, e.party, oi.CustomerPartyID)
	require.Equal(t, "2026-11-11", day(oi.DueDate))
	require.Equal(t, 1, e.periods.calls)
	require.Equal(t, []string{"2026-10-12"}, e.periods.dates)

	// Invariant 14: the accounting consequence is a queued ACC-04 event, balanced, with mapping keys only.
	require.Equal(t, 1, e.postings(inv.InvoiceID))
	var payload []byte
	var srcType, srcID, status string
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT request_payload, source_type, source_id, status FROM ar_accounting_posting_requests
		WHERE tenant_id = $1 AND source_event_id = $2`, e.tenant, inv.InvoiceID).Scan(&payload, &srcType, &srcID, &status))
	require.Equal(t, [3]string{kit.SourceInvoice, inv.InvoiceID, kit.PostingPending}, [3]string{srcType, srcID, status})
	var ev struct {
		FiscalPeriod string `json:"fiscal_period"`
		SourceEvent  string `json:"source_event_id"`
		Currency     string `json:"transaction_currency"`
		PostingDate  string `json:"posting_date"`
		Lines        []struct {
			Key    string  `json:"mapping_key"`
			Debit  float64 `json:"debit_amount"`
			Credit float64 `json:"credit_amount"`
		} `json:"lines"`
	}
	require.NoError(t, json.Unmarshal(payload, &ev))
	require.Equal(t, "2026-10", ev.FiscalPeriod)
	require.Equal(t, inv.InvoiceID, ev.SourceEvent)
	require.Equal(t, "USD", ev.Currency)
	require.Equal(t, "2026-10-12", ev.PostingDate)
	require.Len(t, ev.Lines, 3)
	require.Equal(t, MappingReceivable, ev.Lines[0].Key)
	require.InDelta(t, 1500.60, ev.Lines[0].Debit, 1e-9)
	require.Equal(t, MappingRevenue, ev.Lines[1].Key)
	require.InDelta(t, 1250.50, ev.Lines[1].Credit, 1e-9)
	require.Equal(t, MappingTaxOutput, ev.Lines[2].Key)
	require.InDelta(t, 250.10, ev.Lines[2].Credit, 1e-9)
	require.Equal(t, 1, e.events(inv.InvoiceID, EventCustomerInvoiceIssued))

	// Evidence: a history row and an audit row per command, versions strictly sequential.
	require.Equal(t, []string{"createInvoiceDraft", "submitInvoiceForApproval", "approveInvoice", "issueInvoice"}, e.historyCmds(inv.InvoiceID))
	require.Equal(t, 4, e.audits(inv.InvoiceID))
	code, body = e.query(pNobody, "getInvoiceHistory", "invoice_id="+inv.InvoiceID)
	require.Equal(t, http.StatusOK, code, string(body))
	var hs struct {
		Items     []HistoryEntry   `json:"items"`
		Approvals []ApprovalRecord `json:"approvals"`
	}
	require.NoError(t, jsonDecode(body, &hs))
	require.Len(t, hs.Items, 4)
	for i, it := range hs.Items {
		require.Equal(t, i+1, it.InvoiceVersion)
	}
	require.Equal(t, "CONTRACT-1", hs.Items[0].EvidenceID)
	require.Equal(t, pApprover, hs.Items[2].ApproverID)
	require.Equal(t, StateIssued, hs.Items[3].LifecycleAfter)
	require.Len(t, hs.Approvals, 1)
	require.Equal(t, pApprover, hs.Approvals[0].ApprovedBy)

	// Lookup by legal number; as-issued sealed representation; the kit port.
	code, body = e.query(pNobody, "getInvoice", "legal_entity_id="+e.entity+"&invoice_number=INV-000001")
	require.Equal(t, http.StatusOK, code, string(body))
	require.Equal(t, inv.InvoiceID, asInvoice(t, body).InvoiceID)
	code, body = e.query(pNobody, "getInvoiceAsIssued", "invoice_id="+inv.InvoiceID)
	require.Equal(t, http.StatusOK, code, string(body))
	var as struct {
		Hash      string          `json:"issued_content_hash"`
		Verified  bool            `json:"integrity_verified"`
		Canonical string          `json:"canonical_text"`
		Rep       json.RawMessage `json:"representation"`
	}
	require.NoError(t, jsonDecode(body, &as))
	require.True(t, as.Verified)
	require.Equal(t, inv.IssuedContentHash, as.Hash)
	require.Equal(t, HashOf(as.Canonical), as.Hash, "the stored text hashes to the seal")
	var doc IssuedDocument
	require.NoError(t, json.Unmarshal(as.Rep, &doc))
	require.Equal(t, "INV-000001", doc.InvoiceNumber)
	require.Equal(t, mustAmount("1500.60"), doc.Gross)
	require.Len(t, doc.TaxGroups, 2)
	require.Equal(t, "snap-2026.1", doc.TaxGroups[0].Provenance)
	issued, err := e.m.GetIssued(context.Background(), e.tenant, inv.InvoiceID)
	require.NoError(t, err)
	require.Equal(t, "INV-000001", issued.InvoiceNumber)
	require.Equal(t, inv.IssuedContentHash, issued.ContentHash)
	require.Equal(t, mustAmount("1500.60"), issued.Gross)
	require.Len(t, issued.Lines, 2)
	require.Contains(t, issued.TaxDeterminationID, ",", "two tax groups: both determination ids")

	// The legacy table is untouched: AR-04 has its own tables.
	require.Zero(t, e.count(`SELECT count(*) FROM customer_invoices WHERE tenant_id = $1`, e.tenant))
}

func TestSecondInvoiceGetsTheNextNumber_PerEntityAndTenantSequences(t *testing.T) {
	e := newEnv(t)
	a := e.issued()
	b := e.issued()
	require.Equal(t, "INV-000001", a.InvoiceNumber)
	require.Equal(t, "INV-000002", b.InvoiceNumber)
	// A different legal entity of the same tenant has its own sequence.
	e2 := &env{t: t, m: e.m, h: e.h, tenant: e.tenant, entity: uuid.NewString(), party: e.party, prof: e.prof, items: e.items, periods: e.periods, tax: e.tax}
	c := e2.issued()
	require.Equal(t, "INV-000001", c.InvoiceNumber)
	// So does another tenant.
	e3 := newEnv(t)
	require.Equal(t, "INV-000001", e3.issued().InvoiceNumber)
}

func TestNumberingProfileSuppliesTheFormat(t *testing.T) {
	e := newEnv(t)
	require.NoError(t, e.exec(`INSERT INTO ar04_numbering_profiles (tenant_id, legal_entity_id, series, prefix, number_width, rule_reference, created_by)
		VALUES ($1,$2,'INV','FA/26/',8,'JURISDICTION-PACK-DE-2026','ops')`, e.tenant, e.entity))
	require.Equal(t, "FA/26/00000001", e.issued().InvoiceNumber)
	require.Equal(t, "FA/26/00000002", e.issued().InvoiceNumber)
	// A malformed profile is refused by the table, never half-applied.
	requireRefused(t, e.exec(`INSERT INTO ar04_numbering_profiles (tenant_id, legal_entity_id, series, prefix, number_width, created_by)
		VALUES ($1,$2,'X','bad prefix!',6,'ops')`, e.tenant, uuid.NewString()), "23514", "")
}

// ── §18 #8 tax result missing at issue ───────────────────────────────────────

func TestNeg08_TaxResultMissingAtIssue(t *testing.T) {
	e := newEnv(t)
	inv := e.approved(nil)
	e.tax.notFound[inv.TaxGroups[0].DeterminationID] = true
	code, body := e.mk(pIssuer, "issueInvoice", inv, nil)
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	require.Equal(t, kit.CodeTaxDeterminationMissing, errCode(t, body))
	after := e.get(inv.InvoiceID)
	require.Equal(t, StateApproved, after.State)
	require.Equal(t, inv.Version, after.Version, "the refusal preserves the safe state")
	require.Empty(t, after.InvoiceNumber)
	require.Zero(t, e.counter(), "no number was allocated")
	require.Zero(t, e.postings(inv.InvoiceID))
	require.Zero(t, e.events(inv.InvoiceID, EventCustomerInvoiceIssued))
	require.Zero(t, e.items.count(), "no receivable")
	require.Zero(t, e.periods.calls)

	// The TAX service being down is not "no tax": issue fails closed, the invoice stays approved.
	e.tax.notFound = map[string]bool{}
	e.tax.getErr = taxdet.ErrUnavailable
	code, body = e.mk(pIssuer, "issueInvoice", inv, nil)
	require.Equal(t, http.StatusServiceUnavailable, code, string(body))
	require.Equal(t, kit.CodeDependencyUnavailable, errCode(t, body))
	require.Equal(t, StateApproved, e.get(inv.InvoiceID).State)
	require.Zero(t, e.counter())
	e.tax.getErr = nil
	require.Equal(t, "INV-000001", e.issue(pIssuer, inv).InvoiceNumber, "nothing was consumed by the failed attempts")

	// The same discipline at submit: no usable determination, no approval path.
	d := e.create(pCreator, nil)
	e.tax.detErr = errors.Join(taxdet.ErrRejected, errors.New("unknown product classification"))
	code, body = e.mk(pCreator, "submitInvoiceForApproval", d, nil)
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	require.Equal(t, kit.CodeTaxDeterminationMissing, errCode(t, body))
	e.tax.detErr = taxdet.ErrUnavailable
	code, body = e.mk(pCreator, "submitInvoiceForApproval", d, nil)
	require.Equal(t, http.StatusServiceUnavailable, code, string(body))
	e.tax.detErr = nil
	require.Equal(t, StateDraft, e.get(d.InvoiceID).State)
	require.Equal(t, d.Version, e.get(d.InvoiceID).Version)
	require.Zero(t, e.count(`SELECT count(*) FROM ar04_invoice_tax_determinations WHERE invoice_id = $1`, d.InvoiceID))
	// AR never improvises tax: with no TAX client wired, submit is refused.
	e.m.tax = nil
	code, body = e.mk(pCreator, "submitInvoiceForApproval", d, nil)
	require.Equal(t, http.StatusServiceUnavailable, code, string(body))
	e.m.tax = e.tax
}

// ── §18 #9 (AR-04 table #2) tax result changed after approval ────────────────

func TestNeg09_TaxResultChangedAfterApproval(t *testing.T) {
	e := newEnv(t)
	cases := map[string]func(inv *Invoice){
		"tax amount changed": func(inv *Invoice) {
			e.tax.overrideTax[inv.TaxGroups[1].DeterminationID] = mustAmount("210.00")
		},
		"provenance changed": func(inv *Invoice) {
			e.tax.store[inv.TaxGroups[0].DeterminationID].TaxLogicSnapshotID = "snap-2026.2"
		},
		"determination reversed": func(inv *Invoice) {
			e.tax.store[inv.TaxGroups[0].DeterminationID].Status = "REVERSED"
		},
		"entity changed": func(inv *Invoice) {
			e.tax.store[inv.TaxGroups[0].DeterminationID].LegalEntityID = uuid.NewString()
		},
		"currency changed": func(inv *Invoice) {
			e.tax.store[inv.TaxGroups[0].DeterminationID].Currency = "EUR"
		},
	}
	for name, change := range cases {
		inv := e.approved(nil)
		change(inv)
		before := e.counter()
		code, body := e.mk(pIssuer, "issueInvoice", inv, nil)
		require.Equal(t, http.StatusConflict, code, name+": "+string(body))
		require.Equal(t, kit.CodeInvoiceApprovalInvalidated, errCode(t, body), name)
		after := e.get(inv.InvoiceID)
		require.Equal(t, StateDraft, after.State, name+": the invoice returns to draft")
		require.Empty(t, after.ApprovedHash, name)
		require.Empty(t, after.ApprovedBy, name)
		require.Empty(t, after.TaxGroups, name+": the stale tax result is discarded")
		require.Zero(t, after.TaxTotal, name)
		require.Equal(t, after.NetTotal, after.GrossTotal, name)
		for _, l := range after.Lines {
			require.Zero(t, l.Tax, name)
		}
		require.Empty(t, after.InvoiceNumber, name)
		require.Equal(t, before, e.counter(), name+": no number was allocated")
		require.Zero(t, e.postings(inv.InvoiceID), name)
		require.Zero(t, e.events(inv.InvoiceID, EventCustomerInvoiceIssued), name)
		require.Equal(t, 1, e.events(inv.InvoiceID, EventInvoiceApprovalInvalidated), name)
		cmds := e.historyCmds(inv.InvoiceID)
		require.Equal(t, "invalidateApproval", cmds[len(cmds)-1], name)
		var invalidation, why string
		require.NoError(t, pool.QueryRow(context.Background(), `SELECT new_values->>'invalidation', new_values->>'change' FROM ar04_invoice_history
			WHERE invoice_id = $1 AND command = 'invalidateApproval'`, inv.InvoiceID).Scan(&invalidation, &why))
		require.Equal(t, "TAX_RESULT_CHANGED_AFTER_APPROVAL", invalidation, name)
		require.NotEmpty(t, why, name)
		// The old approval cannot be reused: the invoice must be re-submitted (fresh determinations) and re-approved.
		code, body = e.mk(pBoss, "approveInvoice", after, map[string]any{"content_hash": inv.ContentHash})
		require.Equal(t, http.StatusConflict, code, name+string(body))
		re := e.approveAs(pApprover, e.submit(after))
		require.Equal(t, StateApproved, re.State, name)
		require.NotEqual(t, inv.ContentHash, re.ContentHash, name+": new determinations, new hash")
		e.tax.reset()
		require.Equal(t, StateIssued, e.issue(pIssuer, re).State, name)
	}
}

func TestApprovedContentTamperedDirectlyIsDetectedAtIssue(t *testing.T) {
	e := newEnv(t)
	inv := e.approved(nil)
	// Pre-issue lines are writable by the database; the approval hash still catches any change.
	require.NoError(t, e.exec(`UPDATE ar04_invoice_lines SET net_amount = net_amount + 1, unit_price = unit_price + 1 WHERE invoice_id = $1 AND line_no = 1`, inv.InvoiceID))
	code, body := e.mk(pIssuer, "issueInvoice", inv, nil)
	require.Equal(t, http.StatusConflict, code, string(body))
	require.Equal(t, kit.CodeInvoiceApprovalInvalidated, errCode(t, body))
	require.Equal(t, StateDraft, e.get(inv.InvoiceID).State)
	require.Zero(t, e.counter())
}

// ── §18 #10 (AR-04 table #3) invoice number collision under concurrent issue ─

func TestNeg10_InvoiceNumberCollisionUnderConcurrentIssue(t *testing.T) {
	e := newEnv(t)
	const n = 12
	invs := make([]*Invoice, n)
	for i := range invs {
		invs[i] = e.approved(nil)
	}
	type result struct {
		code int
		body []byte
	}
	res := make([]result, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range invs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			code, body, _ := call(t, e.h, http.MethodPost, base+"commands/issueInvoice", e.tenant, pIssuer, "", ibody(invs[i], nil))
			res[i] = result{code, body}
		}(i)
	}
	close(start)
	wg.Wait()
	var numbers []string
	for i, r := range res {
		require.Equal(t, http.StatusOK, r.code, string(r.body))
		numbers = append(numbers, asInvoice(t, r.body).InvoiceNumber)
		_ = i
	}
	sort.Strings(numbers)
	want := make([]string, n)
	for i := range want {
		want[i] = fmt.Sprintf("INV-%06d", i+1)
	}
	require.Equal(t, want, numbers, "distinct, gapless, no collision")
	require.Equal(t, n+1, e.counter())
	require.Equal(t, n, e.items.count())
	require.Equal(t, n, e.count(`SELECT count(DISTINCT invoice_number) FROM ar04_invoices WHERE tenant_id = $1 AND invoice_number IS NOT NULL`, e.tenant))

	// The same invoice raced by several issuers (different keys): exactly one wins, the rest are INVOICE_ALREADY_ISSUED.
	one := e.approved(nil)
	codes := make([]int, 6)
	bodies := make([][]byte, 6)
	start = make(chan struct{})
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			codes[i], bodies[i], _ = call(t, e.h, http.MethodPost, base+"commands/issueInvoice", e.tenant, pIssuer, "", ibody(one, nil))
		}(i)
	}
	close(start)
	wg.Wait()
	wins := 0
	for i, c := range codes {
		if c == http.StatusOK {
			wins++
			continue
		}
		require.Equal(t, http.StatusConflict, c, string(bodies[i]))
		require.Contains(t, []string{kit.CodeInvoiceAlreadyIssued, kit.CodeStaleVersion}, errCode(t, bodies[i]))
	}
	require.Equal(t, 1, wins)
	require.Equal(t, n+2, e.counter(), "one number consumed for the raced invoice")
	require.Equal(t, 1, e.postings(one.InvoiceID))
	require.Equal(t, 1, e.events(one.InvoiceID, EventCustomerInvoiceIssued))

	// Backstops: the sequence never moves backwards, and the unique index refuses a duplicate number.
	requireRefused(t, e.exec(`UPDATE ar04_number_counters SET next_number = 1 WHERE tenant_id = $1`, e.tenant), "P0001", "AR04_COUNTER_MONOTONIC")
	requireRefused(t, e.exec(`DELETE FROM ar04_number_counters WHERE tenant_id = $1`, e.tenant), "P0001", "AR04_COUNTER_NOT_DELETABLE")
	third := e.approved(nil)
	ctx := context.Background()
	c, err := pool.Begin(ctx)
	require.NoError(t, err)
	// Simulate a corrupted counter (bypassing triggers as a superuser can) and prove the index still refuses a duplicate.
	_, err = c.Exec(ctx, `SET LOCAL session_replication_role = replica`)
	require.NoError(t, err)
	_, err = c.Exec(ctx, `UPDATE ar04_number_counters SET next_number = 1 WHERE tenant_id = $1 AND legal_entity_id = $2`, e.tenant, e.entity)
	require.NoError(t, err)
	require.NoError(t, c.Commit(ctx))
	code, body := e.mk(pIssuer, "issueInvoice", third, nil)
	require.Equal(t, http.StatusConflict, code, string(body))
	require.Equal(t, kit.CodeInvoiceNumberCollision, errCode(t, body))
	require.Equal(t, StateApproved, e.get(third.InvoiceID).State, "nothing was issued")
	require.Zero(t, e.postings(third.InvoiceID))
	require.Equal(t, n+1, e.count(`SELECT count(*) FROM ar04_invoices WHERE tenant_id = $1 AND invoice_number IS NOT NULL`, e.tenant))
}

// ── §18 #11 (AR-04 table #1) issue command replayed after timeout ────────────

func TestNeg11_IssueReplayedAfterTimeout(t *testing.T) {
	e := newEnv(t)
	inv := e.approved(nil)
	key := uuid.NewString()

	// The first attempt times out half way (the period service is down): nothing is consumed and the key is released.
	e.periods.setErr(kit.Unavailable("accounting period service unavailable"))
	code, body, _ := call(t, e.h, http.MethodPost, base+"commands/issueInvoice", e.tenant, pIssuer, key, ibody(inv, nil))
	require.Equal(t, http.StatusServiceUnavailable, code, string(body))
	require.Zero(t, e.counter(), "a failed attempt consumes no number")
	require.Equal(t, StateApproved, e.get(inv.InvoiceID).State)
	e.periods.setErr(nil)

	// The retry with the same key issues exactly once.
	code, body, hdr := call(t, e.h, http.MethodPost, base+"commands/issueInvoice", e.tenant, pIssuer, key, ibody(inv, nil))
	require.Equal(t, http.StatusOK, code, string(body))
	require.Empty(t, hdr.Get("Idempotent-Replay"))
	first := asInvoice(t, body)
	require.Equal(t, "INV-000001", first.InvoiceNumber)

	// A replay after the (lost) response returns the stored result: same number, no second effect.
	code, body2, hdr := call(t, e.h, http.MethodPost, base+"commands/issueInvoice", e.tenant, pIssuer, key, ibody(inv, nil))
	require.Equal(t, http.StatusOK, code, string(body2))
	require.Equal(t, "true", hdr.Get("Idempotent-Replay"))
	require.JSONEq(t, string(body), string(body2))
	require.Equal(t, 2, e.counter())
	require.Equal(t, 1, e.items.count())
	require.Equal(t, 1, e.postings(inv.InvoiceID))
	require.Equal(t, 1, e.events(inv.InvoiceID, EventCustomerInvoiceIssued))
	require.Equal(t, 1, e.periods.calls-1, "the replay never re-ran the issue")

	// Issuing the already-issued invoice with a DIFFERENT key (stale or current version) is INVOICE_ALREADY_ISSUED, never a second number.
	for _, v := range []int{inv.Version, first.Version} {
		code, body, _ = call(t, e.h, http.MethodPost, base+"commands/issueInvoice", e.tenant, pIssuer, "", map[string]any{"invoice_id": inv.InvoiceID, "expected_version": v})
		require.Equal(t, http.StatusConflict, code, string(body))
		require.Equal(t, kit.CodeInvoiceAlreadyIssued, errCode(t, body))
	}
	require.Equal(t, 2, e.counter())
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar04_invoices WHERE tenant_id = $1 AND invoice_number IS NOT NULL`, e.tenant))

	// The same key for a different request is refused.
	code, body, _ = call(t, e.h, http.MethodPost, base+"commands/issueInvoice", e.tenant, pIssuer, key, map[string]any{"invoice_id": inv.InvoiceID, "expected_version": 99})
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	require.Equal(t, kit.CodeIdempotencyReused, errCode(t, body))
}

// ── §18 #12 issued invoice edited in place ───────────────────────────────────

func TestNeg12_IssuedInvoiceEditedInPlace(t *testing.T) {
	e := newEnv(t)
	inv := e.issued()
	snapshot := e.row(inv.InvoiceID)

	// Through the API every content command is refused with the immutability class.
	code, body := e.mk(pCreator, "amendInvoiceDraft", inv, map[string]any{"notes": "edited"})
	require.Equal(t, http.StatusConflict, code, string(body))
	require.Equal(t, kit.CodeImmutable, errCode(t, body))
	for _, name := range []string{"submitInvoiceForApproval", "cancelUnissuedInvoice"} {
		extra := map[string]any{"reason_code": "R"}
		code, body = e.mk(pCreator, name, inv, extra)
		require.Equal(t, http.StatusConflict, code, name+string(body))
		require.Equal(t, kit.CodeImmutable, errCode(t, body), name)
	}
	code, body = e.mk(pApprover, "approveInvoice", inv, map[string]any{"content_hash": inv.ContentHash})
	require.Equal(t, http.StatusConflict, code, string(body))
	require.Equal(t, kit.CodeImmutable, errCode(t, body))
	code, body = e.mk(pIssuer, "issueInvoice", inv, nil)
	require.Equal(t, kit.CodeInvoiceAlreadyIssued, errCode(t, body))

	// Directly in SQL the trigger refuses every content column, lines, tax references and deletion.
	for _, set := range []string{
		`net_total = net_total + 1, gross_total = gross_total + 1`, `invoice_number = 'INV-999999'`, `sequence_no = 999`, `due_date = due_date + 10`,
		`issue_date = issue_date - 1`, `supply_date = supply_date + 1`, `customer_reference = 'X'`, `notes = 'edited'`, `issued_canonical = '{}'`,
		`issued_content_hash = repeat('0', 64)`, `content_hash = repeat('1', 64)`, `approved_by = 'someone-else'`, `approved_content_hash = repeat('2', 64)`,
		`tax_jurisdiction_id = 'XX'`, `payment_terms_days = 90`, `open_item_id = 'OI-OTHER'`, `issued_by = 'mallory'`, `submitted_by = 'mallory'`,
		`tax_total = tax_total + 1, gross_total = gross_total + 1`, `numbering_series = 'ZZ'`, `order_id = gen_random_uuid(), milestone_id = gen_random_uuid()`,
	} {
		requireRefused(t, e.exec(`UPDATE ar04_invoices SET `+set+` WHERE invoice_id = $1`, inv.InvoiceID), "P0001", "AR04_ISSUED_INVOICE_IMMUTABLE")
	}
	requireRefused(t, e.exec(`UPDATE ar04_invoices SET lifecycle_state = 'DRAFT', approved_content_hash = NULL, approved_by = NULL, approved_at = NULL WHERE invoice_id = $1`, inv.InvoiceID),
		"P0001", "AR04_ISSUED_INVOICE_IMMUTABLE")
	requireRefused(t, e.exec(`UPDATE ar04_invoices SET lifecycle_state = 'CANCELLED' WHERE invoice_id = $1`, inv.InvoiceID), "P0001", "AR04_ISSUED_INVOICE_IMMUTABLE")
	requireRefused(t, e.exec(`UPDATE ar04_invoices SET customer_party_id = gen_random_uuid() WHERE invoice_id = $1`, inv.InvoiceID), "P0001", "AR04_INVOICE_IDENTITY_IMMUTABLE")
	requireRefused(t, e.exec(`UPDATE ar04_invoices SET currency = 'EUR' WHERE invoice_id = $1`, inv.InvoiceID), "P0001", "AR04_INVOICE_IDENTITY_IMMUTABLE")
	requireRefused(t, e.exec(`DELETE FROM ar04_invoices WHERE invoice_id = $1`, inv.InvoiceID), "P0001", "AR04_INVOICE_NOT_DELETABLE")
	requireRefused(t, e.exec(`UPDATE ar04_invoice_lines SET net_amount = net_amount + 1 WHERE invoice_id = $1 AND line_no = 1`, inv.InvoiceID), "P0001", "sealed")
	requireRefused(t, e.exec(`UPDATE ar04_invoice_lines SET description = 'edited' WHERE invoice_id = $1`, inv.InvoiceID), "P0001", "sealed")
	requireRefused(t, e.exec(`DELETE FROM ar04_invoice_lines WHERE invoice_id = $1`, inv.InvoiceID), "P0001", "sealed")
	requireRefused(t, e.exec(`INSERT INTO ar04_invoice_lines (invoice_id, line_no, tenant_id, description, quantity, unit_price, net_amount)
		VALUES ($1, 99, $2, 'sneaked in', 1, 1, 1)`, inv.InvoiceID, e.tenant), "P0001", "sealed")
	requireRefused(t, e.exec(`UPDATE ar04_invoice_tax_determinations SET tax_amount = 0 WHERE invoice_id = $1`, inv.InvoiceID), "P0001", "sealed")
	requireRefused(t, e.exec(`UPDATE ar04_invoice_tax_determinations SET determination_id = 'other' WHERE invoice_id = $1`, inv.InvoiceID), "P0001", "sealed")
	requireRefused(t, e.exec(`DELETE FROM ar04_invoice_tax_determinations WHERE invoice_id = $1`, inv.InvoiceID), "P0001", "sealed")
	// Append-only evidence.
	requireRefused(t, e.exec(`UPDATE ar04_invoice_history SET command = 'x' WHERE invoice_id = $1`, inv.InvoiceID), "P0001", "append-only")
	requireRefused(t, e.exec(`DELETE FROM ar04_invoice_history WHERE invoice_id = $1`, inv.InvoiceID), "P0001", "append-only")
	requireRefused(t, e.exec(`UPDATE ar04_invoice_approvals SET approved_by = 'x' WHERE invoice_id = $1`, inv.InvoiceID), "P0001", "append-only")
	requireRefused(t, e.exec(`DELETE FROM ar04_invoice_approvals WHERE invoice_id = $1`, inv.InvoiceID), "P0001", "append-only")
	requireRefused(t, e.exec(`UPDATE ar_audit_events SET command = 'x' WHERE aggregate_id = $1`, inv.InvoiceID), "P0001", "append-only")

	require.Equal(t, snapshot, e.row(inv.InvoiceID), "every refused edit left the issued invoice byte-for-byte unchanged")

	// Only the orthogonal dimensions and the accounting mirror stay writable.
	require.NoError(t, e.exec(`UPDATE ar04_invoices SET delivery_state = 'SENT', delivery_changed_at = now(), version = version + 1 WHERE invoice_id = $1`, inv.InvoiceID))
	require.NoError(t, e.exec(`UPDATE ar04_invoices SET accounting_state = 'POSTED', accounting_journal_id = 'J-1' WHERE invoice_id = $1`, inv.InvoiceID))
	after := e.get(inv.InvoiceID)
	require.Equal(t, inv.IssuedContentHash, after.IssuedContentHash)
	require.Equal(t, inv.NetTotal, after.NetTotal)
	require.Equal(t, DeliverySent, after.Delivery)
}

// A cancelled (never issued) invoice is frozen too.
func TestCancelledInvoiceIsFrozen(t *testing.T) {
	e := newEnv(t)
	inv := e.create(pCreator, nil)
	code, body := e.mk(pCreator, "cancelUnissuedInvoice", inv, nil)
	require.Equal(t, http.StatusBadRequest, code, string(body))
	require.Equal(t, kit.CodeReasonRequired, errCode(t, body))
	inv = e.step(pCreator, "cancelUnissuedInvoice", inv, map[string]any{"reason_code": "ENTERED_IN_ERROR", "evidence_id": "TKT-1"})
	require.Equal(t, StateCancelled, inv.State)
	require.Equal(t, "ENTERED_IN_ERROR", inv.CancelReason)
	require.Equal(t, pCreator, inv.CancelledBy)
	require.Equal(t, 1, e.events(inv.InvoiceID, EventInvoiceCancelled))
	requireRefused(t, e.exec(`UPDATE ar04_invoices SET notes = 'x' WHERE invoice_id = $1`, inv.InvoiceID), "P0001", "AR04_INVOICE_TERMINAL")
	requireRefused(t, e.exec(`UPDATE ar04_invoice_lines SET description = 'x' WHERE invoice_id = $1`, inv.InvoiceID), "P0001", "sealed")
	for _, name := range []string{"amendInvoiceDraft", "submitInvoiceForApproval", "issueInvoice"} {
		var extra map[string]any
		if name == "amendInvoiceDraft" {
			extra = map[string]any{"notes": "again"}
		}
		code, body = e.mk(pBoss, name, inv, extra)
		require.Equal(t, http.StatusConflict, code, name+string(body))
		require.Equal(t, kit.CodeInvalidTransition, errCode(t, body), name)
	}
	_, err := e.m.GetIssued(context.Background(), e.tenant, inv.InvoiceID)
	require.Equal(t, kit.CodeNotFound, mustProblem(t, err).Code, "a cancelled invoice is not an authoritative document")

	// Cancelling an approved invoice voids nothing financial: no number, no posting.
	b := e.approved(nil)
	b = e.step(pCreator, "cancelUnissuedInvoice", b, map[string]any{"reason_code": "CUSTOMER_WITHDREW"})
	require.Equal(t, StateCancelled, b.State)
	require.Empty(t, b.InvoiceNumber)
	require.Zero(t, e.postings(b.InvoiceID))
	require.Zero(t, e.counter())
}

// ── §18 #13 (AR-04 table #4) delivery failure incorrectly voids issued invoice ─

func TestNeg13_DeliveryFailureDoesNotVoidIssuedInvoice(t *testing.T) {
	e := newEnv(t)
	inv := e.issued()

	// An invoice that is not issued has no delivery.
	d := e.create(pCreator, nil)
	code, body := e.cmd(pDelivery, "markDeliveryResult", map[string]any{"invoice_id": d.InvoiceID, "result": "SENT"})
	require.Equal(t, http.StatusConflict, code, string(body))
	require.Equal(t, kit.CodeInvalidTransition, errCode(t, body))

	code, body = e.cmd(pDelivery, "markDeliveryResult", map[string]any{"invoice_id": inv.InvoiceID, "result": "DELIVERY_FAILED", "failure_reason": "mailbox full",
		"channel": "EMAIL", "reference": "MSG-1"})
	require.Equal(t, http.StatusOK, code, string(body))
	failed := asInvoice(t, body)
	require.Equal(t, DeliveryFailed, failed.Delivery)
	require.Equal(t, StateIssued, failed.State, "a delivery failure never voids an issued invoice")
	require.Equal(t, inv.InvoiceNumber, failed.InvoiceNumber)
	require.Equal(t, inv.IssuedContentHash, failed.IssuedContentHash)
	require.Equal(t, inv.NetTotal, failed.NetTotal)
	require.Equal(t, inv.OpenItemID, failed.OpenItemID)
	require.Equal(t, "QUEUED", failed.AccountingState)
	require.Equal(t, "mailbox full", failed.DeliveryDetail)
	require.Equal(t, "MSG-1", failed.DeliveryReference)
	require.Equal(t, 1, e.postings(inv.InvoiceID), "the accounting event is untouched")
	require.Equal(t, 1, e.events(inv.InvoiceID, EventInvoiceDeliveryFailed))
	require.Equal(t, inv.Version+1, failed.Version)

	// The orthogonal states move independently: an e-invoice rejection and a dispute do not touch delivery or lifecycle either.
	require.NoError(t, e.tx(func(tx pgx.Tx) error {
		return e.m.SetEInvoiceState(context.Background(), tx, e.caller(), inv.InvoiceID, "REJECTED")
	}))
	require.NoError(t, e.tx(func(tx pgx.Tx) error {
		return e.m.SetDisputeState(context.Background(), tx, e.caller(), inv.InvoiceID, "DISPUTED")
	}))
	cur := e.get(inv.InvoiceID)
	require.Equal(t, StateIssued, cur.State)
	require.Equal(t, DeliveryFailed, cur.Delivery)
	require.Equal(t, "REJECTED", cur.EInvoice)
	require.Equal(t, "DISPUTED", cur.Dispute)
	require.Equal(t, inv.IssuedContentHash, cur.IssuedContentHash)

	// Retry delivery: failed -> sent -> delivered; delivered is final.
	cur = e.step(pDelivery, "markDeliveryResult", cur, map[string]any{"result": "SENT", "channel": "EMAIL", "reference": "MSG-2"})
	require.Equal(t, DeliverySent, cur.Delivery)
	require.Empty(t, cur.DeliveryDetail, "the failure detail is cleared by a later success")
	code, body = e.cmd(pDelivery, "markDeliveryResult", map[string]any{"invoice_id": inv.InvoiceID, "result": "DELIVERED", "expected_version": cur.Version})
	require.Equal(t, http.StatusOK, code, string(body))
	require.Equal(t, DeliveryDelivered, asInvoice(t, body).Delivery)
	code, body = e.cmd(pDelivery, "markDeliveryResult", map[string]any{"invoice_id": inv.InvoiceID, "result": "SENT"})
	require.Equal(t, http.StatusConflict, code, string(body))
	require.Equal(t, kit.CodeInvalidTransition, errCode(t, body))
	code, body = e.cmd(pDelivery, "markDeliveryResult", map[string]any{"invoice_id": inv.InvoiceID, "result": "DELIVERY_FAILED", "failure_reason": "late bounce", "expected_version": 1})
	require.Equal(t, http.StatusConflict, code, "an enforced stale version is refused")
	require.Equal(t, kit.CodeStaleVersion, errCode(t, body))
	require.Equal(t, 1, e.events(inv.InvoiceID, EventInvoiceDeliveryFailed))

	code, body = e.cmd(pCreator, "markDeliveryResult", map[string]any{"invoice_id": inv.InvoiceID, "result": "SENT"})
	require.Equal(t, http.StatusForbidden, code, "the delivery recorder is its own permission")
}

// ── §18 #35 accounting event missing for issued invoice ──────────────────────

func TestNeg35_IssuedInvoiceWithoutAccountingEventIsAVisibleControlException(t *testing.T) {
	e := newEnv(t)
	inv := e.issued()
	code, body := e.query(pNobody, "getInvoiceAccountingStatus", "invoice_id="+inv.InvoiceID)
	require.Equal(t, http.StatusOK, code, string(body))
	var st map[string]any
	require.NoError(t, jsonDecode(body, &st))
	require.Equal(t, kit.PostingPending, st["posting_status"])
	require.Equal(t, false, st["control_exception"])
	require.Equal(t, "QUEUED", st["accounting_state"])
	require.Equal(t, inv.InvoiceID, st["source_event_id"])

	// The queued posting vanishes (the control the matrix demands: population completeness).
	require.NoError(t, e.exec(`DELETE FROM ar_accounting_posting_requests WHERE tenant_id = $1 AND source_event_id = $2`, e.tenant, inv.InvoiceID))
	code, body = e.query(pNobody, "getInvoiceAccountingStatus", "invoice_id="+inv.InvoiceID)
	require.Equal(t, http.StatusOK, code, string(body))
	st = nil
	require.NoError(t, jsonDecode(body, &st))
	require.Equal(t, AcctMissing, st["posting_status"])
	require.Equal(t, true, st["control_exception"])
	require.Equal(t, "ISSUED_INVOICE_WITHOUT_POSTING_REQUEST", st["exception"])

	// Failed and quarantined postings are exceptions too; an unissued invoice has nothing to post.
	d := e.create(pCreator, nil)
	code, body = e.query(pNobody, "getInvoiceAccountingStatus", "invoice_id="+d.InvoiceID)
	require.Equal(t, http.StatusOK, code, string(body))
	st = nil
	require.NoError(t, jsonDecode(body, &st))
	require.Equal(t, AcctNotApplicable, st["posting_status"])
	require.Equal(t, false, st["control_exception"])
	for status, exc := range map[string]string{kit.PostingFailed: "POSTING_FAILED", kit.PostingQuarantined: "POSTING_QUARANTINED"} {
		x := e.issued()
		require.NoError(t, e.exec(`UPDATE ar_accounting_posting_requests SET status = $3, last_error = 'no mapping' WHERE tenant_id = $1 AND source_event_id = $2`, e.tenant, x.InvoiceID, status))
		code, body = e.query(pNobody, "getInvoiceAccountingStatus", "invoice_id="+x.InvoiceID)
		require.Equal(t, http.StatusOK, code, string(body))
		st = nil
		require.NoError(t, jsonDecode(body, &st))
		require.Equal(t, status, st["posting_status"])
		require.Equal(t, true, st["control_exception"])
		require.Equal(t, exc, st["exception"])
		require.Equal(t, "no mapping", st["last_error"])
	}
	// The database refuses an issued invoice that never queued accounting.
	requireRefused(t, e.exec(`UPDATE ar04_invoices SET accounting_state = 'NOT_QUEUED' WHERE invoice_id = $1`, inv.InvoiceID), "23514", "")
}

type fakePoster struct {
	out kit.PostingOutcome
}

func (f fakePoster) Post(context.Context, string, string, []byte) (kit.PostingOutcome, error) {
	return f.out, nil
}

func TestPostingListenerMirrorsTheOutcome(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	q := func(inv *Invoice) kit.QueuedPosting {
		return kit.QueuedPosting{TenantID: e.tenant, LegalEntityID: e.entity, SourceType: kit.SourceInvoice, SourceID: inv.InvoiceID, SourceEventID: inv.InvoiceID, CorrelationID: "c"}
	}
	a := e.issued()
	require.NoError(t, e.m.OnPostingOutcome(ctx, q(a), kit.PostingOutcome{Status: kit.PostingPosted, JournalID: "J-100", ExecutionID: "X-1"}))
	g := e.get(a.InvoiceID)
	require.Equal(t, "POSTED", g.AccountingState)
	require.Equal(t, "J-100", g.AccountingJournalID)
	require.Equal(t, a.Version, g.Version, "the accounting mirror does not touch the version")
	require.Equal(t, a.IssuedContentHash, g.IssuedContentHash)
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar_audit_events WHERE tenant_id = $1 AND aggregate_id = $2 AND command = 'accountingOutcome'`, e.tenant, a.InvoiceID))
	// A posted invoice is never regressed by a later contradictory outcome; replays are idempotent.
	require.NoError(t, e.m.OnPostingOutcome(ctx, q(a), kit.PostingOutcome{Status: kit.PostingFailed, Error: "late"}))
	require.NoError(t, e.m.OnPostingOutcome(ctx, q(a), kit.PostingOutcome{Status: kit.PostingPosted, JournalID: "J-100"}))
	require.Equal(t, "POSTED", e.get(a.InvoiceID).AccountingState)

	b := e.issued()
	require.NoError(t, e.m.OnPostingOutcome(ctx, q(b), kit.PostingOutcome{Status: kit.PostingFailed, Error: "mapping AR_TAX_OUTPUT missing"}))
	g = e.get(b.InvoiceID)
	require.Equal(t, "FAILED", g.AccountingState)
	require.Equal(t, "mapping AR_TAX_OUTPUT missing", g.AccountingError)
	require.NoError(t, e.m.OnPostingOutcome(ctx, q(b), kit.PostingOutcome{Status: kit.PostingQuarantined, Error: "ambiguous"}))
	require.Equal(t, "QUARANTINED", e.get(b.InvoiceID).AccountingState)

	// Pending outcomes, other source types and unknown invoices.
	c := e.issued()
	require.NoError(t, e.m.OnPostingOutcome(ctx, q(c), kit.PostingOutcome{Status: kit.PostingPending}))
	require.Equal(t, "QUEUED", e.get(c.InvoiceID).AccountingState)
	other := q(c)
	other.SourceType = kit.SourceCreditNote
	require.NoError(t, e.m.OnPostingOutcome(ctx, other, kit.PostingOutcome{Status: kit.PostingPosted, JournalID: "J-X"}))
	require.Equal(t, "QUEUED", e.get(c.InvoiceID).AccountingState)
	ghost := q(c)
	ghost.SourceID = uuid.NewString()
	require.Error(t, e.m.OnPostingOutcome(ctx, ghost, kit.PostingOutcome{Status: kit.PostingPosted}), "a lost outcome stays visible: the queue row is re-delivered")

	// End to end through the real ACC-04 dispatcher and its listener registration.
	d := e.issued()
	disp := kit.NewPostingDispatcher(e.m.deps.DB, fakePoster{out: kit.PostingOutcome{Status: kit.PostingPosted, JournalID: "J-E2E", ExecutionID: "EX"}}, "", e.m.deps.Log)
	disp.Register(kit.SourceInvoice, e.m)
	for i := 0; i < 40 && e.get(d.InvoiceID).AccountingState != "POSTED"; i++ {
		disp.RunOnce(ctx) // the shared test database holds other tests' queued rows: drain in batches
	}
	require.Equal(t, "POSTED", e.get(d.InvoiceID).AccountingState)
	require.Equal(t, "J-E2E", e.get(d.InvoiceID).AccountingJournalID)
	var qs string
	require.NoError(t, pool.QueryRow(ctx, `SELECT status FROM ar_accounting_posting_requests WHERE tenant_id = $1 AND source_event_id = $2`, e.tenant, d.InvoiceID).Scan(&qs))
	require.Equal(t, kit.PostingPosted, qs)
	code, body := e.query(pNobody, "getInvoiceAccountingStatus", "invoice_id="+d.InvoiceID)
	require.Equal(t, http.StatusOK, code, string(body))
	require.Contains(t, string(body), `"journal_id":"J-E2E"`)
	require.Contains(t, string(body), `"mirror_in_sync":true`)
}

// ── receivable creation (AR-07) and the fail-closed prerequisites ────────────

func TestIssueWithoutAR07Wired_IsRefusedWithoutAnyStateChange(t *testing.T) {
	e := newEnv(t)
	e.m.openItems = nil
	inv := e.approved(nil)
	code, body := e.mk(pIssuer, "issueInvoice", inv, nil)
	require.Equal(t, http.StatusServiceUnavailable, code, string(body))
	require.Equal(t, kit.CodeDependencyUnavailable, errCode(t, body))
	require.Contains(t, string(body), "AR-07 receivables not wired")
	after := e.get(inv.InvoiceID)
	require.Equal(t, StateApproved, after.State)
	require.Equal(t, inv.Version, after.Version)
	require.Zero(t, e.counter(), "no number allocated")
	require.Zero(t, e.postings(inv.InvoiceID), "no posting")
	require.Zero(t, e.events(inv.InvoiceID, EventCustomerInvoiceIssued))
	require.Zero(t, e.periods.calls)
	require.Zero(t, e.tax.gets)
	// Drafting, submit, approve and the queries all work without AR-07.
	code, body = e.query(pNobody, "getAvailableActions", "invoice_id="+inv.InvoiceID)
	require.Equal(t, http.StatusOK, code, string(body))
	require.Contains(t, string(body), "AR-07 receivables not wired")
	// Wiring it makes the same invoice issuable.
	e.m.openItems = e.items
	require.Equal(t, "INV-000001", e.issue(pIssuer, inv).InvoiceNumber)
}

func TestIssueFailsClosedWhenTheTaxOrPeriodServicesAreNotWired(t *testing.T) {
	e := newEnv(t)
	inv := e.approved(nil)
	e.m.periods = nil
	code, body := e.mk(pIssuer, "issueInvoice", inv, nil)
	require.Equal(t, http.StatusServiceUnavailable, code, string(body))
	e.m.periods = e.periods
	e.m.tax = nil
	code, body = e.mk(pIssuer, "issueInvoice", inv, nil)
	require.Equal(t, http.StatusServiceUnavailable, code, string(body))
	e.m.tax = e.tax
	require.Equal(t, StateApproved, e.get(inv.InvoiceID).State)
	require.Zero(t, e.counter())
	e.periods.setErr(kit.Conflict(kit.CodePeriodClosed, "period closed"))
	code, body = e.mk(pIssuer, "issueInvoice", inv, nil)
	require.Equal(t, http.StatusConflict, code, string(body))
	require.Equal(t, kit.CodePeriodClosed, errCode(t, body))
	require.Zero(t, e.counter())
	require.Equal(t, StateApproved, e.get(inv.InvoiceID).State)
	e.periods.setErr(nil)
	e.periods.key = "2026-10-ADJ"
	issued := e.issue(pIssuer, inv)
	var payload string
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT request_payload->>'fiscal_period' FROM ar_accounting_posting_requests WHERE tenant_id = $1 AND source_event_id = $2`,
		e.tenant, issued.InvoiceID).Scan(&payload))
	require.Equal(t, "2026-10-ADJ", payload, "the period key resolved by the gate rides on the accounting event")
}

// ── issue atomicity (invariants 1 and 2) ─────────────────────────────────────

func TestIssueAtomicity_AFailingStepRollsBackNumberStateAndOutbox(t *testing.T) {
	e := newEnv(t)
	ord := &fakeOrders{basis: &kit.BillingBasis{CustomerParty: e.party, Currency: "USD", EligibleNet: mustAmount("1250.50")}}
	e.m.orders = ord
	orderID, mileID := uuid.NewString(), uuid.NewString()
	inv := e.approved(map[string]any{"order_id": orderID, "milestone_id": mileID})
	require.Equal(t, orderID, inv.Content.OrderID)

	assertUntouched := func(name string) {
		t.Helper()
		after := e.get(inv.InvoiceID)
		require.Equal(t, StateApproved, after.State, name)
		require.Equal(t, inv.Version, after.Version, name)
		require.Empty(t, after.InvoiceNumber, name)
		require.Zero(t, e.counter(), name+": the number allocation rolled back")
		require.Zero(t, e.postings(inv.InvoiceID), name)
		require.Zero(t, e.events(inv.InvoiceID, EventCustomerInvoiceIssued), name)
		require.Equal(t, []string{"createInvoiceDraft", "submitInvoiceForApproval", "approveInvoice"}, e.historyCmds(inv.InvoiceID), name)
		require.Equal(t, 3, e.audits(inv.InvoiceID), name)
		require.Equal(t, 0, e.count(`SELECT count(*) FROM ar04_invoices WHERE tenant_id = $1 AND invoice_number IS NOT NULL`, e.tenant), name)
		require.Zero(t, e.count(`SELECT count(*) FROM ar04_test_openitems WHERE source_document_id = $1`, inv.InvoiceID), name+": the receivable rolled back")
		require.Zero(t, e.count(`SELECT count(*) FROM ar04_test_orders_log WHERE invoice_id = $1`, inv.InvoiceID), name+": the order write rolled back")
	}

	// 1. AR-07 refuses.
	e.items.setErr(kit.Unprocessable(kit.CodeReceivableResidualInvalid, "refused"))
	code, body := e.mk(pIssuer, "issueInvoice", inv, nil)
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	assertUntouched("open items refuses")
	e.items.setErr(errors.New("ar07 down"))
	code, body = e.mk(pIssuer, "issueInvoice", inv, nil)
	require.Equal(t, http.StatusServiceUnavailable, code, string(body))
	assertUntouched("open items down")
	e.items.setErr(nil)

	// 2. AR-03 refuses the invoicing (it also runs in the issue transaction).
	ord.invErr = kit.Unprocessable(kit.CodeOrderNotBillable, "milestone already billed")
	code, body = e.mk(pIssuer, "issueInvoice", inv, nil)
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	require.Equal(t, kit.CodeOrderNotBillable, errCode(t, body))
	assertUntouched("orders refuses")
	ord.invErr = nil

	// 3. The period gate refuses.
	e.periods.setErr(kit.Conflict(kit.CodePeriodClosed, "closed"))
	code, body = e.mk(pIssuer, "issueInvoice", inv, nil)
	require.Equal(t, http.StatusConflict, code, string(body))
	assertUntouched("period closed")
	e.periods.setErr(nil)

	// 4. The accounting queue insert fails (injected in the database).
	require.NoError(t, e.exec(`CREATE OR REPLACE FUNCTION ar04_test_fail() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'injected posting failure'; END $$ LANGUAGE plpgsql`))
	require.NoError(t, e.exec(`CREATE TRIGGER ar04_test_fail BEFORE INSERT ON ar_accounting_posting_requests FOR EACH ROW EXECUTE FUNCTION ar04_test_fail()`))
	code, body = e.mk(pIssuer, "issueInvoice", inv, nil)
	require.GreaterOrEqual(t, code, 400, string(body))
	require.NoError(t, e.exec(`DROP TRIGGER ar04_test_fail ON ar_accounting_posting_requests`))
	assertUntouched("posting fails")

	// With every obstacle gone it issues once, with the first number, and the order was told exactly once.
	issued := e.issue(pIssuer, inv)
	require.Equal(t, "INV-000001", issued.InvoiceNumber)
	require.Equal(t, 2, e.counter())
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar04_test_orders_log WHERE invoice_id = $1`, inv.InvoiceID), "the order was told exactly once: the failed attempts rolled their writes back")
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar04_test_openitems WHERE source_document_id = $1`, inv.InvoiceID))
	require.Equal(t, 1, e.postings(inv.InvoiceID))
}

// ── order basis, profile and dates ───────────────────────────────────────────

func TestOrderBasisIsCheckedAtDraftSubmitAndIssue(t *testing.T) {
	e := newEnv(t)
	ord := &fakeOrders{basis: &kit.BillingBasis{CustomerParty: e.party, Currency: "USD", EligibleNet: mustAmount("1250.50")}}
	e.m.orders = ord
	orderID, mileID := uuid.NewString(), uuid.NewString()
	extra := map[string]any{"order_id": orderID, "milestone_id": mileID}

	ord.eligErr = kit.Unprocessable(kit.CodeOrderNotBillable, "the order is cancelled")
	code, body := e.cmd(pCreator, "createInvoiceDraft", func() map[string]any {
		b := createBody(e.entity, e.party)
		b["order_id"], b["milestone_id"] = orderID, mileID
		return b
	}())
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	require.Equal(t, kit.CodeOrderNotBillable, errCode(t, body))
	ord.eligErr = nil
	require.Zero(t, e.count(`SELECT count(*) FROM ar04_invoices WHERE tenant_id = $1`, e.tenant), "no draft against a non-billable order")

	ord.elig = 0
	inv := e.create(pCreator, extra)
	require.Equal(t, 1, ord.elig)
	// The order's eligible net must be exactly the invoice net at submit.
	ord.basis.EligibleNet = mustAmount("1000")
	code, body = e.mk(pCreator, "submitInvoiceForApproval", inv, nil)
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	require.Equal(t, kit.CodeOrderNotBillable, errCode(t, body))
	require.Contains(t, string(body), "eligible net")
	require.Equal(t, StateDraft, e.get(inv.InvoiceID).State)
	ord.basis.EligibleNet = mustAmount("1250.50")
	// The order becomes non-billable between draft and submit.
	ord.eligErr = kit.Unprocessable(kit.CodeOrderNotBillable, "held")
	code, _ = e.mk(pCreator, "submitInvoiceForApproval", inv, nil)
	require.Equal(t, http.StatusUnprocessableEntity, code)
	ord.eligErr = nil
	inv = e.approveAs(pApprover, e.submit(inv))
	issued := e.issue(pIssuer, inv)
	require.Equal(t, StateIssued, issued.State)
	require.Equal(t, orderID, issued.Content.OrderID)
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar04_test_orders_log WHERE invoice_id = $1 AND entry = $2`, inv.InvoiceID, orderID+"|"+mileID+"|"+inv.InvoiceID+"|1250.5000"))
	var doc IssuedDocument
	var canonical string
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT issued_canonical FROM ar04_invoices WHERE invoice_id = $1`, inv.InvoiceID).Scan(&canonical))
	require.NoError(t, json.Unmarshal([]byte(canonical), &doc))
	require.Equal(t, orderID, doc.OrderID, "the source order / milestone link is part of the sealed representation")
	require.Equal(t, mileID, doc.MilestoneID)

	// Another customer's order is not billable here.
	ord.basis.CustomerParty = uuid.NewString()
	other := e.create(pCreator, nil)
	code, body = e.mk(pCreator, "amendInvoiceDraft", other, map[string]any{"order_id": orderID, "milestone_id": mileID})
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	require.Equal(t, kit.CodeOrderNotBillable, errCode(t, body))
}

func TestCustomerProfileMustBeActive(t *testing.T) {
	e := newEnv(t)
	inactive := kit.Unprocessable(kit.CodeCustomerProfileInactive, "suspended")
	e.prof.fail(inactive)
	code, body := e.cmd(pCreator, "createInvoiceDraft", createBody(e.entity, e.party))
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	require.Equal(t, kit.CodeCustomerProfileInactive, errCode(t, body))
	require.Zero(t, e.count(`SELECT count(*) FROM ar04_invoices WHERE tenant_id = $1`, e.tenant))
	e.prof.fail(nil)
	inv := e.create(pCreator, nil)

	e.prof.fail(inactive)
	code, body = e.mk(pCreator, "submitInvoiceForApproval", inv, nil)
	require.Equal(t, kit.CodeCustomerProfileInactive, errCode(t, body), string(body))
	code, body = e.mk(pCreator, "amendInvoiceDraft", inv, map[string]any{"notes": "x"})
	require.Equal(t, kit.CodeCustomerProfileInactive, errCode(t, body), string(body))
	e.prof.fail(nil)
	approved := e.approveAs(pApprover, e.submit(inv))

	// The profile in force at the ISSUE date is what counts: a profile that became inactive blocks issuance.
	e.prof.fail(inactive)
	code, body = e.mk(pIssuer, "issueInvoice", approved, nil)
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	require.Equal(t, kit.CodeCustomerProfileInactive, errCode(t, body))
	require.Equal(t, StateApproved, e.get(approved.InvoiceID).State)
	require.Zero(t, e.counter())
	e.prof.fail(svcErrUnavailable())
	code, body = e.mk(pIssuer, "issueInvoice", approved, nil)
	require.Equal(t, http.StatusServiceUnavailable, code, string(body))
	e.prof.fail(nil)
	e.prof.mu.Lock()
	e.prof.at = nil
	e.prof.mu.Unlock()
	require.Equal(t, StateIssued, e.issue(pIssuer, approved).State)
	require.Equal(t, testNow, e.prof.at[len(e.prof.at)-1], "the profile was resolved at the issue date")
}

func TestDueDateFromPaymentTermsOrExplicit(t *testing.T) {
	e := newEnv(t)
	e.prof.days = 45
	a := e.issued()
	require.Equal(t, "2026-10-12", a.IssueDate)
	require.Equal(t, "2026-11-26", a.DueDate, "issue date + 45 days from the customer profile")
	require.Equal(t, 45, a.PaymentTermsDays)
	b := e.issue(pIssuer, e.approved(map[string]any{"due_date": "2026-12-31"}))
	require.Equal(t, "2026-12-31", b.DueDate, "an explicit due date overrides the terms")
	c := e.approved(map[string]any{"due_date": "2026-10-01"})
	code, body := e.mk(pIssuer, "issueInvoice", c, nil)
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	require.Equal(t, kit.CodeValidationFailed, errCode(t, body))
	require.Equal(t, StateApproved, e.get(c.InvoiceID).State)
	e.m.einvoice = fakeEInvoice{required: true}
	d := e.issued()
	require.Equal(t, EInvRequired, d.EInvoice, "the e-invoice requirement is resolved server-side and recorded on its own dimension")
	e.m.einvoice = fakeEInvoice{err: errors.New("down")}
	x := e.approved(nil)
	code, _ = e.mk(pIssuer, "issueInvoice", x, nil)
	require.Equal(t, http.StatusServiceUnavailable, code)
	e.m.einvoice = nil
}

func svcErrUnavailable() error { return errors.New("profile service down") }
