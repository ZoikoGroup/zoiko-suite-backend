package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/contract-lifecycle-svc/internal/authz"
	"zoiko.io/contract-lifecycle-svc/internal/domain"
	"zoiko.io/contract-lifecycle-svc/internal/events"
	"zoiko.io/contract-lifecycle-svc/internal/middleware"
	"zoiko.io/contract-lifecycle-svc/internal/store"
)

// Handler holds all dependencies for the HTTP layer.
type Handler struct {
	store     store.Store
	publisher events.Publisher
	authz     *authz.Client
	logger    *zap.Logger
}

// New creates a new Handler.
func New(st store.Store, pub events.Publisher, az *authz.Client, logger *zap.Logger) *Handler {
	return &Handler{store: st, publisher: pub, authz: az, logger: logger}
}

// RegisterRoutes mounts all contract lifecycle routes onto the given router.
func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/contracts", func(r chi.Router) {
		r.Post("/", h.CreateContract)
		r.Get("/", h.ListContracts)
		r.Get("/{id}", h.GetContract)
		r.Put("/{id}", h.UpdateContract)
		r.Post("/{id}/submit", h.SubmitForApproval)
		r.Post("/{id}/activate", h.ActivateContract)
		r.Post("/{id}/terminate", h.TerminateContract)
		r.Get("/{id}/versions", h.ListContractVersions)
	})
}

// --- Handlers ---

func (h *Handler) CreateContract(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.CreateContractRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Title == "" || req.CounterpartyID == "" || req.EffectiveFrom == "" {
		writeError(w, http.StatusBadRequest, "title, counterparty_id, and effective_from are required")
		return
	}

	c := &domain.Contract{
		TenantID:         tenantID,
		LegalEntityID:    req.LegalEntityID,
		ContractType:     req.ContractType,
		Title:            req.Title,
		Description:      req.Description,
		CounterpartyID:   req.CounterpartyID,
		CounterpartyName: req.CounterpartyName,
		EffectiveFrom:    req.EffectiveFrom,
		EffectiveTo:      req.EffectiveTo,
		Currency:         req.Currency,
		TotalValue:       req.TotalValue,
		CreatedBy:        req.CreatedBy,
	}

	if err := h.store.CreateContract(r.Context(), c); err != nil {
		h.logger.Error("create contract failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create contract")
		return
	}

	_ = h.publisher.Publish(r.Context(), "contract.created", c.ContractID, tenantID, c)
	writeJSON(w, http.StatusCreated, c)
}

func (h *Handler) GetContract(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	c, err := h.store.GetContract(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrContractNotFound) {
			writeError(w, http.StatusNotFound, "contract not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get contract")
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (h *Handler) ListContracts(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	contracts, err := h.store.ListContracts(r.Context(), legalEntityID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list contracts")
		return
	}
	if contracts == nil {
		contracts = []domain.Contract{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"contracts": contracts, "total": len(contracts)})
}

func (h *Handler) UpdateContract(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	existing, err := h.store.GetContract(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrContractNotFound) {
			writeError(w, http.StatusNotFound, "contract not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch contract")
		return
	}
	if existing.Status != domain.ContractStatusDraft && existing.Status != domain.ContractStatusPendingApproval {
		writeError(w, http.StatusConflict, "only DRAFT or PENDING_APPROVAL contracts can be updated")
		return
	}

	var req domain.UpdateContractRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Title != "" {
		existing.Title = req.Title
	}
	if req.Description != "" {
		existing.Description = req.Description
	}
	if req.CounterpartyName != "" {
		existing.CounterpartyName = req.CounterpartyName
	}
	if req.EffectiveTo != nil {
		existing.EffectiveTo = req.EffectiveTo
	}
	if req.Currency != "" {
		existing.Currency = req.Currency
	}
	if req.TotalValue > 0 {
		existing.TotalValue = req.TotalValue
	}

	if err := h.store.UpdateContract(r.Context(), existing, req.ChangeSummary); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update contract")
		return
	}

	_ = h.publisher.Publish(r.Context(), "contract.updated", id, tenantID, existing)
	writeJSON(w, http.StatusOK, existing)
}

func (h *Handler) SubmitForApproval(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	existing, err := h.store.GetContract(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrContractNotFound) {
			writeError(w, http.StatusNotFound, "contract not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch contract")
		return
	}
	if existing.Status != domain.ContractStatusDraft {
		writeError(w, http.StatusConflict, "only DRAFT contracts can be submitted for approval")
		return
	}

	if err := h.store.UpdateContractStatus(r.Context(), id, domain.ContractStatusPendingApproval, ""); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to submit contract")
		return
	}
	existing.Status = domain.ContractStatusPendingApproval
	_ = h.publisher.Publish(r.Context(), "contract.submitted_for_approval", id, tenantID, existing)
	writeJSON(w, http.StatusOK, existing)
}

func (h *Handler) ActivateContract(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.ActivateContractRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.SignedBy == "" {
		writeError(w, http.StatusBadRequest, "signed_by is required")
		return
	}

	c, err := h.store.ActivateContract(r.Context(), id, &req)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrContractNotFound):
			writeError(w, http.StatusNotFound, "contract not found")
		case errors.Is(err, domain.ErrContractAlreadyActive):
			writeError(w, http.StatusConflict, "contract is already active")
		case errors.Is(err, domain.ErrContractTerminated):
			writeError(w, http.StatusConflict, "contract is terminated")
		default:
			writeError(w, http.StatusInternalServerError, "failed to activate contract")
		}
		return
	}

	_ = h.publisher.Publish(r.Context(), "contract.activated", id, tenantID, c)
	writeJSON(w, http.StatusOK, c)
}

func (h *Handler) TerminateContract(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.TerminateContractRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.TerminatedBy == "" {
		writeError(w, http.StatusBadRequest, "terminated_by is required")
		return
	}

	c, err := h.store.TerminateContract(r.Context(), id, &req)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrContractNotFound):
			writeError(w, http.StatusNotFound, "contract not found")
		case errors.Is(err, domain.ErrContractTerminated):
			writeError(w, http.StatusConflict, "contract is already terminated")
		default:
			writeError(w, http.StatusInternalServerError, "failed to terminate contract")
		}
		return
	}

	_ = h.publisher.Publish(r.Context(), "contract.terminated", id, tenantID, c)
	writeJSON(w, http.StatusOK, c)
}

func (h *Handler) ListContractVersions(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	versions, err := h.store.ListContractVersions(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list contract versions")
		return
	}
	if versions == nil {
		versions = []domain.ContractVersion{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"versions": versions, "total": len(versions)})
}

// --- Helpers ---

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
