package handler

import (
	"net/http"
	"regexp"

	"github.com/go-chi/chi/v5"

	"zoiko.io/financial-control-svc/internal/domain"
)

// ActionCertify is the authority to certify or reject a run. It is distinct from
// FINCTRL_EXECUTE so authorization-svc can grant the two to different people.
const ActionCertify = "FINCTRL_CERTIFY"

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// CertifyRun — POST /controls/v1/runs/{run_id}/certification (§26).
// Body {"decision":"CERTIFY"|"REJECT","reason":"..."}; If-Match with the run's ETag
// is mandatory so a decision is always taken on the state the certifier saw.
// Authority is checked against the run's own legal entity, and the store refuses a
// certifier who created or took part in the run (maker-checker).
func (h *Handler) CertifyRun(w http.ResponseWriter, r *http.Request) {
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
	var req domain.CertifyRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		h.writeErr(w, "certify run", err)
		return
	}
	runID := chi.URLParam(r, "run_id")
	run, err := h.store.GetRun(r.Context(), tenantID, runID)
	if err != nil {
		h.writeErr(w, "certify run", err)
		return
	}
	if !h.authorize(w, r, principal, run.LegalEntityID, ActionCertify) {
		return
	}
	out, err := h.store.CertifyRun(r.Context(), tenantID, runID, principal, corrID(r), expected, req)
	if err != nil {
		h.writeErr(w, "certify run", err)
		return
	}
	setETag(w, out.Version)
	writeJSON(w, http.StatusOK, out)
}

// entityPeriod validates the shared query of the two period-level reads.
func (h *Handler) entityPeriod(w http.ResponseWriter, r *http.Request) (entity, period string, ok bool) {
	q := r.URL.Query()
	entity, period = q.Get("legal_entity_id"), q.Get("period_id")
	if !uuidRe.MatchString(entity) {
		writeError(w, http.StatusBadRequest, "invalid_request", "legal_entity_id must be a UUID")
		return "", "", false
	}
	if period == "" || len(period) > 32 {
		writeError(w, http.StatusBadRequest, "invalid_request", "period_id is required (at most 32 characters)")
		return "", "", false
	}
	return entity, period, true
}

// CloseGate — GET /controls/v1/close-gate?legal_entity_id=&period_id= (FIN-CTRL-041).
// Reports whether every mandatory control for the entity and period has a CERTIFIED
// latest run. Fail-closed: no configured mandatory control means the gate is not open.
func (h *Handler) CloseGate(w http.ResponseWriter, r *http.Request) {
	tenantID, principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	entity, period, ok := h.entityPeriod(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principal, entity, ActionRead) {
		return
	}
	g, err := h.store.CloseGate(r.Context(), tenantID, entity, period)
	if err != nil {
		h.writeErr(w, "close gate", err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

// ExceptionSummary — GET /controls/v1/exception-summary?legal_entity_id=&period_id=
// (FIN-CTRL-042). Aggregates unresolved exceptions per currency against the
// entity's aggregate materiality threshold.
func (h *Handler) ExceptionSummary(w http.ResponseWriter, r *http.Request) {
	tenantID, principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	entity, period, ok := h.entityPeriod(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principal, entity, ActionRead) {
		return
	}
	s, err := h.store.ExceptionSummary(r.Context(), tenantID, entity, period)
	if err != nil {
		h.writeErr(w, "exception summary", err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}
