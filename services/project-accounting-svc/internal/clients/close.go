// Package clients holds project-accounting-svc's outbound HTTP clients to
// other platform services.
package clients

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"go.uber.org/zap"

	"zoiko.io/project-accounting-svc/internal/domain"
)

type Clients struct {
	closeURL  string
	ledgerURL string
	http      *http.Client
	log       *zap.Logger
}

func New(closeURL, ledgerURL string, log *zap.Logger) *Clients {
	return &Clients{closeURL: closeURL, ledgerURL: ledgerURL, http: &http.Client{Timeout: 5 * time.Second}, log: log}
}

type periodStatusResp struct {
	CloseStatus string `json:"close_status"`
}

// CheckPeriodOpen is PRJ-03's own "hard-closed-period" dependency on
// financial-close-svc — the spec's own negative path, "closed period ...
// blocks certification." Mirrors inventory-management-svc's own
// internal/clients.Clients.CheckPeriodOpen exactly: a period financial-
// close-svc has never heard of defaults to open (this service does not
// own the calendar), and an unreachable financial-close-svc fails
// CLOSED, never silently open.
func (c *Clients) CheckPeriodOpen(ctx context.Context, tenantID, legalEntityID, periodName string) error {
	u, err := url.Parse(c.closeURL + "/v1/close/periods/status")
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("legal_entity_id", legalEntityID)
	q.Set("period_name", periodName)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return domain.ErrStoreUnavailable
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("financial-close-svc unreachable — failing closed on recognition run certification", zap.Error(err))
		return domain.ErrPeriodCheckUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil // period not registered with financial-close-svc — defaults to open
	}
	if resp.StatusCode != http.StatusOK {
		c.log.Error("unexpected status from financial-close-svc — failing closed", zap.Int("status", resp.StatusCode))
		return domain.ErrPeriodCheckUnavailable
	}

	var statusResp periodStatusResp
	if err := json.NewDecoder(resp.Body).Decode(&statusResp); err != nil {
		return domain.ErrPeriodCheckUnavailable
	}
	if statusResp.CloseStatus == "LOCKED" || statusResp.CloseStatus == "CLOSED" {
		return domain.ErrPeriodLocked
	}
	return nil
}
