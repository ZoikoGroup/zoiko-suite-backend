package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/corporate-tax-svc/internal/authz"
	"zoiko.io/corporate-tax-svc/internal/domain"
	"zoiko.io/corporate-tax-svc/internal/events"
	"zoiko.io/corporate-tax-svc/internal/middleware"
	"zoiko.io/corporate-tax-svc/internal/store"
)

// Handler wires together the HTTP layer with the store and event bus.
type Handler struct {
	store     store.Store
	publisher events.Publisher
	authz     *authz.Client
	logger    *zap.Logger
}

func New(st store.Store, pub events.Publisher, az *authz.Client, logger *zap.Logger) *Handler {
	return &Handler{store: st, publisher: pub, authz: az, logger: logger}
}

// RegisterRoutes mounts all corporate-tax endpoints under a chi Router.
func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/corporate-tax-returns", func(r chi.Router) {
		r.Post("/", h.Create)
		r.Get("/", h.List)
		r.Get("/{id}", h.GetByID)
		r.Put("/{id}", h.Update)
		r.Post("/{id}/submit", h.Submit)
		r.Post("/{id}/assess", h.Assess)
	})
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.CreateTaxReturnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.JurisdictionID == "" || req.FiscalYear == 0 {
		writeError(w, http.StatusBadRequest, "legal_entity_id, jurisdiction_id, and fiscal_year are required")
		return
	}

	ret := &domain.TaxReturn{
		TenantID:              tenantID,
		LegalEntityID:         req.LegalEntityID,
		JurisdictionID:        req.JurisdictionID,
		TaxRegistrationNumber: req.TaxRegistrationNumber,
		FiscalYear:            req.FiscalYear,
		AccountingPeriodStart: req.AccountingPeriodStart,
		AccountingPeriodEnd:   req.AccountingPeriodEnd,
		GrossRevenue:          req.GrossRevenue,
		AllowableDeductions:   req.AllowableDeductions,
		TaxRatePercent:        req.TaxRatePercent,
		TaxCredits:            req.TaxCredits,
		TaxAlreadyPaid:        req.TaxAlreadyPaid,
		Currency:              req.Currency,
		Notes:                 req.Notes,
		EffectiveFrom:         req.EffectiveFrom,
		CreatedBy:             req.CreatedBy,
	}

	if err := h.store.Create(r.Context(), ret); err != nil {
		h.logger.Error("create corporate tax return failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create corporate tax return")
		return
	}
	_ = h.publisher.Publish(r.Context(), "corporate_tax_return.created", ret.ReturnID, tenantID, ret)
	writeJSON(w, http.StatusCreated, ret)
}

func (h *Handler) GetByID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	ret, err := h.store.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrTaxReturnNotFound) {
			writeError(w, http.StatusNotFound, "corporate tax return not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get corporate tax return")
		return
	}
	writeJSON(w, http.StatusOK, ret)
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	jurisdictionID := r.URL.Query().Get("jurisdiction_id")
	status := r.URL.Query().Get("status")
	fiscalYear, _ := strconv.Atoi(r.URL.Query().Get("fiscal_year"))

	returns, err := h.store.List(r.Context(), legalEntityID, jurisdictionID, status, fiscalYear)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list corporate tax returns")
		return
	}
	if returns == nil {
		returns = []domain.TaxReturn{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"corporate_tax_returns": returns,
		"total":                 len(returns),
	})
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	existing, err := h.store.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrTaxReturnNotFound) {
			writeError(w, http.StatusNotFound, "corporate tax return not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch corporate tax return")
		return
	}

	var req domain.CreateTaxReturnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	existing.GrossRevenue = req.GrossRevenue
	existing.AllowableDeductions = req.AllowableDeductions
	existing.TaxRatePercent = req.TaxRatePercent
	existing.TaxCredits = req.TaxCredits
	existing.TaxAlreadyPaid = req.TaxAlreadyPaid
	if req.Notes != "" {
		existing.Notes = req.Notes
	}

	if err := h.store.Update(r.Context(), existing); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update corporate tax return")
		return
	}
	_ = h.publisher.Publish(r.Context(), "corporate_tax_return.updated", id, tenantID, existing)
	writeJSON(w, http.StatusOK, existing)
}

func (h *Handler) Submit(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.SubmitTaxReturnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.SubmittedBy == "" {
		writeError(w, http.StatusBadRequest, "submitted_by is required")
		return
	}

	ret, err := h.store.Submit(r.Context(), id, req.SubmittedBy)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrTaxReturnNotFound):
			writeError(w, http.StatusNotFound, "corporate tax return not found")
		case errors.Is(err, domain.ErrAlreadySubmitted):
			writeError(w, http.StatusConflict, "corporate tax return is already submitted")
		default:
			writeError(w, http.StatusInternalServerError, "failed to submit corporate tax return")
		}
		return
	}
	_ = h.publisher.Publish(r.Context(), "corporate_tax_return.submitted", id, tenantID, ret)
	writeJSON(w, http.StatusOK, ret)
}

func (h *Handler) Assess(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.AssessTaxReturnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.AssessmentReference == "" {
		writeError(w, http.StatusBadRequest, "assessment_reference is required")
		return
	}

	ret, err := h.store.Assess(r.Context(), id, &req)
	if err != nil {
		if errors.Is(err, domain.ErrTaxReturnNotFound) {
			writeError(w, http.StatusNotFound, "corporate tax return not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to record assessment")
		return
	}
	_ = h.publisher.Publish(r.Context(), "corporate_tax_return.assessed", id, tenantID, ret)
	writeJSON(w, http.StatusOK, ret)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
