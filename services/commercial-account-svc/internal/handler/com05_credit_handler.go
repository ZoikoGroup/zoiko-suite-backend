// COM-05 Platform Commercial Billing HTTP surface, part 5c (ZS-SVC-Q-001
// §4.5, §10).
package handler

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/commercial-account-svc/internal/domain"
	svcmiddleware "zoiko.io/commercial-account-svc/internal/middleware"
	"zoiko.io/commercial-account-svc/internal/store"
)

// Every command here is delegated commercial/finance authority — never a
// customer's own (COM-CTRL-005; negative path #33: "Customer writes off
// platform balance via client API without authority -> Deny").
const (
	ActionCreditNoteIssue = "COMMERCIAL_CREDIT_NOTE_ISSUE"
	ActionWriteOffApply   = "COMMERCIAL_WRITE_OFF_APPLY"
	ActionRefundRequest   = "COMMERCIAL_REFUND_REQUEST"
	ActionRefundSettle    = "COMMERCIAL_REFUND_SETTLE"
	ActionBalanceRead     = "COMMERCIAL_BALANCE_READ"
)

const (
	CodeCreditExceedsInvoice       = "CREDIT_EXCEEDS_INVOICE"
	CodeWriteOffExceedsInvoice     = "WRITE_OFF_EXCEEDS_INVOICE"
	CodeRefundExceedsCollected     = "REFUND_EXCEEDS_COLLECTED"
	CodePaymentAttemptNotSucceeded = "PAYMENT_ATTEMPT_NOT_SUCCEEDED"
	CodeRefundRequestNotFound      = "REFUND_REQUEST_NOT_FOUND"
	CodeRefundInvalidState         = "REFUND_INVALID_STATE"
	CodeRefundDestinationMismatch  = "REFUND_DESTINATION_MISMATCH"
)

// creditFailure maps COM-05 (part 5c) errors; writeFailure consults it last.
func creditFailure(err error) (int, string, bool) {
	switch {
	case errors.Is(err, domain.ErrCreditExceedsInvoice):
		return http.StatusUnprocessableEntity, CodeCreditExceedsInvoice, true
	case errors.Is(err, domain.ErrWriteOffExceedsInvoice):
		return http.StatusUnprocessableEntity, CodeWriteOffExceedsInvoice, true
	case errors.Is(err, domain.ErrRefundExceedsCollected):
		return http.StatusUnprocessableEntity, CodeRefundExceedsCollected, true
	case errors.Is(err, domain.ErrPaymentAttemptNotSucceeded):
		return http.StatusUnprocessableEntity, CodePaymentAttemptNotSucceeded, true
	case errors.Is(err, domain.ErrRefundRequestNotFound):
		return http.StatusNotFound, CodeRefundRequestNotFound, true
	case errors.Is(err, domain.ErrRefundInvalidState):
		return http.StatusConflict, CodeRefundInvalidState, true
	case errors.Is(err, domain.ErrRefundDestinationMismatch):
		return http.StatusConflict, CodeRefundDestinationMismatch, true
	}
	return 0, "", false
}

type CreditHandler struct {
	store  store.CreditStore
	authz  AuthzChecker
	logger *zap.Logger
	now    func() time.Time
}

func NewCreditHandler(st store.CreditStore, az AuthzChecker, logger *zap.Logger) *CreditHandler {
	return &CreditHandler{store: st, authz: az, logger: logger, now: serverNow}
}

func (h *CreditHandler) WithClock(now func() time.Time) *CreditHandler {
	h.now = now
	return h
}

func RegisterCreditRoutes(r chi.Router, h *CreditHandler) {
	r.Post("/v1/commercial/invoices/{invoiceID}:credit", h.IssueCreditNote)
	r.Post("/v1/commercial/invoices/{invoiceID}:write-off", h.ApplyWriteOff)
	r.Post("/v1/commercial/invoices/{invoiceID}:request-refund", h.RequestRefund)
	r.Post("/v1/commercial/refund-requests/{id}:settle", h.SettleRefund)
	r.Get("/v1/commercial/refund-requests/{id}", h.GetRefundRequest)
	r.Get("/v1/commercial/invoices/{invoiceID}/credit-notes", h.GetCreditNotes)
	r.Get("/v1/commercial/invoices/{invoiceID}/write-offs", h.GetWriteOffs)
	r.Get("/v1/commercial/invoices/{invoiceID}/refund-requests", h.GetRefundRequests)
	r.Get("/v1/commercial/billing-accounts/{organizationID}/balance", h.GetBalance)
}

func (h *CreditHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeFailure(w, r, h.logger, err)
}

func (h *CreditHandler) principal(w http.ResponseWriter, r *http.Request) (string, bool) {
	p := strings.TrimSpace(r.Header.Get("X-Principal-Id"))
	if p == "" {
		writeProblem(w, r, Problem{Status: http.StatusUnauthorized, Code: CodeUnauthenticated, Detail: "X-Principal-Id is required"})
		return "", false
	}
	return p, true
}

func (h *CreditHandler) authorizeAt(w http.ResponseWriter, r *http.Request, principal, scope, action string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principal, scope, action); err != nil {
		writeProblem(w, r, Problem{Status: http.StatusForbidden, Code: CodeAuthorizationDenied, Detail: "not authorized: " + action})
		return false
	}
	return true
}

func (h *CreditHandler) sellerPrincipal(w http.ResponseWriter, r *http.Request, action string) (string, bool) {
	principal, ok := h.principal(w, r)
	if !ok {
		return "", false
	}
	return principal, h.authorizeAt(w, r, principal, platformScopeID, action)
}

// readScope resolves who a read is about. orgFromQuery (or, for GetBalance,
// the org named in the URL path) only means "an operator naming another
// organization" when it actually differs from the caller's own verified
// tenant — naming (or defensively echoing) your own organization is still a
// self-read and must not be forced through the platform-wide grant a plain
// tenant will never hold.
func (h *CreditHandler) readScope(w http.ResponseWriter, r *http.Request, orgFromQuery string, action string) (string, bool) {
	principal, ok := h.principal(w, r)
	if !ok {
		return "", false
	}
	callerOrg := svcmiddleware.TenantFromContext(r.Context())
	if orgFromQuery != "" && orgFromQuery != callerOrg {
		if !h.authorizeAt(w, r, principal, platformScopeID, action) {
			return "", false
		}
		return orgFromQuery, true
	}
	org := orgFromQuery
	if org == "" {
		org = callerOrg
	}
	if org == "" {
		writeProblem(w, r, Problem{Status: http.StatusUnauthorized, Code: CodeUnauthenticated, Detail: "X-Tenant-Id is required"})
		return "", false
	}
	if !h.authorizeAt(w, r, principal, org, action) {
		return "", false
	}
	return org, true
}

// ── Credit notes ─────────────────────────────────────────────────────────────

type creditNoteRequest struct {
	Amount string `json:"amount"`
	Reason string `json:"reason"`
}

func (h *CreditHandler) IssueCreditNote(w http.ResponseWriter, r *http.Request) {
	invoiceID, ok := parseID(w, r, domain.PrefixInvoice, chi.URLParam(r, "invoiceID"))
	if !ok {
		return
	}
	principal, ok := h.sellerPrincipal(w, r, ActionCreditNoteIssue)
	if !ok {
		return
	}
	var req creditNoteRequest
	raw, ok := readBody(w, r, &req, false)
	if !ok {
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: "reason", Detail: "is required"})
		return
	}
	id := domain.NewCommercialID(domain.PrefixCreditNote)
	cmd, ok := commandFor(w, r, domain.SellerScope, principal, "IssueCreditNote", id, raw, false)
	if !ok {
		return
	}
	c, err := h.store.IssueCreditNote(r.Context(), id, invoiceID, req.Amount, req.Reason, principal, h.now(), cmd.claim)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (h *CreditHandler) GetCreditNotes(w http.ResponseWriter, r *http.Request) {
	invoiceID, ok := parseID(w, r, domain.PrefixInvoice, chi.URLParam(r, "invoiceID"))
	if !ok {
		return
	}
	org, ok := h.readScope(w, r, r.URL.Query().Get("organization_id"), ActionInvoiceRead)
	if !ok {
		return
	}
	cs, err := h.store.GetCreditNotes(svcmiddleware.WithTenant(r.Context(), org), invoiceID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if cs == nil {
		cs = []domain.CreditNote{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"invoice_id": invoiceID, "credit_notes": cs})
}

// ── Write-offs ───────────────────────────────────────────────────────────────

func (h *CreditHandler) ApplyWriteOff(w http.ResponseWriter, r *http.Request) {
	invoiceID, ok := parseID(w, r, domain.PrefixInvoice, chi.URLParam(r, "invoiceID"))
	if !ok {
		return
	}
	principal, ok := h.sellerPrincipal(w, r, ActionWriteOffApply)
	if !ok {
		return
	}
	var req creditNoteRequest
	raw, ok := readBody(w, r, &req, false)
	if !ok {
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: "reason", Detail: "is required"})
		return
	}
	id := domain.NewCommercialID(domain.PrefixWriteOff)
	cmd, ok := commandFor(w, r, domain.SellerScope, principal, "ApplyWriteOff", id, raw, false)
	if !ok {
		return
	}
	wo, err := h.store.ApplyWriteOff(r.Context(), id, invoiceID, req.Amount, req.Reason, principal, h.now(), cmd.claim)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, wo)
}

func (h *CreditHandler) GetWriteOffs(w http.ResponseWriter, r *http.Request) {
	invoiceID, ok := parseID(w, r, domain.PrefixInvoice, chi.URLParam(r, "invoiceID"))
	if !ok {
		return
	}
	org, ok := h.readScope(w, r, r.URL.Query().Get("organization_id"), ActionInvoiceRead)
	if !ok {
		return
	}
	ws, err := h.store.GetWriteOffs(svcmiddleware.WithTenant(r.Context(), org), invoiceID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if ws == nil {
		ws = []domain.WriteOff{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"invoice_id": invoiceID, "write_offs": ws})
}

// ── Refunds ──────────────────────────────────────────────────────────────────

type requestRefundRequest struct {
	PaymentAttemptID string `json:"payment_attempt_id"`
	Amount           string `json:"amount"`
	DestinationRef   string `json:"destination_ref"`
	Reason           string `json:"reason"`
}

func (h *CreditHandler) RequestRefund(w http.ResponseWriter, r *http.Request) {
	invoiceID, ok := parseID(w, r, domain.PrefixInvoice, chi.URLParam(r, "invoiceID"))
	if !ok {
		return
	}
	principal, ok := h.sellerPrincipal(w, r, ActionRefundRequest)
	if !ok {
		return
	}
	var req requestRefundRequest
	raw, ok := readBody(w, r, &req, false)
	if !ok {
		return
	}
	attemptID, ok := parseID(w, r, domain.PrefixPaymentAttempt, req.PaymentAttemptID)
	if !ok {
		return
	}
	if strings.TrimSpace(req.DestinationRef) == "" || strings.TrimSpace(req.Reason) == "" {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext,
			Detail: "destination_ref and reason are both required"})
		return
	}
	id := domain.NewCommercialID(domain.PrefixRefundRequest)
	cmd, ok := commandFor(w, r, domain.SellerScope, principal, "RequestRefund", id, raw, false)
	if !ok {
		return
	}
	rr, err := h.store.RequestRefund(r.Context(), id, invoiceID, attemptID, req.Amount, req.DestinationRef, req.Reason, principal, h.now(), cmd.claim)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, rr)
}

type settleRefundRequest struct {
	Outcome        string  `json:"outcome"`
	DestinationRef string  `json:"destination_ref"`
	SettlementRef  *string `json:"settlement_ref,omitempty"`
	FailureReason  *string `json:"failure_reason,omitempty"`
}

func (h *CreditHandler) SettleRefund(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, domain.PrefixRefundRequest, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	principal, ok := h.sellerPrincipal(w, r, ActionRefundSettle)
	if !ok {
		return
	}
	var req settleRefundRequest
	raw, ok := readBody(w, r, &req, false)
	if !ok {
		return
	}
	domainReq := domain.SettleRefundRequest{
		RefundID: id, Outcome: domain.RefundStatus(req.Outcome), DestinationRef: req.DestinationRef,
		SettlementRef: req.SettlementRef, FailureReason: req.FailureReason, OccurredAt: h.now(), ActorPrincipalID: principal,
	}
	if err := domainReq.Validate(); err != nil {
		h.fail(w, r, err)
		return
	}
	cmd, ok := commandFor(w, r, domain.SellerScope, principal, "SettleRefund", id, raw, false)
	if !ok {
		return
	}
	rr, err := h.store.SettleRefund(r.Context(), domainReq, cmd.claim)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rr)
}

func (h *CreditHandler) GetRefundRequest(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, domain.PrefixRefundRequest, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	org, ok := h.readScope(w, r, r.URL.Query().Get("organization_id"), ActionInvoiceRead)
	if !ok {
		return
	}
	rr, err := h.store.GetRefundRequest(svcmiddleware.WithTenant(r.Context(), org), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rr)
}

func (h *CreditHandler) GetRefundRequests(w http.ResponseWriter, r *http.Request) {
	invoiceID, ok := parseID(w, r, domain.PrefixInvoice, chi.URLParam(r, "invoiceID"))
	if !ok {
		return
	}
	org, ok := h.readScope(w, r, r.URL.Query().Get("organization_id"), ActionInvoiceRead)
	if !ok {
		return
	}
	rs, err := h.store.GetRefundRequests(svcmiddleware.WithTenant(r.Context(), org), invoiceID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if rs == nil {
		rs = []domain.RefundRequest{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"invoice_id": invoiceID, "refund_requests": rs})
}

// ── Balance ──────────────────────────────────────────────────────────────────

func (h *CreditHandler) GetBalance(w http.ResponseWriter, r *http.Request) {
	orgFromPath := chi.URLParam(r, "organizationID")
	org, ok := h.readScope(w, r, orgFromPath, ActionBalanceRead)
	if !ok {
		return
	}
	bal, err := h.store.GetBalance(svcmiddleware.WithTenant(r.Context(), org), org)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, bal)
}
