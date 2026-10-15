package contract

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"zoiko.io/payroll-run-svc/internal/domain"
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

type ActiveContract struct {
	ContractID        string   `json:"contract_id"`
	EmployeeID        string   `json:"employee_id"`
	BaseSalaryAmount  float64  `json:"base_salary_amount"`
	Currency          string   `json:"currency"`
	PayFrequency      string   `json:"pay_frequency"`
	Status            string   `json:"status"`
	CTC               *float64 `json:"ctc,omitempty"`
	BasicSalary       *float64 `json:"basic_salary,omitempty"`
	HRA               *float64 `json:"hra,omitempty"`
	SpecialAllowance  *float64 `json:"special_allowance,omitempty"`
	ConveyanceAllowance *float64 `json:"conveyance_allowance,omitempty"`
	MedicalAllowance  *float64 `json:"medical_allowance,omitempty"`
	LTA               *float64 `json:"lta,omitempty"`
	ProbationPeriodDays *int   `json:"probation_period_days,omitempty"`
	NoticePeriodDays    *int   `json:"notice_period_days,omitempty"`
	WorkingHoursPerWeek *float64 `json:"working_hours_per_week,omitempty"`
	ShiftType           *string `json:"shift_type,omitempty"`
}

func (c *Client) GetActiveContract(ctx context.Context, tenantID, principalID, employeeID string) (*ActiveContract, error) {
	url := fmt.Sprintf("%s/v1/contracts/employee/%s/active", c.baseURL, employeeID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call employment-contracts-svc: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, domain.ErrPayrollRunNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("employment-contracts-svc returned status %d", resp.StatusCode)
	}

	var ctr ActiveContract
	if err := json.NewDecoder(resp.Body).Decode(&ctr); err != nil {
		return nil, fmt.Errorf("decode contract response: %w", err)
	}
	return &ctr, nil
}
