// COM-05 Platform Commercial Billing HTTP surface, part 5e (ZS-SVC-Q-001
// §4.5, §6, §10). Dispute/chargeback and CommercialEvidencePackage.
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

const (
	ActionDisputeManage = "COMMERCIAL_DISPUTE_MANAGE"
	ActionDisputeRead   = "COMMERCIAL_DISPUTE_READ"
)

const (
	CodeDisputeCaseNotFound        = "DISPUTE_CASE_NOT_FOUND"
	CodeDisputeCaseInvalidState    = "DISPUTE_CASE_INVALID_STATE"
	CodeDisputeAttemptNotSucceeded = "DISPUTE_ATTEMPT_NOT_SUCCEEDED"
	CodeEvidencePackageNotFound    = "EVIDENCE_PACKAGE_NOT_FOUND"
)

func disputeFailure(err error) (int, string, bool) {
	switch {
	case errors.Is(err, domain.ErrDisputeCaseNotFound):
		return http.StatusNotFound, CodeDisputeCaseNotFound, true
	case errors.Is(err, domain.ErrDisputeCaseInvalidState):
		return http.StatusConflict, CodeDisputeCaseInvalidState, true
	case errors.Is(err, domain.ErrDisputeAttemptNotSucceeded):
		return http.StatusUnprocessableEntity, CodeDisputeAttemptNotSucceeded, true
	case errors.Is(err, domain.ErrEvidencePackageNotFound):
		return http.StatusNotFound, CodeEvidencePackageNotFound, true
	}
	return 0, "", false
}

type DisputeHandler struct {
	store   store.DisputeStore
	billing store.BillingStore
	authz   AuthzChecker
	logger  *zap.Logger
	now     func() time.Time
}

func NewDisputeHandler(disputeStore store.DisputeStore, billingStore store.BillingStore, az AuthzChecker, logger *zap.Logger) *DisputeHandler {
	return &DisputeHandler{store: disputeStore, billing: billingStore, authz: az, logger: logger, now: serverNow}
}

func (h *DisputeHandler) WithClock(now func() time.Time) *DisputeHandler {
	h.now = now
	return h
}

func RegisterDisputeRoutes(r chi.Router, h *DisputeHandler) {
	r.Route("/v1/commercial/disputes", func(r chi.Router) {
		r.Post("/", h.OpenDispute)
		r.Get("/{id}", h.GetDispute)
		r.Post("/{id}", h.DisputeAction)
	})
	r.Get("/v1/commercial/invoices/{invoiceID}/evidence-package", h.GetEvidencePackage)
}

func (h *DisputeHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeFailure(w, r, h.logger, err)
}

func (h *DisputeHandler) principal(w http.ResponseWriter, r *http.Request) (string, bool) {
	p := strings.TrimSpace(r.Header.Get("X-Principal-Id"))
	if p == "" {
		writeProblem(w, r, Problem{Status: http.StatusUnauthorized, Code: CodeUnauthenticated, Detail: "X-Principal-Id is required"})
		return "", false
	}
	return p, true
}

func (h *DisputeHandler) authorizeAt(w http.ResponseWriter, r *http.Request, principal, scope, action string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principal, scope, action); err != nil {
		writeProblem(w, r, Problem{Status: http.StatusForbidden, Code: CodeAuthorizationDenied, Detail: "not authorized: " + action})
		return false
	}
	return true
}

func (h *DisputeHandler) sellerPrincipal(w http.ResponseWriter, r *http.Request, action string) (string, bool) {
	principal, ok := h.principal(w, r)
	if !ok {
		return "", false
	}
	return principal, h.authorizeAt(w, r, principal, platformScopeID, action)
}

func (h *DisputeHandler) readScope(w http.ResponseWriter, r *http.Request, orgFromQuery string, action string) (string, bool) {
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

type openDisputeRequest struct {
	InvoiceID        string `json:"invoice_id"`
	PaymentAttemptID string `json:"payment_attempt_id"`
	Reason           string `json:"reason"`
}

func (h *DisputeHandler) OpenDispute(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.sellerPrincipal(w, r, ActionDisputeManage)
	if !ok {
		return
	}
	var req openDisputeRequest
	raw, ok := readBody(w, r, &req, false)
	if !ok {
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: "reason", Detail: "is required"})
		return
	}
	invoiceID, ok := parseID(w, r, domain.PrefixInvoice, req.InvoiceID)
	if !ok {
		return
	}
	attemptID, ok := parseID(w, r, domain.PrefixPaymentAttempt, req.PaymentAttemptID)
	if !ok {
		return
	}
	id := domain.NewCommercialID(domain.PrefixDispute)
	cmd, ok := commandFor(w, r, domain.SellerScope, principal, "OpenDispute", id, raw, false)
	if !ok {
		return
	}
	d, err := h.store.OpenDispute(r.Context(), id, invoiceID, attemptID, req.Reason, principal, h.now(), cmd.claim)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

type disputeOutcomeRequest struct {
	Outcome string `json:"outcome"`
}

func (h *DisputeHandler) DisputeAction(w http.ResponseWriter, r *http.Request) {
	rawID, action, found := strings.Cut(chi.URLParam(r, "id"), ":")
	if !found || (action != "outcome" && action != "close") {
		writeProblem(w, r, Problem{Status: http.StatusNotFound, Code: CodeNotFound, Detail: "expected :outcome or :close"})
		return
	}
	id, ok := parseID(w, r, domain.PrefixDispute, rawID)
	if !ok {
		return
	}
	principal, ok := h.sellerPrincipal(w, r, ActionDisputeManage)
	if !ok {
		return
	}
	var req disputeOutcomeRequest
	raw, ok := readBody(w, r, &req, false)
	if !ok {
		return
	}
	cmd, ok := commandFor(w, r, domain.SellerScope, principal, "Dispute:"+action, id, raw, false)
	if !ok {
		return
	}
	var d *domain.DisputeCase
	var err error
	if action == "outcome" {
		if req.Outcome != string(domain.DisputeWon) && req.Outcome != string(domain.DisputeLost) {
			writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: "outcome", Detail: "must be WON or LOST"})
			return
		}
		d, err = h.store.RecordDisputeOutcome(r.Context(), id, domain.DisputeStatus(req.Outcome), principal, h.now(), cmd.claim)
	} else {
		reason := r.URL.Query().Get("reason")
		d, err = h.store.CloseDispute(r.Context(), id, reason, principal, h.now(), cmd.claim)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (h *DisputeHandler) GetDispute(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, domain.PrefixDispute, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	org, ok := h.readScope(w, r, r.URL.Query().Get("organization_id"), ActionDisputeRead)
	if !ok {
		return
	}
	d, err := h.store.GetDispute(svcmiddleware.WithTenant(r.Context(), org), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (h *DisputeHandler) GetEvidencePackage(w http.ResponseWriter, r *http.Request) {
	invoiceID, ok := parseID(w, r, domain.PrefixInvoice, chi.URLParam(r, "invoiceID"))
	if !ok {
		return
	}
	org, ok := h.readScope(w, r, r.URL.Query().Get("organization_id"), ActionInvoiceRead)
	if !ok {
		return
	}
	pkg, err := h.billing.GetEvidencePackage(svcmiddleware.WithTenant(r.Context(), org), invoiceID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, pkg)
}
