// Package handler exposes purchase-request-svc's REST API.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/purchase-request-svc/internal/domain"
)

// Store is the persistence contract the handler depends on.
type Store interface {
	CreateRequest(ctx context.Context, r *domain.PurchaseRequest) (created bool, err error)
	GetRequest(ctx context.Context, requestID string) (*domain.PurchaseRequest, error)
	ListRequests(ctx context.Context, filter domain.ListRequestsFilter) ([]domain.PurchaseRequest, error)
	TransitionRequest(ctx context.Context, tenantID, requestID string, toStatus domain.RequestStatus, actorPrincipalID string, rejectionReason *string) error
}

// Publisher is the event-publishing contract the handler depends on.
type Publisher interface {
	PublishRequestCreated(ctx context.Context, r domain.PurchaseRequest)
	PublishRequestApproved(ctx context.Context, r domain.PurchaseRequest)
	PublishRequestRejected(ctx context.Context, r domain.PurchaseRequest)
}

// AuthZClient is the authorization contract the handler depends on.
type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// Action types checked against authorization-svc. A single, platform-wide
// action type per lifecycle stage — nothing in the docs specifies
// finer-grained codes for v1.
const (
	actionCreateRequest  = "PR_REQUEST_CREATE"
	actionApproveRequest = "PR_REQUEST_APPROVE"
	actionRejectRequest  = "PR_REQUEST_REJECT"
)

type Handler struct {
	store     Store
	publisher Publisher
	authz     AuthZClient
	log       *zap.Logger
}

func New(store Store, publisher Publisher, authz AuthZClient, log *zap.Logger) *Handler {
	return &Handler{store: store, publisher: publisher, authz: authz, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/purchase-requests", func(r chi.Router) {
		r.Post("/", h.CreateRequest)
		r.Get("/", h.ListRequests)
		r.Get("/{request_id}", h.GetRequest)
		r.Post("/{request_id}/approve", h.ApproveRequest)
		r.Post("/{request_id}/reject", h.RejectRequest)
	})
}

// ── POST /v1/purchase-requests ───────────────────────────────────────────────

func (h *Handler) CreateRequest(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateRequestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if missing := requiredFieldMissing(req); missing != "" {
		writeError(w, http.StatusBadRequest, "missing_field", missing)
		return
	}
	if req.Amount <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_field", "amount must be greater than zero")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionCreateRequest); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	pr := &domain.PurchaseRequest{
		RequestID:              uuid.NewString(),
		TenantID:               req.TenantID,
		LegalEntityID:          req.LegalEntityID,
		RequestedByPrincipalID: principalID,
		Description:            req.Description,
		Amount:                 req.Amount,
		CurrencyCode:           req.CurrencyCode,
		Status:                 domain.RequestStatusPending,
		CorrelationID:          req.CorrelationID,
	}
	created, err := h.store.CreateRequest(r.Context(), pr)
	if err != nil {
		h.log.Error("CreateRequest: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if !created {
		// Replay of a prior request with the same correlation_id — return
		// the original request, do not re-publish the created event.
		writeJSON(w, http.StatusOK, pr)
		return
	}

	h.publisher.PublishRequestCreated(r.Context(), *pr)
	writeJSON(w, http.StatusCreated, pr)
}

// ── GET /v1/purchase-requests/{request_id} ───────────────────────────────────

func (h *Handler) GetRequest(w http.ResponseWriter, r *http.Request) {
	requestID := chi.URLParam(r, "request_id")
	pr, err := h.store.GetRequest(r.Context(), requestID)
	if err != nil {
		h.log.Error("GetRequest: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if pr == nil {
		writeError(w, http.StatusNotFound, "request_not_found", "")
		return
	}
	writeJSON(w, http.StatusOK, pr)
}

// ── GET /v1/purchase-requests ─────────────────────────────────────────────────

func (h *Handler) ListRequests(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := domain.ListRequestsFilter{
		TenantID:      q.Get("tenant_id"),
		LegalEntityID: q.Get("legal_entity_id"),
		Status:        q.Get("status"),
	}
	if filter.TenantID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "tenant_id")
		return
	}
	list, err := h.store.ListRequests(r.Context(), filter)
	if err != nil {
		h.log.Error("ListRequests: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// ── POST /v1/purchase-requests/{request_id}/approve ──────────────────────────
//
// PENDING -> APPROVED. Terminal.
func (h *Handler) ApproveRequest(w http.ResponseWriter, r *http.Request) {
	requestID := chi.URLParam(r, "request_id")
	pr, err := h.store.GetRequest(r.Context(), requestID)
	if err != nil {
		h.log.Error("ApproveRequest: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if pr == nil {
		writeError(w, http.StatusNotFound, "request_not_found", "")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, pr.LegalEntityID, actionApproveRequest); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if err := h.store.TransitionRequest(r.Context(), pr.TenantID, requestID, domain.RequestStatusApproved, principalID, nil); err != nil {
		h.handleTransitionErr(w, err)
		return
	}

	pr.Status = domain.RequestStatusApproved
	h.publisher.PublishRequestApproved(r.Context(), *pr)
	writeJSON(w, http.StatusOK, pr)
}

// ── POST /v1/purchase-requests/{request_id}/reject ───────────────────────────
//
// PENDING -> REJECTED. Terminal. Requires a reason — a rejection without a
// stated reason isn't useful evidence.
func (h *Handler) RejectRequest(w http.ResponseWriter, r *http.Request) {
	var req domain.RejectRequestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "reason")
		return
	}

	requestID := chi.URLParam(r, "request_id")
	pr, err := h.store.GetRequest(r.Context(), requestID)
	if err != nil {
		h.log.Error("RejectRequest: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if pr == nil {
		writeError(w, http.StatusNotFound, "request_not_found", "")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, pr.LegalEntityID, actionRejectRequest); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if err := h.store.TransitionRequest(r.Context(), pr.TenantID, requestID, domain.RequestStatusRejected, principalID, &req.Reason); err != nil {
		h.handleTransitionErr(w, err)
		return
	}

	pr.Status = domain.RequestStatusRejected
	pr.RejectionReason = &req.Reason
	h.publisher.PublishRequestRejected(r.Context(), *pr)
	writeJSON(w, http.StatusOK, pr)
}

// ── helpers ──────────────────────────────────────────────────────────────────

func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrAuthorizationDenied):
		writeError(w, http.StatusForbidden, "authorization_denied", "")
	default:
		h.log.Error("authorization check failed — failing closed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "authorization_service_unavailable", "")
	}
}

func (h *Handler) handleTransitionErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrInvalidTransition):
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", domain.ErrInvalidTransition.Error())
	default:
		h.log.Error("TransitionRequest: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
	}
}

func requiredFieldMissing(req domain.CreateRequestRequest) string {
	switch {
	case req.TenantID == "":
		return "tenant_id"
	case req.LegalEntityID == "":
		return "legal_entity_id"
	case req.Description == "":
		return "description"
	case req.CurrencyCode == "":
		return "currency_code"
	case req.CorrelationID == "":
		return "correlation_id"
	default:
		return ""
	}
}

// requirePrincipal reads the caller's identity from X-Principal-Id — set by
// gateway-auth-svc's ForwardAuth verification after checking the signed
// IdentityContextEnvelope JWT. This service never decodes a JWT itself,
// matching every other Phase 3 service's pattern. A request with no
// resolved principal never passed identity verification — fail closed with
// 401.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", domain.ErrIdentityMissing.Error())
		return "", false
	}
	return principalID, true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

type errorResponse struct {
	Error  string `json:"error"`
	Detail string `json:"detail,omitempty"`
}

func writeError(w http.ResponseWriter, status int, code, detail string) {
	writeJSON(w, status, errorResponse{Error: code, Detail: detail})
}
