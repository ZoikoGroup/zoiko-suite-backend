package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/payroll-exceptions-svc/internal/domain"
	"zoiko.io/payroll-exceptions-svc/internal/employee"
	svcmiddleware "zoiko.io/payroll-exceptions-svc/internal/middleware"
)

type Store interface {
	CreateException(ctx context.Context, e *domain.PayrollException) error
	GetException(ctx context.Context, exceptionID string) (*domain.PayrollException, error)
	ListExceptions(ctx context.Context, payrollRunID, employeeID, status, severity string) ([]domain.PayrollException, error)
	ResolveException(ctx context.Context, exceptionID, notes, resolvedBy, newStatus string) error
	GetReleaseBlockers(ctx context.Context, payrollRunID string) (*domain.ReleaseBlockerSummary, error)
}

type Publisher interface {
	PublishExceptionRaised(ctx context.Context, correlationID string, e domain.PayrollException)
	PublishExceptionResolved(ctx context.Context, correlationID string, e domain.PayrollException)
	PublishBlockerFlagged(ctx context.Context, correlationID, payrollRunID string, blockerCount int)
}

type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type EmployeeValidator interface {
	ValidateEmployee(ctx context.Context, tenantID, principalID, employeeID string) (*employee.Employee, error)
}

const (
	actionExceptionRaise   = "EXCEPTION_RAISE"
	actionExceptionView    = "EXCEPTION_VIEW"
	actionExceptionResolve = "EXCEPTION_RESOLVE"
	actionExceptionWaive   = "EXCEPTION_WAIVE"
)

type Handler struct {
	store     Store
	publisher Publisher
	authz     AuthZClient
	employee  EmployeeValidator
	log       *zap.Logger
}

func New(store Store, publisher Publisher, authz AuthZClient, empValidator EmployeeValidator, log *zap.Logger) *Handler {
	return &Handler{
		store:     store,
		publisher: publisher,
		authz:     authz,
		employee:  empValidator,
		log:       log,
	}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/payroll-exceptions", func(r chi.Router) {
		r.Post("/", h.RaiseException)
		r.Get("/", h.ListExceptions)
		r.Get("/{id}", h.GetException)
		r.Post("/{id}/resolve", h.ResolveException)
		r.Post("/{id}/waive", h.WaiveException)
		r.Get("/blockers/{payroll_run_id}", h.GetReleaseBlockers)
	})
}

// ── POST /v1/payroll-exceptions ──────────────────────────────────────────────────

func (h *Handler) RaiseException(w http.ResponseWriter, r *http.Request) {
	var req domain.RaiseExceptionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.PayrollRunID == "" || req.ExceptionCode == "" || req.Severity == "" || req.Description == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "payroll_run_id, exception_code, severity, description are required")
		return
	}

	if req.Severity != "BLOCKER" && req.Severity != "CRITICAL" && req.Severity != "WARNING" {
		writeError(w, http.StatusBadRequest, "invalid_severity", "severity must be BLOCKER, CRITICAL, or WARNING")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	legalEntityID := "GLOBAL"

	if req.EmployeeID != nil && *req.EmployeeID != "" && h.employee != nil {
		emp, err := h.employee.ValidateEmployee(r.Context(), tenantID, principalID, *req.EmployeeID)
		if err != nil {
			if errors.Is(err, domain.ErrEmployeeNotFound) {
				writeError(w, http.StatusBadRequest, "employee_invalid", err.Error())
				return
			}
			h.log.Warn("employee validation call failed, proceeding", zap.Error(err))
		} else if emp != nil && emp.LegalEntityID != "" {
			legalEntityID = emp.LegalEntityID
		}
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionExceptionRaise); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	details := req.DetailsJSON
	if details == "" {
		details = "{}"
	}

	exc := &domain.PayrollException{
		ExceptionID:   uuid.NewString(),
		TenantID:      tenantID,
		PayrollRunID:  req.PayrollRunID,
		EmployeeID:    req.EmployeeID,
		ExceptionCode: req.ExceptionCode,
		Severity:      req.Severity,
		Description:   req.Description,
		DetailsJSON:   details,
		Status:        "OPEN",
		CreatedAt:     time.Now().UTC(),
	}

	if err := h.store.CreateException(r.Context(), exc); err != nil {
		h.log.Error("failed to create exception", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	correlationID := getCorrelationID(r)
	h.publisher.PublishExceptionRaised(r.Context(), correlationID, *exc)

	if exc.Severity == "BLOCKER" {
		h.publisher.PublishBlockerFlagged(r.Context(), correlationID, exc.PayrollRunID, 1)
	}

	writeJSON(w, http.StatusCreated, exc)
}

// ── GET /v1/payroll-exceptions ───────────────────────────────────────────────────

func (h *Handler) ListExceptions(w http.ResponseWriter, r *http.Request) {
	payrollRunID := r.URL.Query().Get("payroll_run_id")
	employeeID := r.URL.Query().Get("employee_id")
	status := r.URL.Query().Get("status")
	severity := r.URL.Query().Get("severity")

	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	list, err := h.store.ListExceptions(r.Context(), payrollRunID, employeeID, status, severity)
	if err != nil {
		h.log.Error("failed to list exceptions", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if list == nil {
		list = []domain.PayrollException{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── GET /v1/payroll-exceptions/{id} ──────────────────────────────────────────────

func (h *Handler) GetException(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	exc, err := h.store.GetException(r.Context(), id)
	if errors.Is(err, domain.ErrExceptionNotFound) {
		writeError(w, http.StatusNotFound, "exception_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch exception", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, exc)
}

// ── POST /v1/payroll-exceptions/{id}/resolve ─────────────────────────────────────

func (h *Handler) ResolveException(w http.ResponseWriter, r *http.Request) {
	h.handleExceptionResolution(w, r, "RESOLVED", actionExceptionResolve)
}

// ── POST /v1/payroll-exceptions/{id}/waive ───────────────────────────────────────

func (h *Handler) WaiveException(w http.ResponseWriter, r *http.Request) {
	h.handleExceptionResolution(w, r, "WAIVED", actionExceptionWaive)
}

func (h *Handler) handleExceptionResolution(w http.ResponseWriter, r *http.Request, targetStatus, actionType string) {
	id := chi.URLParam(r, "id")

	var req domain.ResolveExceptionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.ResolutionNotes == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "resolution_notes are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	exc, err := h.store.GetException(r.Context(), id)
	if errors.Is(err, domain.ErrExceptionNotFound) {
		writeError(w, http.StatusNotFound, "exception_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch exception for resolution", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	legalEntityID := "GLOBAL"
	if exc.EmployeeID != nil && *exc.EmployeeID != "" && h.employee != nil {
		tenantID := svcmiddleware.TenantFromContext(r.Context())
		emp, _ := h.employee.ValidateEmployee(r.Context(), tenantID, principalID, *exc.EmployeeID)
		if emp != nil && emp.LegalEntityID != "" {
			legalEntityID = emp.LegalEntityID
		}
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionType); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if err := h.store.ResolveException(r.Context(), id, req.ResolutionNotes, principalID, targetStatus); err != nil {
		if errors.Is(err, domain.ErrAlreadyResolved) {
			writeError(w, http.StatusConflict, "already_resolved", err.Error())
			return
		}
		h.log.Error("failed to resolve exception", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	now := time.Now().UTC()
	exc.Status = targetStatus
	exc.ResolutionNotes = &req.ResolutionNotes
	exc.ResolvedBy = &principalID
	exc.ResolvedAt = &now

	correlationID := getCorrelationID(r)
	h.publisher.PublishExceptionResolved(r.Context(), correlationID, *exc)

	writeJSON(w, http.StatusOK, exc)
}

// ── GET /v1/payroll-exceptions/blockers/{payroll_run_id} ─────────────────────────

func (h *Handler) GetReleaseBlockers(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "payroll_run_id")

	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	summary, err := h.store.GetReleaseBlockers(r.Context(), runID)
	if err != nil {
		h.log.Error("failed to query release blockers", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, summary)
}

// ── Helpers ──────────────────────────────────────────────────────────────────────

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrIdentityMissing))
		return "", false
	}
	return principalID, true
}

func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, domain.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
	} else {
		writeError(w, http.StatusServiceUnavailable, "authz_unavailable", err.Error())
	}
}

func getCorrelationID(r *http.Request) string {
	cid := r.Header.Get("X-Correlation-ID")
	if cid == "" {
		return uuid.NewString()
	}
	return cid
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error_code":    code,
		"error_message": msg,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}