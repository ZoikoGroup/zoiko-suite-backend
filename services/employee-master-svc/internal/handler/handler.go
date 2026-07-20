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

	"zoiko.io/employee-master-svc/internal/domain"
	svcmiddleware "zoiko.io/employee-master-svc/internal/middleware"
)

type Store interface {
	CreateEmployee(ctx context.Context, emp *domain.Employee) error
	GetEmployee(ctx context.Context, id string) (*domain.Employee, error)
	ListEmployees(ctx context.Context, legalEntityID, status, workerType string) ([]domain.Employee, error)
	UpdateStatus(ctx context.Context, id, newStatus string, terminationDate *string) error
}

type Publisher interface {
	PublishEmployeeCreated(ctx context.Context, correlationID string, emp domain.Employee)
	PublishEmployeeHired(ctx context.Context, correlationID string, emp domain.Employee)
	PublishStatusChanged(ctx context.Context, correlationID string, emp domain.Employee, oldStatus string)
	PublishEmployeeTerminated(ctx context.Context, correlationID string, emp domain.Employee)
}

type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

const (
	actionEmployeeCreate       = "EMPLOYEE_CREATE"
	actionEmployeeView         = "EMPLOYEE_VIEW"
	actionEmployeeUpdateStatus = "EMPLOYEE_UPDATE_STATUS"
)

type Handler struct {
	store     Store
	publisher Publisher
	authz     AuthZClient
	log       *zap.Logger
}

func New(store Store, publisher Publisher, authz AuthZClient, log *zap.Logger) *Handler {
	return &Handler{
		store:     store,
		publisher: publisher,
		authz:     authz,
		log:       log,
	}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/employees", func(r chi.Router) {
		r.Post("/", h.CreateEmployee)
		r.Get("/", h.ListEmployees)
		r.Get("/{id}", h.GetEmployee)
		r.Put("/{id}/status", h.UpdateStatus)
	})
}

// ── POST /v1/employees ────────────────────────────────────────────────────────────

func (h *Handler) CreateEmployee(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateEmployeeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.LegalEntityID == "" || req.FirstName == "" || req.LastName == "" || req.Email == "" || req.WorkerType == "" || req.HireDate == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, first_name, last_name, email, worker_type, hire_date are required")
		return
	}

	if req.WorkerType != "FULL_TIME" && req.WorkerType != "PART_TIME" && req.WorkerType != "CONTRACTOR" {
		writeError(w, http.StatusBadRequest, "invalid_worker_type", "worker_type must be FULL_TIME, PART_TIME, or CONTRACTOR")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionEmployeeCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	correlationID := getCorrelationID(r)

	now := time.Now().UTC()
	emp := &domain.Employee{
		EmployeeID:    uuid.NewString(),
		TenantID:      tenantID,
		LegalEntityID: req.LegalEntityID,
		FirstName:     req.FirstName,
		LastName:      req.LastName,
		Email:         req.Email,
		WorkerType:    req.WorkerType,
		Status:        "ACTIVE",
		HireDate:      req.HireDate,
		EffectiveFrom: now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	if err := h.store.CreateEmployee(r.Context(), emp); errors.Is(err, domain.ErrEmailAlreadyExists) {
		writeError(w, http.StatusConflict, "email_exists", err.Error())
		return
	} else if err != nil {
		h.log.Error("failed to create employee", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	h.publisher.PublishEmployeeCreated(r.Context(), correlationID, *emp)
	h.publisher.PublishEmployeeHired(r.Context(), correlationID, *emp)

	writeJSON(w, http.StatusCreated, emp)
}

// ── GET /v1/employees ─────────────────────────────────────────────────────────────

func (h *Handler) ListEmployees(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	status := r.URL.Query().Get("status")
	workerType := r.URL.Query().Get("worker_type")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if legalEntityID != "" {
		if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionEmployeeView); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
	}

	list, err := h.store.ListEmployees(r.Context(), legalEntityID, status, workerType)
	if err != nil {
		h.log.Error("failed to list employees", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if list == nil {
		list = []domain.Employee{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── GET /v1/employees/{id} ────────────────────────────────────────────────────────

func (h *Handler) GetEmployee(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	emp, err := h.store.GetEmployee(r.Context(), id)
	if errors.Is(err, domain.ErrEmployeeNotFound) {
		writeError(w, http.StatusNotFound, "employee_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch employee", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, emp.LegalEntityID, actionEmployeeView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	writeJSON(w, http.StatusOK, emp)
}

// ── PUT /v1/employees/{id}/status ─────────────────────────────────────────────────

func (h *Handler) UpdateStatus(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req domain.UpdateStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.Status != "ONBOARDING" && req.Status != "ACTIVE" && req.Status != "SUSPENDED" && req.Status != "TERMINATED" {
		writeError(w, http.StatusBadRequest, "invalid_status", "status must be ONBOARDING, ACTIVE, SUSPENDED, or TERMINATED")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	emp, err := h.store.GetEmployee(r.Context(), id)
	if errors.Is(err, domain.ErrEmployeeNotFound) {
		writeError(w, http.StatusNotFound, "employee_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch employee for status update", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, emp.LegalEntityID, actionEmployeeUpdateStatus); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	oldStatus := emp.Status
	if err := h.store.UpdateStatus(r.Context(), id, req.Status, req.TerminationDate); err != nil {
		h.log.Error("failed to update employee status", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	emp.Status = req.Status
	emp.TerminationDate = req.TerminationDate
	now := time.Now().UTC()
	emp.UpdatedAt = now
	if req.Status == "TERMINATED" {
		emp.EffectiveTo = &now
	}

	correlationID := getCorrelationID(r)
	h.publisher.PublishStatusChanged(r.Context(), correlationID, *emp, oldStatus)

	if req.Status == "TERMINATED" {
		h.publisher.PublishEmployeeTerminated(r.Context(), correlationID, *emp)
	}

	writeJSON(w, http.StatusOK, emp)
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