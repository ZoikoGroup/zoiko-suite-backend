package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type TaxRuleDTO struct {
	RuleID            string  `json:"rule_id"`
	JurisdictionID    string  `json:"jurisdiction_id"`
	RuleCode          string  `json:"rule_code"`
	Name              string  `json:"name"`
	Category          string  `json:"category"`
	TaxRatePercentage float64 `json:"tax_rate_percentage"`
	StandardDeductions float64 `json:"standard_deductions"`
	Status            string  `json:"status"`
}

type Client struct {
	httpClient *http.Client
	baseURL    string
}

func NewClient(baseURL string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
}

func (c *Client) FetchActiveRule(ctx context.Context, tenantID, jurisdictionID, category string) (*TaxRuleDTO, error) {
	u := fmt.Sprintf("%s/v1/tax-rules?jurisdiction_id=%s&category=%s&status=ACTIVE",
		c.baseURL, url.QueryEscape(jurisdictionID), url.QueryEscape(category))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Fallback default if tax-rules-svc is unavailable
		return &TaxRuleDTO{
			RuleID:            "trule-default-fallback",
			JurisdictionID:    jurisdictionID,
			RuleCode:          "FALLBACK-TAX",
			Name:              "Default Tax Fallback",
			Category:          category,
			TaxRatePercentage: 0.0,
			Status:            "ACTIVE",
		}, nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tax-rules-svc returned %d", resp.StatusCode)
	}

	var payload struct {
		Rules []TaxRuleDTO `json:"rules"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}

	if len(payload.Rules) > 0 {
		return &payload.Rules[0], nil
	}

	return &TaxRuleDTO{
		RuleID:            "trule-default-zero",
		JurisdictionID:    jurisdictionID,
		RuleCode:          "ZERO-TAX",
		Name:              "Zero Tax Rate",
		Category:          category,
		TaxRatePercentage: 0.0,
		Status:            "ACTIVE",
	}, nil
}
