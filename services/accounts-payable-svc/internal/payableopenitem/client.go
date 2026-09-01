// Package payableopenitem is a real HTTP client to payable-open-item-svc
// (AP-08) — accounts-payable-svc's first real payables consumer. AP-08 has
// only ever had expense-claim-svc as a real source until now; this is its
// second, closing the gap AP-08's own package doc named as a natural next
// step. The call is deliberately best-effort: a failure never blocks or
// reverses ApproveInvoice, which has already committed. Fails closed on the
// call itself (reports the error to the caller), but the caller decides
// what to do with that — see internal/handler's ApproveInvoice.
package payableopenitem

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"go.uber.org/zap"
)

var ErrPayableServiceUnavailable = errors.New("payable-open-item-svc unavailable")

type SourceType string

const SourceSupplierInvoice SourceType = "SUPPLIER_INVOICE"

type CreatePayableRequest struct {
	LegalEntityID   string
	SourceType      SourceType
	SourceReference string
	PayeeRef        string
	OriginalAmount  float64
	Currency        string
	DueDate         time.Time
}

// PayableOpenItem is the subset of AP-08's own PayableOpenItem (PascalCase
// wire shape, no json tags on AP-08's side) this service needs.
type PayableOpenItem struct {
	PayableID string `json:"PayableID"`
	Status    string `json:"Status"`
}

type Client interface {
	CreatePayableFromApprovedSource(ctx context.Context, tenantID, principalID string, req CreatePayableRequest) (*PayableOpenItem, error)
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{baseURL: baseURL, log: log, http: &http.Client{Timeout: 5 * time.Second}}
}

func (c *HTTPClient) CreatePayableFromApprovedSource(ctx context.Context, tenantID, principalID string, req CreatePayableRequest) (*PayableOpenItem, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, ErrPayableServiceUnavailable
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/ap08/payables", bytes.NewReader(body))
	if err != nil {
		return nil, ErrPayableServiceUnavailable
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Tenant-Id", tenantID)
	httpReq.Header.Set("X-Principal-Id", principalID)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		c.log.Error("payable-open-item-svc unreachable — failing closed", zap.Error(err))
		return nil, ErrPayableServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		c.log.Error("unexpected response from payable-open-item-svc — failing closed", zap.Int("status", resp.StatusCode))
		return nil, ErrPayableServiceUnavailable
	}
	var out PayableOpenItem
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, ErrPayableServiceUnavailable
	}
	return &out, nil
}
