package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/domain"
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
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if req.LegalEntityID == "" || req.Name == "" || req.BusinessPurpose == "" || req.TargetDomain == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_fields"})
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, req.LegalEntityID, actionFormManage); err != nil {
		h.writeFormAuthzErr(w, err)
		return
	}

	form, created, err := h.store.CreateForm(r.Context(), domain.CreateFormParams{
		TenantID: tenantID, LegalEntityID: req.LegalEntityID, Name: req.Name, BusinessPurpose: req.BusinessPurpose,
		TargetDomain: req.TargetDomain, Schema: req.Schema, OwnerPrincipalID: actor, CorrelationID: correlationID,
	})
	if err != nil {
		h.writeFormErr(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, form)
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
		h.writeFormErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, form.LegalEntityID, actionFormRead); err != nil {
		h.writeFormAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, form)
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
		h.writeFormErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, existing.LegalEntityID, actionFormApprove); err != nil {
		h.writeFormAuthzErr(w, err)
		return
	}

	published, err := h.store.PublishForm(r.Context(), domain.PublishFormParams{
		FormID: formID, TenantID: tenantID, ActorPrincipalID: actor, CorrelationID: correlationID,
	})
	if err != nil {
		h.writeFormErr(w, err)
		return
	}
	if err := h.publisher.PublishFormPublished(r.Context(), *published, actor, correlationID); err != nil {
		h.log.Error("failed to publish form.published event", zap.Error(err))
	}
	writeJSON(w, http.StatusOK, published)
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
		h.writeFormErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, existing.LegalEntityID, actionFormApprove); err != nil {
		h.writeFormAuthzErr(w, err)
		return
	}

	retired, err := h.store.RetireForm(r.Context(), domain.RetireFormParams{
		FormID: formID, TenantID: tenantID, ActorPrincipalID: actor, CorrelationID: correlationID,
	})
	if err != nil {
		h.writeFormErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, retired)
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
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}

	formID := chi.URLParam(r, "form_id")
	submissionID := chi.URLParam(r, "submission_id")
	var legalEntityID string
	if formID != "" {
		form, err := h.store.GetForm(r.Context(), tenantID, formID)
		if err != nil {
			h.writeFormErr(w, err)
			return
		}
		legalEntityID = form.LegalEntityID
	} else {
		existing, err := h.store.GetSubmission(r.Context(), tenantID, submissionID)
		if err != nil {
			h.writeFormErr(w, err)
			return
		}
		form, err := h.store.GetForm(r.Context(), tenantID, existing.FormID)
		if err != nil {
			h.writeFormErr(w, err)
			return
		}
		legalEntityID = form.LegalEntityID
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, legalEntityID, actionFormSubmit); err != nil {
		h.writeFormAuthzErr(w, err)
		return
	}

	submission, err := h.store.SaveDraft(r.Context(), domain.SaveDraftParams{
		SubmissionID: submissionID, FormID: formID, TenantID: tenantID, SubmitterPrincipalID: actor,
		CorrelationID: correlationID, SubmittedValues: req.SubmittedValues, ConsentAttestation: req.ConsentAttestation,
	})
	if err != nil {
		h.writeFormErr(w, err)
		return
	}
	status := http.StatusOK
	if submissionID == "" {
		status = http.StatusCreated
	}
	writeJSON(w, status, submission)
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
		h.writeFormErr(w, err)
		return
	}
	form, err := h.store.GetForm(r.Context(), tenantID, existing.FormID)
	if err != nil {
		h.writeFormErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, form.LegalEntityID, actionFormSubmit); err != nil {
		h.writeFormAuthzErr(w, err)
		return
	}

	submitted, err := h.store.SubmitForm(r.Context(), domain.SubmitFormParams{
		SubmissionID: submissionID, TenantID: tenantID, ActorPrincipalID: actor, CorrelationID: correlationID,
	})
	if err != nil {
		h.writeFormErr(w, err)
		return
	}
	if err := h.publisher.PublishFormEvent(r.Context(), "form.submission.received", *submitted, actor, correlationID); err != nil {
		h.log.Error("failed to publish form.submission.received event", zap.Error(err))
	}
	writeJSON(w, http.StatusOK, submitted)
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
		h.writeFormErr(w, err)
		return
	}
	form, err := h.store.GetForm(r.Context(), tenantID, existing.FormID)
	if err != nil {
		h.writeFormErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, form.LegalEntityID, actionFormSubmit); err != nil {
		h.writeFormAuthzErr(w, err)
		return
	}

	validated, err := h.store.ValidateSubmission(r.Context(), domain.ValidateSubmissionParams{
		SubmissionID: submissionID, TenantID: tenantID, ActorPrincipalID: actor, CorrelationID: correlationID,
	})
	if err != nil {
		h.writeFormErr(w, err)
		return
	}
	eventType := "form.submission.validated"
	if validated.Status == domain.FormSubmissionRejected {
		eventType = "form.submission.rejected"
	}
	if err := h.publisher.PublishFormEvent(r.Context(), eventType, *validated, actor, correlationID); err != nil {
		h.log.Error("failed to publish form validation event", zap.Error(err))
	}
	writeJSON(w, http.StatusOK, validated)
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
		h.writeFormErr(w, err)
		return
	}
	form, err := h.store.GetForm(r.Context(), tenantID, submission.FormID)
	if err != nil {
		h.writeFormErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, form.LegalEntityID, actionFormRead); err != nil {
		h.writeFormAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, submission)
}

func (h *Handler) writeFormErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrFormNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "form_not_found"})
	case errors.Is(err, domain.ErrFormSubmissionNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "form_submission_not_found"})
	case errors.Is(err, domain.ErrFormRetired):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "form_retired"})
	case errors.Is(err, domain.ErrFormNotDraft):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "form_not_draft"})
	case errors.Is(err, domain.ErrFormNotPublished):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "form_not_published"})
	case errors.Is(err, domain.ErrFormAlreadyRetired):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "form_already_retired"})
	case errors.Is(err, domain.ErrFormSelfPublish):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "self_publish_not_allowed"})
	case errors.Is(err, domain.ErrFormSubmissionNotDraft):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "submission_not_draft"})
	case errors.Is(err, domain.ErrFormSubmissionNotSubmitted):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "submission_not_submitted"})
	case errors.Is(err, domain.ErrFormSubmissionNotAccepted):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "submission_not_accepted"})
	case errors.Is(err, domain.ErrFormSubmissionNotAcceptedOrRejected):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "submission_not_accepted_or_rejected"})
	case errors.Is(err, domain.ErrFormSubmissionAlreadySuperseded):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "submission_already_superseded"})
	case errors.Is(err, domain.ErrFormSubmissionsBelongToDifferentForms):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "submissions_belong_to_different_forms"})
	default:
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
	}
}

func (h *Handler) writeFormAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, domain.ErrAuthorizationDenied) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "authorization_unavailable"})
}
