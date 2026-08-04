package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/exception-escalation-svc/internal/authz"
	"zoiko.io/exception-escalation-svc/internal/domain"
	"zoiko.io/exception-escalation-svc/internal/events"
	"zoiko.io/exception-escalation-svc/internal/middleware"
	"zoiko.io/exception-escalation-svc/internal/store"
)

// AuthZClient is the interface the handler depends on to authorize write
// actions against the Governance Plane authorization-svc. Every write action
// must be checked before it is performed and must fail closed.
type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

const (
	actionExceptionCreate   = "EXCEPTION_CREATE"
	actionExceptionEscalate = "EXCEPTION_ESCALATE"
	actionExceptionResolve  = "EXCEPTION_RESOLVE"
)

type Handler struct {
	store     store.Store
	publisher events.Publisher
	authz     AuthZClient
	logger    *zap.Logger
}

func New(st store.Store, pub events.Publisher, az AuthZClient, logger *zap.Logger) *Handler {
	return &Handler{store: st, publisher: pub, authz: az, logger: logger}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/exception-escalation", func(r chi.Router) {
		r.Post("/exceptions", h.CreateException)
		r.Get("/exceptions", h.ListExceptions)
		r.Get("/exceptions/{id}", h.GetExceptionByID)
		r.Post("/exceptions/{id}/escalate", h.EscalateException)
		r.Post("/exceptions/{id}/resolve", h.ResolveException)

		r.Get("/escalations", h.ListEscalations)
	})
}

func (h *Handler) CreateException(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.CreateExceptionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.JurisdictionID == "" || req.ExceptionType == "" || req.LinkedObjectType == "" || req.LinkedObjectID == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id, jurisdiction_id, exception_type, linked_object_type, and linked_object_id are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionExceptionCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	c := &domain.ExceptionCase{
		TenantID:         tenantID,
		LegalEntityID:    req.LegalEntityID,
		JurisdictionID:   req.JurisdictionID,
		ExceptionType:    req.ExceptionType,
		SeverityLevel:    req.SeverityLevel,
		LinkedObjectType: req.LinkedObjectType,
		LinkedObjectID:   req.LinkedObjectID,
		Description:      req.Description,
		AssignedToRole:   req.AssignedToRole,
		CreatedBy:        req.CreatedBy,
	}

	if err := h.store.CreateException(r.Context(), c); err != nil {
		h.logger.Error("create exception case failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create exception case")
		return
	}
	_ = h.publisher.Publish(r.Context(), "exception.created", c.ExceptionCaseID, tenantID, c)
	writeJSON(w, http.StatusCreated, c)
}

func (h *Handler) GetExceptionByID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	c, err := h.store.GetExceptionByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrExceptionCaseNotFound) {
			writeError(w, http.StatusNotFound, "exception case not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get exception case")
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (h *Handler) ListExceptions(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	caseStatus := r.URL.Query().Get("case_status")
	severityLevel := r.URL.Query().Get("severity_level")
	exceptionType := r.URL.Query().Get("exception_type")

	cases, err := h.store.ListExceptions(r.Context(), legalEntityID, caseStatus, severityLevel, exceptionType)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list exception cases")
		return
	}
	if cases == nil {
		cases = []domain.ExceptionCase{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"exception_cases": cases,
		"total":           len(cases),
	})
}

func (h *Handler) EscalateException(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.EscalateCaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.EscalatedToRole == "" || req.EscalationReason == "" {
		writeError(w, http.StatusBadRequest, "escalated_to_role and escalation_reason are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	existingCase, err := h.store.GetExceptionByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrExceptionCaseNotFound) {
			writeError(w, http.StatusNotFound, "exception case not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get exception case")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existingCase.LegalEntityID, actionExceptionEscalate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	escRecord, updatedCase, err := h.store.EscalateException(r.Context(), id, &req)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrExceptionCaseNotFound):
			writeError(w, http.StatusNotFound, "exception case not found")
		case errors.Is(err, domain.ErrCaseAlreadyClosed):
			writeError(w, http.StatusConflict, "exception case is already closed")
		default:
			writeError(w, http.StatusInternalServerError, "failed to escalate exception case")
		}
		return
	}
	_ = h.publisher.Publish(r.Context(), "exception.escalated", id, tenantID, map[string]interface{}{
		"escalation_record": escRecord,
		"exception_case":    updatedCase,
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"escalation_record": escRecord,
		"exception_case":    updatedCase,
	})
}

func (h *Handler) ResolveException(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.ResolveCaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ClosedBy == "" {
		writeError(w, http.StatusBadRequest, "closed_by is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	existingCase, err := h.store.GetExceptionByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrExceptionCaseNotFound) {
			writeError(w, http.StatusNotFound, "exception case not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get exception case")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existingCase.LegalEntityID, actionExceptionResolve); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	updatedCase, err := h.store.ResolveException(r.Context(), id, &req)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrExceptionCaseNotFound):
			writeError(w, http.StatusNotFound, "exception case not found")
		case errors.Is(err, domain.ErrCaseAlreadyClosed):
			writeError(w, http.StatusConflict, "exception case is already closed")
		default:
			writeError(w, http.StatusInternalServerError, "failed to resolve exception case")
		}
		return
	}
	_ = h.publisher.Publish(r.Context(), "exception.closed", id, tenantID, updatedCase)
	writeJSON(w, http.StatusOK, updatedCase)
}

func (h *Handler) ListEscalations(w http.ResponseWriter, r *http.Request) {
	role := r.URL.Query().Get("escalated_to_role")
	status := r.URL.Query().Get("escalation_status")

	escalations, err := h.store.ListEscalations(r.Context(), role, status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list escalations")
		return
	}
	if escalations == nil {
		escalations = []domain.EscalationRecord{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"escalation_records": escalations,
		"total":              len(escalations),
	})
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, authz.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "authorization denied")
		return
	}
	writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
