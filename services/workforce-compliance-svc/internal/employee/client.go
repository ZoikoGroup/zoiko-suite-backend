package employee

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"zoiko.io/workforce-compliance-svc/internal/middleware"
)

type Employee struct {
	EmployeeID    string `json:"employee_id"`
	LegalEntityID string `json:"legal_entity_id"`
	Status        string `json:"status"`
}

type Validator interface {
	ValidateEmployee(ctx context.Context, principalID, legalEntityID, employeeID string) (*Employee, error)
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

func (c *Client) ValidateEmployee(ctx context.Context, principalID, legalEntityID, employeeID string) (*Employee, error) {
	if c.baseURL == "" {
		return &Employee{EmployeeID: employeeID, LegalEntityID: legalEntityID, Status: "ACTIVE"}, nil
	}

	url := fmt.Sprintf("%s/v1/employees/%s", c.baseURL, employeeID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	tenantID := middleware.GetTenantID(ctx)
	req.Header.Set("X-Tenant-Id", tenantID)
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("employee-master-svc unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("employee %s not found in employee-master-svc", employeeID)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("employee-master-svc returned status %d", resp.StatusCode)
	}

	var emp Employee
	if err := json.NewDecoder(resp.Body).Decode(&emp); err != nil {
		return nil, err
	}

	if legalEntityID != "" && emp.LegalEntityID != "" && emp.LegalEntityID != legalEntityID {
		return nil, fmt.Errorf("employee legal_entity_id mismatch: got %s want %s", emp.LegalEntityID, legalEntityID)
	}

	return &emp, nil
}
