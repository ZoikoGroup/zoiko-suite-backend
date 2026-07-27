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

	"zoiko.io/compensation-svc/internal/domain"
	"zoiko.io/compensation-svc/internal/employee"
	svcmiddleware "zoiko.io/compensation-svc/internal/middleware"
)

type Store interface {
	CreateStructure(ctx context.Context, s *domain.CompensationStructure) error
	ListStructures(ctx context.Context, legalEntityID string) ([]domain.CompensationStructure, error)
	CreateWageRevision(ctx context.Context, rev *domain.WageRevision) error
	GetActiveWageRevision(ctx context.Context, employeeID string) (*domain.WageRevision, error)
	GetWageRevisionHistory(ctx context.Context, employeeID string) ([]domain.WageRevision, error)
	CreateBonusGrant(ctx context.Context, b *domain.BonusGrant) error
	ApproveBonusGrant(ctx context.Context, grantID, approvedBy string) error
	ListBonusGrants(ctx context.Context, employeeID, status string) ([]domain.BonusGrant, error)
}

type Publisher interface {
	PublishCompensationUpdated(ctx context.Context, correlationID string, rev domain.WageRevision)
	PublishBonusApproved(ctx context.Context, correlationID string, b domain.BonusGrant)
	PublishEffectiveChanged(ctx context.Context, correlationID string, rev domain.WageRevision)
}

type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type EmployeeValidator interface {
	ValidateEmployee(ctx context.Context, tenantID, principalID, employeeID string) (*employee.Employee, error)
}

const (
	actionCompCreate   = "COMPENSATION_CREATE"
	actionCompView     = "COMPENSATION_VIEW"
	actionWageRevise   = "WAGE_REVISE"
	actionBonusGrant   = "BONUS_GRANT"
	actionBonusApprove = "BONUS_APPROVE"
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
	r.Route("/v1/compensation", func(r chi.Router) {
		r.Post("/structures", h.CreateStructure)
		r.Get("/structures", h.ListStructures)

		r.Post("/revisions", h.ReviseWage)
		r.Get("/revisions/employee/{employee_id}", h.GetWageHistory)
		r.Get("/revisions/employee/{employee_id}/active", h.GetActiveWage)

		r.Post("/bonuses", h.GrantBonus)
		r.Post("/bonuses/{id}/approve", h.ApproveBonus)
		r.Get("/bonuses", h.ListBonuses)
	})
}

// ── POST /v1/compensation/structures ─────────────────────────────────────────────

func (h *Handler) CreateStructure(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateStructureRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.LegalEntityID == "" || req.Name == "" || req.PayType == "" || req.MinAmount <= 0 || req.MaxAmount <= 0 || req.Currency == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, name, pay_type, min_amount, max_amount, currency are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionCompCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	otMult := 1.50
	if req.OvertimeMultiplier != nil && *req.OvertimeMultiplier > 0 {
		otMult = *req.OvertimeMultiplier
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	now := time.Now().UTC()
	str := &domain.CompensationStructure{
		StructureID:        uuid.NewString(),
		TenantID:           tenantID,
		LegalEntityID:      req.LegalEntityID,
		Name:               req.Name,
		PayType:            req.PayType,
		MinAmount:          req.MinAmount,
		MaxAmount:          req.MaxAmount,
		Currency:           req.Currency,
		OvertimeMultiplier: otMult,
		CreatedAt:          now,
		UpdatedAt:          now,
	}

	if err := h.store.CreateStructure(r.Context(), str); err != nil {
		h.log.Error("failed to create compensation structure", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, str)
}

// ── GET /v1/compensation/structures ──────────────────────────────────────────────

func (h *Handler) ListStructures(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if legalEntityID != "" {
		if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionCompView); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
	}

	list, err := h.store.ListStructures(r.Context(), legalEntityID)
	if err != nil {
		h.log.Error("failed to list structures", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if list == nil {
		list = []domain.CompensationStructure{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── POST /v1/compensation/revisions ──────────────────────────────────────────────

func (h *Handler) ReviseWage(w http.ResponseWriter, r *http.Request) {
	var req domain.ReviseWageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.EmployeeID == "" || req.PayType == "" || req.Amount <= 0 || req.Currency == "" || req.EffectiveFrom == "" || req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "employee_id, pay_type, amount, currency, effective_from, reason are required")
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

	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionWageRevise); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	now := time.Now().UTC()
	rev := &domain.WageRevision{
		RevisionID:    uuid.NewString(),
		TenantID:      tenantID,
		EmployeeID:    req.EmployeeID,
		StructureID:   req.StructureID,
		PayType:       req.PayType,
		Amount:        req.Amount,
		Currency:      req.Currency,
		EffectiveFrom: req.EffectiveFrom,
		Reason:        req.Reason,
		RevisedBy:     principalID,
		Status:        "ACTIVE",
		CreatedAt:     now,
	}

	if err := h.store.CreateWageRevision(r.Context(), rev); err != nil {
		h.log.Error("failed to create wage revision", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	correlationID := getCorrelationID(r)
	h.publisher.PublishCompensationUpdated(r.Context(), correlationID, *rev)
	h.publisher.PublishEffectiveChanged(r.Context(), correlationID, *rev)

	writeJSON(w, http.StatusCreated, rev)
}

// ── GET /v1/compensation/revisions/employee/{employee_id} ────────────────────────

func (h *Handler) GetWageHistory(w http.ResponseWriter, r *http.Request) {
	employeeID := chi.URLParam(r, "employee_id")

	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	history, err := h.store.GetWageRevisionHistory(r.Context(), employeeID)
	if err != nil {
		h.log.Error("failed to fetch wage revision history", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if history == nil {
		history = []domain.WageRevision{}
	}
	writeJSON(w, http.StatusOK, history)
}

// ── GET /v1/compensation/revisions/employee/{employee_id}/active ─────────────────

func (h *Handler) GetActiveWage(w http.ResponseWriter, r *http.Request) {
	employeeID := chi.URLParam(r, "employee_id")

	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	rev, err := h.store.GetActiveWageRevision(r.Context(), employeeID)
	if errors.Is(err, domain.ErrWageRevisionNotFound) {
		writeError(w, http.StatusNotFound, "active_wage_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch active wage revision", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, rev)
}

// ── POST /v1/compensation/bonuses ────────────────────────────────────────────────

func (h *Handler) GrantBonus(w http.ResponseWriter, r *http.Request) {
	var req domain.GrantBonusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.EmployeeID == "" || req.BonusType == "" || req.Amount <= 0 || req.Currency == "" || req.GrantDate == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "employee_id, bonus_type, amount, currency, grant_date are required")
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

	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionBonusGrant); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	now := time.Now().UTC()
	grant := &domain.BonusGrant{
		GrantID:    uuid.NewString(),
		TenantID:   tenantID,
		EmployeeID: req.EmployeeID,
		BonusType:  req.BonusType,
		Amount:     req.Amount,
		Currency:   req.Currency,
		GrantDate:  req.GrantDate,
		Status:     "PENDING",
		CreatedAt:  now,
	}

	if err := h.store.CreateBonusGrant(r.Context(), grant); err != nil {
		h.log.Error("failed to create bonus grant", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, grant)
}

// ── POST /v1/compensation/bonuses/{id}/approve ───────────────────────────────────

func (h *Handler) ApproveBonus(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	list, err := h.store.ListBonusGrants(r.Context(), "", "")
	if err != nil {
		h.log.Error("failed to fetch bonus grant for approval", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	var target *domain.BonusGrant
	for _, b := range list {
		if b.GrantID == id {
			target = &b
			break
		}
	}

	if target == nil {
		writeError(w, http.StatusNotFound, "bonus_not_found", "")
		return
	}

	legalEntityID := "GLOBAL"
	if h.employee != nil {
		tenantID := svcmiddleware.TenantFromContext(r.Context())
		emp, _ := h.employee.ValidateEmployee(r.Context(), tenantID, principalID, target.EmployeeID)
		if emp != nil && emp.LegalEntityID != "" {
			legalEntityID = emp.LegalEntityID
		}
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionBonusApprove); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if err := h.store.ApproveBonusGrant(r.Context(), id, principalID); err != nil {
		if errors.Is(err, domain.ErrInvalidBonusStatus) {
			writeError(w, http.StatusConflict, "invalid_status", err.Error())
			return
		}
		h.log.Error("failed to approve bonus grant", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	target.Status = "APPROVED"
	target.ApprovedBy = &principalID

	correlationID := getCorrelationID(r)
	h.publisher.PublishBonusApproved(r.Context(), correlationID, *target)

	writeJSON(w, http.StatusOK, target)
}

// ── GET /v1/compensation/bonuses ─────────────────────────────────────────────────

func (h *Handler) ListBonuses(w http.ResponseWriter, r *http.Request) {
	employeeID := r.URL.Query().Get("employee_id")
	status := r.URL.Query().Get("status")

	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	list, err := h.store.ListBonusGrants(r.Context(), employeeID, status)
	if err != nil {
		h.log.Error("failed to list bonus grants", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if list == nil {
		list = []domain.BonusGrant{}
	}
	writeJSON(w, http.StatusOK, list)
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