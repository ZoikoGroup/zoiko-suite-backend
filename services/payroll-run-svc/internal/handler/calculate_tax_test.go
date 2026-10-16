package handler_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/compensation"
	"zoiko.io/payroll-run-svc/internal/contract"
	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/employee"
	"zoiko.io/payroll-run-svc/internal/handler"
	"zoiko.io/payroll-run-svc/internal/middleware"
	"zoiko.io/payroll-run-svc/internal/payrolltax"
	"zoiko.io/payroll-run-svc/internal/workerctx"
)

// ── stubs ────────────────────────────────────────────────────────────────────

type stubTaxEngine struct {
	mu    sync.Mutex
	calls []payrolltax.Request
	who   []payrolltax.Caller
	err   error
	// answer builds the engine's reply; nil uses a three-component default at 10%/5%/1%.
	answer func(in payrolltax.Request) *payrolltax.Result
}

func (s *stubTaxEngine) Calculate(_ context.Context, caller payrolltax.Caller, in payrolltax.Request) (*payrolltax.Result, error) {
	s.mu.Lock()
	s.calls = append(s.calls, in)
	s.who = append(s.who, caller)
	s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	if s.answer != nil {
		return s.answer(in), nil
	}
	inc, soc, med := in.GrossTaxableAmount*0.10, in.GrossTaxableAmount*0.05, in.GrossTaxableAmount*0.01
	return &payrolltax.Result{
		CalculationID: "calc-" + in.EmployeeID, JurisdictionCode: in.JurisdictionCode, TotalTaxAmount: inc + soc + med,
		EngineType: "STANDARD_ENGINE", RuleVersionUsed: "2026.1",
		TaxBreakdown: []payrolltax.Component{
			{TaxName: "Income Tax", TaxType: "STATE_FEDERAL", RatePct: 10, TaxAmount: inc},
			{TaxName: "Social Insurance", TaxType: "SOCIAL_SECURITY", RatePct: 5, TaxAmount: soc},
			{TaxName: "Medical Insurance", TaxType: "HEALTH_STATUTORY", RatePct: 1, TaxAmount: med},
		},
	}, nil
}

type stubJurisdictions struct {
	byWorker map[string]domain.WorkerContext
	err      error
	callers  []workerctx.Caller
	asOf     []string
	mu       sync.Mutex
}

func (s *stubJurisdictions) Validate(_ context.Context, c workerctx.Caller, workerID, _, asOf string) (domain.WorkerContext, error) {
	s.mu.Lock()
	s.callers = append(s.callers, c)
	s.asOf = append(s.asOf, asOf)
	s.mu.Unlock()
	if s.err != nil {
		return domain.WorkerContext{}, s.err
	}
	if wc, ok := s.byWorker[workerID]; ok {
		return wc, nil
	}
	return domain.WorkerContext{Valid: true, PayCurrencies: []string{"USD"}, PayrollJurisdictions: []string{"GB"}}, nil
}

func taxRouter(s *stubStore, empC *stubEmployeeClient, ctrC *stubContractClient, compC *stubCompensationClient, tax handler.TaxEngine, j handler.WorkerJurisdictions) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc")))
		})
	})
	s.pub = &stubPublisher{}
	h := handler.New(s, &stubAuthZ{}, empC, ctrC, compC, testTaxRate, zap.NewNop())
	if tax != nil {
		h.WithTaxEngine(tax, j)
	}
	handler.RegisterRoutes(r, h)
	return r
}

func twoEmployees() (*stubEmployeeClient, *stubContractClient) {
	empC := &stubEmployeeClient{employees: []employee.Employee{
		{EmployeeID: "emp-1", EmployeeNumber: "E1", FirstName: "Asha", LastName: "Iyer"},
		{EmployeeID: "emp-2", EmployeeNumber: "E2", FirstName: "Ben", LastName: "Ode"},
	}}
	ctrC := &stubContractClient{contracts: map[string]*contract.ActiveContract{
		"emp-1": {ContractID: "c1", EmployeeID: "emp-1", BaseSalaryAmount: 3000, Currency: "GBP", PayFrequency: "MONTHLY", Status: "ACTIVE"},
		"emp-2": {ContractID: "c2", EmployeeID: "emp-2", BaseSalaryAmount: 2000, Currency: "GBP", PayFrequency: "MONTHLY", Status: "ACTIVE"},
	}}
	return empC, ctrC
}

func runStatus(s *stubStore) domain.PayrollRunStatus {
	for _, r := range s.runs {
		return r.Status
	}
	return ""
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestCalculateRun_TaxEngineModeUsesTheEnginesBreakdownAndRecordsIt(t *testing.T) {
	empC, ctrC := twoEmployees()
	tax, jur := &stubTaxEngine{}, &stubJurisdictions{}
	s := newStubStore()
	r := taxRouter(s, empC, ctrC, &stubCompensationClient{}, tax, jur)
	res, code, body := initiateAndCalculate(t, r, "corr-tax-1")
	require.Equal(t, http.StatusOK, code, body)
	require.Len(t, res.PaySlips, 2)

	for _, slip := range res.PaySlips {
		taxable := slip.TaxableAmount
		assert.InDelta(t, taxable*0.16, slip.TaxWithheld, 0.011, "tax is the engine's (10%+5%+1%), not the placeholder 20%%")
		assert.InDelta(t, slip.GrossPay-slip.TaxWithheld, slip.NetPay, 0.011, "net moves with the tax")
		require.Len(t, slip.TaxLines, 3)
		sum := 0.0
		for i, l := range slip.TaxLines {
			assert.Equal(t, i+1, l.Sequence)
			assert.Equal(t, "calc-"+slip.EmployeeID, l.CalculationID)
			assert.Equal(t, "2026.1", l.RuleVersion)
			assert.Equal(t, "STANDARD_ENGINE", l.EngineType)
			assert.Equal(t, "GB", l.JurisdictionCode, "the jurisdiction comes from the worker financial profile")
			assert.Equal(t, slip.SlipID, l.SlipID)
			assert.Equal(t, "tenant-abc", l.TenantID)
			sum += l.Amount
		}
		assert.InDelta(t, slip.TaxWithheld, sum, 0.011, "the lines are exactly what was withheld")
	}
	assert.InDelta(t, res.PaySlips[0].TaxWithheld+res.PaySlips[1].TaxWithheld, res.Run.TotalTaxDeductions, 0.011)

	require.Len(t, tax.calls, 2)
	for i, c := range tax.calls {
		assert.Equal(t, "GB", c.JurisdictionCode)
		assert.Equal(t, "GBP", c.Currency)
		assert.Equal(t, res.Run.RunID, c.PayrollRunID)
		assert.Equal(t, domain.TaxCorrelationID(res.Run.RunID, c.EmployeeID, c.GrossTaxableAmount, "GB", "GBP"), c.CorrelationID, "an idempotency key the engine can replay")
		assert.Equal(t, "tenant-abc", tax.who[i].TenantID)
		assert.Equal(t, "payroll-admin", tax.who[i].PrincipalID, "the caller's identity is forwarded")
	}
	for _, a := range jur.asOf {
		assert.Equal(t, "2026-02-05", a, "the jurisdiction is the one in force on the pay date")
	}
}

func TestCalculateRun_WithoutTheEngineTheLegacyRateStillApplies(t *testing.T) {
	empC, ctrC := twoEmployees()
	r := taxRouter(newStubStore(), empC, ctrC, &stubCompensationClient{}, nil, nil)
	res, code, body := initiateAndCalculate(t, r, "corr-tax-legacy")
	require.Equal(t, http.StatusOK, code, body)
	for _, slip := range res.PaySlips {
		assert.Empty(t, slip.TaxLines)
		assert.InDelta(t, slip.TaxableAmount*testTaxRate, slip.TaxWithheld, 0.011)
	}
}

func TestCalculateRun_AnythingTheTaxEngineCannotAnswerBlocksTheWholeRun(t *testing.T) {
	cases := map[string]struct {
		tax    *stubTaxEngine
		jur    *stubJurisdictions
		status int
		code   string
	}{
		"engine unreachable":   {&stubTaxEngine{err: domain.ErrTaxEngineUnavailable}, &stubJurisdictions{}, http.StatusServiceUnavailable, "tax_engine_unavailable"},
		"engine refuses":       {&stubTaxEngine{err: errors.New("status 403")}, &stubJurisdictions{}, http.StatusServiceUnavailable, "tax_engine_unavailable"},
		"inconsistent total":   {&stubTaxEngine{err: domain.ErrTaxTotalMismatch}, &stubJurisdictions{}, http.StatusUnprocessableEntity, "tax_total_mismatch"},
		"profile service down": {&stubTaxEngine{}, &stubJurisdictions{err: domain.ErrSourceUnavailable}, http.StatusServiceUnavailable, "tax_jurisdiction_lookup_failed"},
		"two jurisdictions": {&stubTaxEngine{}, &stubJurisdictions{byWorker: map[string]domain.WorkerContext{
			"emp-2": {Valid: true, PayrollJurisdictions: []string{"GB", "US"}}}}, http.StatusUnprocessableEntity, "tax_jurisdiction_unresolved"},
		"no jurisdiction": {&stubTaxEngine{}, &stubJurisdictions{byWorker: map[string]domain.WorkerContext{
			"emp-2": {Valid: true}}}, http.StatusUnprocessableEntity, "tax_jurisdiction_unresolved"},
		"no active profile": {&stubTaxEngine{}, &stubJurisdictions{byWorker: map[string]domain.WorkerContext{
			"emp-2": {Valid: false, Reason: "NO_ACTIVE_PROFILE"}}}, http.StatusUnprocessableEntity, "tax_jurisdiction_unresolved"},
		"unknown tax type": {&stubTaxEngine{answer: func(in payrolltax.Request) *payrolltax.Result {
			return &payrolltax.Result{CalculationID: "c", EngineType: "E", RuleVersionUsed: "1", TotalTaxAmount: 10,
				TaxBreakdown: []payrolltax.Component{{TaxName: "Mystery", TaxType: "UNHEARD_OF", TaxAmount: 10}}}
		}}, &stubJurisdictions{}, http.StatusUnprocessableEntity, "tax_type_unmapped"},
	}
	for name, tc := range cases {
		empC, ctrC := twoEmployees()
		s := newStubStore()
		r := taxRouter(s, empC, ctrC, &stubCompensationClient{}, tc.tax, tc.jur)
		rrInit := doReq(r, http.MethodPost, "/v1/payroll/runs", map[string]any{
			"legal_entity_id": "le-us", "pay_period_start": "2026-01-01", "pay_period_end": "2026-01-31", "pay_date": "2026-02-05", "correlation_id": "corr-" + name,
		}, "payroll-admin")
		require.Equal(t, http.StatusCreated, rrInit.Code, name)
		var run domain.PayrollRun
		require.NoError(t, json.NewDecoder(rrInit.Body).Decode(&run))
		rr := doReq(r, http.MethodPost, "/v1/payroll/runs/"+run.RunID+"/calculate", map[string]any{}, "payroll-admin")
		assert.Equal(t, tc.status, rr.Code, "%s: %s", name, rr.Body.String())
		var body map[string]any
		_ = json.Unmarshal(rr.Body.Bytes(), &body)
		assert.Equal(t, tc.code, body["error_code"], name)
		assert.NotEqual(t, domain.PayrollRunStatusCalculated, runStatus(s), "%s: a failed tax lookup stores nothing: the run is not CALCULATED", name)
		assert.Empty(t, s.slips[run.RunID], "%s: no payslip is stored on a tax failure", name)
	}
}

func TestCalculateRun_ABlockedTaxFailureNamesTheEmployeesAffected(t *testing.T) {
	empC, ctrC := twoEmployees()
	jur := &stubJurisdictions{byWorker: map[string]domain.WorkerContext{"emp-2": {Valid: true}}}
	s := newStubStore()
	r := taxRouter(s, empC, ctrC, &stubCompensationClient{}, &stubTaxEngine{}, jur)
	_, code, body := initiateAndCalculate(t, r, "corr-tax-named")
	assert.Equal(t, http.StatusUnprocessableEntity, code, body)
	var out struct {
		Employees []string `json:"employees_affected"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	assert.Equal(t, []string{"emp-2"}, out.Employees)
}

func TestCalculateRun_NoTaxablePayMeansNoEngineCallAndNoTaxLines(t *testing.T) {
	empC, ctrC := payrollFixtures()
	compC := &stubCompensationClient{breakdowns: map[string]*compensation.Breakdown{
		"emp-1": {StructureID: "s", Currency: "USD", BaseAmount: 10000, TotalEarnings: 0, TotalDeductions: 0, TaxableAmount: 0, GrossEarnings: 10000, NetAmount: 10000},
	}}
	tax := &stubTaxEngine{}
	r := taxRouter(newStubStore(), empC, ctrC, compC, tax, &stubJurisdictions{})
	res, code, body := initiateAndCalculate(t, r, "corr-tax-zero")
	require.Equal(t, http.StatusOK, code, body)
	require.Len(t, res.PaySlips, 1)
	assert.Empty(t, tax.calls, "the engine is not asked about nothing")
	assert.Empty(t, res.PaySlips[0].TaxLines)
	assert.Equal(t, 0.0, res.PaySlips[0].TaxWithheld)
	assert.Equal(t, 10000.0, res.PaySlips[0].NetPay)
}

func TestCalculateRun_RecalculatingTheSameInputsReusesTheEnginesCalculation(t *testing.T) {
	empC, ctrC := twoEmployees()
	tax := &stubTaxEngine{}
	s := newStubStore()
	r := taxRouter(s, empC, ctrC, &stubCompensationClient{}, tax, &stubJurisdictions{})
	rrInit := doReq(r, http.MethodPost, "/v1/payroll/runs", map[string]any{
		"legal_entity_id": "le-us", "pay_period_start": "2026-01-01", "pay_period_end": "2026-01-31", "pay_date": "2026-02-05", "correlation_id": "corr-recalc",
	}, "payroll-admin")
	var run domain.PayrollRun
	require.NoError(t, json.NewDecoder(rrInit.Body).Decode(&run))
	for i := 0; i < 2; i++ {
		rr := doReq(r, http.MethodPost, "/v1/payroll/runs/"+run.RunID+"/calculate", map[string]any{}, "payroll-admin")
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	}
	require.Len(t, tax.calls, 4)
	byEmp := map[string][]string{}
	for _, c := range tax.calls {
		byEmp[c.EmployeeID] = append(byEmp[c.EmployeeID], c.CorrelationID)
	}
	for emp, ids := range byEmp {
		require.Len(t, ids, 2, emp)
		assert.Equal(t, ids[0], ids[1], "%s: the same inputs send the same idempotency key", emp)
	}
}
