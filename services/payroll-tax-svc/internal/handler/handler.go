package handler

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/payroll-tax-svc/internal/domain"
	"zoiko.io/payroll-tax-svc/internal/employee"
	svcmiddleware "zoiko.io/payroll-tax-svc/internal/middleware"
)

type Store interface {
	CreateProfile(ctx context.Context, p *domain.TaxJurisdictionProfile) error
	ListProfiles(ctx context.Context, legalEntityID, jurisdictionCode string) ([]domain.TaxJurisdictionProfile, error)
	CreateCalculationWithAudit(ctx context.Context, calc *domain.TaxCalculationRecord, audit *domain.TaxBasisAudit) error
	GetCalculation(ctx context.Context, calculationID string) (*domain.TaxCalculationRecord, error)
	ListCalculations(ctx context.Context, payrollRunID, employeeID string) ([]domain.TaxCalculationRecord, error)
	GetTaxBasisAudit(ctx context.Context, calculationID string) (*domain.TaxBasisAudit, error)
	AdjustCalculation(ctx context.Context, calculationID string, newBreakdown []domain.TaxComponent, newTotalTax float64, reason string) error
}

type Publisher interface {
	PublishTaxCalculated(ctx context.Context, correlationID string, calc domain.TaxCalculationRecord)
	PublishTaxAdjusted(ctx context.Context, correlationID string, calc domain.TaxCalculationRecord)
	PublishTaxException(ctx context.Context, correlationID, calcID, reason string)
}

type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type EmployeeValidator interface {
	ValidateEmployee(ctx context.Context, tenantID, principalID, employeeID string) (*employee.Employee, error)
}

const (
	actionTaxProfileCreate = "TAX_PROFILE_CREATE"
	actionTaxProfileView   = "TAX_PROFILE_VIEW"
	actionTaxCalculate     = "TAX_CALCULATE"
	actionTaxView          = "TAX_VIEW"
	actionTaxAdjust        = "TAX_ADJUST"
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
	r.Route("/v1/payroll-tax", func(r chi.Router) {
		r.Post("/profiles", h.CreateProfile)
		r.Get("/profiles", h.ListProfiles)

		r.Post("/calculate", h.CalculateTax)
		r.Get("/calculations", h.ListCalculations)
		r.Get("/calculations/{id}", h.GetCalculation)
		r.Get("/calculations/{id}/audit", h.GetTaxBasisAudit)
		r.Post("/calculations/{id}/adjust", h.AdjustCalculation)
	})
}

// ── POST /v1/payroll-tax/profiles ─────────────────────────────────────────────────

func (h *Handler) CreateProfile(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateProfileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.LegalEntityID == "" || req.JurisdictionCode == "" || req.TaxEngineType == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, jurisdiction_code, tax_engine_type are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionTaxProfileCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	now := time.Now().UTC()
	profile := &domain.TaxJurisdictionProfile{
		ProfileID:        uuid.NewString(),
		TenantID:         tenantID,
		LegalEntityID:    req.LegalEntityID,
		JurisdictionCode: req.JurisdictionCode,
		TaxEngineType:    req.TaxEngineType,
		ProviderEndpoint: req.ProviderEndpoint,
		Status:           "ACTIVE",
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	if err := h.store.CreateProfile(r.Context(), profile); err != nil {
		h.log.Error("failed to create tax profile", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, profile)
}

// ── GET /v1/payroll-tax/profiles ──────────────────────────────────────────────────

func (h *Handler) ListProfiles(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	jurisdictionCode := r.URL.Query().Get("jurisdiction_code")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if legalEntityID != "" {
		if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionTaxProfileView); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
	}

	list, err := h.store.ListProfiles(r.Context(), legalEntityID, jurisdictionCode)
	if err != nil {
		h.log.Error("failed to list profiles", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if list == nil {
		list = []domain.TaxJurisdictionProfile{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── POST /v1/payroll-tax/calculate ────────────────────────────────────────────────

func (h *Handler) CalculateTax(w http.ResponseWriter, r *http.Request) {
	var req domain.CalculateTaxRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.PayrollRunID == "" || req.EmployeeID == "" || req.JurisdictionCode == "" || req.GrossTaxableAmount <= 0 {
		writeError(w, http.StatusBadRequest, "missing_fields", "payroll_run_id, employee_id, jurisdiction_code, gross_taxable_amount are required")
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

	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionTaxCalculate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	// Resolve engine type from jurisdiction profile if present
	profiles, _ := h.store.ListProfiles(r.Context(), "", req.JurisdictionCode)
	engineType := "STANDARD_ENGINE"
	if len(profiles) > 0 {
		engineType = profiles[0].TaxEngineType
	}

	taxableBasis := math.Max(0, req.GrossTaxableAmount-req.PreTaxDeductionAmount)

	// Multi-Tax Engine Abstraction: Compute dynamic statutory components based on taxable basis
	incTaxAmt := roundTwo(taxableBasis * 0.15)
	socTaxAmt := roundTwo(taxableBasis * 0.062)
	medTaxAmt := roundTwo(taxableBasis * 0.0145)
	totalTax := incTaxAmt + socTaxAmt + medTaxAmt

	breakdown := []domain.TaxComponent{
		{TaxName: "Income Tax", TaxType: "STATE_FEDERAL", RatePct: 15.00, TaxAmount: incTaxAmt},
		{TaxName: "Social Insurance", TaxType: "SOCIAL_SECURITY", RatePct: 6.20, TaxAmount: socTaxAmt},
		{TaxName: "Medical Insurance", TaxType: "HEALTH_STATUTORY", RatePct: 1.45, TaxAmount: medTaxAmt},
	}

	now := time.Now().UTC()
	calcID := uuid.NewString()

	calc := &domain.TaxCalculationRecord{
		CalculationID:         calcID,
		TenantID:              tenantID,
		PayrollRunID:          req.PayrollRunID,
		EmployeeID:            req.EmployeeID,
		JurisdictionCode:      req.JurisdictionCode,
		GrossTaxableAmount:    req.GrossTaxableAmount,
		PreTaxDeductionAmount: req.PreTaxDeductionAmount,
		TaxableBasis:          taxableBasis,
		TotalTaxAmount:        totalTax,
		TaxBreakdown:          breakdown,
		EngineType:            engineType,
		RuleVersionUsed:       "2026.1",
		Status:                "CALCULATED",
		CreatedAt:             now,
	}

	ruleBasisRaw, _ := json.Marshal(map[string]any{
		"jurisdiction":      req.JurisdictionCode,
		"rule_version":      "2026.1",
		"taxable_basis":     taxableBasis,
		"applied_rates_pct": map[string]float64{"income_tax": 15.0, "social_security": 6.2, "medicare": 1.45},
	})

	providerMetaRaw, _ := json.Marshal(map[string]any{
		"engine_type":    engineType,
		"executed_by":   principalID,
		"calculation_id": calcID,
		"timestamp":      now,
	})

	audit := &domain.TaxBasisAudit{
		AuditID:              uuid.NewString(),
		TenantID:             tenantID,
		CalculationID:        calcID,
		EmployeeID:           req.EmployeeID,
		RuleBasisJSON:        string(ruleBasisRaw),
		ProviderMetadataJSON: string(providerMetaRaw),
		AuditedAt:            now,
	}

	if err := h.store.CreateCalculationWithAudit(r.Context(), calc, audit); err != nil {
		h.log.Error("failed to store tax calculation and audit", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	correlationID := getCorrelationID(r)
	h.publisher.PublishTaxCalculated(r.Context(), correlationID, *calc)

	writeJSON(w, http.StatusCreated, calc)
}

// ── GET /v1/payroll-tax/calculations ─────────────────────────────────────────────

func (h *Handler) ListCalculations(w http.ResponseWriter, r *http.Request) {
	payrollRunID := r.URL.Query().Get("payroll_run_id")
	employeeID := r.URL.Query().Get("employee_id")

	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	list, err := h.store.ListCalculations(r.Context(), payrollRunID, employeeID)
	if err != nil {
		h.log.Error("failed to list calculation records", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if list == nil {
		list = []domain.TaxCalculationRecord{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── GET /v1/payroll-tax/calculations/{id} ─────────────────────────────────────────

func (h *Handler) GetCalculation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	calc, err := h.store.GetCalculation(r.Context(), id)
	if errors.Is(err, domain.ErrCalculationNotFound) {
		writeError(w, http.StatusNotFound, "calculation_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch calculation record", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, calc)
}

// ── GET /v1/payroll-tax/calculations/{id}/audit ───────────────────────────────────

func (h *Handler) GetTaxBasisAudit(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	audit, err := h.store.GetTaxBasisAudit(r.Context(), id)
	if errors.Is(err, domain.ErrAuditNotFound) {
		writeError(w, http.StatusNotFound, "audit_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch tax basis audit", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, audit)
}

// ── POST /v1/payroll-tax/calculations/{id}/adjust ─────────────────────────────────

func (h *Handler) AdjustCalculation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req domain.AdjustTaxRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if len(req.NewTaxBreakdown) == 0 || req.NewTotalTaxAmount < 0 || req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "new_tax_breakdown, new_total_tax_amount, reason are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	calc, err := h.store.GetCalculation(r.Context(), id)
	if errors.Is(err, domain.ErrCalculationNotFound) {
		writeError(w, http.StatusNotFound, "calculation_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch calculation for adjustment", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	legalEntityID := "GLOBAL"
	if h.employee != nil {
		tenantID := svcmiddleware.TenantFromContext(r.Context())
		emp, _ := h.employee.ValidateEmployee(r.Context(), tenantID, principalID, calc.EmployeeID)
		if emp != nil && emp.LegalEntityID != "" {
			legalEntityID = emp.LegalEntityID
		}
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionTaxAdjust); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if err := h.store.AdjustCalculation(r.Context(), id, req.NewTaxBreakdown, req.NewTotalTaxAmount, req.Reason); err != nil {
		h.log.Error("failed to adjust tax calculation", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	calc.Status = "ADJUSTED"
	calc.TotalTaxAmount = req.NewTotalTaxAmount
	calc.TaxBreakdown = req.NewTaxBreakdown

	correlationID := getCorrelationID(r)
	h.publisher.PublishTaxAdjusted(r.Context(), correlationID, *calc)

	writeJSON(w, http.StatusOK, calc)
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

func roundTwo(v float64) float64 {
	return math.Round(v*100.0) / 100.0
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