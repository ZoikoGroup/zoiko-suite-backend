// Package handler exposes accounts-payable-svc's REST API.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/accounts-payable-svc/internal/domain"
	svcmiddleware "zoiko.io/accounts-payable-svc/internal/middleware"
)

// Store is the persistence contract the handler depends on.
type Store interface {
	CreateInvoice(ctx context.Context, inv *domain.VendorInvoice) (created bool, err error)
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
	actionCreateInvoice   = "AP_INVOICE_CREATE"
	actionValidateInvoice = "AP_INVOICE_VALIDATE"
	actionApproveInvoice  = "AP_INVOICE_APPROVE"
	actionRequestPayment  = "AP_PAYMENT_REQUEST"
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
	if !decodeJSON(w, r, &req) {
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

	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// tenant_id in the body is accepted only when it agrees with the verified
	// scope. It used to be the ONLY source of the stored tenant_id — and the
	// store handed that same body value to set_config('app.tenant_id'), so the
	// tenant the request named satisfied the RLS policy on the way past. A body
	// naming another tenant filed the payable in that tenant's ledger, where the
	// duplicate-invoice-number constraint is also scoped, so it could not even
	// collide with the real register it was hiding in.
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
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionCreateInvoice); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	inv := &domain.VendorInvoice{
		InvoiceID:            uuid.NewString(),
		TenantID:             tenantID,
		LegalEntityID:        req.LegalEntityID,
		VendorID:             req.VendorID,
		InvoiceNumber:        req.InvoiceNumber,
		Amount:               req.Amount,
		CurrencyCode:         req.CurrencyCode,
		DueDate:              req.DueDate.Time,
		Status:               domain.InvoiceStatusReceived,
		SourceContractID:     req.SourceContractID,
		CreatedByPrincipalID: principalID,
		CorrelationID:        req.CorrelationID,
	}

	created, err := h.store.CreateInvoice(r.Context(), inv)
	if err != nil {
		// A re-keyed invoice number is the caller's mistake, with a remedy they
		// can act on. It used to arrive here indistinguishable from a dead
		// database and answer 503.
		if errors.Is(err, domain.ErrDuplicateInvoiceNumber) {
			writeError(w, http.StatusConflict, "duplicate_invoice_number",
				"this vendor already has an invoice with this number for this tenant")
			return
		}
		if errors.Is(err, domain.ErrInvalidIdentifier) {
			writeError(w, http.StatusBadRequest, "invalid_field",
				"tenant_id and legal_entity_id must both be UUIDs")
			return
		}
		h.log.Error("CreateInvoice: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if !created {
		// Replay of a prior request with the same correlation_id — return
		// the original invoice, do not re-publish the received event.
		writeJSON(w, http.StatusOK, inv)
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

// ListInvoices returns the caller's own tenant's payables register.
//
// The scope comes from the verified X-Tenant-Id header. It used to come from
// ?tenant_id=, which the store both filtered on AND set app.tenant_id from — so
// the tenant the caller named satisfied the RLS policy on the way past and
// `?tenant_id=<any-uuid>` returned that tenant's entire payables register,
// vendor names and amounts included. Nothing consulted X-Tenant-Id at all.
//
// Reads are scoped rather than authorized, matching general-ledger-svc and
// accounts-receivable-svc either side of it: there is deliberately no
// AP_INVOICE_VIEW action.
func (h *Handler) ListInvoices(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	if claimed := q.Get("tenant_id"); claimed != "" && claimed != tenantID {
		writeError(w, http.StatusForbidden, "tenant_scope_mismatch", domain.ErrTenantScopeMismatch.Error())
		return
	}
	if status := q.Get("status"); status != "" && !domain.ValidInvoiceStatus(status) {
		writeError(w, http.StatusBadRequest, "invalid_field", "status is not a recognised invoice status")
		return
	}
	// legal_entity_id is compared as `legal_entity_id::text = $n`, casting the
	// COLUMN to text rather than the parameter to uuid — so a malformed value does
	// not error, it silently matches nothing, and an empty register reads as "this
	// entity has no payables". Refused here instead.
	legalEntityID := q.Get("legal_entity_id")
	if legalEntityID != "" && !isUUID(legalEntityID) {
		writeError(w, http.StatusBadRequest, "invalid_field", "legal_entity_id must be a UUID")
		return
	}
	filter := domain.ListInvoicesFilter{
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		VendorID:      q.Get("vendor_id"),
		Status:        q.Get("status"),
	}
	invoices, err := h.store.ListInvoices(r.Context(), filter)
	if err != nil {
		// The verified tenant is compared against a uuid column, so a non-UUID
		// here is a gateway that forwarded a malformed scope — a fault worth
		// naming, not a 503 implying the register is down.
		if errors.Is(err, domain.ErrInvalidIdentifier) {
			writeError(w, http.StatusBadRequest, "invalid_field", "tenant scope must be a UUID")
			return
		}
		h.log.Error("ListInvoices: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	// A nil slice marshals to JSON null, which every caller then has to special-case.
	// An empty register is an empty list.
	if invoices == nil {
		invoices = []domain.VendorInvoice{}
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

	// Re-fetch so the response reflects the transition (validated_by etc.),
	// rather than the pre-transition snapshot fetched above.
	inv, err = h.store.GetInvoice(r.Context(), invoiceID)
	if err != nil || inv == nil {
		h.log.Error("ValidateInvoice: re-fetch failed after transition", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
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

	// Segregation of Duties (docs/original_doc/zoiko_suite_doc1.txt §12.3):
	// a payment batch/invoice creator may not approve their own submission.
	if inv.CreatedByPrincipalID == principalID {
		writeError(w, http.StatusForbidden, "self_approval_not_allowed", domain.ErrSelfApprovalNotAllowed.Error())
		return
	}

	if err := h.store.TransitionInvoice(r.Context(), inv.TenantID, invoiceID,
		domain.InvoiceStatusValidated, domain.InvoiceStatusApproved, principalID); err != nil {
		h.handleTransitionErr(w, err)
		return
	}

	// Re-fetch so the response reflects the transition (approved_by etc.),
	// rather than the pre-transition snapshot fetched above.
	inv, err = h.store.GetInvoice(r.Context(), invoiceID)
	if err != nil || inv == nil {
		h.log.Error("ApproveInvoice: re-fetch failed after transition", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
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

	// This endpoint has no client-supplied idempotency key, unlike invoice
	// creation (correlation_id). A retry lands here purely by re-hitting the
	// same invoice_id, so idempotency has to be recognized from the
	// invoice's own state rather than a key lookup: if it's already
	// PAYMENT_REQUESTED, this is a replay of an already-successful call —
	// answer 200 with the current invoice, not an error, and critically,
	// never call PublishPaymentRequested again for it.
	if inv.Status == domain.InvoiceStatusPaymentRequested {
		writeJSON(w, http.StatusOK, inv)
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
		// The atomic UPDATE ... WHERE status = APPROVED already guarantees
		// only one concurrent caller ever transitions the row and publishes
		// — this handles the caller that lost that race. If the invoice is
		// PAYMENT_REQUESTED now, a concurrent request beat this one to it:
		// that's the same idempotent-replay case as above, not a real
		// failure. Any other current status is a genuine invalid transition.
		if current, getErr := h.store.GetInvoice(r.Context(), invoiceID); getErr == nil && current != nil &&
			current.Status == domain.InvoiceStatusPaymentRequested {
			writeJSON(w, http.StatusOK, current)
			return
		}
		h.handleTransitionErr(w, err)
		return
	}

	// Re-fetch so the response reflects the transition, rather than the
	// pre-transition snapshot fetched above.
	inv, err = h.store.GetInvoice(r.Context(), invoiceID)
	if err != nil || inv == nil {
		h.log.Error("RequestPayment: re-fetch failed after transition", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
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
	case errors.Is(err, domain.ErrInvoiceNotFound):
		// Reachable when the id cannot name a row at all. Kept apart from
		// invalid_transition: "there is no such invoice" and "that invoice is
		// not in a state this action can act on" are different facts, and the
		// remedy differs.
		writeError(w, http.StatusNotFound, "invoice_not_found", "")
	default:
		h.log.Error("TransitionInvoice: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
	}
}

// maxRequestBytes caps a request body. Without it a single client can stream an
// unbounded body straight into the decoder and hold a connection and memory for
// as long as it likes. An invoice header is a few hundred bytes; 64 KiB is
// already generous.
const maxRequestBytes = 64 << 10

// decodeJSON reads a JSON body strictly, and reports WHY it was refused.
//
// Two deliberate strictnesses:
//
//   - DisallowUnknownFields. Without it a misspelled key is silently discarded
//     and the service answers 201 for a record missing the value the caller
//     believed they sent — `{"vendorid": "..."}` created an invoice with no
//     vendor at all. Accepting a body and ignoring part of it is worse than
//     rejecting it, because nothing downstream can tell the difference.
//   - MaxBytesReader, for the size cap above.
//
// Returns false when it has already written the response.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		switch {
		case errors.As(err, &maxErr):
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large",
				fmt.Sprintf("body exceeds %d bytes", maxRequestBytes))
		case strings.HasPrefix(err.Error(), "json: unknown field "):
			// The message already names the offending key, and that name is the
			// entire remedy.
			writeError(w, http.StatusBadRequest, "unknown_field", err.Error())
		default:
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		}
		return false
	}
	return true
}

// requiredInvoiceFieldMissing deliberately does NOT require tenant_id: the
// tenant is the caller's verified scope, so a body omitting it is fine and a
// body disagreeing with it is a 403, not a missing field.
func requiredInvoiceFieldMissing(req domain.CreateVendorInvoiceRequest) string {
	switch {
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
