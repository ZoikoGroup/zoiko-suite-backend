// Package ledger provides a client against general-ledger-svc:
//
//  1. Read-only Verify — confirms the books account for a receivable before
//     payment is recorded (existing).
//  2. Write PostInvoiceIssuedAccountingEvent — posts the issuance accounting
//     event through GL's system-originated path (POST /v1/postings/events),
//     using the invoice ID as source_event_id for idempotency (new).
//
// The tenant scope travels in X-Tenant-Id — the gateway-auth-svc convention
// every service here trusts — and the journal id travels as a path segment,
// path-escaped, never interpolated into a query string.
package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"time"

	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/domain"
)

var (
	// ErrJournalNotFound means general-ledger-svc was reached and has no
	// FINALIZED journal correlated to this invoice.
	ErrJournalNotFound = errors.New("no finalized journal is correlated to this invoice")

	// ErrAmountMismatch means a correlated FINALIZED journal exists but does not
	// account for the invoice's amount. Deliberately distinct from
	// ErrJournalNotFound: "the books do not mention this invoice" and "the books
	// mention it for a different figure" are different findings with different
	// remedies, and collapsing them would hide the second entirely.
	ErrAmountMismatch = errors.New("the correlated journal does not account for the invoice amount")

	// ErrUnavailable means general-ledger-svc could not be reached or answered
	// something unexpected. Callers MUST fail closed — never read this as "no
	// matching journal", which is how an outage becomes an unbacked payment.
	ErrUnavailable = errors.New("general-ledger-svc unavailable")
)

// JournalHeader is the subset of general-ledger-svc's register entry needed to
// find the journal for an invoice.
type JournalHeader struct {
	JournalID     string `json:"journal_id"`
	CorrelationID string `json:"correlation_id"`
	Status        string `json:"status"`
}

// JournalLine is the subset of a journal line needed to total it.
//
// There is no currency field here because general-ledger-svc's lines do not carry
// one — see Verify's note on what that means for this check.
type JournalLine struct {
	AccountCode  string  `json:"account_code"`
	DebitAmount  float64 `json:"debit_amount"`
	CreditAmount float64 `json:"credit_amount"`
}

// JournalWithLines mirrors what GET /v1/journals/{id} returns: the header fields
// flattened alongside the lines.
type JournalWithLines struct {
	JournalID     string        `json:"journal_id"`
	TenantID      string        `json:"tenant_id"`
	LegalEntityID string        `json:"legal_entity_id"`
	Status        string        `json:"status"`
	CorrelationID string        `json:"correlation_id"`
	Lines         []JournalLine `json:"lines"`
}

// TotalDebitsCents is the journal's total debit movement, in exact cents.
//
// In cents and compared exactly, never as float64 within an epsilon. The amounts
// are NUMERIC(18,2) at both ends and are only floats in transit because that is
// what JSON gives us; 0.1+0.2 != 0.3 in binary floating point, and this
// comparison decides whether money is declared received. Same reasoning, and the
// same helper, as bank-reconciliation-svc's ledger client.
func (j JournalWithLines) TotalDebitsCents() int64 {
	var cents int64
	for _, l := range j.Lines {
		cents += toCents(l.DebitAmount)
	}
	return cents
}

func toCents(v float64) int64 { return int64(math.Round(v * 100)) }

// ToCents converts a JSON-decoded money amount to exact cents.
func ToCents(v float64) int64 { return toCents(v) }

// Client is the narrow interface the handler depends on.
type Client interface {
	Verify(ctx context.Context, tenantID, legalEntityID, invoiceID string, amount float64) error
	PostInvoiceIssuedAccountingEvent(ctx context.Context, tenantID, principalID string, inv *domain.CustomerInvoice) (journalID string, err error)
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{
		baseURL: baseURL,
		http:    &http.Client{Timeout: 3 * time.Second, Transport: newRetryTransport()},
		log:     log,
	}
}

// Verify reports whether the books account for this invoice.
//
// Two calls, because general-ledger-svc exposes no lookup by correlation_id: the
// register is read filtered to FINALIZED for this entity to find the journal
// whose correlation_id IS the invoice id, then that journal is read by id for its
// lines. Since the journal is located BY the invoice id, it is by construction
// the journal for this invoice alone — which is what makes comparing its total
// debits against the invoice amount the right test rather than an approximation.
// A journal carrying tax still totals to the invoice amount on the debit side.
//
// WHAT THIS CANNOT CHECK, stated rather than glossed:
//
//   - CURRENCY. general-ledger-svc's lines carry no currency code at all, so a
//     journal for 24500 satisfies an invoice for £24,500 and one for €24,500
//     alike. Closing that needs a currency on the ledger line; it is not
//     something this client can infer.
//   - DIRECTION AND ACCOUNT. Verifying that the debit landed on a receivables
//     account needs a chart of accounts, and no chart-of-accounts service exists
//     on this platform. bank-reconciliation-svc can check direction only because
//     its statement lines record the ledger account; nothing gives this service
//     the equivalent.
//
// So this proves the books carry a finalized entry of the right SIZE for this
// invoice. That is materially more than the previous check, which proved only
// that some entry existed.
func (c *HTTPClient) Verify(ctx context.Context, tenantID, legalEntityID, invoiceID string, amount float64) error {
	journalID, err := c.findCorrelatedJournal(ctx, tenantID, legalEntityID, invoiceID)
	if err != nil {
		return err
	}

	journal, err := c.getJournal(ctx, tenantID, journalID)
	if err != nil {
		return err
	}

	// Re-checked rather than trusted from the register listing: the two reads are
	// not atomic, and a journal reversed between them must not still clear a
	// receivable.
	if journal.Status != "FINALIZED" {
		return fmt.Errorf("%w: journal %s is %s, not FINALIZED", ErrJournalNotFound, journalID, journal.Status)
	}

	want := toCents(amount)
	got := journal.TotalDebitsCents()
	if got != want {
		return fmt.Errorf("%w: journal %s totals %d.%02d in debits, the invoice is for %d.%02d",
			ErrAmountMismatch, journalID, got/100, abs(got%100), want/100, abs(want%100))
	}
	return nil
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func (c *HTTPClient) findCorrelatedJournal(ctx context.Context, tenantID, legalEntityID, invoiceID string) (string, error) {
	params := url.Values{}
	params.Set("tenant_id", tenantID)
	params.Set("legal_entity_id", legalEntityID)
	params.Set("status", "FINALIZED")

	// general-ledger-svc scopes this read to the verified X-Tenant-Id and refuses
	// a tenant_id parameter that disagrees with it. Both are sent, and they agree:
	// this is a service-to-service call inside the estate and the tenant is the
	// invoice's own.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/v1/journals?%s", c.baseURL, params.Encode()), nil)
	if err != nil {
		return "", fmt.Errorf("%w: build request: %v", ErrUnavailable, err)
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("%w: status %d: %s", ErrUnavailable, resp.StatusCode, body)
	}

	var journals []JournalHeader
	if err := json.NewDecoder(resp.Body).Decode(&journals); err != nil {
		return "", fmt.Errorf("%w: decode register: %v", ErrUnavailable, err)
	}

	for _, j := range journals {
		if j.CorrelationID == invoiceID {
			return j.JournalID, nil
		}
	}
	return "", ErrJournalNotFound
}

func (c *HTTPClient) getJournal(ctx context.Context, tenantID, journalID string) (*JournalWithLines, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/v1/journals/%s", c.baseURL, url.PathEscape(journalID)), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", ErrUnavailable, err)
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()

	// A 404 here is NOT "no journal for this invoice" — the register just named
	// this id, so its disappearance mid-check is a fault, and answering
	// ErrJournalNotFound would report a broken ledger as an unaccounted invoice.
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("%w: reading journal %s: status %d: %s",
			ErrUnavailable, journalID, resp.StatusCode, body)
	}

	var j JournalWithLines
	if err := json.NewDecoder(resp.Body).Decode(&j); err != nil {
		return nil, fmt.Errorf("%w: decode journal: %v", ErrUnavailable, err)
	}
	return &j, nil
}

// ── Write: PostInvoiceIssuedAccountingEvent ──────────────────────────────────
//
// Posts the AR invoice issuance accounting event through general-ledger-svc's
// system-originated posting path (POST /v1/postings/events).
//
// The invoice's InvoiceID is used as source_event_id, making a retried call
// idempotent against GL's own UNIQUE(tenant_id, source_event_id) constraint.
//
// Lines posted (using account mappings registered in GL via ACC-02):
//   - Debit: AR_RECEIVABLE_STANDARD for inv.Amount (gross)
//   - Credit: AR_REVENUE_STANDARD for inv.NetAmount
//   - Credit: AR_TAX_OUTPUT for inv.TaxAmount (only if TaxAmount > 0)
//
// The sum of debits equals sum of credits (Amount == NetAmount + TaxAmount),
// which is already enforced by the invoice's Balances() check.
//
// FiscalPeriod is derived from inv.InvoiceDate as "YYYY-MM".
// DocumentDate and PostingDate are both set to inv.InvoiceDate as "YYYY-MM-DD".
// TransactionCurrency is inv.CurrencyCode.

type postingEventLine struct {
	MappingKey   string  `json:"mapping_key"`
	DebitAmount  float64 `json:"debit_amount,omitempty"`
	CreditAmount float64 `json:"credit_amount,omitempty"`
}

type postAccountingEventRequest struct {
	LegalEntityID       string             `json:"legal_entity_id"`
	FiscalPeriod        string             `json:"fiscal_period"`
	Description         string             `json:"description"`
	SourceEventID       string             `json:"source_event_id"`
	CorrelationID       string             `json:"correlation_id"`
	TransactionCurrency string             `json:"transaction_currency"`
	DocumentDate        string             `json:"document_date"`
	PostingDate         string             `json:"posting_date"`
	Lines               []postingEventLine `json:"lines"`
}

type postingExecutionResponse struct {
	JournalID *string `json:"journal_id"`
	Status    string  `json:"status"`
}

// PostInvoiceIssuedAccountingEvent posts the issuance accounting event for an
// AR invoice. Returns the created journal ID on success.
//
// On non-2xx response from GL, returns an error — caller MUST fail the invoice
// creation (see handler.CreateInvoice). GL's source_event_id uniqueness makes
// retry safe even though the invoice row already exists.
func (c *HTTPClient) PostInvoiceIssuedAccountingEvent(ctx context.Context, tenantID, principalID string, inv *domain.CustomerInvoice) (journalID string, err error) {
	fiscalPeriod := inv.InvoiceDate.Time.Format("2006-01")
	documentDate := inv.InvoiceDate.Time.Format("2006-01-02")
	postingDate := inv.InvoiceDate.Time.Format("2006-01-02")

	description := fmt.Sprintf("AR invoice %s issued", inv.InvoiceNumber)

	lines := []postingEventLine{
		{MappingKey: "AR_RECEIVABLE_STANDARD", DebitAmount: inv.Amount},
		{MappingKey: "AR_REVENUE_STANDARD", CreditAmount: inv.NetAmount},
	}
	if inv.TaxAmount > 0 {
		lines = append(lines, postingEventLine{MappingKey: "AR_TAX_OUTPUT", CreditAmount: inv.TaxAmount})
	}

	body := postAccountingEventRequest{
		LegalEntityID:       inv.LegalEntityID,
		FiscalPeriod:        fiscalPeriod,
		Description:         description,
		SourceEventID:       inv.InvoiceID,
		CorrelationID:       inv.CorrelationID,
		TransactionCurrency: inv.CurrencyCode,
		DocumentDate:        documentDate,
		PostingDate:         postingDate,
		Lines:               lines,
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/postings/events", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("failed to post AR invoice issuance accounting event", zap.Error(err))
		return "", ErrUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		c.log.Error("GL returned error posting AR issuance event", zap.Int("status", resp.StatusCode), zap.String("body", string(body)))
		return "", ErrUnavailable
	}

	var execResp postingExecutionResponse
	if err := json.NewDecoder(resp.Body).Decode(&execResp); err != nil {
		return "", err
	}
	if execResp.JournalID == nil {
		return "", ErrUnavailable
	}
	return *execResp.JournalID, nil
}
