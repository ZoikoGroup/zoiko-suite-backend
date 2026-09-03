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
	svcmiddleware "zoiko.io/payroll-run-svc/internal/middleware"
)

type Store interface {
	CreatePayrollRun(ctx context.Context, r *domain.PayrollRun) (created bool, err error)
	GetPayrollRun(ctx context.Context, id string) (*domain.PayrollRun, error)
	ListPayrollRuns(ctx context.Context, legalEntityID, status string, isShadowRun *bool) ([]domain.PayrollRun, error)
	SaveCalculatedResults(ctx context.Context, runID string, totalGross, totalNet, totalTax, totalDeductions float64, slips []domain.PaySlip, shadowComps []domain.ShadowComparison) error
	GetPaySlipsByRun(ctx context.Context, runID string) ([]domain.PaySlip, error)
	GetShadowComparisonsByRun(ctx context.Context, runID string) ([]domain.ShadowComparison, error)
	FinalizePayrollRun(ctx context.Context, runID string, governanceDecisionID *string) error
}

// actorID on every method is the already-authorized principal from that
// request's requirePrincipal() call — domain.PayrollRun carries no actor
// field of its own (no InitiatedBy/CalculatedBy/etc.), so this is the only
// real source for the event envelope's actor_id (Doc 03 §19).
type Publisher interface {
	PublishRunInitiated(ctx context.Context, correlationID, actorID string, r domain.PayrollRun)
	PublishRunCalculated(ctx context.Context, correlationID, actorID string, r domain.PayrollRun)
	PublishRunCompleted(ctx context.Context, correlationID, actorID string, r domain.PayrollRun)
	PublishRunBlocked(ctx context.Context, correlationID, actorID string, r domain.PayrollRun, reason string)
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
	publisher  Publisher
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
}

func New(store Store, publisher Publisher, authz AuthZClient, empClient EmployeeClient, ctrClient ContractClient, compClient CompensationClient, defaultTaxRate float64, log *zap.Logger) *Handler {
	return &Handler{
		store:          store,
		publisher:      publisher,
		authz:          authz,
		empClient:      empClient,
		ctrClient:      ctrClient,
		compClient:     compClient,
		defaultTaxRate: defaultTaxRate,
		log:            log,
	}
}

func RegisterRoutes(r chi.Router, h *Handler) {
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
		Status:         "INITIATED",
		IsShadowRun:    req.IsShadowRun,
		CorrelationID:  req.CorrelationID,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	created, err := h.store.CreatePayrollRun(r.Context(), payrollRun)
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

	correlationID := getCorrelationID(r)
	h.publisher.PublishRunInitiated(r.Context(), correlationID, principalID, *payrollRun)

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

	if run.Status == "COMPLETED" {
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
		contracts := make(map[string]*contract.ActiveContract, len(activeEmployees))
		var missingContracts []string
		for _, emp := range activeEmployees {
			if h.ctrClient == nil {
				missingContracts = append(missingContracts, emp.EmployeeID)
				continue
			}
			ctr, err := h.ctrClient.GetActiveContract(r.Context(), tenantID, principalID, emp.EmployeeID)
			if err != nil || ctr == nil || ctr.BaseSalaryAmount <= 0 {
				h.log.Error("failed to verify active salary contract for employee",
					zap.String("employee_id", emp.EmployeeID), zap.Error(err))
				missingContracts = append(missingContracts, emp.EmployeeID)
				continue
			}
			contracts[emp.EmployeeID] = ctr
		}
		if len(missingContracts) > 0 {
			h.publisher.PublishRunBlocked(r.Context(), getCorrelationID(r), principalID, *run, domain.ErrContractLookupFailed.Error())
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error_code":         "contract_lookup_failed",
				"error_message":      domain.ErrContractLookupFailed.Error(),
				"employees_affected": missingContracts,
			})
			return
		}

		// Resolve each employee's compensation breakdown. The base salary still
		// comes from the contract above — that is the authoritative figure.
		// compensation-svc supplies only the composition layered on it.
		//
		// An employee with no structure configured is a normal state: they are
		// paid a flat base salary. compensation-svc being unreachable is not,
		// and blocks the run rather than inventing deductions.
		breakdowns := make(map[string]*compensation.Breakdown, len(activeEmployees))
		for _, emp := range activeEmployees {
			if h.compClient == nil {
				continue
			}
			bd, err := h.compClient.GetBreakdown(r.Context(), tenantID, principalID, emp.EmployeeID, contracts[emp.EmployeeID].BaseSalaryAmount)
			if errors.Is(err, compensation.ErrNoStructure) {
				continue
			}
			if err != nil {
				h.log.Error("failed to resolve compensation breakdown",
					zap.String("employee_id", emp.EmployeeID), zap.Error(err))
				h.publisher.PublishRunBlocked(r.Context(), getCorrelationID(r), principalID, *run, domain.ErrCompensationLookupFailed.Error())
				writeError(w, http.StatusServiceUnavailable, "compensation_lookup_failed", string(domain.ErrCompensationLookupFailed))
				return
			}
			breakdowns[emp.EmployeeID] = bd
		}

		for _, emp := range activeEmployees {
			ctr := contracts[emp.EmployeeID]
			curr := "USD"
			if ctr.Currency != "" {
				curr = ctr.Currency
			}

			slipCalc := h.calculateSlip(ctr.BaseSalaryAmount, breakdowns[emp.EmployeeID])
			gross := slipCalc.gross
			tax := slipCalc.tax
			benefits := slipCalc.deductions
			net := slipCalc.net
			if slipCalc.currency != "" {
				curr = slipCalc.currency
			}

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
				Items:              buildSlipItems(tenantID, slipID, now, breakdowns[emp.EmployeeID]),
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

	if err := h.store.SaveCalculatedResults(r.Context(), id, totalGross, totalNet, totalTax, totalDeductions, slips, shadowComps); err != nil {
		h.log.Error("failed to save calculated payroll results", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	updatedRun, _ := h.store.GetPayrollRun(r.Context(), id)
	correlationID := getCorrelationID(r)
	h.publisher.PublishRunCalculated(r.Context(), correlationID, principalID, *updatedRun)

	writeJSON(w, http.StatusOK, map[string]any{
		"run":                updatedRun,
		"pay_slips":          slips,
		"shadow_comparisons": shadowComps,
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

	if err := h.store.FinalizePayrollRun(r.Context(), id, req.GovernanceDecisionID); err != nil {
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
	correlationID := getCorrelationID(r)
	h.publisher.PublishRunCompleted(r.Context(), correlationID, principalID, *finalizedRun)

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
