// Package paymentstatus is a real HTTP client to payment-status-svc
// (BNK-07) — the first real caller of BNK-07's RecordPaymentStatus and
// GetPaymentStatus anywhere in this codebase. Fails closed throughout.
package paymentstatus

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"go.uber.org/zap"

	"zoiko.io/payment-run-svc/internal/domain"
)

type Client interface {
	// RecordInitialStatus registers the canonical BNK-07 execution record for
	// an instruction right after BNK-06 accepts submission, correlating it
	// via ProviderRequestID (BNK-06's own attempt reference) and
	// SourceReference (this service's own instruction ID).
	RecordInitialStatus(ctx context.Context, tenantID, principalID string, req RecordInitialStatusRequest) (*PaymentState, error)
	// GetStatus fetches BNK-07's real, current canonical status for a
	// payment — this is what a poll asks instead of accepting a caller's
	// unverified word for the external status.
	GetStatus(ctx context.Context, tenantID, paymentID string) (*PaymentState, error)
}

type RecordInitialStatusRequest struct {
	LegalEntityID     string
	ProviderRequestID string
	SourceReference   string
}

// PaymentState is the subset of BNK-07's own PaymentExecutionState
// (PascalCase wire shape, no json tags on BNK-07's side) this service needs.
type PaymentState struct {
	PaymentID string `json:"PaymentID"`
	Status    string `json:"Status"`
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{baseURL: baseURL, log: log, http: &http.Client{Timeout: 5 * time.Second}}
}

func (c *HTTPClient) RecordInitialStatus(ctx context.Context, tenantID, principalID string, req RecordInitialStatusRequest) (*PaymentState, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, domain.ErrPaymentStatusUnavailable
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/bnk07/payments", bytes.NewReader(body))
	if err != nil {
		return nil, domain.ErrPaymentStatusUnavailable
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Tenant-Id", tenantID)
	httpReq.Header.Set("X-Principal-Id", principalID)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		c.log.Error("payment-status-svc unreachable — failing closed", zap.Error(err))
		return nil, domain.ErrPaymentStatusUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		c.log.Error("unexpected response recording initial status — failing closed", zap.Int("status", resp.StatusCode))
		return nil, domain.ErrPaymentStatusUnavailable
	}
	var out PaymentState
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, domain.ErrPaymentStatusUnavailable
	}
	return &out, nil
}

func (c *HTTPClient) GetStatus(ctx context.Context, tenantID, paymentID string) (*PaymentState, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/bnk07/payments/"+paymentID, nil)
	if err != nil {
		return nil, domain.ErrPaymentStatusUnavailable
	}
	httpReq.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		c.log.Error("payment-status-svc unreachable — failing closed", zap.Error(err))
		return nil, domain.ErrPaymentStatusUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.log.Error("unexpected response fetching payment status — failing closed", zap.Int("status", resp.StatusCode))
		return nil, domain.ErrPaymentStatusUnavailable
	}
	var out PaymentState
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, domain.ErrPaymentStatusUnavailable
	}
	return &out, nil
}
