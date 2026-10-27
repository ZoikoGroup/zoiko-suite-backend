package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/compensation"
	"zoiko.io/payroll-run-svc/internal/contract"
	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/employee"
	"zoiko.io/payroll-run-svc/internal/events"
	svcmiddleware "zoiko.io/payroll-run-svc/internal/middleware"
	"zoiko.io/payroll-run-svc/internal/store"
)

type Store interface {
	CreatePayrollRun(ctx context.Context, r *domain.PayrollRun) (created bool, err error)
	GetPayrollRun(ctx context.Context, id string) (*domain.PayrollRun, error)
	ListPayrollRuns(ctx context.Context, legalEntityID, status string, isShadowRun *bool) ([]domain.PayrollRun, error)
	SaveCalculatedResults(ctx context.Context, runID string, totalGross, totalNet, totalTax, totalDeductions float64, slips []domain.PaySlip, shadowComps []domain.ShadowComparison) error
	GetPaySlipsByRun(ctx context.Context, runID string) ([]domain.PaySlip, error)
	GetShadowComparisonsByRun(ctx context.Context, runID string) ([]domain.ShadowComparison, error)
	FinalizePayrollRun(ctx context.Context, runID string, governanceDecisionID *string) error
	// EnqueueRunEvent writes a run event that has no state change of its own.
	EnqueueRunEvent(ctx context.Context, run domain.PayrollRun, fn store.RunEventFunc) error
	// ControlPopulation serves the control-population contract (pay-slips, payroll-runs).
	ControlPopulation(ctx context.Context, q domain.ControlPopulationQuery) (*domain.ControlPopulationPage, error)

	// The frozen WFP input batch a calculation may take its inputs from (WFP-CTRL-006).
	GetInputBatch(ctx context.Context, id string) (*domain.InputBatch, error)
	GetInputBatchByRun(ctx context.Context, runID string) (*domain.InputBatch, error)
	ListInputLines(ctx context.Context, batchID, state, workerID string) ([]domain.InputLine, error)
	ConsumeInputBatch(ctx context.Context, id string, expectedVersion int, runID string, mk store.InputEventBuilder) (*domain.InputBatch, error)
}

type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type EmployeeClient interface {
	ListActiveEmployeesByEntity(ctx context.Context, tenantID, principalID, legalEntityID string) ([]employee.Employee, error)
}

type ContractClient interface {
	GetActiveContract(ctx context.Context, tenantID, principalID, employeeID string) (*contract.ActiveContract, error)
}

// CompensationClient resolves what an employee's pay is composed of.
// GetBreakdown returns compensation.ErrNoStructure when the employee simply has
// none configured, which is a normal state and not an error.
type CompensationClient interface {
	GetBreakdown(ctx context.Context, tenantID, principalID, employeeID string, baseAmount float64) (*compensation.Breakdown, error)
}

const (
	actionRunCreate   = "PAYROLL_RUN_CREATE"
	actionRunView     = "PAYROLL_RUN_VIEW"
	actionRunCalc     = "PAYROLL_RUN_CALCULATE"
	actionRunFinalize = "PAYROLL_RUN_FINALIZE"
)

type Handler struct {
	store      Store
	authz      AuthZClient
	empClient  EmployeeClient
	ctrClient  ContractClient
	compClient CompensationClient
	// defaultTaxRate is applied to the taxable amount when no jurisdiction
	// rule is available.
	//
	// This is a placeholder, and the last fabricated number left in this
	// calculation. payroll-tax-svc is the authoritative source and should
	// replace it; until it is wired, the rate is at least named, configurable,
	// and applied to the correct base rather than to gross.
	defaultTaxRate float64
	log            *zap.Logger

	// tax and jurisdictions, when set (WithTaxEngine), replace the placeholder rate with payroll-tax-svc.
	tax           TaxEngine
	jurisdictions WorkerJurisdictions
}

func New(st Store, authz AuthZClient, empClient EmployeeClient, ctrClient ContractClient, compClient CompensationClient, defaultTaxRate float64, log *zap.Logger) *Handler {
	return &Handler{
		store:          st,
		authz:          authz,
		empClient:      empClient,
		ctrClient:      ctrClient,
		compClient:     compClient,
		defaultTaxRate: defaultTaxRate,
		log:            log,
	}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Get("/v1/control-populations/{population}", h.GetControlPopulation)
	r.Route("/v1/payroll/runs", func(r chi.Router) {
		r.Post("/", h.InitiateRun)
		r.Get("/", h.ListRuns)
		r.Get("/{id}", h.GetRun)
		r.Post("/{id}/calculate", h.CalculateRun)
		r.Get("/{id}/slips", h.GetPaySlips)
		r.Get("/{id}/shadow-comparison", h.GetShadowComparison)
		r.Post("/{id}/finalize", h.FinalizeRun)
	})
}

// ── POST /v1/payroll/runs ─────────────────────────────────────────────────────────

func (h *Handler) InitiateRun(w http.ResponseWriter, r *http.Request) {
	var req domain.InitiatePayrollRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.LegalEntityID == "" || req.PayPeriodStart == "" || req.PayPeriodEnd == "" || req.PayDate == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, pay_period_start, pay_period_end, pay_date are required")
		return
	}
	if req.CorrelationID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "correlation_id is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionRunCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	runNum := req.RunNumber
	if runNum == "" {
		runNum = fmt.Sprintf("PAY-%s", uuid.NewString()[:8])
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	now := time.Now().UTC()
	payrollRun := &domain.PayrollRun{
		RunID:          uuid.NewString(),
		TenantID:       tenantID,
		LegalEntityID:  req.LegalEntityID,
		RunNumber:      runNum,
		PayPeriodStart: req.PayPeriodStart,
		PayPeriodEnd:   req.PayPeriodEnd,
		PayDate:        req.PayDate,
		Status:         domain.PayrollRunStatusInitiated,
		IsShadowRun:    req.IsShadowRun,
		CorrelationID:  req.CorrelationID,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	// The initiated event is written in the same transaction as the run (outbox).
	created, err := h.store.CreatePayrollRun(store.WithRunEvent(r.Context(), h.runEvent(events.RunInitiated, getCorrelationID(r), principalID, "")), payrollRun)
	if err != nil {
		h.log.Error("failed to create payroll run", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if !created {
		// Replay of a prior request with the same correlation_id — return
		// the original run, do not re-publish the initiated event.
		writeJSON(w, http.StatusOK, payrollRun)
		return
	}

	writeJSON(w, http.StatusCreated, payrollRun)
}

// ── POST /v1/payroll/runs/{id}/calculate ──────────────────────────────────────────

func (h *Handler) CalculateRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req domain.CalculateRunRequest
	_ = json.NewDecoder(r.Body).Decode(&req)

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	run, err := h.store.GetPayrollRun(r.Context(), id)
	if errors.Is(err, domain.ErrPayrollRunNotFound) {
		writeError(w, http.StatusNotFound, "run_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch payroll run", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if run.Status == domain.PayrollRunStatusCompleted {
		writeError(w, http.StatusConflict, "run_finalized", string(domain.ErrRunAlreadyFinalized))
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, run.LegalEntityID, actionRunCalc); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())

	// 1. Fetch active workers for legal entity. Fail closed: if
	// employee-master-svc is unreachable, we cannot know who should be
	// paid, so this must not silently proceed as if the entity has zero
	// active employees (which would "successfully" calculate an empty,
	// wrong payroll run instead of surfacing the real outage).
	var activeEmployees []employee.Employee
	if h.empClient != nil {
		activeEmployees, err = h.empClient.ListActiveEmployeesByEntity(r.Context(), tenantID, principalID, run.LegalEntityID)
		if err != nil {
			h.log.Error("failed to fetch active workers from employee-master-svc", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "employee_lookup_failed", "cannot verify active employees: employee-master-svc unavailable")
			return
		}
	}

	// Frozen WFP input batch (WFP-CTRL-006): the one this run already consumed, or the one the request names.
	rb, bfail := h.resolveInputBatch(r, principalID, run, req.InputBatchID)
	if bfail != nil {
		bfail.write(h, w)
		return
	}

	// 2. Build shadow lookup map if provided
	shadowMap := make(map[string]domain.ShadowInputItem)
	for _, item := range req.ShadowBaselineItems {
		shadowMap[item.EmployeeID] = item
	}

	now := time.Now().UTC()
	var (
		slips           []domain.PaySlip
		shadowComps     []domain.ShadowComparison
		totalGross      float64
		totalNet        float64
		totalTax        float64
		totalDeductions float64
	)

	// Process workers. Fail closed on the salary source: this loop used to
	// default an employee's gross pay to a hardcoded $8000/month baseline
	// whenever the contract lookup failed or returned no active contract,
	// silently computing (and potentially finalizing) real disbursement
	// figures off a made-up number. Any employee whose real salary can't
	// be confirmed now blocks the entire calculation instead. Each
	// employee's contract is fetched exactly once and cached here.
	if len(activeEmployees) > 0 {
		// Inputs come from the frozen WFP batch when the run names (or already consumed) one, otherwise from the
		// employee's contract and compensation structure. Either way: fail closed.
		var inputs map[string]slipInput
		var ifail *inputFailure
		if rb != nil {
			inputs, ifail = h.batchInputs(tenantID, activeEmployees, rb)
		} else {
			inputs, ifail = h.legacyInputs(r, tenantID, principalID, activeEmployees)
		}
		if ifail != nil {
			if ifail.block != "" {
				h.enqueueBlocked(r, principalID, *run, ifail.block)
			}
			ifail.write(h, w)
			return
		}

		// Tax from the tax engine when it is configured (fail closed); nil means the legacy placeholder rate.
		taxes, tfail := h.computeTaxes(r.Context(), tenantID, principalID, run, activeEmployees, inputs)
		if tfail != nil {
			h.enqueueBlocked(r, principalID, *run, tfail.message)
			writeJSON(w, tfail.status, tfail.body())
			return
		}

		for _, emp := range activeEmployees {
			in := inputs[emp.EmployeeID]
			slipCalc := in.calc
			taxOut := taxes[emp.EmployeeID]
			if taxOut != nil {
				taxOut.apply(&slipCalc)
			}
			gross := slipCalc.gross
			tax := slipCalc.tax
			benefits := slipCalc.deductions
			net := slipCalc.net
			curr := in.currency

			totalGross += gross
			totalNet += net
			totalTax += tax
			totalDeductions += benefits

			name := fmt.Sprintf("%s %s", emp.FirstName, emp.LastName)
			slipID := uuid.NewString()
			slip := domain.PaySlip{
				SlipID:             slipID,
				TenantID:           tenantID,
				RunID:              run.RunID,
				EmployeeID:         emp.EmployeeID,
				EmployeeNumber:     emp.EmployeeNumber,
				EmployeeName:       name,
				GrossPay:           gross,
				TaxWithheld:        tax,
				BenefitsDeductions: benefits,
				NetPay:             net,
				Currency:           curr,
				EffectiveDate:      run.PayDate,
				TaxableAmount:      slipCalc.taxable,
				StructureID:        slipCalc.structureID,
				Items:              in.items(slipID, now),
				TaxLines:           taxLinesOf(taxOut, tenantID, slipID, now),
				CreatedAt:          now,
			}
			slips = append(slips, slip)

			if run.IsShadowRun {
				legacyItem, hasLegacy := shadowMap[emp.EmployeeID]
				if !hasLegacy {
					legacyItem = domain.ShadowInputItem{
						EmployeeID:        emp.EmployeeID,
						LegacyGrossPay:    gross,
						LegacyNetPay:      net,
						LegacyTaxWithheld: tax,
					}
				}

				gVar := legacyItem.LegacyGrossPay - gross
				nVar := legacyItem.LegacyNetPay - net
				tVar := legacyItem.LegacyTaxWithheld - tax
				isEquiv := math.Abs(nVar) < 0.01

				shadowComps = append(shadowComps, domain.ShadowComparison{
					ComparisonID:      uuid.NewString(),
					TenantID:          tenantID,
					RunID:             run.RunID,
					EmployeeID:        emp.EmployeeID,
					LegacyGrossPay:    legacyItem.LegacyGrossPay,
					LegacyNetPay:      legacyItem.LegacyNetPay,
					LegacyTaxWithheld: legacyItem.LegacyTaxWithheld,
					ZoikoGrossPay:     gross,
					ZoikoNetPay:       net,
					ZoikoTaxWithheld:  tax,
					GrossVariance:     gVar,
					NetVariance:       nVar,
					TaxVariance:       tVar,
					IsEquivalent:      isEquiv,
					CreatedAt:         now,
				})
			}
		}
	} else if len(req.ShadowBaselineItems) > 0 {
		// Fallback: use shadow input items directly if employee-master empty
		for _, item := range req.ShadowBaselineItems {
			gross := item.LegacyGrossPay
			tax := item.LegacyTaxWithheld
			net := item.LegacyNetPay

			// Other deductions are derived from the legacy figures rather than
			// assumed: whatever is left after tax that did not reach net pay is,
			// by definition, the legacy system's other deductions. This used to
			// be a flat gross*0.05, which put a number on the slip that the
			// legacy payroll never produced. Floored at zero because legacy
			// inputs that do not reconcile must not create a negative deduction.
			benefits := math.Max(round2(gross-tax-net), 0)

			totalGross += gross
			totalNet += net
			totalTax += tax
			totalDeductions += benefits

			slip := domain.PaySlip{
				SlipID:             uuid.NewString(),
				TenantID:           tenantID,
				RunID:              run.RunID,
				EmployeeID:         item.EmployeeID,
				EmployeeNumber:     "EMP-" + item.EmployeeID[:6],
				EmployeeName:       "Worker " + item.EmployeeID[:6],
				GrossPay:           gross,
				TaxWithheld:        tax,
				BenefitsDeductions: benefits,
				NetPay:             net,
				Currency:           "USD",
				EffectiveDate:      run.PayDate,
				CreatedAt:          now,
			}
			slips = append(slips, slip)

			if run.IsShadowRun {
				shadowComps = append(shadowComps, domain.ShadowComparison{
					ComparisonID:      uuid.NewString(),
					TenantID:          tenantID,
					RunID:             run.RunID,
					EmployeeID:        item.EmployeeID,
					LegacyGrossPay:    item.LegacyGrossPay,
					LegacyNetPay:      item.LegacyNetPay,
					LegacyTaxWithheld: item.LegacyTaxWithheld,
					ZoikoGrossPay:     gross,
					ZoikoNetPay:       net,
					ZoikoTaxWithheld:  tax,
					GrossVariance:     0.0,
					NetVariance:       0.0,
					TaxVariance:       0.0,
					IsEquivalent:      true,
					CreatedAt:         now,
				})
			}
		}
	}

	if rb != nil && !rb.alreadyConsumed {
		// Tie the batch to this run now that every input has been resolved and every tax answered. A batch that
		// another run consumed in the meantime, or that changed, refuses here and nothing is stored.
		if _, cerr := h.store.ConsumeInputBatch(r.Context(), rb.batch.BatchID, rb.batch.Version, run.RunID, inputEventBuilder(r, principalID, tenantID)); cerr != nil {
			switch {
			case errors.Is(cerr, domain.ErrRunAlreadyLinked), errors.Is(cerr, domain.ErrInputBatchNotFrozen), errors.Is(cerr, domain.ErrVersionConflict):
				writeError(w, http.StatusConflict, "batch_consume_conflict", cerr.Error())
			default:
				h.log.Error("failed to consume the input batch", zap.Error(cerr))
				writeError(w, http.StatusServiceUnavailable, "store_unavailable", "cannot consume the input batch")
			}
			return
		}
	}

	if err := h.store.SaveCalculatedResults(store.WithRunEvent(r.Context(), h.runEvent(events.RunCalculated, getCorrelationID(r), principalID, "")), id, totalGross, totalNet, totalTax, totalDeductions, slips, shadowComps); err != nil {
		h.log.Error("failed to save calculated payroll results", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	updatedRun, _ := h.store.GetPayrollRun(r.Context(), id)

	writeJSON(w, http.StatusOK, map[string]any{
		"run":                updatedRun,
		"pay_slips":          slips,
		"shadow_comparisons": shadowComps,
		"input_batch_id":     inputBatchIDOf(rb),
	})
}

// ── GET /v1/payroll/runs ──────────────────────────────────────────────────────────

func (h *Handler) ListRuns(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	status := r.URL.Query().Get("status")
	shadowStr := r.URL.Query().Get("is_shadow_run")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if legalEntityID != "" {
		if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionRunView); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
	}

	var isShadow *bool
	if shadowStr != "" {
		v, err := strconv.ParseBool(shadowStr)
		if err == nil {
			isShadow = &v
		}
	}

	list, err := h.store.ListPayrollRuns(r.Context(), legalEntityID, status, isShadow)
	if err != nil {
		h.log.Error("failed to list payroll runs", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if list == nil {
		list = []domain.PayrollRun{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── GET /v1/payroll/runs/{id} ─────────────────────────────────────────────────────

func (h *Handler) GetRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	run, err := h.store.GetPayrollRun(r.Context(), id)
	if errors.Is(err, domain.ErrPayrollRunNotFound) {
		writeError(w, http.StatusNotFound, "run_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch payroll run", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, run.LegalEntityID, actionRunView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	writeJSON(w, http.StatusOK, run)
}

// ── GET /v1/payroll/runs/{id}/slips ───────────────────────────────────────────────

func (h *Handler) GetPaySlips(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	run, err := h.store.GetPayrollRun(r.Context(), id)
	if errors.Is(err, domain.ErrPayrollRunNotFound) {
		writeError(w, http.StatusNotFound, "run_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch payroll run", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, run.LegalEntityID, actionRunView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	slips, err := h.store.GetPaySlipsByRun(r.Context(), id)
	if err != nil {
		h.log.Error("failed to fetch pay slips", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if slips == nil {
		slips = []domain.PaySlip{}
	}
	writeJSON(w, http.StatusOK, slips)
}

// ── GET /v1/payroll/runs/{id}/shadow-comparison ──────────────────────────────────

func (h *Handler) GetShadowComparison(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	run, err := h.store.GetPayrollRun(r.Context(), id)
	if errors.Is(err, domain.ErrPayrollRunNotFound) {
		writeError(w, http.StatusNotFound, "run_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch payroll run", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, run.LegalEntityID, actionRunView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	comps, err := h.store.GetShadowComparisonsByRun(r.Context(), id)
	if err != nil {
		h.log.Error("failed to fetch shadow comparisons", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if comps == nil {
		comps = []domain.ShadowComparison{}
	}

	equivalentCount := 0
	for _, c := range comps {
		if c.IsEquivalent {
			equivalentCount++
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"run_id":            id,
		"total_compared":    len(comps),
		"equivalent_count":  equivalentCount,
		"is_all_equivalent": len(comps) > 0 && equivalentCount == len(comps),
		"comparisons":       comps,
	})
}

// ── POST /v1/payroll/runs/{id}/finalize ───────────────────────────────────────────

func (h *Handler) FinalizeRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	run, err := h.store.GetPayrollRun(r.Context(), id)
	if errors.Is(err, domain.ErrPayrollRunNotFound) {
		writeError(w, http.StatusNotFound, "run_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch payroll run", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, run.LegalEntityID, actionRunFinalize); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	// Body is optional — a caller with no governance decision to cite may
	// finalize with an empty body, same as before this field existed.
	var req domain.FinalizeRunRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	if err := h.store.FinalizePayrollRun(store.WithRunEvent(r.Context(), h.runEvent(events.RunCompleted, getCorrelationID(r), principalID, "")), id, req.GovernanceDecisionID); err != nil {
		if errors.Is(err, domain.ErrRunAlreadyFinalized) {
			writeError(w, http.StatusConflict, "already_finalized", err.Error())
			return
		}
		if errors.Is(err, domain.ErrRunNotCalculated) {
			writeError(w, http.StatusBadRequest, "not_calculated", err.Error())
			return
		}
		h.log.Error("failed to finalize payroll run", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	finalizedRun, _ := h.store.GetPayrollRun(r.Context(), id)
	writeJSON(w, http.StatusOK, finalizedRun)
}

// ── Helpers ──────────────────────────────────────────────────────────────────────

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrIdentityMissing))
		return "", false
	}
	return principalID, true
}

func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, domain.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
	} else {
		writeError(w, http.StatusServiceUnavailable, "authz_unavailable", err.Error())
	}
}

func getCorrelationID(r *http.Request) string {
	cid := r.Header.Get("X-Correlation-ID")
	if cid == "" {
		return uuid.NewString()
	}
	return cid
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error_code":    code,
		"error_message": msg,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// runEvent builds the outbox event for a run lifecycle change. The actor is the
// already-authorized principal: domain.PayrollRun carries no actor of its own.
func (h *Handler) runEvent(eventType, correlationID, actorID, reason string) store.RunEventFunc {
	return func(run domain.PayrollRun) (domain.OutboxEvent, error) {
		body, err := events.BuildRunEnvelope(eventType, correlationID, actorID, run, reason)
		return domain.OutboxEvent{TenantID: run.TenantID, ObjectID: run.RunID, EventType: eventType, Payload: body}, err
	}
}

// enqueueBlocked records that a run was blocked before it could be calculated. A
// failure to record is logged, not allowed to change the answer the caller gets.
func (h *Handler) enqueueBlocked(r *http.Request, principalID string, run domain.PayrollRun, reason string) {
	if err := h.store.EnqueueRunEvent(r.Context(), run, h.runEvent(events.RunBlocked, getCorrelationID(r), principalID, reason)); err != nil {
		h.log.Error("failed to record payroll.run.blocked", zap.String("run_id", run.RunID), zap.Error(err))
	}
}
