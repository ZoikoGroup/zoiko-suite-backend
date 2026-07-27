package employee

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"zoiko.io/payroll-tax-svc/internal/domain"
)

type Client struct {
	baseURL string
	client  *http.Client
}

func NewClient(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &Client{baseURL: baseURL, client: httpClient}
}

type Employee struct {
	EmployeeID    string `json:"employee_id"`
	LegalEntityID string `json:"legal_entity_id"`
	Status        string `json:"status"`
}

func (c *Client) ValidateEmployee(ctx context.Context, tenantID, principalID, employeeID string) (*Employee, error) {
	url := fmt.Sprintf("%s/v1/employees/%s", c.baseURL, employeeID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call employee-master-svc: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, domain.ErrEmployeeNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("employee-master-svc returned status %d", resp.StatusCode)
	}

	var emp Employee
	if err := json.NewDecoder(resp.Body).Decode(&emp); err != nil {
		return nil, fmt.Errorf("decode employee response: %w", err)
	}
	if emp.Status == "TERMINATED" {
		return nil, domain.ErrEmployeeNotFound
	}
	return &emp, nil
}