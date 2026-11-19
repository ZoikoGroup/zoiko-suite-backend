package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/domain"
	"zoiko.io/workflow-svc/internal/store"
)

const (
	// actionFormManage gates CreateForm — the form owner's own act of
	// authorship. actionFormApprove gates PublishForm/RetireForm — the
	// separate target-domain-mapping approver role the doc names.
	// actionFormSubmit gates the submission lifecycle (SaveDraft,
	// SubmitForm, ValidateSubmission) — a different population again,
	// whoever is filling out the form. actionFormRead gates the two
	// queries.
	actionFormManage  = "FORM_MANAGE"
	actionFormApprove = "FORM_APPROVE"
	actionFormSubmit  = "FORM_SUBMIT"
	actionFormRead    = "FORM_READ"
)

type createFormRequest struct {
	LegalEntityID   string   `json:"legal_entity_id"`
	Name            string   `json:"name"`
	BusinessPurpose string   `json:"business_purpose"`
	TargetDomain    string   `json:"target_domain"`
	Schema          []string `json:"schema,omitempty"`
}

// CreateForm creates a new DRAFT form definition — BIZ-04's own
// CreateForm command. The caller becomes the owner.
func (h *Handler) CreateForm(w http.ResponseWriter, r *http.Request) {
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
	var req createFormRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if req.LegalEntityID == "" || req.Name == "" || req.BusinessPurpose == "" || req.TargetDomain == "" {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "missing_fields"})
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, req.LegalEntityID, actionFormManage, ""); err != nil {
		h.writeFormAuthzErr(w, r, err)
		return
	}

	form, created, err := h.store.CreateForm(r.Context(), domain.CreateFormParams{
		TenantID: tenantID, LegalEntityID: req.LegalEntityID, Name: req.Name, BusinessPurpose: req.BusinessPurpose,
		TargetDomain: req.TargetDomain, Schema: req.Schema, OwnerPrincipalID: actor, CorrelationID: correlationID,
	})
	if err != nil {
		h.writeFormErr(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, r, status, form)
}

// GetForm — BIZ-04's own GetForm query.
func (h *Handler) GetForm(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	form, err := h.store.GetForm(r.Context(), tenantID, chi.URLParam(r, "form_id"))
	if err != nil {
		h.writeFormErr(w, r, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, form.LegalEntityID, actionFormRead, ""); err != nil {
		h.writeFormAuthzErr(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, form)
}

// PublishForm — BIZ-04's own PublishForm command. Fetched (read-only)
// BEFORE authorization and BEFORE the mutation, same
// fetch-then-authorize-then-mutate discipline as every handler in this
// platform.
func (h *Handler) PublishForm(w http.ResponseWriter, r *http.Request) {
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
	formID := chi.URLParam(r, "form_id")
	existing, err := h.store.GetForm(r.Context(), tenantID, formID)
	if err != nil {
		h.writeFormErr(w, r, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, existing.LegalEntityID, actionFormApprove, ""); err != nil {
		h.writeFormAuthzErr(w, r, err)
		return
	}

	published, err := h.store.PublishForm(r.Context(), domain.PublishFormParams{
		FormID: formID, TenantID: tenantID, ActorPrincipalID: actor, CorrelationID: correlationID,
	})
	if err != nil {
		h.writeFormErr(w, r, err)
		return
	}
	if err := h.publisher.PublishFormPublished(r.Context(), *published, actor, correlationID); err != nil {
		h.log.Error("failed to publish form.published event", zap.Error(err))
	}
	writeJSON(w, r, http.StatusOK, published)
}

// RetireForm — BIZ-04's own RetireForm command (filling a real gap in
// the doc's command list — see internal/store/form_store.go's own doc
// comment on RetireForm for why this is a fill, not an invention).
func (h *Handler) RetireForm(w http.ResponseWriter, r *http.Request) {
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
	formID := chi.URLParam(r, "form_id")
	existing, err := h.store.GetForm(r.Context(), tenantID, formID)
	if err != nil {
		h.writeFormErr(w, r, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, existing.LegalEntityID, actionFormApprove, ""); err != nil {
		h.writeFormAuthzErr(w, r, err)
		return
	}

	retired, err := h.store.RetireForm(r.Context(), domain.RetireFormParams{
		FormID: formID, TenantID: tenantID, ActorPrincipalID: actor, CorrelationID: correlationID,
	})
	if err != nil {
		h.writeFormErr(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, retired)
}

type saveDraftRequest struct {
	SubmittedValues    map[string]string `json:"submitted_values,omitempty"`
	ConsentAttestation string            `json:"consent_attestation,omitempty"`
}

// SaveDraft — BIZ-04's own SaveDraft command. Mounted twice: POST
// /v1/forms/{form_id}/drafts creates the first draft; POST
// /v1/form-submissions/{submission_id}/drafts updates an existing one —
// the URL names whichever id it has.
func (h *Handler) SaveDraft(w http.ResponseWriter, r *http.Request) {
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
	var req saveDraftRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}

	formID := chi.URLParam(r, "form_id")
	submissionID := chi.URLParam(r, "submission_id")
	var legalEntityID string
	if formID != "" {
		form, err := h.store.GetForm(r.Context(), tenantID, formID)
		if err != nil {
			h.writeFormErr(w, r, err)
			return
		}
		legalEntityID = form.LegalEntityID
	} else {
		existing, err := h.store.GetSubmission(r.Context(), tenantID, submissionID)
		if err != nil {
			h.writeFormErr(w, r, err)
			return
		}
		form, err := h.store.GetForm(r.Context(), tenantID, existing.FormID)
		if err != nil {
			h.writeFormErr(w, r, err)
			return
		}
		legalEntityID = form.LegalEntityID
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, legalEntityID, actionFormSubmit, ""); err != nil {
		h.writeFormAuthzErr(w, r, err)
		return
	}

	submission, err := h.store.SaveDraft(r.Context(), domain.SaveDraftParams{
		SubmissionID: submissionID, FormID: formID, TenantID: tenantID, SubmitterPrincipalID: actor,
		CorrelationID: correlationID, SubmittedValues: req.SubmittedValues, ConsentAttestation: req.ConsentAttestation,
	})
	if err != nil {
		h.writeFormErr(w, r, err)
		return
	}
	status := http.StatusOK
	if submissionID == "" {
		status = http.StatusCreated
	}
	writeJSON(w, r, status, submission)
}

// SubmitForm — BIZ-04's own SubmitForm command.
func (h *Handler) SubmitForm(w http.ResponseWriter, r *http.Request) {
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
	submissionID := chi.URLParam(r, "submission_id")
	existing, err := h.store.GetSubmission(r.Context(), tenantID, submissionID)
	if err != nil {
		h.writeFormErr(w, r, err)
		return
	}
	form, err := h.store.GetForm(r.Context(), tenantID, existing.FormID)
	if err != nil {
		h.writeFormErr(w, r, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, form.LegalEntityID, actionFormSubmit, ""); err != nil {
		h.writeFormAuthzErr(w, r, err)
		return
	}

	submitted, err := h.store.SubmitForm(r.Context(), domain.SubmitFormParams{
		SubmissionID: submissionID, TenantID: tenantID, ActorPrincipalID: actor, CorrelationID: correlationID,
	})
	if err != nil {
		h.writeFormErr(w, r, err)
		return
	}
	if err := h.publisher.PublishFormEvent(r.Context(), "form.submission.received", *submitted, actor, correlationID); err != nil {
		h.log.Error("failed to publish form.submission.received event", zap.Error(err))
	}
	writeJSON(w, r, http.StatusOK, submitted)
}

// ValidateSubmission — BIZ-04's own ValidateSubmission command.
func (h *Handler) ValidateSubmission(w http.ResponseWriter, r *http.Request) {
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
	submissionID := chi.URLParam(r, "submission_id")
	existing, err := h.store.GetSubmission(r.Context(), tenantID, submissionID)
	if err != nil {
		h.writeFormErr(w, r, err)
		return
	}
	form, err := h.store.GetForm(r.Context(), tenantID, existing.FormID)
	if err != nil {
		h.writeFormErr(w, r, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, form.LegalEntityID, actionFormSubmit, ""); err != nil {
		h.writeFormAuthzErr(w, r, err)
		return
	}

	validated, err := h.store.ValidateSubmission(r.Context(), domain.ValidateSubmissionParams{
		SubmissionID: submissionID, TenantID: tenantID, ActorPrincipalID: actor, CorrelationID: correlationID,
	})
	if err != nil {
		h.writeFormErr(w, r, err)
		return
	}
	eventType := "form.submission.validated"
	if validated.Status == domain.FormSubmissionRejected {
		eventType = "form.submission.rejected"
	}
	if err := h.publisher.PublishFormEvent(r.Context(), eventType, *validated, actor, correlationID); err != nil {
		h.log.Error("failed to publish form validation event", zap.Error(err))
	}
	writeJSON(w, r, http.StatusOK, validated)
}

// GetSubmission — BIZ-04's own GetSubmission query.
func (h *Handler) GetSubmission(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	submissionID := chi.URLParam(r, "submission_id")
	submission, err := h.store.GetSubmission(r.Context(), tenantID, submissionID)
	if err != nil {
		h.writeFormErr(w, r, err)
		return
	}
	form, err := h.store.GetForm(r.Context(), tenantID, submission.FormID)
	if err != nil {
		h.writeFormErr(w, r, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, form.LegalEntityID, actionFormRead, ""); err != nil {
		h.writeFormAuthzErr(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, submission)
}

type supersedeSubmissionRequest struct {
	NewSubmissionID string `json:"new_submission_id"`
}

// SupersedeSubmission — BIZ-04's own SupersedeSubmission command. The
// submissionID in the URL is the PREVIOUS (currently governing)
// submission; the replacement is named in the body.
func (h *Handler) SupersedeSubmission(w http.ResponseWriter, r *http.Request) {
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
	var req supersedeSubmissionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	submissionID := chi.URLParam(r, "submission_id")
	existing, err := h.store.GetSubmission(r.Context(), tenantID, submissionID)
	if err != nil {
		h.writeFormErr(w, r, err)
		return
	}
	form, err := h.store.GetForm(r.Context(), tenantID, existing.FormID)
	if err != nil {
		h.writeFormErr(w, r, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, form.LegalEntityID, actionFormSubmit, ""); err != nil {
		h.writeFormAuthzErr(w, r, err)
		return
	}

	superseded, err := h.store.SupersedeSubmission(r.Context(), domain.SupersedeSubmissionParams{
		PreviousSubmissionID: submissionID, NewSubmissionID: req.NewSubmissionID, TenantID: tenantID,
		ActorPrincipalID: actor, CorrelationID: correlationID,
	})
	if err != nil {
		h.writeFormErr(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, superseded)
}

type routeToDomainRequest struct {
	CommandReference string `json:"command_reference,omitempty"`
	ResultReference  string `json:"result_reference,omitempty"`
	Outcome          string `json:"outcome"`
	FailureReason    string `json:"failure_reason,omitempty"`
}

// RouteToDomain — BIZ-04's own RouteToDomain command.
func (h *Handler) RouteToDomain(w http.ResponseWriter, r *http.Request) {
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
	var req routeToDomainRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if req.Outcome != "SUCCEEDED" && req.Outcome != "FAILED" {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid_outcome"})
		return
	}
	submissionID := chi.URLParam(r, "submission_id")
	existing, err := h.store.GetSubmission(r.Context(), tenantID, submissionID)
	if err != nil {
		h.writeFormErr(w, r, err)
		return
	}
	form, err := h.store.GetForm(r.Context(), tenantID, existing.FormID)
	if err != nil {
		h.writeFormErr(w, r, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, form.LegalEntityID, actionFormSubmit, ""); err != nil {
		h.writeFormAuthzErr(w, r, err)
		return
	}

	route, err := h.store.RouteToDomain(r.Context(), domain.RouteToDomainParams{
		SubmissionID: submissionID, TenantID: tenantID, ActorPrincipalID: actor, CorrelationID: correlationID,
		CommandReference: req.CommandReference, ResultReference: req.ResultReference, Outcome: req.Outcome, FailureReason: req.FailureReason,
	})
	if err != nil {
		h.writeFormErr(w, r, err)
		return
	}
	if err := h.publisher.PublishFormEvent(r.Context(), "form.submission.routed", *existing, actor, correlationID); err != nil {
		h.log.Error("failed to publish form.submission.routed event", zap.Error(err))
	}
	writeJSON(w, r, http.StatusCreated, route)
}

// GetSubmissionVersion — BIZ-04's own GetSubmissionVersion query.
func (h *Handler) GetSubmissionVersion(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	submissionID := chi.URLParam(r, "submission_id")
	existing, err := h.store.GetSubmission(r.Context(), tenantID, submissionID)
	if err != nil {
		h.writeFormErr(w, r, err)
		return
	}
	form, err := h.store.GetForm(r.Context(), tenantID, existing.FormID)
	if err != nil {
		h.writeFormErr(w, r, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, form.LegalEntityID, actionFormRead, ""); err != nil {
		h.writeFormAuthzErr(w, r, err)
		return
	}

	version, err := h.store.GetSubmissionVersion(r.Context(), tenantID, submissionID)
	if err != nil {
		h.writeFormErr(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, version)
}

// GetValidationResult — BIZ-04's own GetValidationResult query.
func (h *Handler) GetValidationResult(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	submissionID := chi.URLParam(r, "submission_id")
	existing, err := h.store.GetSubmission(r.Context(), tenantID, submissionID)
	if err != nil {
		h.writeFormErr(w, r, err)
		return
	}
	form, err := h.store.GetForm(r.Context(), tenantID, existing.FormID)
	if err != nil {
		h.writeFormErr(w, r, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, form.LegalEntityID, actionFormRead, ""); err != nil {
		h.writeFormAuthzErr(w, r, err)
		return
	}

	result, err := h.store.GetValidationResult(r.Context(), tenantID, submissionID)
	if err != nil {
		h.writeFormErr(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, result)
}

// ListPendingSubmissions — BIZ-04's own ListPendingSubmissions query,
// scoped to one form so authorization stays meaningful.
func (h *Handler) ListPendingSubmissions(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	formID := chi.URLParam(r, "form_id")
	form, err := h.store.GetForm(r.Context(), tenantID, formID)
	if err != nil {
		h.writeFormErr(w, r, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, form.LegalEntityID, actionFormRead, ""); err != nil {
		h.writeFormAuthzErr(w, r, err)
		return
	}

	limit := getIntQuery(r, "limit", 50)
	if limit > 200 {
		limit = 200
	}

	var cursor *domain.PaginationCursor
	if cursorStr := r.URL.Query().Get("cursor"); cursorStr != "" {
		c, err := domain.DecodePaginationCursor(cursorStr)
		if err != nil {
			writeJSON(w, r, http.StatusBadRequest, map[string]string{
				"error":   "invalid_cursor",
				"message": "cursor is not valid",
			})
			return
		}
		cursor = c
	}

	pending, nextCursor, err := h.store.ListPendingSubmissions(r.Context(), tenantID, formID, domain.ListPendingSubmissionsParams{
		TenantID: tenantID,
		FormID:   formID,
		Limit:    limit,
		Cursor:   cursor,
	})
	if err != nil {
		h.writeFormErr(w, r, err)
		return
	}
	if pending == nil {
		pending = []*domain.FormSubmission{}
	}

	type listResponse struct {
		Records    []*domain.FormSubmission `json:"records"`
		NextCursor string                   `json:"next_cursor,omitempty"`
	}

	resp := listResponse{
		Records: pending,
	}
	if nextCursor != nil {
		if encoded, err := domain.EncodePaginationCursor(nextCursor); err == nil {
			resp.NextCursor = encoded
		}
	}

	writeJSON(w, r, http.StatusOK, resp)
}

func (h *Handler) writeFormErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrFormNotFound):
		writeJSON(w, r, http.StatusNotFound, map[string]string{"error": "form_not_found"})
	case errors.Is(err, domain.ErrFormSubmissionNotFound):
		writeJSON(w, r, http.StatusNotFound, map[string]string{"error": "form_submission_not_found"})
	case errors.Is(err, domain.ErrFormRetired):
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": "form_retired"})
	case errors.Is(err, domain.ErrFormNotDraft):
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": "form_not_draft"})
	case errors.Is(err, domain.ErrFormNotPublished):
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": "form_not_published"})
	case errors.Is(err, domain.ErrFormAlreadyRetired):
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": "form_already_retired"})
	case errors.Is(err, domain.ErrFormSelfPublish):
		writeJSON(w, r, http.StatusForbidden, map[string]string{"error": "self_publish_not_allowed"})
	case errors.Is(err, domain.ErrFormSubmissionNotDraft):
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": "submission_not_draft"})
	case errors.Is(err, domain.ErrFormSubmissionNotSubmitted):
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": "submission_not_submitted"})
	case errors.Is(err, domain.ErrFormSubmissionNotAccepted):
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": "submission_not_accepted"})
	case errors.Is(err, domain.ErrFormSubmissionNotAcceptedOrRejected):
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": "submission_not_accepted_or_rejected"})
	case errors.Is(err, domain.ErrFormSubmissionAlreadySuperseded):
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": "submission_already_superseded"})
	case errors.Is(err, domain.ErrFormSubmissionsBelongToDifferentForms):
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "submissions_belong_to_different_forms"})
	case errors.Is(err, domain.ErrFormSubmissionNotYetValidated):
		writeJSON(w, r, http.StatusNotFound, map[string]string{"error": "not_yet_validated"})
	default:
		writeJSON(w, r, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
	}
}

func (h *Handler) writeFormAuthzErr(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, domain.ErrAuthorizationDenied) {
		writeJSON(w, r, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	writeJSON(w, r, http.StatusServiceUnavailable, map[string]string{"error": "authorization_unavailable"})
}

// =============================================================================
// WFC-04: Cases (WF-2)
// =============================================================================

type createCaseRequest struct {
	WorkflowInstanceID    *string `json:"workflow_instance_id,omitempty"`
	CaseType              string  `json:"case_type"`                // INVESTIGATION, DISPUTE, EXCEPTION, etc.
	Title                 string  `json:"title"`
	Description           string  `json:"description"`
	AssignedToPrincipalID *string `json:"assigned_to_principal_id,omitempty"`
}

func (req createCaseRequest) missingField() string {
	switch {
	case req.CaseType == "":
		return "case_type"
	case req.Title == "":
		return "title"
	default:
		return ""
	}
}

// CreateCase handles POST /v1/cases.
// Creates a new case (optionally linked to a workflow instance).
func (h *Handler) CreateCase(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	var req createCaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", "invalid JSON body", correlationID, nil)
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": missing,
		})
		return
	}

	// Verify workflow exists if provided
	if req.WorkflowInstanceID != nil {
		_, err := h.store.FindWorkflowByID(r.Context(), *req.WorkflowInstanceID)
		if err != nil {
			writeStoreErr(w, r, h.log, err, correlationID, "CreateCase")
			return
		}
	}

	// Authorize against legal entity (need to get from workflow or create with tenant scope)
	// For now, require AUDIT_ENGAGEMENT_MANAGE or similar
	// TODO: Add specific CASE_CREATE action
	if !h.authorizeRead(w, r, nil, principalID, correlationID, "CreateCase") {
		return
	}

	params := domain.CreateCaseParams{
		TenantID:             tenantID,
		LegalEntityID:        "", // will be set from workflow or require in request
		WorkflowInstanceID:   req.WorkflowInstanceID,
		CaseType:             req.CaseType,
		Title:                req.Title,
		Description:          req.Description,
		AssignedToPrincipal:  req.AssignedToPrincipalID,
		CreatedByPrincipalID: principalID,
		CorrelationID:        correlationID,
	}

	// If linked to workflow, get legal entity from workflow
	if req.WorkflowInstanceID != nil {
		wf, err := h.store.FindWorkflowByID(r.Context(), *req.WorkflowInstanceID)
		if err != nil {
			writeStoreErr(w, r, h.log, err, correlationID, "CreateCase")
			return
		}
		params.LegalEntityID = wf.LegalEntityID
	} else {
		// Need legal entity in request for standalone case
		writeJSON(w, r, http.StatusBadRequest, map[string]string{
			"error":  "missing_field",
			"field":  "legal_entity_id",
			"detail": "required when not linking to a workflow",
		})
		return
	}

	c, err := h.store.CreateCase(r.Context(), params)
	if err != nil {
		writeStoreErr(w, r, h.log, err, correlationID, "CreateCase")
		return
	}

	h.log.Info("case created",
		zap.String("case_id", c.CaseID),
		zap.String("case_type", c.CaseType),
		zap.String("correlation_id", correlationID),
	)
	w.Header().Set("Location", "/v1/cases/"+c.CaseID)
	writeJSON(w, r, http.StatusCreated, c)
}

// GetCase handles GET /v1/cases/{case_id}.
func (h *Handler) GetCase(w http.ResponseWriter, r *http.Request) {
	caseID := chi.URLParam(r, "case_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	_, ok = h.requireTenant(w, r)
	if !ok {
		return
	}

	c, err := h.store.FindCaseByID(r.Context(), caseID)
	if err != nil {
		writeStoreErr(w, r, h.log, err, r.Header.Get("X-Correlation-ID"), "GetCase")
		return
	}

	if !h.authorizeRead(w, r, nil, principalID, r.Header.Get("X-Correlation-ID"), "GetCase") {
		return
	}

	writeJSONWithETag(w, r, http.StatusOK, c, c.RowVersion)
}

// ListCases handles GET /v1/cases.
// Lists cases with optional filters.
func (h *Handler) ListCases(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	if !h.authorizeRead(w, r, nil, principalID, r.Header.Get("X-Correlation-ID"), "ListCases") {
		return
	}

	limit := getIntQuery(r, "limit", 50)
	if limit > 200 {
		limit = 200
	}

	filter := domain.ListCasesFilter{
		TenantID:       tenantID,
		LegalEntityID:  r.URL.Query().Get("legal_entity_id"),
		CaseType:       r.URL.Query().Get("case_type"),
		CaseState:      domain.CaseState(r.URL.Query().Get("case_state")),
		Limit:          limit,
		Offset:         getIntQuery(r, "offset", 0),
	}

	if wfID := r.URL.Query().Get("workflow_instance_id"); wfID != "" {
		filter.WorkflowInstanceID = &wfID
	}

	results, err := h.store.ListCases(r.Context(), filter)
	if err != nil {
		writeStoreErr(w, r, h.log, err, r.Header.Get("X-Correlation-ID"), "ListCases")
		return
	}

	if results == nil {
		results = []*domain.Case{}
	}
	writeJSON(w, r, http.StatusOK, results)
}

type transitionCaseRequest struct {
	CaseState          string  `json:"case_state"`                    // IN_REVIEW, RESOLVED, CLOSED, ESCALATED
	ExpectedVersion    int     `json:"expected_version,omitempty"`    // If-Match version
	ResolutionReason   *string `json:"resolution_reason,omitempty"`   // for RESOLVED
	CloseReason        *string `json:"close_reason,omitempty"`        // for CLOSED
}

// TransitionCase handles POST /v1/cases/{case_id}/status.
// Transitions a case to a new state.
func (h *Handler) TransitionCase(w http.ResponseWriter, r *http.Request) {
	caseID := chi.URLParam(r, "case_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	_, ok = h.requireTenant(w, r)
	if !ok {
		return
	}

	var req transitionCaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", "invalid JSON body", correlationID, nil)
		return
	}
	if req.CaseState == "" {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": "case_state",
		})
		return
	}
	if !domain.CaseStates[domain.CaseState(req.CaseState)] {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{
			"error":  "invalid_field",
			"field":  "case_state",
			"detail": "case_state must be a valid case state",
		})
		return
	}

	if !h.authorizeRead(w, r, nil, principalID, correlationID, "TransitionCase") {
		return
	}

	// Parse If-Match header
	expectedVersion := req.ExpectedVersion
	if match := r.Header.Get("If-Match"); match != "" {
		match = strings.TrimPrefix(match, "W/")
		match = strings.Trim(match, `"`)
		if v, err := strconv.Atoi(match); err == nil {
			expectedVersion = v
		} else {
			writeJSON(w, r, http.StatusBadRequest, map[string]string{
				"error":  "invalid_if_match",
				"detail": "If-Match must be an integer version",
			})
			return
		}
	}

	params := domain.TransitionCaseParams{
		CaseID:           caseID,
		NewState:         domain.CaseState(req.CaseState),
		ExpectedVersion:  expectedVersion,
		ActorPrincipalID: principalID,
		CorrelationID:    correlationID,
		ResolutionReason: req.ResolutionReason,
		CloseReason:      req.CloseReason,
	}

	updated, transitioned, err := h.store.TransitionCase(r.Context(), params)
	if err != nil {
		writeStoreErr(w, r, h.log, err, correlationID, "TransitionCase")
		return
	}

	h.log.Info("case state transitioned",
		zap.String("case_id", caseID),
		zap.String("new_state", string(updated.CaseState)),
		zap.Bool("transitioned", transitioned),
		zap.String("correlation_id", correlationID),
	)
	writeJSONWithETag(w, r, http.StatusOK, updated, updated.RowVersion)
}

// GetCaseWorkflow handles GET /v1/cases/{case_id}/workflow.
// Returns the linked workflow instance if any.
func (h *Handler) GetCaseWorkflow(w http.ResponseWriter, r *http.Request) {
	caseID := chi.URLParam(r, "case_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	_, ok = h.requireTenant(w, r)
	if !ok {
		return
	}

	c, err := h.store.FindCaseByID(r.Context(), caseID)
	if err != nil {
		writeStoreErr(w, r, h.log, err, r.Header.Get("X-Correlation-ID"), "GetCaseWorkflow")
		return
	}

	if !h.authorizeRead(w, r, nil, principalID, r.Header.Get("X-Correlation-ID"), "GetCaseWorkflow") {
		return
	}

	if c.WorkflowInstanceID == nil {
		writeJSON(w, r, http.StatusNotFound, map[string]string{"error": "no_linked_workflow"})
		return
	}

	wf, err := h.store.FindWorkflowByID(r.Context(), *c.WorkflowInstanceID)
	if err != nil {
		writeStoreErr(w, r, h.log, err, r.Header.Get("X-Correlation-ID"), "GetCaseWorkflow")
		return
	}

	writeJSON(w, r, http.StatusOK, wf)
}

// =============================================================================
// ST-1/2/3: State Machine Registry
// =============================================================================

// GetStateMachine handles GET /v1/state-machines/{workflow_type}.
// Returns the state machine definition for a workflow type.
func (h *Handler) GetStateMachine(w http.ResponseWriter, r *http.Request) {
	workflowType := chi.URLParam(r, "workflow_type")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	// Authorization: WORKFLOW_READ on platform scope
	if !h.authorizeRead(w, r, nil, principalID, r.Header.Get("X-Correlation-ID"), "GetStateMachine") {
		return
	}

	// Look up the workflow definition to get its stage structure
	defs, _, err := h.store.ListWorkflowDefinitions(r.Context(), domain.ListWorkflowDefinitionsParams{
		TenantID:     tenantID,
		WorkflowType: &workflowType,
		IsActive:     ptrBool(true),
		Limit:        1,
	})
	if err != nil {
		writeStoreErr(w, r, h.log, err, r.Header.Get("X-Correlation-ID"), "GetStateMachine")
		return
	}

	if len(defs) == 0 {
		writeJSON(w, r, http.StatusNotFound, map[string]string{
			"error": "state_machine_not_found",
			"detail": "no active workflow definition for type: " + workflowType,
		})
		return
	}

	def := defs[0]
	// Build state machine from definition stages
	states := []string{"PENDING"}
	transitions := map[string][]string{
		"PENDING": {"APPROVED", "REJECTED", "ESCALATED", "CANCELLED", "EXPIRED"},
	}

	var defStages []domain.DefinitionStage
	if len(def.StagesJSON) > 0 {
		if err := json.Unmarshal(def.StagesJSON, &defStages); err != nil {
			h.log.Warn("failed to unmarshal stages_json", zap.Error(err))
		} else {
			for _, stage := range defStages {
				states = append(states, "STAGE_"+strconv.Itoa(stage.Order))
			}
		}
	}
	states = append(states, "APPROVED", "REJECTED", "ESCALATED", "CANCELLED", "EXPIRED")

	response := map[string]any{
		"workflow_type":    def.WorkflowType,
		"definition_id":    def.WorkflowDefinitionID,
		"version":          def.Version,
		"states":           states,
		"transitions":      transitions,
		"initial_state":    "PENDING",
		"terminal_states":  []string{"APPROVED", "REJECTED", "CANCELLED", "EXPIRED"},
		"requires_human":   true,
	}
	writeJSON(w, r, http.StatusOK, response)
}

// GetAvailableActions handles GET /v1/workflows/{workflow_instance_id}/available-actions.
// Returns the actions available for the current principal on this workflow.
// ST-2 endpoint.
func (h *Handler) GetAvailableActions(w http.ResponseWriter, r *http.Request) {
	workflowInstanceID := chi.URLParam(r, "workflow_instance_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	instance, err := h.store.FindWorkflowByID(r.Context(), workflowInstanceID)
	if err != nil {
		writeStoreErr(w, r, h.log, err, r.Header.Get("X-Correlation-ID"), "GetAvailableActions")
		return
	}

	if instance.TenantID != tenantID {
		writeJSON(w, r, http.StatusNotFound, map[string]string{"error": "workflow_not_found"})
		return
	}

	// Check if principal can act on this workflow
	canAct := false
	reason := ""
	actions := []string{}

	if instance.WorkflowStatus == domain.WorkflowStatusPending {
		stage, err := h.store.FindCurrentStage(r.Context(), workflowInstanceID)
		if err != nil {
			writeStoreErr(w, r, h.log, err, r.Header.Get("X-Correlation-ID"), "GetAvailableActions")
			return
		}

		if stage.ApproverPrincipalID == principalID {
			canAct = true
			actions = []string{"APPROVE", "REJECT", "REQUEST_CHANGES"}
		} else if stage.StageType == domain.StageTypeQuorum {
			// Check if principal is in quorum pool
			// For now, check via store
			var eligible bool
			err := h.store.(*store.PgStore).Pool().QueryRow(r.Context(),
				`SELECT EXISTS(SELECT 1 FROM workflow_stage_quorum_approvers WHERE workflow_stage_id = $1 AND approver_principal_id = $2)`,
				stage.WorkflowStageID, principalID).Scan(&eligible)
			if err == nil && eligible {
				canAct = true
				actions = []string{"APPROVE", "REJECT"}
			}
		} else if instance.InitiatedBy == principalID {
			// Initiator can cancel
			canAct = true
			actions = []string{"CANCEL"}
		}

		// Approvers and admins can escalate
		if canAct || stage.ApproverPrincipalID == principalID {
			actions = append(actions, "ESCALATE", "ESCALATE_TO")
		}

		if !canAct && stage.ApproverPrincipalID != principalID {
			reason = "not the current approver for this stage"
		}
	} else if instance.WorkflowStatus == domain.WorkflowStatusEscalated {
		// Escalation target can act
		_, err := h.store.FindCurrentStage(r.Context(), workflowInstanceID)
		if err == nil && instance.EscalationTarget != nil && *instance.EscalationTarget == principalID {
			canAct = true
			actions = []string{"APPROVE", "REJECT", "REQUEST_CHANGES"}
		}
	} else if instance.WorkflowStatus == domain.WorkflowStatusApproved ||
		instance.WorkflowStatus == domain.WorkflowStatusRejected ||
		instance.WorkflowStatus == domain.WorkflowStatusCancelled ||
		instance.WorkflowStatus == domain.WorkflowStatusExpired {
		// Terminal states - no actions
		actions = []string{}
	}

	response := domain.AvailableActions{
		WorkflowInstanceID: workflowInstanceID,
		CurrentStage:       instance.CurrentStage,
		CurrentStatus:      instance.WorkflowStatus,
		Actions:            actions,
		PrincipalID:        principalID,
		CanAct:             canAct,
		Reason:             reason,
	}
	writeJSON(w, r, http.StatusOK, response)
}

// GetTransitionHistory handles GET /v1/workflows/{workflow_instance_id}/transition-history.
// Returns the complete transition history for a workflow.
// ST-3 endpoint.
func (h *Handler) GetTransitionHistory(w http.ResponseWriter, r *http.Request) {
	workflowInstanceID := chi.URLParam(r, "workflow_instance_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	instance, err := h.store.FindWorkflowByID(r.Context(), workflowInstanceID)
	if err != nil {
		writeStoreErr(w, r, h.log, err, r.Header.Get("X-Correlation-ID"), "GetTransitionHistory")
		return
	}

	if instance.TenantID != tenantID {
		writeJSON(w, r, http.StatusNotFound, map[string]string{"error": "workflow_not_found"})
		return
	}

	if !h.authorizeRead(w, r, instance, principalID, r.Header.Get("X-Correlation-ID"), "GetTransitionHistory") {
		return
	}

	transitions, err := h.store.GetApprovalEvidence(r.Context(), workflowInstanceID)
	if err != nil {
		writeStoreErr(w, r, h.log, err, r.Header.Get("X-Correlation-ID"), "GetTransitionHistory")
		return
	}

history := make([]domain.TransitionHistoryEntry, len(transitions))
	for i, t := range transitions {
		fromStage := 0
		if t.StageOrder != nil && *t.StageOrder > 0 {
			fromStage = *t.StageOrder - 1
		}
		toStage := 0
		if t.StageOrder != nil {
			toStage = *t.StageOrder
		}
		history[i] = domain.TransitionHistoryEntry{
			TransitionID:       t.WorkflowTransitionID,
			WorkflowInstanceID: t.WorkflowInstanceID,
			FromStage:          fromStage,
			ToStage:            toStage,
			FromStatus:         t.FromState,
			ToStatus:           t.ToState,
			ActorPrincipalID:   t.ActedBy,
ActionType:         t.FromState + "->" + t.ToState,
			Rationale:          t.Rationale,
			DecisionID:         t.AccessDecisionID,
			Quorum:             nil,
			OccurredAt:         t.ActedAt,
		}
	}

	writeJSON(w, r, http.StatusOK, history)
}

// ptrBool returns a pointer to a bool.
func ptrBool(b bool) *bool {
	return &b
}
