package handler_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	"zoiko.io/payroll-run-svc/internal/compensation"
	"zoiko.io/payroll-run-svc/internal/contract"
	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/employee"
)

// testTaxRate matches the service default, so these tests assert the wiring
// rather than a particular rate.
const testTaxRate = 0.20

// ── compensation client stub ───────────────────────────────────────────────────

type stubCompensationClient struct {
	// breakdowns keyed by employee ID. An employee absent from the map has no
	// structure configured, which is a normal state.
	breakdowns map[string]*compensation.Breakdown
	// err simulates compensation-svc being unreachable.
	err error
}

func (c *stubCompensationClient) GetBreakdown(_ context.Context, _, _, employeeID string, _ float64) (*compensation.Breakdown, error) {
	if c.err != nil {
		return nil, c.err
	}
	bd, ok := c.breakdowns[employeeID]
	if !ok {
		return nil, compensation.ErrNoStructure
	}
	return bd, nil
}

// ── fixtures ───────────────────────────────────────────────────────────────────

func payrollFixtures() (*stubEmployeeClient, *stubContractClient) {
	empC := &stubEmployeeClient{
		employees: []employee.Employee{
			{EmployeeID: "emp-1", EmployeeNumber: "EMP-001", FirstName: "Asha", LastName: "Iyer"},
		},
	}
	ctrC := &stubContractClient{
		contracts: map[string]*contract.ActiveContract{
			"emp-1": {
				ContractID: "ctr-1", EmployeeID: "emp-1",
				BaseSalaryAmount: 10000, Currency: "USD",
				PayFrequency: "MONTHLY", Status: "ACTIVE",
			},
		},
	}
	return empC, ctrC
}

// calcResult is the shape CalculateRun responds with.
type calcResult struct {
	Run         domain.PayrollRun         `json:"run"`
	PaySlips    []domain.PaySlip          `json:"pay_slips"`
	ShadowComps []domain.ShadowComparison `json:"shadow_comparisons"`
}

// initiateAndCalculate runs the two-step flow and returns the calculate
// response, its status code, and the raw body for failure messages.
func initiateAndCalculate(t *testing.T, r chi.Router, correlationID string) (calcResult, int, string) {
	t.Helper()

	rrInit := doReq(r, http.MethodPost, "/v1/payroll/runs", map[string]any{
		"legal_entity_id":  "le-us",
		"pay_period_start": "2026-01-01",
		"pay_period_end":   "2026-01-31",
		"pay_date":         "2026-02-05",
		"correlation_id":   correlationID,
	}, "payroll-admin")
	if rrInit.Code != http.StatusCreated {
		t.Fatalf("initiate: expected 201 got %d: %s", rrInit.Code, rrInit.Body.String())
	}
	var run domain.PayrollRun
	_ = json.NewDecoder(rrInit.Body).Decode(&run)

	rrCalc := doReq(r, http.MethodPost, "/v1/payroll/runs/"+run.RunID+"/calculate", map[string]any{}, "payroll-admin")

	var out calcResult
	body := rrCalc.Body.String()
	_ = json.Unmarshal([]byte(body), &out)
	return out, rrCalc.Code, body
}

// ── tests ──────────────────────────────────────────────────────────────────────

// With a structure configured, gross and deductions come from compensation-svc
// rather than from a hardcoded percentage.
func TestCalculateRun_UsesCompensationBreakdown(t *testing.T) {
	empC, ctrC := payrollFixtures()
	compC := &stubCompensationClient{
		breakdowns: map[string]*compensation.Breakdown{
			"emp-1": {
				StructureID: "str-1", StructureName: "Eng Grade 5", Currency: "USD",
				BaseAmount: 10000,
				Lines: []compensation.BreakdownLine{
					{ComponentID: "c1", ComponentCode: "HRA", ComponentName: "House Rent", ComponentType: "EARNING",
						IsTaxable: true, CalculationMethod: "PERCENT_OF_BASE", CalculationValue: 40, Amount: 4000, Sequence: 1},
					{ComponentID: "c2", ComponentCode: "TRANSPORT", ComponentName: "Transport", ComponentType: "EARNING",
						IsTaxable: false, CalculationMethod: "FIXED", CalculationValue: 500, Amount: 500, Sequence: 2},
					{ComponentID: "c3", ComponentCode: "PF", ComponentName: "Provident Fund", ComponentType: "DEDUCTION",
						IsTaxable: false, CalculationMethod: "PERCENT_OF_BASE", CalculationValue: 12, Amount: 1200, Sequence: 3},
				},
				TotalEarnings: 4500, TotalDeductions: 1200,
				TaxableAmount: 14000, GrossEarnings: 14500, NetAmount: 13300,
			},
		},
	}

	r := newRouterWithComp(newStubStore(), &stubPublisher{}, &stubAuthZ{}, empC, ctrC, compC)
	res, code, body := initiateAndCalculate(t, r, "corr-comp-breakdown")
	if code != http.StatusOK {
		t.Fatalf("calculate: expected 200 got %d: %s", code, body)
	}

	if len(res.PaySlips) != 1 {
		t.Fatalf("expected 1 payslip got %d", len(res.PaySlips))
	}
	slip := res.PaySlips[0]

	// Tax is 20% of the taxable amount (14000), not of gross (14500).
	for _, c := range []struct {
		field string
		got   float64
		want  float64
	}{
		{"gross_pay", slip.GrossPay, 14500},
		{"benefits_deductions", slip.BenefitsDeductions, 1200},
		{"taxable_amount", slip.TaxableAmount, 14000},
		{"tax_withheld", slip.TaxWithheld, 2800},
		{"net_pay", slip.NetPay, 10500},
	} {
		if c.got != c.want {
			t.Errorf("%s: expected %.2f got %.2f", c.field, c.want, c.got)
		}
	}

	if slip.StructureID == nil || *slip.StructureID != "str-1" {
		t.Errorf("expected the slip to record structure str-1, got %v", slip.StructureID)
	}
}

// The lines behind the totals are recorded on the slip.
func TestCalculateRun_RecordsPayslipLines(t *testing.T) {
	empC, ctrC := payrollFixtures()
	compC := &stubCompensationClient{
		breakdowns: map[string]*compensation.Breakdown{
			"emp-1": {
				StructureID: "str-1", Currency: "USD", BaseAmount: 10000,
				Lines: []compensation.BreakdownLine{
					{ComponentID: "c1", ComponentCode: "HRA", ComponentName: "House Rent", ComponentType: "EARNING",
						IsTaxable: true, CalculationMethod: "PERCENT_OF_BASE", CalculationValue: 40, Amount: 4000, Sequence: 1},
					{ComponentID: "c3", ComponentCode: "PF", ComponentName: "Provident Fund", ComponentType: "DEDUCTION",
						IsTaxable: false, CalculationMethod: "FIXED", CalculationValue: 1200, Amount: 1200, Sequence: 2},
				},
				TotalEarnings: 4000, TotalDeductions: 1200,
				TaxableAmount: 14000, GrossEarnings: 14000, NetAmount: 12800,
			},
		},
	}

	r := newRouterWithComp(newStubStore(), &stubPublisher{}, &stubAuthZ{}, empC, ctrC, compC)
	res, code, body := initiateAndCalculate(t, r, "corr-comp-lines")
	if code != http.StatusOK {
		t.Fatalf("calculate: expected 200 got %d: %s", code, body)
	}

	items := res.PaySlips[0].Items
	if len(items) != 2 {
		t.Fatalf("expected 2 payslip lines got %d", len(items))
	}
	if items[0].ComponentCode != "HRA" || items[0].Amount != 4000 {
		t.Errorf("first line should be HRA at 4000, got %s at %.2f", items[0].ComponentCode, items[0].Amount)
	}
	// The derivation is copied onto the line so the slip stays explicable after
	// the structure is edited.
	if items[0].CalculationMethod != "PERCENT_OF_BASE" || items[0].CalculationValue != 40 {
		t.Errorf("expected the line to record how it was derived, got %s %.2f",
			items[0].CalculationMethod, items[0].CalculationValue)
	}
	if items[1].ComponentType != "DEDUCTION" {
		t.Errorf("expected PF to be recorded as a DEDUCTION, got %s", items[1].ComponentType)
	}
}

// No structure configured is a normal state: base salary, and no invented
// component deductions.
func TestCalculateRun_NoStructure_PaysFlatBaseWithNoInventedDeductions(t *testing.T) {
	empC, ctrC := payrollFixtures()

	r := newRouterWithComp(newStubStore(), &stubPublisher{}, &stubAuthZ{}, empC, ctrC, &stubCompensationClient{})
	res, code, body := initiateAndCalculate(t, r, "corr-comp-none")
	if code != http.StatusOK {
		t.Fatalf("calculate: expected 200 got %d: %s", code, body)
	}

	slip := res.PaySlips[0]
	if slip.GrossPay != 10000 {
		t.Errorf("expected gross to be the base salary 10000, got %.2f", slip.GrossPay)
	}
	// This is the behaviour change: it used to be a fabricated gross*0.05.
	if slip.BenefitsDeductions != 0 {
		t.Errorf("expected no component deductions when no structure is configured, got %.2f", slip.BenefitsDeductions)
	}
	if slip.TaxWithheld != 2000 {
		t.Errorf("expected tax 2000 got %.2f", slip.TaxWithheld)
	}
	if slip.NetPay != 8000 {
		t.Errorf("expected net 8000 got %.2f", slip.NetPay)
	}
	if slip.StructureID != nil {
		t.Errorf("expected no structure recorded, got %v", slip.StructureID)
	}
	if len(slip.Items) != 0 {
		t.Errorf("expected no payslip lines, got %d", len(slip.Items))
	}
}

// compensation-svc being unreachable is different from an employee having no
// structure: it blocks the run rather than inventing the composition.
func TestCalculateRun_CompensationUnavailable_FailsClosed(t *testing.T) {
	empC, ctrC := payrollFixtures()
	compC := &stubCompensationClient{err: errors.New("connection refused")}

	store := newStubStore()
	pub := &stubPublisher{}
	r := newRouterWithComp(store, pub, &stubAuthZ{}, empC, ctrC, compC)

	_, code, body := initiateAndCalculate(t, r, "corr-comp-down")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 got %d: %s", code, body)
	}

	// Nothing may be persisted from a calculation that could not be completed.
	for runID := range store.runs {
		slips, _ := store.GetPaySlipsByRun(context.Background(), runID)
		if len(slips) != 0 {
			t.Errorf("expected no payslips persisted when compensation-svc is down, got %d", len(slips))
		}
	}
}

// Net pay is allowed to go negative when deductions exceed pay — that is a real
// state a payroll manager needs to see, not something to clamp away.
func TestCalculateRun_DeductionsExceedingPay_ReportNegativeNet(t *testing.T) {
	empC, ctrC := payrollFixtures()
	compC := &stubCompensationClient{
		breakdowns: map[string]*compensation.Breakdown{
			"emp-1": {
				StructureID: "str-1", Currency: "USD", BaseAmount: 10000,
				Lines: []compensation.BreakdownLine{
					{ComponentID: "c1", ComponentCode: "RECOVERY", ComponentName: "Advance Recovery",
						ComponentType: "DEDUCTION", IsTaxable: false,
						CalculationMethod: "FIXED", CalculationValue: 12000, Amount: 12000, Sequence: 1},
				},
				TotalEarnings: 0, TotalDeductions: 12000,
				TaxableAmount: 10000, GrossEarnings: 10000, NetAmount: -2000,
			},
		},
	}

	r := newRouterWithComp(newStubStore(), &stubPublisher{}, &stubAuthZ{}, empC, ctrC, compC)
	res, code, body := initiateAndCalculate(t, r, "corr-comp-negative")
	if code != http.StatusOK {
		t.Fatalf("calculate: expected 200 got %d: %s", code, body)
	}

	// net = -2000 (from breakdown) - 2000 (tax on 10000 taxable)
	if res.PaySlips[0].NetPay != -4000 {
		t.Errorf("expected net -4000 got %.2f", res.PaySlips[0].NetPay)
	}
}
