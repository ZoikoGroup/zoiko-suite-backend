// Package ledger provides a client against general-ledger-svc for posting
// AP invoice approval accounting events (ACC-14).
package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"

	"zoiko.io/accounts-payable-svc/internal/domain"
)

var (
	// ErrUnavailable means general-ledger-svc could not be reached or answered
	// something unexpected. Callers MUST fail closed.
	ErrUnavailable = errors.New("general-ledger-svc unavailable")
)

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

// HTTPClient implements the Client interface.
type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

// NewHTTPClient builds a new ledger client.
func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{
		baseURL: baseURL,
		http:    &http.Client{Timeout: 3 * time.Second},
		log:     log,
	}
}

// Client is the interface the handler depends on.
type Client interface {
	PostInvoiceApprovedAccountingEvent(ctx context.Context, tenantID, principalID string, inv *domain.VendorInvoice) (journalID string, err error)
}

// PostInvoiceApprovedAccountingEvent posts the AP invoice approval accounting
// event through general-ledger-svc's system-originated posting path
// (POST /v1/postings/events).
//
// The invoice's InvoiceID is used as source_event_id, making a retried call
// idempotent against GL's own UNIQUE(tenant_id, source_event_id) constraint.
//
// Lines posted (using account mappings registered in GL via ACC-02):
//   - Debit: AP_EXPENSE_STANDARD for inv.Amount (gross)
//   - Credit: AP_PAYABLE_STANDARD for inv.Amount
//
// FiscalPeriod is derived from inv.InvoiceDate as "YYYY-MM".
// DocumentDate and PostingDate are both set to inv.InvoiceDate as "YYYY-MM-DD".
// TransactionCurrency is inv.CurrencyCode.
func (c *HTTPClient) PostInvoiceApprovedAccountingEvent(ctx context.Context, tenantID, principalID string, inv *domain.VendorInvoice) (journalID string, err error) {
	fiscalPeriod := inv.InvoiceDate.Time.Format("2006-01")
	documentDate := inv.InvoiceDate.Time.Format("2006-01-02")
	postingDate := inv.InvoiceDate.Time.Format("2006-01-02")

	description := fmt.Sprintf("AP invoice %s approved", inv.InvoiceNumber)

	lines := []postingEventLine{
		{MappingKey: "AP_EXPENSE_STANDARD", DebitAmount: inv.Amount},
		{MappingKey: "AP_PAYABLE_STANDARD", CreditAmount: inv.Amount},
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
		c.log.Error("failed to post AP invoice approval accounting event", zap.Error(err))
		return "", ErrUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		c.log.Error("GL returned error posting AP approval event", zap.Int("status", resp.StatusCode), zap.String("body", string(body)))
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