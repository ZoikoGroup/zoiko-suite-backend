// Package compensation talks to compensation-svc to resolve what a salary is
// actually made of.
//
// The base salary itself still comes from employment-contracts-svc — that is
// the authoritative figure and it is verified fail-closed before this package
// is ever consulted. compensation-svc supplies the composition: which
// allowances add to the base, which deductions come off it, and which of them
// are taxable. Two sources of truth for the base amount would be one too many.
package compensation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
)

// ErrNoStructure means the employee has no active wage revision, or has one
// that names no compensation structure. It is a normal, expected state — an
// employee simply paid a flat base salary with nothing layered on top — and is
// deliberately distinct from the service being unreachable.
var ErrNoStructure = errors.New("no compensation structure configured for employee")

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

type wageRevision struct {
	RevisionID  string  `json:"revision_id"`
	EmployeeID  string  `json:"employee_id"`
	StructureID *string `json:"structure_id,omitempty"`
	Status      string  `json:"status"`
}

// BreakdownLine is one resolved component of pay.
type BreakdownLine struct {
	ComponentID       string  `json:"component_id"`
	ComponentCode     string  `json:"component_code"`
	ComponentName     string  `json:"component_name"`
	ComponentType     string  `json:"component_type"` // EARNING, DEDUCTION
	IsTaxable         bool    `json:"is_taxable"`
	CalculationMethod string  `json:"calculation_method"`
	CalculationValue  float64 `json:"calculation_value"`
	Amount            float64 `json:"amount"`
	Sequence          int     `json:"sequence"`
}

// Breakdown is a compensation structure resolved against a base amount.
type Breakdown struct {
	StructureID     string          `json:"structure_id"`
	StructureName   string          `json:"structure_name"`
	Currency        string          `json:"currency"`
	BaseAmount      float64         `json:"base_amount"`
	Lines           []BreakdownLine `json:"lines"`
	TotalEarnings   float64         `json:"total_earnings"`
	TotalDeductions float64         `json:"total_deductions"`
	TaxableAmount   float64         `json:"taxable_amount"`
	GrossEarnings   float64         `json:"gross_earnings"`
	NetAmount       float64         `json:"net_amount"`
}

// GetBreakdown resolves the employee's compensation structure against
// baseAmount. It returns ErrNoStructure when the employee has none configured,
// and a wrapped transport error when compensation-svc could not answer — the
// caller must treat those differently.
func (c *Client) GetBreakdown(ctx context.Context, tenantID, principalID, employeeID string, baseAmount float64) (*Breakdown, error) {
	rev, err := c.getActiveWage(ctx, tenantID, principalID, employeeID)
	if err != nil {
		return nil, err
	}
	if rev.StructureID == nil || *rev.StructureID == "" {
		return nil, ErrNoStructure
	}
	return c.getStructureBreakdown(ctx, tenantID, principalID, *rev.StructureID, baseAmount)
}

func (c *Client) getActiveWage(ctx context.Context, tenantID, principalID, employeeID string) (*wageRevision, error) {
	url := fmt.Sprintf("%s/v1/compensation/revisions/employee/%s/active", c.baseURL, employeeID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call compensation-svc: %w", err)
	}
	defer resp.Body.Close()

	// 404 is an answer, not a failure: this employee has no wage revision.
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNoStructure
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("compensation-svc returned status %d for active wage", resp.StatusCode)
	}

	var rev wageRevision
	if err := json.NewDecoder(resp.Body).Decode(&rev); err != nil {
		return nil, fmt.Errorf("decode active wage response: %w", err)
	}
	return &rev, nil
}

func (c *Client) getStructureBreakdown(ctx context.Context, tenantID, principalID, structureID string, baseAmount float64) (*Breakdown, error) {
	url := fmt.Sprintf("%s/v1/compensation/structures/%s/breakdown?base_amount=%s",
		c.baseURL, structureID, strconv.FormatFloat(baseAmount, 'f', -1, 64))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call compensation-svc: %w", err)
	}
	defer resp.Body.Close()

	// The wage revision named a structure that no longer resolves. That is a
	// broken reference rather than an absent one, but payroll can still pay the
	// base salary, so it degrades the same way.
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNoStructure
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("compensation-svc returned status %d for breakdown", resp.StatusCode)
	}

	var b Breakdown
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		return nil, fmt.Errorf("decode breakdown response: %w", err)
	}
	return &b, nil
}
