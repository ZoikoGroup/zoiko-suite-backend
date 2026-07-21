package jurisdiction

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"zoiko.io/workforce-compliance-svc/internal/middleware"
)

type WorkingHourLimitRule struct {
	JurisdictionCode string  `json:"jurisdiction_code"`
	MaxWeeklyHours   float64 `json:"max_weekly_hours"`
	MaxDailyHours    float64 `json:"max_daily_hours"`
}

type Validator interface {
	GetWorkingHourLimit(ctx context.Context, legalEntityID string) (float64, error)
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

func (c *Client) GetWorkingHourLimit(ctx context.Context, legalEntityID string) (float64, error) {
	if c.baseURL == "" {
		return 48.0, nil
	}

	url := fmt.Sprintf("%s/v1/jurisdictions/rules?legal_entity_id=%s&category=WORKING_HOUR_LIMIT", c.baseURL, legalEntityID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 48.0, nil
	}

	tenantID := middleware.GetTenantID(ctx)
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 48.0, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 48.0, nil
	}

	var rule WorkingHourLimitRule
	if err := json.NewDecoder(resp.Body).Decode(&rule); err != nil || rule.MaxWeeklyHours == 0 {
		return 48.0, nil
	}

	return rule.MaxWeeklyHours, nil
}
