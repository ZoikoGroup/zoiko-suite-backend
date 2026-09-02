// Package provideradapter is a real HTTP client to
// payment-initiation-adapter-svc (BNK-06) — the first real caller of
// BNK-06's PrepareAttempt/SubmitAttempt anywhere in this codebase. Fails
// closed throughout: any transport error, non-2xx response, or unreadable
// body is reported to the caller as ErrProviderAdapterUnavailable, never
// silently treated as success.
package provideradapter

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
	// PrepareAndSubmit calls BNK-06's PrepareAttempt then SubmitAttempt in
	// sequence, using the same idempotency key for both — a retry of the
	// whole SubmitPaymentRun call reaches the same PREPARED row rather than
	// creating a second attempt (BNK-06's own duplicate-idempotency-key
	// handling returns the existing attempt).
	PrepareAndSubmit(ctx context.Context, tenantID, principalID string, req PrepareAndSubmitRequest) (*Attempt, error)
}

type PrepareAndSubmitRequest struct {
	LegalEntityID        string
	SourceReference      string
	PayerAccountRef      string
	PayeeRef             string
	Amount               float64
	Currency             string
	ExecutionDate        time.Time
	PayerAccountVerified bool
	IdempotencyKey       string
}

// Attempt is the subset of BNK-06's own PaymentInitiationAttempt (PascalCase
// wire shape, no json tags on BNK-06's side) this service needs.
type Attempt struct {
	AttemptID         string `json:"AttemptID"`
	Status            string `json:"Status"`
	ProviderRequestID string `json:"ProviderRequestID"`
	RejectionReason   string `json:"RejectionReason"`
}

type prepareRequestBody struct {
	LegalEntityID        string    `json:"LegalEntityID"`
	SourceReference      string    `json:"SourceReference"`
	PayerAccountRef      string    `json:"PayerAccountRef"`
	PayeeRef             string    `json:"PayeeRef"`
	Amount               float64   `json:"Amount"`
	Currency             string    `json:"Currency"`
	ExecutionDate        time.Time `json:"ExecutionDate"`
	PayerAccountVerified bool      `json:"PayerAccountVerified"`
	IdempotencyKey       string    `json:"IdempotencyKey"`
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{baseURL: baseURL, log: log, http: &http.Client{Timeout: 5 * time.Second}}
}

func (c *HTTPClient) doJSON(ctx context.Context, method, path, tenantID, principalID string, body interface{}, out interface{}) error {
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return domain.ErrProviderAdapterUnavailable
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return domain.ErrProviderAdapterUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("payment-initiation-adapter-svc unreachable — failing closed", zap.Error(err))
		return domain.ErrProviderAdapterUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		c.log.Error("unexpected response from payment-initiation-adapter-svc — failing closed", zap.Int("status", resp.StatusCode))
		return domain.ErrProviderAdapterUnavailable
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return domain.ErrProviderAdapterUnavailable
		}
	}
	return nil
}

func (c *HTTPClient) PrepareAndSubmit(ctx context.Context, tenantID, principalID string, req PrepareAndSubmitRequest) (*Attempt, error) {
	var prepared Attempt
	if err := c.doJSON(ctx, http.MethodPost, "/bnk06/attempts", tenantID, principalID, prepareRequestBody{
		LegalEntityID:        req.LegalEntityID,
		SourceReference:      req.SourceReference,
		PayerAccountRef:      req.PayerAccountRef,
		PayeeRef:             req.PayeeRef,
		Amount:               req.Amount,
		Currency:             req.Currency,
		ExecutionDate:        req.ExecutionDate,
		PayerAccountVerified: req.PayerAccountVerified,
		IdempotencyKey:       req.IdempotencyKey,
	}, &prepared); err != nil {
		return nil, err
	}

	var submitted Attempt
	if err := c.doJSON(ctx, http.MethodPost, "/bnk06/attempts/"+prepared.AttemptID+"/submit", tenantID, principalID, nil, &submitted); err != nil {
		return nil, err
	}
	return &submitted, nil
}
