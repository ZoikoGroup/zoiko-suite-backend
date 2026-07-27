package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/vat-gst-svc/internal/authz"
	"zoiko.io/vat-gst-svc/internal/domain"
	"zoiko.io/vat-gst-svc/internal/events"
	"zoiko.io/vat-gst-svc/internal/middleware"
	"zoiko.io/vat-gst-svc/internal/store"
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
	r.Route("/v1/vat-returns", func(r chi.Router) {
		r.Post("/", h.CreateVATReturn)
		r.Get("/", h.ListVATReturns)
		r.Get("/{id}", h.GetVATReturn)
		r.Put("/{id}", h.UpdateVATReturn)
		r.Post("/{id}/file", h.FileVATReturn)
	})
}

func (h *Handler) CreateVATReturn(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.CreateVATReturnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.JurisdictionID == "" || req.TaxPeriod == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id, jurisdiction_id, and tax_period are required")
		return
	}

	vret := &domain.VATReturn{
		TenantID:              tenantID,
		LegalEntityID:         req.LegalEntityID,
		JurisdictionID:        req.JurisdictionID,
		TaxRegistrationNumber: req.TaxRegistrationNumber,
		TaxPeriod:             req.TaxPeriod,
		TotalSalesAmount:      req.TotalSalesAmount,
		TotalPurchaseAmount:   req.TotalPurchaseAmount,
		OutputTaxAmount:       req.OutputTaxAmount,
		InputTaxAmount:        req.InputTaxAmount,
		Currency:              req.Currency,
		EffectiveFrom:         req.EffectiveFrom,
		CreatedBy:             req.CreatedBy,
	}

	if err := h.store.CreateVATReturn(r.Context(), vret); err != nil {
		h.logger.Error("create vat return failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create vat return")
		return
	}

	_ = h.publisher.Publish(r.Context(), "vat_return.created", vret.ReturnID, tenantID, vret)
	writeJSON(w, http.StatusCreated, vret)
}

func (h *Handler) GetVATReturn(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	vret, err := h.store.GetVATReturn(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrVATReturnNotFound) {
			writeError(w, http.StatusNotFound, "vat return not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get vat return")
		return
	}
	writeJSON(w, http.StatusOK, vret)
}

func (h *Handler) ListVATReturns(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	jurisdictionID := r.URL.Query().Get("jurisdiction_id")
	status := r.URL.Query().Get("status")
	returns, err := h.store.ListVATReturns(r.Context(), legalEntityID, jurisdictionID, status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list vat returns")
		return
	}
	if returns == nil {
		returns = []domain.VATReturn{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"vat_returns": returns, "total": len(returns)})
}

func (h *Handler) UpdateVATReturn(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	existing, err := h.store.GetVATReturn(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrVATReturnNotFound) {
			writeError(w, http.StatusNotFound, "vat return not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch vat return")
		return
	}

	var req domain.CreateVATReturnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	existing.TotalSalesAmount = req.TotalSalesAmount
	existing.TotalPurchaseAmount = req.TotalPurchaseAmount
	existing.OutputTaxAmount = req.OutputTaxAmount
	existing.InputTaxAmount = req.InputTaxAmount

	if err := h.store.UpdateVATReturn(r.Context(), existing); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update vat return")
		return
	}

	_ = h.publisher.Publish(r.Context(), "vat_return.updated", id, tenantID, existing)
	writeJSON(w, http.StatusOK, existing)
}

func (h *Handler) FileVATReturn(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.FileVATReturnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.FiledBy == "" {
		writeError(w, http.StatusBadRequest, "filed_by is required")
		return
	}

	vret, err := h.store.FileVATReturn(r.Context(), id, req.FiledBy)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrVATReturnNotFound):
			writeError(w, http.StatusNotFound, "vat return not found")
		case errors.Is(err, domain.ErrAlreadyFiled):
			writeError(w, http.StatusConflict, "vat return is already filed")
		default:
			writeError(w, http.StatusInternalServerError, "failed to file vat return")
		}
		return
	}

	_ = h.publisher.Publish(r.Context(), "vat_return.filed", id, tenantID, vret)
	writeJSON(w, http.StatusOK, vret)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
