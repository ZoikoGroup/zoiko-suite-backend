package clients

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// EmployeeMasterClient calls employee-master-svc's real GET /v1/employees/{id}
// endpoint synchronously to confirm a review record's subject employee
// actually exists (and belongs to the expected legal entity) before the
// record is created.
type EmployeeMasterClient struct {
	baseURL string
	http    *http.Client
}

func NewEmployeeMasterClient(baseURL string) *EmployeeMasterClient {
	return &EmployeeMasterClient{baseURL: baseURL, http: &http.Client{Timeout: 5 * time.Second}}
}

type Employee struct {
	EmployeeID    string `json:"employee_id"`
	LegalEntityID string `json:"legal_entity_id"`
	Status        string `json:"status"`
}

func (c *EmployeeMasterClient) GetEmployee(ctx context.Context, tenantID, principalID, employeeID string) (*Employee, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/employees/"+employeeID, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("employee-master-svc unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("employee-master-svc returned %d for GET /v1/employees/%s", resp.StatusCode, employeeID)
	}

	var emp Employee
	if err := json.NewDecoder(resp.Body).Decode(&emp); err != nil {
		return nil, fmt.Errorf("decode employee-master-svc response: %w", err)
	}
	return &emp, nil
}
