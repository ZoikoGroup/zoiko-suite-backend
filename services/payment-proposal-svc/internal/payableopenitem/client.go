// Package payableopenitem verifies a payable against the real
// payable-open-item-svc (AP-08) before it can be selected into a payment
// proposal — replacing the previous direct calls to expense-claim-svc
// (EXPENSE_CLAIM items) and accounts-payable-svc (AP_INVOICE items). AP-08
// is both peers' own real payables consumer now (see their own updated
// package docs), so checking a payable's real open/residual/hold/dispute
// state here is a materially stronger real check than either peer's own
// simple status test. This client confirms the payable still exists and is
// unsettled (GetEligiblePayable); whether a held/disputed one may still be
// force-added is the calling handler's own SoD decision — never decided
// unilaterally here. Fails closed.
package payableopenitem

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"go.uber.org/zap"

	"zoiko.io/payment-proposal-svc/internal/domain"
)

type SourceType string

const (
	SourceExpenseClaim    SourceType = "EXPENSE_CLAIM"
	SourceSupplierInvoice SourceType = "SUPPLIER_INVOICE"
)

type Client interface {
	// GetEligiblePayable looks up the AP-08 payable created for
	// (sourceType, sourceReference) and confirms it belongs to
	// legalEntityID and is genuinely eligible for payment selection right
	// now.
	GetEligiblePayable(ctx context.Context, tenantID, legalEntityID string, sourceType SourceType, sourceReference string) (*Payable, error)
}

// Payable is the subset of AP-08's own PayableOpenItem (PascalCase wire
// shape, no json tags on AP-08's side) this service needs.
type Payable struct {
	PayableID       string    `json:"PayableID"`
	LegalEntityID   string    `json:"LegalEntityID"`
	SourceReference string    `json:"SourceReference"`
	PayeeRef        string    `json:"PayeeRef"`
	ResidualAmount  float64   `json:"ResidualAmount"`
	Currency        string    `json:"Currency"`
	DueDate         time.Time `json:"DueDate"`
	Status          string    `json:"Status"`
	IsHeld          bool      `json:"IsHeld"`
	IsDisputed      bool      `json:"IsDisputed"`
}

// isSelectable checks only that the payable is still a real, unsettled
// liability (open or partially-settled, residual left) — the existence/
// status half of eligibility. It deliberately does NOT reject on
// IsHeld/IsDisputed: whether a held or disputed payable may still be
// force-added is the caller handler's own SoD decision (the same
// ExceptionRef override pattern already used for an ON_HOLD AP_INVOICE
// supplier), not something this client should decide unilaterally.
func isSelectable(p Payable) bool {
	return (p.Status == "OPEN" || p.Status == "PARTIALLY_SETTLED") && p.ResidualAmount > 0
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{baseURL: baseURL, log: log, http: &http.Client{Timeout: 2 * time.Second}}
}

func (c *HTTPClient) GetEligiblePayable(ctx context.Context, tenantID, legalEntityID string, sourceType SourceType, sourceReference string) (*Payable, error) {
	q := url.Values{"source_type": {string(sourceType)}, "source_reference": {sourceReference}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/ap08/payables/by-source?"+q.Encode(), nil)
	if err != nil {
		return nil, domain.ErrPayableServiceUnavailable
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("payable-open-item-svc unreachable — failing closed", zap.Error(err))
		return nil, domain.ErrPayableServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, domain.ErrPayableNotEligible
	}
	if resp.StatusCode != http.StatusOK {
		c.log.Error("unexpected response from payable-open-item-svc — failing closed", zap.Int("status", resp.StatusCode))
		return nil, domain.ErrPayableServiceUnavailable
	}

	var p Payable
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return nil, domain.ErrPayableServiceUnavailable
	}
	if p.PayableID == "" || p.LegalEntityID != legalEntityID {
		return nil, domain.ErrPayableNotEligible
	}
	if !isSelectable(p) {
		return nil, domain.ErrPayableNotEligible
	}
	return &p, nil
}
