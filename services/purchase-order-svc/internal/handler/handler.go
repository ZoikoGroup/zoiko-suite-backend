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
	svcmiddleware "zoiko.io/purchase-order-svc/internal/middleware"
	"zoiko.io/purchase-order-svc/internal/purchaserequest"
)

// Store is the persistence contract the handler depends on.
type Store interface {
	CreateOrder(ctx context.Context, o *domain.PurchaseOrder) (created bool, err error)
	GetOrder(ctx context.Context, orderID string) (*domain.PurchaseOrder, error)
	ListOrders(ctx context.Context, filter domain.ListOrdersFilter) ([]domain.PurchaseOrder, error)
	ListAmendments(ctx context.Context, orderID string) ([]domain.PurchaseOrderAmendment, error)
	AmendOrder(ctx context.Context, tenantID, orderID string, newTotalAmount float64, reason, actorPrincipalID string) (*domain.PurchaseOrder, error)
	CloseOrder(ctx context.Context, tenantID, orderID, actorPrincipalID string) (*domain.PurchaseOrder, error)
}

// Publisher is the event-publishing contract the handler depends on.
type Publisher interface {
	PublishOrderIssued(ctx context.Context, o domain.PurchaseOrder)
	PublishOrderAmended(ctx context.Context, actorID string, o domain.PurchaseOrder)
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
	store     Store
	publisher Publisher
	authz     AuthZClient
	prClient  PurchaseRequestClient
	log       *zap.Logger
}

func New(store Store, publisher Publisher, authz AuthZClient, prClient PurchaseRequestClient, log *zap.Logger) *Handler {
	return &Handler{store: store, publisher: publisher, authz: authz, prClient: prClient, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/purchase-orders", func(r chi.Router) {
		r.Post("/", h.IssueOrder)
		r.Get("/", h.ListOrders)
		r.Get("/{purchase_order_id}", h.GetOrder)
		r.Get("/{purchase_order_id}/amendments", h.ListAmendments)
		r.Post("/{purchase_order_id}/amend", h.AmendOrder)
		r.Post("/{purchase_order_id}/close", h.CloseOrder)
	})
}

// ── POST /v1/purchase-orders ─────────────────────────────────────────────────

func (h *Handler) IssueOrder(w http.ResponseWriter, r *http.Request) {
	var req domain.IssueOrderRequest
	if !decodeJSON(w, r, &req) {
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

	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// tenant_id in the body is accepted only when it agrees with the verified
	// scope. It used to be the ONLY source of the stored tenant_id — and the
	// store handed that same body value to set_config('app.tenant_id'), so the
	// tenant the request named satisfied the RLS policy on the way past. A body
	// naming another tenant issued the order into that tenant's register, and
	// the same value was what the purchase-request lookup below was scoped to,
	// so the referenced request was checked in the attacker's chosen tenant too.
	if req.TenantID != "" && req.TenantID != tenantID {
		writeError(w, http.StatusForbidden, "tenant_scope_mismatch", domain.ErrTenantScopeMismatch.Error())
		return
	}
	if !isUUID(req.LegalEntityID) {
		writeError(w, http.StatusBadRequest, "invalid_field", "legal_entity_id must be a UUID")
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
		if _, err := h.prClient.GetApprovedRequest(r.Context(), tenantID, req.LegalEntityID, *req.PurchaseRequestID); err != nil {
			h.writePurchaseRequestErr(w, err)
			return
		}
	}

	po := &domain.PurchaseOrder{
		PurchaseOrderID:     uuid.NewString(),
		TenantID:            tenantID,
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
		h.writeStoreErr(w, "IssueOrder", err)
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
		h.writeStoreErr(w, "GetOrder", err)
		return
	}
	if po == nil {
		writeError(w, http.StatusNotFound, "order_not_found", "")
		return
	}
	writeJSON(w, http.StatusOK, po)
}

// ── GET /v1/purchase-orders ───────────────────────────────────────────────────

// ListOrders returns the caller's own tenant's register.
//
// The scope comes from the verified X-Tenant-Id header. It used to come from
// ?tenant_id=, which the store both filtered on AND set app.tenant_id from — so
// the tenant the caller named satisfied the RLS policy on the way past and
// `?tenant_id=<any-uuid>` returned that tenant's entire purchase-order register.
// Nothing consulted X-Tenant-Id at all.
//
// Reads are scoped rather than authorized, matching the rest of the
// commercial-ops chain: there is deliberately no PO_ORDER_VIEW action.
func (h *Handler) ListOrders(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	if claimed := q.Get("tenant_id"); claimed != "" && claimed != tenantID {
		writeError(w, http.StatusForbidden, "tenant_scope_mismatch", domain.ErrTenantScopeMismatch.Error())
		return
	}
	if status := q.Get("status"); status != "" && !domain.ValidOrderStatus(status) {
		writeError(w, http.StatusBadRequest, "invalid_field", "status is not a recognised purchase order status")
		return
	}
	// legal_entity_id is compared as `legal_entity_id::text = $n`, casting the
	// COLUMN to text rather than the parameter to uuid — so a malformed value does
	// not error, it silently matches nothing, and an empty result reads as "this
	// entity has no orders". Refused here instead.
	legalEntityID := q.Get("legal_entity_id")
	if legalEntityID != "" && !isUUID(legalEntityID) {
		writeError(w, http.StatusBadRequest, "invalid_field", "legal_entity_id must be a UUID")
		return
	}
	filter := domain.ListOrdersFilter{
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		Status:        q.Get("status"),
	}
	list, err := h.store.ListOrders(r.Context(), filter)
	if err != nil {
		// legal_entity_id is compared as text, so the verified tenant is the only
		// uuid comparison left here. A gateway that forwarded a non-UUID tenant
		// scope is a fault worth naming rather than reporting as a dead store.
		if errors.Is(err, domain.ErrInvalidIdentifier) {
			writeError(w, http.StatusBadRequest, "invalid_field", "tenant scope must be a UUID")
			return
		}
		h.log.Error("ListOrders: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if list == nil {
		list = []domain.PurchaseOrder{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── GET /v1/purchase-orders/{purchase_order_id}/amendments ───────────────────
//
// The append-only amendment ledger for one order, oldest first. AmendOrder has
// always written these rows — with the before/after totals and the operator's
// stated reason — and nothing could read them back, so the order's `version`
// counter was the only evidence an amendment ever happened.
//
// The order is resolved first so an unknown id is a 404 rather than an empty
// list: "this order has no amendments" and "there is no such order" are
// different facts, and an empty array would report the second as the first.
func (h *Handler) ListAmendments(w http.ResponseWriter, r *http.Request) {
	orderID := chi.URLParam(r, "purchase_order_id")

	po, err := h.store.GetOrder(r.Context(), orderID)
	if err != nil {
		h.writeStoreErr(w, "ListAmendments", err)
		return
	}
	if po == nil {
		writeError(w, http.StatusNotFound, "order_not_found", "")
		return
	}

	list, err := h.store.ListAmendments(r.Context(), orderID)
	if err != nil {
		h.writeStoreErr(w, "ListAmendments", err)
		return
	}
	if list == nil {
		list = []domain.PurchaseOrderAmendment{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── POST /v1/purchase-orders/{purchase_order_id}/amend ───────────────────────
//
// Updates total_amount, bumps version, records an append-only amendment row.
// Only legal while ISSUED — does not change po_status.
func (h *Handler) AmendOrder(w http.ResponseWriter, r *http.Request) {
	var req domain.AmendOrderRequest
	if !decodeJSON(w, r, &req) {
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
		h.writeStoreErr(w, "AmendOrder", err)
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

	h.publisher.PublishOrderAmended(r.Context(), principalID, *updated)
	writeJSON(w, http.StatusOK, updated)
}

// ── POST /v1/purchase-orders/{purchase_order_id}/close ────────────────────────
//
// ISSUED -> CLOSED. Terminal.
func (h *Handler) CloseOrder(w http.ResponseWriter, r *http.Request) {
	orderID := chi.URLParam(r, "purchase_order_id")
	po, err := h.store.GetOrder(r.Context(), orderID)
	if err != nil {
		h.writeStoreErr(w, "CloseOrder", err)
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

// writeStoreErr maps store errors to HTTP responses. A malformed identifier
// (e.g. a non-UUID in a path or body field) is a caller error, not an
// infrastructure failure — it must never surface as 503.
func (h *Handler) writeStoreErr(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, domain.ErrInvalidIdentifier):
		writeError(w, http.StatusBadRequest, "invalid_identifier", "")
	default:
		h.log.Error(op+": store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
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

// requiredFieldMissing deliberately does NOT require tenant_id: the tenant is
// the caller's verified scope, so a body omitting it is fine and a body
// disagreeing with it is a 403, not a missing field.
func requiredFieldMissing(req domain.IssueOrderRequest) string {
	switch {
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

// requireTenant reads the caller's verified tenant scope from context (set by
// middleware.TenantContext from X-Tenant-Id). A request with no scope is
// refused — it must never fall back to a tenant the request itself named.
func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		writeError(w, http.StatusUnauthorized, "tenant_scope_missing", domain.ErrTenantScopeMissing.Error())
		return "", false
	}
	return tenantID, true
}

func isUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
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

// maxRequestBytes caps a JSON request body. A bare json.Decoder reads until EOF,
// so without this a single request can make the service allocate whatever the
// client is willing to send -- no auth needed, and nothing in the metrics to
// distinguish it from load.
const maxRequestBytes = 256 << 10 // 256 KiB

// decodeJSON reads a size-capped JSON body, answering 413 rather than 400 when
// the cap is what stopped it: "too large" and "malformed" are different faults
// and a caller can only act on the difference.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	return true
}
