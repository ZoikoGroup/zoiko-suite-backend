package jurisdiction

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"zoiko.io/offboarding-severance-svc/internal/middleware"
)

type NoticeRule struct {
	JurisdictionCode string `json:"jurisdiction_code"`
	MinNoticeDays    int    `json:"min_notice_days"`
	MandatoryPayOut  bool   `json:"mandatory_pay_out"`
}

type Validator interface {
	ValidateNoticePeriod(ctx context.Context, legalEntityID string, requestedNoticeDays int) (int, error)
}

type Client struct {
	baseURL    string
	httpClient *http.Client
}

func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

func (c *Client) ValidateNoticePeriod(ctx context.Context, legalEntityID string, requestedNoticeDays int) (int, error) {
	if c.baseURL == "" {
		if requestedNoticeDays < 14 {
			return 14, nil
		}
		return requestedNoticeDays, nil
	}

	url := fmt.Sprintf("%s/v1/jurisdictions/rules?legal_entity_id=%s&category=NOTICE_PERIOD", c.baseURL, legalEntityID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return requestedNoticeDays, nil
	}

	tenantID := middleware.GetTenantID(ctx)
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return requestedNoticeDays, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return requestedNoticeDays, nil
	}

	var rule NoticeRule
	if err := json.NewDecoder(resp.Body).Decode(&rule); err != nil {
		return requestedNoticeDays, nil
	}

	if requestedNoticeDays < rule.MinNoticeDays {
		return rule.MinNoticeDays, nil
	}
	return requestedNoticeDays, nil
}
