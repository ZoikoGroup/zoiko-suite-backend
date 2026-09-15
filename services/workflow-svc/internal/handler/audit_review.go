package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"zoiko.io/workflow-svc/internal/domain"
)

const (
	actionReviewManage  = "AUDIT_REVIEW_MANAGE"
	actionReviewSignOff = "AUDIT_REVIEW_SIGNOFF"
	actionReviewRead    = "AUDIT_REVIEW_READ"
)

type openReviewRequest struct {
	TargetType string `json:"target_type"`
	TargetID   string `json:"target_id"`
}

// OpenReview resolves InitiatedByPrincipalID from the target itself
// (currently only WORKPAPER is wired to a real lookup) so "no self-review"
// is checked against the person actually responsible for the target, not
// whoever happens to call this endpoint.
func (h *Handler) OpenReview(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	correlationID, ok := h.requireCorrelationID(w, r)
	if !ok {
		return
	}
	engagement, err := h.store.GetAuditEngagement(r.Context(), tenantID, chi.URLParam(r, "engagement_id"))
	if err != nil {
		h.writeAuditEngagementErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, engagement.LegalEntityID, actionReviewManage); err != nil {
		h.writeAuditEngagementAuthzErr(w, err)
		return
	}
	var req openReviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	var initiatedBy string
	switch req.TargetType {
	case domain.ReviewTargetWorkpaper:
		wp, err := h.store.GetWorkpaper(r.Context(), tenantID, req.TargetID)
		if err != nil {
			h.writeWorkpaperErr(w, err)
			return
		}
		if wp.PreparedByPrincipalID == nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "target_not_yet_prepared"})
			return
		}
		initiatedBy = *wp.PreparedByPrincipalID
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": "target_type"})
		return
	}
	scope, created, err := h.store.OpenReview(r.Context(), domain.OpenReviewParams{
		EngagementID: engagement.EngagementID, TenantID: tenantID, TargetType: req.TargetType, TargetID: req.TargetID,
		InitiatedByPrincipalID: initiatedBy, CorrelationID: correlationID,
	})
	if err != nil {
		h.writeAuditReviewErr(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, scope)
}

type assignReviewerRequest struct {
	ReviewerPrincipalID string `json:"reviewer_principal_id"`
	Role                string `json:"role"`
}

func (h *Handler) AssignReviewer(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	scopeID := chi.URLParam(r, "review_scope_id")
	scope, err := h.store.GetReviewScope(r.Context(), tenantID, scopeID)
	if err != nil {
		h.writeAuditReviewErr(w, err)
		return
	}
	engagement, err := h.store.GetAuditEngagement(r.Context(), tenantID, scope.EngagementID)
	if err != nil {
		h.writeAuditEngagementErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, engagement.LegalEntityID, actionReviewManage); err != nil {
		h.writeAuditEngagementAuthzErr(w, err)
		return
	}
	var req assignReviewerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ReviewerPrincipalID == "" || req.Role == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field"})
		return
	}
	assignment, err := h.store.AssignReviewer(r.Context(), domain.AssignReviewerParams{ReviewScopeID: scopeID, TenantID: tenantID, ReviewerPrincipalID: req.ReviewerPrincipalID, Role: req.Role})
	if err != nil {
		h.writeAuditReviewErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, assignment)
}

type raiseReviewNoteRequest struct {
	Body      string `json:"body"`
	Mandatory bool   `json:"mandatory"`
}

func (h *Handler) RaiseReviewNote(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	scopeID := chi.URLParam(r, "review_scope_id")
	scope, err := h.store.GetReviewScope(r.Context(), tenantID, scopeID)
	if err != nil {
		h.writeAuditReviewErr(w, err)
		return
	}
	engagement, err := h.store.GetAuditEngagement(r.Context(), tenantID, scope.EngagementID)
	if err != nil {
		h.writeAuditEngagementErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, engagement.LegalEntityID, actionReviewManage); err != nil {
		h.writeAuditEngagementAuthzErr(w, err)
		return
	}
	var req raiseReviewNoteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "body"})
		return
	}
	note, err := h.store.RaiseReviewNote(r.Context(), domain.RaiseReviewNoteParams{ReviewScopeID: scopeID, TenantID: tenantID, RaisedByPrincipalID: actor, Body: req.Body, Mandatory: req.Mandatory})
	if err != nil {
		h.writeAuditReviewErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, note)
}

type respondToReviewNoteRequest struct {
	ResponseBody string `json:"response_body"`
}

func (h *Handler) RespondToReviewNote(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req respondToReviewNoteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ResponseBody == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "response_body"})
		return
	}
	note, err := h.store.RespondToReviewNote(r.Context(), domain.RespondToReviewNoteParams{ReviewNoteID: chi.URLParam(r, "review_note_id"), TenantID: tenantID, ActorPrincipalID: actor, ResponseBody: req.ResponseBody})
	if err != nil {
		h.writeAuditReviewErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, note)
}

func (h *Handler) ResolveReviewNote(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	note, err := h.store.ResolveReviewNote(r.Context(), domain.ResolveReviewNoteParams{ReviewNoteID: chi.URLParam(r, "review_note_id"), TenantID: tenantID, ActorPrincipalID: actor})
	if err != nil {
		h.writeAuditReviewErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, note)
}

// SignOff derives the target's current content fingerprint server-side
// (never trusting a caller-supplied one) — currently only WORKPAPER is
// wired to a real fingerprint (the workpaper's own lock_digest, which
// only exists once LOCKED).
func (h *Handler) SignOff(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	correlationID, ok := h.requireCorrelationID(w, r)
	if !ok {
		return
	}
	scopeID := chi.URLParam(r, "review_scope_id")
	scope, err := h.store.GetReviewScope(r.Context(), tenantID, scopeID)
	if err != nil {
		h.writeAuditReviewErr(w, err)
		return
	}
	engagement, err := h.store.GetAuditEngagement(r.Context(), tenantID, scope.EngagementID)
	if err != nil {
		h.writeAuditEngagementErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, engagement.LegalEntityID, actionReviewSignOff); err != nil {
		h.writeAuditEngagementAuthzErr(w, err)
		return
	}
	var fingerprint string
	switch scope.TargetType {
	case domain.ReviewTargetWorkpaper:
		wp, err := h.store.GetWorkpaper(r.Context(), tenantID, scope.TargetID)
		if err != nil {
			h.writeWorkpaperErr(w, err)
			return
		}
		if wp.Status != domain.WorkpaperLocked || wp.LockDigest == nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "target_not_locked"})
			return
		}
		fingerprint = *wp.LockDigest
	default:
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "unsupported_target_type"})
		return
	}
	signOff, err := h.store.SignOff(r.Context(), domain.SignOffParams{ReviewScopeID: scopeID, TenantID: tenantID, ActorPrincipalID: actor, ContentFingerprint: fingerprint, CorrelationID: correlationID})
	if err != nil {
		h.writeAuditReviewErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, signOff)
}

func (h *Handler) WithdrawSignOff(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	signOff, err := h.store.WithdrawSignOff(r.Context(), domain.WithdrawSignOffParams{SignOffID: chi.URLParam(r, "sign_off_id"), TenantID: tenantID, ActorPrincipalID: actor})
	if err != nil {
		h.writeAuditReviewErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, signOff)
}

func (h *Handler) StartQualityReview(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	correlationID, ok := h.requireCorrelationID(w, r)
	if !ok {
		return
	}
	engagement, err := h.store.GetAuditEngagement(r.Context(), tenantID, chi.URLParam(r, "engagement_id"))
	if err != nil {
		h.writeAuditEngagementErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, engagement.LegalEntityID, actionReviewManage); err != nil {
		h.writeAuditEngagementAuthzErr(w, err)
		return
	}
	record, created, err := h.store.StartQualityReview(r.Context(), domain.StartQualityReviewParams{EngagementID: engagement.EngagementID, TenantID: tenantID, ActorPrincipalID: actor, CorrelationID: correlationID})
	if err != nil {
		h.writeAuditReviewErr(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, record)
}

func (h *Handler) CompleteQualityReview(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	correlationID, ok := h.requireCorrelationID(w, r)
	if !ok {
		return
	}
	record, _, err := h.store.CompleteQualityReview(r.Context(), domain.CompleteQualityReviewParams{QualityReviewID: chi.URLParam(r, "quality_review_id"), TenantID: tenantID, ActorPrincipalID: actor, CorrelationID: correlationID})
	if err != nil {
		h.writeAuditReviewErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, record)
}

// EvaluateReleaseGates is a thin wrapper over AUD-01's own REPORT-stage
// completion-gate query — the doc names it under AUD-09, but the
// authoritative gate check lives with the engagement it gates.
func (h *Handler) EvaluateReleaseGates(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	engagement, err := h.store.GetAuditEngagement(r.Context(), tenantID, chi.URLParam(r, "engagement_id"))
	if err != nil {
		h.writeAuditEngagementErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, engagement.LegalEntityID, actionReviewRead); err != nil {
		h.writeAuditEngagementAuthzErr(w, err)
		return
	}
	allSignOffsValid, noUnresolvedNotes, err := h.store.GetAuditEngagementReportGates(r.Context(), tenantID, engagement.EngagementID)
	if err != nil {
		h.writeAuditReviewErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"all_sign_offs_valid": allSignOffsValid, "no_unresolved_mandatory_notes": noUnresolvedNotes})
}

func (h *Handler) writeAuditReviewErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrReviewScopeNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "review_scope_not_found"})
	case errors.Is(err, domain.ErrReviewNoteNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "review_note_not_found"})
	case errors.Is(err, domain.ErrReviewNoteInvalidState):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "invalid_review_note_state"})
	case errors.Is(err, domain.ErrSignOffInvalidState):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "invalid_sign_off_state"})
	case errors.Is(err, domain.ErrSelfReviewNotAllowed):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "self_review_not_allowed"})
	case errors.Is(err, domain.ErrReviewAssignmentRequired):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "review_assignment_required"})
	case errors.Is(err, domain.ErrReviewNoteMandatoryUnresolved):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "mandatory_review_notes_unresolved"})
	case errors.Is(err, domain.ErrQualityReviewNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "quality_review_not_found"})
	case errors.Is(err, domain.ErrQualityReviewInvalidState):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "invalid_quality_review_state"})
	default:
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
	}
}
