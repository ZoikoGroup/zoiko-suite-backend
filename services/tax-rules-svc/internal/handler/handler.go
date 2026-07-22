package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/tax-rules-svc/internal/authz"
	"zoiko.io/tax-rules-svc/internal/domain"
	"zoiko.io/tax-rules-svc/internal/events"
	"zoiko.io/tax-rules-svc/internal/middleware"
	"zoiko.io/tax-rules-svc/internal/store"
)

type Handler struct {
	store     store.Store
	publisher events.Publisher
	authz     *authz.Client
	logger    *zap.Logger
}

func New(st store.Store, pub events.Publisher, az *authz.Client, logger *zap.Logger) *Handler {
	return &Handler{store: st, publisher: pub, authz: az, logger: logger}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/tax-rules", func(r chi.Router) {
		r.Post("/", h.CreateTaxRule)
		r.Get("/", h.ListTaxRules)
		r.Get("/{id}", h.GetTaxRule)
		r.Put("/{id}", h.UpdateTaxRule)
	})
}

func (h *Handler) CreateTaxRule(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.CreateTaxRuleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.JurisdictionID == "" || req.RuleCode == "" || req.Name == "" || req.Category == "" {
		writeError(w, http.StatusBadRequest, "jurisdiction_id, rule_code, name, and category are required")
		return
	}

	rule := &domain.TaxRule{
		TenantID:           tenantID,
		JurisdictionID:     req.JurisdictionID,
		RuleCode:           req.RuleCode,
		Name:               req.Name,
		Category:           req.Category,
		TaxRatePercentage: req.TaxRatePercentage,
		StandardDeductions: req.StandardDeductions,
		ExemptionsJSON:     req.ExemptionsJSON,
		EffectiveFrom:      req.EffectiveFrom,
		EffectiveTo:        req.EffectiveTo,
		CreatedBy:          req.CreatedBy,
	}

	if err := h.store.CreateTaxRule(r.Context(), rule); err != nil {
		h.logger.Error("create tax rule failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create tax rule")
		return
	}

	_ = h.publisher.Publish(r.Context(), "tax_rule.created", rule.RuleID, tenantID, rule)
	writeJSON(w, http.StatusCreated, rule)
}

func (h *Handler) GetTaxRule(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	rule, err := h.store.GetTaxRule(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrTaxRuleNotFound) {
			writeError(w, http.StatusNotFound, "tax rule not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get tax rule")
		return
	}
	writeJSON(w, http.StatusOK, rule)
}

func (h *Handler) ListTaxRules(w http.ResponseWriter, r *http.Request) {
	jurisdictionID := r.URL.Query().Get("jurisdiction_id")
	category := r.URL.Query().Get("category")
	status := r.URL.Query().Get("status")
	rules, err := h.store.ListTaxRules(r.Context(), jurisdictionID, category, status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list tax rules")
		return
	}
	if rules == nil {
		rules = []domain.TaxRule{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"rules": rules, "total": len(rules)})
}

func (h *Handler) UpdateTaxRule(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	existing, err := h.store.GetTaxRule(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrTaxRuleNotFound) {
			writeError(w, http.StatusNotFound, "tax rule not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch tax rule")
		return
	}

	var req domain.UpdateTaxRuleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Name != "" {
		existing.Name = req.Name
	}
	if req.Category != "" {
		existing.Category = req.Category
	}
	if req.TaxRatePercentage != nil {
		existing.TaxRatePercentage = *req.TaxRatePercentage
	}
	if req.StandardDeductions != nil {
		existing.StandardDeductions = *req.StandardDeductions
	}
	if req.ExemptionsJSON != "" {
		existing.ExemptionsJSON = req.ExemptionsJSON
	}
	if req.Status != "" {
		existing.Status = req.Status
	}
	if req.EffectiveTo != nil {
		existing.EffectiveTo = req.EffectiveTo
	}

	if err := h.store.UpdateTaxRule(r.Context(), existing); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update tax rule")
		return
	}

	_ = h.publisher.Publish(r.Context(), "tax_rule.updated", id, tenantID, existing)
	writeJSON(w, http.StatusOK, existing)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
