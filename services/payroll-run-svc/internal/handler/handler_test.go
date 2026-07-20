package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/contract"
	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/employee"
	"zoiko.io/payroll-run-svc/internal/handler"
	"zoiko.io/payroll-run-svc/internal/middleware"
)

// ── stubs ─────────────────────────────────────────────────────────────────────

type stubStore struct {
	runs        map[string]*domain.PayrollRun
	slips       map[string][]domain.PaySlip
	shadowComps map[string][]domain.ShadowComparison
}

func newStubStore() *stubStore {
	return &stubStore{
		runs:        make(map[string]*domain.PayrollRun),
		slips:       make(map[string][]domain.PaySlip),
		shadowComps: make(map[string][]domain.ShadowComparison),
	}
}

func (s *stubStore) CreatePayrollRun(_ context.Context, r *domain.PayrollRun) error {
	s.runs[r.RunID] = r
	return nil
}

func (s *stubStore) GetPayrollRun(_ context.Context, id string) (*domain.PayrollRun, error) {
	r, ok := s.runs[id]
	if !ok {
		return nil, domain.ErrPayrollRunNotFound
	}
	return r, nil
}

func (s *stubStore) ListPayrollRuns(_ context.Context, legalEntityID, status string, isShadowRun *bool) ([]domain.PayrollRun, error) {
	var out []domain.PayrollRun
	for _, r := range s.runs {
		if legalEntityID != "" && r.LegalEntityID != legalEntityID {
			continue
		}
		if status != "" && r.Status != status {
			continue
		}
		if isShadowRun != nil && r.IsShadowRun != *isShadowRun {
			continue
		}
		out = append(out, *r)
	}
	return out, nil
}

func (s *stubStore) SaveCalculatedResults(_ context.Context, runID string, totalGross, totalNet, totalTax, totalDeductions float64, slips []domain.PaySlip, shadowComps []domain.ShadowComparison) error {
	r, ok := s.runs[runID]
	if !ok {
		return domain.ErrPayrollRunNotFound
	}
	if r.Status == "COMPLETED" {
		return domain.ErrRunAlreadyFinalized
	}

	r.Status = "CALCULATED"
	r.TotalGrossPay = totalGross
	r.TotalNetPay = totalNet
	r.TotalTaxDeductions = totalTax
	r.TotalOtherDeductions = totalDeductions
	r.EmployeeCount = len(slips)
	r.UpdatedAt = time.Now().UTC()

	s.slips[runID] = slips
	s.shadowComps[runID] = shadowComps
	return nil
}

func (s *stubStore) GetPaySlipsByRun(_ context.Context, runID string) ([]domain.PaySlip, error) {
	return s.slips[runID], nil
}

func (s *stubStore) GetShadowComparisonsByRun(_ context.Context, runID string) ([]domain.ShadowComparison, error) {
	return s.shadowComps[runID], nil
}

func (s *stubStore) FinalizePayrollRun(_ context.Context, runID string) error {
	r, ok := s.runs[runID]
	if !ok {
		return domain.ErrPayrollRunNotFound
	}
	if r.Status == "COMPLETED" {
		return domain.ErrRunAlreadyFinalized
	}
	if r.Status != "CALCULATED" {
		return domain.ErrRunNotCalculated
	}

	now := time.Now().UTC()
	r.Status = "COMPLETED"
	r.UpdatedAt = now
	r.FinalizedAt = &now
	return nil
}

type stubPublisher struct {
	initiated, calculated, completed, blocked int
}

func (p *stubPublisher) PublishRunInitiated(_ context.Context, _ string, _ domain.PayrollRun) {
	p.initiated++
}
func (p *stubPublisher) PublishRunCalculated(_ context.Context, _ string, _ domain.PayrollRun) {
	p.calculated++
}
func (p *stubPublisher) PublishRunCompleted(_ context.Context, _ string, _ domain.PayrollRun) {
	p.completed++
}
func (p *stubPublisher) PublishRunBlocked(_ context.Context, _ string, _ domain.PayrollRun, _ string) {
	p.blocked++
}

type stubAuthZ struct{ err error }

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

type stubEmployeeClient struct {
	employees []employee.Employee
	err       error
}

func (c *stubEmployeeClient) ListActiveEmployeesByEntity(_ context.Context, _, _, _ string) ([]employee.Employee, error) {
	return c.employees, c.err
}

type stubContractClient struct {
	contracts map[string]*contract.ActiveContract
	err       error
}

func (c *stubContractClient) GetActiveContract(_ context.Context, _, _, employeeID string) (*contract.ActiveContract, error) {
	if c.err != nil {
		return nil, c.err
	}
	ctr, ok := c.contracts[employeeID]
	if !ok {
		return nil, domain.ErrPayrollRunNotFound
	}
	return ctr, nil
}

// ── router factory ─────────────────────────────────────────────────────────────

func newRouter(s *stubStore, pub *stubPublisher, authz *stubAuthZ, empC *stubEmployeeClient, ctrC *stubContractClient) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req = req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc"))
			next.ServeHTTP(w, req)
		})
	})
	h := handler.New(s, pub, authz, empC, ctrC, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

func doReq(r chi.Router, method, path string, body any, principalID string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

// ── InitiateRun Tests ──────────────────────────────────────────────────────────

func TestInitiateRun_MissingPrincipal(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeClient{}, &stubContractClient{})
	rr := doReq(r, http.MethodPost, "/v1/payroll/runs", map[string]any{
		"legal_entity_id":  "le-us",
		"pay_period_start": "2024-01-01",
		"pay_period_end":   "2024-01-31",
		"pay_date":         "2024-02-05",
	}, "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 got %d", rr.Code)
	}
}

func TestInitiateRun_HappyPath(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubEmployeeClient{}, &stubContractClient{})

	rr := doReq(r, http.MethodPost, "/v1/payroll/runs", map[string]any{
		"legal_entity_id":  "le-us",
		"run_number":       "PAY-2024-01",
		"pay_period_start": "2024-01-01",
		"pay_period_end":   "2024-01-31",
		"pay_date":         "2024-02-05",
		"is_shadow_run":    true,
	}, "payroll-admin")

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}

	var run domain.PayrollRun
	_ = json.NewDecoder(rr.Body).Decode(&run)

	if run.RunNumber != "PAY-2024-01" {
		t.Errorf("expected PAY-2024-01 got %q", run.RunNumber)
	}
	if run.Status != "INITIATED" {
		t.Errorf("expected status INITIATED got %q", run.Status)
	}
	if !run.IsShadowRun {
		t.Errorf("expected IsShadowRun true")
	}
	if pub.initiated != 1 {
		t.Errorf("expected 1 initiated event got %d", pub.initiated)
	}
}

// ── CalculateRun & Shadow Comparison Tests ─────────────────────────────────────

func TestCalculateRun_StandardAndShadowMode(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}

	empC := &stubEmployeeClient{
		employees: []employee.Employee{
			{EmployeeID: "emp-1", EmployeeNumber: "E-001", FirstName: "Alice", LastName: "Smith", LegalEntityID: "le-us", Status: "ACTIVE"},
		},
	}
	ctrC := &stubContractClient{
		contracts: map[string]*contract.ActiveContract{
			"emp-1": {ContractID: "c-1", EmployeeID: "emp-1", BaseSalaryAmount: 10000.0, Currency: "USD", PayFrequency: "MONTHLY", Status: "ACTIVE"},
		},
	}

	r := newRouter(s, pub, &stubAuthZ{}, empC, ctrC)

	// 1. Initiate shadow run
	rrInit := doReq(r, http.MethodPost, "/v1/payroll/runs", map[string]any{
		"legal_entity_id":  "le-us",
		"pay_period_start": "2024-01-01",
		"pay_period_end":   "2024-01-31",
		"pay_date":         "2024-02-05",
		"is_shadow_run":    true,
	}, "payroll-admin")

	var initRun domain.PayrollRun
	_ = json.NewDecoder(rrInit.Body).Decode(&initRun)

	// 2. Calculate run with legacy shadow inputs
	rrCalc := doReq(r, http.MethodPost, "/v1/payroll/runs/"+initRun.RunID+"/calculate", map[string]any{
		"shadow_baseline_items": []map[string]any{
			{
				"employee_id":         "emp-1",
				"legacy_gross_pay":    10000.0,
				"legacy_net_pay":      7500.0, // Zoiko net = 10000 - 2000 (tax) - 500 (benefits) = 7500.0 (equivalent!)
				"legacy_tax_withheld": 2000.0,
			},
		},
	}, "payroll-admin")

	if rrCalc.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rrCalc.Code, rrCalc.Body.String())
	}

	var calcRes struct {
		Run              domain.PayrollRun         `json:"run"`
		PaySlips         []domain.PaySlip          `json:"pay_slips"`
		ShadowComps      []domain.ShadowComparison `json:"shadow_comparisons"`
	}
	_ = json.NewDecoder(rrCalc.Body).Decode(&calcRes)

	if calcRes.Run.Status != "CALCULATED" {
		t.Errorf("expected CALCULATED got %q", calcRes.Run.Status)
	}
	if len(calcRes.PaySlips) != 1 {
		t.Fatalf("expected 1 payslip got %d", len(calcRes.PaySlips))
	}
	if calcRes.PaySlips[0].GrossPay != 10000.0 {
		t.Errorf("expected gross 10000 got %f", calcRes.PaySlips[0].GrossPay)
	}
	if calcRes.PaySlips[0].NetPay != 7500.0 {
		t.Errorf("expected net 7500 got %f", calcRes.PaySlips[0].NetPay)
	}

	if len(calcRes.ShadowComps) != 1 {
		t.Fatalf("expected 1 shadow comp got %d", len(calcRes.ShadowComps))
	}
	if !calcRes.ShadowComps[0].IsEquivalent {
		t.Errorf("expected shadow comparison equivalence true")
	}
	if pub.calculated != 1 {
		t.Errorf("expected 1 calculated event got %d", pub.calculated)
	}
}

// ── FinalizeRun & Immutability Tests ──────────────────────────────────────────

func TestFinalizeRun_LocksRunAndEnforcesImmutability(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubEmployeeClient{}, &stubContractClient{})

	// 1. Initiate run
	rrInit := doReq(r, http.MethodPost, "/v1/payroll/runs", map[string]any{
		"legal_entity_id":  "le-us",
		"pay_period_start": "2024-01-01",
		"pay_period_end":   "2024-01-31",
		"pay_date":         "2024-02-05",
	}, "payroll-admin")
	var run domain.PayrollRun
	_ = json.NewDecoder(rrInit.Body).Decode(&run)

	// 2. Finalize uncalculated run fails
	rrFinFail := doReq(r, http.MethodPost, "/v1/payroll/runs/"+run.RunID+"/finalize", nil, "payroll-admin")
	if rrFinFail.Code != http.StatusBadRequest {
		t.Errorf("expected 400 when finalizing uncalculated run got %d", rrFinFail.Code)
	}

	// 3. Calculate run
	_ = doReq(r, http.MethodPost, "/v1/payroll/runs/"+run.RunID+"/calculate", map[string]any{
		"shadow_baseline_items": []map[string]any{
			{"employee_id": "emp-99", "legacy_gross_pay": 5000.0, "legacy_net_pay": 3750.0, "legacy_tax_withheld": 1000.0},
		},
	}, "payroll-admin")

	// 4. Finalize calculated run
	rrFin := doReq(r, http.MethodPost, "/v1/payroll/runs/"+run.RunID+"/finalize", nil, "payroll-admin")
	if rrFin.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rrFin.Code, rrFin.Body.String())
	}

	var finalizedRun domain.PayrollRun
	_ = json.NewDecoder(rrFin.Body).Decode(&finalizedRun)
	if finalizedRun.Status != "COMPLETED" {
		t.Errorf("expected status COMPLETED got %q", finalizedRun.Status)
	}

	// 5. Attempting to recalculate finalized run must fail (409 Conflict)
	rrReCalc := doReq(r, http.MethodPost, "/v1/payroll/runs/"+run.RunID+"/calculate", nil, "payroll-admin")
	if rrReCalc.Code != http.StatusConflict {
		t.Errorf("expected 409 Conflict on recalculating finalized run got %d", rrReCalc.Code)
	}

	if pub.completed != 1 {
		t.Errorf("expected 1 completed event got %d", pub.completed)
	}
}