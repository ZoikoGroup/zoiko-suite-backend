package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/benefits-svc/internal/domain"
	"zoiko.io/benefits-svc/internal/employee"
	svcmiddleware "zoiko.io/benefits-svc/internal/middleware"
)

type Store interface {
	CreatePlan(ctx context.Context, p *domain.BenefitPlan) error
	ListPlans(ctx context.Context, legalEntityID, status string) ([]domain.BenefitPlan, error)
	GetPlan(ctx context.Context, planID string) (*domain.BenefitPlan, error)
	CreateElection(ctx context.Context, e *domain.BenefitElection) error
	UpdateElection(ctx context.Context, e *domain.BenefitElection) error
	CancelElection(ctx context.Context, electionID, cancelDate string) error
	ListElectionsByEmployee(ctx context.Context, employeeID, status string) ([]domain.BenefitElection, error)
}

type Publisher interface {
	PublishBenefitEnrolled(ctx context.Context, correlationID string, e domain.BenefitElection)
	PublishBenefitChanged(ctx context.Context, correlationID string, e domain.BenefitElection)
	PublishBenefitTerminated(ctx context.Context, correlationID string, e domain.BenefitElection)
}

type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type EmployeeValidator interface {
	ValidateEmployee(ctx context.Context, tenantID, principalID, employeeID string) (*employee.Employee, error)
}

const (
	actionBenefitsCreate = "BENEFITS_CREATE"
	actionBenefitsView   = "BENEFITS_VIEW"
	actionBenefitsEnroll = "BENEFITS_ENROLL"
	actionBenefitsUpdate = "BENEFITS_UPDATE"
	actionBenefitsCancel = "BENEFITS_CANCEL"
)

type Handler struct {
	store     Store
	publisher Publisher
	authz     AuthZClient
	employee  EmployeeValidator
	log       *zap.Logger
}

func New(store Store, publisher Publisher, authz AuthZClient, empValidator EmployeeValidator, log *zap.Logger) *Handler {
	return &Handler{
		store:     store,
		publisher: publisher,
		authz:     authz,
		employee:  empValidator,
		log:       log,
	}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/benefits", func(r chi.Router) {
		r.Post("/plans", h.CreatePlan)
		r.Get("/plans", h.ListPlans)

		r.Post("/elections", h.EnrollBenefit)
		r.Put("/elections/{id}", h.UpdateElection)
		r.Post("/elections/{id}/cancel", h.CancelElection)
		r.Get("/elections/employee/{employee_id}", h.GetEmployeeElections)

		r.Get("/deductions/employee/{employee_id}", h.GetDeductionSummary)
	})
}

// ── POST /v1/benefits/plans ───────────────────────────────────────────────────────

func (h *Handler) CreatePlan(w http.ResponseWriter, r *http.Request) {
	var req domain.CreatePlanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.LegalEntityID == "" || req.Name == "" || req.PlanType == "" || req.ProviderName == "" || req.DeductionTaxTreatment == "" || req.Currency == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, name, plan_type, provider_name, deduction_tax_treatment, currency are required")
		return
	}

	if req.DeductionTaxTreatment != "PRE_TAX" && req.DeductionTaxTreatment != "POST_TAX" {
		writeError(w, http.StatusBadRequest, "invalid_tax_treatment", "deduction_tax_treatment must be PRE_TAX or POST_TAX")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionBenefitsCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	empPct := 0.0
	if req.EmployerContributionPct != nil && *req.EmployerContributionPct >= 0 {
		empPct = *req.EmployerContributionPct
	}
	eeAmt := 0.0
	if req.EmployeeContributionAmount != nil && *req.EmployeeContributionAmount >= 0 {
		eeAmt = *req.EmployeeContributionAmount
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	now := time.Now().UTC()
	p := &domain.BenefitPlan{
		PlanID:                      uuid.NewString(),
		TenantID:                    tenantID,
		LegalEntityID:               req.LegalEntityID,
		Name:                        req.Name,
		PlanType:                    req.PlanType,
		ProviderName:                req.ProviderName,
		DeductionTaxTreatment:      req.DeductionTaxTreatment,
		EmployerContributionPct:    empPct,
		EmployeeContributionAmount: eeAmt,
		Currency:                    req.Currency,
		Status:                      "ACTIVE",
		CreatedAt:                   now,
		UpdatedAt:                   now,
	}

	if err := h.store.CreatePlan(r.Context(), p); err != nil {
		h.log.Error("failed to create benefit plan", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, p)
}

// ── GET /v1/benefits/plans ────────────────────────────────────────────────────────

func (h *Handler) ListPlans(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	status := r.URL.Query().Get("status")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if legalEntityID != "" {
		if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionBenefitsView); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
	}

	list, err := h.store.ListPlans(r.Context(), legalEntityID, status)
	if err != nil {
		h.log.Error("failed to list benefit plans", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if list == nil {
		list = []domain.BenefitPlan{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── POST /v1/benefits/elections ───────────────────────────────────────────────────

func (h *Handler) EnrollBenefit(w http.ResponseWriter, r *http.Request) {
	var req domain.EnrollBenefitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.EmployeeID == "" || req.PlanID == "" || req.CoverageLevel == "" || req.EffectiveFrom == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "employee_id, plan_id, coverage_level, effective_from are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	legalEntityID := "GLOBAL"

	if h.employee != nil {
		emp, err := h.employee.ValidateEmployee(r.Context(), tenantID, principalID, req.EmployeeID)
		if err != nil {
			if errors.Is(err, domain.ErrEmployeeNotFound) {
				writeError(w, http.StatusBadRequest, "employee_invalid", err.Error())
				return
			}
			h.log.Warn("employee validation call failed, proceeding", zap.Error(err))
		} else if emp != nil && emp.LegalEntityID != "" {
			legalEntityID = emp.LegalEntityID
		}
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionBenefitsEnroll); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	plan, err := h.store.GetPlan(r.Context(), req.PlanID)
	if errors.Is(err, domain.ErrPlanNotFound) {
		writeError(w, http.StatusBadRequest, "plan_not_found", err.Error())
		return
	}
	if err != nil {
		h.log.Error("failed to get plan during enrollment", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	eeContribution := plan.EmployeeContributionAmount
	if req.EmployeeContributionAmount != nil && *req.EmployeeContributionAmount >= 0 {
		eeContribution = *req.EmployeeContributionAmount
	}
	erContribution := (eeContribution * plan.EmployerContributionPct) / 100.0

	now := time.Now().UTC()
	election := &domain.BenefitElection{
		ElectionID:                 uuid.NewString(),
		TenantID:                   tenantID,
		EmployeeID:                 req.EmployeeID,
		PlanID:                     req.PlanID,
		CoverageLevel:              req.CoverageLevel,
		EmployeeContributionAmount: eeContribution,
		EmployerContributionAmount: erContribution,
		EffectiveFrom:              req.EffectiveFrom,
		Status:                     "ACTIVE",
		CreatedAt:                  now,
		UpdatedAt:                  now,
	}

	if err := h.store.CreateElection(r.Context(), election); err != nil {
		h.log.Error("failed to create benefit election", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	correlationID := getCorrelationID(r)
	h.publisher.PublishBenefitEnrolled(r.Context(), correlationID, *election)

	writeJSON(w, http.StatusCreated, election)
}

// ── PUT /v1/benefits/elections/{id} ───────────────────────────────────────────────

func (h *Handler) UpdateElection(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req domain.UpdateElectionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	elections, err := h.store.ListElectionsByEmployee(r.Context(), "", "ACTIVE")
	if err != nil {
		h.log.Error("failed to fetch active election for update", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	var target *domain.BenefitElection
	for _, e := range elections {
		if e.ElectionID == id {
			target = &e
			break
		}
	}

	if target == nil {
		writeError(w, http.StatusNotFound, "election_not_found", "")
		return
	}

	legalEntityID := "GLOBAL"
	if h.employee != nil {
		tenantID := svcmiddleware.TenantFromContext(r.Context())
		emp, _ := h.employee.ValidateEmployee(r.Context(), tenantID, principalID, target.EmployeeID)
		if emp != nil && emp.LegalEntityID != "" {
			legalEntityID = emp.LegalEntityID
		}
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionBenefitsUpdate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if req.CoverageLevel != "" {
		target.CoverageLevel = req.CoverageLevel
	}
	if req.EmployeeContributionAmount != nil && *req.EmployeeContributionAmount >= 0 {
		target.EmployeeContributionAmount = *req.EmployeeContributionAmount

		// Recompute employer contribution based on plan
		if plan, err := h.store.GetPlan(r.Context(), target.PlanID); err == nil && plan != nil {
			target.EmployerContributionAmount = (target.EmployeeContributionAmount * plan.EmployerContributionPct) / 100.0
		}
	}
	target.UpdatedAt = time.Now().UTC()

	if err := h.store.UpdateElection(r.Context(), target); err != nil {
		h.log.Error("failed to update benefit election", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	correlationID := getCorrelationID(r)
	h.publisher.PublishBenefitChanged(r.Context(), correlationID, *target)

	writeJSON(w, http.StatusOK, target)
}

// ── POST /v1/benefits/elections/{id}/cancel ───────────────────────────────────────

func (h *Handler) CancelElection(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	elections, err := h.store.ListElectionsByEmployee(r.Context(), "", "")
	if err != nil {
		h.log.Error("failed to fetch election for cancel", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	var target *domain.BenefitElection
	for _, e := range elections {
		if e.ElectionID == id {
			target = &e
			break
		}
	}

	if target == nil {
		writeError(w, http.StatusNotFound, "election_not_found", "")
		return
	}

	legalEntityID := "GLOBAL"
	if h.employee != nil {
		tenantID := svcmiddleware.TenantFromContext(r.Context())
		emp, _ := h.employee.ValidateEmployee(r.Context(), tenantID, principalID, target.EmployeeID)
		if emp != nil && emp.LegalEntityID != "" {
			legalEntityID = emp.LegalEntityID
		}
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionBenefitsCancel); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	cancelDate := time.Now().UTC().Format("2006-01-02")
	if err := h.store.CancelElection(r.Context(), id, cancelDate); err != nil {
		if errors.Is(err, domain.ErrElectionAlreadyCancelled) {
			writeError(w, http.StatusConflict, "already_cancelled", err.Error())
			return
		}
		h.log.Error("failed to cancel benefit election", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	target.Status = "CANCELLED"
	target.EffectiveTo = &cancelDate

	correlationID := getCorrelationID(r)
	h.publisher.PublishBenefitTerminated(r.Context(), correlationID, *target)

	writeJSON(w, http.StatusOK, target)
}

// ── GET /v1/benefits/elections/employee/{employee_id} ─────────────────────────────

func (h *Handler) GetEmployeeElections(w http.ResponseWriter, r *http.Request) {
	employeeID := chi.URLParam(r, "employee_id")
	status := r.URL.Query().Get("status")

	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	list, err := h.store.ListElectionsByEmployee(r.Context(), employeeID, status)
	if err != nil {
		h.log.Error("failed to list employee elections", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if list == nil {
		list = []domain.BenefitElection{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── GET /v1/benefits/deductions/employee/{employee_id} ───────────────────────────

func (h *Handler) GetDeductionSummary(w http.ResponseWriter, r *http.Request) {
	employeeID := chi.URLParam(r, "employee_id")

	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	elections, err := h.store.ListElectionsByEmployee(r.Context(), employeeID, "ACTIVE")
	if err != nil {
		h.log.Error("failed to fetch active elections for deduction summary", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	var preTaxTotal, postTaxTotal, erTotal float64
	currency := "USD"

	for _, e := range elections {
		erTotal += e.EmployerContributionAmount

		plan, err := h.store.GetPlan(r.Context(), e.PlanID)
		if err == nil && plan != nil {
			currency = plan.Currency
			if plan.DeductionTaxTreatment == "PRE_TAX" {
				preTaxTotal += e.EmployeeContributionAmount
			} else {
				postTaxTotal += e.EmployeeContributionAmount
			}
		} else {
			postTaxTotal += e.EmployeeContributionAmount
		}
	}

	summary := domain.DeductionSummary{
		EmployeeID:                employeeID,
		PreTaxDeductionTotal:      preTaxTotal,
		PostTaxDeductionTotal:     postTaxTotal,
		EmployerContributionTotal: erTotal,
		Currency:                  currency,
		ElectionsCount:            len(elections),
	}

	writeJSON(w, http.StatusOK, summary)
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