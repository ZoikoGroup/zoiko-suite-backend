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

	"zoiko.io/leave-absence-svc/internal/domain"
	"zoiko.io/leave-absence-svc/internal/employee"
	svcmiddleware "zoiko.io/leave-absence-svc/internal/middleware"
)

type Store interface {
	CreateLeaveType(ctx context.Context, lt *domain.LeaveType) error
	ListLeaveTypes(ctx context.Context, legalEntityID string) ([]domain.LeaveType, error)
	GetLeaveType(ctx context.Context, leaveTypeID string) (*domain.LeaveType, error)
	GetLeaveBalances(ctx context.Context, employeeID string) ([]domain.LeaveBalance, error)
	AccrueLeaveBalance(ctx context.Context, employeeID, leaveTypeID string, hours float64) (*domain.LeaveBalance, error)
	SubmitLeaveRequest(ctx context.Context, req *domain.SubmitLeaveRequest) (*domain.LeaveRequest, error)
	GetLeaveRequest(ctx context.Context, requestID string) (*domain.LeaveRequest, error)
	ListLeaveRequests(ctx context.Context, employeeID, status string) ([]domain.LeaveRequest, error)
	ApproveLeaveRequest(ctx context.Context, requestID, reviewerID, notes string) error
	RejectLeaveRequest(ctx context.Context, requestID, reviewerID, notes string) error
}

type Publisher interface {
	PublishLeaveRequested(ctx context.Context, correlationID string, r domain.LeaveRequest)
	PublishLeaveApproved(ctx context.Context, correlationID string, r domain.LeaveRequest)
	PublishLeaveRejected(ctx context.Context, correlationID string, r domain.LeaveRequest)
	PublishBalanceUpdated(ctx context.Context, correlationID string, b domain.LeaveBalance)
}

type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type EmployeeValidator interface {
	ValidateEmployee(ctx context.Context, tenantID, principalID, employeeID string) (*employee.Employee, error)
}

const (
	actionLeaveTypeCreate   = "LEAVE_TYPE_CREATE"
	actionLeaveTypeView     = "LEAVE_TYPE_VIEW"
	actionLeaveBalanceView  = "LEAVE_BALANCE_VIEW"
	actionLeaveBalanceUpdate= "LEAVE_BALANCE_UPDATE"
	actionLeaveRequestSubmit= "LEAVE_REQUEST_SUBMIT"
	actionLeaveRequestView  = "LEAVE_REQUEST_VIEW"
	actionLeaveRequestApprove= "LEAVE_REQUEST_APPROVE"
	actionLeaveRequestReject = "LEAVE_REQUEST_REJECT"
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
	r.Route("/v1/leave", func(r chi.Router) {
		r.Post("/types", h.CreateLeaveType)
		r.Get("/types", h.ListLeaveTypes)

		r.Get("/balances/employee/{employee_id}", h.GetLeaveBalances)
		r.Post("/balances/accrue", h.AccrueLeaveBalance)

		r.Post("/requests", h.SubmitLeaveRequest)
		r.Get("/requests", h.ListLeaveRequests)
		r.Get("/requests/{id}", h.GetLeaveRequest)
		r.Post("/requests/{id}/approve", h.ApproveLeaveRequest)
		r.Post("/requests/{id}/reject", h.RejectLeaveRequest)
	})
}

// ── POST /v1/leave/types ───────────────────────────────────────────────────────────

func (h *Handler) CreateLeaveType(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateLeaveTypeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.LegalEntityID == "" || req.Name == "" || req.Code == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, name, code are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionLeaveTypeCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	now := time.Now().UTC()
	lt := &domain.LeaveType{
		LeaveTypeID:        uuid.NewString(),
		TenantID:           tenantID,
		LegalEntityID:      req.LegalEntityID,
		Name:               req.Name,
		Code:               req.Code,
		IsPaid:             req.IsPaid,
		AccrualRatePerYear: req.AccrualRatePerYear,
		MaxBalance:         req.MaxBalance,
		Status:             "ACTIVE",
		CreatedAt:          now,
		UpdatedAt:          now,
	}

	if err := h.store.CreateLeaveType(r.Context(), lt); err != nil {
		h.log.Error("failed to create leave type", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, lt)
}

// ── GET /v1/leave/types ────────────────────────────────────────────────────────────

func (h *Handler) ListLeaveTypes(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if legalEntityID != "" {
		if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionLeaveTypeView); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
	}

	list, err := h.store.ListLeaveTypes(r.Context(), legalEntityID)
	if err != nil {
		h.log.Error("failed to list leave types", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if list == nil {
		list = []domain.LeaveType{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── GET /v1/leave/balances/employee/{employee_id} ─────────────────────────────────

func (h *Handler) GetLeaveBalances(w http.ResponseWriter, r *http.Request) {
	empID := chi.URLParam(r, "employee_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	legalEntityID := "GLOBAL"

	if h.employee != nil {
		emp, err := h.employee.ValidateEmployee(r.Context(), tenantID, principalID, empID)
		if err != nil {
			if errors.Is(err, domain.ErrEmployeeNotFound) {
				writeError(w, http.StatusNotFound, "employee_not_found", err.Error())
				return
			}
			h.log.Warn("employee validation call failed, proceeding", zap.Error(err))
		} else if emp != nil && emp.LegalEntityID != "" {
			legalEntityID = emp.LegalEntityID
		}
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionLeaveBalanceView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	balances, err := h.store.GetLeaveBalances(r.Context(), empID)
	if err != nil {
		h.log.Error("failed to fetch leave balances", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if balances == nil {
		balances = []domain.LeaveBalance{}
	}
	writeJSON(w, http.StatusOK, balances)
}

// ── POST /v1/leave/balances/accrue ────────────────────────────────────────────────

func (h *Handler) AccrueLeaveBalance(w http.ResponseWriter, r *http.Request) {
	var req domain.AccrueBalanceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.EmployeeID == "" || req.LeaveTypeID == "" || req.Hours <= 0 {
		writeError(w, http.StatusBadRequest, "missing_fields", "employee_id, leave_type_id, hours (> 0) are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	legalEntityID := "GLOBAL"

	if h.employee != nil {
		emp, err := h.employee.ValidateEmployee(r.Context(), tenantID, principalID, req.EmployeeID)
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

	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionLeaveBalanceUpdate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	bal, err := h.store.AccrueLeaveBalance(r.Context(), req.EmployeeID, req.LeaveTypeID, req.Hours)
	if err != nil {
		h.log.Error("failed to accrue leave balance", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	correlationID := getCorrelationID(r)
	h.publisher.PublishBalanceUpdated(r.Context(), correlationID, *bal)

	writeJSON(w, http.StatusOK, bal)
}

// ── POST /v1/leave/requests ───────────────────────────────────────────────────────

func (h *Handler) SubmitLeaveRequest(w http.ResponseWriter, r *http.Request) {
	var req domain.SubmitLeaveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.EmployeeID == "" || req.LeaveTypeID == "" || req.StartDate == "" || req.EndDate == "" || req.TotalHours <= 0 {
		writeError(w, http.StatusBadRequest, "missing_fields", "employee_id, leave_type_id, start_date, end_date, total_hours are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	legalEntityID := "GLOBAL"

	if h.employee != nil {
		emp, err := h.employee.ValidateEmployee(r.Context(), tenantID, principalID, req.EmployeeID)
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

	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionLeaveRequestSubmit); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	lr, err := h.store.SubmitLeaveRequest(r.Context(), &req)
	if errors.Is(err, domain.ErrInsufficientBalance) {
		writeError(w, http.StatusBadRequest, "insufficient_balance", err.Error())
		return
	}
	if err != nil {
		h.log.Error("failed to submit leave request", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	correlationID := getCorrelationID(r)
	h.publisher.PublishLeaveRequested(r.Context(), correlationID, *lr)

	writeJSON(w, http.StatusCreated, lr)
}

// ── GET /v1/leave/requests ────────────────────────────────────────────────────────

func (h *Handler) ListLeaveRequests(w http.ResponseWriter, r *http.Request) {
	employeeID := r.URL.Query().Get("employee_id")
	status := r.URL.Query().Get("status")

	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	list, err := h.store.ListLeaveRequests(r.Context(), employeeID, status)
	if err != nil {
		h.log.Error("failed to list leave requests", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if list == nil {
		list = []domain.LeaveRequest{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── GET /v1/leave/requests/{id} ────────────────────────────────────────────────────

func (h *Handler) GetLeaveRequest(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	lr, err := h.store.GetLeaveRequest(r.Context(), id)
	if errors.Is(err, domain.ErrRequestNotFound) {
		writeError(w, http.StatusNotFound, "request_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch leave request", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, lr)
}

// ── POST /v1/leave/requests/{id}/approve ──────────────────────────────────────────

func (h *Handler) ApproveLeaveRequest(w http.ResponseWriter, r *http.Request) {
	h.handleLeaveReview(w, r, actionLeaveRequestApprove, true)
}

// ── POST /v1/leave/requests/{id}/reject ───────────────────────────────────────────

func (h *Handler) RejectLeaveRequest(w http.ResponseWriter, r *http.Request) {
	h.handleLeaveReview(w, r, actionLeaveRequestReject, false)
}

func (h *Handler) handleLeaveReview(w http.ResponseWriter, r *http.Request, actionType string, isApprove bool) {
	id := chi.URLParam(r, "id")

	var req domain.ReviewLeaveRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	lr, err := h.store.GetLeaveRequest(r.Context(), id)
	if errors.Is(err, domain.ErrRequestNotFound) {
		writeError(w, http.StatusNotFound, "request_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch leave request for review", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	legalEntityID := "GLOBAL"
	if h.employee != nil {
		tenantID := svcmiddleware.TenantFromContext(r.Context())
		emp, _ := h.employee.ValidateEmployee(r.Context(), tenantID, principalID, lr.EmployeeID)
		if emp != nil && emp.LegalEntityID != "" {
			legalEntityID = emp.LegalEntityID
		}
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionType); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if isApprove {
		err = h.store.ApproveLeaveRequest(r.Context(), id, principalID, req.ReviewerNotes)
	} else {
		err = h.store.RejectLeaveRequest(r.Context(), id, principalID, req.ReviewerNotes)
	}

	if errors.Is(err, domain.ErrInvalidStatusTransition) {
		writeError(w, http.StatusConflict, "invalid_status_transition", err.Error())
		return
	}
	if err != nil {
		h.log.Error("failed to review leave request", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	now := time.Now().UTC()
	if isApprove {
		lr.Status = "APPROVED"
	} else {
		lr.Status = "REJECTED"
	}
	lr.ReviewerID = &principalID
	lr.ReviewerNotes = &req.ReviewerNotes
	lr.ReviewedAt = &now

	correlationID := getCorrelationID(r)
	if isApprove {
		h.publisher.PublishLeaveApproved(r.Context(), correlationID, *lr)
	} else {
		h.publisher.PublishLeaveRejected(r.Context(), correlationID, *lr)
	}

	writeJSON(w, http.StatusOK, lr)
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