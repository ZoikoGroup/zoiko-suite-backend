package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/events"
)

// ZS-JUR-001 s23, s31 and JUR-NEG-19: the remediation workflow for outcomes produced with a pack version that was rolled back or withdrawn.
//
// Withdrawing a version, or rolling it back in a ring and region, opens a case automatically when decisions were made with it. Each decision in the
// case needs a disposition with evidence (reconfirmed, not affected, recalculated, superseded); a case closes only when every decision has one, and only
// by someone other than the person who opened it. This service records and enforces that; re-evaluating an outcome is the job of the service that used it.

const maxDispositionBatch = 100

func registerRemediationRoutes(r chi.Router, h *Handler) {
	r.Post("/v1/admin/remediation-cases", h.OpenRemediationCase)
	r.Get("/v1/admin/remediation-cases", h.ListRemediationCases)
	r.Get("/v1/admin/remediation-cases/{case_id}", h.GetRemediationCase)
	r.Get("/v1/admin/remediation-cases/{case_id}/decisions", h.ListRemediationDecisions)
	r.Post("/v1/admin/remediation-cases/{case_id}/dispositions", h.RecordRemediationDispositions)
	r.Post("/v1/admin/remediation-cases/{case_id}/close", h.CloseRemediationCase)
}

func (h *Handler) writeRemediationError(w http.ResponseWriter, err error, corr string) {
	switch {
	case errors.Is(err, domain.ErrRemediationNotFound):
		writeError(w, http.StatusNotFound, "remediation_case_not_found", err.Error())
	case errors.Is(err, domain.ErrNothingToRemediate):
		writeError(w, http.StatusConflict, "nothing_to_remediate", err.Error())
	case errors.Is(err, domain.ErrDispositionDiffers):
		writeError(w, http.StatusConflict, "disposition_differs", err.Error())
	case errors.Is(err, domain.ErrDecisionNotInCase):
		writeError(w, http.StatusUnprocessableEntity, "decision_not_in_case", err.Error())
	case errors.Is(err, domain.ErrRemediationRefused):
		writeError(w, http.StatusUnprocessableEntity, "remediation_refused", err.Error())
	default:
		h.writeOpsError(w, err, corr)
	}
}

type OpenRemediationRequest struct {
	PackRef     string `json:"pack_ref"`
	Version     string `json:"version"`
	Ring        string `json:"ring"`
	Region      string `json:"region"`
	Reason      string `json:"reason"`
	EvidenceRef string `json:"evidence_ref"`
}

// OpenRemediationCase opens a case by hand for a version and scope, for an incident the automatic triggers do not cover. Withdrawals and rollbacks
// open one by themselves.
//
//	201 opened      200 a case was already open for this version and scope      409 nothing_to_remediate (no decisions)      404 no such version
func (h *Handler) OpenRemediationCase(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "remediation_case", "open")
	if !ok {
		return
	}
	var req OpenRemediationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if field := firstBlank(requiredField{"pack_ref", req.PackRef}, requiredField{"version", req.Version}, requiredField{"reason", req.Reason}); field != "" {
		writeMissingField(w, field)
		return
	}
	c, err := h.registry.OpenRemediationCase(r.Context(), req.PackRef, req.Version, strings.TrimSpace(req.Ring), strings.TrimSpace(req.Region),
		strings.TrimSpace(req.Reason), strings.TrimSpace(req.EvidenceRef), actor)
	if err != nil {
		h.writeRemediationError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	status := http.StatusOK
	if c.Opened {
		status = http.StatusCreated
		h.emitRemediationOpened(r, c.CaseID, req.PackRef, req.Version, domain.RemediationManual, c.DecisionsTotal, actor)
	}
	writeJSON(w, status, c)
}

func (h *Handler) emitRemediationOpened(r *http.Request, caseID, ref, version, trigger string, decisions int, actor string) {
	h.emitRegistry(r, events.EventRemediationOpened, caseID, actor, map[string]any{"case_id": caseID, "pack_ref": ref, "version": version, "trigger": trigger, "decisions": decisions})
}

func (h *Handler) ListRemediationCases(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r, "remediation_case", "view"); !ok {
		return
	}
	limit, offset, ok := h.parsePaging(w, r.URL.Query())
	if !ok {
		return
	}
	status := strings.ToUpper(r.URL.Query().Get("status"))
	if status != "" && status != "OPEN" && status != "CLOSED" {
		writeError(w, http.StatusBadRequest, "invalid_status", "status must be OPEN or CLOSED")
		return
	}
	out, err := h.registry.ListRemediationCases(r.Context(), status, limit, offset)
	if err != nil {
		h.writeRemediationError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cases": out})
}

func (h *Handler) GetRemediationCase(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r, "remediation_case", "view"); !ok {
		return
	}
	c, err := h.registry.GetRemediationCase(r.Context(), chi.URLParam(r, "case_id"))
	if err != nil {
		h.writeRemediationError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// ListRemediationDecisions lists a case's decisions with their dispositions; ?pending=true shows only those still to do.
func (h *Handler) ListRemediationDecisions(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r, "remediation_case", "view"); !ok {
		return
	}
	limit, offset, ok := h.parsePaging(w, r.URL.Query())
	if !ok {
		return
	}
	pending := r.URL.Query().Get("pending") == "true"
	out, err := h.registry.ListRemediationDecisions(r.Context(), chi.URLParam(r, "case_id"), pending, limit, offset)
	if err != nil {
		h.writeRemediationError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"decisions": out})
}

type RecordDispositionsRequest struct {
	Dispositions []domain.DispositionInput `json:"dispositions"`
}

// RecordRemediationDispositions records what was done about decisions of an OPEN case, all or nothing (1 to 100 per call). Repeating an identical
// disposition is a replay; a different one for the same decision is refused (409), because dispositions are final.
//
//	200 {recorded, replayed}      400 invalid disposition      404 no such case      409 case closed or disposition_differs
//	422 decision_not_in_case or remediation_refused (for example a replacement made with the same faulty version)
func (h *Handler) RecordRemediationDispositions(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "remediation_case", "dispose")
	if !ok {
		return
	}
	var req RecordDispositionsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Dispositions) == 0 || len(req.Dispositions) > maxDispositionBatch {
		writeError(w, http.StatusBadRequest, "invalid_dispositions", "dispositions needs 1 to 100 entries")
		return
	}
	seen := map[string]bool{}
	for i, d := range req.Dispositions {
		if err := d.Validate(); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_disposition", "dispositions["+strconv.Itoa(i)+"]: "+err.Error())
			return
		}
		if seen[d.DecisionID] {
			writeError(w, http.StatusBadRequest, "invalid_dispositions", "dispositions["+strconv.Itoa(i)+"]: decision "+d.DecisionID+" appears twice in the batch")
			return
		}
		seen[d.DecisionID] = true
	}
	res, err := h.registry.RecordDispositions(r.Context(), chi.URLParam(r, "case_id"), req.Dispositions, actor)
	if err != nil {
		h.writeRemediationError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, res)
}

type CloseRemediationRequest struct {
	Note string `json:"note"`
}

// CloseRemediationCase closes a case once every decision has a disposition, by someone other than the person who opened it.
//
//	200 closed (changed=true) or already closed (changed=false)      400 note missing      404 no such case
//	409 operation_blocked (decisions still pending)      403 the opener cannot close their own case
func (h *Handler) CloseRemediationCase(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "remediation_case", "close")
	if !ok {
		return
	}
	var req CloseRemediationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Note) == "" {
		writeMissingField(w, "note")
		return
	}
	c, changed, err := h.registry.CloseRemediationCase(r.Context(), chi.URLParam(r, "case_id"), strings.TrimSpace(req.Note), actor)
	if err != nil {
		h.writeRemediationError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	if changed {
		h.emitRegistry(r, events.EventRemediationClosed, c.CaseID, actor, map[string]any{"case_id": c.CaseID, "pack_ref": c.PackRef, "version": c.Version,
			"decisions": c.DecisionsTotal})
	}
	writeJSON(w, http.StatusOK, map[string]any{"changed": changed, "case": c})
}
