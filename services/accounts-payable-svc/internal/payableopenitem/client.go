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

var (
	ErrPayableServiceUnavailable  = errors.New("payable-open-item-svc unavailable")
	ErrPayableAuthorizationDenied = errors.New("payable-open-item-svc refused the principal")
)

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

// Envelope carries the outbound call's governed headers. AP-08 validates the
// 23-header envelope the same way every consumer does, and a call arriving
// without correlation_id, request_id, a source channel or an idempotency key
// is a call that contaminates AP-08's audit trail — so the headers this
// service received are forwarded, and an idempotency key is supplied by the
// caller (handed from the inbound request) rather than omitted.
type Envelope struct {
	CorrelationID string
	RequestID     string
	SourceChannel string
	IdempotencyKey string
}

// PayableOpenItem is the subset of AP-08's own PayableOpenItem (PascalCase
// wire shape, no json tags on AP-08's side) this service needs.
type PayableOpenItem struct {
	PayableID string `json:"PayableID"`
	Status    string `json:"Status"`
}

type Client interface {
	CreatePayableFromApprovedSource(ctx context.Context, tenantID, principalID string, envelope Envelope, req CreatePayableRequest) (*PayableOpenItem, error)
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{baseURL: baseURL, log: log, http: &http.Client{Timeout: 5 * time.Second}}
}

func (c *HTTPClient) CreatePayableFromApprovedSource(ctx context.Context, tenantID, principalID string, envelope Envelope, req CreatePayableRequest) (*PayableOpenItem, error) {
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
	httpReq.Header.Set("X-Correlation-ID", envelope.CorrelationID)
	if envelope.RequestID != "" {
		httpReq.Header.Set("X-Request-Id", envelope.RequestID)
	}
	if envelope.SourceChannel != "" {
		httpReq.Header.Set("X-Source-Channel", envelope.SourceChannel)
	}
	if envelope.IdempotencyKey != "" {
		httpReq.Header.Set("Idempotency-Key", envelope.IdempotencyKey)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		c.log.Error("payable-open-item-svc unreachable — failing closed", zap.Error(err))
		return nil, ErrPayableServiceUnavailable
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusCreated, http.StatusOK:
		// success below
	case http.StatusForbidden:
		// AP-08 authorized the principal for its own SOURCE_* action, not for
		// ACCOUNT_PAYABLE_WRITE on this tenant/entity. That is a real denial,
		// not an outage — logged distinctly and reported distinctly, so an
		// approval that stands despite a refused payable is diagnosable instead
		// of reading as "AP-08 down".
		c.log.Error("payable-open-item-svc denied the payable write — approval stands", zap.Int("status", resp.StatusCode))
		return nil, ErrPayableAuthorizationDenied
	default:
		c.log.Error("unexpected response from payable-open-item-svc — failing closed", zap.Int("status", resp.StatusCode))
		return nil, ErrPayableServiceUnavailable
	}
	var out PayableOpenItem
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, ErrPayableServiceUnavailable
	}
	return &out, nil
}
