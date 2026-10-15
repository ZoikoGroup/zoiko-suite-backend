// Package paymentstatus is a read-only HTTP client to payment-status-svc
// (BNK-07). AP-08 uses it to confirm, before applying a payment to a
// payable, that Banking itself reports the payment SETTLED (AP-08 failure
// semantics: "payment application requires authoritative BNK status").
// Fails closed throughout.
package paymentstatus

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"go.uber.org/zap"
)

var ErrUnavailable = errors.New("payment-status-svc unavailable")

type Client interface {
	GetStatus(ctx context.Context, tenantID, paymentID string) (*PaymentState, error)
}

// PaymentState is the subset of BNK-07's PaymentExecutionState (PascalCase
// wire shape, no json tags on BNK-07's side) AP-08 needs.
type PaymentState struct {
	PaymentID       string `json:"PaymentID"`
	LegalEntityID   string `json:"LegalEntityID"`
	Status          string `json:"Status"`
	HasOpenConflict bool   `json:"HasOpenConflict"`
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{baseURL: baseURL, log: log, http: &http.Client{Timeout: 3 * time.Second}}
}

func (c *HTTPClient) GetStatus(ctx context.Context, tenantID, paymentID string) (*PaymentState, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/bnk07/payments/"+paymentID, nil)
	if err != nil {
		return nil, ErrUnavailable
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("payment-status-svc unreachable — failing closed", zap.Error(err))
		return nil, ErrUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return &PaymentState{PaymentID: paymentID}, nil // unknown to Banking: not settled
	}
	if resp.StatusCode != http.StatusOK {
		c.log.Error("unexpected response from payment-status-svc — failing closed", zap.Int("status", resp.StatusCode))
		return nil, ErrUnavailable
	}
	var out PaymentState
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, ErrUnavailable
	}
	return &out, nil
}
