// Group 2 completion pass (21 Oct 2026): attribute updates, the filing
// lifecycle, and the definition / entity-mapping catalogue.
package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/obligations-svc/internal/authz"
	"zoiko.io/obligations-svc/internal/domain"
	"zoiko.io/obligations-svc/internal/envelope"
)

func registerCompletionRoutes(r chi.Router, h *Handler) {
	r.Patch("/v1/obligations/{obligation_id}", h.UpdateObligation)
	r.Get("/v1/obligations/{obligation_id}/filing-requirements/{filing_requirement_id}", h.GetFilingRequirement)
	r.Post("/v1/obligations/{obligation_id}/filing-requirements/{filing_requirement_id}/status", h.UpdateFilingStatus)
	r.Get("/v1/obligations/{obligation_id}/filing-requirements/{filing_requirement_id}/transitions", h.ListFilingTransitions)

	r.Post("/v1/obligation-definitions", h.CreateDefinition)
	r.Get("/v1/obligation-definitions", h.ListDefinitions)
	r.Get("/v1/obligation-definitions/{definition_id}", h.GetDefinition)
	r.Get("/v1/obligation-definitions/{definition_id}/versions", h.ListDefinitionVersions)
	r.Post("/v1/obligation-definitions/{definition_id}/versions", h.NewDefinitionVersion)
	r.Post("/v1/obligation-definitions/{definition_id}/retire", h.RetireDefinition)
	r.Post("/v1/obligation-definitions/{definition_id}/entity-mappings", h.CreateMapping)
	r.Get("/v1/obligation-definitions/{definition_id}/entity-mappings", h.ListMappings)
	r.Post("/v1/obligation-definitions/{definition_id}/entity-mappings/{mapping_id}/end", h.EndMapping)
}

// ifMatch reads If-Match ("3", W/"3") or, failing that, expected_version in
// the body (passed as bodyVersion). present is false when neither was sent.
func ifMatch(w http.ResponseWriter, r *http.Request, bodyVersion int) (version int, present, ok bool) {
	if raw := strings.TrimSpace(r.Header.Get("If-Match")); raw != "" {
		v, err := strconv.Atoi(strings.Trim(strings.TrimPrefix(raw, "W/"), `"`))
		if err != nil || v < 1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_if_match", "detail": "If-Match must be a positive integer version"})
			return 0, false, false
		}
		return v, true, true
	}
	if bodyVersion > 0 {
		return bodyVersion, true, true
	}
	return 0, false, true
}

// preconditionRequired answers 428: a protected update without the version it
// was based on (GCP §16 expected_version; API standard If-Match).
func preconditionRequired(w http.ResponseWriter) {
	writeJSON(w, http.StatusPreconditionRequired, map[string]string{"error": "precondition_required",
		"detail": "send If-Match (or expected_version) with the version this change is based on"})
}

func correlationOf(r *http.Request) string { return r.Header.Get("X-Correlation-ID") }

// ── PATCH /v1/obligations/{obligation_id} ───────────────────────────────────

type updateObligationRequest struct {
	DueDate             *time.Time `json:"due_date,omitempty"`
	SeverityLevel       *string    `json:"severity_level,omitempty"`
	ResponsibleFunction *string    `json:"responsible_function,omitempty"`
	ExpectedVersion     int        `json:"expected_version,omitempty"`
}

// UpdateObligation changes due date, severity or responsible function. It is
// what obligation.updated covers besides status changes. If-Match is
// mandatory: an edit made from a stale read would overwrite a change the
// editor never saw.
func (h *Handler) UpdateObligation(w http.ResponseWriter, r *http.Request) {
	obligationID := chi.URLParam(r, "obligation_id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	var req updateObligationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	version, present, ok := ifMatch(w, r, req.ExpectedVersion)
	if !ok {
		return
	}
	if !present {
		preconditionRequired(w)
		return
	}
	if req.DueDate == nil && req.SeverityLevel == nil && req.ResponsibleFunction == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "due_date, severity_level or responsible_function"})
		return
	}
	for name, v := range map[string]*string{"severity_level": req.SeverityLevel, "responsible_function": req.ResponsibleFunction} {
		if v != nil && strings.TrimSpace(*v) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": name})
			return
		}
	}
	if req.DueDate != nil && req.DueDate.IsZero() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": "due_date"})
		return
	}

	existing, err := h.store.FindObligationByID(r.Context(), obligationID)
	if err != nil {
		h.writeStoreErr(w, "UpdateObligation: lookup failed", err)
		return
	}
	if !h.authorize(w, r, principalID, existing.LegalEntityID, authz.ActionObligationUpdate) {
		return
	}
	updated, changed, err := h.store.UpdateObligation(r.Context(), domain.UpdateObligationParams{
		ObligationID: obligationID, ExpectedVersion: version, DueDate: req.DueDate,
		SeverityLevel: req.SeverityLevel, ResponsibleFunction: req.ResponsibleFunction,
		ActorPrincipalID: principalID, CorrelationID: correlationOf(r),
	})
	if err != nil {
		h.writeStoreErr(w, "UpdateObligation: store unavailable", err)
		return
	}
	h.log.Info("obligation updated", zap.String("obligation_id", obligationID), zap.Bool("changed", changed),
		zap.String("correlation_id", correlationOf(r)))
	w.Header().Set("ETag", strconv.Quote(strconv.Itoa(updated.Version)))
	writeJSON(w, http.StatusOK, updated)
}

// ── filing lifecycle ────────────────────────────────────────────────────────

// GetFilingRequirement handles GET …/filing-requirements/{filing_requirement_id}.
func (h *Handler) GetFilingRequirement(w http.ResponseWriter, r *http.Request) {
	obligationID, filingID := chi.URLParam(r, "obligation_id"), chi.URLParam(r, "filing_requirement_id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	parent, err := h.store.FindObligationByID(r.Context(), obligationID)
	if err != nil {
		h.writeStoreErr(w, "GetFilingRequirement: lookup failed", err)
		return
	}
	if !h.authorize(w, r, principalID, parent.LegalEntityID, authz.ActionFilingRequirementRead) {
		return
	}
	f, err := h.store.FindFilingRequirement(r.Context(), obligationID, filingID)
	if err != nil {
		h.writeStoreErr(w, "GetFilingRequirement: store unavailable", err)
		return
	}
	w.Header().Set("ETag", strconv.Quote(strconv.Itoa(f.RowVersion)))
	writeJSON(w, http.StatusOK, f)
}

type updateFilingStatusRequest struct {
	FilingStatus    string `json:"filing_status"`
	Reason          string `json:"reason,omitempty"`
	ExpectedVersion int    `json:"expected_version,omitempty"`
}

// UpdateFilingStatus moves a filing requirement along its lifecycle (Doc 04
// §13.1 filing_status was stuck at PENDING: no route could change it).
func (h *Handler) UpdateFilingStatus(w http.ResponseWriter, r *http.Request) {
	obligationID, filingID := chi.URLParam(r, "obligation_id"), chi.URLParam(r, "filing_requirement_id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	var req updateFilingStatusRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.FilingStatus == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "filing_status"})
		return
	}
	if _, known := domain.FilingStatuses[req.FilingStatus]; !known {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": "filing_status",
			"detail": "filing_status must be one of PENDING, IN_PREPARATION, SUBMITTED, ACCEPTED, REJECTED, WITHDRAWN"})
		return
	}
	if (req.FilingStatus == "REJECTED" || req.FilingStatus == "WITHDRAWN") && strings.TrimSpace(req.Reason) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "reason",
			"detail": "a rejection or withdrawal must record why"})
		return
	}
	version, _, ok := ifMatch(w, r, req.ExpectedVersion)
	if !ok {
		return
	}
	parent, err := h.store.FindObligationByID(r.Context(), obligationID)
	if err != nil {
		h.writeStoreErr(w, "UpdateFilingStatus: lookup failed", err)
		return
	}
	if !h.authorize(w, r, principalID, parent.LegalEntityID, authz.ActionFilingStatusUpdate) {
		return
	}
	f, moved, err := h.store.UpdateFilingStatus(r.Context(), domain.UpdateFilingStatusParams{
		ObligationID: obligationID, FilingRequirementID: filingID, NewStatus: req.FilingStatus, Reason: req.Reason,
		ExpectedVersion: version, ActorPrincipalID: principalID, CorrelationID: correlationOf(r),
	})
	if err != nil {
		h.writeStoreErr(w, "UpdateFilingStatus: store unavailable", err)
		return
	}
	h.log.Info("filing status updated", zap.String("filing_requirement_id", filingID),
		zap.String("filing_status", f.FilingStatus), zap.Bool("moved", moved))
	w.Header().Set("ETag", strconv.Quote(strconv.Itoa(f.RowVersion)))
	writeJSON(w, http.StatusOK, f)
}

// ListFilingTransitions handles GET …/filing-requirements/{id}/transitions.
func (h *Handler) ListFilingTransitions(w http.ResponseWriter, r *http.Request) {
	obligationID, filingID := chi.URLParam(r, "obligation_id"), chi.URLParam(r, "filing_requirement_id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	parent, err := h.store.FindObligationByID(r.Context(), obligationID)
	if err != nil {
		h.writeStoreErr(w, "ListFilingTransitions: lookup failed", err)
		return
	}
	if !h.authorize(w, r, principalID, parent.LegalEntityID, authz.ActionFilingRequirementRead) {
		return
	}
	out, err := h.store.ListFilingTransitions(r.Context(), obligationID, filingID)
	if err != nil {
		h.writeStoreErr(w, "ListFilingTransitions: store unavailable", err)
		return
	}
	if out == nil {
		out = []*domain.FilingTransition{}
	}
	writeJSON(w, http.StatusOK, out)
}

// ── definitions ─────────────────────────────────────────────────────────────

type definitionContentRequest struct {
	JurisdictionID       string `json:"jurisdiction_id"`
	ObligationType       string `json:"obligation_type"`
	ObligationSourceType string `json:"obligation_source_type"`
	ObligationSourceID   string `json:"obligation_source_id"`
	SourceReference      string `json:"source_reference"`
	SeverityLevel        string `json:"severity_level"`
	ResponsibleFunction  string `json:"responsible_function"`
	Description          string `json:"description,omitempty"`
}

func (c definitionContentRequest) missingField() string {
	for _, f := range []struct{ v, name string }{
		{c.JurisdictionID, "jurisdiction_id"}, {c.ObligationType, "obligation_type"},
		{c.ObligationSourceType, "obligation_source_type"}, {c.ObligationSourceID, "obligation_source_id"},
		{c.SourceReference, "source_reference"}, {c.SeverityLevel, "severity_level"},
		{c.ResponsibleFunction, "responsible_function"},
	} {
		if strings.TrimSpace(f.v) == "" {
			return f.name
		}
	}
	return ""
}

func (c definitionContentRequest) content() domain.DefinitionContent {
	return domain.DefinitionContent{JurisdictionID: c.JurisdictionID, ObligationType: c.ObligationType,
		ObligationSourceType: c.ObligationSourceType, ObligationSourceID: c.ObligationSourceID,
		SourceReference: c.SourceReference, SeverityLevel: c.SeverityLevel,
		ResponsibleFunction: c.ResponsibleFunction, Description: c.Description}
}

// validateJurisdiction applies the same fail-closed jurisdiction check as an
// obligation create: a definition is jurisdiction-bound too.
func (h *Handler) validateJurisdiction(w http.ResponseWriter, r *http.Request, id string) bool {
	err := h.jurisdictionValidator.ValidateExists(r.Context(), id)
	switch {
	case err == nil:
		return true
	case errors.Is(err, domain.ErrJurisdictionNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "jurisdiction_not_found", "jurisdiction_id": id})
	default:
		h.log.Error("jurisdiction validation failed", zap.String("jurisdiction_id", id), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "jurisdiction_service_unavailable"})
	}
	return false
}

type createDefinitionRequest struct {
	DefinitionCode string `json:"definition_code"`
	definitionContentRequest
}

// CreateDefinition handles POST /v1/obligation-definitions.
func (h *Handler) CreateDefinition(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	var req createDefinitionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.DefinitionCode) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "definition_code"})
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": missing})
		return
	}
	if !h.authorize(w, r, principalID, authz.PlatformScope, authz.ActionObligationDefinitionManage) {
		return
	}
	if !h.validateJurisdiction(w, r, req.JurisdictionID) {
		return
	}
	d, created, err := h.store.CreateDefinition(r.Context(), domain.CreateDefinitionParams{
		DefinitionCode: req.DefinitionCode, Content: req.content(),
		CreatedByPrincipalID: principalID, CorrelationID: correlationOf(r),
	})
	if err != nil {
		h.writeStoreErr(w, "CreateDefinition: store unavailable", err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		w.Header().Set("Location", "/v1/obligation-definitions/"+d.ObligationDefinitionID)
	}
	w.Header().Set("ETag", strconv.Quote(strconv.Itoa(d.CurrentVersion)))
	writeJSON(w, status, d)
}

// ListDefinitions handles GET /v1/obligation-definitions?status=&limit=&cursor=.
func (h *Handler) ListDefinitions(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	limit, _, ok := parsePaging(w, r)
	if !ok {
		return
	}
	status := r.URL.Query().Get("status")
	if status != "" && status != "ACTIVE" && status != "RETIRED" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": "status"})
		return
	}
	if !h.authorize(w, r, principalID, authz.PlatformScope, authz.ActionObligationDefinitionRead) {
		return
	}
	after := r.URL.Query().Get("cursor")
	out, err := h.store.ListDefinitions(r.Context(), status, limit, after)
	if err != nil {
		h.writeStoreErr(w, "ListDefinitions: store unavailable", err)
		return
	}
	if out == nil {
		out = []*domain.ObligationDefinition{}
	}
	if len(out) == limit {
		w.Header().Set("X-Next-Cursor", out[len(out)-1].DefinitionCode)
	}
	writeJSON(w, http.StatusOK, out)
}

// GetDefinition handles GET /v1/obligation-definitions/{definition_id}.
func (h *Handler) GetDefinition(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if !h.authorize(w, r, principalID, authz.PlatformScope, authz.ActionObligationDefinitionRead) {
		return
	}
	d, err := h.store.GetDefinition(r.Context(), chi.URLParam(r, "definition_id"))
	if err != nil {
		h.writeStoreErr(w, "GetDefinition: store unavailable", err)
		return
	}
	w.Header().Set("ETag", strconv.Quote(strconv.Itoa(d.CurrentVersion)))
	writeJSON(w, http.StatusOK, d)
}

// ListDefinitionVersions handles GET …/{definition_id}/versions.
func (h *Handler) ListDefinitionVersions(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if !h.authorize(w, r, principalID, authz.PlatformScope, authz.ActionObligationDefinitionRead) {
		return
	}
	out, err := h.store.ListDefinitionVersions(r.Context(), chi.URLParam(r, "definition_id"))
	if err != nil {
		h.writeStoreErr(w, "ListDefinitionVersions: store unavailable", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type newDefinitionVersionRequest struct {
	definitionContentRequest
	ChangeReason    string `json:"change_reason"`
	ExpectedVersion int    `json:"expected_version,omitempty"`
}

// NewDefinitionVersion handles POST …/{definition_id}/versions (If-Match).
func (h *Handler) NewDefinitionVersion(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	var req newDefinitionVersionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	version, present, ok := ifMatch(w, r, req.ExpectedVersion)
	if !ok {
		return
	}
	if !present {
		preconditionRequired(w)
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": missing})
		return
	}
	if strings.TrimSpace(req.ChangeReason) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "change_reason"})
		return
	}
	if !h.authorize(w, r, principalID, authz.PlatformScope, authz.ActionObligationDefinitionManage) {
		return
	}
	if !h.validateJurisdiction(w, r, req.JurisdictionID) {
		return
	}
	d, err := h.store.NewDefinitionVersion(r.Context(), domain.NewDefinitionVersionParams{
		ObligationDefinitionID: chi.URLParam(r, "definition_id"), ExpectedVersion: version,
		Content: req.content(), ChangeReason: req.ChangeReason,
		ActorPrincipalID: principalID, CorrelationID: correlationOf(r),
	})
	if err != nil {
		h.writeStoreErr(w, "NewDefinitionVersion: store unavailable", err)
		return
	}
	w.Header().Set("ETag", strconv.Quote(strconv.Itoa(d.CurrentVersion)))
	writeJSON(w, http.StatusOK, d)
}

// RetireDefinition handles POST …/{definition_id}/retire.
func (h *Handler) RetireDefinition(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	version, _, ok := ifMatch(w, r, 0)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, authz.PlatformScope, authz.ActionObligationDefinitionManage) {
		return
	}
	d, err := h.store.RetireDefinition(r.Context(), chi.URLParam(r, "definition_id"), version, principalID, correlationOf(r))
	if err != nil {
		h.writeStoreErr(w, "RetireDefinition: store unavailable", err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// ── entity mappings ─────────────────────────────────────────────────────────

type createMappingRequest struct {
	LegalEntityID string    `json:"legal_entity_id"`
	EffectiveFrom time.Time `json:"effective_from"`
}

// CreateMapping handles POST …/{definition_id}/entity-mappings. Authorized at
// the entity being bound, which must exist in this tenant.
func (h *Handler) CreateMapping(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	var req createMappingRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.LegalEntityID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "legal_entity_id"})
		return
	}
	if req.EffectiveFrom.IsZero() {
		req.EffectiveFrom = time.Now().UTC()
	}
	if env, _ := envelope.FromContext(r.Context()); env.LegalEntityID != "" && env.LegalEntityID != req.LegalEntityID {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "legal_entity_mismatch",
			"detail": "X-Legal-Entity-Id header must match body legal_entity_id"})
		return
	}
	if !h.authorize(w, r, principalID, req.LegalEntityID, authz.ActionObligationMappingManage) {
		return
	}
	if !h.checkEntity(w, r, req.LegalEntityID) {
		return
	}
	definitionID := chi.URLParam(r, "definition_id")
	m, err := h.store.CreateMapping(r.Context(), domain.CreateMappingParams{
		ObligationDefinitionID: definitionID, LegalEntityID: req.LegalEntityID,
		EffectiveFrom: req.EffectiveFrom, ActorPrincipalID: principalID, CorrelationID: correlationOf(r),
	})
	if err != nil {
		h.writeStoreErr(w, "CreateMapping: store unavailable", err)
		return
	}
	w.Header().Set("Location", "/v1/obligation-definitions/"+definitionID+"/entity-mappings/"+m.MappingID)
	writeJSON(w, http.StatusCreated, m)
}

// ListMappings handles GET …/{definition_id}/entity-mappings.
func (h *Handler) ListMappings(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if !h.authorize(w, r, principalID, authz.PlatformScope, authz.ActionObligationDefinitionRead) {
		return
	}
	out, err := h.store.ListMappings(r.Context(), chi.URLParam(r, "definition_id"))
	if err != nil {
		h.writeStoreErr(w, "ListMappings: store unavailable", err)
		return
	}
	if out == nil {
		out = []*domain.ObligationEntityMapping{}
	}
	writeJSON(w, http.StatusOK, out)
}

type endMappingRequest struct {
	EffectiveTo time.Time `json:"effective_to"`
}

// EndMapping handles POST …/entity-mappings/{mapping_id}/end.
func (h *Handler) EndMapping(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	var req endMappingRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.EffectiveTo.IsZero() {
		req.EffectiveTo = time.Now().UTC()
	}
	definitionID, mappingID := chi.URLParam(r, "definition_id"), chi.URLParam(r, "mapping_id")
	existing, err := h.store.FindMapping(r.Context(), definitionID, mappingID)
	if err != nil {
		h.writeStoreErr(w, "EndMapping: lookup failed", err)
		return
	}
	if !h.authorize(w, r, principalID, existing.LegalEntityID, authz.ActionObligationMappingManage) {
		return
	}
	m, err := h.store.EndMapping(r.Context(), definitionID, mappingID, req.EffectiveTo, principalID, correlationOf(r))
	if err != nil {
		h.writeStoreErr(w, "EndMapping: store unavailable", err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}
