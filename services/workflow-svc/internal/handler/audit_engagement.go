package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/domain"
)

const (
	actionAuditEngagementCreate = "AUDIT_ENGAGEMENT_CREATE"
	actionAuditEngagementAccept = "AUDIT_ENGAGEMENT_ACCEPTANCE_SUBMIT"
	actionAuditEngagementDecide = "AUDIT_ENGAGEMENT_ACCEPTANCE_DECIDE"
	actionAuditEngagementManage = "AUDIT_ENGAGEMENT_MANAGE"
	actionAuditEngagementRead   = "AUDIT_ENGAGEMENT_READ"
)

type createAuditEngagementRequest struct {
	TenantID                string    `json:"tenant_id"`
	LegalEntityID           string    `json:"legal_entity_id"`
	EngagementCode          string    `json:"engagement_code"`
	EngagementType          string    `json:"engagement_type"`
	ReportingPeriodStart    time.Time `json:"reporting_period_start"`
	ReportingPeriodEnd      time.Time `json:"reporting_period_end"`
	FrameworkProfileID      string    `json:"framework_profile_id"`
	FrameworkProfileVersion string    `json:"framework_profile_version"`
	MethodologyID           string    `json:"methodology_id"`
	MethodologyVersion      string    `json:"methodology_version"`
	ResponsiblePartnerID    string    `json:"responsible_partner_id"`
	ScopeSummary            string    `json:"scope_summary"`
}

func (r createAuditEngagementRequest) missing() string {
	switch {
	case r.TenantID == "":
		return "tenant_id"
	case r.LegalEntityID == "":
		return "legal_entity_id"
	case r.EngagementCode == "":
		return "engagement_code"
	case r.EngagementType == "":
		return "engagement_type"
	case r.ReportingPeriodStart.IsZero():
		return "reporting_period_start"
	case r.ReportingPeriodEnd.IsZero():
		return "reporting_period_end"
	case r.FrameworkProfileID == "":
		return "framework_profile_id"
	case r.FrameworkProfileVersion == "":
		return "framework_profile_version"
	case r.MethodologyID == "":
		return "methodology_id"
	case r.MethodologyVersion == "":
		return "methodology_version"
	case r.ResponsiblePartnerID == "":
		return "responsible_partner_id"
	case r.ScopeSummary == "":
		return "scope_summary"
	default:
		return ""
	}
}

// CreateAuditEngagement creates the typed AUD-01 container. The framework and
// methodology references are opaque versioned identifiers at this boundary:
// this service records precisely what the authorized caller selected but does
// not invent an audit-framework interpretation.
func (h *Handler) CreateAuditEngagement(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	correlationID := r.Header.Get("X-Correlation-ID")
	if correlationID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_correlation_id"})
		return
	}
	var req createAuditEngagementRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if missing := req.missing(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": missing})
		return
	}
	if req.ReportingPeriodEnd.Before(req.ReportingPeriodStart) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_reporting_period"})
		return
	}
	if h.refuseForeignTenant(w, req.TenantID, tenantID) {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, req.LegalEntityID, actionAuditEngagementCreate); err != nil {
		h.writeAuditEngagementAuthzErr(w, err)
		return
	}
	engagement, created, err := h.store.CreateAuditEngagement(r.Context(), domain.CreateAuditEngagementParams{
		TenantID: tenantID, LegalEntityID: req.LegalEntityID, EngagementCode: req.EngagementCode, EngagementType: req.EngagementType,
		ReportingPeriodStart: req.ReportingPeriodStart, ReportingPeriodEnd: req.ReportingPeriodEnd,
		FrameworkProfileID: req.FrameworkProfileID, FrameworkProfileVersion: req.FrameworkProfileVersion,
		MethodologyID: req.MethodologyID, MethodologyVersion: req.MethodologyVersion, ResponsiblePartnerID: req.ResponsiblePartnerID,
		ScopeSummary: req.ScopeSummary, CreatedByPrincipalID: actor, CorrelationID: correlationID,
	})
	if err != nil {
		h.writeAuditEngagementErr(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		h.publishAuditEngagementEvent(r, "audit.engagement.created", *engagement, actor)
	}
	writeJSON(w, status, engagement)
}

func (h *Handler) GetAuditEngagement(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	engagement, err := h.store.GetAuditEngagement(r.Context(), tenantID, chi.URLParam(r, "engagement_id"))
	if err != nil {
		h.writeAuditEngagementErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, engagement.LegalEntityID, actionAuditEngagementRead); err != nil {
		h.writeAuditEngagementAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, engagement)
}

type submitAuditAcceptanceRequest struct {
	DocumentID string `json:"document_id"`
}

func (h *Handler) SubmitAuditEngagementAcceptance(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	correlationID := r.Header.Get("X-Correlation-ID")
	if correlationID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_correlation_id"})
		return
	}
	var req submitAuditAcceptanceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if req.DocumentID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "acceptance_evidence_required"})
		return
	}
	engagement, err := h.store.GetAuditEngagement(r.Context(), tenantID, chi.URLParam(r, "engagement_id"))
	if err != nil {
		h.writeAuditEngagementErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, engagement.LegalEntityID, actionAuditEngagementAccept); err != nil {
		h.writeAuditEngagementAuthzErr(w, err)
		return
	}
	version, err := h.documents.VerifyDocument(r.Context(), tenantID, engagement.LegalEntityID, req.DocumentID, actor, correlationID)
	if err != nil {
		h.writeAuditEngagementErr(w, err)
		return
	}
	engagement, changed, err := h.store.SubmitAuditEngagementAcceptance(r.Context(), domain.SubmitAuditEngagementAcceptanceParams{
		EngagementID: engagement.EngagementID, TenantID: tenantID, EvidenceDocumentID: req.DocumentID, EvidenceDocumentVersion: version, ActorPrincipalID: actor, CorrelationID: correlationID,
	})
	if err != nil {
		h.writeAuditEngagementErr(w, err)
		return
	}
	if changed {
		h.publishAuditEngagementEvent(r, "audit.engagement.acceptance_submitted", *engagement, actor)
	}
	writeJSON(w, http.StatusOK, engagement)
}

type auditAcceptanceDecisionRequest struct {
	Decision string `json:"decision"`
}

// RecordAuditAcceptanceDecision is deliberately separate from submission:
// the person who created an engagement cannot accept it.
func (h *Handler) RecordAuditAcceptanceDecision(w http.ResponseWriter, r *http.Request) {
	var req auditAcceptanceDecisionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	next := ""
	switch req.Decision {
	case "ACCEPT":
		next = domain.AuditEngagementAccepted
	case "REJECT":
		next = domain.AuditEngagementWithdrawn
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_acceptance_decision"})
		return
	}
	h.transitionAuditEngagement(w, r, actionAuditEngagementDecide, []string{domain.AuditEngagementAcceptanceReview}, next, true)
}

func (h *Handler) ActivateAuditEngagement(w http.ResponseWriter, r *http.Request) {
	h.transitionAuditEngagement(w, r, actionAuditEngagementManage, []string{domain.AuditEngagementAccepted}, domain.AuditEngagementActive, false)
}

// Later lifecycle gates are intentionally not routed yet. AUD-02, AUD-06,
// AUD-07, AUD-08, and AUD-09 own the evidence, workpaper, finding, review,
// and sign-off assertions required before fieldwork can complete or a report
// can be released. Exposing a generic manager transition before those services
// enforce their controls would let a caller bypass the specification.
func (h *Handler) WithdrawAuditEngagement(w http.ResponseWriter, r *http.Request) {
	h.transitionAuditEngagement(w, r, actionAuditEngagementManage, []string{domain.AuditEngagementProposed, domain.AuditEngagementAcceptanceReview, domain.AuditEngagementAccepted, domain.AuditEngagementActive}, domain.AuditEngagementWithdrawn, false)
}

func (h *Handler) transitionAuditEngagement(w http.ResponseWriter, r *http.Request, action string, expected []string, next string, preventCreator bool) {
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	correlationID := r.Header.Get("X-Correlation-ID")
	if correlationID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_correlation_id"})
		return
	}
	engagement, err := h.store.GetAuditEngagement(r.Context(), tenantID, chi.URLParam(r, "engagement_id"))
	if err != nil {
		h.writeAuditEngagementErr(w, err)
		return
	}
	if preventCreator && engagement.CreatedByPrincipalID == actor {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "self_acceptance_forbidden"})
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, engagement.LegalEntityID, action); err != nil {
		h.writeAuditEngagementAuthzErr(w, err)
		return
	}
	engagement, changed, err := h.store.TransitionAuditEngagement(r.Context(), domain.TransitionAuditEngagementParams{EngagementID: engagement.EngagementID, TenantID: tenantID, ActorPrincipalID: actor, CorrelationID: correlationID, ExpectedStatuses: expected, NextStatus: next})
	if err != nil {
		h.writeAuditEngagementErr(w, err)
		return
	}
	if changed {
		eventType := map[string]string{
			domain.AuditEngagementAccepted:  "audit.engagement.accepted",
			domain.AuditEngagementActive:    "audit.engagement.activated",
			domain.AuditEngagementWithdrawn: "audit.engagement.withdrawn",
		}[next]
		if next == domain.AuditEngagementWithdrawn && len(expected) == 1 && expected[0] == domain.AuditEngagementAcceptanceReview {
			eventType = "audit.engagement.rejected"
		}
		if eventType != "" {
			h.publishAuditEngagementEvent(r, eventType, *engagement, actor)
		}
	}
	writeJSON(w, http.StatusOK, engagement)
}

// Engagement state is already committed before Kafka publication. A transient
// broker outage therefore never rolls back a valid governed decision; it is
// logged for operational replay rather than hidden from operators.
func (h *Handler) publishAuditEngagementEvent(r *http.Request, eventType string, engagement domain.AuditEngagement, actor string) {
	if err := h.publisher.PublishAuditEngagementEvent(r.Context(), eventType, engagement, actor, r.Header.Get("X-Correlation-ID")); err != nil {
		h.log.Error("failed to publish audit engagement event", zap.String("event_type", eventType), zap.String("engagement_id", engagement.EngagementID), zap.String("correlation_id", r.Header.Get("X-Correlation-ID")), zap.Error(err))
	}
}

func (h *Handler) writeAuditEngagementAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, domain.ErrAuthorizationDenied) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "authorization_unavailable"})
}

func (h *Handler) writeAuditEngagementErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrAuditEngagementNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "audit_engagement_not_found"})
	case errors.Is(err, domain.ErrAuditEngagementDuplicateCode):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "duplicate_engagement_code"})
	case errors.Is(err, domain.ErrAuditEngagementInvalidState):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "invalid_engagement_state"})
	case errors.Is(err, domain.ErrAuditAcceptanceEvidenceRequired):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "acceptance_evidence_required"})
	case errors.Is(err, domain.ErrAuditAcceptanceEvidenceInvalid):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "acceptance_evidence_invalid"})
	case errors.Is(err, domain.ErrAuditAcceptanceEvidenceUnavailable):
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "acceptance_evidence_unavailable"})
	default:
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
	}
}
