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

	"zoiko.io/employment-contracts-svc/internal/domain"
	"zoiko.io/employment-contracts-svc/internal/employee"

	svcmiddleware "zoiko.io/employment-contracts-svc/internal/middleware"
)

type Store interface {
	IssueContract(ctx context.Context, c *domain.EmploymentContract) error
	GetContract(ctx context.Context, id string) (*domain.EmploymentContract, error)
	GetActiveContractByEmployee(ctx context.Context, employeeID string) (*domain.EmploymentContract, error)
	ListContracts(ctx context.Context, legalEntityID, employeeID, status string) ([]domain.EmploymentContract, error)
	GetContractVersionHistory(ctx context.Context, contractNumber string) ([]domain.EmploymentContract, error)
	AmendContract(ctx context.Context, oldContractID string, newContract *domain.EmploymentContract, amd *domain.ContractAmendment) error
	TerminateContract(ctx context.Context, contractID, terminationDate string) error
}

type Publisher interface {
	PublishContractIssued(ctx context.Context, correlationID string, c domain.EmploymentContract)
	PublishContractAmended(ctx context.Context, correlationID string, c domain.EmploymentContract, amd domain.ContractAmendment)
	PublishContractTerminated(ctx context.Context, correlationID string, c domain.EmploymentContract)
}

type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type EmployeeValidator interface {
	ValidateEmployee(ctx context.Context, tenantID, principalID, employeeID string) (*employee.EmployeeResponse, error)
}

const (
	actionContractCreate    = "CONTRACT_CREATE"
	actionContractView      = "CONTRACT_VIEW"
	actionContractAmend     = "CONTRACT_AMEND"
	actionContractTerminate = "CONTRACT_TERMINATE"
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
	r.Route("/v1/contracts", func(r chi.Router) {
		r.Post("/", h.IssueContract)
		r.Get("/", h.ListContracts)
		r.Get("/{id}", h.GetContract)
		r.Get("/employee/{employee_id}/active", h.GetActiveContractByEmployee)
		r.Post("/{id}/amend", h.AmendContract)
		r.Post("/{id}/terminate", h.TerminateContract)
	})
}

// ── POST /v1/contracts ────────────────────────────────────────────────────────────

func (h *Handler) IssueContract(w http.ResponseWriter, r *http.Request) {
	var req domain.IssueContractRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.LegalEntityID == "" || req.EmployeeID == "" || req.ContractType == "" || req.Title == "" || req.BaseSalaryAmount <= 0 || req.Currency == "" || req.PayFrequency == "" || req.EffectiveFrom == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, employee_id, contract_type, title, base_salary_amount, currency, pay_frequency, effective_from are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionContractCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if h.employee != nil {
		if _, err := h.employee.ValidateEmployee(r.Context(), tenantID, principalID, req.EmployeeID); err != nil {
			if errors.Is(err, domain.ErrEmployeeNotFound) {
				writeError(w, http.StatusBadRequest, "employee_invalid", err.Error())
				return
			}
			h.log.Warn("employee validation call failed, proceeding", zap.Error(err))
		}
	}

	contractNum := req.ContractNumber
	if contractNum == "" {
		contractNum = fmt.Sprintf("CTR-%s", uuid.NewString()[:8])
	}

	now := time.Now().UTC()
	contract := &domain.EmploymentContract{
		ContractID:       uuid.NewString(),
		TenantID:         tenantID,
		LegalEntityID:   req.LegalEntityID,
		EmployeeID:       req.EmployeeID,
		ContractNumber:   contractNum,
		Version:          1,
		ContractType:     req.ContractType,
		Status:           "ACTIVE",
		Title:            req.Title,
		BaseSalaryAmount: req.BaseSalaryAmount,
		Currency:         req.Currency,
		PayFrequency:     req.PayFrequency,
		EffectiveFrom:    req.EffectiveFrom,
		EffectiveTo:      req.EffectiveTo,
		DocumentVaultRef: req.DocumentVaultRef,
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	if err := h.store.IssueContract(r.Context(), contract); err != nil {
		h.log.Error("failed to issue contract", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	correlationID := getCorrelationID(r)
	h.publisher.PublishContractIssued(r.Context(), correlationID, *contract)

	writeJSON(w, http.StatusCreated, contract)
}

// ── GET /v1/contracts ─────────────────────────────────────────────────────────────

func (h *Handler) ListContracts(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	employeeID := r.URL.Query().Get("employee_id")
	status := r.URL.Query().Get("status")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if legalEntityID != "" {
		if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionContractView); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
	}

	list, err := h.store.ListContracts(r.Context(), legalEntityID, employeeID, status)
	if err != nil {
		h.log.Error("failed to list contracts", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if list == nil {
		list = []domain.EmploymentContract{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── GET /v1/contracts/{id} ────────────────────────────────────────────────────────

func (h *Handler) GetContract(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	c, err := h.store.GetContract(r.Context(), id)
	if errors.Is(err, domain.ErrContractNotFound) {
		writeError(w, http.StatusNotFound, "contract_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch contract", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, c.LegalEntityID, actionContractView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	writeJSON(w, http.StatusOK, c)
}

// ── GET /v1/contracts/employee/{employee_id}/active ───────────────────────────────

func (h *Handler) GetActiveContractByEmployee(w http.ResponseWriter, r *http.Request) {
	employeeID := chi.URLParam(r, "employee_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	c, err := h.store.GetActiveContractByEmployee(r.Context(), employeeID)
	if errors.Is(err, domain.ErrContractNotFound) {
		writeError(w, http.StatusNotFound, "active_contract_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch active contract", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, c.LegalEntityID, actionContractView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	writeJSON(w, http.StatusOK, c)
}

// ── POST /v1/contracts/{id}/amend ─────────────────────────────────────────────────

func (h *Handler) AmendContract(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req domain.AmendContractRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.AmendmentReason == "" || req.EffectiveFrom == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "amendment_reason, effective_from are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	oldContract, err := h.store.GetContract(r.Context(), id)
	if errors.Is(err, domain.ErrContractNotFound) {
		writeError(w, http.StatusNotFound, "contract_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch contract for amendment", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if oldContract.Status != "ACTIVE" {
		writeError(w, http.StatusConflict, "contract_not_active", "only ACTIVE contracts can be amended")
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, oldContract.LegalEntityID, actionContractAmend); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	title := oldContract.Title
	if req.Title != nil {
		title = *req.Title
	}

	salary := oldContract.BaseSalaryAmount
	if req.BaseSalaryAmount != nil {
		salary = *req.BaseSalaryAmount
	}

	currency := oldContract.Currency
	if req.Currency != nil {
		currency = *req.Currency
	}

	freq := oldContract.PayFrequency
	if req.PayFrequency != nil {
		freq = *req.PayFrequency
	}

	now := time.Now().UTC()
	newContract := &domain.EmploymentContract{
		ContractID:       uuid.NewString(),
		TenantID:         oldContract.TenantID,
		LegalEntityID:   oldContract.LegalEntityID,
		EmployeeID:       oldContract.EmployeeID,
		ContractNumber:   oldContract.ContractNumber,
		Version:          oldContract.Version + 1,
		ContractType:     oldContract.ContractType,
		Status:           "ACTIVE",
		Title:            title,
		BaseSalaryAmount: salary,
		Currency:         currency,
		PayFrequency:     freq,
		EffectiveFrom:    req.EffectiveFrom,
		DocumentVaultRef: oldContract.DocumentVaultRef,
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	amendment := &domain.ContractAmendment{
		AmendmentID:     uuid.NewString(),
		TenantID:        oldContract.TenantID,
		ContractID:      newContract.ContractID,
		FromVersion:     oldContract.Version,
		ToVersion:       newContract.Version,
		AmendmentReason: req.AmendmentReason,
		AmendedBy:       principalID,
		EffectiveFrom:   req.EffectiveFrom,
		CreatedAt:       now,
	}

	if err := h.store.AmendContract(r.Context(), id, newContract, amendment); err != nil {
		h.log.Error("failed to amend contract", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	correlationID := getCorrelationID(r)
	h.publisher.PublishContractAmended(r.Context(), correlationID, *newContract, *amendment)

	writeJSON(w, http.StatusOK, map[string]any{
		"contract":  newContract,
		"amendment": amendment,
	})
}

// ── POST /v1/contracts/{id}/terminate ─────────────────────────────────────────────

func (h *Handler) TerminateContract(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req domain.TerminateContractRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.TerminationDate == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "termination_date is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	oldContract, err := h.store.GetContract(r.Context(), id)
	if errors.Is(err, domain.ErrContractNotFound) {
		writeError(w, http.StatusNotFound, "contract_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch contract for termination", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, oldContract.LegalEntityID, actionContractTerminate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if err := h.store.TerminateContract(r.Context(), id, req.TerminationDate); err != nil {
		h.log.Error("failed to terminate contract", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	oldContract.Status = "TERMINATED"
	oldContract.EffectiveTo = &req.TerminationDate
	oldContract.UpdatedAt = time.Now().UTC()

	correlationID := getCorrelationID(r)
	h.publisher.PublishContractTerminated(r.Context(), correlationID, *oldContract)

	writeJSON(w, http.StatusOK, oldContract)
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