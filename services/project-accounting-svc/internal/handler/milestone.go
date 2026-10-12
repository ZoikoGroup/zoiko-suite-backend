package handler

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/project-accounting-svc/internal/domain"
)

// ── PRJ-03 milestone-based recognition ───────────────────────────────────────
//
// A milestone only contributes to recognised revenue once it is ACHIEVED
// (with evidence) AND the achievement has been APPROVED by a different
// principal. Invoices/billing never enter the figure.

// DefineMilestone: POST /v1/projects/{id}/milestones
func (h *Handler) DefineMilestone(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "id")
	var req domain.DefineMilestoneRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", domain.ErrMilestoneNameRequired.Error())
		return
	}
	if req.Amount <= 0 {
		writeError(w, http.StatusUnprocessableEntity, "invalid_amount", domain.ErrMilestoneAmountInvalid.Error())
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	p, err := h.store.GetProject(r.Context(), projectID)
	if err != nil {
		h.writeProjectErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, p.LegalEntityID, actionProjectMilestoneManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	m := &domain.Milestone{
		MilestoneID: uuid.NewString(), TenantID: tenantID, ProjectID: projectID, Name: req.Name, Amount: req.Amount,
		Status: domain.MilestoneStatusPlanned, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	if err := h.store.DefineMilestone(r.Context(), m); err != nil {
		h.writeMilestoneErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

// ListMilestones: GET /v1/projects/{id}/milestones
func (h *Handler) ListMilestones(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	p, err := h.store.GetProject(r.Context(), projectID)
	if err != nil {
		h.writeProjectErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, p.LegalEntityID, actionProjectRevenueRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	list, err := h.store.ListMilestones(r.Context(), projectID)
	if err != nil {
		h.writeMilestoneErr(w, err)
		return
	}
	if list == nil {
		list = []domain.Milestone{}
	}
	writeJSON(w, http.StatusOK, list)
}

// loadProjectMilestone fetches the milestone and refuses one that does not
// belong to the project in the URL (404, never a cross-project action).
func (h *Handler) loadProjectMilestone(w http.ResponseWriter, r *http.Request) (*domain.Project, *domain.Milestone, bool) {
	projectID := chi.URLParam(r, "id")
	milestoneID := chi.URLParam(r, "milestone_id")
	p, err := h.store.GetProject(r.Context(), projectID)
	if err != nil {
		h.writeProjectErr(w, err)
		return nil, nil, false
	}
	m, err := h.store.GetMilestone(r.Context(), milestoneID)
	if err != nil {
		h.writeMilestoneErr(w, err)
		return nil, nil, false
	}
	if m.ProjectID != projectID {
		h.writeMilestoneErr(w, domain.ErrMilestoneNotFound)
		return nil, nil, false
	}
	return p, m, true
}

// MarkMilestoneAchieved: POST /v1/projects/{id}/milestones/{milestone_id}/achieve
// Evidence is required. The milestone does not count toward revenue until
// ApproveMilestoneAchievement.
func (h *Handler) MarkMilestoneAchieved(w http.ResponseWriter, r *http.Request) {
	var req domain.MarkMilestoneAchievedRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.AchievementEvidenceRef) == "" {
		writeError(w, http.StatusUnprocessableEntity, "evidence_required", domain.ErrMilestoneEvidenceRequired.Error())
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	p, m, ok := h.loadProjectMilestone(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, p.LegalEntityID, actionProjectMilestoneManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if err := h.store.MarkMilestoneAchieved(r.Context(), m.MilestoneID, principalID, req.AchievementEvidenceRef, time.Now().UTC()); err != nil {
		h.writeMilestoneErr(w, err)
		return
	}
	updated, err := h.store.GetMilestone(r.Context(), m.MilestoneID)
	if err != nil {
		h.writeMilestoneErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// ApproveMilestoneAchievement: POST /v1/projects/{id}/milestones/{milestone_id}/approve
// Refuses self-approval (the principal who marked it achieved).
func (h *Handler) ApproveMilestoneAchievement(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	p, m, ok := h.loadProjectMilestone(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, p.LegalEntityID, actionProjectMilestoneApprove); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if err := h.store.ApproveMilestoneAchievement(r.Context(), m.MilestoneID, principalID, time.Now().UTC()); err != nil {
		h.writeMilestoneErr(w, err)
		return
	}
	updated, err := h.store.GetMilestone(r.Context(), m.MilestoneID)
	if err != nil {
		h.writeMilestoneErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// GetRunMilestones: GET /v1/recognition/runs/{id}/milestones — the
// insert-only evidence of which milestones a run was built from.
func (h *Handler) GetRunMilestones(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	run, err := h.store.GetRecognitionRun(r.Context(), id)
	if err != nil {
		h.writeRecognitionErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, run.LegalEntityID, actionProjectRevenueRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	list, err := h.store.ListRunMilestones(r.Context(), id)
	if err != nil {
		h.writeMilestoneErr(w, err)
		return
	}
	if list == nil {
		list = []domain.RunMilestone{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *Handler) writeMilestoneErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrMilestoneNotFound):
		writeError(w, http.StatusNotFound, "milestone_not_found", "")
	case errors.Is(err, domain.ErrDuplicateMilestoneName):
		writeError(w, http.StatusUnprocessableEntity, "duplicate_milestone_name", err.Error())
	case errors.Is(err, domain.ErrMilestoneEvidenceRequired):
		writeError(w, http.StatusUnprocessableEntity, "evidence_required", err.Error())
	case errors.Is(err, domain.ErrInvalidMilestoneTransition):
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", err.Error())
	case errors.Is(err, domain.ErrSelfApprovalNotPermittedMilestone):
		writeError(w, http.StatusForbidden, "self_approval_not_permitted", err.Error())
	default:
		h.log.Error("milestone store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
	}
}
