package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/corporate-actions-svc/internal/authz"
	"zoiko.io/corporate-actions-svc/internal/domain"
	"zoiko.io/corporate-actions-svc/internal/events"
	"zoiko.io/corporate-actions-svc/internal/middleware"
	"zoiko.io/corporate-actions-svc/internal/store"
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
	r.Route("/v1/corporate-actions", func(r chi.Router) {
		r.Post("/", h.CreateAction)
		r.Get("/", h.ListActions)
		r.Get("/{id}", h.GetAction)
		r.Put("/{id}", h.UpdateAction)
		r.Post("/{id}/execute", h.ExecuteAction)
	})
}

func (h *Handler) CreateAction(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.CreateCorporateActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Title == "" || req.ActionType == "" || req.EffectiveDate == "" {
		writeError(w, http.StatusBadRequest, "title, action_type, and effective_date are required")
		return
	}

	a := &domain.CorporateAction{
		TenantID:        tenantID,
		LegalEntityID:   req.LegalEntityID,
		Title:           req.Title,
		ActionType:      req.ActionType,
		Description:     req.Description,
		ResolutionID:    req.ResolutionID,
		EffectiveDate:   req.EffectiveDate,
		ValuationAmount: req.ValuationAmount,
		Currency:        req.Currency,
		EffectiveFrom:   req.EffectiveFrom,
		CreatedBy:       req.CreatedBy,
	}

	if err := h.store.CreateAction(r.Context(), a); err != nil {
		h.logger.Error("create corporate action failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create corporate action")
		return
	}

	_ = h.publisher.Publish(r.Context(), "corporate_action.created", a.ActionID, tenantID, a)
	writeJSON(w, http.StatusCreated, a)
}

func (h *Handler) GetAction(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	a, err := h.store.GetAction(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrCorporateActionNotFound) {
			writeError(w, http.StatusNotFound, "corporate action not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get corporate action")
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (h *Handler) ListActions(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	actionType := r.URL.Query().Get("action_type")
	status := r.URL.Query().Get("status")
	actions, err := h.store.ListActions(r.Context(), legalEntityID, actionType, status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list corporate actions")
		return
	}
	if actions == nil {
		actions = []domain.CorporateAction{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"actions": actions, "total": len(actions)})
}

func (h *Handler) UpdateAction(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	existing, err := h.store.GetAction(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrCorporateActionNotFound) {
			writeError(w, http.StatusNotFound, "corporate action not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch corporate action")
		return
	}

	var req domain.UpdateCorporateActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Title != "" {
		existing.Title = req.Title
	}
	if req.ActionType != "" {
		existing.ActionType = req.ActionType
	}
	if req.Description != "" {
		existing.Description = req.Description
	}
	if req.ResolutionID != "" {
		existing.ResolutionID = req.ResolutionID
	}
	if req.EffectiveDate != "" {
		existing.EffectiveDate = req.EffectiveDate
	}
	if req.Status != "" {
		existing.Status = req.Status
	}
	if req.ValuationAmount > 0 {
		existing.ValuationAmount = req.ValuationAmount
	}
	if req.Currency != "" {
		existing.Currency = req.Currency
	}
	if req.EffectiveTo != nil {
		existing.EffectiveTo = req.EffectiveTo
	}

	if err := h.store.UpdateAction(r.Context(), existing); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update corporate action")
		return
	}

	_ = h.publisher.Publish(r.Context(), "corporate_action.updated", id, tenantID, existing)
	writeJSON(w, http.StatusOK, existing)
}

func (h *Handler) ExecuteAction(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.ExecuteCorporateActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ExecutedBy == "" {
		writeError(w, http.StatusBadRequest, "executed_by is required")
		return
	}

	a, err := h.store.ExecuteAction(r.Context(), id, &req)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrCorporateActionNotFound):
			writeError(w, http.StatusNotFound, "corporate action not found")
		case errors.Is(err, domain.ErrActionAlreadyExecuted):
			writeError(w, http.StatusConflict, "corporate action is already executed")
		default:
			writeError(w, http.StatusInternalServerError, "failed to execute corporate action")
		}
		return
	}

	_ = h.publisher.Publish(r.Context(), "corporate_action.executed", id, tenantID, a)
	writeJSON(w, http.StatusOK, a)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
