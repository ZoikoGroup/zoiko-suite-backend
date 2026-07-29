// Package handler exposes purchase-order-svc's REST API.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/purchase-order-svc/internal/domain"
	"zoiko.io/purchase-order-svc/internal/purchaserequest"
)

// Store is the persistence contract the handler depends on.
type Store interface {
	CreateOrder(ctx context.Context, o *domain.PurchaseOrder) (created bool, err error)
	GetOrder(ctx context.Context, orderID string) (*domain.PurchaseOrder, error)
	ListOrders(ctx context.Context, filter domain.ListOrdersFilter) ([]domain.PurchaseOrder, error)
	AmendOrder(ctx context.Context, tenantID, orderID string, newTotalAmount float64, reason, actorPrincipalID string) (*domain.PurchaseOrder, error)
	CloseOrder(ctx context.Context, tenantID, orderID, actorPrincipalID string) (*domain.PurchaseOrder, error)
}

// Publisher is the event-publishing contract the handler depends on.
type Publisher interface {
	PublishOrderIssued(ctx context.Context, o domain.PurchaseOrder)
	PublishOrderAmended(ctx context.Context, o domain.PurchaseOrder)
	PublishOrderClosed(ctx context.Context, o domain.PurchaseOrder)
}

// AuthZClient is the authorization contract the handler depends on.
type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// PurchaseRequestClient is the cross-service verification contract the
// handler depends on — see internal/purchaserequest's package doc for why
// this exists.
type PurchaseRequestClient interface {
	GetApprovedRequest(ctx context.Context, tenantID, legalEntityID, requestID string) (*purchaserequest.Summary, error)
}

// Action types checked against authorization-svc. A single, platform-wide
// action type per lifecycle stage — nothing in the docs specifies
// finer-grained codes for v1.
const (
	actionIssueOrder = "PO_ISSUE"
	actionAmendOrder = "PO_AMEND"
	actionCloseOrder = "PO_CLOSE"
)

type Handler struct {
	store   Store
	publisher Publisher
	authz   AuthZClient
	prClient PurchaseRequestClient
	log     *zap.Logger
}

func New(store Store, publisher Publisher, authz AuthZClient, prClient PurchaseRequestClient, log *zap.Logger) *Handler {
	return &Handler{store: store, publisher: publisher, authz: authz, prClient: prClient, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/purchase-orders", func(r chi.Router) {
		r.Post("/", h.IssueOrder)
		r.Get("/", h.ListOrders)
		r.Get("/{purchase_order_id}", h.GetOrder)
		r.Post("/{purchase_order_id}/amend", h.AmendOrder)
		r.Post("/{purchase_order_id}/close", h.CloseOrder)
	})
}

// ── POST /v1/purchase-orders ─────────────────────────────────────────────────

func (h *Handler) IssueOrder(w http.ResponseWriter, r *http.Request) {
	var req domain.IssueOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if missing := requiredFieldMissing(req); missing != "" {
		writeError(w, http.StatusBadRequest, "missing_field", missing)
		return
	}
	if req.TotalAmount <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_field", "total_amount must be greater than zero")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionIssueOrder); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if req.PurchaseRequestID != nil && *req.PurchaseRequestID != "" {
		if _, err := h.prClient.GetApprovedRequest(r.Context(), req.TenantID, req.LegalEntityID, *req.PurchaseRequestID); err != nil {
			h.writePurchaseRequestErr(w, err)
			return
		}
	}

	po := &domain.PurchaseOrder{
		PurchaseOrderID:     uuid.NewString(),
		TenantID:            req.TenantID,
		LegalEntityID:       req.LegalEntityID,
		PurchaseRequestID:   req.PurchaseRequestID,
		VendorProfileID:     req.VendorProfileID,
		TotalAmount:         req.TotalAmount,
		CurrencyCode:        req.CurrencyCode,
		IssuedByPrincipalID: principalID,
		CorrelationID:       req.CorrelationID,
	}
	created, err := h.store.CreateOrder(r.Context(), po)
	if err != nil {
		h.log.Error("IssueOrder: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
		h.publisher.PublishOrderIssued(r.Context(), *po)
	}
	writeJSON(w, status, po)
}

// ── GET /v1/purchase-orders/{purchase_order_id} ──────────────────────────────

func (h *Handler) GetOrder(w http.ResponseWriter, r *http.Request) {
	orderID := chi.URLParam(r, "purchase_order_id")
	po, err := h.store.GetOrder(r.Context(), orderID)
	if err != nil {
		h.log.Error("GetOrder: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if po == nil {
		writeError(w, http.StatusNotFound, "order_not_found", "")
		return
	}
	writeJSON(w, http.StatusOK, po)
}

// ── GET /v1/purchase-orders ───────────────────────────────────────────────────

func (h *Handler) ListOrders(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := domain.ListOrdersFilter{
		TenantID:      q.Get("tenant_id"),
		LegalEntityID: q.Get("legal_entity_id"),
		Status:        q.Get("status"),
	}
	if filter.TenantID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "tenant_id")
		return
	}
	list, err := h.store.ListOrders(r.Context(), filter)
	if err != nil {
		h.log.Error("ListOrders: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// ── POST /v1/purchase-orders/{purchase_order_id}/amend ───────────────────────
//
// Updates total_amount, bumps version, records an append-only amendment row.
// Only legal while ISSUED — does not change po_status.
func (h *Handler) AmendOrder(w http.ResponseWriter, r *http.Request) {
	var req domain.AmendOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "reason")
		return
	}
	if req.NewTotalAmount <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_field", "new_total_amount must be greater than zero")
		return
	}

	orderID := chi.URLParam(r, "purchase_order_id")
	po, err := h.store.GetOrder(r.Context(), orderID)
	if err != nil {
		h.log.Error("AmendOrder: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if po == nil {
		writeError(w, http.StatusNotFound, "order_not_found", "")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, po.LegalEntityID, actionAmendOrder); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	updated, err := h.store.AmendOrder(r.Context(), po.TenantID, orderID, req.NewTotalAmount, req.Reason, principalID)
	if err != nil {
		h.handleTransitionErr(w, err)
		return
	}

	h.publisher.PublishOrderAmended(r.Context(), *updated)
	writeJSON(w, http.StatusOK, updated)
}

// ── POST /v1/purchase-orders/{purchase_order_id}/close ────────────────────────
//
// ISSUED -> CLOSED. Terminal.
func (h *Handler) CloseOrder(w http.ResponseWriter, r *http.Request) {
	orderID := chi.URLParam(r, "purchase_order_id")
	po, err := h.store.GetOrder(r.Context(), orderID)
	if err != nil {
		h.log.Error("CloseOrder: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if po == nil {
		writeError(w, http.StatusNotFound, "order_not_found", "")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, po.LegalEntityID, actionCloseOrder); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	updated, err := h.store.CloseOrder(r.Context(), po.TenantID, orderID, principalID)
	if err != nil {
		h.handleTransitionErr(w, err)
		return
	}

	h.publisher.PublishOrderClosed(r.Context(), *updated)
	writeJSON(w, http.StatusOK, updated)
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

func (h *Handler) writePurchaseRequestErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrPurchaseRequestNotFound):
		writeError(w, http.StatusUnprocessableEntity, "purchase_request_not_found", err.Error())
	case errors.Is(err, domain.ErrPurchaseRequestNotApproved):
		writeError(w, http.StatusUnprocessableEntity, "purchase_request_not_approved", err.Error())
	case errors.Is(err, domain.ErrPurchaseRequestMismatch):
		writeError(w, http.StatusUnprocessableEntity, "purchase_request_mismatch", err.Error())
	default:
		h.log.Error("purchase-request-svc verification failed — failing closed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "purchase_request_service_unavailable", "")
	}
}

func (h *Handler) handleTransitionErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrInvalidTransition):
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", domain.ErrInvalidTransition.Error())
	default:
		h.log.Error("store: transition failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
	}
}

func requiredFieldMissing(req domain.IssueOrderRequest) string {
	switch {
	case req.TenantID == "":
		return "tenant_id"
	case req.LegalEntityID == "":
		return "legal_entity_id"
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
