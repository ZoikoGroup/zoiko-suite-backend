// Compliance status, exception cases, and escalation records HTTP handlers.
// Per Doc 04 §13.1.
package handler

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/obligations-svc/internal/authz"
	"zoiko.io/obligations-svc/internal/domain"
	"zoiko.io/obligations-svc/internal/envelope"
	"zoiko.io/obligations-svc/internal/store"
)

// RegisterComplianceRoutes mounts compliance status, exception case, and escalation
// record routes on the given chi router.
func RegisterComplianceRoutes(r chi.Router, h *Handler) {
	// Compliance Status
	r.Post("/v1/compliance-status", h.CreateComplianceStatus)
	r.Get("/v1/compliance-status", h.ListComplianceStatuses)
	r.Get("/v1/compliance-status/{compliance_status_id}", h.GetComplianceStatus)
	r.Post("/v1/compliance-status/{compliance_status_id}", h.UpdateComplianceStatus)

	// Exception Cases
	r.Post("/v1/exceptions", h.CreateExceptionCase)
	r.Get("/v1/exceptions", h.ListExceptionCases)
	r.Get("/v1/exceptions/{exception_case_id}", h.GetExceptionCase)
	r.Post("/v1/exceptions/{exception_case_id}/status", h.UpdateExceptionCaseStatus)

	// Escalation Records
	r.Post("/v1/exceptions/{exception_case_id}/escalations", h.CreateEscalationRecord)
	r.Get("/v1/exceptions/{exception_case_id}/escalations", h.ListEscalationRecords)
	r.Get("/v1/exceptions/{exception_case_id}/escalations/{escalation_record_id}", h.GetEscalationRecord)
}

// ── Compliance Status ─────────────────────────────────────────────────────────

type createComplianceStatusRequest struct {
	ComplianceStatusID       string    `json:"compliance_status_id,omitempty"`
	LegalEntityID            string    `json:"legal_entity_id"`
	DomainCode               string    `json:"domain_code"`
	StatusCode               string    `json:"status_code"`
	StatusReason             string    `json:"status_reason"`
	EvidenceSufficiencyState string    `json:"evidence_sufficiency_state"`
	EvaluatedAt              time.Time `json:"evaluated_at"`
	CreatedByPrincipalID     string    `json:"created_by_principal_id"`
}

func (req createComplianceStatusRequest) missingField() string {
	switch {
	case req.LegalEntityID == "":
		return "legal_entity_id"
	case req.DomainCode == "":
		return "domain_code"
	case req.StatusCode == "":
		return "status_code"
	case req.StatusReason == "":
		return "status_reason"
	case req.EvidenceSufficiencyState == "":
		return "evidence_sufficiency_state"
	case req.EvaluatedAt.IsZero():
		return "evaluated_at"
	case req.CreatedByPrincipalID == "":
		return "created_by_principal_id"
	default:
		return ""
	}
}

func (h *Handler) CreateComplianceStatus(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	bodyBytes, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		writeJSON(w, r, http.StatusRequestEntityTooLarge, map[string]string{"error": "request_too_large"})
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

	env, _ := envelope.FromContext(r.Context())
	idempotencyKey := env.IdempotencyKey

	if idempotencyKey != "" {
		requestHash := store.ComputeRequestHash(bodyBytes)
		alreadyProcessed, existingResourceID, existingResourceType, err := h.store.CheckIdempotencyKey(r.Context(), idempotencyKey, requestHash)
		if err != nil {
			h.writeStoreErr(w, r, "CreateComplianceStatus: idempotency check failed", err)
			return
		}
		if alreadyProcessed && existingResourceType == "compliance_status" {
			existing, err := h.store.FindComplianceStatusByID(r.Context(), existingResourceID)
			if err != nil {
				h.writeStoreErr(w, r, "CreateComplianceStatus: replay lookup failed", err)
				return
			}
			h.log.Info("idempotent replay: compliance status already exists",
				zap.String("compliance_status_id", existing.ComplianceStatusID),
				zap.String("idempotency_key", idempotencyKey))
			writeJSON(w, r, http.StatusOK, existing)
			return
		}
		if alreadyProcessed && existingResourceType != "compliance_status" {
			writeJSON(w, r, http.StatusConflict, map[string]string{
				"error":  "idempotency_key_conflict",
				"detail": "idempotency key already used for different resource type",
			})
			return
		}
	}

	var req createComplianceStatusRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": missing})
		return
	}

	// Validate status_code
	validStatusCodes := map[string]bool{
		"COMPLIANT":       true,
		"NON_COMPLIANT":   true,
		"AT_RISK":         true,
		"UNKNOWN":         true,
		"WAIVED":          true,
	}
	if !validStatusCodes[req.StatusCode] {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{
			"error":  "invalid_field",
			"field":  "status_code",
			"detail": "status_code must be one of COMPLIANT, NON_COMPLIANT, AT_RISK, UNKNOWN, WAIVED",
		})
		return
	}

	// Validate evidence_sufficiency_state
	validEvidenceStates := map[string]bool{
		"SUFFICIENT":           true,
		"INSUFFICIENT":         true,
		"PENDING":              true,
		"NOT_APPLICABLE":       true,
	}
	if !validEvidenceStates[req.EvidenceSufficiencyState] {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{
			"error":  "invalid_field",
			"field":  "evidence_sufficiency_state",
			"detail": "evidence_sufficiency_state must be one of SUFFICIENT, INSUFFICIENT, PENDING, NOT_APPLICABLE",
		})
		return
	}

	if req.CreatedByPrincipalID != principalID {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{
			"error":  "created_by_mismatch",
			"detail": "created_by_principal_id must be the authenticated principal",
		})
		return
	}

	// Authorize against the legal entity
	if !h.authorize(w, r, principalID, req.LegalEntityID, authz.ActionObligationCreate) {
		return
	}
	if !h.checkEntity(w, r, req.LegalEntityID) {
		return
	}

	cs, err := h.store.CreateComplianceStatus(r.Context(), domain.CreateComplianceStatusParams{
		ComplianceStatusID:       req.ComplianceStatusID,
		LegalEntityID:            req.LegalEntityID,
		DomainCode:               req.DomainCode,
		StatusCode:               req.StatusCode,
		StatusReason:             req.StatusReason,
		EvidenceSufficiencyState: req.EvidenceSufficiencyState,
		EvaluatedAt:              req.EvaluatedAt,
		CreatedByPrincipalID:     principalID,
		CorrelationID:            correlationID,
	})
	if err != nil {
		h.writeStoreErr(w, r, "CreateComplianceStatus: store unavailable", err)
		return
	}

	if idempotencyKey != "" {
		requestHash := store.ComputeRequestHash(bodyBytes)
		if err := h.store.StoreIdempotencyKey(r.Context(), idempotencyKey, requestHash, cs.ComplianceStatusID, "compliance_status"); err != nil {
			h.log.Error("CreateComplianceStatus: failed to store idempotency key",
				zap.String("compliance_status_id", cs.ComplianceStatusID),
				zap.String("idempotency_key", idempotencyKey),
				zap.Error(err))
		}
	}

	h.log.Info("compliance status created",
		zap.String("compliance_status_id", cs.ComplianceStatusID),
		zap.String("legal_entity_id", cs.LegalEntityID),
		zap.String("domain_code", cs.DomainCode),
		zap.String("correlation_id", correlationID))
	w.Header().Set("Location", "/v1/compliance-status/"+cs.ComplianceStatusID)
	writeJSON(w, r, http.StatusCreated, cs)
}

func (h *Handler) GetComplianceStatus(w http.ResponseWriter, r *http.Request) {
	complianceStatusID := chi.URLParam(r, "compliance_status_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	cs, err := h.store.FindComplianceStatusByID(r.Context(), complianceStatusID)
	if err != nil {
		h.writeStoreErr(w, r, "GetComplianceStatus: store unavailable", err)
		return
	}

	if !h.authorize(w, r, principalID, cs.LegalEntityID, authz.ActionObligationRead) {
		return
	}

	writeJSON(w, r, http.StatusOK, cs)
}

func (h *Handler) ListComplianceStatuses(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	limit, offset, ok := parsePaging(w, r)
	if !ok {
		return
	}

	legalEntityID := q.Get("legal_entity_id")
	if legalEntityID == "" {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": "legal_entity_id query parameter required for authorization",
		})
		return
	}
	if !h.authorize(w, r, principalID, legalEntityID, authz.ActionObligationRead) {
		return
	}

	filter := domain.ListComplianceStatusFilter{
		Limit:          limit,
		Offset:         offset,
		LegalEntityID:  legalEntityID,
		DomainCode:     q.Get("domain_code"),
		StatusCode:     q.Get("status_code"),
	}

	results, err := h.store.ListComplianceStatuses(r.Context(), filter)
	if err != nil {
		h.writeStoreErr(w, r, "ListComplianceStatuses: store unavailable", err)
		return
	}
	if results == nil {
		results = []*domain.ComplianceStatus{}
	}
	writeJSON(w, r, http.StatusOK, results)
}

type updateComplianceStatusRequest struct {
	StatusCode               string `json:"status_code"`
	StatusReason             string `json:"status_reason"`
	EvidenceSufficiencyState string `json:"evidence_sufficiency_state"`
	ExpectedVersion          int    `json:"expected_version"`
}

func (h *Handler) UpdateComplianceStatus(w http.ResponseWriter, r *http.Request) {
	complianceStatusID := chi.URLParam(r, "compliance_status_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	var req updateComplianceStatusRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.StatusCode == "" || req.StatusReason == "" || req.EvidenceSufficiencyState == "" {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "status_code, status_reason, evidence_sufficiency_state required"})
		return
	}

	// Validate status_code
	validStatusCodes := map[string]bool{
		"COMPLIANT":       true,
		"NON_COMPLIANT":   true,
		"AT_RISK":         true,
		"UNKNOWN":         true,
		"WAIVED":          true,
	}
	if !validStatusCodes[req.StatusCode] {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{
			"error":  "invalid_field",
			"field":  "status_code",
			"detail": "status_code must be one of COMPLIANT, NON_COMPLIANT, AT_RISK, UNKNOWN, WAIVED",
		})
		return
	}

	// Validate evidence_sufficiency_state
	validEvidenceStates := map[string]bool{
		"SUFFICIENT":           true,
		"INSUFFICIENT":         true,
		"PENDING":              true,
		"NOT_APPLICABLE":       true,
	}
	if !validEvidenceStates[req.EvidenceSufficiencyState] {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{
			"error":  "invalid_field",
			"field":  "evidence_sufficiency_state",
			"detail": "evidence_sufficiency_state must be one of SUFFICIENT, INSUFFICIENT, PENDING, NOT_APPLICABLE",
		})
		return
	}

	// Parse If-Match header
	expectedVersion := req.ExpectedVersion
	if match := r.Header.Get("If-Match"); match != "" && expectedVersion == 0 {
		match = strings.TrimPrefix(match, "W/")
		match = strings.Trim(match, `"`)
		if v, err := strconv.Atoi(match); err == nil {
			expectedVersion = v
		}
	}

	existing, err := h.store.FindComplianceStatusByID(r.Context(), complianceStatusID)
	if err != nil {
		h.writeStoreErr(w, r, "UpdateComplianceStatus: lookup failed", err)
		return
	}
	if !h.authorize(w, r, principalID, existing.LegalEntityID, authz.ActionObligationStatusUpdate) {
		return
	}

	updated, err := h.store.UpdateComplianceStatus(r.Context(), complianceStatusID, req.StatusCode, req.StatusReason, req.EvidenceSufficiencyState, expectedVersion, principalID, correlationID)
	if err != nil {
		h.writeStoreErr(w, r, "UpdateComplianceStatus: store unavailable", err)
		return
	}

	h.log.Info("compliance status updated",
		zap.String("compliance_status_id", complianceStatusID),
		zap.String("new_status", req.StatusCode),
		zap.String("correlation_id", correlationID))
	writeJSON(w, r, http.StatusOK, updated)
}

// ── Exception Cases ───────────────────────────────────────────────────────────

type createExceptionCaseRequest struct {
	ExceptionCaseID      string `json:"exception_case_id,omitempty"`
	LegalEntityID        string `json:"legal_entity_id"`
	ExceptionType        string `json:"exception_type"`
	SeverityLevel        string `json:"severity_level"`
	LinkedObjectType     string `json:"linked_object_type"`
	LinkedObjectID       string `json:"linked_object_id"`
	CreatedByPrincipalID string `json:"created_by_principal_id"`
}

func (req createExceptionCaseRequest) missingField() string {
	switch {
	case req.LegalEntityID == "":
		return "legal_entity_id"
	case req.ExceptionType == "":
		return "exception_type"
	case req.SeverityLevel == "":
		return "severity_level"
	case req.LinkedObjectType == "":
		return "linked_object_type"
	case req.LinkedObjectID == "":
		return "linked_object_id"
	case req.CreatedByPrincipalID == "":
		return "created_by_principal_id"
	default:
		return ""
	}
}

func (h *Handler) CreateExceptionCase(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	bodyBytes, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		writeJSON(w, r, http.StatusRequestEntityTooLarge, map[string]string{"error": "request_too_large"})
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

	env, _ := envelope.FromContext(r.Context())
	idempotencyKey := env.IdempotencyKey

	if idempotencyKey != "" {
		requestHash := store.ComputeRequestHash(bodyBytes)
		alreadyProcessed, existingResourceID, existingResourceType, err := h.store.CheckIdempotencyKey(r.Context(), idempotencyKey, requestHash)
		if err != nil {
			h.writeStoreErr(w, r, "CreateExceptionCase: idempotency check failed", err)
			return
		}
		if alreadyProcessed && existingResourceType == "exception_case" {
			existing, err := h.store.FindExceptionCaseByID(r.Context(), existingResourceID)
			if err != nil {
				h.writeStoreErr(w, r, "CreateExceptionCase: replay lookup failed", err)
				return
			}
			h.log.Info("idempotent replay: exception case already exists",
				zap.String("exception_case_id", existing.ExceptionCaseID),
				zap.String("idempotency_key", idempotencyKey))
			writeJSON(w, r, http.StatusOK, existing)
			return
		}
		if alreadyProcessed && existingResourceType != "exception_case" {
			writeJSON(w, r, http.StatusConflict, map[string]string{
				"error":  "idempotency_key_conflict",
				"detail": "idempotency key already used for different resource type",
			})
			return
		}
	}

	var req createExceptionCaseRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": missing})
		return
	}

	// Validate exception_type
	validExceptionTypes := map[string]bool{
		"MISSED_DEADLINE":       true,
		"INSUFFICIENT_EVIDENCE": true,
		"RULE_CONFLICT":         true,
		"JURISDICTION_CHANGE":   true,
		"DATA_QUALITY":          true,
	}
	if !validExceptionTypes[req.ExceptionType] {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{
			"error":  "invalid_field",
			"field":  "exception_type",
			"detail": "exception_type must be one of MISSED_DEADLINE, INSUFFICIENT_EVIDENCE, RULE_CONFLICT, JURISDICTION_CHANGE, DATA_QUALITY",
		})
		return
	}

	// Validate severity_level
	validSeverityLevels := map[string]bool{
		"CRITICAL": true,
		"HIGH":     true,
		"MEDIUM":   true,
		"LOW":      true,
	}
	if !validSeverityLevels[req.SeverityLevel] {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{
			"error":  "invalid_field",
			"field":  "severity_level",
			"detail": "severity_level must be one of CRITICAL, HIGH, MEDIUM, LOW",
		})
		return
	}

	// Validate linked_object_type
	validLinkedObjectTypes := map[string]bool{
		"OBLIGATION":  true,
		"FILING":      true,
		"POLICY":      true,
		"CONTRACT":    true,
	}
	if !validLinkedObjectTypes[req.LinkedObjectType] {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{
			"error":  "invalid_field",
			"field":  "linked_object_type",
			"detail": "linked_object_type must be one of OBLIGATION, FILING, POLICY, CONTRACT",
		})
		return
	}

	if req.CreatedByPrincipalID != principalID {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{
			"error":  "created_by_mismatch",
			"detail": "created_by_principal_id must be the authenticated principal",
		})
		return
	}

	// Authorize against the legal entity
	if !h.authorize(w, r, principalID, req.LegalEntityID, authz.ActionObligationCreate) {
		return
	}
	if !h.checkEntity(w, r, req.LegalEntityID) {
		return
	}

	ec, err := h.store.CreateExceptionCase(r.Context(), domain.CreateExceptionCaseParams{
		ExceptionCaseID:      req.ExceptionCaseID,
		LegalEntityID:        req.LegalEntityID,
		ExceptionType:        req.ExceptionType,
		SeverityLevel:        req.SeverityLevel,
		LinkedObjectType:     req.LinkedObjectType,
		LinkedObjectID:       req.LinkedObjectID,
		CreatedByPrincipalID: principalID,
		CorrelationID:        correlationID,
	})
	if err != nil {
		h.writeStoreErr(w, r, "CreateExceptionCase: store unavailable", err)
		return
	}

	if idempotencyKey != "" {
		requestHash := store.ComputeRequestHash(bodyBytes)
		if err := h.store.StoreIdempotencyKey(r.Context(), idempotencyKey, requestHash, ec.ExceptionCaseID, "exception_case"); err != nil {
			h.log.Error("CreateExceptionCase: failed to store idempotency key",
				zap.String("exception_case_id", ec.ExceptionCaseID),
				zap.String("idempotency_key", idempotencyKey),
				zap.Error(err))
		}
	}

	h.log.Info("exception case created",
		zap.String("exception_case_id", ec.ExceptionCaseID),
		zap.String("exception_type", ec.ExceptionType),
		zap.String("severity_level", ec.SeverityLevel),
		zap.String("correlation_id", correlationID))
	w.Header().Set("Location", "/v1/exceptions/"+ec.ExceptionCaseID)
	writeJSON(w, r, http.StatusCreated, ec)
}

func (h *Handler) GetExceptionCase(w http.ResponseWriter, r *http.Request) {
	exceptionCaseID := chi.URLParam(r, "exception_case_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	ec, err := h.store.FindExceptionCaseByID(r.Context(), exceptionCaseID)
	if err != nil {
		h.writeStoreErr(w, r, "GetExceptionCase: store unavailable", err)
		return
	}

	if !h.authorize(w, r, principalID, ec.LegalEntityID, authz.ActionObligationRead) {
		return
	}

	writeJSON(w, r, http.StatusOK, ec)
}

func (h *Handler) ListExceptionCases(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	limit, offset, ok := parsePaging(w, r)
	if !ok {
		return
	}

	legalEntityID := q.Get("legal_entity_id")
	if legalEntityID == "" {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": "legal_entity_id query parameter required for authorization",
		})
		return
	}
	if !h.authorize(w, r, principalID, legalEntityID, authz.ActionObligationRead) {
		return
	}

	filter := domain.ListExceptionCasesFilter{
		Limit:         limit,
		Offset:        offset,
		LegalEntityID: legalEntityID,
		ExceptionType: q.Get("exception_type"),
		CaseStatus:    q.Get("case_status"),
	}

	results, err := h.store.ListExceptionCases(r.Context(), filter)
	if err != nil {
		h.writeStoreErr(w, r, "ListExceptionCases: store unavailable", err)
		return
	}
	if results == nil {
		results = []*domain.ExceptionCase{}
	}
	writeJSON(w, r, http.StatusOK, results)
}

type updateExceptionCaseStatusRequest struct {
	CaseStatus    string `json:"case_status"`
	ExpectedVersion int   `json:"expected_version"`
}

func (h *Handler) UpdateExceptionCaseStatus(w http.ResponseWriter, r *http.Request) {
	exceptionCaseID := chi.URLParam(r, "exception_case_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	var req updateExceptionCaseStatusRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.CaseStatus == "" {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "case_status"})
		return
	}

	// Validate case_status
	validCaseStatuses := map[string]bool{
		"OPEN":         true,
		"INVESTIGATING": true,
		"RESOLVED":     true,
		"ESCALATED":    true,
		"CLOSED":       true,
	}
	if !validCaseStatuses[req.CaseStatus] {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{
			"error":  "invalid_field",
			"field":  "case_status",
			"detail": "case_status must be one of OPEN, INVESTIGATING, RESOLVED, ESCALATED, CLOSED",
		})
		return
	}

	// Parse If-Match header
	expectedVersion := req.ExpectedVersion
	if match := r.Header.Get("If-Match"); match != "" && expectedVersion == 0 {
		match = strings.TrimPrefix(match, "W/")
		match = strings.Trim(match, `"`)
		if v, err := strconv.Atoi(match); err == nil {
			expectedVersion = v
		}
	}

	existing, err := h.store.FindExceptionCaseByID(r.Context(), exceptionCaseID)
	if err != nil {
		h.writeStoreErr(w, r, "UpdateExceptionCaseStatus: lookup failed", err)
		return
	}
	if !h.authorize(w, r, principalID, existing.LegalEntityID, authz.ActionObligationStatusUpdate) {
		return
	}

	updated, err := h.store.UpdateExceptionCaseStatus(r.Context(), exceptionCaseID, req.CaseStatus, expectedVersion, principalID, correlationID)
	if err != nil {
		h.writeStoreErr(w, r, "UpdateExceptionCaseStatus: store unavailable", err)
		return
	}

	h.log.Info("exception case status updated",
		zap.String("exception_case_id", exceptionCaseID),
		zap.String("new_status", req.CaseStatus),
		zap.String("correlation_id", correlationID))
	writeJSON(w, r, http.StatusOK, updated)
}

// ── Escalation Records ────────────────────────────────────────────────────────

type createEscalationRecordRequest struct {
	EscalationRecordID    string `json:"escalation_record_id,omitempty"`
	ExceptionCaseID       string `json:"exception_case_id"`
	EscalatedToRole       string `json:"escalated_to_role"`
	EscalationReason      string `json:"escalation_reason"`
	CreatedByPrincipalID  string `json:"created_by_principal_id"`
}

func (req createEscalationRecordRequest) missingField() string {
	switch {
	case req.ExceptionCaseID == "":
		return "exception_case_id"
	case req.EscalatedToRole == "":
		return "escalated_to_role"
	case req.EscalationReason == "":
		return "escalation_reason"
	case req.CreatedByPrincipalID == "":
		return "created_by_principal_id"
	default:
		return ""
	}
}

func (h *Handler) CreateEscalationRecord(w http.ResponseWriter, r *http.Request) {
	exceptionCaseID := chi.URLParam(r, "exception_case_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	bodyBytes, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		writeJSON(w, r, http.StatusRequestEntityTooLarge, map[string]string{"error": "request_too_large"})
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

	env, _ := envelope.FromContext(r.Context())
	idempotencyKey := env.IdempotencyKey

	if idempotencyKey != "" {
		requestHash := store.ComputeRequestHash(bodyBytes)
		alreadyProcessed, existingResourceID, existingResourceType, err := h.store.CheckIdempotencyKey(r.Context(), idempotencyKey, requestHash)
		if err != nil {
			h.writeStoreErr(w, r, "CreateEscalationRecord: idempotency check failed", err)
			return
		}
		if alreadyProcessed && existingResourceType == "escalation_record" {
			existing, err := h.store.FindEscalationRecordByID(r.Context(), existingResourceID)
			if err != nil {
				h.writeStoreErr(w, r, "CreateEscalationRecord: replay lookup failed", err)
				return
			}
			h.log.Info("idempotent replay: escalation record already exists",
				zap.String("escalation_record_id", existing.EscalationRecordID),
				zap.String("idempotency_key", idempotencyKey))
			writeJSON(w, r, http.StatusOK, existing)
			return
		}
		if alreadyProcessed && existingResourceType != "escalation_record" {
			writeJSON(w, r, http.StatusConflict, map[string]string{
				"error":  "idempotency_key_conflict",
				"detail": "idempotency key already used for different resource type",
			})
			return
		}
	}

	var req createEscalationRecordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": missing})
		return
	}

	// Verify the exception case exists and get its legal entity for authorization
	ec, err := h.store.FindExceptionCaseByID(r.Context(), exceptionCaseID)
	if err != nil {
		h.writeStoreErr(w, r, "CreateEscalationRecord: exception case lookup failed", err)
		return
	}

	// Verify the exception_case_id in body matches the URL
	if req.ExceptionCaseID != exceptionCaseID {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{
			"error": "invalid_field",
			"field": "exception_case_id",
			"detail": "body exception_case_id must match URL",
		})
		return
	}

	if req.CreatedByPrincipalID != principalID {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{
			"error":  "created_by_mismatch",
			"detail": "created_by_principal_id must be the authenticated principal",
		})
		return
	}

	// Authorize against the exception case's legal entity
	if !h.authorize(w, r, principalID, ec.LegalEntityID, authz.ActionObligationStatusUpdate) {
		return
	}

	er, err := h.store.CreateEscalationRecord(r.Context(), domain.CreateEscalationRecordParams{
		EscalationRecordID:    req.EscalationRecordID,
		ExceptionCaseID:       req.ExceptionCaseID,
		EscalatedToRole:       req.EscalatedToRole,
		EscalationReason:      req.EscalationReason,
		CreatedByPrincipalID:  principalID,
		CorrelationID:         correlationID,
	})
	if err != nil {
		h.writeStoreErr(w, r, "CreateEscalationRecord: store unavailable", err)
		return
	}

	if idempotencyKey != "" {
		requestHash := store.ComputeRequestHash(bodyBytes)
		if err := h.store.StoreIdempotencyKey(r.Context(), idempotencyKey, requestHash, er.EscalationRecordID, "escalation_record"); err != nil {
			h.log.Error("CreateEscalationRecord: failed to store idempotency key",
				zap.String("escalation_record_id", er.EscalationRecordID),
				zap.String("idempotency_key", idempotencyKey),
				zap.Error(err))
		}
	}

	h.log.Info("escalation record created",
		zap.String("escalation_record_id", er.EscalationRecordID),
		zap.String("exception_case_id", er.ExceptionCaseID),
		zap.String("correlation_id", correlationID))
	w.Header().Set("Location", "/v1/exceptions/"+exceptionCaseID+"/escalations/"+er.EscalationRecordID)
	writeJSON(w, r, http.StatusCreated, er)
}

func (h *Handler) GetEscalationRecord(w http.ResponseWriter, r *http.Request) {
	escalationRecordID := chi.URLParam(r, "escalation_record_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	er, err := h.store.FindEscalationRecordByID(r.Context(), escalationRecordID)
	if err != nil {
		h.writeStoreErr(w, r, "GetEscalationRecord: store unavailable", err)
		return
	}

	// Need to check authorization via the parent exception case
	ec, err := h.store.FindExceptionCaseByID(r.Context(), er.ExceptionCaseID)
	if err != nil {
		h.writeStoreErr(w, r, "GetEscalationRecord: exception case lookup failed", err)
		return
	}
	if !h.authorize(w, r, principalID, ec.LegalEntityID, authz.ActionObligationRead) {
		return
	}

	writeJSON(w, r, http.StatusOK, er)
}

func (h *Handler) ListEscalationRecords(w http.ResponseWriter, r *http.Request) {
	exceptionCaseID := chi.URLParam(r, "exception_case_id")

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

	// Verify the exception case exists and get its legal entity for authorization
	ec, err := h.store.FindExceptionCaseByID(r.Context(), exceptionCaseID)
	if err != nil {
		h.writeStoreErr(w, r, "ListEscalationRecords: exception case lookup failed", err)
		return
	}
	if !h.authorize(w, r, principalID, ec.LegalEntityID, authz.ActionObligationRead) {
		return
	}

	results, err := h.store.ListEscalationRecords(r.Context(), exceptionCaseID, limit)
	if err != nil {
		h.writeStoreErr(w, r, "ListEscalationRecords: store unavailable", err)
		return
	}
	if results == nil {
		results = []*domain.EscalationRecord{}
	}
	writeJSON(w, r, http.StatusOK, results)
}