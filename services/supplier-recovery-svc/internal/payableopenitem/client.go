// Package payableopenitem is a real HTTP client to payable-open-item-svc
// (AP-08) — the first real caller of AP-08's ApplyRecovery anywhere in this
// codebase. AP-08's own package doc has named ApplyRecovery as having "no
// real caller yet" since AP-08 was built; this service is that caller.
// Fails closed.
package payableopenitem

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"go.uber.org/zap"

	"zoiko.io/supplier-recovery-svc/internal/domain"
)

type ApplyRecoveryRequest struct {
	Amount      float64
	RecoveryRef string
	Reason      string
}

// Payable is the subset of AP-08's own PayableOpenItem (PascalCase wire
// shape, no json tags on AP-08's side) this service needs.
type Payable struct {
	PayableID      string  `json:"PayableID"`
	LegalEntityID  string  `json:"LegalEntityID"`
	ResidualAmount float64 `json:"ResidualAmount"`
	Status         string  `json:"Status"`
}

type Client interface {
	// GetPayable confirms the source payable this recovery case names
	// genuinely exists.
	GetPayable(ctx context.Context, tenantID, payableID string) (*Payable, error)
	// ApplyRecovery is a real, state-changing call against AP-08 — it
	// genuinely reduces the source payable's residual, not a locally
	// recorded intention.
	ApplyRecovery(ctx context.Context, tenantID, principalID, payableID string, req ApplyRecoveryRequest) (*Payable, error)
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{baseURL: baseURL, log: log, http: &http.Client{Timeout: 5 * time.Second}}
}

func (c *HTTPClient) GetPayable(ctx context.Context, tenantID, payableID string) (*Payable, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/ap08/payables/"+payableID, nil)
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
		return nil, domain.ErrCaseNotFound
	}
	if resp.StatusCode != http.StatusOK {
		c.log.Error("unexpected response from payable-open-item-svc — failing closed", zap.Int("status", resp.StatusCode))
		return nil, domain.ErrPayableServiceUnavailable
	}
	var p Payable
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return nil, domain.ErrPayableServiceUnavailable
	}
	return &p, nil
}

func (c *HTTPClient) ApplyRecovery(ctx context.Context, tenantID, principalID, payableID string, req ApplyRecoveryRequest) (*Payable, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, domain.ErrPayableServiceUnavailable
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/ap08/payables/"+payableID+"/apply-recovery", bytes.NewReader(body))
	if err != nil {
		return nil, domain.ErrPayableServiceUnavailable
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Tenant-Id", tenantID)
	httpReq.Header.Set("X-Principal-Id", principalID)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		c.log.Error("payable-open-item-svc unreachable — failing closed", zap.Error(err))
		return nil, domain.ErrPayableServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.log.Error("unexpected response applying recovery at payable-open-item-svc — failing closed", zap.Int("status", resp.StatusCode))
		return nil, domain.ErrOffsetFailedAtPayable
	}
	var p Payable
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return nil, domain.ErrPayableServiceUnavailable
	}
	return &p, nil
}
