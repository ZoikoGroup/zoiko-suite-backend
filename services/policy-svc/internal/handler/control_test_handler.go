// Control test / attestation HTTP handlers — doc7 §E3, §E6, §I3.
package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/policy-svc/internal/authz"
	"zoiko.io/policy-svc/internal/domain"
)

// RegisterControlTestRoutes mounts control-test and attestation routes on
// the given chi router. Call AFTER RegisterRoutes so the
// correlationIDMiddleware it installs via r.Use also covers these routes —
// chi's Use applies to every route subsequently added on the same router.
func RegisterControlTestRoutes(r chi.Router, h *Handler) {
	r.Post("/v1/control-test-definitions", h.CreateControlTestDefinition)
	r.Get("/v1/control-test-definitions/{id}", h.GetControlTestDefinition)
	r.Post("/v1/control-test-definitions/{id}/executions", h.CreateControlTestExecution)
	r.Get("/v1/control-test-definitions/{id}/executions", h.ListControlTestExecutions)
	r.Get("/v1/controls/{control_ref}/effectiveness", h.GetControlEffectiveness)

	r.Post("/v1/attestations", h.CreateAttestation)
	r.Get("/v1/attestations/{id}", h.GetAttestation)
	r.Post("/v1/attestations/{id}/revoke", h.RevokeAttestation)
}

// ── POST /v1/control-test-definitions ───────────────────────────────────────

type createControlTestDefinitionRequest struct {
	ControlTestDefinitionID string `json:"control_test_definition_id,omitempty"`
	ControlRef              string `json:"control_ref"`
	TestCode                string `json:"test_code"`
	Title                   string `json:"title"`
	Methodology             string `json:"methodology"`
	SampleApproach          string `json:"sample_approach,omitempty"`
	TestFrequency           string `json:"test_frequency,omitempty"`
}

func (req createControlTestDefinitionRequest) missingField() string {
	switch {
	case req.ControlRef == "":
		return "control_ref"
	case req.TestCode == "":
		return "test_code"
	case req.Title == "":
		return "title"
	case req.Methodology == "":
		return "methodology"
	default:
		return ""
	}
}

// CreateControlTestDefinition handles POST /v1/control-test-definitions.
//
// Response:
//
//	201 → created for the first time
//	200 → test_code already existed with identical attributes; no-op
//	400 → missing required field
//	401 → no caller identity
//	403 → authorization denied
//	409 → test_code already exists with differing attributes
//	503 → authz or store unavailable
func (h *Handler) CreateControlTestDefinition(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	// Not scoped to a legal entity — control_ref is a free-text cross-cutting
	// identifier, so this is a platform-scope decision.
	if !h.authorize(w, r, principalID, authz.ActionControlTestDefinitionCreate, nil) {
		return
	}

	var req createControlTestDefinitionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": missing})
		return
	}

	params := domain.CreateControlTestDefinitionParams{
		ControlTestDefinitionID: req.ControlTestDefinitionID,
		ControlRef:              req.ControlRef,
		TestCode:                req.TestCode,
		Title:                   req.Title,
		Methodology:             req.Methodology,
		SampleApproach:          req.SampleApproach,
		TestFrequency:           req.TestFrequency,
		CreatedByPrincipalID:    principalID,
	}

	d, created, err := h.store.CreateControlTestDefinition(r.Context(), params)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrConflict):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "control_test_definition_conflict", "test_code": req.TestCode})
		default:
			h.log.Error("CreateControlTestDefinition: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, d)
}

// GetControlTestDefinition handles GET /v1/control-test-definitions/{id}.
func (h *Handler) GetControlTestDefinition(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	d, err := h.store.FindControlTestDefinitionByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrControlTestDefinitionNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "control_test_definition_not_found"})
			return
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// ── POST /v1/control-test-definitions/{id}/executions ───────────────────────

type createControlTestExecutionRequest struct {
	ControlTestExecutionID string    `json:"control_test_execution_id,omitempty"`
	PeriodStart            time.Time `json:"period_start"`
	PeriodEnd              time.Time `json:"period_end"`
	PopulationDescription  string    `json:"population_description,omitempty"`
	SampleDescription      string    `json:"sample_description,omitempty"`
	ProcedureNotes         string    `json:"procedure_notes,omitempty"`
	EvidenceRefs           []string  `json:"evidence_refs,omitempty"`
	Result                 string    `json:"result"`
	ExceptionsNoted        string    `json:"exceptions_noted,omitempty"`
	ReviewerPrincipalID    string    `json:"reviewer_principal_id,omitempty"`
}

func (req createControlTestExecutionRequest) missingField() string {
	switch {
	case req.PeriodStart.IsZero():
		return "period_start"
	case req.PeriodEnd.IsZero():
		return "period_end"
	case req.Result == "":
		return "result"
	default:
		return ""
	}
}

// CreateControlTestExecution handles
// POST /v1/control-test-definitions/{id}/executions.
//
// TesterPrincipalID is always the acting principal from X-Principal-Id —
// same doctrine as policy-svc's activated_by_principal_id: a caller cannot
// choose what the audit trail records about who ran the test.
//
// Response:
//
//	201 → execution recorded
//	400 → missing required field
//	401 → no caller identity
//	403 → authorization denied
//	404 → control_test_definition_id not found
//	503 → authz or store unavailable
func (h *Handler) CreateControlTestExecution(w http.ResponseWriter, r *http.Request) {
	definitionID := chi.URLParam(r, "id")
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, authz.ActionControlTestExecutionRecord, nil) {
		return
	}

	var req createControlTestExecutionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": missing})
		return
	}

	params := domain.CreateControlTestExecutionParams{
		ControlTestExecutionID:  req.ControlTestExecutionID,
		ControlTestDefinitionID: definitionID,
		PeriodStart:             req.PeriodStart,
		PeriodEnd:               req.PeriodEnd,
		PopulationDescription:   req.PopulationDescription,
		SampleDescription:       req.SampleDescription,
		ProcedureNotes:          req.ProcedureNotes,
		EvidenceRefs:            req.EvidenceRefs,
		TesterPrincipalID:       principalID,
		Result:                  req.Result,
		ExceptionsNoted:         req.ExceptionsNoted,
		ReviewerPrincipalID:     req.ReviewerPrincipalID,
		CreatedByPrincipalID:    principalID,
	}

	e, err := h.store.CreateControlTestExecution(r.Context(), params)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrControlTestDefinitionNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "control_test_definition_not_found"})
		default:
			h.log.Error("CreateControlTestExecution: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}
	writeJSON(w, http.StatusCreated, e)
}

// ListControlTestExecutions handles
// GET /v1/control-test-definitions/{id}/executions.
func (h *Handler) ListControlTestExecutions(w http.ResponseWriter, r *http.Request) {
	definitionID := chi.URLParam(r, "id")
	results, err := h.store.ListControlTestExecutions(r.Context(), definitionID)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	if results == nil {
		results = []*domain.ControlTestExecution{}
	}
	writeJSON(w, http.StatusOK, results)
}

// GetControlEffectiveness handles GET /v1/controls/{control_ref}/effectiveness
// — the doc7 §E3 answer endpoint: DESIGN_STATUS and OPERATING_EFFECTIVENESS
// composed as two independent fields, never one collapsed status. No authz
// gate — a read of a compliance signal every consuming service needs to
// check cheaply and often, same posture as capability-registry-svc's
// resolution endpoint.
func (h *Handler) GetControlEffectiveness(w http.ResponseWriter, r *http.Request) {
	controlRef := chi.URLParam(r, "control_ref")
	result, err := h.store.ResolveControlEffectiveness(r.Context(), controlRef)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// ── POST /v1/attestations ────────────────────────────────────────────────────

type createAttestationRequest struct {
	AttestationID    string     `json:"attestation_id,omitempty"`
	Statement        string     `json:"statement"`
	StatementVersion string     `json:"statement_version"`
	SubjectRef       string     `json:"subject_ref"`
	PeriodStart      time.Time  `json:"period_start"`
	PeriodEnd        time.Time  `json:"period_end"`
	SignerRole       string     `json:"signer_role"`
	EvidenceRefs     []string   `json:"evidence_refs,omitempty"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
}

func (req createAttestationRequest) missingField() string {
	switch {
	case req.Statement == "":
		return "statement"
	case req.StatementVersion == "":
		return "statement_version"
	case req.SubjectRef == "":
		return "subject_ref"
	case req.PeriodStart.IsZero():
		return "period_start"
	case req.PeriodEnd.IsZero():
		return "period_end"
	case req.SignerRole == "":
		return "signer_role"
	default:
		return ""
	}
}

// CreateAttestation handles POST /v1/attestations.
//
// SignerPrincipalID is always the acting principal from X-Principal-Id —
// doc7 §E6's "signed/attributed assertion" requires the signer be the
// verified identity making the call, not a value the caller can type in.
//
// Response:
//
//	201 → attestation recorded
//	400 → missing required field
//	401 → no caller identity
//	403 → authorization denied
//	503 → authz or store unavailable
func (h *Handler) CreateAttestation(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, authz.ActionAttestationCreate, nil) {
		return
	}

	var req createAttestationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": missing})
		return
	}

	params := domain.CreateAttestationParams{
		AttestationID:        req.AttestationID,
		Statement:            req.Statement,
		StatementVersion:     req.StatementVersion,
		SubjectRef:           req.SubjectRef,
		PeriodStart:          req.PeriodStart,
		PeriodEnd:            req.PeriodEnd,
		SignerPrincipalID:    principalID,
		SignerRole:           req.SignerRole,
		EvidenceRefs:         req.EvidenceRefs,
		ExpiresAt:            req.ExpiresAt,
		CreatedByPrincipalID: principalID,
	}

	a, err := h.store.CreateAttestation(r.Context(), params)
	if err != nil {
		h.log.Error("CreateAttestation: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

// GetAttestation handles GET /v1/attestations/{id}.
func (h *Handler) GetAttestation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	a, err := h.store.FindAttestationByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrAttestationNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "attestation_not_found"})
			return
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// ── POST /v1/attestations/{id}/revoke ────────────────────────────────────────

type revokeAttestationRequest struct {
	Reason string `json:"reason,omitempty"`
}

// RevokeAttestation handles POST /v1/attestations/{id}/revoke — doc7 §E6's
// "challenge/revocation state." Legal only from ACTIVE or CHALLENGED.
//
// Response:
//
//	200 → revoked
//	401 → no caller identity
//	403 → authorization denied
//	404 → attestation not found
//	409 → already REVOKED — illegal transition
//	503 → authz or store unavailable
func (h *Handler) RevokeAttestation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, authz.ActionAttestationRevoke, nil) {
		return
	}

	var req revokeAttestationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}

	a, err := h.store.RevokeAttestation(r.Context(), id, req.Reason)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrAttestationNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "attestation_not_found"})
		case errors.Is(err, domain.ErrInvalidAttestationTransition):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "invalid_transition"})
		default:
			h.log.Error("RevokeAttestation: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}
	writeJSON(w, http.StatusOK, a)
}
