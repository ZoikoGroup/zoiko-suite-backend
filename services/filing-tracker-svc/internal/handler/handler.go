package handler

import (
	"context"
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

// Action types passed to authorization-svc for each write action this
// service performs. No domain service self-authorizes a material action.
const (
	ActionFilingRequirementCreate = "FILING_REQUIREMENT_CREATE"
	ActionFilingRequirementUpdate = "FILING_REQUIREMENT_UPDATE"
	ActionFilingSubmit            = "FILING_SUBMIT"
	ActionFilingConfirm           = "FILING_CONFIRM"
	ActionFilingMarkOverdue       = "FILING_MARK_OVERDUE"
)

// AuthzClient is the authorization-svc contract this handler depends on.
// Defined here (rather than depending on the concrete *authz.Client) so
// tests can supply a stub.
type AuthzClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type Handler struct {
	store     store.Store
	publisher events.Publisher
	authz     AuthzClient
	logger    *zap.Logger
}

func New(st store.Store, pub events.Publisher, az AuthzClient, logger *zap.Logger) *Handler {
	return &Handler{store: st, publisher: pub, authz: az, logger: logger}
}

// requirePrincipal extracts the caller's principal from the X-Principal-Id
// header. If missing, it writes a 401 and returns ok=false.
func requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

// checkAllowed calls authorization-svc and writes the appropriate error
// response (403 for an explicit denial, 503 for anything else, including
// unavailability) if the action is not granted. Fails CLOSED.
func (h *Handler) checkAllowed(w http.ResponseWriter, r *http.Request, principalID, legalEntityID, actionType string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionType); err != nil {
		if errors.Is(err, authz.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "not authorized to perform this action")
			return false
		}
		writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
		return false
	}
	return true
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

	principalID, ok := requirePrincipal(w, r)
	if !ok {
		return
	}

	var req domain.CreateRequirementRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.JurisdictionID == "" || req.FilingAuthority == "" || req.DueDate == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id, jurisdiction_id, filing_authority, and due_date are required")
		return
	}

	if !h.checkAllowed(w, r, principalID, req.LegalEntityID, ActionFilingRequirementCreate) {
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
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "filing.due", FilingID: f.FilingID, TenantID: tenantID,
		LegalEntityID: f.LegalEntityID, Jurisdiction: f.JurisdictionID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: f,
	})
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

	principalID, ok := requirePrincipal(w, r)
	if !ok {
		return
	}

	existing, err := h.store.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrRequirementNotFound) {
			writeError(w, http.StatusNotFound, "filing requirement not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch filing requirement")
		return
	}

	if !h.checkAllowed(w, r, principalID, existing.LegalEntityID, ActionFilingRequirementUpdate) {
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
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "filing.requirement.updated", FilingID: id, TenantID: tenantID,
		LegalEntityID: existing.LegalEntityID, Jurisdiction: existing.JurisdictionID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: existing,
	})
	writeJSON(w, http.StatusOK, existing)
}

func (h *Handler) Submit(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	principalID, ok := requirePrincipal(w, r)
	if !ok {
		return
	}

	existing, err := h.store.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrRequirementNotFound) {
			writeError(w, http.StatusNotFound, "filing requirement not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch filing requirement")
		return
	}

	var req domain.SubmitFilingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.SubmissionReference == "" || req.SubmittedBy == "" {
		writeError(w, http.StatusBadRequest, "submission_reference and submitted_by are required")
		return
	}

	if !h.checkAllowed(w, r, principalID, existing.LegalEntityID, ActionFilingSubmit) {
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
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "filing.submitted", FilingID: id, TenantID: tenantID,
		LegalEntityID: f.LegalEntityID, Jurisdiction: f.JurisdictionID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: f,
	})
	writeJSON(w, http.StatusOK, f)
}

func (h *Handler) Confirm(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	principalID, ok := requirePrincipal(w, r)
	if !ok {
		return
	}

	existing, err := h.store.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrRequirementNotFound) {
			writeError(w, http.StatusNotFound, "filing requirement not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch filing requirement")
		return
	}

	var req domain.ConfirmFilingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ConfirmationReference == "" {
		writeError(w, http.StatusBadRequest, "confirmation_reference is required")
		return
	}

	if !h.checkAllowed(w, r, principalID, existing.LegalEntityID, ActionFilingConfirm) {
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
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "filing.confirmed", FilingID: id, TenantID: tenantID,
		LegalEntityID: f.LegalEntityID, Jurisdiction: f.JurisdictionID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: f,
	})
	writeJSON(w, http.StatusOK, f)
}

func (h *Handler) MarkOverdue(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	principalID, ok := requirePrincipal(w, r)
	if !ok {
		return
	}

	existing, err := h.store.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrRequirementNotFound) {
			writeError(w, http.StatusNotFound, "filing requirement not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch filing requirement")
		return
	}

	if !h.checkAllowed(w, r, principalID, existing.LegalEntityID, ActionFilingMarkOverdue) {
		return
	}

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
		_ = h.publisher.Publish(r.Context(), events.PublishParams{
			EventType: "filing.overdue", FilingID: id, TenantID: tenantID,
			LegalEntityID: f.LegalEntityID, Jurisdiction: f.JurisdictionID, ActorID: principalID,
			CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: f,
		})
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
