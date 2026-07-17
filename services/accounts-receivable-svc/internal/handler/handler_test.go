package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/handler"
)

// ── stubs ────────────────────────────────────────────────────────────────────

type stubStore struct {
	invoices map[string]*domain.CustomerInvoice

	createErr     error
	getErr        error
	listErr       error
	transitionErr error
}

func newStubStore() *stubStore {
	return &stubStore{invoices: map[string]*domain.CustomerInvoice{}}
}

func (s *stubStore) CreateInvoice(_ context.Context, inv *domain.CustomerInvoice) error {
	if s.createErr != nil {
		return s.createErr
	}
	s.invoices[inv.InvoiceID] = inv
	return nil
}

func (s *stubStore) GetInvoice(_ context.Context, invoiceID string) (*domain.CustomerInvoice, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	inv, ok := s.invoices[invoiceID]
	if !ok {
		return nil, nil
	}
	return inv, nil
}

func (s *stubStore) ListInvoices(_ context.Context, _ domain.ListInvoicesFilter) ([]domain.CustomerInvoice, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []domain.CustomerInvoice
	for _, inv := range s.invoices {
		out = append(out, *inv)
	}
	return out, nil
}

func (s *stubStore) TransitionInvoice(_ context.Context, _, invoiceID string, from, to domain.InvoiceStatus, _ string) error {
	if s.transitionErr != nil {
		return s.transitionErr
	}
	inv, ok := s.invoices[invoiceID]
	if !ok || inv.Status != from {
		return domain.ErrInvalidTransition
	}
	inv.Status = to
	return nil
}

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

func newRouter(s *stubStore, p *stubPublisher, a *stubAuthZ, ledgerURL string) chi.Router {
	r := chi.NewRouter()
	h := handler.New(s, p, a, ledgerURL, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

func doRequest(r chi.Router, method, path string, body any, principalID string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
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
	}
}

func TestCreateInvoice_Success(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, "")
	rec := doRequest(r, http.MethodPost, "/v1/invoices/", validCreateReq(), "principal-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
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

func TestMarkOverdue_Success(t *testing.T) {
	s := newStubStore()
	s.invoices["i1"] = &domain.CustomerInvoice{InvoiceID: "i1", TenantID: "t1", LegalEntityID: "e1", Status: domain.InvoiceStatusSent}

	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, "")
	rec := doRequest(r, http.MethodPost, "/v1/invoices/i1/overdue", nil, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if s.invoices["i1"].Status != domain.InvoiceStatusOverdue {
		t.Fatalf("expected status OVERDUE, got %s", s.invoices["i1"].Status)
	}
}

func TestReceivePayment_LedgerFinalizedJournalCheck(t *testing.T) {
	// Scenario A: Ledger returns no matching finalized journal entry
	ledgerMockFail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		journals := []map[string]any{} // empty
		_ = json.NewEncoder(w).Encode(journals)
	}))
	defer ledgerMockFail.Close()

	s := newStubStore()
	s.invoices["i1"] = &domain.CustomerInvoice{InvoiceID: "i1", TenantID: "t1", LegalEntityID: "e1", Status: domain.InvoiceStatusSent}

	rFail := newRouter(s, &stubPublisher{}, &stubAuthZ{}, ledgerMockFail.URL)
	rec := doRequest(rFail, http.MethodPost, "/v1/invoices/i1/pay", nil, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when ledger journal verification fails, got %d: %s", rec.Code, rec.Body.String())
	}

	// Scenario B: Ledger returns matching finalized journal entry
	ledgerMockSuccess := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		journals := []map[string]any{
			{
				"journal_id":     "j1",
				"correlation_id": "i1", // matches invoice ID
				"status":         "FINALIZED",
			},
		}
		_ = json.NewEncoder(w).Encode(journals)
	}))
	defer ledgerMockSuccess.Close()

	pub := &stubPublisher{}
	rSuccess := newRouter(s, pub, &stubAuthZ{}, ledgerMockSuccess.URL)
	rec2 := doRequest(rSuccess, http.MethodPost, "/v1/invoices/i1/pay", nil, "principal-1")
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 when ledger verification passes, got %d: %s", rec2.Code, rec2.Body.String())
	}
	if s.invoices["i1"].Status != domain.InvoiceStatusPaid {
		t.Fatalf("expected status PAID, got %s", s.invoices["i1"].Status)
	}
	if pub.paymentReceived != 1 {
		t.Fatalf("expected payment.received event to be published, got %d", pub.paymentReceived)
	}
}
