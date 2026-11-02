package handler

import (
	"net/http"
	"strings"

	"zoiko.io/reporting-orchestration-svc/internal/domain"
	"zoiko.io/reporting-orchestration-svc/internal/store"
)

// CreateDefinition handles POST /v1/reports/definitions.
//
// Validation: body is a CreateDefinitionRequest (unknown fields rejected);
// report_type / output_format from closed sets; at least one data source;
// tenant_id (optional) must equal X-Tenant-Id; legal_entity_id defaults to
// and must agree with X-Legal-Entity-Id.
// Writes: one report_definitions row in DRAFT, version 1.
// Event: report.definition.created.v1 (outbox).
// Statuses: 201 created; 400 malformed/invalid; 401 no tenant/principal;
// 403 tenant/legal-entity mismatch or authz denied; 503 authz unavailable.
func (h *Handler) CreateDefinition(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.requireCaller(w, r)
	if !ok {
		return
	}

	var request domain.CreateDefinitionRequest
	if !requireJSONBody(w, r, &request) {
		return
	}
	if !requireTenantAgreement(w, r, caller, request.TenantID) {
		return
	}
	legalEntityID, ok := resolveLegalEntity(w, r, caller, request.LegalEntityID)
	if !ok {
		return
	}
	request.LegalEntityID = legalEntityID
	if err := request.Validate(); err != nil {
		validationProblem(w, r, err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), caller.TenantID, caller.PrincipalID, request.LegalEntityID, actionDefinitionCreate); err != nil {
		h.writeAuthzError(w, r, err)
		return
	}

	newDefinition := &domain.ReportDefinition{
		TenantID:      caller.TenantID,
		LegalEntityID: request.LegalEntityID,
		ReportName:    request.ReportName,
		ReportType:    request.ReportType,
		OutputFormat:  request.OutputFormat,
		DataSources:   request.DataSources,
		ScheduleCron:  request.ScheduleCron,
		IsScheduled:   request.IsScheduled,
		CreatedBy:     caller.PrincipalID,
	}
	event := newEvent(caller, domain.EventDefinitionCreated, domain.AggregateReportDefinition, "", request.LegalEntityID)

	created, err := h.definitions.CreateDefinition(r.Context(), newDefinition, event)
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}
	setETag(w, created.Version)
	writeJSON(w, http.StatusCreated, created)
}

// GetDefinition handles GET /v1/reports/definitions/{id}.
//
// Validation: id must be a UUID.
// Statuses: 200 with ETag; 400 bad id; 401 no tenant/principal;
// 404 unknown or another tenant's definition.
func (h *Handler) GetDefinition(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.requireCaller(w, r)
	if !ok {
		return
	}
	definitionID, ok := requireUUIDParam(w, r, "id")
	if !ok {
		return
	}

	definition, err := h.definitions.GetDefinition(r.Context(), caller.TenantID, definitionID)
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}
	setETag(w, definition.Version)
	writeJSON(w, http.StatusOK, definition)
}

// ListDefinitions handles GET /v1/reports/definitions.
//
// Query: legal_entity_id, report_type, status (filters); limit (1-200,
// default 50), offset (0-100000).
// Statuses: 200 {data, count, limit, offset}; 400 invalid query;
// 401 no tenant/principal.
func (h *Handler) ListDefinitions(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.requireCaller(w, r)
	if !ok {
		return
	}
	page, ok := parsePage(w, r)
	if !ok {
		return
	}

	query := r.URL.Query()
	filter := store.DefinitionFilter{
		LegalEntityID: query.Get("legal_entity_id"),
		ReportType:    strings.ToUpper(query.Get("report_type")),
	}
	if rawStatus := query.Get("status"); rawStatus != "" {
		status, known := domain.ParseDefinitionStatus(rawStatus)
		if !known {
			writeProblem(w, r, http.StatusBadRequest, CodeInvalidQuery, "status must be one of DRAFT, VALIDATED, ACTIVE, PAUSED, DEPRECATED")
			return
		}
		filter.Status = string(status)
	}

	definitions, err := h.definitions.ListDefinitions(r.Context(), caller.TenantID, filter, page)
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, listResponse{Data: definitions, Count: len(definitions), Limit: page.Limit, Offset: page.Offset})
}

// UpdateDefinitionStatus handles PATCH /v1/reports/definitions/{id}/status.
//
// Validation: id must be a UUID; body {"status": ..., "reason"?: ...} with a
// known status; optional If-Match must equal the current ETag.
// Lifecycle: DRAFT->VALIDATED->ACTIVE<->PAUSED, any non-terminal->DEPRECATED.
// Writes: status + version bump. Event: report.definition.status_changed.v1.
// Statuses: 200 with new ETag; 400 malformed/invalid; 401; 403 authz denied;
// 404 unknown/another tenant's; 409 illegal transition; 412 stale If-Match.
func (h *Handler) UpdateDefinitionStatus(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.requireCaller(w, r)
	if !ok {
		return
	}
	definitionID, ok := requireUUIDParam(w, r, "id")
	if !ok {
		return
	}
	expectedVersion, ok := parseIfMatch(w, r)
	if !ok {
		return
	}

	var request domain.UpdateDefinitionStatusRequest
	if !requireJSONBody(w, r, &request) {
		return
	}
	if strings.TrimSpace(request.Status) == "" {
		validationProblem(w, r, "status is required")
		return
	}
	targetStatus, known := domain.ParseDefinitionStatus(request.Status)
	if !known {
		validationProblem(w, r, "status must be one of DRAFT, VALIDATED, ACTIVE, PAUSED, DEPRECATED")
		return
	}

	existing, err := h.definitions.GetDefinition(r.Context(), caller.TenantID, definitionID)
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), caller.TenantID, caller.PrincipalID, existing.LegalEntityID, actionDefinitionStatusUpdate); err != nil {
		h.writeAuthzError(w, r, err)
		return
	}

	event := newEvent(caller, domain.EventDefinitionStatusChanged, domain.AggregateReportDefinition, definitionID, existing.LegalEntityID)
	transition, err := h.definitions.TransitionDefinitionStatus(r.Context(), store.TransitionDefinitionParams{
		TenantID:        caller.TenantID,
		DefinitionID:    definitionID,
		ToStatus:        targetStatus,
		ExpectedVersion: expectedVersion,
		Event:           event,
	})
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}

	setETag(w, transition.Definition.Version)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":          transition.Definition.ID,
		"from_status": transition.FromStatus,
		"status":      transition.Definition.Status,
		"version":     transition.Definition.Version,
		"definition":  transition.Definition,
	})
}
