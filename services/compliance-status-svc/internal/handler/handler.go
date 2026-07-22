package handler

import (
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
	_ = h.publisher.Publish(r.Context(), "compliance.status.changed", c.StatusID, tenantID, c)
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
	_ = h.publisher.Publish(r.Context(), "compliance.gap.detected", g.GapID, tenantID, g)
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
	_ = h.publisher.Publish(r.Context(), "compliance.gap.resolved", id, tenantID, g)
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
