package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/filing-preparation-svc/internal/authz"
	"zoiko.io/filing-preparation-svc/internal/domain"
	"zoiko.io/filing-preparation-svc/internal/events"
	"zoiko.io/filing-preparation-svc/internal/middleware"
	"zoiko.io/filing-preparation-svc/internal/store"
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
	r.Route("/v1/filing-preparation/drafts", func(r chi.Router) {
		r.Post("/", h.Create)
		r.Get("/", h.List)
		r.Get("/{id}", h.GetByID)
		r.Put("/{id}", h.Update)
		r.Post("/{id}/validate", h.Validate)
		r.Post("/{id}/finalize", h.Finalize)
	})
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.CreateDraftRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.JurisdictionID == "" || req.PeriodKey == "" || req.DueDate == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id, jurisdiction_id, period_key, and due_date are required")
		return
	}

	d := &domain.FilingDraft{
		TenantID:            tenantID,
		LegalEntityID:       req.LegalEntityID,
		JurisdictionID:      req.JurisdictionID,
		FilingType:          req.FilingType,
		PeriodKey:           req.PeriodKey,
		DueDate:             req.DueDate,
		PayloadData:         req.PayloadData,
		EvidenceManifestRef: req.EvidenceManifestRef,
		Notes:               req.Notes,
		CreatedBy:           req.CreatedBy,
	}

	if err := h.store.Create(r.Context(), d); err != nil {
		h.logger.Error("create filing draft failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create filing draft")
		return
	}
	_ = h.publisher.Publish(r.Context(), "filing.draft.created", d.DraftID, tenantID, d)
	writeJSON(w, http.StatusCreated, d)
}

func (h *Handler) GetByID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	d, err := h.store.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrDraftNotFound) {
			writeError(w, http.StatusNotFound, "filing draft not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get filing draft")
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	jurisdictionID := r.URL.Query().Get("jurisdiction_id")
	filingType := r.URL.Query().Get("filing_type")
	status := r.URL.Query().Get("status")

	drafts, err := h.store.List(r.Context(), legalEntityID, jurisdictionID, filingType, status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list filing drafts")
		return
	}
	if drafts == nil {
		drafts = []domain.FilingDraft{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"filing_drafts": drafts,
		"total":         len(drafts),
	})
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	existing, err := h.store.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrDraftNotFound) {
			writeError(w, http.StatusNotFound, "filing draft not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch filing draft")
		return
	}

	var req domain.CreateDraftRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.PayloadData != "" {
		existing.PayloadData = req.PayloadData
	}
	if req.EvidenceManifestRef != "" {
		existing.EvidenceManifestRef = req.EvidenceManifestRef
	}
	if req.Notes != "" {
		existing.Notes = req.Notes
	}

	if err := h.store.Update(r.Context(), existing); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update filing draft")
		return
	}
	_ = h.publisher.Publish(r.Context(), "filing.draft.updated", id, tenantID, existing)
	writeJSON(w, http.StatusOK, existing)
}

func (h *Handler) Validate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.ValidateDraftRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	d, err := h.store.Validate(r.Context(), id, &req)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrDraftNotFound):
			writeError(w, http.StatusNotFound, "filing draft not found")
		case errors.Is(err, domain.ErrDraftAlreadyFinal):
			writeError(w, http.StatusConflict, "filing draft is already finalized")
		default:
			writeError(w, http.StatusInternalServerError, "failed to validate filing draft")
		}
		return
	}

	eventType := "filing.prepared"
	if d.ValidationStatus == domain.StatusBlocked {
		eventType = "filing.blocked"
	}
	_ = h.publisher.Publish(r.Context(), eventType, id, tenantID, d)
	writeJSON(w, http.StatusOK, d)
}

func (h *Handler) Finalize(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.FinalizeDraftRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	d, err := h.store.Finalize(r.Context(), id, &req)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrDraftNotFound):
			writeError(w, http.StatusNotFound, "filing draft not found")
		case errors.Is(err, domain.ErrValidationBlocked):
			writeError(w, http.StatusUnprocessableEntity, "filing draft is blocked by validation errors")
		case errors.Is(err, domain.ErrDraftAlreadyFinal):
			writeError(w, http.StatusConflict, "filing draft is already finalized")
		default:
			writeError(w, http.StatusInternalServerError, "failed to finalize filing draft")
		}
		return
	}

	_ = h.publisher.Publish(r.Context(), "filing.ready.for.submission", id, tenantID, d)
	writeJSON(w, http.StatusOK, d)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
