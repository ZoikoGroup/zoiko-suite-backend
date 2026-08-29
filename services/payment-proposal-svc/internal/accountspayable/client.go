// Package accountspayable verifies a vendor invoice against the real
// accounts-payable-svc before it can be selected into a payment proposal.
// Fails closed. accounts-payable-svc has no hold/disputed concept and no
// bank/payee reference field at all — see internal/domain's package doc.
package accountspayable

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"go.uber.org/zap"

	"zoiko.io/payment-proposal-svc/internal/domain"
)

// Invoice's DueDate is time.Time — accounts-payable-svc marshals it as an
// RFC3339 timestamp, which Go's json package decodes into time.Time
// automatically.

type Client interface {
	// GetEligibleInvoice confirms invoiceID exists, belongs to
	// tenantID/legalEntityID, and is APPROVED (eligible for a payment
	// proposal — not yet RECEIVED/VALIDATED, and not already
	// PAYMENT_REQUESTED by some other flow).
	GetEligibleInvoice(ctx context.Context, tenantID, legalEntityID, invoiceID string) (*Invoice, error)
}

type Invoice struct {
	InvoiceID     string    `json:"invoice_id"`
	TenantID      string    `json:"tenant_id"`
	LegalEntityID string    `json:"legal_entity_id"`
	VendorID      string    `json:"vendor_id"`
	Amount        float64   `json:"amount"`
	CurrencyCode  string    `json:"currency_code"`
	DueDate       time.Time `json:"due_date"`
	Status        string    `json:"status"`
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{baseURL: baseURL, log: log, http: &http.Client{Timeout: 2 * time.Second}}
}

func (c *HTTPClient) GetEligibleInvoice(ctx context.Context, tenantID, legalEntityID, invoiceID string) (*Invoice, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/invoices/"+invoiceID, nil)
	if err != nil {
		return nil, domain.ErrPayableServiceUnavailable
	}

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("accounts-payable-svc unreachable — failing closed", zap.Error(err))
		return nil, domain.ErrPayableServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, domain.ErrPayableNotEligible
	}
	if resp.StatusCode != http.StatusOK {
		c.log.Error("unexpected response from accounts-payable-svc — failing closed", zap.Int("status", resp.StatusCode))
		return nil, domain.ErrPayableServiceUnavailable
	}

	var inv Invoice
	if err := json.NewDecoder(resp.Body).Decode(&inv); err != nil {
		return nil, domain.ErrPayableServiceUnavailable
	}
	if inv.InvoiceID == "" || inv.TenantID != tenantID || inv.LegalEntityID != legalEntityID || inv.Status != "APPROVED" {
		return nil, domain.ErrPayableNotEligible
	}
	return &inv, nil
}
