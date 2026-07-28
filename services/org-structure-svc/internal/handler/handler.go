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

	"zoiko.io/org-structure-svc/internal/domain"
	"zoiko.io/org-structure-svc/internal/employee"
	svcmiddleware "zoiko.io/org-structure-svc/internal/middleware"
)

type Store interface {
	CreateDepartment(ctx context.Context, d *domain.Department) (created bool, err error)
	ListDepartments(ctx context.Context, legalEntityID string) ([]domain.Department, error)
	GetDepartment(ctx context.Context, departmentID string) (*domain.Department, error)

	CreatePosition(ctx context.Context, p *domain.Position) (created bool, err error)
	ListPositions(ctx context.Context, departmentID string) ([]domain.Position, error)
	GetPosition(ctx context.Context, positionID string) (*domain.Position, error)

	AssignEmployee(ctx context.Context, req *domain.AssignEmployeeRequest) (*domain.OrgAssignment, error)
	GetEmployeeAssignment(ctx context.Context, employeeID string) (*domain.OrgAssignment, error)
}

type Publisher interface {
	PublishPositionCreated(ctx context.Context, correlationID string, pos domain.Position)
	PublishEmployeeAssigned(ctx context.Context, correlationID string, assign domain.OrgAssignment)
	PublishOrgStructureChanged(ctx context.Context, correlationID string, eventType, entityID string)
}

type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type EmployeeValidator interface {
	ValidateEmployee(ctx context.Context, tenantID, principalID, employeeID string) (*employee.Employee, error)
}

const (
	actionOrgDeptCreate       = "ORG_DEPT_CREATE"
	actionOrgDeptView         = "ORG_DEPT_VIEW"
	actionOrgPositionCreate   = "ORG_POSITION_CREATE"
	actionOrgPositionView     = "ORG_POSITION_VIEW"
	actionOrgAssignmentCreate = "ORG_ASSIGNMENT_CREATE"
	actionOrgAssignmentView   = "ORG_ASSIGNMENT_VIEW"
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
	r.Route("/v1/org", func(r chi.Router) {
		r.Post("/departments", h.CreateDepartment)
		r.Get("/departments", h.ListDepartments)
		r.Get("/departments/{id}", h.GetDepartment)

		r.Post("/positions", h.CreatePosition)
		r.Get("/positions", h.ListPositions)
		r.Get("/positions/{id}", h.GetPosition)

		r.Post("/assignments", h.AssignEmployee)
		r.Get("/assignments/employee/{employee_id}", h.GetEmployeeAssignment)
	})
}

// ── Departments ────────────────────────────────────────────────────────────────────

func (h *Handler) CreateDepartment(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateDepartmentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.LegalEntityID == "" || req.Name == "" || req.Code == "" || req.CostCenterCode == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, name, code, cost_center_code are required")
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

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionOrgDeptCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	now := time.Now().UTC()
	dept := &domain.Department{
		DepartmentID:       uuid.NewString(),
		TenantID:           tenantID,
		LegalEntityID:      req.LegalEntityID,
		Name:               req.Name,
		Code:               req.Code,
		CostCenterCode:     req.CostCenterCode,
		ParentDepartmentID: req.ParentDepartmentID,
		Status:             "ACTIVE",
		CorrelationID:      req.CorrelationID,
		CreatedAt:          now,
		UpdatedAt:          now,
	}

	created, err := h.store.CreateDepartment(r.Context(), dept)
	if err != nil {
		h.log.Error("failed to create department", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if !created {
		writeJSON(w, http.StatusOK, dept)
		return
	}

	correlationID := getCorrelationID(r)
	h.publisher.PublishOrgStructureChanged(r.Context(), correlationID, "DEPARTMENT_CREATED", dept.DepartmentID)

	writeJSON(w, http.StatusCreated, dept)
}

func (h *Handler) ListDepartments(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id is required")
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionOrgDeptView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	list, err := h.store.ListDepartments(r.Context(), legalEntityID)
	if err != nil {
		h.log.Error("failed to list departments", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if list == nil {
		list = []domain.Department{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *Handler) GetDepartment(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	dept, err := h.store.GetDepartment(r.Context(), id)
	if errors.Is(err, domain.ErrDepartmentNotFound) {
		writeError(w, http.StatusNotFound, "department_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to get department", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, dept.LegalEntityID, actionOrgDeptView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	writeJSON(w, http.StatusOK, dept)
}

// ── Positions ──────────────────────────────────────────────────────────────────────

func (h *Handler) CreatePosition(w http.ResponseWriter, r *http.Request) {
	var req domain.CreatePositionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.LegalEntityID == "" || req.DepartmentID == "" || req.Title == "" || req.Code == "" || req.JobLevel == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, department_id, title, code, job_level are required")
		return
	}
	if req.CorrelationID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "correlation_id is required")
		return
	}

	if req.MaxHeadcount <= 0 {
		req.MaxHeadcount = 1
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionOrgPositionCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	now := time.Now().UTC()
	pos := &domain.Position{
		PositionID:       uuid.NewString(),
		TenantID:         tenantID,
		LegalEntityID:    req.LegalEntityID,
		DepartmentID:     req.DepartmentID,
		Title:            req.Title,
		Code:             req.Code,
		JobLevel:         req.JobLevel,
		MaxHeadcount:     req.MaxHeadcount,
		CurrentHeadcount: 0,
		Status:           "ACTIVE",
		CorrelationID:    req.CorrelationID,
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	created, err := h.store.CreatePosition(r.Context(), pos)
	if err != nil {
		h.log.Error("failed to create position", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if !created {
		writeJSON(w, http.StatusOK, pos)
		return
	}

	correlationID := getCorrelationID(r)
	h.publisher.PublishPositionCreated(r.Context(), correlationID, *pos)

	writeJSON(w, http.StatusCreated, pos)
}

func (h *Handler) ListPositions(w http.ResponseWriter, r *http.Request) {
	departmentID := r.URL.Query().Get("department_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if departmentID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "department_id is required")
		return
	}

	dept, err := h.store.GetDepartment(r.Context(), departmentID)
	if errors.Is(err, domain.ErrDepartmentNotFound) {
		writeError(w, http.StatusBadRequest, "department_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch department for position listing", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, dept.LegalEntityID, actionOrgPositionView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	list, err := h.store.ListPositions(r.Context(), departmentID)
	if err != nil {
		h.log.Error("failed to list positions", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if list == nil {
		list = []domain.Position{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *Handler) GetPosition(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	pos, err := h.store.GetPosition(r.Context(), id)
	if errors.Is(err, domain.ErrPositionNotFound) {
		writeError(w, http.StatusNotFound, "position_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to get position", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, pos.LegalEntityID, actionOrgPositionView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	writeJSON(w, http.StatusOK, pos)
}

// ── Org Assignments ─────────────────────────────────────────────────────────

func (h *Handler) AssignEmployee(w http.ResponseWriter, r *http.Request) {
	var req domain.AssignEmployeeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.EmployeeID == "" || req.DepartmentID == "" || req.PositionID == "" || req.EffectiveFrom == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "employee_id, department_id, position_id, effective_from are required")
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

	if req.ManagerEmployeeID != nil && *req.ManagerEmployeeID != "" && h.employee != nil {
		mgr, err := h.employee.ValidateEmployee(r.Context(), tenantID, principalID, *req.ManagerEmployeeID)
		if err != nil {
			if errors.Is(err, domain.ErrEmployeeNotFound) {
				writeError(w, http.StatusBadRequest, "manager_invalid", string(domain.ErrManagerNotFound))
				return
			}
			h.log.Error("manager validation failed", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "employee_validation_failed", domain.ErrEmployeeValidationFailed.Error())
			return
		}
		_ = mgr
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionOrgAssignmentCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	oa, err := h.store.AssignEmployee(r.Context(), &req)
	if err != nil {
		h.log.Error("failed to assign employee", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	correlationID := getCorrelationID(r)
	h.publisher.PublishEmployeeAssigned(r.Context(), correlationID, *oa)

	writeJSON(w, http.StatusCreated, oa)
}

func (h *Handler) GetEmployeeAssignment(w http.ResponseWriter, r *http.Request) {
	empID := chi.URLParam(r, "employee_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	oa, err := h.store.GetEmployeeAssignment(r.Context(), empID)
	if errors.Is(err, domain.ErrAssignmentNotFound) {
		writeError(w, http.StatusNotFound, "assignment_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to get employee assignment", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, oa.LegalEntityID, actionOrgAssignmentView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	writeJSON(w, http.StatusOK, oa)
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
// would evaluate meaninglessly — a prior version logged a warning and
// proceeded under "GLOBAL" on any non-not-found error.
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
