package handler_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/contract"
	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/employee"
	"zoiko.io/payroll-run-svc/internal/handler"
	"zoiko.io/payroll-run-svc/internal/middleware"
)

// actionAuthZ allows everything except the named actions.
type actionAuthZ struct{ deny map[string]error }

func (a *actionAuthZ) CheckAllowed(_ context.Context, _, _, action string) error {
	return a.deny[action]
}

func batchRouter(s *stubStore, empC *stubEmployeeClient, ctrC *stubContractClient, az handler.AuthZClient, tax handler.TaxEngine, j handler.WorkerJurisdictions) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc")))
		})
	})
	s.pub = &stubPublisher{}
	h := handler.New(s, az, empC, ctrC, &stubCompensationClient{}, testTaxRate, zap.NewNop())
	if tax != nil {
		h.WithTaxEngine(tax, j)
	}
	handler.RegisterRoutes(r, h)
	return r
}

func newRun(t *testing.T, r chi.Router, corr string, shadow bool) domain.PayrollRun {
	t.Helper()
	rr := doReq(r, http.MethodPost, "/v1/payroll/runs", map[string]any{
		"legal_entity_id": "le-us", "pay_period_start": "2026-01-01", "pay_period_end": "2026-01-31", "pay_date": "2026-02-05",
		"is_shadow_run": shadow, "correlation_id": corr,
	}, "payroll-admin")
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	var run domain.PayrollRun
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&run))
	return run
}

func calc(r chi.Router, runID string, body map[string]any) (int, map[string]any) {
	rr := doReq(r, http.MethodPost, "/v1/payroll/runs/"+runID+"/calculate", body, "payroll-admin")
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

func bline(worker, kind, category, treatment, code, amount, currency string) domain.InputLine {
	tt, pc := treatment, code
	return domain.InputLine{LineID: uuid.NewString(), WorkerID: worker, SourceKind: kind, InputCode: code, PayrollCode: &pc, Category: category,
		TaxTreatment: &tt, Amount: amount, Currency: currency, State: domain.InputLineAccepted}
}

func frozenBatch(s *stubStore, entity string, lines []domain.InputLine) *domain.InputBatch {
	b := &domain.InputBatch{BatchID: uuid.NewString(), LegalEntityID: entity, PayrollPeriodID: "2026-01", PeriodStart: "2026-01-01", PeriodEnd: "2026-01-31",
		State: domain.InputBatchFrozen, Version: 4}
	if s.batches == nil {
		s.batches, s.batchLines = map[string]*domain.InputBatch{}, map[string][]domain.InputLine{}
	}
	s.batches[b.BatchID] = b
	s.batchLines[b.BatchID] = lines
	return b
}

// both workers: base salary + an allowance + a pre-tax pension contribution. Their contracts say something else, so a
// result that follows the batch cannot have come from the contract.
func standardLines() []domain.InputLine {
	var out []domain.InputLine
	for _, w := range []string{"emp-1", "emp-2"} {
		out = append(out,
			bline(w, domain.SourceBaseSalary, domain.CategoryEarning, domain.TaxTaxable, "BASE", "4000.0000", "GBP"),
			bline(w, domain.SourceCompComponent, domain.CategoryEarning, domain.TaxTaxable, "HRA", "500.0000", "GBP"),
			bline(w, domain.SourceBenefitEmployee, domain.CategoryBenefit, domain.TaxPreTax, "PENSION", "200.0000", "GBP"),
		)
	}
	return out
}

func TestCalculateRun_AFrozenBatchDrivesTheInputsAndIsConsumedOnce(t *testing.T) {
	empC, ctrC := twoEmployees() // contracts say 3000 and 2000: the batch must win
	s := newStubStore()
	r := batchRouter(s, empC, ctrC, &stubAuthZ{}, nil, nil)
	run := newRun(t, r, "corr-batch-1", false)
	b := frozenBatch(s, "le-us", standardLines())

	code, out := calc(r, run.RunID, map[string]any{"input_batch_id": b.BatchID})
	require.Equal(t, http.StatusOK, code, out)
	assert.Equal(t, b.BatchID, out["input_batch_id"])
	assert.Equal(t, []string{b.BatchID}, s.consumed, "the batch is tied to the run")
	assert.Equal(t, domain.InputBatchConsumed, s.batches[b.BatchID].State)

	slips := s.slips[run.RunID]
	require.Len(t, slips, 2)
	for _, slip := range slips {
		assert.Equal(t, 4500.0, slip.GrossPay, "base 4000 + HRA 500 from the batch, not the contract")
		assert.Equal(t, 200.0, slip.BenefitsDeductions)
		assert.Equal(t, 4300.0, slip.TaxableAmount, "taxable 4500 less the pre-tax pension")
		assert.InDelta(t, 4300*testTaxRate, slip.TaxWithheld, 0.011)
		assert.InDelta(t, 4500-200-slip.TaxWithheld, slip.NetPay, 0.011)
		assert.Equal(t, "GBP", slip.Currency)
		codes := map[string]string{}
		for _, it := range slip.Items {
			codes[it.ComponentCode] = it.ComponentType
		}
		assert.Equal(t, map[string]string{"HRA": "EARNING", "PENSION": "DEDUCTION"}, codes, "base salary is not a payslip line")
	}

	// A recalculation keeps the run's frozen inputs without naming them again, and consumes nothing more.
	code, out = calc(r, run.RunID, map[string]any{})
	require.Equal(t, http.StatusOK, code, out)
	assert.Equal(t, b.BatchID, out["input_batch_id"], "a run that consumed a batch cannot silently drop it")
	assert.Len(t, s.consumed, 1)
}

func TestCalculateRun_TheFrozenBatchAndTheTaxEngineWorkTogether(t *testing.T) {
	empC, ctrC := twoEmployees()
	s := newStubStore()
	tax := &stubTaxEngine{}
	r := batchRouter(s, empC, ctrC, &stubAuthZ{}, tax, &stubJurisdictions{})
	run := newRun(t, r, "corr-batch-tax", false)
	b := frozenBatch(s, "le-us", standardLines())
	code, out := calc(r, run.RunID, map[string]any{"input_batch_id": b.BatchID})
	require.Equal(t, http.StatusOK, code, out)
	require.Len(t, tax.calls, 2)
	for _, c := range tax.calls {
		assert.Equal(t, 4300.0, c.GrossTaxableAmount, "the engine is asked about the batch's taxable pay")
		assert.Equal(t, "GBP", c.Currency)
	}
	for _, slip := range s.slips[run.RunID] {
		assert.Len(t, slip.TaxLines, 3)
		assert.InDelta(t, 4300*0.16, slip.TaxWithheld, 0.011)
	}
}

func TestCalculateRun_ABatchThatDoesNotFitTheRunIsRefusedAndNothingIsStored(t *testing.T) {
	empC, ctrC := twoEmployees()
	notFrozen := func(b *domain.InputBatch) { b.State = domain.InputBatchReady }
	consumedByOther := func(b *domain.InputBatch) { b.State = domain.InputBatchConsumed }
	cases := map[string]struct {
		mutate func(*domain.InputBatch)
		lines  []domain.InputLine
		status int
		code   string
		naming func(*domain.InputBatch) any
	}{
		"other entity":      {func(b *domain.InputBatch) { b.LegalEntityID = "le-uk" }, standardLines(), 422, "batch_entity_mismatch", nil},
		"other period":      {func(b *domain.InputBatch) { b.PeriodEnd = "2026-02-28" }, standardLines(), 422, "batch_period_mismatch", nil},
		"not frozen":        {notFrozen, standardLines(), 409, "batch_not_frozen", nil},
		"consumed by other": {consumedByOther, standardLines(), 409, "batch_not_frozen", nil},
		"empty":             {nil, nil, 422, "batch_empty", nil},
		"not a uuid":        {nil, standardLines(), 400, "validation_failed", func(*domain.InputBatch) any { return "nope" }},
		"unknown batch":     {nil, standardLines(), 404, "input_batch_not_found", func(*domain.InputBatch) any { return uuid.NewString() }},
		"missing a worker":  {nil, standardLines()[:3], 422, "batch_population_mismatch", nil},
		"extra worker": {nil, append(standardLines(), bline("emp-9", domain.SourceBaseSalary, domain.CategoryEarning, domain.TaxTaxable, "BASE", "1.0000", "GBP")),
			422, "batch_population_mismatch", nil},
		"mixed currency": {nil, append(standardLines(), bline("emp-1", domain.SourceCompComponent, domain.CategoryEarning, domain.TaxTaxable, "X", "1.0000", "USD")),
			422, "batch_not_treatable", nil},
		"reimbursement": {nil, append(standardLines(), bline("emp-1", domain.SourceCompBonus, domain.CategoryReimbursement, domain.TaxNonTaxable, "X", "1.0000", "GBP")),
			422, "batch_not_treatable", nil},
	}
	for name, tc := range cases {
		s := newStubStore()
		r := batchRouter(s, empC, ctrC, &stubAuthZ{}, nil, nil)
		run := newRun(t, r, "corr-"+name, false)
		b := frozenBatch(s, "le-us", tc.lines)
		if tc.mutate != nil {
			tc.mutate(b)
		}
		var named any = b.BatchID
		if tc.naming != nil {
			named = tc.naming(b)
		}
		code, out := calc(r, run.RunID, map[string]any{"input_batch_id": named})
		assert.Equal(t, tc.status, code, "%s: %v", name, out)
		assert.Equal(t, tc.code, out["error_code"], name)
		assert.Empty(t, s.slips[run.RunID], "%s: nothing stored", name)
		assert.Empty(t, s.consumed, "%s: nothing consumed", name)
		assert.NotEqual(t, domain.PayrollRunStatusCalculated, runStatus(s), name)
	}
}

func TestCalculateRun_NamingABatchNeedsThePermissionToConsumeIt(t *testing.T) {
	empC, ctrC := twoEmployees()
	s := newStubStore()
	az := &actionAuthZ{deny: map[string]error{"WFP_INPUT_CONSUME": domain.ErrAuthorizationDenied}}
	r := batchRouter(s, empC, ctrC, az, nil, nil)
	run := newRun(t, r, "corr-batch-authz", false)
	b := frozenBatch(s, "le-us", standardLines())
	code, out := calc(r, run.RunID, map[string]any{"input_batch_id": b.BatchID})
	assert.Equal(t, http.StatusForbidden, code, out)
	assert.Empty(t, s.consumed)

	// Calculating without a batch needs only the calculate permission.
	code, _ = calc(r, run.RunID, map[string]any{})
	assert.Equal(t, http.StatusOK, code)
}

func TestCalculateRun_ARunCannotSwitchBatchesAndAShadowRunConsumesNone(t *testing.T) {
	empC, ctrC := twoEmployees()
	s := newStubStore()
	r := batchRouter(s, empC, ctrC, &stubAuthZ{}, nil, nil)
	run := newRun(t, r, "corr-batch-switch", false)
	first := frozenBatch(s, "le-us", standardLines())
	other := frozenBatch(s, "le-us", standardLines())
	code, _ := calc(r, run.RunID, map[string]any{"input_batch_id": first.BatchID})
	require.Equal(t, http.StatusOK, code)
	code, out := calc(r, run.RunID, map[string]any{"input_batch_id": other.BatchID})
	assert.Equal(t, http.StatusConflict, code, out)
	assert.Equal(t, "run_linked_to_other_batch", out["error_code"])

	shadow := newRun(t, r, "corr-batch-shadow", true)
	third := frozenBatch(s, "le-us", standardLines())
	code, out = calc(r, shadow.RunID, map[string]any{"input_batch_id": third.BatchID})
	assert.Equal(t, http.StatusUnprocessableEntity, code, out)
	assert.Equal(t, "shadow_run_cannot_consume_batch", out["error_code"])
	assert.Equal(t, domain.InputBatchFrozen, s.batches[third.BatchID].State)
}

func TestCalculateRun_AConsumeRaceRefusesTheCalculation(t *testing.T) {
	empC, ctrC := twoEmployees()
	s := newStubStore()
	r := batchRouter(s, empC, ctrC, &stubAuthZ{}, nil, nil)
	run := newRun(t, r, "corr-batch-race", false)
	b := frozenBatch(s, "le-us", standardLines())
	s.consumeErr = domain.ErrRunAlreadyLinked
	code, out := calc(r, run.RunID, map[string]any{"input_batch_id": b.BatchID})
	assert.Equal(t, http.StatusConflict, code, out)
	assert.Equal(t, "batch_consume_conflict", out["error_code"])
	assert.Empty(t, s.slips[run.RunID], "the batch was taken by another run: nothing is stored")

	s.consumeErr = errors.New("db down")
	code, _ = calc(r, run.RunID, map[string]any{"input_batch_id": b.BatchID})
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Empty(t, s.slips[run.RunID])
}

func TestCalculateRun_WithoutABatchTheContractPathIsUnchanged(t *testing.T) {
	empC := &stubEmployeeClient{employees: []employee.Employee{{EmployeeID: "emp-1", EmployeeNumber: "E1", FirstName: "A", LastName: "B"}}}
	ctrC := &stubContractClient{contracts: map[string]*contract.ActiveContract{"emp-1": {ContractID: "c", EmployeeID: "emp-1", BaseSalaryAmount: 3000, Currency: "GBP", Status: "ACTIVE"}}}
	s := newStubStore()
	r := batchRouter(s, empC, ctrC, &stubAuthZ{}, nil, nil)
	run := newRun(t, r, "corr-no-batch", false)
	code, out := calc(r, run.RunID, map[string]any{})
	require.Equal(t, http.StatusOK, code, out)
	assert.Nil(t, out["input_batch_id"])
	assert.Equal(t, 3000.0, s.slips[run.RunID][0].GrossPay)
	assert.Empty(t, s.consumed)
}
