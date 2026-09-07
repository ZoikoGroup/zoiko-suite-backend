package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	CreateHoliday(ctx context.Context, h *domain.Holiday) error
	ListHolidays(ctx context.Context, f domain.HolidayFilter) ([]domain.Holiday, error)
	GetHoliday(ctx context.Context, holidayID string) (*domain.Holiday, error)
	DeactivateHoliday(ctx context.Context, holidayID string) error
}

type Publisher interface {
	PublishLeaveRequested(ctx context.Context, correlationID, legalEntityID, actorID string, r domain.LeaveRequest)
	PublishLeaveApproved(ctx context.Context, correlationID, legalEntityID string, r domain.LeaveRequest)
	PublishLeaveRejected(ctx context.Context, correlationID, legalEntityID string, r domain.LeaveRequest)
	PublishBalanceUpdated(ctx context.Context, correlationID, legalEntityID, actorID string, b domain.LeaveBalance)
}

type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type EmployeeValidator interface {
	ValidateEmployee(ctx context.Context, tenantID, principalID, employeeID string) (*employee.Employee, error)
}

const (
	actionLeaveTypeCreate     = "LEAVE_TYPE_CREATE"
	actionLeaveTypeView       = "LEAVE_TYPE_VIEW"
	actionLeaveBalanceView    = "LEAVE_BALANCE_VIEW"
	actionLeaveBalanceUpdate  = "LEAVE_BALANCE_UPDATE"
	actionLeaveRequestSubmit  = "LEAVE_REQUEST_SUBMIT"
	actionLeaveRequestView    = "LEAVE_REQUEST_VIEW"
	actionLeaveRequestApprove = "LEAVE_REQUEST_APPROVE"
	actionLeaveRequestReject  = "LEAVE_REQUEST_REJECT"
	actionHolidayCreate       = "HOLIDAY_CREATE"
	actionHolidayView         = "HOLIDAY_VIEW"
	actionHolidayDeactivate   = "HOLIDAY_DEACTIVATE"
)

// dateLayout is the only accepted wire format for a calendar date.
const dateLayout = "2006-01-02"

// autoApprovalReviewerID attributes an auto-approval to the policy rather than
// to the submitting principal, so the audit trail never shows someone approving
// their own leave.
const autoApprovalReviewerID = "system:leave-policy-auto-approval"

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

		r.Post("/holidays", h.CreateHoliday)
		r.Get("/holidays", h.ListHolidays)
		r.Delete("/holidays/{id}", h.DeactivateHoliday)
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

	if req.MinNoticeDays < 0 || req.MaxConsecutiveDays < 0 || req.CarryForwardMaxHours < 0 {
		writeError(w, http.StatusBadRequest, "invalid_policy",
			string(domain.ErrInvalidPolicy)+": min_notice_days, max_consecutive_days and carry_forward_max_hours must not be negative")
		return
	}

	// Carrying forward with a zero cap would discard the whole balance at year
	// end, which is a configuration mistake rather than a policy.
	if req.CarryForwardAllowed && req.CarryForwardMaxHours <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_policy",
			string(domain.ErrInvalidPolicy)+": carry_forward_max_hours must be positive when carry_forward_allowed is true")
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

	// Approval is required unless the caller explicitly says otherwise, so an
	// older client that does not know the field keeps its leave reviewed.
	requiresApproval := true
	if req.RequiresApproval != nil {
		requiresApproval = *req.RequiresApproval
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

		CarryForwardAllowed:  req.CarryForwardAllowed,
		CarryForwardMaxHours: req.CarryForwardMaxHours,
		MinNoticeDays:        req.MinNoticeDays,
		MaxConsecutiveDays:   req.MaxConsecutiveDays,
		RequiresApproval:     requiresApproval,
		ColorHex:             req.ColorHex,
		Icon:                 req.Icon,

		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := h.store.CreateLeaveType(r.Context(), lt); errors.Is(err, domain.ErrLeaveTypeCodeExists) {
		writeError(w, http.StatusConflict, "leave_type_code_exists", err.Error())
		return
	} else if err != nil {
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
	legalEntityID, ok := h.resolveEmployeeEntity(w, r, tenantID, principalID, empID)
	if !ok {
		return
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
	legalEntityID, ok := h.resolveEmployeeEntity(w, r, tenantID, principalID, req.EmployeeID)
	if !ok {
		return
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
	h.publisher.PublishBalanceUpdated(r.Context(), correlationID, legalEntityID, principalID, *bal)

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
	if req.CorrelationID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "correlation_id is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	legalEntityID, ok := h.resolveEmployeeEntity(w, r, tenantID, principalID, req.EmployeeID)
	if !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionLeaveRequestSubmit); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	// The leave type carries the policy, so it has to be read before the request
	// is persisted — a request that violates policy must never reach the store.
	leaveType, err := h.store.GetLeaveType(r.Context(), req.LeaveTypeID)
	if errors.Is(err, domain.ErrLeaveTypeNotFound) {
		writeError(w, http.StatusNotFound, "leave_type_not_found", err.Error())
		return
	}
	if err != nil {
		h.log.Error("failed to fetch leave type for policy check", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if code, msg := checkLeavePolicy(*leaveType, req.StartDate, req.EndDate, time.Now().UTC()); code != "" {
		writeError(w, http.StatusBadRequest, code, msg)
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
	h.publisher.PublishLeaveRequested(r.Context(), correlationID, legalEntityID, principalID, *lr)

	// A leave type configured not to require approval is approved here rather
	// than left sitting in SUBMITTED for a reviewer who is never coming.
	if !leaveType.RequiresApproval {
		if err := h.store.ApproveLeaveRequest(r.Context(), lr.RequestID, autoApprovalReviewerID, "auto-approved by leave type policy"); err != nil {
			// The request itself is valid and stored; it simply stays in
			// SUBMITTED for manual review. Report success with the state the
			// caller can actually see rather than failing the submission.
			h.log.Error("auto-approval failed, leaving request in SUBMITTED",
				zap.String("request_id", lr.RequestID), zap.Error(err))
			writeJSON(w, http.StatusCreated, lr)
			return
		}

		approved, err := h.store.GetLeaveRequest(r.Context(), lr.RequestID)
		if err != nil {
			h.log.Error("failed to re-read auto-approved request", zap.Error(err))
			writeJSON(w, http.StatusCreated, lr)
			return
		}

		h.publisher.PublishLeaveApproved(r.Context(), correlationID, legalEntityID, *approved)
		writeJSON(w, http.StatusCreated, approved)
		return
	}

	writeJSON(w, http.StatusCreated, lr)
}

// checkLeavePolicy validates a proposed leave span against the leave type.
// It returns an empty code when the request is acceptable; otherwise an error
// code and message ready for a 400.
//
// now is passed rather than read so the notice-period boundary is testable.
func checkLeavePolicy(lt domain.LeaveType, startDate, endDate string, now time.Time) (string, string) {
	start, err := time.Parse(dateLayout, startDate)
	if err != nil {
		return "invalid_date", "start_date " + string(domain.ErrInvalidDate)
	}
	end, err := time.Parse(dateLayout, endDate)
	if err != nil {
		return "invalid_date", "end_date " + string(domain.ErrInvalidDate)
	}

	if end.Before(start) {
		return "invalid_range", string(domain.ErrEndBeforeStart)
	}

	if lt.MaxConsecutiveDays > 0 {
		// Inclusive of both endpoints: a single-day request spans one day.
		spanDays := int(end.Sub(start).Hours()/24) + 1
		if spanDays > lt.MaxConsecutiveDays {
			return "span_too_long", fmt.Sprintf("%s: %d days requested, %d allowed",
				string(domain.ErrSpanTooLong), spanDays, lt.MaxConsecutiveDays)
		}
	}

	if lt.MinNoticeDays > 0 {
		// Compare whole days: a request submitted at 23:00 for leave starting
		// two days later gives two days of notice, not one and a fraction.
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		noticeDays := int(start.Sub(today).Hours() / 24)
		if noticeDays < lt.MinNoticeDays {
			return "notice_too_short", fmt.Sprintf("%s: %d days notice given, %d required",
				string(domain.ErrNoticeTooShort), noticeDays, lt.MinNoticeDays)
		}
	}

	return "", ""
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

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	legalEntityID, ok := h.resolveEmployeeEntity(w, r, tenantID, principalID, lr.EmployeeID)
	if !ok {
		return
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
		h.publisher.PublishLeaveApproved(r.Context(), correlationID, legalEntityID, *lr)
	} else {
		h.publisher.PublishLeaveRejected(r.Context(), correlationID, legalEntityID, *lr)
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

// resolveEmployeeEntity confirms an employee exists and returns their real
// legal entity, to be used as the authorization scope for the action being
// performed. Fails closed: if employee-master-svc is unreachable or returns
// anything other than a clean "not found," the request is rejected (503)
// rather than proceeding under a placeholder entity that authorization
// would evaluate meaninglessly — prior versions of these call sites either
// logged a warning and proceeded, or discarded the error entirely.
func (h *Handler) resolveEmployeeEntity(w http.ResponseWriter, r *http.Request, tenantID, principalID, employeeID string) (string, bool) {
	if h.employee == nil {
		return "GLOBAL", true
	}
	emp, err := h.employee.ValidateEmployee(r.Context(), tenantID, principalID, employeeID)
	if err != nil {
		if errors.Is(err, domain.ErrEmployeeNotFound) {
			writeError(w, http.StatusBadRequest, "employee_invalid", err.Error())
			return "", false
		}
		h.log.Error("employee validation failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "employee_validation_failed", domain.ErrEmployeeValidationFailed.Error())
		return "", false
	}
	if emp == nil || emp.LegalEntityID == "" {
		return "GLOBAL", true
	}
	return emp.LegalEntityID, true
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
