package ledger_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/accounts-payable-svc/internal/domain"
	"zoiko.io/accounts-payable-svc/internal/ledger"
)

type capturedLine struct {
	MappingKey   string  `json:"mapping_key"`
	DebitAmount  float64 `json:"debit_amount"`
	CreditAmount float64 `json:"credit_amount"`
}

type capturedRequest struct {
	LegalEntityID       string         `json:"legal_entity_id"`
	FiscalPeriod        string         `json:"fiscal_period"`
	SourceEventID       string         `json:"source_event_id"`
	CorrelationID       string         `json:"correlation_id"`
	TransactionCurrency string         `json:"transaction_currency"`
	DocumentDate        string         `json:"document_date"`
	PostingDate         string         `json:"posting_date"`
	Lines               []capturedLine `json:"lines"`
}

func testInvoice() *domain.VendorInvoice {
	return &domain.VendorInvoice{
		InvoiceID:     "inv-456",
		LegalEntityID: "le-1",
		CorrelationID: "corr-1",
		Amount:        500,
		CurrencyCode:  "USD",
		InvoiceDate:   domain.CalendarDate{Time: time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)},
	}
}

// TestPostInvoiceApprovedAccountingEvent_RequestShape proves the exact wire
// request GL receives for an AP approval event — mapping keys, the
// YYYY-MM / YYYY-MM-DD date formats, and that the two lines balance.
func TestPostInvoiceApprovedAccountingEvent_RequestShape(t *testing.T) {
	var got capturedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v1/postings/events" {
			t.Fatalf("expected path /v1/postings/events, got %s", r.URL.Path)
		}
		if tid := r.Header.Get("X-Tenant-Id"); tid != "tenant-1" {
			t.Fatalf("expected X-Tenant-Id tenant-1, got %q", tid)
		}
		if pid := r.Header.Get("X-Principal-Id"); pid != "principal-1" {
			t.Fatalf("expected X-Principal-Id principal-1, got %q", pid)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("failed to decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"journal_id": "j-1", "status": "COMMITTED"})
	}))
	defer srv.Close()

	c := ledger.NewHTTPClient(srv.URL, zap.NewNop())
	journalID, err := c.PostInvoiceApprovedAccountingEvent(context.Background(), "tenant-1", "principal-1", testInvoice())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if journalID != "j-1" {
		t.Fatalf("expected journal_id j-1, got %q", journalID)
	}

	if got.LegalEntityID != "le-1" {
		t.Fatalf("legal_entity_id = %q, want le-1", got.LegalEntityID)
	}
	if got.SourceEventID != "inv-456" {
		t.Fatalf("source_event_id = %q, want the invoice_id (inv-456) — this is what makes a retry idempotent against GL", got.SourceEventID)
	}
	if got.CorrelationID != "corr-1" {
		t.Fatalf("correlation_id = %q, want corr-1", got.CorrelationID)
	}
	if got.TransactionCurrency != "USD" {
		t.Fatalf("transaction_currency = %q, want USD", got.TransactionCurrency)
	}
	if got.FiscalPeriod != "2026-11" {
		t.Fatalf("fiscal_period = %q, want 2026-11 (YYYY-MM derived from InvoiceDate)", got.FiscalPeriod)
	}
	if got.DocumentDate != "2026-11-03" {
		t.Fatalf("document_date = %q, want 2026-11-03", got.DocumentDate)
	}
	if got.PostingDate != "2026-11-03" {
		t.Fatalf("posting_date = %q, want 2026-11-03", got.PostingDate)
	}

	if len(got.Lines) != 2 {
		t.Fatalf("expected 2 lines (expense debit, payable credit), got %d: %+v", len(got.Lines), got.Lines)
	}
	byKey := map[string]capturedLine{}
	for _, l := range got.Lines {
		byKey[l.MappingKey] = l
	}
	if l, ok := byKey["AP_EXPENSE_STANDARD"]; !ok || l.DebitAmount != 500 || l.CreditAmount != 0 {
		t.Fatalf("AP_EXPENSE_STANDARD line wrong: ok=%v %+v, want debit 500", ok, l)
	}
	if l, ok := byKey["AP_PAYABLE_STANDARD"]; !ok || l.CreditAmount != 500 || l.DebitAmount != 0 {
		t.Fatalf("AP_PAYABLE_STANDARD line wrong: ok=%v %+v, want credit 500", ok, l)
	}

	var totalDebit, totalCredit float64
	for _, l := range got.Lines {
		totalDebit += l.DebitAmount
		totalCredit += l.CreditAmount
	}
	if totalDebit != totalCredit {
		t.Fatalf("journal does not balance: debits=%v credits=%v", totalDebit, totalCredit)
	}
}

// TestPostInvoiceApprovedAccountingEvent_GLRejects_ReturnsError proves the
// failure path: a non-2xx from GL must surface as an error the handler can
// fail the request on, never a silently-empty journal ID.
func TestPostInvoiceApprovedAccountingEvent_GLRejects_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"posting_rule_ambiguity"}`))
	}))
	defer srv.Close()

	c := ledger.NewHTTPClient(srv.URL, zap.NewNop())
	_, err := c.PostInvoiceApprovedAccountingEvent(context.Background(), "tenant-1", "principal-1", testInvoice())
	if err == nil {
		t.Fatal("expected an error when GL returns a non-2xx status, got nil")
	}
}

// TestPostInvoiceApprovedAccountingEvent_GLUnreachable_ReturnsError proves
// the client fails closed, not with a nil error and an empty journal ID,
// when general-ledger-svc cannot be reached at all.
func TestPostInvoiceApprovedAccountingEvent_GLUnreachable_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // closed before use: nothing is listening

	c := ledger.NewHTTPClient(srv.URL, zap.NewNop())
	_, err := c.PostInvoiceApprovedAccountingEvent(context.Background(), "tenant-1", "principal-1", testInvoice())
	if err == nil {
		t.Fatal("expected an error when general-ledger-svc is unreachable, got nil")
	}
}
