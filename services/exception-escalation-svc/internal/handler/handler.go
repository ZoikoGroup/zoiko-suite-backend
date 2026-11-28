package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/exception-escalation-svc/internal/authz"
	"zoiko.io/exception-escalation-svc/internal/domain"
	"zoiko.io/exception-escalation-svc/internal/events"
	"zoiko.io/exception-escalation-svc/internal/middleware"
	"zoiko.io/exception-escalation-svc/internal/store"
)

type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

const (
	actionExceptionCreate  = "EXCEPTION_CREATE"
	actionExceptionUpdate  = "EXCEPTION_UPDATE"
	actionExceptionRead    = "EXCEPTION_READ"
	actionEscalationCreate = "ESCALATION_CREATE"
	actionEscalationRead   = "ESCALATION_READ"
)

type Handler struct {
	store     store.Store
	publisher events.Publisher
	authz     AuthZClient
	logger    *zap.Logger
}

func New(st store.Store, pub events.Publisher, az AuthZClient, logger *zap.Logger) *Handler {
	return &Handler{store: st, publisher: pub, authz: az, logger: logger}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/exceptions", func(r chi.Router) {
		r.Post("/", h.CreateExceptionCase)
		r.Get("/", h.ListExceptionCases)
		r.Get("/{exception_case_id}", h.GetExceptionCase)
		r.Post("/{exception_case_id}/status", h.UpdateExceptionCaseStatus)

		r.Post("/{exception_case_id}/escalations", h.CreateEscalationRecord)
		r.Get("/{exception_case_id}/escalations", h.ListEscalationRecords)
		r.Get("/{exception_case_id}/escalations/{escalation_record_id}", h.GetEscalationRecord)
	})
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, authz.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "authorization denied")
		return
	}
	writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
}

// ── Exception Cases ──────────────────────────────────────────────────────────

type createExceptionCaseRequest struct {
	ExceptionCaseID      string `json:"exception_case_id,omitempty"`
	LegalEntityID        string `json:"legal_entity_id"`
	ExceptionType        string `json:"exception_type"`
	SeverityLevel        string `json:"severity_level"`
	LinkedObjectType     string `json:"linked_object_type"`
	LinkedObjectID       string `json:"linked_object_id"`
	AssignedToRole       string `json:"assigned_to_role,omitempty"`
	AssignedToUser       string `json:"assigned_to_user,omitempty"`
	CreatedByPrincipalID string `json:"created_by_principal_id"`
}

func (req *createExceptionCaseRequest) validate() error {
	if req.LegalEntityID == "" {
		return errors.New("legal_entity_id is required")
	}
	if req.ExceptionType == "" {
		return errors.New("exception_type is required")
	}
	if !domain.IsValidExceptionType(domain.ExceptionType(req.ExceptionType)) {
		return errors.New("invalid exception_type: must be MISSED_DEADLINE, INSUFFICIENT_EVIDENCE, RULE_CONFLICT, JURISDICTION_CHANGE, or DATA_QUALITY")
	}
	if req.SeverityLevel == "" {
		return errors.New("severity_level is required")
	}
	if !domain.IsValidSeverityLevel(domain.SeverityLevel(req.SeverityLevel)) {
		return errors.New("invalid severity_level: must be CRITICAL, HIGH, MEDIUM, or LOW")
	}
	if req.LinkedObjectType == "" {
		return errors.New("linked_object_type is required")
	}
	if !domain.IsValidLinkedObjectType(domain.LinkedObjectType(req.LinkedObjectType)) {
		return errors.New("invalid linked_object_type: must be OBLIGATION, FILING, POLICY, or CONTRACT")
	}
	if req.LinkedObjectID == "" {
		return errors.New("linked_object_id is required")
	}
	if req.CreatedByPrincipalID == "" {
		return errors.New("created_by_principal_id is required")
	}
	return nil
}

func (h *Handler) CreateExceptionCase(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req createExceptionCaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionExceptionCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	ec, err := h.store.CreateExceptionCase(r.Context(), domain.CreateExceptionCaseParams{
		ExceptionCaseID:      req.ExceptionCaseID,
		TenantID:             tenantID,
		LegalEntityID:        req.LegalEntityID,
		ExceptionType:        domain.ExceptionType(req.ExceptionType),
		SeverityLevel:        domain.SeverityLevel(req.SeverityLevel),
		LinkedObjectType:     domain.LinkedObjectType(req.LinkedObjectType),
		LinkedObjectID:       req.LinkedObjectID,
		AssignedToRole:       req.AssignedToRole,
		AssignedToUser:       req.AssignedToUser,
		CreatedByPrincipalID: req.CreatedByPrincipalID,
		CorrelationID:        r.Header.Get("X-Correlation-ID"),
	})
	if err != nil {
		h.logger.Error("create exception case failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create exception case")
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "exception.created", CaseID: ec.ExceptionCaseID, TenantID: tenantID,
		LegalEntityID: ec.LegalEntityID, Jurisdiction: "", ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: ec,
	})
	writeJSON(w, http.StatusCreated, ec)
}

func (h *Handler) GetExceptionCase(w http.ResponseWriter, r *http.Request) {
	exceptionCaseID := chi.URLParam(r, "exception_case_id")

	ec, err := h.store.FindExceptionCaseByID(r.Context(), exceptionCaseID)
	if err != nil {
		if errors.Is(err, domain.ErrExceptionCaseNotFound) {
			writeError(w, http.StatusNotFound, "exception case not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get exception case")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, ec.LegalEntityID, actionExceptionRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	writeJSON(w, http.StatusOK, ec)
}

func (h *Handler) ListExceptionCases(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	exceptionType := r.URL.Query().Get("exception_type")
	caseStatus := r.URL.Query().Get("case_status")
	severityLevel := r.URL.Query().Get("severity_level")
	assignedToUser := r.URL.Query().Get("assigned_to_user")
	limitStr := r.URL.Query().Get("limit")
	offsetStr := r.URL.Query().Get("offset")

	limit := 50
	if limitStr != "" {
		if v, err := strconv.Atoi(limitStr); err == nil && v > 0 && v <= 200 {
			limit = v
		}
	}
	offset := 0
	if offsetStr != "" {
		if v, err := strconv.Atoi(offsetStr); err == nil && v >= 0 {
			offset = v
		}
	}

	filter := domain.ListExceptionCasesFilter{
		LegalEntityID:  legalEntityID,
		ExceptionType:  domain.ExceptionType(exceptionType),
		CaseStatus:     domain.CaseStatus(caseStatus),
		SeverityLevel:  domain.SeverityLevel(severityLevel),
		AssignedToUser: assignedToUser,
		Limit:          limit,
		Offset:         offset,
	}

	cases, err := h.store.ListExceptionCases(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list exception cases")
		return
	}
	if cases == nil {
		cases = []*domain.ExceptionCase{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"exception_cases": cases,
		"total":           len(cases),
	})
}

type updateExceptionCaseStatusRequest struct {
	CaseStatus    string `json:"case_status"`
	ExpectedVersion int   `json:"expected_version,omitempty"`
}

func (req *updateExceptionCaseStatusRequest) validate() error {
	if req.CaseStatus == "" {
		return errors.New("case_status is required")
	}
	if !domain.IsValidCaseStatus(domain.CaseStatus(req.CaseStatus)) {
		return errors.New("invalid case_status: must be OPEN, INVESTIGATING, RESOLVED, ESCALATED, or CLOSED")
	}
	return nil
}

func (h *Handler) UpdateExceptionCaseStatus(w http.ResponseWriter, r *http.Request) {
	exceptionCaseID := chi.URLParam(r, "exception_case_id")

	var req updateExceptionCaseStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	existing, err := h.store.FindExceptionCaseByID(r.Context(), exceptionCaseID)
	if err != nil {
		if errors.Is(err, domain.ErrExceptionCaseNotFound) {
			writeError(w, http.StatusNotFound, "exception case not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get exception case")
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionExceptionUpdate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	ec, err := h.store.UpdateExceptionCaseStatus(r.Context(), exceptionCaseID, domain.UpdateExceptionCaseStatusParams{
		CaseStatus:      domain.CaseStatus(req.CaseStatus),
		ExpectedVersion: req.ExpectedVersion,
	}, principalID, r.Header.Get("X-Correlation-ID"))
	if err != nil {
		if errors.Is(err, domain.ErrExceptionCaseNotFound) {
			writeError(w, http.StatusNotFound, "exception case not found")
			return
		}
		h.logger.Error("update exception case status failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to update exception case status")
		return
	}

	eventType := "exception.updated"
	if ec.CaseStatus == domain.CaseStatusEscalated {
		eventType = "exception.escalated"
	} else if ec.CaseStatus == domain.CaseStatusClosed || ec.CaseStatus == domain.CaseStatusResolved {
		eventType = "exception.closed"
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: eventType, CaseID: ec.ExceptionCaseID, TenantID: ec.TenantID,
		LegalEntityID: ec.LegalEntityID, Jurisdiction: "", ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: ec,
	})
	writeJSON(w, http.StatusOK, ec)
}

// ── Escalation Records ───────────────────────────────────────────────────────

type createEscalationRecordRequest struct {
	EscalationRecordID    string `json:"escalation_record_id,omitempty"`
	ExceptionCaseID       string `json:"exception_case_id"`
	EscalatedToRole       string `json:"escalated_to_role"`
	EscalationReason      string `json:"escalation_reason"`
	CreatedByPrincipalID  string `json:"created_by_principal_id"`
}

func (req *createEscalationRecordRequest) validate() error {
	if req.ExceptionCaseID == "" {
		return errors.New("exception_case_id is required")
	}
	if req.EscalatedToRole == "" {
		return errors.New("escalated_to_role is required")
	}
	if req.EscalationReason == "" {
		return errors.New("escalation_reason is required")
	}
	if req.CreatedByPrincipalID == "" {
		return errors.New("created_by_principal_id is required")
	}
	return nil
}

func (h *Handler) CreateEscalationRecord(w http.ResponseWriter, r *http.Request) {
	exceptionCaseID := chi.URLParam(r, "exception_case_id")
	tenantID := middleware.GetTenantID(r.Context())

	var req createEscalationRecordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if req.ExceptionCaseID != exceptionCaseID {
		writeError(w, http.StatusBadRequest, "exception_case_id in body must match URL")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	existingCase, err := h.store.FindExceptionCaseByID(r.Context(), exceptionCaseID)
	if err != nil {
		if errors.Is(err, domain.ErrExceptionCaseNotFound) {
			writeError(w, http.StatusNotFound, "exception case not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get exception case")
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, existingCase.LegalEntityID, actionEscalationCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	er, err := h.store.CreateEscalationRecord(r.Context(), domain.CreateEscalationRecordParams{
		EscalationRecordID:    req.EscalationRecordID,
		ExceptionCaseID:       req.ExceptionCaseID,
		EscalatedToRole:       req.EscalatedToRole,
		EscalationReason:      req.EscalationReason,
		CreatedByPrincipalID:  req.CreatedByPrincipalID,
		CorrelationID:         r.Header.Get("X-Correlation-ID"),
	})
	if err != nil {
		h.logger.Error("create escalation record failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create escalation record")
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "escalation.created", CaseID: er.EscalationRecordID, TenantID: tenantID,
		LegalEntityID: existingCase.LegalEntityID, Jurisdiction: "", ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: er,
	})
	writeJSON(w, http.StatusCreated, er)
}

func (h *Handler) GetEscalationRecord(w http.ResponseWriter, r *http.Request) {
	escalationRecordID := chi.URLParam(r, "escalation_record_id")

	er, err := h.store.FindEscalationRecordByID(r.Context(), escalationRecordID)
	if err != nil {
		if errors.Is(err, domain.ErrEscalationRecordNotFound) {
			writeError(w, http.StatusNotFound, "escalation record not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get escalation record")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	// Check authorization via parent exception case
	ec, err := h.store.FindExceptionCaseByID(r.Context(), er.ExceptionCaseID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get parent exception case")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, ec.LegalEntityID, actionEscalationRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	writeJSON(w, http.StatusOK, er)
}

func (h *Handler) ListEscalationRecords(w http.ResponseWriter, r *http.Request) {
	exceptionCaseID := chi.URLParam(r, "exception_case_id")
	escalatedToRole := r.URL.Query().Get("escalated_to_role")
	escalationStatus := r.URL.Query().Get("escalation_status")
	limitStr := r.URL.Query().Get("limit")
	offsetStr := r.URL.Query().Get("offset")

	limit := 50
	if limitStr != "" {
		if v, err := strconv.Atoi(limitStr); err == nil && v > 0 && v <= 200 {
			limit = v
		}
	}
	offset := 0
	if offsetStr != "" {
		if v, err := strconv.Atoi(offsetStr); err == nil && v >= 0 {
			offset = v
		}
	}

	filter := domain.ListEscalationRecordsFilter{
		ExceptionCaseID: exceptionCaseID,
		EscalatedToRole: escalatedToRole,
		EscalationStatus: domain.EscalationStatus(escalationStatus),
		Limit:           limit,
		Offset:          offset,
	}

	records, err := h.store.ListEscalationRecords(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list escalation records")
		return
	}
	if records == nil {
		records = []*domain.EscalationRecord{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"escalation_records": records,
		"total":              len(records),
	})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}