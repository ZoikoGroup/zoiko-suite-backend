package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/compliance-status-svc/internal/authz"
	"zoiko.io/compliance-status-svc/internal/domain"
	"zoiko.io/compliance-status-svc/internal/events"
	"zoiko.io/compliance-status-svc/internal/middleware"
	"zoiko.io/compliance-status-svc/internal/store"
)

// Action types used when checking authorization for write operations against
// authorization-svc. Every write action must be checked before it happens and
// must fail closed (see internal/authz/client.go).
const (
	actionComplianceStatusEvaluate = "COMPLIANCE_STATUS_EVALUATE"
	actionComplianceGapCreate      = "COMPLIANCE_GAP_CREATE"
	actionComplianceGapResolve     = "COMPLIANCE_GAP_RESOLVE"
)

// AuthZClient checks whether a principal is authorized to perform an action
// against a legal entity. Implementations must fail closed.
type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type Handler struct {
	store     store.Store
	publisher events.Publisher
	authz     AuthZClient
	logger    *zap.Logger
}

func New(st store.Store, pub events.Publisher, az AuthZClient, logger *zap.Logger) *Handler {
	return &Handler{store: st, publisher: pub, authz: az, logger: logger}
}

// requirePrincipal extracts the calling principal from the X-Principal-Id
// header, responding with 401 and returning ok=false if absent.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "principal identity missing")
		return "", false
	}
	return principalID, true
}

// writeAuthzErr responds appropriately for an error returned by AuthZClient.CheckAllowed:
// explicit denial maps to 403, anything else (including unavailability) fails
// closed as 503.
func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, authz.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "action not authorized")
		return
	}
	writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/compliance-status", func(r chi.Router) {
		r.Post("/evaluate", h.Evaluate)
		r.Get("/", h.List)
		r.Get("/{id}", h.GetByID)

		r.Post("/gaps", h.CreateGap)
		r.Get("/gaps", h.ListGaps)
		r.Post("/gaps/{id}/resolve", h.ResolveGap)
	})
}

func (h *Handler) Evaluate(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.EvaluateComplianceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.JurisdictionID == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id and jurisdiction_id are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionComplianceStatusEvaluate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	c := &domain.ComplianceHealth{
		TenantID:             tenantID,
		LegalEntityID:        req.LegalEntityID,
		JurisdictionID:       req.JurisdictionID,
		DomainName:           req.DomainName,
		TotalObligations:     req.TotalObligations,
		FulfilledObligations: req.FulfilledObligations,
		PendingObligations:   req.PendingObligations,
		OverdueObligations:   req.OverdueObligations,
		OpenExceptions:       req.OpenExceptions,
		Notes:                req.Notes,
		EffectiveFrom:        req.EffectiveFrom,
		CreatedBy:            req.CreatedBy,
	}

	if err := h.store.Evaluate(r.Context(), c); err != nil {
		h.logger.Error("evaluate compliance status failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to evaluate compliance status")
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "compliance.status.changed", SubjectID: c.StatusID, TenantID: tenantID,
		LegalEntityID: c.LegalEntityID, Jurisdiction: c.JurisdictionID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: c,
	})
	writeJSON(w, http.StatusOK, c)
}

func (h *Handler) GetByID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	c, err := h.store.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrStatusRecordNotFound) {
			writeError(w, http.StatusNotFound, "compliance status record not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get compliance status record")
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	jurisdictionID := r.URL.Query().Get("jurisdiction_id")
	domainName := r.URL.Query().Get("domain_name")
	status := r.URL.Query().Get("status")

	records, err := h.store.List(r.Context(), legalEntityID, jurisdictionID, domainName, status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list compliance status records")
		return
	}
	if records == nil {
		records = []domain.ComplianceHealth{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"compliance_status_records": records,
		"total":                     len(records),
	})
}

func (h *Handler) CreateGap(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.CreateGapRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.JurisdictionID == "" || req.DomainName == "" || req.GapType == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id, jurisdiction_id, domain_name, and gap_type are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionComplianceGapCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	g := &domain.ComplianceGap{
		TenantID:        tenantID,
		LegalEntityID:   req.LegalEntityID,
		JurisdictionID:  req.JurisdictionID,
		DomainName:      req.DomainName,
		GapType:         req.GapType,
		Severity:        req.Severity,
		SourceReference: req.SourceReference,
		Description:     req.Description,
		RemediationPlan: req.RemediationPlan,
	}

	if err := h.store.CreateGap(r.Context(), g); err != nil {
		h.logger.Error("create compliance gap failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to log compliance gap")
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "compliance.gap.detected", SubjectID: g.GapID, TenantID: tenantID,
		LegalEntityID: g.LegalEntityID, Jurisdiction: g.JurisdictionID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: g,
	})
	writeJSON(w, http.StatusCreated, g)
}

func (h *Handler) ListGaps(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	domainName := r.URL.Query().Get("domain_name")
	severity := r.URL.Query().Get("severity")
	status := r.URL.Query().Get("status")

	gaps, err := h.store.ListGaps(r.Context(), legalEntityID, domainName, severity, status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list compliance gaps")
		return
	}
	if gaps == nil {
		gaps = []domain.ComplianceGap{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"compliance_gaps": gaps,
		"total":           len(gaps),
	})
}

func (h *Handler) ResolveGap(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.ResolveGapRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	existing, err := h.store.GetGapByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrGapNotFound) {
			writeError(w, http.StatusNotFound, "compliance gap not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to resolve compliance gap")
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionComplianceGapResolve); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	g, err := h.store.ResolveGap(r.Context(), id, &req)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrGapNotFound):
			writeError(w, http.StatusNotFound, "compliance gap not found")
		case errors.Is(err, domain.ErrGapAlreadyResolved):
			writeError(w, http.StatusConflict, "compliance gap is already resolved")
		default:
			writeError(w, http.StatusInternalServerError, "failed to resolve compliance gap")
		}
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "compliance.gap.resolved", SubjectID: id, TenantID: tenantID,
		LegalEntityID: g.LegalEntityID, Jurisdiction: g.JurisdictionID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: g,
	})
	writeJSON(w, http.StatusOK, g)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
