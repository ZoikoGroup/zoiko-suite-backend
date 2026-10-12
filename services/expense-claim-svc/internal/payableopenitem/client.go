// Package payableopenitem is a real HTTP client to payable-open-item-svc
// (AP-08). The hand-off is driven by the payable relay from a durable
// payable_requests row, so creation is retried until it succeeds; it is
// idempotent on AP-08's own (source_type, source_reference = claim id)
// uniqueness, and every call also carries a stable Idempotency-Key. Fails
// closed: a transport error or non-2xx response is reported to the caller,
// never silently swallowed.
package payableopenitem

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"go.uber.org/zap"

	"zoiko.io/expense-claim-svc/internal/domain"
)

type SourceType string

const SourceExpenseClaim SourceType = "EXPENSE_CLAIM"

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
	PayableID      string  `json:"PayableID"`
	Status         string  `json:"Status"`
	ResidualAmount float64 `json:"ResidualAmount"`
}

// StatusSettled is AP-08's fully-settled status.
const StatusSettled = "SETTLED"

type Client interface {
	// CreatePayableFromApprovedSource is idempotent on source_reference.
	CreatePayableFromApprovedSource(ctx context.Context, tenantID, principalID string, req CreatePayableRequest) (*PayableOpenItem, error)
	// GetPayable reads a payable's current status (settlement observation).
	GetPayable(ctx context.Context, tenantID, principalID, payableID string) (*PayableOpenItem, error)
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
		return nil, domain.ErrPayableServiceUnavailable
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/ap08/payables", bytes.NewReader(body))
	if err != nil {
		return nil, domain.ErrPayableServiceUnavailable
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Tenant-Id", tenantID)
	httpReq.Header.Set("X-Principal-Id", principalID)
	httpReq.Header.Set("Idempotency-Key", "expense-claim-payable:"+req.SourceReference)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		c.log.Error("payable-open-item-svc unreachable — failing closed", zap.Error(err))
		return nil, domain.ErrPayableServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		c.log.Error("unexpected response from payable-open-item-svc — failing closed", zap.Int("status", resp.StatusCode))
		return nil, domain.ErrPayableServiceUnavailable
	}
	var out PayableOpenItem
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.PayableID == "" {
		return nil, domain.ErrPayableServiceUnavailable
	}
	return &out, nil
}

func (c *HTTPClient) GetPayable(ctx context.Context, tenantID, principalID, payableID string) (*PayableOpenItem, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/ap08/payables/"+payableID, nil)
	if err != nil {
		return nil, domain.ErrPayableServiceUnavailable
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, domain.ErrPayableServiceUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, domain.ErrPayableServiceUnavailable
	}
	var out PayableOpenItem
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, domain.ErrPayableServiceUnavailable
	}
	return &out, nil
}
