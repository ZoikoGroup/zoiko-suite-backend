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

	"zoiko.io/employee-master-svc/internal/domain"
	svcmiddleware "zoiko.io/employee-master-svc/internal/middleware"
)

type Store interface {
	CreateEmployee(ctx context.Context, emp *domain.Employee) error
	GetEmployee(ctx context.Context, id string) (*domain.Employee, error)
	ListEmployees(ctx context.Context, filter domain.EmployeeFilter) ([]domain.Employee, error)
	UpdateEmployee(ctx context.Context, emp *domain.Employee) error
	UpdateStatus(ctx context.Context, id, newStatus string, terminationDate *string) error
}

type Publisher interface {
	PublishEmployeeCreated(ctx context.Context, correlationID, actorID string, emp domain.Employee)
	PublishEmployeeHired(ctx context.Context, correlationID, actorID string, emp domain.Employee)
	PublishEmployeeUpdated(ctx context.Context, correlationID, actorID string, emp domain.Employee)
	PublishStatusChanged(ctx context.Context, correlationID, actorID string, emp domain.Employee, oldStatus string)
	PublishEmployeeTerminated(ctx context.Context, correlationID, actorID string, emp domain.Employee)
}

type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

const (
	actionEmployeeCreate       = "EMPLOYEE_CREATE"
	actionEmployeeView         = "EMPLOYEE_VIEW"
	actionEmployeeUpdate       = "EMPLOYEE_UPDATE"
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
		r.Put("/{id}", h.UpdateEmployee)
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

	if !validGender(req.Gender) {
		writeError(w, http.StatusBadRequest, "invalid_gender", genderMessage)
		return
	}

	for _, d := range []struct {
		field string
		value *string
	}{
		{"date_of_birth", req.DateOfBirth},
		{"confirmation_date", req.ConfirmationDate},
	} {
		if !validDate(d.value) {
			writeError(w, http.StatusBadRequest, "invalid_date", d.field+" "+string(domain.ErrInvalidDate))
			return
		}
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

	empNum := req.EmployeeNumber
	if empNum == "" {
		empNum = fmt.Sprintf("EMP-%s", uuid.NewString()[:8])
	}

	jobTitle := req.JobTitle
	if jobTitle == "" {
		jobTitle = "Employee"
	}

	now := time.Now().UTC()
	emp := &domain.Employee{
		EmployeeID:        uuid.NewString(),
		TenantID:          tenantID,
		LegalEntityID:     req.LegalEntityID,
		EmployeeNumber:    empNum,
		FirstName:         req.FirstName,
		LastName:          req.LastName,
		Email:             req.Email,
		Phone:             req.Phone,
		JobTitle:          jobTitle,
		DepartmentID:      req.DepartmentID,
		ManagerEmployeeID: req.ManagerEmployeeID,
		WorkerType:        req.WorkerType,
		Status:            "ACTIVE",
		HireDate:          req.HireDate,
		EffectiveFrom:     now,

		DateOfBirth:       req.DateOfBirth,
		Gender:            req.Gender,
		ProfilePictureURL: req.ProfilePictureURL,
		PersonalEmail:     req.PersonalEmail,
		WorkEmail:         req.WorkEmail,

		CurrentAddress:   req.CurrentAddress,
		PermanentAddress: req.PermanentAddress,
		City:             req.City,
		State:            req.State,
		Country:          req.Country,
		PostalCode:       req.PostalCode,

		Company:          req.Company,
		BusinessUnit:     req.BusinessUnit,
		Division:         req.Division,
		Team:             req.Team,
		DesignationID:    req.DesignationID,
		ConfirmationDate: req.ConfirmationDate,

		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := h.store.CreateEmployee(r.Context(), emp); errors.Is(err, domain.ErrEmailAlreadyExists) {
		writeError(w, http.StatusConflict, "email_exists", err.Error())
		return
	} else if errors.Is(err, domain.ErrEmployeeNumberExists) {
		writeError(w, http.StatusConflict, "employee_number_exists", err.Error())
		return
	} else if errors.Is(err, domain.ErrWorkEmailAlreadyExists) {
		writeError(w, http.StatusConflict, "work_email_exists", err.Error())
		return
	} else if err != nil {
		h.log.Error("failed to create employee", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	h.publisher.PublishEmployeeCreated(r.Context(), correlationID, principalID, *emp)
	h.publisher.PublishEmployeeHired(r.Context(), correlationID, principalID, *emp)

	writeJSON(w, http.StatusCreated, emp)
}

// ── GET /v1/employees ─────────────────────────────────────────────────────────────

func (h *Handler) ListEmployees(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := domain.EmployeeFilter{
		LegalEntityID:     q.Get("legal_entity_id"),
		Status:            q.Get("status"),
		WorkerType:        q.Get("worker_type"),
		DepartmentID:      q.Get("department_id"),
		ManagerEmployeeID: q.Get("manager_employee_id"),
		BusinessUnit:      q.Get("business_unit"),
		Division:          q.Get("division"),
		DesignationID:     q.Get("designation_id"),
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	// legal_entity_id stays mandatory: it is what the authorization decision is
	// scoped to, so without it there is nothing to authorize the listing against.
	if filter.LegalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id is required")
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, filter.LegalEntityID, actionEmployeeView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	list, err := h.store.ListEmployees(r.Context(), filter)
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

// ── PUT /v1/employees/{id} ────────────────────────────────────────────────────────

func (h *Handler) UpdateEmployee(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req domain.UpdateEmployeeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
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
		h.log.Error("failed to fetch employee for update", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, emp.LegalEntityID, actionEmployeeUpdate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if req.FirstName != nil {
		emp.FirstName = *req.FirstName
	}
	if req.LastName != nil {
		emp.LastName = *req.LastName
	}
	if req.Phone != nil {
		emp.Phone = req.Phone
	}
	if req.JobTitle != nil {
		emp.JobTitle = *req.JobTitle
	}
	if req.DepartmentID != nil {
		emp.DepartmentID = req.DepartmentID
	}
	if req.ManagerEmployeeID != nil {
		emp.ManagerEmployeeID = req.ManagerEmployeeID
	}
	if req.WorkerType != nil {
		if *req.WorkerType != "FULL_TIME" && *req.WorkerType != "PART_TIME" && *req.WorkerType != "CONTRACTOR" {
			writeError(w, http.StatusBadRequest, "invalid_worker_type", "worker_type must be FULL_TIME, PART_TIME, or CONTRACTOR")
			return
		}
		emp.WorkerType = *req.WorkerType
	}

	if req.Gender != nil {
		if !validGender(req.Gender) {
			writeError(w, http.StatusBadRequest, "invalid_gender", genderMessage)
			return
		}
		emp.Gender = req.Gender
	}
	if req.DateOfBirth != nil {
		if !validDate(req.DateOfBirth) {
			writeError(w, http.StatusBadRequest, "invalid_date", "date_of_birth "+string(domain.ErrInvalidDate))
			return
		}
		emp.DateOfBirth = req.DateOfBirth
	}
	if req.ConfirmationDate != nil {
		if !validDate(req.ConfirmationDate) {
			writeError(w, http.StatusBadRequest, "invalid_date", "confirmation_date "+string(domain.ErrInvalidDate))
			return
		}
		emp.ConfirmationDate = req.ConfirmationDate
	}
	if req.ProfilePictureURL != nil {
		emp.ProfilePictureURL = req.ProfilePictureURL
	}
	if req.PersonalEmail != nil {
		emp.PersonalEmail = req.PersonalEmail
	}
	if req.WorkEmail != nil {
		emp.WorkEmail = req.WorkEmail
	}
	if req.CurrentAddress != nil {
		emp.CurrentAddress = req.CurrentAddress
	}
	if req.PermanentAddress != nil {
		emp.PermanentAddress = req.PermanentAddress
	}
	if req.City != nil {
		emp.City = req.City
	}
	if req.State != nil {
		emp.State = req.State
	}
	if req.Country != nil {
		emp.Country = req.Country
	}
	if req.PostalCode != nil {
		emp.PostalCode = req.PostalCode
	}
	if req.Company != nil {
		emp.Company = req.Company
	}
	if req.BusinessUnit != nil {
		emp.BusinessUnit = req.BusinessUnit
	}
	if req.Division != nil {
		emp.Division = req.Division
	}
	if req.Team != nil {
		emp.Team = req.Team
	}
	if req.DesignationID != nil {
		emp.DesignationID = req.DesignationID
	}

	emp.UpdatedAt = time.Now().UTC()

	if err := h.store.UpdateEmployee(r.Context(), emp); errors.Is(err, domain.ErrWorkEmailAlreadyExists) {
		writeError(w, http.StatusConflict, "work_email_exists", err.Error())
		return
	} else if err != nil {
		h.log.Error("failed to update employee profile", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	correlationID := getCorrelationID(r)
	h.publisher.PublishEmployeeUpdated(r.Context(), correlationID, principalID, *emp)

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

	if req.Status == "TERMINATED" && (req.TerminationDate == nil || *req.TerminationDate == "") {
		writeError(w, http.StatusBadRequest, "missing_fields", "termination_date is required when status is TERMINATED")
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
	h.publisher.PublishStatusChanged(r.Context(), correlationID, principalID, *emp, oldStatus)

	if req.Status == "TERMINATED" {
		h.publisher.PublishEmployeeTerminated(r.Context(), correlationID, principalID, *emp)
	}

	writeJSON(w, http.StatusOK, emp)
}

// ── Helpers ──────────────────────────────────────────────────────────────────────

// allowedGenders mirrors the employees_gender_check constraint in migration
// 000002. Kept here as well so a bad value is a 400 naming the field rather than
// a 503 from a constraint violation the caller cannot interpret.
var allowedGenders = map[string]bool{
	"MALE":        true,
	"FEMALE":      true,
	"NON_BINARY":  true,
	"OTHER":       true,
	"UNSPECIFIED": true,
}

const genderMessage = "gender must be MALE, FEMALE, NON_BINARY, OTHER, or UNSPECIFIED"

// validGender accepts nil — the field is optional to disclose.
func validGender(g *string) bool {
	if g == nil {
		return true
	}
	return allowedGenders[*g]
}

// validDate accepts nil, and otherwise requires a real calendar date in
// YYYY-MM-DD. Postgres would reject a malformed one anyway, but as a 503 that
// reads like the store is down rather than a 400 the caller can act on.
func validDate(d *string) bool {
	if d == nil {
		return true
	}
	_, err := time.Parse("2006-01-02", *d)
	return err == nil
}

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
