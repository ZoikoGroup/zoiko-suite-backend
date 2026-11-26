package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/compliance-status-svc/internal/authz"
	"zoiko.io/compliance-status-svc/internal/domain"
	"zoiko.io/compliance-status-svc/internal/events"
	"zoiko.io/compliance-status-svc/internal/middleware"
	"zoiko.io/compliance-status-svc/internal/store"
)

// Action types used when checking authorization
const (
	actionComplianceStatusCreate = "COMPLIANCE_STATUS_CREATE"
	actionComplianceStatusRead   = "COMPLIANCE_STATUS_READ"
	actionComplianceStatusUpdate = "COMPLIANCE_STATUS_UPDATE"
)

// AuthZClient checks whether a principal is authorized to perform an action
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

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "principal identity missing")
		return "", false
	}
	return principalID, true
}

func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, authz.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "action not authorized")
		return
	}
	writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/compliance-status", func(r chi.Router) {
		r.Post("/", h.CreateComplianceStatus)
		r.Get("/", h.ListComplianceStatuses)
		r.Get("/{compliance_status_id}", h.GetComplianceStatus)
		r.Post("/{compliance_status_id}", h.UpdateComplianceStatus)
	})
}

type createComplianceStatusRequest struct {
	ComplianceStatusID       string `json:"compliance_status_id,omitempty"`
	LegalEntityID            string `json:"legal_entity_id"`
	DomainCode               string `json:"domain_code"`
	StatusCode               string `json:"status_code"`
	StatusReason             string `json:"status_reason"`
	EvidenceSufficiencyState string `json:"evidence_sufficiency_state"`
	EvaluatedAt              string `json:"evaluated_at"` // RFC3339
	CreatedByPrincipalID     string `json:"created_by_principal_id"`
}

func (req *createComplianceStatusRequest) validate() error {
	if req.LegalEntityID == "" {
		return errors.New("legal_entity_id is required")
	}
	if req.DomainCode == "" {
		return errors.New("domain_code is required")
	}
	if !domain.IsValidDomainCode(domain.DomainCode(req.DomainCode)) {
		return errors.New("invalid domain_code: must be TAX, PAYROLL, HR, FINANCE, LEGAL, or COMMERCIAL")
	}
	if req.StatusCode == "" {
		return errors.New("status_code is required")
	}
	if !domain.IsValidStatusCode(domain.ComplianceStatusCode(req.StatusCode)) {
		return errors.New("invalid status_code: must be COMPLIANT, NON_COMPLIANT, AT_RISK, UNKNOWN, or WAIVED")
	}
	if req.StatusReason == "" {
		return errors.New("status_reason is required")
	}
	if req.EvidenceSufficiencyState == "" {
		return errors.New("evidence_sufficiency_state is required")
	}
	if !domain.IsValidEvidenceState(domain.EvidenceSufficiencyState(req.EvidenceSufficiencyState)) {
		return errors.New("invalid evidence_sufficiency_state: must be SUFFICIENT, INSUFFICIENT, PENDING, or NOT_APPLICABLE")
	}
	if req.EvaluatedAt == "" {
		return errors.New("evaluated_at is required (RFC3339)")
	}
	if _, err := time.Parse(time.RFC3339, req.EvaluatedAt); err != nil {
		return errors.New("evaluated_at must be RFC3339 format")
	}
	if req.CreatedByPrincipalID == "" {
		return errors.New("created_by_principal_id is required")
	}
	return nil
}

func (h *Handler) CreateComplianceStatus(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req createComplianceStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	// The actor is the authenticated principal: an absent created_by is
	// filled from it, a different one is refused.
	if req.CreatedByPrincipalID != principalID {
		writeError(w, http.StatusForbidden, "created_by_principal_id must be the authenticated principal")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionComplianceStatusCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	evaluatedAt, _ := time.Parse(time.RFC3339, req.EvaluatedAt)

	cs, err := h.store.CreateComplianceStatus(r.Context(), domain.CreateComplianceStatusParams{
		ComplianceStatusID:       req.ComplianceStatusID,
		TenantID:                 tenantID,
		LegalEntityID:            req.LegalEntityID,
		DomainCode:               domain.DomainCode(req.DomainCode),
		StatusCode:               domain.ComplianceStatusCode(req.StatusCode),
		StatusReason:             req.StatusReason,
		EvidenceSufficiencyState: domain.EvidenceSufficiencyState(req.EvidenceSufficiencyState),
		EvaluatedAt:              evaluatedAt,
		CreatedByPrincipalID:     req.CreatedByPrincipalID,
		CorrelationID:            r.Header.Get("X-Correlation-ID"),
	})
	if err != nil {
		h.logger.Error("create compliance status failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create compliance status")
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "compliance.status.changed", SubjectID: cs.ComplianceStatusID, TenantID: tenantID,
		LegalEntityID: cs.LegalEntityID, Jurisdiction: "", ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: cs,
	})
	writeJSON(w, http.StatusCreated, cs)
}

func (h *Handler) GetComplianceStatus(w http.ResponseWriter, r *http.Request) {
	complianceStatusID := chi.URLParam(r, "compliance_status_id")

	cs, err := h.store.FindComplianceStatusByID(r.Context(), complianceStatusID)
	if err != nil {
		if errors.Is(err, domain.ErrComplianceStatusNotFound) {
			writeError(w, http.StatusNotFound, "compliance status not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get compliance status")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, cs.LegalEntityID, actionComplianceStatusRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	writeJSON(w, http.StatusOK, cs)
}

func (h *Handler) ListComplianceStatuses(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	domainCode := r.URL.Query().Get("domain_code")
	statusCode := r.URL.Query().Get("status_code")
	limitStr := r.URL.Query().Get("limit")
	offsetStr := r.URL.Query().Get("offset")

	limit := 50
	if limitStr != "" {
		if v, err := strconv.Atoi(limitStr); err == nil && v > 0 && v <= 200 {
			limit = v
		}
	}
	offset := 0
	if offsetStr != "" {
		if v, err := strconv.Atoi(offsetStr); err == nil && v >= 0 {
			offset = v
		}
	}

	filter := domain.ListComplianceStatusFilter{
		LegalEntityID: legalEntityID,
		DomainCode:    domain.DomainCode(domainCode),
		StatusCode:    domain.ComplianceStatusCode(statusCode),
		Limit:         limit,
		Offset:        offset,
	}

	// A list used to be served to any caller in the tenant without an
	// authorization decision. It needs COMPLIANCE_STATUS_READ on the entity,
	// or on the tenant when it spans entities.
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	scope := legalEntityID
	if scope == "" {
		scope = middleware.GetTenantID(r.Context())
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, scope, actionComplianceStatusRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	records, err := h.store.ListComplianceStatuses(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list compliance statuses")
		return
	}
	if records == nil {
		records = []*domain.ComplianceStatus{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"compliance_statuses": records,
		"total":               len(records),
	})
}

type updateComplianceStatusRequest struct {
	StatusCode               string `json:"status_code"`
	StatusReason             string `json:"status_reason"`
	EvidenceSufficiencyState string `json:"evidence_sufficiency_state"`
	ExpectedVersion          int    `json:"expected_version,omitempty"`
}

func (req *updateComplianceStatusRequest) validate() error {
	if req.StatusCode == "" {
		return errors.New("status_code is required")
	}
	if !domain.IsValidStatusCode(domain.ComplianceStatusCode(req.StatusCode)) {
		return errors.New("invalid status_code: must be COMPLIANT, NON_COMPLIANT, AT_RISK, UNKNOWN, or WAIVED")
	}
	if req.StatusReason == "" {
		return errors.New("status_reason is required")
	}
	if req.EvidenceSufficiencyState == "" {
		return errors.New("evidence_sufficiency_state is required")
	}
	if !domain.IsValidEvidenceState(domain.EvidenceSufficiencyState(req.EvidenceSufficiencyState)) {
		return errors.New("invalid evidence_sufficiency_state: must be SUFFICIENT, INSUFFICIENT, PENDING, or NOT_APPLICABLE")
	}
	return nil
}

func (h *Handler) UpdateComplianceStatus(w http.ResponseWriter, r *http.Request) {
	complianceStatusID := chi.URLParam(r, "compliance_status_id")

	var req updateComplianceStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	// First check authorization by reading the existing record
	existing, err := h.store.FindComplianceStatusByID(r.Context(), complianceStatusID)
	if err != nil {
		if errors.Is(err, domain.ErrComplianceStatusNotFound) {
			writeError(w, http.StatusNotFound, "compliance status not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get compliance status")
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionComplianceStatusUpdate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	cs, err := h.store.UpdateComplianceStatus(r.Context(), complianceStatusID, domain.UpdateComplianceStatusParams{
		StatusCode:               domain.ComplianceStatusCode(req.StatusCode),
		StatusReason:             req.StatusReason,
		EvidenceSufficiencyState: domain.EvidenceSufficiencyState(req.EvidenceSufficiencyState),
		ExpectedVersion:          req.ExpectedVersion,
	}, principalID, r.Header.Get("X-Correlation-ID"))
	if err != nil {
		if errors.Is(err, domain.ErrComplianceStatusNotFound) {
			writeError(w, http.StatusNotFound, "compliance status not found")
			return
		}
		h.logger.Error("update compliance status failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to update compliance status")
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "compliance.status.changed", SubjectID: cs.ComplianceStatusID, TenantID: cs.TenantID,
		LegalEntityID: cs.LegalEntityID, Jurisdiction: "", ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: cs,
	})
	writeJSON(w, http.StatusOK, cs)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
