// Package benefits reads benefit plans and elections from benefits-svc for WFP-03.
// Every failure is an error: an input population is never built on a guess that
// a worker has no elections.
package benefits

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	baseURL string
	client  *http.Client
}

func NewClient(baseURL string, c *http.Client) *Client {
	if c == nil {
		c = &http.Client{Timeout: 5 * time.Second}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), client: c}
}

type Plan struct {
	PlanID                string `json:"plan_id"`
	PlanType              string `json:"plan_type"`
	DeductionTaxTreatment string `json:"deduction_tax_treatment"` // PRE_TAX | POST_TAX
	Currency              string `json:"currency"`
	Status                string `json:"status"`
}

type Election struct {
	ElectionID                 string  `json:"election_id"`
	EmployeeID                 string  `json:"employee_id"`
	PlanID                     string  `json:"plan_id"`
	EmployeeContributionAmount float64 `json:"employee_contribution_amount"`
	EmployerContributionAmount float64 `json:"employer_contribution_amount"`
	EffectiveFrom              string  `json:"effective_from"`
	EffectiveTo                *string `json:"effective_to,omitempty"`
	Status                     string  `json:"status"`
}

func (c *Client) get(ctx context.Context, path, tenantID, principalID string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("call benefits-svc: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("benefits-svc returned status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode benefits-svc response: %w", err)
	}
	return nil
}

// ListPlans returns every plan of a legal entity, any status (an ACTIVE election
// on an INACTIVE plan must still be classifiable).
func (c *Client) ListPlans(ctx context.Context, tenantID, principalID, legalEntityID string) ([]Plan, error) {
	var out []Plan
	err := c.get(ctx, "/v1/benefits/plans?legal_entity_id="+url.QueryEscape(legalEntityID), tenantID, principalID, &out)
	return out, err
}

// ListActiveElections returns the employee's ACTIVE elections.
func (c *Client) ListActiveElections(ctx context.Context, tenantID, principalID, employeeID string) ([]Election, error) {
	var out []Election
	err := c.get(ctx, "/v1/benefits/elections/employee/"+url.PathEscape(employeeID)+"?status=ACTIVE", tenantID, principalID, &out)
	return out, err
}
