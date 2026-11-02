package handler

import (
	"net/http"
	"strings"

	"github.com/google/uuid"

	"zoiko.io/reporting-orchestration-svc/internal/domain"
	"zoiko.io/reporting-orchestration-svc/internal/store"
)

// TriggerRun handles POST /v1/reports/definitions/{id}/runs.
//
// Validation: id must be a UUID; optional body {triggered_by, period_start,
// period_end} — triggered_by in MANUAL/SCHEDULED/API (default MANUAL),
// periods YYYY-MM-DD, supplied together, start <= end. Malformed JSON is 400.
// Rule: the definition must be ACTIVE (checked under a row lock).
// Writes: one report_runs row. No data is aggregated — the run is honestly
// recorded as NOT_IMPLEMENTED (see domain.RunStatusNotImplemented).
// Event: report.run.not_implemented.v1.
// Statuses: 202 recorded; 400; 401; 403 authz denied; 404 unknown/another
// tenant's definition; 409 definition not ACTIVE.
func (h *Handler) TriggerRun(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.requireCaller(w, r)
	if !ok {
		return
	}
	definitionID, ok := requireUUIDParam(w, r, "id")
	if !ok {
		return
	}

	var request domain.TriggerRunRequest
	if !optionalJSONBody(w, r, &request) {
		return
	}
	if err := request.Validate(); err != nil {
		validationProblem(w, r, err.Error())
		return
	}

	definition, err := h.definitions.GetDefinition(r.Context(), caller.TenantID, definitionID)
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), caller.TenantID, caller.PrincipalID, definition.LegalEntityID, actionRunTrigger); err != nil {
		h.writeAuthzError(w, r, err)
		return
	}
	if definition.Status != domain.DefStatusActive {
		h.writeDomainError(w, r, domain.ErrDefinitionNotActive)
		return
	}

	run := domain.NewRecordedRun(definition, request, caller.PrincipalID)
	event := newEvent(caller, domain.EventRunRecorded, domain.AggregateReportRun, "", definition.LegalEntityID)

	recorded, err := h.runs.CreateRun(r.Context(), run, event)
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, recorded)
}

// GetRun handles GET /v1/reports/runs/{runId}.
//
// Statuses: 200; 400 bad id; 401; 404 unknown or another tenant's run.
func (h *Handler) GetRun(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.requireCaller(w, r)
	if !ok {
		return
	}
	runID, ok := requireUUIDParam(w, r, "runId")
	if !ok {
		return
	}

	run, err := h.runs.GetRun(r.Context(), caller.TenantID, runID)
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// ListRuns handles GET /v1/reports/runs.
//
// Query: definition_id (UUID), status (filters); limit, offset.
// Statuses: 200 {data, count, limit, offset}; 400 invalid query; 401.
func (h *Handler) ListRuns(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.requireCaller(w, r)
	if !ok {
		return
	}
	page, ok := parsePage(w, r)
	if !ok {
		return
	}

	query := r.URL.Query()
	filter := store.RunFilter{Status: strings.ToUpper(query.Get("status"))}
	if rawDefinitionID := query.Get("definition_id"); rawDefinitionID != "" {
		parsed, err := uuid.Parse(rawDefinitionID)
		if err != nil {
			writeProblem(w, r, http.StatusBadRequest, CodeInvalidQuery, "definition_id must be a UUID")
			return
		}
		filter.DefinitionID = parsed.String()
	}

	runs, err := h.runs.ListRuns(r.Context(), caller.TenantID, filter, page)
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, listResponse{Data: runs, Count: len(runs), Limit: page.Limit, Offset: page.Offset})
}
