package handler

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"zoiko.io/financial-control-svc/internal/domain"
)

// ResolveException — POST /controls/v1/exceptions/{exception_id}/transition (§21).
//
// Body: {"to_state","reason", and per target state "evidence_ref" | "authority_ref" | "carry_to_period"}.
// If-Match with the exception's ETag is mandatory. The action demanded depends on the
// target: waiving or carrying forward needs FINCTRL_EXCEPTION_WAIVE, reperformance needs
// FINCTRL_REPERFORM, everything else FINCTRL_EXCEPTION_RESOLVE. Independence rules (owner
// works it; owner cannot waive it; remediator cannot reperform it) are enforced by the
// store under the row lock, so they cannot be raced.
func (h *Handler) ResolveException(w http.ResponseWriter, r *http.Request) {
	tenantID, principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	expected, ok := parseIfMatch(w, r, -2)
	if !ok {
		return
	}
	if expected == -2 {
		writeError(w, http.StatusPreconditionRequired, "if_match_required", "If-Match with the exception's ETag is required")
		return
	}
	var req domain.ResolveExceptionRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		h.writeErr(w, "resolve exception", err)
		return
	}
	id := chi.URLParam(r, "exception_id")
	ex, err := h.store.GetException(r.Context(), tenantID, id)
	if err != nil {
		h.writeErr(w, "resolve exception", err)
		return
	}
	if !h.authorize(w, r, principal, ex.LegalEntityID, req.RequiredAction()) {
		return
	}
	out, err := h.store.ResolveException(r.Context(), tenantID, id, principal, corrID(r), expected, req)
	if err != nil {
		h.writeErr(w, "resolve exception", err)
		return
	}
	out.Attention = out.DeriveAttention(time.Now().UTC())
	setETag(w, out.Version)
	writeJSON(w, http.StatusOK, out)
}

// ListExceptionTransitions — GET /controls/v1/exceptions/{exception_id}/transitions.
// The append-only history, including the authority and evidence behind each decision.
func (h *Handler) ListExceptionTransitions(w http.ResponseWriter, r *http.Request) {
	tenantID, principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "exception_id")
	ex, err := h.store.GetException(r.Context(), tenantID, id)
	if err != nil {
		h.writeErr(w, "list exception transitions", err)
		return
	}
	if !h.authorize(w, r, principal, ex.LegalEntityID, ActionRead) {
		return
	}
	list, err := h.store.ListExceptionTransitions(r.Context(), tenantID, id)
	if err != nil {
		h.writeErr(w, "list exception transitions", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"transitions": nonNil(list)})
}

type submitRequest struct {
	Reason string `json:"reason"`
}

// SubmitForCertification — POST /controls/v1/runs/{run_id}/submit-for-certification.
// The operator's resubmission of a run in EXCEPTION_REVIEW whose exceptions are all
// resolved (the way back after a rejected certification). It decides nothing: the
// certifier still does, and is still barred from having taken part in the run.
func (h *Handler) SubmitForCertification(w http.ResponseWriter, r *http.Request) {
	tenantID, principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	expected, ok := parseIfMatch(w, r, -2)
	if !ok {
		return
	}
	if expected == -2 {
		writeError(w, http.StatusPreconditionRequired, "if_match_required", "If-Match with the run's ETag is required")
		return
	}
	var req submitRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "reason is required")
		return
	}
	runID := chi.URLParam(r, "run_id")
	run, err := h.store.GetRun(r.Context(), tenantID, runID)
	if err != nil {
		h.writeErr(w, "submit for certification", err)
		return
	}
	if !h.authorize(w, r, principal, run.LegalEntityID, domain.ActionResolve) {
		return
	}
	out, err := h.store.SubmitForCertification(r.Context(), tenantID, runID, principal, corrID(r), expected, req.Reason)
	if err != nil {
		h.writeErr(w, "submit for certification", err)
		return
	}
	setETag(w, out.Version)
	writeJSON(w, http.StatusOK, out)
}
