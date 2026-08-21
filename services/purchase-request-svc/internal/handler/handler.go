// Package handler exposes purchase-request-svc's REST API.
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

	"zoiko.io/purchase-request-svc/internal/domain"
	svcmiddleware "zoiko.io/purchase-request-svc/internal/middleware"
)

// Store is the persistence contract the handler depends on.
type Store interface {
	CreateRequest(ctx context.Context, r *domain.PurchaseRequest) (created bool, err error)
	GetRequest(ctx context.Context, requestID string) (*domain.PurchaseRequest, error)
	ListRequests(ctx context.Context, filter domain.ListRequestsFilter) ([]domain.PurchaseRequest, error)
	TransitionRequest(ctx context.Context, tenantID, requestID string, toStatus domain.RequestStatus, actorPrincipalID string, actedAt time.Time, rejectionReason *string) error
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
	if !decodeJSON(w, r, &req) {
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

	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// tenant_id in the body is accepted only when it agrees with the verified
	// scope. It used to be the ONLY source of the stored tenant_id — and the
	// store handed that same body value to set_config('app.tenant_id'), so the
	// tenant the request named satisfied the RLS policy on the way past. A body
	// naming another tenant filed the request in that tenant's register.
	if req.TenantID != "" && req.TenantID != tenantID {
		writeError(w, http.StatusForbidden, "tenant_scope_mismatch", domain.ErrTenantScopeMismatch.Error())
		return
	}
	// legal_entity_id reaches a uuid column on insert; a malformed one died
	// inside the driver as 22P02 and surfaced as 503 store_unavailable.
	if !isUUID(req.LegalEntityID) {
		writeError(w, http.StatusBadRequest, "invalid_field", "legal_entity_id must be a UUID")
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
		TenantID:               tenantID,
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

// ListRequests returns the caller's own tenant's register.
//
// The scope comes from the verified X-Tenant-Id header. It used to come from
// ?tenant_id=, which the store both filtered on AND set app.tenant_id from — so
// the tenant the caller named satisfied the RLS policy on the way past, and
// `?tenant_id=<any-uuid>` returned that tenant's entire purchase-request
// register. Nothing consulted X-Tenant-Id at all.
//
// Reads are scoped rather than authorized, matching the services either side of
// this one in the commercial-ops chain (purchase-order-svc, accounts-payable-svc):
// there is deliberately no PR_REQUEST_VIEW action.
func (h *Handler) ListRequests(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	if claimed := q.Get("tenant_id"); claimed != "" && claimed != tenantID {
		writeError(w, http.StatusForbidden, "tenant_scope_mismatch", domain.ErrTenantScopeMismatch.Error())
		return
	}
	if status := q.Get("status"); status != "" && !domain.ValidRequestStatus(status) {
		writeError(w, http.StatusBadRequest, "invalid_field", "status is not a recognised purchase request status")
		return
	}
	// legal_entity_id is compared as `legal_entity_id::text = $n`, casting the
	// COLUMN to text rather than the parameter to uuid — so a malformed value does
	// not error, it silently matches nothing, and an empty result reads as "this
	// entity has raised no requests". Refused here instead.
	legalEntityID := q.Get("legal_entity_id")
	if legalEntityID != "" && !isUUID(legalEntityID) {
		writeError(w, http.StatusBadRequest, "invalid_field", "legal_entity_id must be a UUID")
		return
	}
	filter := domain.ListRequestsFilter{
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		Status:        q.Get("status"),
	}
	list, err := h.store.ListRequests(r.Context(), filter)
	if err != nil {
		// legal_entity_id is compared as text, so the verified tenant is the only
		// uuid comparison left here. A gateway that forwarded a non-UUID tenant
		// scope is a fault worth naming rather than reporting as a dead store.
		if errors.Is(err, domain.ErrInvalidIdentifier) {
			writeError(w, http.StatusBadRequest, "invalid_field", "tenant scope must be a UUID")
			return
		}
		h.log.Error("ListRequests: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if list == nil {
		list = []domain.PurchaseRequest{}
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

	if pr.RequestedByPrincipalID == principalID {
		writeError(w, http.StatusForbidden, "self_approval_not_allowed", domain.ErrSelfApprovalNotAllowed.Error())
		return
	}

	now := time.Now().UTC()
	if err := h.store.TransitionRequest(r.Context(), pr.TenantID, requestID, domain.RequestStatusApproved, principalID, now, nil); err != nil {
		h.handleTransitionErr(w, err)
		return
	}

	pr.Status = domain.RequestStatusApproved
	pr.ApprovedByPrincipalID = &principalID
	pr.ApprovedAt = &now
	h.publisher.PublishRequestApproved(r.Context(), *pr)
	writeJSON(w, http.StatusOK, pr)
}

// ── POST /v1/purchase-requests/{request_id}/reject ───────────────────────────
//
// PENDING -> REJECTED. Terminal. Requires a reason — a rejection without a
// stated reason isn't useful evidence.
func (h *Handler) RejectRequest(w http.ResponseWriter, r *http.Request) {
	var req domain.RejectRequestRequest
	if !decodeJSON(w, r, &req) {
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

	if pr.RequestedByPrincipalID == principalID {
		writeError(w, http.StatusForbidden, "self_approval_not_allowed", domain.ErrSelfApprovalNotAllowed.Error())
		return
	}

	now := time.Now().UTC()
	if err := h.store.TransitionRequest(r.Context(), pr.TenantID, requestID, domain.RequestStatusRejected, principalID, now, &req.Reason); err != nil {
		h.handleTransitionErr(w, err)
		return
	}

	pr.Status = domain.RequestStatusRejected
	pr.RejectedByPrincipalID = &principalID
	pr.RejectedAt = &now
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

// requiredFieldMissing deliberately does NOT require tenant_id: the tenant is
// the caller's verified scope, so a body omitting it is fine and a body
// disagreeing with it is a 403, not a missing field.
func requiredFieldMissing(req domain.CreateRequestRequest) string {
	switch {
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
