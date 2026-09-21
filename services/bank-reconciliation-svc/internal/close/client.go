// Package close is BNK-05's Wave 13 dependency on financial-close-svc —
// CertifyRun checks it before writing a certificate, mirroring
// general-ledger-svc's own internal/close/client.go idiom exactly (same
// endpoint, same period_name format, same fail-closed posture) rather
// than inventing a second, divergent integration with the same service.
//
// The doc's own SoD rule for BNK-05 is explicit: "closed-period/control
// exceptions need authorized remediation." Before this package existed,
// nothing in bank-reconciliation-svc checked period status at all —
// CertifyRun could certify a run against a period general-ledger-svc
// itself would already refuse to post journals into.
package close

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"go.uber.org/zap"

	"zoiko.io/bank-reconciliation-svc/internal/domain"
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

// CheckPeriodOpen fails closed (domain.ErrCloseServiceUnavailable) on any
// transport error or unexpected status, and maps a CLOSED/LOCKED period
// to domain.ErrPeriodLocked. A 404 (period never registered with
// financial-close-svc) is treated as open — the same default
// general-ledger-svc's own client already uses for this exact endpoint:
// most legal-entity/period combinations are never explicitly registered
// unless someone closes them, so "unregistered" is not the ambiguous
// case to fail closed on; "registered and locked" is.
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
		c.log.Error("financial-close-svc unreachable — failing closed on certification", zap.Error(err))
		return domain.ErrCloseServiceUnavailable
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		c.log.Error("unexpected status from financial-close-svc — failing closed", zap.Int("status", resp.StatusCode))
		return domain.ErrCloseServiceUnavailable
	}

	var statusResp periodStatusResp
	if err := json.NewDecoder(resp.Body).Decode(&statusResp); err != nil {
		return domain.ErrCloseServiceUnavailable
	}

	switch statusResp.CloseStatus {
	case "LOCKED", "CLOSED":
		return domain.ErrPeriodLocked
	case "", "OPEN":
		return nil
	default:
		// An unrecognized close_status is exactly the ambiguous case that
		// must fail closed, never default to "assume open."
		c.log.Error("unrecognized close_status from financial-close-svc — failing closed", zap.String("close_status", statusResp.CloseStatus))
		return domain.ErrCloseServiceUnavailable
	}
}
