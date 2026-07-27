package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/obligation-tracking-svc/internal/authz"
	"zoiko.io/obligation-tracking-svc/internal/domain"
	"zoiko.io/obligation-tracking-svc/internal/events"
	"zoiko.io/obligation-tracking-svc/internal/middleware"
	"zoiko.io/obligation-tracking-svc/internal/store"
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
	r.Route("/v1/obligations", func(r chi.Router) {
		r.Post("/", h.CreateObligation)
		r.Get("/", h.ListObligations)
		r.Get("/{id}", h.GetObligation)
		r.Put("/{id}", h.UpdateObligation)
		r.Post("/{id}/fulfill", h.FulfillObligation)
	})
}

func (h *Handler) CreateObligation(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.CreateObligationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Title == "" || req.DueDate == "" || req.ObligationType == "" {
		writeError(w, http.StatusBadRequest, "title, due_date, and obligation_type are required")
		return
	}

	o := &domain.Obligation{
		TenantID:       tenantID,
		LegalEntityID:  req.LegalEntityID,
		SourceType:     req.SourceType,
		SourceID:       req.SourceID,
		Title:          req.Title,
		Description:    req.Description,
		ObligationType: req.ObligationType,
		RiskLevel:      req.RiskLevel,
		DueDate:        req.DueDate,
		AssignedTo:     req.AssignedTo,
		EffectiveFrom:  req.EffectiveFrom,
		EffectiveTo:    req.EffectiveTo,
		CreatedBy:      req.CreatedBy,
	}

	if err := h.store.CreateObligation(r.Context(), o); err != nil {
		h.logger.Error("create obligation failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create obligation")
		return
	}

	_ = h.publisher.Publish(r.Context(), "obligation.created", o.ObligationID, tenantID, o)
	writeJSON(w, http.StatusCreated, o)
}

func (h *Handler) GetObligation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	o, err := h.store.GetObligation(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrObligationNotFound) {
			writeError(w, http.StatusNotFound, "obligation not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get obligation")
		return
	}
	writeJSON(w, http.StatusOK, o)
}

func (h *Handler) ListObligations(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	status := r.URL.Query().Get("status")
	sourceType := r.URL.Query().Get("source_type")
	obligations, err := h.store.ListObligations(r.Context(), legalEntityID, status, sourceType)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list obligations")
		return
	}
	if obligations == nil {
		obligations = []domain.Obligation{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"obligations": obligations, "total": len(obligations)})
}

func (h *Handler) UpdateObligation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	existing, err := h.store.GetObligation(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrObligationNotFound) {
			writeError(w, http.StatusNotFound, "obligation not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch obligation")
		return
	}

	var req domain.UpdateObligationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Title != "" {
		existing.Title = req.Title
	}
	if req.Description != "" {
		existing.Description = req.Description
	}
	if req.ObligationType != "" {
		existing.ObligationType = req.ObligationType
	}
	if req.RiskLevel != "" {
		existing.RiskLevel = req.RiskLevel
	}
	if req.DueDate != "" {
		existing.DueDate = req.DueDate
	}
	if req.AssignedTo != "" {
		existing.AssignedTo = req.AssignedTo
	}
	if req.Status != "" {
		existing.Status = req.Status
	}
	if req.EffectiveTo != nil {
		existing.EffectiveTo = req.EffectiveTo
	}

	if err := h.store.UpdateObligation(r.Context(), existing); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update obligation")
		return
	}

	_ = h.publisher.Publish(r.Context(), "obligation.updated", id, tenantID, existing)
	writeJSON(w, http.StatusOK, existing)
}

func (h *Handler) FulfillObligation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.FulfillObligationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.FulfilledBy == "" {
		writeError(w, http.StatusBadRequest, "fulfilled_by is required")
		return
	}

	o, err := h.store.FulfillObligation(r.Context(), id, &req)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrObligationNotFound):
			writeError(w, http.StatusNotFound, "obligation not found")
		case errors.Is(err, domain.ErrObligationAlreadyFulfilled):
			writeError(w, http.StatusConflict, "obligation is already fulfilled")
		default:
			writeError(w, http.StatusInternalServerError, "failed to fulfill obligation")
		}
		return
	}

	_ = h.publisher.Publish(r.Context(), "obligation.fulfilled", id, tenantID, o)
	writeJSON(w, http.StatusOK, o)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
