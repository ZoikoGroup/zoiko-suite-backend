package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/employee"
	"zoiko.io/payroll-run-svc/internal/payrolltax"
	"zoiko.io/payroll-run-svc/internal/workerctx"
)

// TaxEngine is payroll-tax-svc as the calculation sees it.
type TaxEngine interface {
	Calculate(ctx context.Context, caller payrolltax.Caller, in payrolltax.Request) (*payrolltax.Result, error)
}

// WorkerJurisdictions resolves a worker's payroll jurisdiction from the WFP-01 financial profile.
type WorkerJurisdictions interface {
	Validate(ctx context.Context, caller workerctx.Caller, workerID, legalEntityID, asOf string) (domain.WorkerContext, error)
}

// taxConcurrency bounds the calls to the tax engine and the profile service during one calculation.
const taxConcurrency = 8

// WithTaxEngine turns the tax engine mode on: from now on a calculation takes each employee's tax from
// payroll-tax-svc and records its breakdown, and fails closed when it cannot. Without it the legacy
// placeholder rate applies and results say so. It returns h for chaining.
func (h *Handler) WithTaxEngine(t TaxEngine, j WorkerJurisdictions) *Handler {
	h.tax, h.jurisdictions = t, j
	return h
}

// taxOutcome is one employee's tax from the engine.
type taxOutcome struct {
	jurisdiction string
	result       *payrolltax.Result
	total        float64 // round2 of the sum of the components: the figure withheld
}

// taxFailure is why a calculation was blocked on tax.
type taxFailure struct {
	status    int
	code      string
	message   string
	employees []string
}

func (f *taxFailure) body() map[string]any {
	m := map[string]any{"error_code": f.code, "error_message": f.message}
	if len(f.employees) > 0 {
		m["employees_affected"] = f.employees
	}
	return m
}

// apply replaces the placeholder tax of a slip with the engine's. Net pay moves with it: the pre-tax net is
// unchanged, only the tax withheld differs.
func (o *taxOutcome) apply(c *slipCalculation) {
	preTaxNet := round2(c.net + c.tax)
	c.tax = o.total
	c.net = round2(preTaxNet - o.total)
}

// lines builds the slip's persisted tax lines, in the engine's order.
func (o *taxOutcome) lines(tenantID, slipID string, now time.Time) []domain.PaySlipTaxLine {
	out := make([]domain.PaySlipTaxLine, 0, len(o.result.TaxBreakdown))
	for i, c := range o.result.TaxBreakdown {
		out = append(out, domain.PaySlipTaxLine{
			LineID: uuid.NewString(), TenantID: tenantID, SlipID: slipID, Sequence: i + 1, TaxName: c.TaxName, TaxType: c.TaxType,
			RatePct: c.RatePct, Amount: c.TaxAmount, CalculationID: o.result.CalculationID, EngineType: o.result.EngineType,
			RuleVersion: o.result.RuleVersionUsed, JurisdictionCode: o.jurisdiction, CreatedAt: now,
		})
	}
	return out
}

// computeTaxes asks the tax engine for every employee with taxable pay. It returns nil, nil when the engine
// mode is off. Every failure is collected before anything is stored, so a run is blocked as a whole.
func (h *Handler) computeTaxes(ctx context.Context, tenantID, principalID string, run *domain.PayrollRun, emps []employee.Employee,
	inputs map[string]slipInput) (map[string]*taxOutcome, *taxFailure) {
	if h.tax == nil {
		return nil, nil
	}
	if h.jurisdictions == nil {
		return nil, &taxFailure{status: http.StatusServiceUnavailable, code: "tax_engine_not_configured", message: domain.ErrTaxEngineNotConfigured.Error()}
	}

	type task struct {
		emp      employee.Employee
		taxable  float64
		currency string
	}
	var tasks []task
	for _, emp := range emps {
		in := inputs[emp.EmployeeID]
		if in.calc.taxable > 0 {
			tasks = append(tasks, task{emp: emp, taxable: in.calc.taxable, currency: in.currency})
		}
	}

	var (
		mu         sync.Mutex
		out        = make(map[string]*taxOutcome, len(tasks))
		noJuris    []string
		engineDown []string
		unmapped   []string
		mismatch   []string
		lookupDown []string
		wg         sync.WaitGroup
		sem        = make(chan struct{}, taxConcurrency)
	)
	asOf := run.PayDate
	for _, t := range tasks {
		wg.Add(1)
		sem <- struct{}{}
		go func(t task) {
			defer wg.Done()
			defer func() { <-sem }()
			wc, err := h.jurisdictions.Validate(ctx, workerctx.Caller{TenantID: tenantID, PrincipalID: principalID, LegalEntityID: run.LegalEntityID,
				CorrelationID: uuid.NewString()}, t.emp.EmployeeID, run.LegalEntityID, asOf)
			if err != nil {
				mu.Lock()
				lookupDown = append(lookupDown, t.emp.EmployeeID)
				mu.Unlock()
				return
			}
			if !wc.Valid || len(wc.PayrollJurisdictions) != 1 || strings.TrimSpace(wc.PayrollJurisdictions[0]) == "" {
				mu.Lock()
				noJuris = append(noJuris, t.emp.EmployeeID)
				mu.Unlock()
				return
			}
			juris := wc.PayrollJurisdictions[0]
			res, err := h.tax.Calculate(ctx, payrolltax.Caller{TenantID: tenantID, PrincipalID: principalID}, payrolltax.Request{
				PayrollRunID: run.RunID, EmployeeID: t.emp.EmployeeID, JurisdictionCode: juris, GrossTaxableAmount: t.taxable,
				Currency: t.currency, CorrelationID: domain.TaxCorrelationID(run.RunID, t.emp.EmployeeID, t.taxable, juris, t.currency),
			})
			if err != nil {
				h.log.Error("tax engine refused or failed", zap.String("employee_id", t.emp.EmployeeID), zap.Error(err))
				mu.Lock()
				if errors.Is(err, domain.ErrTaxTotalMismatch) {
					mismatch = append(mismatch, t.emp.EmployeeID)
				} else {
					engineDown = append(engineDown, t.emp.EmployeeID)
				}
				mu.Unlock()
				return
			}
			sum := 0.0
			for _, c := range res.TaxBreakdown {
				if _, kerr := domain.ComponentKindForTaxType(c.TaxType); kerr != nil {
					mu.Lock()
					unmapped = append(unmapped, t.emp.EmployeeID)
					mu.Unlock()
					return
				}
				sum += c.TaxAmount
			}
			mu.Lock()
			out[t.emp.EmployeeID] = &taxOutcome{jurisdiction: juris, result: res, total: round2(sum)}
			mu.Unlock()
		}(t)
	}
	wg.Wait()

	fail := func(status int, code string, err error, ids []string) *taxFailure {
		sort.Strings(ids)
		return &taxFailure{status: status, code: code, message: err.Error(), employees: ids}
	}
	switch {
	case len(lookupDown) > 0:
		return nil, fail(http.StatusServiceUnavailable, "tax_jurisdiction_lookup_failed", fmt.Errorf("the worker financial profile service could not be asked: %w", domain.ErrSourceUnavailable), lookupDown)
	case len(noJuris) > 0:
		return nil, fail(http.StatusUnprocessableEntity, "tax_jurisdiction_unresolved", domain.ErrTaxJurisdictionUnresolved, noJuris)
	case len(engineDown) > 0:
		return nil, fail(http.StatusServiceUnavailable, "tax_engine_unavailable", domain.ErrTaxEngineUnavailable, engineDown)
	case len(mismatch) > 0:
		return nil, fail(http.StatusUnprocessableEntity, "tax_total_mismatch", domain.ErrTaxTotalMismatch, mismatch)
	case len(unmapped) > 0:
		return nil, fail(http.StatusUnprocessableEntity, "tax_type_unmapped", domain.ErrUnmappedTaxType, unmapped)
	}
	return out, nil
}

// taxLinesOf is nil when the slip's tax did not come from the engine.
func taxLinesOf(o *taxOutcome, tenantID, slipID string, now time.Time) []domain.PaySlipTaxLine {
	if o == nil {
		return nil
	}
	return o.lines(tenantID, slipID, now)
}
