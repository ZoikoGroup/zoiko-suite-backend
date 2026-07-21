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

	"zoiko.io/accounts-payable-svc/internal/domain"
	"zoiko.io/accounts-payable-svc/internal/handler"
)

// ── stubs ────────────────────────────────────────────────────────────────────

type stubStore struct {
	invoices     map[string]*domain.VendorInvoice
	byCorrelation map[string]string

	createErr     error
	getErr        error
	listErr       error
	transitionErr error
}

func newStubStore() *stubStore {
	return &stubStore{invoices: map[string]*domain.VendorInvoice{}, byCorrelation: map[string]string{}}
}

func (s *stubStore) CreateInvoice(_ context.Context, inv *domain.VendorInvoice) (bool, error) {
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

func (s *stubStore) GetInvoice(_ context.Context, invoiceID string) (*domain.VendorInvoice, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	inv, ok := s.invoices[invoiceID]
	if !ok {
		return nil, nil
	}
	return inv, nil
}

func (s *stubStore) ListInvoices(_ context.Context, _ domain.ListInvoicesFilter) ([]domain.VendorInvoice, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []domain.VendorInvoice
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
	received, validated, approved, paymentRequested int
}

func (p *stubPublisher) PublishVendorInvoiceReceived(_ context.Context, _ domain.VendorInvoice)  { p.received++ }
func (p *stubPublisher) PublishVendorInvoiceValidated(_ context.Context, _ domain.VendorInvoice) { p.validated++ }
func (p *stubPublisher) PublishVendorInvoiceApproved(_ context.Context, _ domain.VendorInvoice)  { p.approved++ }
func (p *stubPublisher) PublishPaymentRequested(_ context.Context, _ domain.VendorInvoice)       { p.paymentRequested++ }

type stubAuthZ struct {
	err error
}

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

func newRouter(s *stubStore, p *stubPublisher, a *stubAuthZ) chi.Router {
	r := chi.NewRouter()
	h := handler.New(s, p, a, zap.NewNop())
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

func validCreateReq() domain.CreateVendorInvoiceRequest {
	return domain.CreateVendorInvoiceRequest{
		TenantID:      "t1",
		LegalEntityID: "e1",
		VendorID:      "v1",
		InvoiceNumber: "INV-001",
		Amount:        1000,
		CurrencyCode:  "USD",
		DueDate:       time.Now().Add(30 * 24 * time.Hour),
		CorrelationID: "corr-1",
	}
}

func TestCreateInvoice_Success(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/invoices/", validCreateReq(), "principal-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateInvoice_MissingPrincipalHeader_Returns401(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/invoices/", validCreateReq(), "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no X-Principal-Id, got %d", rec.Code)
	}
}

func TestCreateInvoice_AuthorizationDenied_Returns403(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied})
	rec := doRequest(r, http.MethodPost, "/v1/invoices/", validCreateReq(), "principal-1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when authorization-svc denies, got %d", rec.Code)
	}
}

func TestCreateInvoice_AuthorizationServiceUnavailable_FailsClosed(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationServiceUnavailable})
	rec := doRequest(r, http.MethodPost, "/v1/invoices/", validCreateReq(), "principal-1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when authorization-svc is unreachable (fail closed), got %d", rec.Code)
	}
}

func TestCreateInvoice_ZeroAmount_Rejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	req := validCreateReq()
	req.Amount = 0
	rec := doRequest(r, http.MethodPost, "/v1/invoices/", req, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a zero-amount invoice, got %d", rec.Code)
	}
}

func TestCreateInvoice_MissingCorrelationID_Rejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	req := validCreateReq()
	req.CorrelationID = ""
	rec := doRequest(r, http.MethodPost, "/v1/invoices/", req, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 with no correlation_id, got %d", rec.Code)
	}
}

func TestCreateInvoice_RetriedCorrelationID_ReturnsOriginalNotDuplicate(t *testing.T) {
	pub := &stubPublisher{}
	r := newRouter(newStubStore(), pub, &stubAuthZ{})
	req := validCreateReq()

	first := doRequest(r, http.MethodPost, "/v1/invoices/", req, "principal-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("expected 201 on first call, got %d: %s", first.Code, first.Body.String())
	}
	var firstInv domain.VendorInvoice
	_ = json.NewDecoder(first.Body).Decode(&firstInv)

	retry := doRequest(r, http.MethodPost, "/v1/invoices/", req, "principal-1")
	if retry.Code != http.StatusOK {
		t.Fatalf("expected 200 on retried call with the same correlation_id, got %d: %s", retry.Code, retry.Body.String())
	}
	var retryInv domain.VendorInvoice
	_ = json.NewDecoder(retry.Body).Decode(&retryInv)
	if retryInv.InvoiceID != firstInv.InvoiceID {
		t.Fatalf("retried call resolved to a different invoice_id (%s) than the original (%s)", retryInv.InvoiceID, firstInv.InvoiceID)
	}
	if pub.received != 1 {
		t.Fatalf("expected exactly 1 PublishVendorInvoiceReceived call, got %d — replay must not re-publish", pub.received)
	}
}

// ── ValidateInvoice / ApproveInvoice / RequestPayment lifecycle ──────────────

func TestApproveInvoice_FromReceived_Rejected(t *testing.T) {
	// State machine must be sequential: RECEIVED -> APPROVED directly
	// (skipping VALIDATED) is not a legal transition.
	s := newStubStore()
	s.invoices["i1"] = &domain.VendorInvoice{InvoiceID: "i1", TenantID: "t1", LegalEntityID: "e1", Status: domain.InvoiceStatusReceived}

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/invoices/i1/approve", nil, "principal-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 approving a RECEIVED (not VALIDATED) invoice, got %d", rec.Code)
	}
}

func TestValidateInvoice_FromReceived_Succeeds(t *testing.T) {
	s := newStubStore()
	s.invoices["i1"] = &domain.VendorInvoice{InvoiceID: "i1", TenantID: "t1", LegalEntityID: "e1", Status: domain.InvoiceStatusReceived}

	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/invoices/i1/validate", nil, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.invoices["i1"].Status != domain.InvoiceStatusValidated {
		t.Fatalf("expected status VALIDATED, got %s", s.invoices["i1"].Status)
	}
	if pub.validated != 1 {
		t.Fatalf("expected vendor.invoice.validated to be published once, got %d", pub.validated)
	}
}

func TestApproveInvoice_FromValidated_Succeeds(t *testing.T) {
	s := newStubStore()
	s.invoices["i1"] = &domain.VendorInvoice{InvoiceID: "i1", TenantID: "t1", LegalEntityID: "e1", Status: domain.InvoiceStatusValidated}

	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/invoices/i1/approve", nil, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.invoices["i1"].Status != domain.InvoiceStatusApproved {
		t.Fatalf("expected status APPROVED, got %s", s.invoices["i1"].Status)
	}
	if pub.approved != 1 {
		t.Fatalf("expected vendor.invoice.approved to be published once, got %d", pub.approved)
	}
}

func TestRequestPayment_FromReceived_Rejected(t *testing.T) {
	// Critical constraint: payment initiation requires having passed through
	// both VALIDATED and APPROVED — a RECEIVED invoice must be rejected.
	s := newStubStore()
	s.invoices["i1"] = &domain.VendorInvoice{InvoiceID: "i1", TenantID: "t1", LegalEntityID: "e1", Status: domain.InvoiceStatusReceived}

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/invoices/i1/request-payment", nil, "principal-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 requesting payment on a RECEIVED (not APPROVED) invoice, got %d", rec.Code)
	}
}

func TestRequestPayment_FromApproved_Succeeds(t *testing.T) {
	s := newStubStore()
	s.invoices["i1"] = &domain.VendorInvoice{InvoiceID: "i1", TenantID: "t1", LegalEntityID: "e1", Status: domain.InvoiceStatusApproved}

	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/invoices/i1/request-payment", nil, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.invoices["i1"].Status != domain.InvoiceStatusPaymentRequested {
		t.Fatalf("expected status PAYMENT_REQUESTED, got %s", s.invoices["i1"].Status)
	}
	if pub.paymentRequested != 1 {
		t.Fatalf("expected payment.requested to be published once, got %d", pub.paymentRequested)
	}
}

func TestRequestPayment_FromPaymentRequested_Rejected(t *testing.T) {
	// Terminal state — cannot request payment twice.
	s := newStubStore()
	s.invoices["i1"] = &domain.VendorInvoice{InvoiceID: "i1", TenantID: "t1", LegalEntityID: "e1", Status: domain.InvoiceStatusPaymentRequested}

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/invoices/i1/request-payment", nil, "principal-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 requesting payment twice on an already PAYMENT_REQUESTED invoice, got %d", rec.Code)
	}
}

// ── GetInvoice / ListInvoices ────────────────────────────────────────────────

func TestGetInvoice_NotFound(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodGet, "/v1/invoices/does-not-exist", nil, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestListInvoices_RequiresTenantID(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodGet, "/v1/invoices/", nil, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without tenant_id query param, got %d", rec.Code)
	}
}
