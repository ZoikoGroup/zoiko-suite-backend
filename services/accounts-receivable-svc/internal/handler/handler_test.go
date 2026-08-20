package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/entity"
	"zoiko.io/accounts-receivable-svc/internal/handler"
	"zoiko.io/accounts-receivable-svc/internal/ledger"
	svcmiddleware "zoiko.io/accounts-receivable-svc/internal/middleware"
)

// ── stubs ────────────────────────────────────────────────────────────────────

type stubStore struct {
	invoices      map[string]*domain.CustomerInvoice
	byCorrelation map[string]string

	// lastFilter is the filter the most recent ListInvoices call received, so a
	// test can assert the tenant the handler resolved rather than only the rows
	// that came back.
	lastFilter domain.ListInvoicesFilter

	createErr     error
	getErr        error
	listErr       error
	transitionErr error
}

func newStubStore() *stubStore {
	return &stubStore{invoices: map[string]*domain.CustomerInvoice{}, byCorrelation: map[string]string{}}
}

func (s *stubStore) CreateInvoice(_ context.Context, inv *domain.CustomerInvoice) (bool, error) {
	if s.createErr != nil {
		return false, s.createErr
	}
	key := inv.TenantID + "|" + inv.CorrelationID
	if inv.CorrelationID != "" {
		if existingID, ok := s.byCorrelation[key]; ok {
			*inv = *s.invoices[existingID]
			return false, nil
		}
		s.byCorrelation[key] = inv.InvoiceID
	}
	s.invoices[inv.InvoiceID] = inv
	return true, nil
}

// GetInvoice honours tenantID, as the real store does. It used to ignore the
// tenant entirely — matching a signature that read it from the context — so no
// handler test could tell a tenant-scoped read from an unscoped one.
func (s *stubStore) GetInvoice(_ context.Context, tenantID, invoiceID string) (*domain.CustomerInvoice, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	inv, ok := s.invoices[invoiceID]
	if !ok || inv.TenantID != tenantID {
		return nil, nil
	}
	return inv, nil
}

// ListInvoices records the filter it was handed and applies its tenant, so a
// test can assert WHICH tenant the handler resolved. The previous stub discarded
// the filter and returned every invoice it held, which meant the register's
// tenant scoping was untestable — and, as it turned out, absent.
func (s *stubStore) ListInvoices(_ context.Context, filter domain.ListInvoicesFilter) ([]domain.CustomerInvoice, error) {
	s.lastFilter = filter
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []domain.CustomerInvoice
	for _, inv := range s.invoices {
		if inv.TenantID == filter.TenantID {
			out = append(out, *inv)
		}
	}
	return out, nil
}

// TransitionInvoice stamps the actor and the timestamp, as the real UPDATE does,
// and returns the row. It used to return only an error, so nothing could catch the
// handlers reporting a transition with sent_at / sent_by_principal_id still null.
func (s *stubStore) TransitionInvoice(
	_ context.Context,
	tenantID, invoiceID string,
	from, to domain.InvoiceStatus,
	actorPrincipalID string,
) (*domain.CustomerInvoice, error) {
	if s.transitionErr != nil {
		return nil, s.transitionErr
	}
	inv, ok := s.invoices[invoiceID]
	if !ok || inv.Status != from || inv.TenantID != tenantID {
		return nil, domain.ErrInvalidTransition
	}
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	inv.Status = to
	switch to {
	case domain.InvoiceStatusSent:
		inv.SentByPrincipalID, inv.SentAt = &actorPrincipalID, &now
	case domain.InvoiceStatusOverdue:
		inv.MarkedOverdueByPrincipalID, inv.MarkedOverdueAt = &actorPrincipalID, &now
	case domain.InvoiceStatusPaid:
		inv.PaymentReceivedByPrincipalID, inv.PaymentReceivedAt = &actorPrincipalID, &now
	}
	copied := *inv
	return &copied, nil
}

// stubLedger stands in for general-ledger-svc when a test cares about the
// handler's branching rather than the client's HTTP behaviour. Tests that care
// about the AMOUNT check drive the real ledger.HTTPClient against a fake server
// instead — that is where the comparison lives.
type stubLedger struct{ err error }

func (l *stubLedger) Verify(_ context.Context, _, _, _ string, _ float64) error { return l.err }

// stubEntities stands in for tenant-entity-registry-svc. The zero value ACCEPTS,
// so every pre-existing test keeps testing what it was written to test rather
// than tripping over a dependency added later.
type stubEntities struct{ err error }

func (e *stubEntities) VerifyInTenant(_ context.Context, _, _ string) error { return e.err }

type stubPublisher struct {
	issued, sent, overdue, paymentReceived int
}

func (p *stubPublisher) PublishInvoiceIssued(_ context.Context, _ domain.CustomerInvoice)    { p.issued++ }
func (p *stubPublisher) PublishInvoiceSent(_ context.Context, _ domain.CustomerInvoice)      { p.sent++ }
func (p *stubPublisher) PublishReceivableOverdue(_ context.Context, _ domain.CustomerInvoice) { p.overdue++ }
func (p *stubPublisher) PublishPaymentReceived(_ context.Context, _ domain.CustomerInvoice)   { p.paymentReceived++ }

type stubAuthZ struct {
	err error
}

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

// testTenant is the tenant every request carries unless a test says otherwise.
const testTenant = "t1"

// newRouter mounts TenantContext, which the real server has always mounted and
// this harness never did. That omission is why none of these tests could see the
// cross-tenant defect: with no middleware in the chain the handlers found no
// tenant in the context, and both reads and writes silently fell back to a
// tenant taken from the query string or the request body — the very behaviour
// under test. A handler harness has to include the middleware the handler
// depends on, or it tests a server that does not exist.
// newRouter keeps the original signature — a ledger BASE URL — because the
// payment tests point it at a fake general-ledger-svc and exercising the real
// ledger.HTTPClient against that is the point: the amount comparison lives in the
// client, so a stub there would test nothing. The entity registry accepts by
// default; tests that care pass their own via newRouterWith.
func newRouter(s *stubStore, p *stubPublisher, a *stubAuthZ, ledgerURL string) chi.Router {
	return newRouterWith(s, p, a, ledger.NewHTTPClient(ledgerURL), &stubEntities{}, nil)
}

// newRouterAtTime is newRouter with the clock pinned, for the overdue check.
func newRouterAtTime(s *stubStore, p *stubPublisher, a *stubAuthZ, ledgerURL string, now time.Time) chi.Router {
	return newRouterWith(s, p, a, ledger.NewHTTPClient(ledgerURL), &stubEntities{},
		func() time.Time { return now })
}

func newRouterWith(
	s *stubStore,
	p *stubPublisher,
	a *stubAuthZ,
	l handler.LedgerClient,
	e handler.EntityClient,
	clock func() time.Time,
) chi.Router {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	h := handler.New(s, p, a, l, e, zap.NewNop())
	if clock != nil {
		h = h.WithClock(clock)
	}
	handler.RegisterRoutes(r, h)
	return r
}

// fakeLedger serves the two general-ledger-svc routes this service reads: the
// FINALIZED register, and one journal by id with its lines.
//
// Both are needed because verifying an invoice takes two calls — find the journal
// whose correlation_id is the invoice id, then read that journal for its lines to
// total. A fake that served only the register would let a test "pass" the amount
// check by never reaching it.
func fakeLedger(t *testing.T, register []map[string]any, byID map[string]map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if id := strings.TrimPrefix(r.URL.Path, "/v1/journals/"); id != r.URL.Path && id != "" {
			journal, ok := byID[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "journal_not_found"})
				return
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(journal)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(register)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// journalFor builds a balanced journal whose debits total amount, as a real
// posting for a receivable would.
func journalFor(journalID, correlationID, status string, amount float64) map[string]any {
	return map[string]any{
		"journal_id":     journalID,
		"correlation_id": correlationID,
		"status":         status,
		"lines": []map[string]any{
			{"account_code": "1200", "debit_amount": amount, "credit_amount": 0.0},
			{"account_code": "4000", "debit_amount": 0.0, "credit_amount": amount},
		},
	}
}

func doRequest(r chi.Router, method, path string, body any, principalID string) *httptest.ResponseRecorder {
	return doRequestAs(r, method, path, body, principalID, testTenant)
}

// doRequestAs sends a request as a named tenant. An empty tenantID omits
// X-Tenant-Id altogether, which is how the "no verified scope" cases are built.
func doRequestAs(r chi.Router, method, path string, body any, principalID, tenantID string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// ── CreateInvoice ────────────────────────────────────────────────────────────

func validCreateReq() domain.CreateCustomerInvoiceRequest {
	return domain.CreateCustomerInvoiceRequest{
		TenantID:      "t1",
		LegalEntityID: "e1",
		CustomerID:    "c1",
		InvoiceNumber: "INV-001",
		Amount:        1500,
		CurrencyCode:  "USD",
		DueDate:       time.Now().Add(15 * 24 * time.Hour),
		CorrelationID: "corr-1",
	}
}

func TestCreateInvoice_Success(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, "")
	rec := doRequest(r, http.MethodPost, "/v1/invoices/", validCreateReq(), "principal-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateInvoice_MissingCorrelationID_Rejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, "")
	req := validCreateReq()
	req.CorrelationID = ""
	rec := doRequest(r, http.MethodPost, "/v1/invoices/", req, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 with no correlation_id, got %d", rec.Code)
	}
}

func TestCreateInvoice_RetriedCorrelationID_ReturnsOriginalNotDuplicate(t *testing.T) {
	pub := &stubPublisher{}
	r := newRouter(newStubStore(), pub, &stubAuthZ{}, "")
	req := validCreateReq()

	first := doRequest(r, http.MethodPost, "/v1/invoices/", req, "principal-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("expected 201 on first call, got %d: %s", first.Code, first.Body.String())
	}
	var firstInv domain.CustomerInvoice
	_ = json.NewDecoder(first.Body).Decode(&firstInv)

	retry := doRequest(r, http.MethodPost, "/v1/invoices/", req, "principal-1")
	if retry.Code != http.StatusOK {
		t.Fatalf("expected 200 on retried call with the same correlation_id, got %d: %s", retry.Code, retry.Body.String())
	}
	var retryInv domain.CustomerInvoice
	_ = json.NewDecoder(retry.Body).Decode(&retryInv)
	if retryInv.InvoiceID != firstInv.InvoiceID {
		t.Fatalf("retried call resolved to a different invoice_id (%s) than the original (%s)", retryInv.InvoiceID, firstInv.InvoiceID)
	}
	if pub.issued != 1 {
		t.Fatalf("expected exactly 1 PublishInvoiceIssued call, got %d — replay must not re-publish", pub.issued)
	}
}

func TestCreateInvoice_MissingPrincipalHeader_Returns401(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, "")
	rec := doRequest(r, http.MethodPost, "/v1/invoices/", validCreateReq(), "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestCreateInvoice_AuthorizationDenied_Returns403(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied}, "")
	rec := doRequest(r, http.MethodPost, "/v1/invoices/", validCreateReq(), "principal-1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

// ── SendInvoice / MarkOverdue / ReceivePayment ───────────────────────────────

func TestSendInvoice_Success(t *testing.T) {
	s := newStubStore()
	s.invoices["i1"] = &domain.CustomerInvoice{InvoiceID: "i1", TenantID: "t1", LegalEntityID: "e1", Status: domain.InvoiceStatusIssued}

	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, "")
	rec := doRequest(r, http.MethodPost, "/v1/invoices/i1/send", nil, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.invoices["i1"].Status != domain.InvoiceStatusSent {
		t.Fatalf("expected status SENT, got %s", s.invoices["i1"].Status)
	}
	if pub.sent != 1 {
		t.Fatalf("expected invoice.sent to be published, got %d", pub.sent)
	}
}

// The due date is set explicitly and in the past. This test used to leave
// DueDate at its zero value, which is year 1 — so once the overdue check was
// added it went on passing by accident rather than because the invoice was
// genuinely late. See TestMarkOverdue_BeforeDueDate_Returns422 for the boundary.
func TestMarkOverdue_Success(t *testing.T) {
	s := newStubStore()
	s.invoices["i1"] = &domain.CustomerInvoice{
		InvoiceID: "i1", TenantID: "t1", LegalEntityID: "e1",
		Status:  domain.InvoiceStatusSent,
		DueDate: time.Now().UTC().AddDate(0, 0, -30),
	}

	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, "")
	rec := doRequest(r, http.MethodPost, "/v1/invoices/i1/overdue", nil, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.invoices["i1"].Status != domain.InvoiceStatusOverdue {
		t.Fatalf("expected status OVERDUE, got %s", s.invoices["i1"].Status)
	}
}

// ── the ledger gate ──────────────────────────────────────────────────────────
//
// These tests drive the REAL ledger.HTTPClient against a fake general-ledger-svc,
// because the check they are about — does the journal account for the invoice's
// amount — lives in that client. A stubbed ledger would assert nothing here.

func sentInvoice(id, tenantID string, amount float64) *domain.CustomerInvoice {
	return &domain.CustomerInvoice{
		InvoiceID: id, TenantID: tenantID, LegalEntityID: "e1",
		Status: domain.InvoiceStatusSent, Amount: amount, CurrencyCode: "GBP",
	}
}

func TestReceivePayment_NoCorrelatedJournal_Returns400(t *testing.T) {
	// A FINALIZED register that mentions this invoice nowhere.
	gl := fakeLedger(t, []map[string]any{}, nil)

	s := newStubStore()
	s.invoices["i1"] = sentInvoice("i1", testTenant, 24500)
	pub := &stubPublisher{}

	r := newRouter(s, pub, &stubAuthZ{}, gl.URL)
	rec := doRequest(r, http.MethodPost, "/v1/invoices/i1/pay", nil, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when the books do not mention the invoice, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ledger_verification_failed") {
		t.Fatalf("expected ledger_verification_failed, got %s", rec.Body.String())
	}
	if s.invoices["i1"].Status != domain.InvoiceStatusSent {
		t.Fatalf("status changed on a refused payment, to %s", s.invoices["i1"].Status)
	}
	if pub.paymentReceived != 0 {
		t.Fatalf("payment.received was published for a refused payment (%d times)", pub.paymentReceived)
	}
}

// TestReceivePayment_MatchingAmount_Succeeds is the happy path, and note that the
// invoice has a REAL amount and the journal REAL lines. The version of this test
// that preceded it left both at zero, so the amount comparison — 0 == 0 — passed
// without being exercised at all.
func TestReceivePayment_MatchingAmount_Succeeds(t *testing.T) {
	gl := fakeLedger(t,
		[]map[string]any{{"journal_id": "j1", "correlation_id": "i1", "status": "FINALIZED"}},
		map[string]map[string]any{"j1": journalFor("j1", "i1", "FINALIZED", 24500)},
	)

	s := newStubStore()
	s.invoices["i1"] = sentInvoice("i1", testTenant, 24500)
	pub := &stubPublisher{}

	r := newRouter(s, pub, &stubAuthZ{}, gl.URL)
	rec := doRequest(r, http.MethodPost, "/v1/invoices/i1/pay", nil, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 when the books account for the invoice, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.invoices["i1"].Status != domain.InvoiceStatusPaid {
		t.Fatalf("expected status PAID, got %s", s.invoices["i1"].Status)
	}
	if pub.paymentReceived != 1 {
		t.Fatalf("expected payment.received to be published once, got %d", pub.paymentReceived)
	}
}

// TestReceivePayment_JournalForWrongAmount_Returns400 is the defect this pass
// closed. The gate used to accept ANY finalized journal correlated to the invoice
// without looking at the figure, so a journal for a penny discharged a receivable
// for any sum — meaning anyone who could post a journal could mark any invoice
// paid by posting a trivial one against it.
func TestReceivePayment_JournalForWrongAmount_Returns400(t *testing.T) {
	gl := fakeLedger(t,
		[]map[string]any{{"journal_id": "j1", "correlation_id": "i1", "status": "FINALIZED"}},
		// One penny against a £24,500 receivable.
		map[string]map[string]any{"j1": journalFor("j1", "i1", "FINALIZED", 0.01)},
	)

	s := newStubStore()
	s.invoices["i1"] = sentInvoice("i1", testTenant, 24500)
	pub := &stubPublisher{}

	r := newRouter(s, pub, &stubAuthZ{}, gl.URL)
	rec := doRequest(r, http.MethodPost, "/v1/invoices/i1/pay", nil, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("UNBACKED PAYMENT: expected 400 for a journal that does not cover the invoice, got %d: %s",
			rec.Code, rec.Body.String())
	}
	// A distinct code from "no journal": an entry exists and disagrees, which is a
	// bookkeeping error to look at rather than a missing posting to make.
	if !strings.Contains(rec.Body.String(), "ledger_amount_mismatch") {
		t.Fatalf("expected ledger_amount_mismatch, got %s", rec.Body.String())
	}
	if s.invoices["i1"].Status != domain.InvoiceStatusPaid && s.invoices["i1"].Status != domain.InvoiceStatusSent {
		t.Fatalf("unexpected status %s", s.invoices["i1"].Status)
	}
	if s.invoices["i1"].Status == domain.InvoiceStatusPaid {
		t.Fatal("UNBACKED PAYMENT: the invoice was marked PAID against a journal for the wrong amount")
	}
	if pub.paymentReceived != 0 {
		t.Fatalf("payment.received was published for an unbacked payment (%d times)", pub.paymentReceived)
	}
}

// TestReceivePayment_AmountComparedInCents guards the comparison's arithmetic.
// 0.1 + 0.2 != 0.3 in binary floating point, so a journal built from lines that
// sum to the invoice amount in decimal must still match exactly.
func TestReceivePayment_AmountComparedInCents(t *testing.T) {
	gl := fakeLedger(t,
		[]map[string]any{{"journal_id": "j1", "correlation_id": "i1", "status": "FINALIZED"}},
		map[string]map[string]any{"j1": {
			"journal_id": "j1", "correlation_id": "i1", "status": "FINALIZED",
			"lines": []map[string]any{
				{"account_code": "1200", "debit_amount": 0.10, "credit_amount": 0.0},
				{"account_code": "1200", "debit_amount": 0.20, "credit_amount": 0.0},
				{"account_code": "4000", "debit_amount": 0.0, "credit_amount": 0.30},
			},
		}},
	)

	s := newStubStore()
	s.invoices["i1"] = sentInvoice("i1", testTenant, 0.30)

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, gl.URL)
	rec := doRequest(r, http.MethodPost, "/v1/invoices/i1/pay", nil, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("0.10 + 0.20 must equal 0.30 in cents; got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestReceivePayment_JournalReversedBetweenReads_Returns400 — the register listing
// and the journal read are two calls and not atomic. A journal reversed in between
// must not still clear a receivable, which is why the status is re-checked on the
// second read rather than trusted from the first.
func TestReceivePayment_JournalReversedBetweenReads_Returns400(t *testing.T) {
	gl := fakeLedger(t,
		[]map[string]any{{"journal_id": "j1", "correlation_id": "i1", "status": "FINALIZED"}},
		map[string]map[string]any{"j1": journalFor("j1", "i1", "REVERSED", 24500)},
	)

	s := newStubStore()
	s.invoices["i1"] = sentInvoice("i1", testTenant, 24500)

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, gl.URL)
	rec := doRequest(r, http.MethodPost, "/v1/invoices/i1/pay", nil, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a journal reversed between the two reads, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.invoices["i1"].Status == domain.InvoiceStatusPaid {
		t.Fatal("the invoice was marked PAID against a REVERSED journal")
	}
}

// TestReceivePayment_JournalVanishesBetweenReads_Returns503 — the register just
// named this journal, so a 404 on the second read is a FAULT, not "the books do
// not mention this invoice". Reporting it as the latter would present a broken
// ledger as an unaccounted receivable, and invite an operator to post a duplicate.
func TestReceivePayment_JournalVanishesBetweenReads_Returns503(t *testing.T) {
	gl := fakeLedger(t,
		[]map[string]any{{"journal_id": "j1", "correlation_id": "i1", "status": "FINALIZED"}},
		map[string]map[string]any{}, // j1 is gone
	)

	s := newStubStore()
	s.invoices["i1"] = sentInvoice("i1", testTenant, 24500)

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, gl.URL)
	rec := doRequest(r, http.MethodPost, "/v1/invoices/i1/pay", nil, "principal-1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when a named journal cannot be read, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestReceivePayment_LedgerServiceConnectionRefused_Returns503(t *testing.T) {
	s := newStubStore()
	s.invoices["i1"] = sentInvoice("i1", testTenant, 24500)

	// A port nothing is listening on: the client must fail closed rather than read
	// an outage as "no matching journal".
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, "http://127.0.0.1:1")
	rec := doRequest(r, http.MethodPost, "/v1/invoices/i1/pay", nil, "principal-1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when the ledger is unreachable, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.invoices["i1"].Status != domain.InvoiceStatusSent {
		t.Fatalf("expected status to remain SENT, got %s", s.invoices["i1"].Status)
	}
}

func TestReceivePayment_LedgerService500_Returns503(t *testing.T) {
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer gl.Close()

	s := newStubStore()
	s.invoices["i1"] = sentInvoice("i1", testTenant, 24500)

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, gl.URL)
	rec := doRequest(r, http.MethodPost, "/v1/invoices/i1/pay", nil, "principal-1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when the ledger errors, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.invoices["i1"].Status != domain.InvoiceStatusSent {
		t.Fatalf("expected status to remain SENT, got %s", s.invoices["i1"].Status)
	}
}


// ── tenant scoping ───────────────────────────────────────────────────────────
//
// The tests in this section cover the defect that made every tenant's
// receivables register readable, and writable, by anyone who could reach the
// port: the register built its filter from ?tenant_id= and the create path took
// tenant_id from the request body, and neither consulted the verified
// X-Tenant-Id at all.

// TestListInvoices_ScopesToTheVerifiedTenant_NotAQueryParameter is the direct
// regression test. Before the fix, `?tenant_id=<other>` returned that tenant's
// entire register.
func TestListInvoices_ScopesToTheVerifiedTenant_NotAQueryParameter(t *testing.T) {
	s := newStubStore()
	s.invoices["a1"] = &domain.CustomerInvoice{InvoiceID: "a1", TenantID: "tenant-a", LegalEntityID: "e1", Status: domain.InvoiceStatusIssued}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, "")

	// Verified as tenant-b, naming no tenant at all: must see nothing of
	// tenant-a's, and must have queried as tenant-b.
	rec := doRequestAs(r, http.MethodGet, "/v1/invoices/", nil, "principal-1", "tenant-b")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.lastFilter.TenantID != "tenant-b" {
		t.Fatalf("register queried tenant %q, expected the verified scope tenant-b", s.lastFilter.TenantID)
	}
	var got []domain.CustomerInvoice
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("failed to decode the register: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("CROSS-TENANT READ: tenant-b saw %d of tenant-a's invoices", len(got))
	}
}

// TestListInvoices_QueryTenantOtherThanVerified_Returns403 refuses the
// disagreement outright rather than quietly ignoring it, so a caller who
// believed the parameter worked finds out. Mirrors general-ledger-svc, which
// this service already calls with both the header and the parameter set.
func TestListInvoices_QueryTenantOtherThanVerified_Returns403(t *testing.T) {
	s := newStubStore()
	s.invoices["a1"] = &domain.CustomerInvoice{InvoiceID: "a1", TenantID: "tenant-a", LegalEntityID: "e1", Status: domain.InvoiceStatusIssued}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, "")

	rec := doRequestAs(r, http.MethodGet, "/v1/invoices/?tenant_id=tenant-a", nil, "principal-1", "tenant-b")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a tenant_id other than the verified scope, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.lastFilter.TenantID != "" {
		t.Fatalf("the store was queried despite the refusal, as tenant %q", s.lastFilter.TenantID)
	}
}

// TestListInvoices_QueryTenantMatchingVerified_Allowed keeps the parameter
// usable when it agrees — several console reads still send it alongside the
// header, and refusing agreement would be gratuitous.
func TestListInvoices_QueryTenantMatchingVerified_Allowed(t *testing.T) {
	s := newStubStore()
	s.invoices["b1"] = &domain.CustomerInvoice{InvoiceID: "b1", TenantID: "tenant-b", LegalEntityID: "e1", Status: domain.InvoiceStatusIssued}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, "")

	rec := doRequestAs(r, http.MethodGet, "/v1/invoices/?tenant_id=tenant-b", nil, "principal-1", "tenant-b")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 when tenant_id agrees with the verified scope, got %d: %s", rec.Code, rec.Body.String())
	}
	var got []domain.CustomerInvoice
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if len(got) != 1 {
		t.Fatalf("expected tenant-b's single invoice, got %d", len(got))
	}
}

// TestListInvoices_NoVerifiedTenant_Returns401 — the register used to answer
// 400 missing_field for a missing ?tenant_id, which meant a caller with no
// identity at all was invited to supply the scope themselves.
func TestListInvoices_NoVerifiedTenant_Returns401(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, "")

	rec := doRequestAs(r, http.MethodGet, "/v1/invoices/?tenant_id=tenant-a", nil, "principal-1", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no verified tenant scope, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.lastFilter.TenantID != "" {
		t.Fatalf("the store was queried with no verified scope, as tenant %q", s.lastFilter.TenantID)
	}
}

// TestListInvoices_EmptyRegister_ReturnsEmptyArray guards the JSON shape. A nil
// slice marshals to `null`, which every caller then has to special-case — and
// the console's list parse would report it as a malformed body.
func TestListInvoices_EmptyRegister_ReturnsEmptyArray(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, "")

	rec := doRequest(r, http.MethodGet, "/v1/invoices/", nil, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if body := bytes.TrimSpace(rec.Body.Bytes()); string(body) != "[]" {
		t.Fatalf("expected an empty JSON array for an empty register, got %q", string(body))
	}
}

// TestListInvoices_UnknownStatusFilter_Returns400 — an unrecognised status
// matched no row and returned an empty list, so `?status=OUSTANDING` read as
// "this tenant has no invoices" rather than as a typo.
func TestListInvoices_UnknownStatusFilter_Returns400(t *testing.T) {
	s := newStubStore()
	s.invoices["b1"] = &domain.CustomerInvoice{InvoiceID: "b1", TenantID: testTenant, LegalEntityID: "e1", Status: domain.InvoiceStatusIssued}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, "")

	rec := doRequest(r, http.MethodGet, "/v1/invoices/?status=OUTSTANDING", nil, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unknown status filter, got %d: %s", rec.Code, rec.Body.String())
	}

	// A real status still works.
	ok := doRequest(r, http.MethodGet, "/v1/invoices/?status=ISSUED", nil, "principal-1")
	if ok.Code != http.StatusOK {
		t.Fatalf("expected 200 for status=ISSUED, got %d: %s", ok.Code, ok.Body.String())
	}
}

// TestCreateInvoice_BodyTenantOtherThanVerified_Returns403 covers the write half
// of the defect: tenant_id in the body was the ONLY source of the stored
// tenant_id, so a body naming another tenant filed the receivable in that
// tenant's register.
func TestCreateInvoice_BodyTenantOtherThanVerified_Returns403(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, "")

	req := validCreateReq()
	req.TenantID = "tenant-victim"
	rec := doRequestAs(r, http.MethodPost, "/v1/invoices/", req, "principal-1", "tenant-attacker")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a body tenant_id other than the verified scope, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(s.invoices) != 0 {
		t.Fatalf("CROSS-TENANT WRITE: %d invoice(s) were stored despite the refusal", len(s.invoices))
	}
}

// TestCreateInvoice_StoresTheVerifiedTenant_WhenBodyOmitsIt — tenant_id is no
// longer a required body field, because it is not the caller's to choose. The
// stored tenant must be the verified one.
func TestCreateInvoice_StoresTheVerifiedTenant_WhenBodyOmitsIt(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, "")

	req := validCreateReq()
	req.TenantID = ""
	rec := doRequestAs(r, http.MethodPost, "/v1/invoices/", req, "principal-1", "tenant-real")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 with no tenant_id in the body, got %d: %s", rec.Code, rec.Body.String())
	}

	var stored *domain.CustomerInvoice
	for _, inv := range s.invoices {
		stored = inv
	}
	if stored == nil {
		t.Fatal("no invoice was stored")
	}
	if stored.TenantID != "tenant-real" {
		t.Fatalf("invoice stored under tenant %q, expected the verified scope tenant-real", stored.TenantID)
	}
}

func TestCreateInvoice_NoVerifiedTenant_Returns401(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, "")

	rec := doRequestAs(r, http.MethodPost, "/v1/invoices/", validCreateReq(), "principal-1", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no verified tenant scope, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(s.invoices) != 0 {
		t.Fatalf("%d invoice(s) were stored with no verified tenant scope", len(s.invoices))
	}
}

// TestGetInvoice_NoVerifiedTenant_Returns401 — this used to answer 404
// invoice_not_found, because the store returned (nil, nil) when the context
// carried no tenant. A missing identity header is not the same fact as a
// missing invoice, and reporting it as one hid the misconfiguration.
func TestGetInvoice_NoVerifiedTenant_Returns401(t *testing.T) {
	s := newStubStore()
	s.invoices["a1"] = &domain.CustomerInvoice{InvoiceID: "a1", TenantID: "tenant-a", LegalEntityID: "e1", Status: domain.InvoiceStatusIssued}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, "")

	rec := doRequestAs(r, http.MethodGet, "/v1/invoices/a1", nil, "principal-1", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no verified tenant scope, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGetInvoice_AnotherTenantsInvoice_Returns404(t *testing.T) {
	s := newStubStore()
	s.invoices["a1"] = &domain.CustomerInvoice{InvoiceID: "a1", TenantID: "tenant-a", LegalEntityID: "e1", Status: domain.InvoiceStatusIssued}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, "")

	rec := doRequestAs(r, http.MethodGet, "/v1/invoices/a1", nil, "principal-1", "tenant-b")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 reading another tenant's invoice, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestSendInvoice_AnotherTenantsInvoice_Returns404 — the transitions look the
// invoice up before authorizing, so the lookup is what has to be tenant-scoped.
func TestSendInvoice_AnotherTenantsInvoice_Returns404(t *testing.T) {
	s := newStubStore()
	s.invoices["a1"] = &domain.CustomerInvoice{InvoiceID: "a1", TenantID: "tenant-a", LegalEntityID: "e1", Status: domain.InvoiceStatusIssued}
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, "")

	rec := doRequestAs(r, http.MethodPost, "/v1/invoices/a1/send", nil, "principal-1", "tenant-b")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 sending another tenant's invoice, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.invoices["a1"].Status != domain.InvoiceStatusIssued {
		t.Fatalf("CROSS-TENANT WRITE: tenant-b moved tenant-a's invoice to %s", s.invoices["a1"].Status)
	}
	if pub.sent != 0 {
		t.Fatalf("invoice.sent was published for a refused transition (%d times)", pub.sent)
	}
}

// ── the overdue check ────────────────────────────────────────────────────────

// TestMarkOverdue_BeforeDueDate_Returns422 — nothing checked the due date, so a
// SENT invoice could be marked OVERDUE the moment it was sent, and
// receivable.overdue is what aging and impairment count downstream.
func TestMarkOverdue_BeforeDueDate_Returns422(t *testing.T) {
	dueDate := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	s := newStubStore()
	s.invoices["i1"] = &domain.CustomerInvoice{
		InvoiceID: "i1", TenantID: testTenant, LegalEntityID: "e1",
		Status: domain.InvoiceStatusSent, DueDate: dueDate,
	}
	pub := &stubPublisher{}

	// Due today: still on time, because due_date is the last day to pay.
	r := newRouterAtTime(s, pub, &stubAuthZ{}, "", dueDate.Add(12*time.Hour))
	rec := doRequest(r, http.MethodPost, "/v1/invoices/i1/overdue", nil, "principal-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 marking an invoice overdue on its due date, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.invoices["i1"].Status != domain.InvoiceStatusSent {
		t.Fatalf("status changed on a refused transition, to %s", s.invoices["i1"].Status)
	}
	if pub.overdue != 0 {
		t.Fatalf("receivable.overdue was published for an invoice that is not yet late (%d times)", pub.overdue)
	}
}

// TestMarkOverdue_AfterDueDate_Succeeds — the invoice turns overdue at the start
// of the day after its due date.
func TestMarkOverdue_AfterDueDate_Succeeds(t *testing.T) {
	dueDate := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	s := newStubStore()
	s.invoices["i1"] = &domain.CustomerInvoice{
		InvoiceID: "i1", TenantID: testTenant, LegalEntityID: "e1",
		Status: domain.InvoiceStatusSent, DueDate: dueDate,
	}
	pub := &stubPublisher{}

	r := newRouterAtTime(s, pub, &stubAuthZ{}, "", dueDate.AddDate(0, 0, 1))
	rec := doRequest(r, http.MethodPost, "/v1/invoices/i1/overdue", nil, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 the day after the due date, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.invoices["i1"].Status != domain.InvoiceStatusOverdue {
		t.Fatalf("expected status OVERDUE, got %s", s.invoices["i1"].Status)
	}
	if pub.overdue != 1 {
		t.Fatalf("expected receivable.overdue to be published once, got %d", pub.overdue)
	}
}

// TestCreateInvoice_DuplicateInvoiceNumber_Returns409 — the (tenant, customer,
// invoice_number) UNIQUE constraint has been in 000001 since the service was
// written, but its violation fell through to the generic store error and the
// handler answered 503 store_unavailable. Re-keying an invoice number was
// therefore indistinguishable from the database being down: an answer that is
// wrong about whose problem it is, and offers nothing to do about it.
func TestCreateInvoice_DuplicateInvoiceNumber_Returns409(t *testing.T) {
	s := newStubStore()
	s.createErr = domain.ErrDuplicateInvoiceNumber
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, "")

	rec := doRequest(r, http.MethodPost, "/v1/invoices/", validCreateReq(), "principal-1")
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 for a duplicate invoice number, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestCreateInvoice_NonUuidIdentifier_Returns400 — tenant_id and legal_entity_id
// are UUID columns, so a non-UUID dies in the driver before any row is examined
// (SQLSTATE 22P02), which also answered 503. The console's legacy client sent
// "tenant-zoiko-dev-01" and "le-singapore-01", so every create it ever attempted
// landed here and was reported as a dead store.
func TestCreateInvoice_NonUuidIdentifier_Returns400(t *testing.T) {
	s := newStubStore()
	s.createErr = domain.ErrInvalidIdentifier
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, "")

	rec := doRequest(r, http.MethodPost, "/v1/invoices/", validCreateReq(), "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a non-UUID identifier, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestListInvoices_NonUuidTenantScope_Returns400 — same for the register: a
// gateway that forwarded a malformed tenant scope is a fault worth naming, not a
// dead store.
func TestListInvoices_NonUuidTenantScope_Returns400(t *testing.T) {
	s := newStubStore()
	s.listErr = domain.ErrInvalidIdentifier
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, "")

	rec := doRequest(r, http.MethodGet, "/v1/invoices/", nil, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a non-UUID tenant scope, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ── legal entity / tenant reconciliation ─────────────────────────────────────
//
// Fixing the tenant scope closed the register and the stored tenant_id, but left
// this: authority is granted per legal entity, so a principal holding a grant on
// an entity in ANOTHER tenant could raise an invoice attributed to that entity
// while the row was filed under their own — a receivable whose two halves name
// different tenants. This is the gap left open in obligations-svc.

func TestCreateInvoice_LegalEntityInAnotherTenant_Returns403(t *testing.T) {
	s := newStubStore()
	// The authz client would ALLOW this: the point is that the entity check runs
	// first and refuses regardless of what grants the caller holds.
	r := newRouterWith(s, &stubPublisher{}, &stubAuthZ{},
		&stubLedger{}, &stubEntities{err: entity.ErrForeignTenant}, nil)

	rec := doRequest(r, http.MethodPost, "/v1/invoices/", validCreateReq(), "principal-1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a legal entity in another tenant, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "legal_entity_not_in_tenant") {
		t.Fatalf("expected legal_entity_not_in_tenant, got %s", rec.Body.String())
	}
	if len(s.invoices) != 0 {
		t.Fatalf("MIS-ATTRIBUTED RECEIVABLE: %d invoice(s) written against an out-of-tenant entity", len(s.invoices))
	}
}

// TestCreateInvoice_UnknownAndForeignEntityAreIndistinguishable — the caller must
// not be able to use this endpoint as an oracle for which entity ids exist in
// other tenants, so both answer identically. The difference is kept in the log.
func TestCreateInvoice_UnknownAndForeignEntityAreIndistinguishable(t *testing.T) {
	body := func(err error) (int, string) {
		r := newRouterWith(newStubStore(), &stubPublisher{}, &stubAuthZ{},
			&stubLedger{}, &stubEntities{err: err}, nil)
		rec := doRequest(r, http.MethodPost, "/v1/invoices/", validCreateReq(), "principal-1")
		return rec.Code, rec.Body.String()
	}
	unknownCode, unknownBody := body(entity.ErrNotFound)
	foreignCode, foreignBody := body(entity.ErrForeignTenant)

	if unknownCode != foreignCode || unknownBody != foreignBody {
		t.Fatalf("an unknown entity and one in another tenant must be indistinguishable to the caller:\n  unknown: %d %s\n  foreign: %d %s",
			unknownCode, unknownBody, foreignCode, foreignBody)
	}
}

func TestCreateInvoice_DissolvedEntity_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouterWith(s, &stubPublisher{}, &stubAuthZ{},
		&stubLedger{}, &stubEntities{err: entity.ErrNotActive}, nil)

	rec := doRequest(r, http.MethodPost, "/v1/invoices/", validCreateReq(), "principal-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for an entity that may not take on receivables, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(s.invoices) != 0 {
		t.Fatalf("%d invoice(s) written against an inactive entity", len(s.invoices))
	}
}

// TestCreateInvoice_EntityRegistryUnavailable_Returns503_AndWritesNothing — fails
// CLOSED, like every other cross-service dependency here. An invoice whose
// attribution could not be checked is not written.
func TestCreateInvoice_EntityRegistryUnavailable_Returns503_AndWritesNothing(t *testing.T) {
	s := newStubStore()
	r := newRouterWith(s, &stubPublisher{}, &stubAuthZ{},
		&stubLedger{}, &stubEntities{err: entity.ErrUnavailable}, nil)

	rec := doRequest(r, http.MethodPost, "/v1/invoices/", validCreateReq(), "principal-1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when the entity registry is unreachable, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(s.invoices) != 0 {
		t.Fatalf("FAILED OPEN: %d invoice(s) written while attribution could not be checked", len(s.invoices))
	}
}

// TestCreateInvoice_EntityCheckedBeforeAuthorization — order matters. Refusing on
// the entity first means an out-of-tenant entity does not reveal, via the
// authorization answer, whether the caller holds a grant on it.
func TestCreateInvoice_EntityCheckedBeforeAuthorization(t *testing.T) {
	// Both would refuse. If authorization ran first the answer would be 403
	// authorization_denied; the entity refusal must win.
	r := newRouterWith(newStubStore(), &stubPublisher{},
		&stubAuthZ{err: domain.ErrAuthorizationDenied},
		&stubLedger{}, &stubEntities{err: entity.ErrForeignTenant}, nil)

	rec := doRequest(r, http.MethodPost, "/v1/invoices/", validCreateReq(), "principal-1")
	if !strings.Contains(rec.Body.String(), "legal_entity_not_in_tenant") {
		t.Fatalf("the entity check must run before authorization; got %s", rec.Body.String())
	}
}

// ── transition responses carry what was written ──────────────────────────────

// TestSendInvoice_ResponseCarriesTheAttribution — the handlers used to return the
// invoice they had read a moment earlier with `.Status` patched by hand, so the
// response to a send reported sent_at: null and sent_by_principal_id: null for the
// hop it had just recorded. The database had them; the API denied they existed.
func TestSendInvoice_ResponseCarriesTheAttribution(t *testing.T) {
	s := newStubStore()
	s.invoices["i1"] = &domain.CustomerInvoice{
		InvoiceID: "i1", TenantID: testTenant, LegalEntityID: "e1",
		Status: domain.InvoiceStatusIssued, Amount: 100, CurrencyCode: "GBP",
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, "")

	rec := doRequest(r, http.MethodPost, "/v1/invoices/i1/send", nil, "principal-7")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var got domain.CustomerInvoice
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("failed to decode the response: %v", err)
	}
	if got.Status != domain.InvoiceStatusSent {
		t.Fatalf("expected SENT, got %s", got.Status)
	}
	if got.SentByPrincipalID == nil || *got.SentByPrincipalID != "principal-7" {
		t.Fatalf("the response omits who sent it: %#v", got.SentByPrincipalID)
	}
	if got.SentAt == nil {
		t.Fatal("the response omits when it was sent, for the hop it is reporting")
	}
}

func TestMarkOverdue_ResponseCarriesTheAttribution(t *testing.T) {
	dueDate := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	s := newStubStore()
	s.invoices["i1"] = &domain.CustomerInvoice{
		InvoiceID: "i1", TenantID: testTenant, LegalEntityID: "e1",
		Status: domain.InvoiceStatusSent, DueDate: dueDate, Amount: 100, CurrencyCode: "GBP",
	}
	r := newRouterAtTime(s, &stubPublisher{}, &stubAuthZ{}, "", dueDate.AddDate(0, 0, 5))

	rec := doRequest(r, http.MethodPost, "/v1/invoices/i1/overdue", nil, "principal-9")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got domain.CustomerInvoice
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if got.MarkedOverdueByPrincipalID == nil || *got.MarkedOverdueByPrincipalID != "principal-9" {
		t.Fatalf("the response omits who declared it overdue: %#v", got.MarkedOverdueByPrincipalID)
	}
	if got.MarkedOverdueAt == nil {
		t.Fatal("the response omits when it was declared overdue")
	}
}

// ── the register's paging ────────────────────────────────────────────────────

// TestListInvoices_DefaultsToABoundedPage — the read was unbounded: every invoice
// a tenant had ever raised, on every dashboard load.
func TestListInvoices_DefaultsToABoundedPage(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, "")

	rec := doRequest(r, http.MethodGet, "/v1/invoices/", nil, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if s.lastFilter.Limit <= 0 {
		t.Fatalf("the register was queried with no limit (%d) — an unbounded read", s.lastFilter.Limit)
	}
}

func TestListInvoices_PagingIsValidated(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, "")

	for _, q := range []string{
		"?limit=0",
		"?limit=-1",
		"?limit=abc",
		"?limit=100000", // above the cap: refused, not silently clamped
		"?offset=-1",
	} {
		rec := doRequest(r, http.MethodGet, "/v1/invoices/"+q, nil, "principal-1")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for %q, got %d: %s", q, rec.Code, rec.Body.String())
		}
	}

	// A valid page is passed through as asked.
	s := newStubStore()
	ok := newRouter(s, &stubPublisher{}, &stubAuthZ{}, "")
	rec := doRequest(ok, http.MethodGet, "/v1/invoices/?limit=25&offset=50", nil, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for a valid page, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.lastFilter.Limit != 25 || s.lastFilter.Offset != 50 {
		t.Fatalf("paging was not passed through: limit=%d offset=%d", s.lastFilter.Limit, s.lastFilter.Offset)
	}
}

// TestListInvoices_MalformedLegalEntityFilter_Returns400 — the store compares
// `legal_entity_id::text = $n`, casting the COLUMN rather than the parameter, so a
// malformed value does not error: it silently matches nothing, and an empty
// register reads as "this entity has no invoices". The console was validating the
// filter itself to work around this; the check belongs here, where a direct caller
// is held to it too.
func TestListInvoices_MalformedLegalEntityFilter_Returns400(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, "")

	rec := doRequest(r, http.MethodGet, "/v1/invoices/?legal_entity_id=not-a-uuid", nil, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a non-UUID legal_entity_id filter, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.lastFilter.LegalEntityID != "" {
		t.Fatalf("the malformed filter reached the store as %q", s.lastFilter.LegalEntityID)
	}

	// A well-formed one is passed through.
	valid := "22222222-2222-2222-2222-222222222222"
	ok := doRequest(r, http.MethodGet, "/v1/invoices/?legal_entity_id="+valid, nil, "principal-1")
	if ok.Code != http.StatusOK {
		t.Fatalf("expected 200 for a valid legal_entity_id, got %d: %s", ok.Code, ok.Body.String())
	}
	if s.lastFilter.LegalEntityID != valid {
		t.Fatalf("the valid filter did not reach the store: %q", s.lastFilter.LegalEntityID)
	}
}
