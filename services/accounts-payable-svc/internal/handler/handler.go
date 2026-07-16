// Package handler exposes accounts-payable-svc's REST API.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/accounts-payable-svc/internal/domain"
)

// Store is the persistence contract the handler depends on.
type Store interface {
	CreateInvoice(ctx context.Context, inv *domain.VendorInvoice) error
	GetInvoice(ctx context.Context, invoiceID string) (*domain.VendorInvoice, error)
	ListInvoices(ctx context.Context, filter domain.ListInvoicesFilter) ([]domain.VendorInvoice, error)
	TransitionInvoice(ctx context.Context, tenantID, invoiceID string, fromStatus, toStatus domain.InvoiceStatus, actorPrincipalID string) error
}

// Publisher is the event-publishing contract the handler depends on.
type Publisher interface {
	PublishVendorInvoiceReceived(ctx context.Context, inv domain.VendorInvoice)
	PublishVendorInvoiceValidated(ctx context.Context, inv domain.VendorInvoice)
	PublishVendorInvoiceApproved(ctx context.Context, inv domain.VendorInvoice)
	PublishPaymentRequested(ctx context.Context, inv domain.VendorInvoice)
}

// AuthZClient is the authorization contract the handler depends on.
type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// Action types checked against authorization-svc. A single, platform-wide
// action type per lifecycle stage — nothing in the docs specifies
// finer-grained codes for v1.
const (
	actionCreateInvoice  = "AP_INVOICE_CREATE"
	actionValidateInvoice = "AP_INVOICE_VALIDATE"
	actionApproveInvoice = "AP_INVOICE_APPROVE"
	actionRequestPayment = "AP_PAYMENT_REQUEST"
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
	r.Route("/v1/invoices", func(r chi.Router) {
		r.Post("/", h.CreateInvoice)
		r.Get("/", h.ListInvoices)
		r.Get("/{invoice_id}", h.GetInvoice)
		r.Post("/{invoice_id}/validate", h.ValidateInvoice)
		r.Post("/{invoice_id}/approve", h.ApproveInvoice)
		r.Post("/{invoice_id}/request-payment", h.RequestPayment)
	})
}

// ── POST /v1/invoices ────────────────────────────────────────────────────────

func (h *Handler) CreateInvoice(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateVendorInvoiceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if missing := requiredInvoiceFieldMissing(req); missing != "" {
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
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionCreateInvoice); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	inv := &domain.VendorInvoice{
		InvoiceID:            uuid.NewString(),
		TenantID:             req.TenantID,
		LegalEntityID:        req.LegalEntityID,
		VendorID:             req.VendorID,
		InvoiceNumber:        req.InvoiceNumber,
		Amount:               req.Amount,
		CurrencyCode:         req.CurrencyCode,
		DueDate:              req.DueDate,
		Status:               domain.InvoiceStatusReceived,
		CreatedByPrincipalID: principalID,
		CorrelationID:        req.CorrelationID,
	}

	if err := h.store.CreateInvoice(r.Context(), inv); err != nil {
		h.log.Error("CreateInvoice: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}

	h.publisher.PublishVendorInvoiceReceived(r.Context(), *inv)
	writeJSON(w, http.StatusCreated, inv)
}

// ── GET /v1/invoices/{invoice_id} ────────────────────────────────────────────

func (h *Handler) GetInvoice(w http.ResponseWriter, r *http.Request) {
	invoiceID := chi.URLParam(r, "invoice_id")
	inv, err := h.store.GetInvoice(r.Context(), invoiceID)
	if err != nil {
		h.log.Error("GetInvoice: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if inv == nil {
		writeError(w, http.StatusNotFound, "invoice_not_found", "")
		return
	}
	writeJSON(w, http.StatusOK, inv)
}

// ── GET /v1/invoices ──────────────────────────────────────────────────────────

func (h *Handler) ListInvoices(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := domain.ListInvoicesFilter{
		TenantID:      q.Get("tenant_id"),
		LegalEntityID: q.Get("legal_entity_id"),
		VendorID:      q.Get("vendor_id"),
		Status:        q.Get("status"),
	}
	if filter.TenantID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "tenant_id")
		return
	}
	invoices, err := h.store.ListInvoices(r.Context(), filter)
	if err != nil {
		h.log.Error("ListInvoices: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	writeJSON(w, http.StatusOK, invoices)
}

// ── POST /v1/invoices/{invoice_id}/validate ──────────────────────────────────
//
// RECEIVED -> VALIDATED.
func (h *Handler) ValidateInvoice(w http.ResponseWriter, r *http.Request) {
	invoiceID := chi.URLParam(r, "invoice_id")
	inv, err := h.store.GetInvoice(r.Context(), invoiceID)
	if err != nil {
		h.log.Error("ValidateInvoice: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if inv == nil {
		writeError(w, http.StatusNotFound, "invoice_not_found", "")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, inv.LegalEntityID, actionValidateInvoice); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if err := h.store.TransitionInvoice(r.Context(), inv.TenantID, invoiceID,
		domain.InvoiceStatusReceived, domain.InvoiceStatusValidated, principalID); err != nil {
		h.handleTransitionErr(w, err)
		return
	}

	inv.Status = domain.InvoiceStatusValidated
	h.publisher.PublishVendorInvoiceValidated(r.Context(), *inv)
	writeJSON(w, http.StatusOK, inv)
}

// ── POST /v1/invoices/{invoice_id}/approve ───────────────────────────────────
//
// VALIDATED -> APPROVED. The "approval-state" half of the critical
// constraint ("No payable may proceed to payment initiation without
// approval-state and evidence-state validation") — this transition is only
// reachable from VALIDATED, so by the time an invoice is APPROVED it has
// necessarily passed validation too.
func (h *Handler) ApproveInvoice(w http.ResponseWriter, r *http.Request) {
	invoiceID := chi.URLParam(r, "invoice_id")
	inv, err := h.store.GetInvoice(r.Context(), invoiceID)
	if err != nil {
		h.log.Error("ApproveInvoice: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if inv == nil {
		writeError(w, http.StatusNotFound, "invoice_not_found", "")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, inv.LegalEntityID, actionApproveInvoice); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if err := h.store.TransitionInvoice(r.Context(), inv.TenantID, invoiceID,
		domain.InvoiceStatusValidated, domain.InvoiceStatusApproved, principalID); err != nil {
		h.handleTransitionErr(w, err)
		return
	}

	inv.Status = domain.InvoiceStatusApproved
	h.publisher.PublishVendorInvoiceApproved(r.Context(), *inv)
	writeJSON(w, http.StatusOK, inv)
}

// ── POST /v1/invoices/{invoice_id}/request-payment ───────────────────────────
//
// APPROVED -> PAYMENT_REQUESTED. The payment-initiation step itself —
// reachable only from APPROVED, which is itself only reachable from
// VALIDATED, so this transition is structurally impossible to reach without
// both prior checks having happened. Terminal state for this service;
// actual payment execution belongs to a future Treasury/Payments service.
func (h *Handler) RequestPayment(w http.ResponseWriter, r *http.Request) {
	invoiceID := chi.URLParam(r, "invoice_id")
	inv, err := h.store.GetInvoice(r.Context(), invoiceID)
	if err != nil {
		h.log.Error("RequestPayment: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if inv == nil {
		writeError(w, http.StatusNotFound, "invoice_not_found", "")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, inv.LegalEntityID, actionRequestPayment); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if err := h.store.TransitionInvoice(r.Context(), inv.TenantID, invoiceID,
		domain.InvoiceStatusApproved, domain.InvoiceStatusPaymentRequested, principalID); err != nil {
		h.handleTransitionErr(w, err)
		return
	}

	inv.Status = domain.InvoiceStatusPaymentRequested
	h.publisher.PublishPaymentRequested(r.Context(), *inv)
	writeJSON(w, http.StatusOK, inv)
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
		h.log.Error("TransitionInvoice: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
	}
}

func requiredInvoiceFieldMissing(req domain.CreateVendorInvoiceRequest) string {
	switch {
	case req.TenantID == "":
		return "tenant_id"
	case req.LegalEntityID == "":
		return "legal_entity_id"
	case req.VendorID == "":
		return "vendor_id"
	case req.InvoiceNumber == "":
		return "invoice_number"
	case req.CurrencyCode == "":
		return "currency_code"
	case req.DueDate.IsZero():
		return "due_date"
	default:
		return ""
	}
}

// requirePrincipal reads the caller's identity from X-Principal-Id — set by
// gateway-auth-svc's ForwardAuth verification after checking the signed
// IdentityContextEnvelope JWT. This service never decodes a JWT itself,
// matching schema-registry-svc's and general-ledger-svc's pattern. A
// request with no resolved principal never passed identity verification —
// fail closed with 401.
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
