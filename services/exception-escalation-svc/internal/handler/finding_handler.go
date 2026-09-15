package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"zoiko.io/exception-escalation-svc/internal/domain"
	"zoiko.io/exception-escalation-svc/internal/middleware"
	"zoiko.io/exception-escalation-svc/internal/store"
)

const (
	actionFindingCreate = "AUDIT_FINDING_CREATE"
	actionFindingManage = "AUDIT_FINDING_MANAGE"
	actionFindingClose  = "AUDIT_FINDING_CLOSE"
)

// findingHandler embeds *Handler so it reuses requirePrincipal/writeAuthzErr
// without duplicating them — same composition pattern used for AUD-05/06.
type findingHandler struct {
	*Handler
	store store.FindingStore
}

func RegisterFindingRoutes(r chi.Router, h *Handler, findingStore store.FindingStore) {
	fh := &findingHandler{Handler: h, store: findingStore}
	r.Route("/v1/audit-findings", func(r chi.Router) {
		r.Post("/", fh.CreateFinding)
		r.Get("/{finding_id}", fh.GetFinding)
		r.Get("/{finding_id}/misstatement-summary", fh.GetMisstatementSummary)
		r.Post("/{finding_id}/misstatements", fh.AccumulateMisstatement)
		r.Post("/{finding_id}/materiality", fh.RecordMaterialityEvaluation)
		r.Post("/{finding_id}/communicate", fh.CommunicateFinding)
		r.Post("/{finding_id}/management-response", fh.RecordManagementResponse)
		r.Post("/{finding_id}/remediation", fh.LinkRemediation)
		r.Post("/{finding_id}/scope-limitations", fh.RecordScopeLimitation)
		r.Get("/{finding_id}/scope-limitations/open", fh.ListOpenScopeLimitations)
		r.Post("/{finding_id}/close", fh.CloseFinding)
		r.Post("/{finding_id}/reopen", fh.ReopenFinding)
	})
	r.Post("/v1/audit-findings/scope-limitations/{limitation_id}/resolve", fh.ResolveScopeLimitation)
}

type createFindingRequest struct {
	ExceptionCaseID             string             `json:"exception_case_id"`
	LegalEntityID               string             `json:"legal_entity_id"`
	EngagementID                string             `json:"engagement_id"`
	FindingType                 domain.FindingType `json:"finding_type"`
	RequiresRemediationEvidence bool               `json:"requires_remediation_evidence"`
}

func (h *findingHandler) CreateFinding(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req createFindingRequest
	if !decodeFindingJSON(w, r, &req) {
		return
	}
	if req.ExceptionCaseID == "" || req.LegalEntityID == "" || req.EngagementID == "" || req.FindingType == "" {
		writeError(w, http.StatusBadRequest, "exception_case_id, legal_entity_id, engagement_id, and finding_type are required")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionFindingCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	finding, err := h.store.CreateFinding(r.Context(), domain.CreateFindingParams{
		ExceptionCaseID: req.ExceptionCaseID, TenantID: middleware.GetTenantID(r.Context()), LegalEntityID: req.LegalEntityID,
		EngagementID: req.EngagementID, FindingType: req.FindingType, RequiresRemediationEvidence: req.RequiresRemediationEvidence,
		CreatedByPrincipalID: principalID,
	})
	if err != nil {
		h.writeFindingErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, finding)
}

func (h *findingHandler) GetFinding(w http.ResponseWriter, r *http.Request) {
	finding, err := h.store.GetFinding(r.Context(), chi.URLParam(r, "finding_id"))
	if err != nil {
		h.writeFindingErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, finding)
}

// GetMisstatementSummary is the AUD-NEG-028 read — see
// PgStore.GetMisstatementSummary's own doc comment for how "is_corrected"
// is derived rather than stored-and-mutated.
func (h *findingHandler) GetMisstatementSummary(w http.ResponseWriter, r *http.Request) {
	summary, err := h.store.GetMisstatementSummary(r.Context(), chi.URLParam(r, "finding_id"))
	if err != nil {
		h.writeFindingErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

type accumulateMisstatementRequest struct {
	Amount                 float64 `json:"amount"`
	CorrectsMisstatementID *string `json:"corrects_misstatement_id,omitempty"`
}

// AccumulateMisstatement is AUD-NEG-027's own recording half — a
// correction is always a NEW row (see PgStore's own doc comment); it
// never overwrites the original.
func (h *findingHandler) AccumulateMisstatement(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req accumulateMisstatementRequest
	if !decodeFindingJSON(w, r, &req) {
		return
	}
	m, err := h.store.AccumulateMisstatement(r.Context(), domain.AccumulateMisstatementParams{
		FindingID: chi.URLParam(r, "finding_id"), TenantID: middleware.GetTenantID(r.Context()), Amount: req.Amount,
		CorrectsMisstatementID: req.CorrectsMisstatementID, RecordedByPrincipalID: principalID,
	})
	if err != nil {
		h.writeFindingErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

type recordMaterialityEvaluationRequest struct {
	IsMaterial       bool   `json:"is_material"`
	QualitativeNotes string `json:"qualitative_notes"`
}

func (h *findingHandler) RecordMaterialityEvaluation(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req recordMaterialityEvaluationRequest
	if !decodeFindingJSON(w, r, &req) {
		return
	}
	e, err := h.store.RecordMaterialityEvaluation(r.Context(), domain.RecordMaterialityEvaluationParams{
		FindingID: chi.URLParam(r, "finding_id"), TenantID: middleware.GetTenantID(r.Context()), IsMaterial: req.IsMaterial,
		QualitativeNotes: req.QualitativeNotes, EvaluatedByPrincipalID: principalID,
	})
	if err != nil {
		h.writeFindingErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, e)
}

func (h *findingHandler) CommunicateFinding(w http.ResponseWriter, r *http.Request) {
	updated, err := h.store.CommunicateFinding(r.Context(), domain.CommunicateFindingParams{FindingID: chi.URLParam(r, "finding_id"), TenantID: middleware.GetTenantID(r.Context())})
	if err != nil {
		h.writeFindingErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

type recordManagementResponseRequest struct {
	ResponseText    string `json:"response_text"`
	RemediationPlan string `json:"remediation_plan"`
}

func (h *findingHandler) RecordManagementResponse(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req recordManagementResponseRequest
	if !decodeFindingJSON(w, r, &req) {
		return
	}
	m, err := h.store.RecordManagementResponse(r.Context(), domain.RecordManagementResponseParams{
		FindingID: chi.URLParam(r, "finding_id"), TenantID: middleware.GetTenantID(r.Context()), ResponseText: req.ResponseText,
		RemediationPlan: req.RemediationPlan, RespondedByPrincipalID: principalID,
	})
	if err != nil {
		h.writeFindingErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

type linkRemediationRequest struct {
	EvidenceRef string `json:"evidence_ref"`
	Reperformed bool   `json:"reperformed"`
}

func (h *findingHandler) LinkRemediation(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req linkRemediationRequest
	if !decodeFindingJSON(w, r, &req) || req.EvidenceRef == "" {
		writeError(w, http.StatusBadRequest, "evidence_ref is required")
		return
	}
	rem, err := h.store.LinkRemediation(r.Context(), domain.LinkRemediationParams{
		FindingID: chi.URLParam(r, "finding_id"), TenantID: middleware.GetTenantID(r.Context()), EvidenceRef: req.EvidenceRef,
		Reperformed: req.Reperformed, RecordedByPrincipalID: principalID,
	})
	if err != nil {
		h.writeFindingErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rem)
}

type recordScopeLimitationRequest struct {
	Description string `json:"description"`
}

func (h *findingHandler) RecordScopeLimitation(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req recordScopeLimitationRequest
	if !decodeFindingJSON(w, r, &req) || req.Description == "" {
		writeError(w, http.StatusBadRequest, "description is required")
		return
	}
	l, err := h.store.RecordScopeLimitation(r.Context(), domain.RecordScopeLimitationParams{
		FindingID: chi.URLParam(r, "finding_id"), TenantID: middleware.GetTenantID(r.Context()), Description: req.Description, RecordedByPrincipalID: principalID,
	})
	if err != nil {
		h.writeFindingErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, l)
}

// ResolveScopeLimitation never deletes or hides the limitation's own
// record of having once impacted the report — see migration 000002's
// reject_scope_limitation_mutation and AUD-NEG-029.
func (h *findingHandler) ResolveScopeLimitation(w http.ResponseWriter, r *http.Request) {
	updated, err := h.store.ResolveScopeLimitation(r.Context(), domain.ResolveScopeLimitationParams{LimitationID: chi.URLParam(r, "limitation_id"), TenantID: middleware.GetTenantID(r.Context())})
	if err != nil {
		h.writeFindingErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *findingHandler) ListOpenScopeLimitations(w http.ResponseWriter, r *http.Request) {
	limitations, err := h.store.ListOpenScopeLimitations(r.Context(), middleware.GetTenantID(r.Context()), chi.URLParam(r, "finding_id"))
	if err != nil {
		h.writeFindingErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, limitations)
}

type closeFindingRequest struct {
	ClosureNotes string `json:"closure_notes"`
}

// CloseFinding is AUD-NEG-026's own enforcement point — see
// PgStore.CloseFinding's own doc comment for the CAS predicate.
func (h *findingHandler) CloseFinding(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	findingID := chi.URLParam(r, "finding_id")
	finding, err := h.store.GetFinding(r.Context(), findingID)
	if err != nil {
		h.writeFindingErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, finding.LegalEntityID, actionFindingClose); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	var req closeFindingRequest
	if !decodeFindingJSON(w, r, &req) {
		return
	}
	updated, err := h.store.CloseFinding(r.Context(), domain.CloseFindingParams{
		FindingID: findingID, TenantID: middleware.GetTenantID(r.Context()), ClosedByPrincipalID: principalID, ClosureNotes: req.ClosureNotes,
	})
	if err != nil {
		h.writeFindingErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

type reopenFindingRequest struct {
	Reason string `json:"reason"`
}

func (h *findingHandler) ReopenFinding(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req reopenFindingRequest
	if !decodeFindingJSON(w, r, &req) || req.Reason == "" {
		writeError(w, http.StatusBadRequest, "reason is required")
		return
	}
	updated, err := h.store.ReopenFinding(r.Context(), domain.ReopenFindingParams{
		FindingID: chi.URLParam(r, "finding_id"), TenantID: middleware.GetTenantID(r.Context()), ActorPrincipalID: principalID, Reason: req.Reason,
	})
	if err != nil {
		h.writeFindingErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func decodeFindingJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

func (h *findingHandler) writeFindingErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrFindingNotFound):
		writeError(w, http.StatusNotFound, "audit finding not found")
	case errors.Is(err, domain.ErrFindingInvalidState):
		writeError(w, http.StatusUnprocessableEntity, "invalid finding state for this action")
	case errors.Is(err, domain.ErrClosureEvidenceRequired):
		writeError(w, http.StatusUnprocessableEntity, "closure requires linked remediation evidence")
	case errors.Is(err, domain.ErrScopeLimitationNotFound):
		writeError(w, http.StatusNotFound, "scope limitation not found")
	case errors.Is(err, domain.ErrSelfApprovalNotAllowed):
		writeError(w, http.StatusForbidden, domain.ErrSelfApprovalNotAllowed.Error())
	default:
		h.logger.Error("audit finding store error")
		writeError(w, http.StatusInternalServerError, "audit finding store unavailable")
	}
}
