package handler_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/handler"
	"zoiko.io/accounts-receivable-svc/internal/ledger"
	svcmiddleware "zoiko.io/accounts-receivable-svc/internal/middleware"
)

// stubNewPath stands in for ar04.Module.NewPathInvoiceExists.
type stubNewPath struct {
	ids     map[string]bool
	numbers map[string]bool // "<legalEntityID>|<number>"
	err     error
	calls   int
}

func (g *stubNewPath) NewPathInvoiceExists(_ context.Context, tenantID, legalEntityID, invoiceID, invoiceNumber string) (bool, error) {
	g.calls++
	if g.err != nil {
		return false, g.err
	}
	if tenantID != testTenant {
		return false, nil
	}
	if invoiceID != "" && g.ids[invoiceID] {
		return true, nil
	}
	if invoiceNumber != "" && g.numbers[legalEntityID+"|"+invoiceNumber] {
		return true, nil
	}
	return false, nil
}

func routerWithGuard(s *stubStore, ledgerURL string, g handler.NewPathGuard, legacy bool) chi.Router {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	h := handler.New(s, &stubPublisher{}, &stubAuthZ{}, ledger.NewHTTPClient(ledgerURL, zap.NewNop()), &stubEntities{}, zap.NewNop()).WithLegacyInvoiceAPI(legacy)
	if g != nil {
		h = h.WithNewPathGuard(g)
	}
	handler.RegisterRoutes(r, h)
	return r
}

func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("not json: %s", body)
	}
	return e.Code
}

// A new-path (AR-04) invoice settles only through AR-08 cash application: the legacy pay endpoint
// refuses its id with 409 CASH_APPLICATION_REQUIRED before it reads the ledger or any legacy row.
func TestReceivePayment_InvoiceInAR04Tables_Returns409CashApplicationRequired(t *testing.T) {
	g := &stubNewPath{ids: map[string]bool{"new-1": true}}
	s := newStubStore()
	r := routerWithGuard(s, "", g, true)
	rec := doRequest(r, http.MethodPost, "/v1/invoices/new-1/pay", nil, "principal-1")
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := errorCode(t, rec.Body.Bytes()); got != "CASH_APPLICATION_REQUIRED" {
		t.Fatalf("expected CASH_APPLICATION_REQUIRED, got %q (%s)", got, rec.Body.String())
	}
}

// A legacy row whose legal number was also issued through AR-04 is the same legal document.
func TestReceivePayment_LegacyInvoiceWhoseNumberExistsInAR04_Returns409(t *testing.T) {
	g := &stubNewPath{numbers: map[string]bool{"e1|INV-000001": true}}
	s := newStubStore()
	inv := sentInvoice("i1", testTenant, 24500)
	inv.InvoiceNumber = "INV-000001"
	s.invoices["i1"] = inv
	pub := &stubPublisher{}
	r := routerWithGuard(s, "", g, true)
	rec := doRequest(r, http.MethodPost, "/v1/invoices/i1/pay", nil, "principal-1")
	if rec.Code != http.StatusConflict || errorCode(t, rec.Body.Bytes()) != "CASH_APPLICATION_REQUIRED" {
		t.Fatalf("expected 409 CASH_APPLICATION_REQUIRED, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.invoices["i1"].Status != domain.InvoiceStatusSent || pub.paymentReceived != 0 {
		t.Fatalf("a refused payment changed the legacy invoice")
	}
}

// An unanswerable AR-04 lookup fails closed: the payment is not recorded.
func TestReceivePayment_AR04LookupUnavailable_FailsClosed(t *testing.T) {
	g := &stubNewPath{err: errors.New("db down")}
	s := newStubStore()
	s.invoices["i1"] = sentInvoice("i1", testTenant, 100)
	r := routerWithGuard(s, "", g, true)
	rec := doRequest(r, http.MethodPost, "/v1/invoices/i1/pay", nil, "principal-1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.invoices["i1"].Status != domain.InvoiceStatusSent {
		t.Fatalf("status changed on an unverifiable payment")
	}
}

// A purely legacy invoice (unknown to AR-04) keeps the existing behaviour.
func TestReceivePayment_InvoiceUnknownToAR04_KeepsLegacyBehaviour(t *testing.T) {
	gl := fakeLedger(t,
		[]map[string]any{{"journal_id": "j1", "correlation_id": "i1", "status": "FINALIZED"}},
		map[string]map[string]any{"j1": journalFor("j1", "i1", "FINALIZED", 24500)},
	)
	g := &stubNewPath{}
	s := newStubStore()
	s.invoices["i1"] = sentInvoice("i1", testTenant, 24500)
	r := routerWithGuard(s, gl.URL, g, true)
	rec := doRequest(r, http.MethodPost, "/v1/invoices/i1/pay", nil, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if g.calls != 2 {
		t.Fatalf("expected the id and the number lookups, got %d calls", g.calls)
	}
}

// AR_LEGACY_INVOICE_API=false disables legacy create and pay (410) and nothing else.
func TestLegacyInvoiceAPI_DisabledRefusesCreateAndPayOnly(t *testing.T) {
	s := newStubStore()
	s.invoices["i1"] = sentInvoice("i1", testTenant, 100)
	r := routerWithGuard(s, "", nil, false)

	rec := doRequest(r, http.MethodPost, "/v1/invoices", validCreateReq(), "principal-1")
	if rec.Code != http.StatusGone || errorCode(t, rec.Body.Bytes()) != "LEGACY_INVOICE_API_DISABLED" {
		t.Fatalf("create: expected 410 LEGACY_INVOICE_API_DISABLED, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(s.invoices) != 1 {
		t.Fatalf("a disabled create stored an invoice")
	}
	rec = doRequest(r, http.MethodPost, "/v1/invoices/i1/pay", nil, "principal-1")
	if rec.Code != http.StatusGone {
		t.Fatalf("pay: expected 410, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.invoices["i1"].Status != domain.InvoiceStatusSent {
		t.Fatalf("a disabled pay changed the invoice")
	}
	for _, path := range []string{"/v1/invoices", "/v1/invoices/i1"} {
		if rec := doRequest(r, http.MethodGet, path, nil, "principal-1"); rec.Code != http.StatusOK {
			t.Fatalf("GET %s must keep working, got %d", path, rec.Code)
		}
	}
	for _, path := range []string{"/v1/invoices/i1/send", "/v1/invoices/i1/overdue"} {
		if rec := doRequest(r, http.MethodPost, path, nil, "principal-1"); rec.Code == http.StatusGone {
			t.Fatalf("POST %s must not be gated", path)
		}
	}
}

// The default (zero value) keeps the legacy API enabled.
func TestLegacyInvoiceAPI_EnabledByDefault(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, "")
	rec := doRequest(r, http.MethodPost, "/v1/invoices/missing/pay", nil, "principal-1")
	if rec.Code == http.StatusGone {
		t.Fatalf("the legacy API must be enabled by default")
	}
}
