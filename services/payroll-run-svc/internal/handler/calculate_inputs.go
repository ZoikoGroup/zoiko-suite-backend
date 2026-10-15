package handler

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/compensation"
	"zoiko.io/payroll-run-svc/internal/contract"
	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/employee"
)

// slipInput is what one employee's payslip is built from, whichever source supplied it.
type slipInput struct {
	calc     slipCalculation
	currency string
	items    func(slipID string, now time.Time) []domain.PaySlipItem
}

// inputFailure is why the inputs of a run could not be resolved. block, when set, is the reason recorded on
// the run's BLOCKED event; authErr hands an authorization outcome to the caller's own mapping.
type inputFailure struct {
	status    int
	code      string
	message   string
	employees []string
	block     string
	authErr   error
}

func (f *inputFailure) write(h *Handler, w http.ResponseWriter) {
	if f.authErr != nil {
		h.writeAuthzErr(w, f.authErr)
		return
	}
	if len(f.employees) > 0 {
		writeJSON(w, f.status, map[string]any{"error_code": f.code, "error_message": f.message, "employees_affected": f.employees})
		return
	}
	writeError(w, f.status, f.code, f.message)
}

// resolvedBatch is the frozen input batch a calculation takes its inputs from.
type resolvedBatch struct {
	batch           *domain.InputBatch
	lines           []domain.InputLine
	alreadyConsumed bool // by THIS run (a recalculation)
}

// resolveInputBatch decides which frozen batch, if any, a calculation uses. A run that already consumed a batch
// keeps using it (a recalculation can never silently drop its frozen inputs); otherwise a batch the request names
// must be FROZEN, of the run's entity and exact period, and the caller must hold the consume permission.
func (h *Handler) resolveInputBatch(r *http.Request, principalID string, run *domain.PayrollRun, named *string) (*resolvedBatch, *inputFailure) {
	ctx := r.Context()
	existing, err := h.store.GetInputBatchByRun(ctx, run.RunID)
	if err != nil {
		h.log.Error("failed to look up the run's input batch", zap.Error(err))
		return nil, &inputFailure{status: http.StatusServiceUnavailable, code: "store_unavailable", message: "cannot determine whether the run consumed an input batch"}
	}
	var b *domain.InputBatch
	consumed := false
	switch {
	case existing != nil:
		if named != nil && strings.TrimSpace(*named) != "" && *named != existing.BatchID {
			return nil, &inputFailure{status: http.StatusConflict, code: "run_linked_to_other_batch", message: domain.ErrRunLinkedToOtherBatch.Error()}
		}
		b, consumed = existing, true
	case named != nil && strings.TrimSpace(*named) != "":
		if run.IsShadowRun {
			return nil, &inputFailure{status: http.StatusUnprocessableEntity, code: "shadow_run_cannot_consume_batch", message: "a shadow run is a comparison and does not consume a frozen input batch"}
		}
		if _, perr := uuid.Parse(*named); perr != nil {
			return nil, &inputFailure{status: http.StatusBadRequest, code: "validation_failed", message: "input_batch_id must be a UUID"}
		}
		if aerr := h.authz.CheckAllowed(ctx, principalID, run.LegalEntityID, actionInputConsume); aerr != nil {
			return nil, &inputFailure{authErr: aerr}
		}
		got, gerr := h.store.GetInputBatch(ctx, *named)
		switch {
		case errors.Is(gerr, domain.ErrInputBatchNotFound):
			return nil, &inputFailure{status: http.StatusNotFound, code: "input_batch_not_found", message: gerr.Error()}
		case gerr != nil:
			h.log.Error("failed to load the input batch", zap.Error(gerr))
			return nil, &inputFailure{status: http.StatusServiceUnavailable, code: "store_unavailable", message: "cannot load the input batch"}
		}
		if got.LegalEntityID != run.LegalEntityID {
			return nil, &inputFailure{status: http.StatusUnprocessableEntity, code: "batch_entity_mismatch", message: domain.ErrBatchEntityMismatch.Error()}
		}
		if got.PeriodStart != run.PayPeriodStart || got.PeriodEnd != run.PayPeriodEnd {
			return nil, &inputFailure{status: http.StatusUnprocessableEntity, code: "batch_period_mismatch", message: domain.ErrRunPeriodMismatch.Error()}
		}
		if got.State != domain.InputBatchFrozen {
			return nil, &inputFailure{status: http.StatusConflict, code: "batch_not_frozen", message: fmt.Sprintf("%s (it is %s)", domain.ErrBatchNotFrozen.Error(), got.State)}
		}
		b = got
	default:
		return nil, nil
	}
	lines, lerr := h.store.ListInputLines(ctx, b.BatchID, domain.InputLineAccepted, "")
	if lerr != nil {
		h.log.Error("failed to load the input batch lines", zap.Error(lerr))
		return nil, &inputFailure{status: http.StatusServiceUnavailable, code: "store_unavailable", message: "cannot load the input batch lines"}
	}
	if len(lines) == 0 {
		return nil, &inputFailure{status: http.StatusUnprocessableEntity, code: "batch_empty", message: domain.ErrBatchEmpty.Error()}
	}
	return &resolvedBatch{batch: b, lines: lines, alreadyConsumed: consumed}, nil
}

// legacyInputs reads each employee's salary from their active contract and their composition from
// compensation-svc, exactly as the calculation always has. Fail closed on both.
func (h *Handler) legacyInputs(r *http.Request, tenantID, principalID string, emps []employee.Employee) (map[string]slipInput, *inputFailure) {
	contracts := make(map[string]*contract.ActiveContract, len(emps))
	var missing []string
	for _, emp := range emps {
		if h.ctrClient == nil {
			missing = append(missing, emp.EmployeeID)
			continue
		}
		ctr, err := h.ctrClient.GetActiveContract(r.Context(), tenantID, principalID, emp.EmployeeID)
		if err != nil || ctr == nil || ctr.BaseSalaryAmount <= 0 {
			h.log.Error("failed to verify active salary contract for employee", zap.String("employee_id", emp.EmployeeID), zap.Error(err))
			missing = append(missing, emp.EmployeeID)
			continue
		}
		contracts[emp.EmployeeID] = ctr
	}
	if len(missing) > 0 {
		return nil, &inputFailure{status: http.StatusUnprocessableEntity, code: "contract_lookup_failed", message: domain.ErrContractLookupFailed.Error(),
			employees: missing, block: domain.ErrContractLookupFailed.Error()}
	}

	// The base salary comes from the contract (authoritative); compensation-svc supplies only the composition layered
	// on it. No structure is a normal state (flat base); the service being unreachable is not.
	breakdowns := make(map[string]*compensation.Breakdown, len(emps))
	for _, emp := range emps {
		if h.compClient == nil {
			continue
		}
		bd, err := h.compClient.GetBreakdown(r.Context(), tenantID, principalID, emp.EmployeeID, contracts[emp.EmployeeID].BaseSalaryAmount)
		if errors.Is(err, compensation.ErrNoStructure) {
			continue
		}
		if err != nil {
			h.log.Error("failed to resolve compensation breakdown", zap.String("employee_id", emp.EmployeeID), zap.Error(err))
			return nil, &inputFailure{status: http.StatusServiceUnavailable, code: "compensation_lookup_failed", message: string(domain.ErrCompensationLookupFailed),
				block: domain.ErrCompensationLookupFailed.Error()}
		}
		breakdowns[emp.EmployeeID] = bd
	}

	out := make(map[string]slipInput, len(emps))
	for _, emp := range emps {
		ctr := contracts[emp.EmployeeID]
		bd := breakdowns[emp.EmployeeID]
		curr := "USD"
		if ctr.Currency != "" {
			curr = ctr.Currency
		}
		calc := h.calculateSlip(ctr.BaseSalaryAmount, bd)
		if calc.currency != "" {
			curr = calc.currency
		}
		out[emp.EmployeeID] = slipInput{calc: calc, currency: curr, items: func(slipID string, now time.Time) []domain.PaySlipItem {
			return buildSlipItems(tenantID, slipID, now, bd)
		}}
	}
	return out, nil
}

// batchInputs derives every employee's pay from the frozen batch. The batch and the active workers must be the same
// population: a worker the batch lacks would be paid nothing, and one it has who is no longer active would be paid
// from stale facts, so either blocks the run.
func (h *Handler) batchInputs(tenantID string, emps []employee.Employee, rb *resolvedBatch) (map[string]slipInput, *inputFailure) {
	pay, err := domain.BuildBatchPay(rb.lines)
	if err != nil {
		return nil, &inputFailure{status: http.StatusUnprocessableEntity, code: "batch_not_treatable", message: err.Error(), block: err.Error()}
	}
	active := make(map[string]bool, len(emps))
	var missing []string
	for _, emp := range emps {
		active[emp.EmployeeID] = true
		if pay[emp.EmployeeID] == nil {
			missing = append(missing, emp.EmployeeID)
		}
	}
	var extra []string
	for id := range pay {
		if !active[id] {
			extra = append(extra, id)
		}
	}
	if len(missing) > 0 || len(extra) > 0 {
		sort.Strings(missing)
		sort.Strings(extra)
		return nil, &inputFailure{status: http.StatusUnprocessableEntity, code: "batch_population_mismatch",
			message:   fmt.Sprintf("%s: %d active worker(s) have no lines in the batch, %d batch worker(s) are not active", domain.ErrBatchPopulationMismatch.Error(), len(missing), len(extra)),
			employees: append(missing, extra...), block: domain.ErrBatchPopulationMismatch.Error()}
	}
	out := make(map[string]slipInput, len(emps))
	for _, emp := range emps {
		p := pay[emp.EmployeeID]
		for _, it := range p.Items {
			if len(it.Code) > 50 {
				return nil, &inputFailure{status: http.StatusUnprocessableEntity, code: "batch_code_too_long", message: fmt.Sprintf("payroll code %q exceeds 50 characters", it.Code),
					employees: []string{emp.EmployeeID}, block: "payroll code too long"}
			}
		}
		tax := round2(p.Taxable * h.defaultTaxRate)
		calc := slipCalculation{gross: p.Gross, tax: tax, deductions: p.Deductions, net: round2(p.Gross - p.Deductions - tax), taxable: p.Taxable, currency: p.Currency}
		items := p.Items
		out[emp.EmployeeID] = slipInput{calc: calc, currency: p.Currency, items: func(slipID string, now time.Time) []domain.PaySlipItem {
			res := make([]domain.PaySlipItem, 0, len(items))
			for i, it := range items {
				res = append(res, domain.PaySlipItem{
					ItemID: uuid.NewString(), TenantID: tenantID, SlipID: slipID, ComponentCode: it.Code, ComponentName: it.Code, ComponentType: it.Type,
					IsTaxable: it.IsTaxable, CalculationMethod: "FIXED", CalculationValue: it.Amount, Amount: it.Amount, Sequence: i + 1, CreatedAt: now,
				})
			}
			return res
		}}
	}
	return out, nil
}

// inputBatchIDOf is the batch a calculation used, or nil.
func inputBatchIDOf(rb *resolvedBatch) *string {
	if rb == nil {
		return nil
	}
	return &rb.batch.BatchID
}
