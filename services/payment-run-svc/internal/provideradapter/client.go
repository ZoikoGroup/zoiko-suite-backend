// Package provideradapter is a real HTTP client to
// payment-initiation-adapter-svc (BNK-06).
//
// Errors are classified because AP-11 must tell "nothing was sent" apart
// from "we don't know whether it was sent" (invariant #18):
//
//   - ErrBankingPrepareRejected: BNK-06 refused PrepareAttempt with a 4xx.
//     Nothing reached the bank.
//   - ErrBankingAttemptConflict: BNK-06 answered 409 to submit/retry — the
//     attempt moved on (usually an earlier call reached it). Re-read it.
//   - ErrProviderAdapterUnavailable: transport error, timeout, 5xx or an
//     unreadable body. For Prepare that is safe to retry (Prepare is
//     idempotent on IdempotencyKey and sends nothing to the bank); for
//     Submit/Retry the outcome is UNKNOWN.
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

// BNK-06 attempt statuses AP-11 acts on.
const (
	AttemptPrepared                 = "PREPARED"
	AttemptSubmitted                = "SUBMITTED"
	AttemptPendingUnknown           = "PENDING_UNKNOWN"
	AttemptRejectedBeforeSubmission = "REJECTED_BEFORE_SUBMISSION"
	AttemptCancelled                = "CANCELLED"
	AttemptQuarantined              = "QUARANTINED"
)

type Client interface {
	// Prepare calls BNK-06 PrepareAttempt. A repeat with the same
	// IdempotencyKey returns the existing attempt (in whatever status it has
	// reached) rather than creating a second one.
	Prepare(ctx context.Context, tenantID, principalID string, req PrepareRequest) (*Attempt, error)
	// Submit calls BNK-06 SubmitAttempt for a PREPARED attempt.
	Submit(ctx context.Context, tenantID, principalID, attemptID string) (*Attempt, error)
	// Retry calls BNK-06 RetrySameAttempt for a PENDING_UNKNOWN attempt —
	// same attempt, same idempotency key, never a new payment.
	Retry(ctx context.Context, tenantID, principalID, attemptID string) (*Attempt, error)
	// GetAttempt reads BNK-06's current view of an attempt.
	GetAttempt(ctx context.Context, tenantID, principalID, attemptID string) (*Attempt, error)
}

type PrepareRequest struct {
	LegalEntityID        string
	SourceReference      string
	PayerAccountRef      string
	PayeeRef             string
	Amount               float64
	Currency             string
	ExecutionDate        time.Time
	PayerAccountVerified bool
	IdempotencyKey       string
	// AuthorizationID/AuthorizationFingerprint/AuthorizationSource carry
	// AP-10's own service-computed fingerprint through to BNK-06 for
	// independent re-verification there (Wave 11a).
	AuthorizationID          string
	AuthorizationFingerprint string
	AuthorizationSource      string
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
	LegalEntityID            string    `json:"LegalEntityID"`
	SourceReference          string    `json:"SourceReference"`
	PayerAccountRef          string    `json:"PayerAccountRef"`
	PayeeRef                 string    `json:"PayeeRef"`
	Amount                   float64   `json:"Amount"`
	Currency                 string    `json:"Currency"`
	ExecutionDate            time.Time `json:"ExecutionDate"`
	PayerAccountVerified     bool      `json:"PayerAccountVerified"`
	IdempotencyKey           string    `json:"IdempotencyKey"`
	AuthorizationID          string    `json:"AuthorizationID"`
	AuthorizationFingerprint string    `json:"AuthorizationFingerprint"`
	AuthorizationSource      string    `json:"AuthorizationSource"`
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{baseURL: baseURL, log: log, http: &http.Client{Timeout: 5 * time.Second}}
}

// doJSON maps the response to the error classes in the package doc. 4xx
// other than 409 is reported as rejectErr, which callers pick per call.
func (c *HTTPClient) doJSON(ctx context.Context, method, path, tenantID, principalID string, body interface{}, out interface{}, rejectErr error) error {
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
		c.log.Error("payment-initiation-adapter-svc unreachable — outcome unknown to caller", zap.String("path", path), zap.Error(err))
		return domain.ErrProviderAdapterUnavailable
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated:
	case resp.StatusCode == http.StatusConflict:
		return domain.ErrBankingAttemptConflict
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		c.log.Warn("payment-initiation-adapter-svc refused the request", zap.String("path", path), zap.Int("status", resp.StatusCode))
		return rejectErr
	default:
		c.log.Error("unexpected response from payment-initiation-adapter-svc", zap.String("path", path), zap.Int("status", resp.StatusCode))
		return domain.ErrProviderAdapterUnavailable
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return domain.ErrProviderAdapterUnavailable
		}
	}
	return nil
}

func (c *HTTPClient) Prepare(ctx context.Context, tenantID, principalID string, req PrepareRequest) (*Attempt, error) {
	var out Attempt
	if err := c.doJSON(ctx, http.MethodPost, "/bnk06/attempts", tenantID, principalID, prepareRequestBody(req), &out, domain.ErrBankingPrepareRejected); err != nil {
		return nil, err
	}
	if out.AttemptID == "" {
		return nil, domain.ErrProviderAdapterUnavailable
	}
	return &out, nil
}

func (c *HTTPClient) Submit(ctx context.Context, tenantID, principalID, attemptID string) (*Attempt, error) {
	var out Attempt
	// A 4xx other than 409 after the attempt exists is not a definitive
	// "not sent" answer from the bank's side, so it is reported as
	// unavailable (outcome unknown) rather than rejected.
	if err := c.doJSON(ctx, http.MethodPost, "/bnk06/attempts/"+attemptID+"/submit", tenantID, principalID, nil, &out, domain.ErrProviderAdapterUnavailable); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *HTTPClient) Retry(ctx context.Context, tenantID, principalID, attemptID string) (*Attempt, error) {
	var out Attempt
	if err := c.doJSON(ctx, http.MethodPost, "/bnk06/attempts/"+attemptID+"/retry", tenantID, principalID, nil, &out, domain.ErrProviderAdapterUnavailable); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *HTTPClient) GetAttempt(ctx context.Context, tenantID, principalID, attemptID string) (*Attempt, error) {
	var out Attempt
	if err := c.doJSON(ctx, http.MethodGet, "/bnk06/attempts/"+attemptID, tenantID, principalID, nil, &out, domain.ErrProviderAdapterUnavailable); err != nil {
		return nil, err
	}
	return &out, nil
}
