package store_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/accounts-payable-svc/internal/accountingdispatch"
	"zoiko.io/accounts-payable-svc/internal/domain"
)

// fakeGL is a general-ledger-svc ACC-04 stand-in that records what it was asked.
type fakeGL struct {
	mu       sync.Mutex
	srv      *httptest.Server
	status   int
	body     string
	requests []map[string]any
	headers  []http.Header
}

func newFakeGL(t *testing.T) *fakeGL {
	g := &fakeGL{status: 201, body: `{"execution_id":"11111111-1111-1111-1111-111111111111","status":"COMMITTED","journal_id":"journal-1"}`}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		g.mu.Lock()
		g.requests = append(g.requests, m)
		g.headers = append(g.headers, r.Header.Clone())
		st, body := g.status, g.body
		g.mu.Unlock()
		w.WriteHeader(st)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGL) set(status int, body string) {
	g.mu.Lock()
	g.status, g.body = status, body
	g.mu.Unlock()
}

func (g *fakeGL) calls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.requests)
}

type postingRow struct {
	status    string
	attempts  int
	execID    string
	principal string
	source    string
}

func (f mfx) posting(t *testing.T, invoiceID string) postingRow {
	t.Helper()
	var r postingRow
	if err := f.owner.QueryRow(context.Background(), `SELECT status, attempts, COALESCE(posting_execution_id,''), principal_id, source_event_id FROM accounting_posting_requests WHERE invoice_id = $1`, invoiceID).
		Scan(&r.status, &r.attempts, &r.execID, &r.principal, &r.source); err != nil {
		t.Fatalf("no posting request for %s: %v", invoiceID, err)
	}
	return r
}

// approvedPlain creates, validates and approves an invoice with no PO (so no match is needed).
func (f mfx) approvedPlain(t *testing.T, amount, tax float64) *domain.VendorInvoice {
	t.Helper()
	inv := newTestInvoice(f.tenant)
	inv.LegalEntityID = f.le
	inv.Amount, inv.TaxAmount, inv.NetAmount = amount, tax, amount-tax
	if _, err := f.s.CreateInvoice(f.ctx, inv, nil); err != nil {
		t.Fatal(err)
	}
	f.validate(t, inv)
	if err := f.s.TransitionInvoice(f.ctx, f.tenant, inv.InvoiceID, domain.InvoiceStatusValidated, domain.InvoiceStatusApproved, "approver-1"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	return inv
}

func (f mfx) dispatcher(gl *fakeGL, principal string) *accountingdispatch.Dispatcher {
	return accountingdispatch.New(f.s, accountingdispatch.NewHTTPClient(gl.srv.URL), principal, zap.NewNop())
}

// The accounting obligation commits WITH the approval and is balanced.
func TestApproval_WritesOneBalancedPostingRequest_InTheSameTransaction(t *testing.T) {
	f := matchSetup(t)
	inv := f.approvedPlain(t, 1000, 100)
	row := f.posting(t, inv.InvoiceID)
	if row.status != "PENDING" || row.source != inv.InvoiceID || row.principal != "approver-1" {
		t.Fatalf("expected one PENDING request keyed by the invoice, posted as the approver, got %+v", row)
	}
	var payload domain.AccountingPostingPayload
	var raw []byte
	if err := f.owner.QueryRow(context.Background(), `SELECT request_payload FROM accounting_posting_requests WHERE invoice_id = $1`, inv.InvoiceID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(raw, &payload)
	var dr, cr float64
	keys := map[string]float64{}
	for _, l := range payload.Lines {
		dr += l.DebitAmount
		cr += l.CreditAmount
		keys[l.MappingKey] += l.DebitAmount + l.CreditAmount
	}
	if dr != 1000 || cr != 1000 || keys["AP_EXPENSE"] != 900 || keys["AP_TAX_INPUT"] != 100 || keys["AP_PAYABLE_CONTROL"] != 1000 {
		t.Fatalf("expected Dr 900 expense + 100 tax = Cr 1000 payable control, got %+v", payload.Lines)
	}
	if got, _ := f.s.GetInvoice(f.ctx, inv.InvoiceID); got.AccountingState != domain.AccountingRequested {
		t.Fatalf("approval must leave the invoice REQUESTED, got %s", got.AccountingState)
	}
}

// A pre-contract invoice carries only a gross amount; the posting must still balance.
func TestApproval_PreContractInvoice_PostsBalanced(t *testing.T) {
	f := matchSetup(t)
	inv := newTestInvoice(f.tenant)
	inv.LegalEntityID = f.le
	inv.Amount, inv.NetAmount, inv.TaxAmount = 1000, 0, 0
	f.s.CreateInvoice(f.ctx, inv, nil) //nolint:errcheck
	f.validate(t, inv)
	if err := f.s.TransitionInvoice(f.ctx, f.tenant, inv.InvoiceID, domain.InvoiceStatusValidated, domain.InvoiceStatusApproved, "approver-1"); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	f.owner.QueryRow(context.Background(), `SELECT request_payload FROM accounting_posting_requests WHERE invoice_id = $1`, inv.InvoiceID).Scan(&raw) //nolint:errcheck
	var p domain.AccountingPostingPayload
	_ = json.Unmarshal(raw, &p)
	var dr, cr float64
	for _, l := range p.Lines {
		dr += l.DebitAmount
		cr += l.CreditAmount
	}
	if dr != 1000 || cr != 1000 {
		t.Fatalf("the posting must balance on gross alone, got Dr %v Cr %v", dr, cr)
	}
}

func TestApproval_IsAtomicWithItsPostingRequest(t *testing.T) {
	f := matchSetup(t)
	inv := newTestInvoice(f.tenant)
	inv.LegalEntityID = f.le
	f.s.CreateInvoice(f.ctx, inv, nil) //nolint:errcheck
	f.validate(t, inv)
	ctx := context.Background()
	if _, err := f.owner.Exec(ctx, `ALTER TABLE accounting_posting_requests RENAME TO apr_tmp`); err != nil {
		t.Fatal(err)
	}
	err := f.s.TransitionInvoice(f.ctx, f.tenant, inv.InvoiceID, domain.InvoiceStatusValidated, domain.InvoiceStatusApproved, "approver-1")
	if _, rerr := f.owner.Exec(ctx, `ALTER TABLE apr_tmp RENAME TO accounting_posting_requests`); rerr != nil {
		t.Fatal(rerr)
	}
	if err == nil {
		t.Fatal("an approval whose accounting obligation cannot be written must fail")
	}
	if got, _ := f.s.GetInvoice(f.ctx, inv.InvoiceID); got.ApprovalState == domain.ApprovalApproved {
		t.Fatal("the invoice must not be approved without its posting obligation")
	}
	if f.events(t, inv.InvoiceID, "vendor.invoice.approved") != 0 {
		t.Fatal("no approval event may survive the rollback")
	}
}

func TestDispatcher_PostsOnce_RecordsTheJournalOnTheInvoice(t *testing.T) {
	f := matchSetup(t)
	gl := newFakeGL(t)
	inv := f.approvedPlain(t, 500, 0)
	before, _ := f.s.GetInvoice(f.ctx, inv.InvoiceID)

	if n := f.dispatcher(gl, "").RunOnce(context.Background()); n != 1 {
		t.Fatalf("expected one request handled, got %d", n)
	}
	row := f.posting(t, inv.InvoiceID)
	got, _ := f.s.GetInvoice(f.ctx, inv.InvoiceID)
	if row.status != "POSTED" || row.execID != "11111111-1111-1111-1111-111111111111" || got.AccountingState != domain.AccountingPosted ||
		got.ApprovalJournalID == nil || *got.ApprovalJournalID != "journal-1" || got.AccountingEventID == nil || *got.AccountingEventID != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("expected POSTED with the journal recorded on the invoice, got %+v / %+v", row, got)
	}
	if got.Version != before.Version {
		t.Fatal("bookkeeping must not bump the invoice version")
	}
	if f.events(t, inv.InvoiceID, "vendor.invoice.accounting_posted") != 1 || f.count(t, `SELECT count(*) FROM invoice_history WHERE invoice_id = $1 AND command = 'AccountingPosted'`, inv.InvoiceID) != 1 {
		t.Fatal("expected one accounting_posted event and one history row")
	}
	// The GL was asked exactly once, as the approver, with the invoice id as the idempotency key.
	if gl.calls() != 1 || gl.requests[0]["source_event_id"] != inv.InvoiceID || gl.headers[0].Get("X-Principal-Id") != "approver-1" || gl.headers[0].Get("X-Tenant-Id") != f.tenant {
		t.Fatalf("unexpected GL call: %v %v", gl.requests, gl.headers)
	}
	// Nothing further is due.
	if n := f.dispatcher(gl, "").RunOnce(context.Background()); n != 0 || gl.calls() != 1 {
		t.Fatalf("a POSTED request is never delivered again, got %d (GL calls %d)", n, gl.calls())
	}
}

func TestDispatcher_ConfiguredServiceIdentityIsUsed(t *testing.T) {
	f := matchSetup(t)
	gl := newFakeGL(t)
	f.approvedPlain(t, 500, 0)
	f.dispatcher(gl, "svc-accounting").RunOnce(context.Background())
	if gl.calls() != 1 || gl.headers[0].Get("X-Principal-Id") != "svc-accounting" {
		t.Fatalf("expected the configured service identity, got %v", gl.headers)
	}
}

func TestDispatcher_TransientFailure_BacksOff_ThenQuarantine_ThenCap(t *testing.T) {
	f := matchSetup(t)
	gl := newFakeGL(t)
	ctx := context.Background()
	inv := f.approvedPlain(t, 500, 0)

	// 503: stays PENDING with backoff; the invoice is untouched.
	gl.set(503, `{"error":"down"}`)
	f.dispatcher(gl, "").RunOnce(ctx)
	row := f.posting(t, inv.InvoiceID)
	if row.status != "PENDING" || row.attempts != 1 {
		t.Fatalf("a transient failure counts an attempt and stays PENDING, got %+v", row)
	}
	if got, _ := f.s.GetInvoice(f.ctx, inv.InvoiceID); got.AccountingState != domain.AccountingRequested {
		t.Fatalf("the invoice stays REQUESTED, got %s", got.AccountingState)
	}
	if n := f.dispatcher(gl, "").RunOnce(ctx); n != 0 {
		t.Fatalf("a backed-off request is not yet due, got %d", n)
	}

	// 412 (period locked) is also transient.
	f.owner.Exec(ctx, `UPDATE accounting_posting_requests SET next_attempt_at = now() WHERE invoice_id = $1`, inv.InvoiceID) //nolint:errcheck
	gl.set(412, `{"error":"period locked"}`)
	f.dispatcher(gl, "").RunOnce(ctx)
	if row := f.posting(t, inv.InvoiceID); row.status != "PENDING" || row.attempts != 2 {
		t.Fatalf("a locked period is retried, got %+v", row)
	}

	// 422: an ambiguous mapping needs a human -> QUARANTINED, the invoice FAILED, nothing retried.
	f.owner.Exec(ctx, `UPDATE accounting_posting_requests SET next_attempt_at = now() WHERE invoice_id = $1`, inv.InvoiceID) //nolint:errcheck
	gl.set(422, `{"error":"no ACC-02 mapping for AP_EXPENSE"}`)
	f.dispatcher(gl, "").RunOnce(ctx)
	if row := f.posting(t, inv.InvoiceID); row.status != "QUARANTINED" {
		t.Fatalf("expected QUARANTINED, got %+v", row)
	}
	got, _ := f.s.GetInvoice(f.ctx, inv.InvoiceID)
	if got.AccountingState != domain.AccountingFailed || got.ApprovalJournalID != nil || got.ApprovalState != domain.ApprovalApproved {
		t.Fatalf("the invoice mirrors FAILED, never loses its approval and has no journal, got %+v", got)
	}
	calls := gl.calls()
	f.dispatcher(gl, "").RunOnce(ctx)
	if gl.calls() != calls {
		t.Fatal("a QUARANTINED request is not retried")
	}

	// A request that keeps failing transiently is capped, not retried forever.
	inv2 := f.approvedPlain(t, 700, 0)
	gl.set(503, `{}`)
	f.owner.Exec(ctx, `UPDATE accounting_posting_requests SET attempts = $2 WHERE invoice_id = $1`, inv2.InvoiceID, accountingdispatch.MaxAttempts-1) //nolint:errcheck
	f.dispatcher(gl, "").RunOnce(ctx)
	if row := f.posting(t, inv2.InvoiceID); row.status != "FAILED" {
		t.Fatalf("expected FAILED after the retry cap, got %+v", row)
	}
	if got, _ := f.s.GetInvoice(f.ctx, inv2.InvoiceID); got.AccountingState != domain.AccountingFailed || got.ApprovalState != domain.ApprovalApproved {
		t.Fatalf("a posting failure never undoes the approval, got %+v", got)
	}
}

// If the worker dies after the GL answered but before the queue row closed, the lease
// lapses and the request is re-delivered; ACC-04's idempotency makes that safe and
// the invoice is not double-recorded.
func TestDispatcher_RedeliveryAfterALostLease_IsIdempotent(t *testing.T) {
	f := matchSetup(t)
	gl := newFakeGL(t)
	ctx := context.Background()
	inv := f.approvedPlain(t, 500, 0)
	f.dispatcher(gl, "").RunOnce(ctx)
	// Simulate "invoice updated, queue row never closed".
	f.owner.Exec(ctx, `UPDATE accounting_posting_requests SET status = 'IN_PROGRESS', locked_until = now() - interval '1 minute', completed_at = NULL, next_attempt_at = now() WHERE invoice_id = $1`, inv.InvoiceID) //nolint:errcheck
	f.dispatcher(gl, "").RunOnce(ctx)
	if row := f.posting(t, inv.InvoiceID); row.status != "POSTED" {
		t.Fatalf("expected the re-delivery to complete as POSTED, got %+v", row)
	}
	if f.count(t, `SELECT count(*) FROM invoice_history WHERE invoice_id = $1 AND command = 'AccountingPosted'`, inv.InvoiceID) != 1 ||
		f.events(t, inv.InvoiceID, "vendor.invoice.accounting_posted") != 1 {
		t.Fatal("the invoice must record the posting exactly once")
	}
}

func TestDispatcher_CrossTenant_UnderTheRestrictedRole(t *testing.T) {
	f := matchSetup(t)
	fb := f.other()
	gl := newFakeGL(t)
	a, b := f.approvedPlain(t, 100, 0), fb.approvedPlain(t, 200, 0)
	if n := f.dispatcher(gl, "").RunOnce(context.Background()); n != 2 {
		t.Fatalf("one worker must serve every tenant, got %d", n)
	}
	for _, x := range []struct {
		f   mfx
		inv *domain.VendorInvoice
	}{{f, a}, {fb, b}} {
		got, _ := x.f.s.GetInvoice(x.f.ctx, x.inv.InvoiceID)
		if got.AccountingState != domain.AccountingPosted || got.ApprovalJournalID == nil {
			t.Fatalf("each tenant's invoice must be POSTED with its journal, got %+v", got)
		}
	}
	// Tenant isolation of the journal link written by the store.
	if err := f.s.SetApprovalJournalID(fb.ctx, fb.tenant, a.InvoiceID, "intruder"); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.s.GetInvoice(f.ctx, a.InvoiceID); got.ApprovalJournalID == nil || *got.ApprovalJournalID == "intruder" {
		t.Fatal("another tenant must not be able to stamp a journal link")
	}
	_ = time.Second
}
