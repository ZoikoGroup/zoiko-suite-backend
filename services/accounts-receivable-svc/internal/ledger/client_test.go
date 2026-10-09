package ledger_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/ledger"
)

// capturedRequest is the subset of PostInvoiceIssuedAccountingEvent's outgoing
// request body this test cares about — same field names GL's own
// PostAccountingEventRequest uses, decoded independently here so a drift in
// field naming (not just values) would also be caught.
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

func testInvoice() *domain.CustomerInvoice {
	return &domain.CustomerInvoice{
		InvoiceID:     "inv-123",
		LegalEntityID: "le-1",
		CorrelationID: "corr-1",
		Amount:        1200,
		NetAmount:     1000,
		TaxAmount:     200,
		CurrencyCode:  "GBP",
		InvoiceDate:   domain.CalendarDate{Time: time.Date(2026, 3, 17, 0, 0, 0, 0, time.UTC)},
	}
}

// TestPostInvoiceIssuedAccountingEvent_RequestShape proves the exact wire
// request GL receives — mapping keys, the YYYY-MM / YYYY-MM-DD date formats,
// and that debit (gross) equals the sum of the two credit lines (net + tax).
// Nothing asserted this before: the request-building logic was correct by
// inspection only.
func TestPostInvoiceIssuedAccountingEvent_RequestShape(t *testing.T) {
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
	journalID, err := c.PostInvoiceIssuedAccountingEvent(context.Background(), "tenant-1", "principal-1", testInvoice())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if journalID != "j-1" {
		t.Fatalf("expected journal_id j-1, got %q", journalID)
	}

	if got.LegalEntityID != "le-1" {
		t.Fatalf("legal_entity_id = %q, want le-1", got.LegalEntityID)
	}
	if got.SourceEventID != "inv-123" {
		t.Fatalf("source_event_id = %q, want the invoice_id (inv-123) — this is what makes a retry idempotent against GL", got.SourceEventID)
	}
	if got.CorrelationID != "corr-1" {
		t.Fatalf("correlation_id = %q, want corr-1", got.CorrelationID)
	}
	if got.TransactionCurrency != "GBP" {
		t.Fatalf("transaction_currency = %q, want GBP", got.TransactionCurrency)
	}
	if got.FiscalPeriod != "2026-03" {
		t.Fatalf("fiscal_period = %q, want 2026-03 (YYYY-MM derived from InvoiceDate)", got.FiscalPeriod)
	}
	if got.DocumentDate != "2026-03-17" {
		t.Fatalf("document_date = %q, want 2026-03-17", got.DocumentDate)
	}
	if got.PostingDate != "2026-03-17" {
		t.Fatalf("posting_date = %q, want 2026-03-17", got.PostingDate)
	}

	if len(got.Lines) != 3 {
		t.Fatalf("expected 3 lines (receivable debit, revenue credit, tax credit), got %d: %+v", len(got.Lines), got.Lines)
	}
	byKey := map[string]capturedLine{}
	for _, l := range got.Lines {
		byKey[l.MappingKey] = l
	}
	if l, ok := byKey["AR_RECEIVABLE_STANDARD"]; !ok || l.DebitAmount != 1200 || l.CreditAmount != 0 {
		t.Fatalf("AR_RECEIVABLE_STANDARD line wrong: ok=%v %+v, want debit 1200", ok, l)
	}
	if l, ok := byKey["AR_REVENUE_STANDARD"]; !ok || l.CreditAmount != 1000 || l.DebitAmount != 0 {
		t.Fatalf("AR_REVENUE_STANDARD line wrong: ok=%v %+v, want credit 1000", ok, l)
	}
	if l, ok := byKey["AR_TAX_OUTPUT"]; !ok || l.CreditAmount != 200 || l.DebitAmount != 0 {
		t.Fatalf("AR_TAX_OUTPUT line wrong: ok=%v %+v, want credit 200", ok, l)
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

// TestPostInvoiceIssuedAccountingEvent_NoTax_OmitsTaxLine proves the tax
// credit line is conditional on TaxAmount > 0 — an invoice with no tax must
// not post a zero-amount AR_TAX_OUTPUT line, which would misrepresent the
// journal's own line count to anything reading it (ExplainAggregation-style
// consumers, reconciliation).
func TestPostInvoiceIssuedAccountingEvent_NoTax_OmitsTaxLine(t *testing.T) {
	var got capturedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"journal_id": "j-2", "status": "COMMITTED"})
	}))
	defer srv.Close()

	inv := testInvoice()
	inv.Amount, inv.NetAmount, inv.TaxAmount = 1000, 1000, 0

	c := ledger.NewHTTPClient(srv.URL, zap.NewNop())
	if _, err := c.PostInvoiceIssuedAccountingEvent(context.Background(), "tenant-1", "principal-1", inv); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Lines) != 2 {
		t.Fatalf("expected 2 lines when TaxAmount is 0, got %d: %+v", len(got.Lines), got.Lines)
	}
	for _, l := range got.Lines {
		if l.MappingKey == "AR_TAX_OUTPUT" {
			t.Fatalf("AR_TAX_OUTPUT line present despite TaxAmount being 0: %+v", l)
		}
	}
}

// TestPostInvoiceIssuedAccountingEvent_GLRejects_ReturnsErrUnavailable proves
// the failure path: a non-2xx from GL must surface as an error the handler
// can fail the request on, never a silently-empty journal ID.
func TestPostInvoiceIssuedAccountingEvent_GLRejects_ReturnsErrUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"posting_rule_ambiguity"}`))
	}))
	defer srv.Close()

	c := ledger.NewHTTPClient(srv.URL, zap.NewNop())
	_, err := c.PostInvoiceIssuedAccountingEvent(context.Background(), "tenant-1", "principal-1", testInvoice())
	if err == nil {
		t.Fatal("expected an error when GL returns a non-2xx status, got nil")
	}
}
