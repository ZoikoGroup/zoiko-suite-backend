package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"zoiko.io/workflow-svc/internal/domain"
)

const (
	actionWorkpaperManage = "AUDIT_WORKPAPER_MANAGE"
	actionWorkpaperLock   = "AUDIT_WORKPAPER_LOCK"
	actionWorkpaperRead   = "AUDIT_WORKPAPER_READ"
)

type createWorkpaperRequest struct {
	Reference string `json:"reference"`
	Purpose   string `json:"purpose"`
	Required  bool   `json:"required"`
}

func (h *Handler) CreateWorkpaper(w http.ResponseWriter, r *http.Request) {
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
	if err := h.authz.CheckAllowed(r.Context(), actor, engagement.LegalEntityID, actionWorkpaperManage); err != nil {
		h.writeAuditEngagementAuthzErr(w, err)
		return
	}
	var req createWorkpaperRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if req.Reference == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "reference"})
		return
	}
	wp, created, err := h.store.CreateWorkpaper(r.Context(), domain.CreateWorkpaperParams{
		EngagementID: engagement.EngagementID, TenantID: tenantID, Reference: req.Reference, Purpose: req.Purpose,
		Required: req.Required, CreatedByPrincipalID: actor, CorrelationID: correlationID,
	})
	if err != nil {
		h.writeWorkpaperErr(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, wp)
}

func (h *Handler) getWorkpaperForAccess(w http.ResponseWriter, r *http.Request, action string) (*domain.Workpaper, bool) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return nil, false
	}
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return nil, false
	}
	wp, err := h.store.GetWorkpaper(r.Context(), tenantID, chi.URLParam(r, "workpaper_id"))
	if err != nil {
		h.writeWorkpaperErr(w, err)
		return nil, false
	}
	engagement, err := h.store.GetAuditEngagement(r.Context(), tenantID, wp.EngagementID)
	if err != nil {
		h.writeAuditEngagementErr(w, err)
		return nil, false
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, engagement.LegalEntityID, action); err != nil {
		h.writeAuditEngagementAuthzErr(w, err)
		return nil, false
	}
	return wp, true
}

func (h *Handler) GetWorkpaper(w http.ResponseWriter, r *http.Request) {
	wp, ok := h.getWorkpaperForAccess(w, r, actionWorkpaperRead)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, wp)
}

type descriptionRequest struct {
	Description string `json:"description"`
}

func (h *Handler) RecordProcedure(w http.ResponseWriter, r *http.Request) {
	wp, ok := h.getWorkpaperForAccess(w, r, actionWorkpaperManage)
	if !ok {
		return
	}
	var req descriptionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Description == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "description"})
		return
	}
	if err := h.store.RecordProcedure(r.Context(), domain.RecordProcedureParams{WorkpaperID: wp.WorkpaperID, TenantID: wp.TenantID, Description: req.Description}); err != nil {
		h.writeWorkpaperErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "recorded"})
}

func (h *Handler) RecordResult(w http.ResponseWriter, r *http.Request) {
	wp, ok := h.getWorkpaperForAccess(w, r, actionWorkpaperManage)
	if !ok {
		return
	}
	var req descriptionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Description == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "description"})
		return
	}
	if err := h.store.RecordResult(r.Context(), domain.RecordResultParams{WorkpaperID: wp.WorkpaperID, TenantID: wp.TenantID, Description: req.Description}); err != nil {
		h.writeWorkpaperErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "recorded"})
}

func (h *Handler) RecordConclusion(w http.ResponseWriter, r *http.Request) {
	wp, ok := h.getWorkpaperForAccess(w, r, actionWorkpaperManage)
	if !ok {
		return
	}
	var req descriptionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Description == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "description"})
		return
	}
	if err := h.store.RecordConclusion(r.Context(), domain.RecordConclusionParams{WorkpaperID: wp.WorkpaperID, TenantID: wp.TenantID, Description: req.Description}); err != nil {
		h.writeWorkpaperErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "recorded"})
}

type addCrossReferenceRequest struct {
	ToWorkpaperID string `json:"to_workpaper_id"`
}

func (h *Handler) AddCrossReference(w http.ResponseWriter, r *http.Request) {
	wp, ok := h.getWorkpaperForAccess(w, r, actionWorkpaperManage)
	if !ok {
		return
	}
	var req addCrossReferenceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ToWorkpaperID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "to_workpaper_id"})
		return
	}
	if err := h.store.AddWorkpaperCrossReference(r.Context(), domain.AddWorkpaperCrossReferenceParams{
		FromWorkpaperID: wp.WorkpaperID, ToWorkpaperID: req.ToWorkpaperID, TenantID: wp.TenantID,
	}); err != nil {
		h.writeWorkpaperErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "linked"})
}

type linkEvidenceRequest struct {
	EvidenceID string `json:"evidence_id"`
}

// LinkEvidence calls out to document-vault-svc's AUD-06 evidence read to
// learn whether this evidence carries a contradiction flag — "contradictory
// evidence linked," surfaced on the workpaper itself, never dropped.
func (h *Handler) LinkEvidence(w http.ResponseWriter, r *http.Request) {
	wp, ok := h.getWorkpaperForAccess(w, r, actionWorkpaperManage)
	if !ok {
		return
	}
	actor, _ := h.requirePrincipal(w, r)
	var req linkEvidenceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.EvidenceID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "evidence_id"})
		return
	}
	contradictionFlag := false
	if h.evidence != nil {
		flag, err := h.evidence.GetContradictionFlag(r.Context(), wp.TenantID, req.EvidenceID, actor, r.Header.Get("X-Correlation-ID"))
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "evidence_service_unavailable"})
			return
		}
		contradictionFlag = flag
	}
	if err := h.store.LinkWorkpaperEvidence(r.Context(), domain.LinkWorkpaperEvidenceParams{
		WorkpaperID: wp.WorkpaperID, TenantID: wp.TenantID, EvidenceID: req.EvidenceID, LinkedByPrincipalID: actor, ContradictionFlag: contradictionFlag,
	}); err != nil {
		h.writeWorkpaperErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"status": "linked", "contradiction_flag": contradictionFlag})
}

func (h *Handler) MarkWorkpaperPrepared(w http.ResponseWriter, r *http.Request) {
	wp, ok := h.getWorkpaperForAccess(w, r, actionWorkpaperManage)
	if !ok {
		return
	}
	actor, _ := h.requirePrincipal(w, r)
	correlationID, ok := h.requireCorrelationID(w, r)
	if !ok {
		return
	}
	updated, _, err := h.store.MarkWorkpaperPrepared(r.Context(), domain.MarkWorkpaperPreparedParams{WorkpaperID: wp.WorkpaperID, TenantID: wp.TenantID, ActorPrincipalID: actor, CorrelationID: correlationID})
	if err != nil {
		h.writeWorkpaperErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// LockWorkpaper is the real enforcement point for "purpose/procedure/
// result/conclusion mandatory" and "lock digest generated" — see
// PgStore.LockWorkpaper's own doc comment.
func (h *Handler) LockWorkpaper(w http.ResponseWriter, r *http.Request) {
	wp, ok := h.getWorkpaperForAccess(w, r, actionWorkpaperLock)
	if !ok {
		return
	}
	actor, _ := h.requirePrincipal(w, r)
	correlationID, ok := h.requireCorrelationID(w, r)
	if !ok {
		return
	}
	locked, _, err := h.store.LockWorkpaper(r.Context(), domain.LockWorkpaperParams{WorkpaperID: wp.WorkpaperID, TenantID: wp.TenantID, ActorPrincipalID: actor, CorrelationID: correlationID})
	if err != nil {
		h.writeWorkpaperErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, locked)
}

type addPostLockAddendumRequest struct {
	Reason  string `json:"reason"`
	Effect  string `json:"effect"`
	Content string `json:"content"`
}

func (h *Handler) AddPostLockAddendum(w http.ResponseWriter, r *http.Request) {
	wp, ok := h.getWorkpaperForAccess(w, r, actionWorkpaperManage)
	if !ok {
		return
	}
	actor, _ := h.requirePrincipal(w, r)
	var req addPostLockAddendumRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	addendum, err := h.store.AddPostLockAddendum(r.Context(), domain.AddPostLockAddendumParams{
		WorkpaperID: wp.WorkpaperID, TenantID: wp.TenantID, ActorPrincipalID: actor, Reason: req.Reason, Effect: req.Effect, Content: req.Content,
	})
	if err != nil {
		h.writeWorkpaperErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, addendum)
}

func (h *Handler) writeWorkpaperErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrWorkpaperNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "workpaper_not_found"})
	case errors.Is(err, domain.ErrWorkpaperInvalidState):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "invalid_workpaper_state"})
	case errors.Is(err, domain.ErrWorkpaperLockRequiresContent):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "lock_requires_content"})
	case errors.Is(err, domain.ErrWorkpaperAddendumIncomplete):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "addendum_requires_reason_and_effect"})
	default:
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
	}
}
