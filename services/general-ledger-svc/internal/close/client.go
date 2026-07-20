package close

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"go.uber.org/zap"
	"zoiko.io/general-ledger-svc/internal/domain"
)

type Client interface {
	CheckPeriodOpen(ctx context.Context, tenantID, legalEntityID, periodName string) error
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{
		baseURL: baseURL,
		log:     log,
		http:    &http.Client{Timeout: 2 * time.Second, Transport: newRetryTransport()},
	}
}

type periodStatusResp struct {
	CloseStatus string `json:"close_status"`
}

func (c *HTTPClient) CheckPeriodOpen(ctx context.Context, tenantID, legalEntityID, periodName string) error {
	u, err := url.Parse(c.baseURL + "/v1/close/periods/status")
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("legal_entity_id", legalEntityID)
	q.Set("period_name", periodName)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return domain.ErrCloseServiceUnavailable
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("financial-close-svc unreachable — failing closed on journal posting", zap.Error(err))
		return domain.ErrCloseServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil // If period not registered, default to open
	}
	if resp.StatusCode != http.StatusOK {
		c.log.Error("unexpected status from financial-close-svc — failing closed", zap.Int("status", resp.StatusCode))
		return domain.ErrCloseServiceUnavailable
	}

	var statusResp periodStatusResp
	if err := json.NewDecoder(resp.Body).Decode(&statusResp); err != nil {
		return domain.ErrCloseServiceUnavailable
	}

	if statusResp.CloseStatus == "LOCKED" || statusResp.CloseStatus == "CLOSED" {
		return domain.ErrPeriodLocked
	}
	return nil
}
