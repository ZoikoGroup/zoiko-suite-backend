// Package payrolltax is the read-through client to payroll-tax-svc's POST /v1/payroll-tax/calculate.
//
// payroll-run-svc never computes statutory tax itself. When the tax engine mode is on, the calculation
// asks payroll-tax-svc for each employee's tax and records exactly what came back: the component
// breakdown, the calculation id, the engine type and the rule version. Anything other than a clean answer
// (unreachable, refused, an internally inconsistent total) blocks the run; it is never estimated.
package payrolltax

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"zoiko.io/payroll-run-svc/internal/domain"
)

type Client struct {
	baseURL string
	http    *http.Client
}

func NewClient(baseURL string, c *http.Client) *Client {
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: c}
}

// Caller is the authenticated identity forwarded to the tax service.
type Caller struct {
	TenantID    string
	PrincipalID string
}

type Request struct {
	PayrollRunID          string  `json:"payroll_run_id"`
	EmployeeID            string  `json:"employee_id"`
	JurisdictionCode      string  `json:"jurisdiction_code"`
	GrossTaxableAmount    float64 `json:"gross_taxable_amount"`
	PreTaxDeductionAmount float64 `json:"pre_tax_deduction_amount"`
	Currency              string  `json:"currency"`
	CorrelationID         string  `json:"correlation_id"`
}

type Component struct {
	TaxName   string  `json:"tax_name"`
	TaxType   string  `json:"tax_type"`
	RatePct   float64 `json:"rate_pct"`
	TaxAmount float64 `json:"tax_amount"`
}

type Result struct {
	CalculationID    string      `json:"calculation_id"`
	JurisdictionCode string      `json:"jurisdiction_code"`
	TotalTaxAmount   float64     `json:"total_tax_amount"`
	TaxBreakdown     []Component `json:"tax_breakdown"`
	EngineType       string      `json:"engine_type"`
	RuleVersionUsed  string      `json:"rule_version_used"`
}

// Calculate asks the tax engine. The returned result is internally consistent (the total equals the sum of
// its components to the cent) or it is an error.
func (c *Client) Calculate(ctx context.Context, caller Caller, in Request) (*Result, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/payroll-tax/calculate", bytes.NewReader(body))
	if err != nil {
		return nil, domain.ErrTaxEngineUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", caller.TenantID)
	req.Header.Set("X-Principal-Id", caller.PrincipalID)
	req.Header.Set("X-Correlation-ID", in.CorrelationID)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrTaxEngineUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("%w: status %d", domain.ErrTaxEngineUnavailable, resp.StatusCode)
	}
	var out Result
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("%w: undecodable answer", domain.ErrTaxEngineUnavailable)
	}
	if strings.TrimSpace(out.CalculationID) == "" || strings.TrimSpace(out.RuleVersionUsed) == "" || strings.TrimSpace(out.EngineType) == "" {
		return nil, fmt.Errorf("%w: answer lacks calculation id, engine type or rule version", domain.ErrTaxEngineUnavailable)
	}
	sum := 0.0
	for _, comp := range out.TaxBreakdown {
		if comp.TaxAmount < 0 || math.IsNaN(comp.TaxAmount) || math.IsInf(comp.TaxAmount, 0) {
			return nil, fmt.Errorf("%w: component %q has an invalid amount", domain.ErrTaxEngineUnavailable, comp.TaxName)
		}
		sum += comp.TaxAmount
	}
	if math.Abs(sum-out.TotalTaxAmount) > 0.005 {
		return nil, fmt.Errorf("%w: total %.2f, components %.2f", domain.ErrTaxTotalMismatch, out.TotalTaxAmount, sum)
	}
	return &out, nil
}
