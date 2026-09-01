package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/accounts-payable-svc/internal/domain"
	"zoiko.io/accounts-payable-svc/internal/handler"
	svcmiddleware "zoiko.io/accounts-payable-svc/internal/middleware"
	"zoiko.io/accounts-payable-svc/internal/payableopenitem"
)

// tenant_id and legal_entity_id are uuid columns, so the fixtures are UUIDs —
// "t1"/"e1" would be refused by the handler's own identifier checks now that a
// malformed id is a 400 rather than a 503 from the driver.
const (
	tenantA = "11111111-1111-1111-1111-111111111111"
	tenantB = "22222222-2222-2222-2222-222222222222"
	entityA = "33333333-3333-3333-3333-333333333333"
)

// ── stubs ────────────────────────────────────────────────────────────────────

type stubStore struct {
	invoices      map[string]*domain.VendorInvoice
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

func (p *stubPublisher) PublishVendorInvoiceReceived(_ context.Context, _ domain.VendorInvoice) {
	p.received++
}
func (p *stubPublisher) PublishVendorInvoiceValidated(_ context.Context, _ domain.VendorInvoice) {
	p.validated++
}
func (p *stubPublisher) PublishVendorInvoiceApproved(_ context.Context, _ domain.VendorInvoice) {
	p.approved++
}
func (p *stubPublisher) PublishPaymentRequested(_ context.Context, _ domain.VendorInvoice) {
	p.paymentRequested++
}

type stubAuthZ struct {
	err error
}

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

// stubPayables is a stub payable-open-item-svc (AP-08) client — see
// internal/payableopenitem's package doc.
type stubPayables struct {
	fail  bool
	calls int
}

func (p *stubPayables) CreatePayableFromApprovedSource(_ context.Context, _, _ string, req payableopenitem.CreatePayableRequest) (*payableopenitem.PayableOpenItem, error) {
	p.calls++
	if p.fail {
		return nil, payableopenitem.ErrPayableServiceUnavailable
	}
	return &payableopenitem.PayableOpenItem{PayableID: "payable-" + req.SourceReference, Status: "OPEN"}, nil
}

// newRouter mounts TenantContext, which the real server mounts in
// cmd/server/main.go. It used to be omitted, so every handler under test saw an
// empty tenant scope and fell back to the query parameter or the body — the very
// behaviour these tests are supposed to be checking. A handler harness must
// mount the middleware the handler depends on.
func newRouter(s *stubStore, p *stubPublisher, a *stubAuthZ) chi.Router {
	return newRouterWithPayables(s, p, a, &stubPayables{})
}

func newRouterWithPayables(s *stubStore, p *stubPublisher, a *stubAuthZ, payables *stubPayables) chi.Router {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	h := handler.New(s, p, a, payables, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

// doRequest sends a request in tenantA's scope, which is the ordinary case.
func doRequest(r chi.Router, method, path string, body any, principalID string) *httptest.ResponseRecorder {
	return doRequestAs(r, method, path, body, principalID, tenantA)
}

// doRequestAs sends a request in an explicit tenant scope; tenantID "" omits the
// X-Tenant-Id header entirely, which is how a request with no verified scope is
// simulated.
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

func validCreateReq() domain.CreateVendorInvoiceRequest {
	return domain.CreateVendorInvoiceRequest{
		TenantID:      tenantA,
		LegalEntityID: entityA,
		VendorID:      "v1",
		InvoiceNumber: "INV-001",
		Amount:        1000,
		CurrencyCode:  "USD",
		DueDate:       domain.CalendarDate{Time: time.Now().Add(30 * 24 * time.Hour)},
		CorrelationID: "corr-1",
	}
}

// doRawRequest posts a body verbatim, so a test can send JSON that no Go struct
// would produce — a misspelled key, a bare date, an oversized payload.
// doRawRequest sends a hand-written body in tenantA's scope. The tenant header
// is set here too — a raw-body test is about the body's SHAPE, and leaving the
// scope off would make every one of them fail on identity instead.
func doRawRequest(r chi.Router, method, path, body, principalID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantA)
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func decodeErrorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error  string `json:"error"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response body is not JSON: %s", rec.Body.String())
	}
	return body.Error
}

// A duplicate (vendor, invoice_number) must be a 409 the caller can act on.
// It used to reach the generic store branch and answer 503 `store_unavailable`,
// which points an operator at infrastructure over a re-keyed number.
func TestCreateInvoice_DuplicateInvoiceNumber_Returns409(t *testing.T) {
	store := newStubStore()
	store.createErr = domain.ErrDuplicateInvoiceNumber
	r := newRouter(store, &stubPublisher{}, &stubAuthZ{})

	rec := doRequest(r, http.MethodPost, "/v1/invoices/", validCreateReq(), "principal-1")

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 for a duplicate invoice number, got %d: %s", rec.Code, rec.Body.String())
	}
	if code := decodeErrorCode(t, rec); code != "duplicate_invoice_number" {
		t.Fatalf("expected error code duplicate_invoice_number, got %q", code)
	}
}

// A store error that is NOT one of the recognised caller mistakes must still be
// a 503 — the point of the mapping is to stop over-reporting outages, not to
// stop reporting them.
func TestCreateInvoice_GenuineStoreFailure_Still503(t *testing.T) {
	store := newStubStore()
	store.createErr = errors.New("connection refused")
	r := newRouter(store, &stubPublisher{}, &stubAuthZ{})

	rec := doRequest(r, http.MethodPost, "/v1/invoices/", validCreateReq(), "principal-1")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for a real store failure, got %d", rec.Code)
	}
}

// A misspelled field used to be discarded silently, so this returned 201 for an
// invoice with no vendor — the caller believing they had sent one.
func TestCreateInvoice_UnknownField_Rejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	body := `{"tenant_id":"t1","legal_entity_id":"e1","vendorid":"v1","invoice_number":"INV-1",
	          "amount":10,"currency_code":"USD","due_date":"2026-09-01","correlation_id":"c1"}`

	rec := doRawRequest(r, http.MethodPost, "/v1/invoices/", body, "principal-1")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unknown field, got %d: %s", rec.Code, rec.Body.String())
	}
	if code := decodeErrorCode(t, rec); code != "unknown_field" {
		t.Fatalf("expected error code unknown_field, got %q", code)
	}
}

func TestCreateInvoice_BodyTooLarge_Rejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	// A single field padded past the 64 KiB cap.
	body := `{"tenant_id":"` + strings.Repeat("t", 70<<10) + `"}`

	rec := doRawRequest(r, http.MethodPost, "/v1/invoices/", body, "principal-1")

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for an oversized body, got %d", rec.Code)
	}
}

// due_date names a day, and the DATE column stores one. Both wire forms are
// accepted; the bare form used to fail as `invalid_json`, an error that never
// mentions dates.
func TestCreateInvoice_DueDate_AcceptsBareCalendarDateAndRFC3339(t *testing.T) {
	for _, tc := range []struct{ name, dueDate string }{
		{"bare calendar date", `"2026-09-01"`},
		{"RFC3339", `"2026-09-01T00:00:00Z"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
			body := `{"tenant_id":"` + tenantA + `","legal_entity_id":"` + entityA + `","vendor_id":"v1","invoice_number":"INV-1",
			          "amount":10,"currency_code":"USD","due_date":` + tc.dueDate + `,"correlation_id":"c1"}`

			rec := doRawRequest(r, http.MethodPost, "/v1/invoices/", body, "principal-1")

			if rec.Code != http.StatusCreated {
				t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
			}
			var got domain.VendorInvoice
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("response is not an invoice: %v", err)
			}
			// Both forms must mean the same instant, at UTC midnight — parsing a
			// bare date in a zone behind Greenwich would land a day early.
			want := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
			if !got.DueDate.Equal(want) {
				t.Fatalf("due_date = %s, want %s", got.DueDate, want)
			}
		})
	}
}

func TestCreateInvoice_DueDate_GarbageRejectedAsInvalidJSON(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	body := `{"tenant_id":"t1","legal_entity_id":"e1","vendor_id":"v1","invoice_number":"INV-1",
	          "amount":10,"currency_code":"USD","due_date":"not-a-date","correlation_id":"c1"}`

	rec := doRawRequest(r, http.MethodPost, "/v1/invoices/", body, "principal-1")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unparseable due_date, got %d", rec.Code)
	}
	// The detail must name the field — that is the whole improvement over a bare
	// "invalid character" message.
	if !strings.Contains(rec.Body.String(), "due_date") {
		t.Fatalf("expected the error to name due_date, got %s", rec.Body.String())
	}
}

// An id that cannot be a UUID names no invoice, so the answer is "absent", not
// "the store is unavailable".
func TestTransition_InvoiceNotFound_Returns404(t *testing.T) {
	s := newStubStore()
	s.invoices["i1"] = &domain.VendorInvoice{InvoiceID: "i1", TenantID: tenantA, LegalEntityID: entityA, Status: domain.InvoiceStatusReceived}
	// The handler reads the invoice first, so the store must be reachable for the
	// read and only fail on the write — which is exactly the 22P02 case, where
	// the id is well-formed enough to look up but not to compare.
	s.transitionErr = domain.ErrInvoiceNotFound
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})

	rec := doRequest(r, http.MethodPost, "/v1/invoices/i1/validate", nil, "principal-1")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when the id names no invoice, got %d: %s", rec.Code, rec.Body.String())
	}
	if code := decodeErrorCode(t, rec); code != "invoice_not_found" {
		t.Fatalf("expected error code invoice_not_found, got %q", code)
	}
}

func TestListInvoices_MalformedTenantScope_Returns400(t *testing.T) {
	store := newStubStore()
	store.listErr = domain.ErrInvalidIdentifier
	r := newRouter(store, &stubPublisher{}, &stubAuthZ{})

	// A non-UUID can no longer arrive as ?tenant_id= — that is refused as a
	// scope mismatch before the store is reached — so this is the gateway
	// forwarding a malformed X-Tenant-Id.
	rec := doRequestAs(r, http.MethodGet, "/v1/invoices/", nil, "principal-1", "not-a-uuid")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a non-UUID tenant_id, got %d: %s", rec.Code, rec.Body.String())
	}
	if code := decodeErrorCode(t, rec); code != "invalid_field" {
		t.Fatalf("expected error code invalid_field, got %q", code)
	}
}

// An empty register is an empty list. A nil slice marshals to `null`, which
// every consumer then has to special-case — and the console's did.
func TestListInvoices_Empty_ReturnsEmptyArrayNotNull(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})

	rec := doRequest(r, http.MethodGet, "/v1/invoices/?tenant_id="+tenantA, nil, "principal-1")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Fatalf("expected [] for an empty register, got %q", got)
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
	s.invoices["i1"] = &domain.VendorInvoice{InvoiceID: "i1", TenantID: tenantA, LegalEntityID: entityA, Status: domain.InvoiceStatusReceived}

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/invoices/i1/approve", nil, "principal-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 approving a RECEIVED (not VALIDATED) invoice, got %d", rec.Code)
	}
}

func TestValidateInvoice_FromReceived_Succeeds(t *testing.T) {
	s := newStubStore()
	s.invoices["i1"] = &domain.VendorInvoice{InvoiceID: "i1", TenantID: tenantA, LegalEntityID: entityA, Status: domain.InvoiceStatusReceived}

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
	s.invoices["i1"] = &domain.VendorInvoice{InvoiceID: "i1", TenantID: tenantA, LegalEntityID: entityA, Status: domain.InvoiceStatusValidated, CreatedByPrincipalID: "principal-creator"}

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

// TestApproveInvoice_CreatesRealAP08Payable is the first real consumer
// payable-open-item-svc (AP-08) has ever had for a vendor invoice — AP-08
// previously only had expense-claim-svc as a real source.
func TestApproveInvoice_CreatesRealAP08Payable(t *testing.T) {
	s := newStubStore()
	s.invoices["i1"] = &domain.VendorInvoice{
		InvoiceID: "i1", TenantID: tenantA, LegalEntityID: entityA, VendorID: "vendor-1",
		Amount: 500, CurrencyCode: "USD", DueDate: time.Now().UTC().Add(30 * 24 * time.Hour),
		Status: domain.InvoiceStatusValidated, CreatedByPrincipalID: "principal-creator",
	}

	payables := &stubPayables{}
	r := newRouterWithPayables(s, &stubPublisher{}, &stubAuthZ{}, payables)
	rec := doRequest(r, http.MethodPost, "/v1/invoices/i1/approve", nil, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if payables.calls != 1 {
		t.Fatalf("expected exactly one real CreatePayableFromApprovedSource call to AP-08, got %d", payables.calls)
	}
}

// TestApproveInvoice_AP08Unavailable_ApprovalStillStands verifies the AP-08
// call is genuinely best-effort — mirroring expense-claim-svc's own
// doctrine — and never undoes an approval that already succeeded.
func TestApproveInvoice_AP08Unavailable_ApprovalStillStands(t *testing.T) {
	s := newStubStore()
	s.invoices["i1"] = &domain.VendorInvoice{
		InvoiceID: "i1", TenantID: tenantA, LegalEntityID: entityA, VendorID: "vendor-1",
		Amount: 500, CurrencyCode: "USD", DueDate: time.Now().UTC().Add(30 * 24 * time.Hour),
		Status: domain.InvoiceStatusValidated, CreatedByPrincipalID: "principal-creator",
	}

	payables := &stubPayables{fail: true}
	r := newRouterWithPayables(s, &stubPublisher{}, &stubAuthZ{}, payables)
	rec := doRequest(r, http.MethodPost, "/v1/invoices/i1/approve", nil, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 approval to stand despite AP-08 failure, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.invoices["i1"].Status != domain.InvoiceStatusApproved {
		t.Fatalf("expected status APPROVED despite AP-08 failure, got %s", s.invoices["i1"].Status)
	}
}

func TestApproveInvoice_BySameCreator_Returns403(t *testing.T) {
	// Segregation of Duties (docs/original_doc/zoiko_suite_doc1.txt §12.3):
	// the principal who created the invoice may not be the one approving it.
	s := newStubStore()
	s.invoices["i1"] = &domain.VendorInvoice{InvoiceID: "i1", TenantID: tenantA, LegalEntityID: entityA, Status: domain.InvoiceStatusValidated, CreatedByPrincipalID: "principal-1"}

	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/invoices/i1/approve", nil, "principal-1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 approving an invoice created by the same principal, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.invoices["i1"].Status != domain.InvoiceStatusValidated {
		t.Fatalf("expected status to remain VALIDATED after rejected self-approval, got %s", s.invoices["i1"].Status)
	}
	if pub.approved != 0 {
		t.Fatalf("expected no approved event to be published for a rejected self-approval, got %d", pub.approved)
	}
}

func TestRequestPayment_FromReceived_Rejected(t *testing.T) {
	// Critical constraint: payment initiation requires having passed through
	// both VALIDATED and APPROVED — a RECEIVED invoice must be rejected.
	s := newStubStore()
	s.invoices["i1"] = &domain.VendorInvoice{InvoiceID: "i1", TenantID: tenantA, LegalEntityID: entityA, Status: domain.InvoiceStatusReceived}

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/invoices/i1/request-payment", nil, "principal-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 requesting payment on a RECEIVED (not APPROVED) invoice, got %d", rec.Code)
	}
}

func TestRequestPayment_FromApproved_Succeeds(t *testing.T) {
	s := newStubStore()
	s.invoices["i1"] = &domain.VendorInvoice{InvoiceID: "i1", TenantID: tenantA, LegalEntityID: entityA, Status: domain.InvoiceStatusApproved}

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

func TestRequestPayment_FromPaymentRequested_IsIdempotentReplay(t *testing.T) {
	// This endpoint has no client-supplied idempotency key, so a retry is
	// recognized from the invoice's own state: requesting payment again on
	// an invoice already PAYMENT_REQUESTED must succeed as a replay of the
	// original result, not fail — and, the important part, must not publish
	// PublishPaymentRequested a second time.
	s := newStubStore()
	s.invoices["i1"] = &domain.VendorInvoice{InvoiceID: "i1", TenantID: tenantA, LegalEntityID: entityA, Status: domain.InvoiceStatusPaymentRequested}
	pub := &stubPublisher{}

	r := newRouter(s, pub, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/invoices/i1/request-payment", nil, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (idempotent replay) requesting payment twice on an already PAYMENT_REQUESTED invoice, got %d", rec.Code)
	}
	if pub.paymentRequested != 0 {
		t.Fatalf("expected the replay to NOT publish PublishPaymentRequested again, got %d calls", pub.paymentRequested)
	}
}

func TestRequestPayment_FromReceived_StillRejected(t *testing.T) {
	// A genuinely wrong source status (not the idempotent-replay case) must
	// still be rejected — the idempotency fix must not turn every status
	// into a silent 200.
	s := newStubStore()
	s.invoices["i1"] = &domain.VendorInvoice{InvoiceID: "i1", TenantID: tenantA, LegalEntityID: entityA, Status: domain.InvoiceStatusReceived}

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/invoices/i1/request-payment", nil, "principal-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 requesting payment on a RECEIVED invoice, got %d", rec.Code)
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

// TestListInvoices_NoTenantScope_Refused replaces a test that asserted a 400
// when ?tenant_id= was absent — which documented the vulnerability as correct,
// since supplying the parameter was exactly how a caller read another tenant's
// payables register. The scope now comes from the header, so its absence is the
// failure.
func TestListInvoices_NoTenantScope_Refused(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rec := doRequestAs(r, http.MethodGet, "/v1/invoices/", nil, "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no X-Tenant-Id, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ── tenant scope ─────────────────────────────────────────────────────────────

// TestListInvoices_ForeignTenantQueryParam_Refused is the regression test for
// the headline defect: ?tenant_id= was handed straight to the store, which both
// filtered on it and set app.tenant_id from it, so the tenant the caller named
// satisfied the RLS policy on the way past — vendor names and amounts included.
func TestListInvoices_ForeignTenantQueryParam_Refused(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rec := doRequestAs(r, http.MethodGet, "/v1/invoices/?tenant_id="+tenantB, nil, "", tenantA)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 listing another tenant's register, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestListInvoices_UnknownStatusFilter_Refused(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodGet, "/v1/invoices/?status=APROVED", nil, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unrecognised status filter, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestListInvoices_MalformedLegalEntityFilter_Refused(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodGet, "/v1/invoices/?legal_entity_id=not-a-uuid", nil, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a malformed legal_entity_id filter, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestCreateInvoice_ForeignTenantBody_Refused is the write half: tenant_id in the
// body was the only source of the stored tenant, so a payable could be filed in
// another tenant's ledger — where the duplicate-invoice-number constraint is also
// scoped, so it could not even collide with the register it was hiding in.
func TestCreateInvoice_ForeignTenantBody_Refused(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	req := validCreateReq()
	req.TenantID = tenantB
	rec := doRequestAs(r, http.MethodPost, "/v1/invoices/", req, "principal-1", tenantA)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 creating into another tenant, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(s.invoices) != 0 {
		t.Fatalf("expected nothing written, got %d rows", len(s.invoices))
	}
}

func TestCreateInvoice_NoTenantInBody_UsesVerifiedScope(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	req := validCreateReq()
	req.TenantID = ""
	rec := doRequestAs(r, http.MethodPost, "/v1/invoices/", req, "principal-1", tenantA)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var got domain.VendorInvoice
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got.TenantID != tenantA {
		t.Fatalf("expected the invoice filed under the verified tenant %s, got %s", tenantA, got.TenantID)
	}
}

func TestCreateInvoice_NoTenantScope_Refused(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	rec := doRequestAs(r, http.MethodPost, "/v1/invoices/", validCreateReq(), "principal-1", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no X-Tenant-Id, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(s.invoices) != 0 {
		t.Fatalf("expected nothing written, got %d rows", len(s.invoices))
	}
}

func TestCreateInvoice_MalformedLegalEntityID_Refused(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	req := validCreateReq()
	req.LegalEntityID = "e1"
	rec := doRequest(r, http.MethodPost, "/v1/invoices/", req, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a non-UUID legal_entity_id, got %d: %s", rec.Code, rec.Body.String())
	}
}
