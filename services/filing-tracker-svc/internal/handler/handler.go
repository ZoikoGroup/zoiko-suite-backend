package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/filing-tracker-svc/internal/authz"
	"zoiko.io/filing-tracker-svc/internal/domain"
	"zoiko.io/filing-tracker-svc/internal/events"
	"zoiko.io/filing-tracker-svc/internal/middleware"
	"zoiko.io/filing-tracker-svc/internal/store"
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
	r.Route("/v1/filing-tracker/requirements", func(r chi.Router) {
		r.Post("/", h.Create)
		r.Get("/", h.List)
		r.Get("/{id}", h.GetByID)
		r.Put("/{id}", h.Update)
		r.Post("/{id}/submit", h.Submit)
		r.Post("/{id}/confirm", h.Confirm)
		r.Post("/{id}/mark-overdue", h.MarkOverdue)
	})
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.CreateRequirementRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.JurisdictionID == "" || req.FilingAuthority == "" || req.DueDate == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id, jurisdiction_id, filing_authority, and due_date are required")
		return
	}

	f := &domain.FilingRequirement{
		TenantID:        tenantID,
		LegalEntityID:   req.LegalEntityID,
		JurisdictionID:  req.JurisdictionID,
		FilingAuthority: req.FilingAuthority,
		FilingType:      req.FilingType,
		PeriodKey:       req.PeriodKey,
		DueDate:         req.DueDate,
		Notes:           req.Notes,
		CreatedBy:       req.CreatedBy,
	}

	if err := h.store.Create(r.Context(), f); err != nil {
		h.logger.Error("create filing requirement failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create filing requirement")
		return
	}
	_ = h.publisher.Publish(r.Context(), "filing.due", f.FilingID, tenantID, f)
	writeJSON(w, http.StatusCreated, f)
}

func (h *Handler) GetByID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	f, err := h.store.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrRequirementNotFound) {
			writeError(w, http.StatusNotFound, "filing requirement not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get filing requirement")
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	jurisdictionID := r.URL.Query().Get("jurisdiction_id")
	filingAuthority := r.URL.Query().Get("filing_authority")
	status := r.URL.Query().Get("status")

	requirements, err := h.store.List(r.Context(), legalEntityID, jurisdictionID, filingAuthority, status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list filing requirements")
		return
	}
	if requirements == nil {
		requirements = []domain.FilingRequirement{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"filing_requirements": requirements,
		"total":               len(requirements),
	})
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	existing, err := h.store.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrRequirementNotFound) {
			writeError(w, http.StatusNotFound, "filing requirement not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch filing requirement")
		return
	}

	var req domain.CreateRequirementRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Notes != "" {
		existing.Notes = req.Notes
	}

	if err := h.store.Update(r.Context(), existing); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update filing requirement")
		return
	}
	_ = h.publisher.Publish(r.Context(), "filing.requirement.updated", id, tenantID, existing)
	writeJSON(w, http.StatusOK, existing)
}

func (h *Handler) Submit(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.SubmitFilingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.SubmissionReference == "" || req.SubmittedBy == "" {
		writeError(w, http.StatusBadRequest, "submission_reference and submitted_by are required")
		return
	}

	f, err := h.store.Submit(r.Context(), id, &req)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrRequirementNotFound):
			writeError(w, http.StatusNotFound, "filing requirement not found")
		case errors.Is(err, domain.ErrAlreadySubmitted):
			writeError(w, http.StatusConflict, "filing requirement is already submitted")
		default:
			writeError(w, http.StatusInternalServerError, "failed to submit filing requirement")
		}
		return
	}
	_ = h.publisher.Publish(r.Context(), "filing.submitted", id, tenantID, f)
	writeJSON(w, http.StatusOK, f)
}

func (h *Handler) Confirm(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.ConfirmFilingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ConfirmationReference == "" {
		writeError(w, http.StatusBadRequest, "confirmation_reference is required")
		return
	}

	f, err := h.store.Confirm(r.Context(), id, &req)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrRequirementNotFound):
			writeError(w, http.StatusNotFound, "filing requirement not found")
		case errors.Is(err, domain.ErrAlreadyConfirmed):
			writeError(w, http.StatusConflict, "filing requirement is already confirmed")
		default:
			writeError(w, http.StatusInternalServerError, "failed to confirm filing requirement")
		}
		return
	}
	_ = h.publisher.Publish(r.Context(), "filing.confirmed", id, tenantID, f)
	writeJSON(w, http.StatusOK, f)
}

func (h *Handler) MarkOverdue(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	todayStr := time.Now().Format("2006-01-02")
	f, err := h.store.MarkOverdue(r.Context(), id, todayStr)
	if err != nil {
		if errors.Is(err, domain.ErrRequirementNotFound) {
			writeError(w, http.StatusNotFound, "filing requirement not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to mark filing requirement overdue")
		return
	}

	if f.Status == domain.StatusOverdue {
		_ = h.publisher.Publish(r.Context(), "filing.overdue", id, tenantID, f)
	}
	writeJSON(w, http.StatusOK, f)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
