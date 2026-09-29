// COM-05 Platform Commercial Billing HTTP surface, part 5a (ZS-SVC-Q-001
// §4.5, §10).
package handler

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/commercial-account-svc/internal/domain"
	svcmiddleware "zoiko.io/commercial-account-svc/internal/middleware"
	"zoiko.io/commercial-account-svc/internal/store"
)

// Opening a billing account and every step of turning usage/subscription
// facts into an issued invoice is platform billing-operations authority —
// never the customer's own. Reading an invoice is the organization's own, or
// an operator's with the platform grant, same shape as COM-04's usage reads.
const (
	ActionBillingAccountManage = "COMMERCIAL_BILLING_ACCOUNT_MANAGE"
	ActionInvoiceGenerate      = "COMMERCIAL_INVOICE_GENERATE"
	ActionInvoiceApprove       = "COMMERCIAL_INVOICE_APPROVE"
	ActionInvoiceIssue         = "COMMERCIAL_INVOICE_ISSUE"
	ActionInvoiceRead          = "COMMERCIAL_INVOICE_READ"
)

const (
	CodeBillingAccountNotFound   = "BILLING_ACCOUNT_NOT_FOUND"
	CodeBillingAccountExists     = "BILLING_ACCOUNT_EXISTS"
	CodeBillingAccountNotActive  = "BILLING_ACCOUNT_NOT_ACTIVE"
	CodeUsageBasisNotCertified   = "USAGE_BASIS_NOT_CERTIFIED"
	CodeInvoiceCandidateNotFound = "INVOICE_CANDIDATE_NOT_FOUND"
	CodeInvoiceCandidateInvalid  = "INVOICE_CANDIDATE_INVALID_STATE"
	CodeInvoiceAlreadyIssuedTerm = "INVOICE_ALREADY_ISSUED_FOR_TERM"
	CodeInvoiceBasisChanged      = "INVOICE_BASIS_CHANGED"
	CodeInvoiceNotFound          = "INVOICE_NOT_FOUND"
	CodeEmptyInvoiceCandidate    = "EMPTY_INVOICE_CANDIDATE"
)

// billingFailure maps COM-05 (part 5a) errors; writeFailure consults it last.
func billingFailure(err error) (int, string, bool) {
	switch {
	case errors.Is(err, domain.ErrBillingAccountNotFound):
		return http.StatusNotFound, CodeBillingAccountNotFound, true
	case errors.Is(err, domain.ErrBillingAccountExists):
		return http.StatusConflict, CodeBillingAccountExists, true
	case errors.Is(err, domain.ErrBillingAccountNotActive):
		return http.StatusConflict, CodeBillingAccountNotActive, true
	case errors.Is(err, domain.ErrUsageBasisNotCertified):
		return http.StatusUnprocessableEntity, CodeUsageBasisNotCertified, true
	case errors.Is(err, domain.ErrInvoiceCandidateNotFound):
		return http.StatusNotFound, CodeInvoiceCandidateNotFound, true
	case errors.Is(err, domain.ErrInvoiceCandidateInvalidState):
		return http.StatusConflict, CodeInvoiceCandidateInvalid, true
	case errors.Is(err, domain.ErrInvoiceCandidateSelfApproval):
		return http.StatusForbidden, CodeSoDViolation, true
	case errors.Is(err, domain.ErrInvoiceAlreadyIssuedForTerm):
		return http.StatusConflict, CodeInvoiceAlreadyIssuedTerm, true
	case errors.Is(err, domain.ErrInvoiceBasisChanged):
		return http.StatusConflict, CodeInvoiceBasisChanged, true
	case errors.Is(err, domain.ErrInvoiceNotFound):
		return http.StatusNotFound, CodeInvoiceNotFound, true
	case errors.Is(err, domain.ErrEmptyInvoiceCandidate):
		return http.StatusUnprocessableEntity, CodeEmptyInvoiceCandidate, true
	case errors.Is(err, domain.ErrSubscriptionNotFound), errors.Is(err, ErrNoEffectiveVersionAlias):
		return http.StatusNotFound, CodeNotFound, true
	}
	return 0, "", false
}

// ErrNoEffectiveVersionAlias lets billingFailure recognize
// store.ErrNoEffectiveVersion without an import cycle concern — the store
// package already depends on domain, not the reverse, so this handler
// package (which imports both) names it directly instead.
var ErrNoEffectiveVersionAlias = store.ErrNoEffectiveVersion

type BillingHandler struct {
	store  store.BillingStore
	authz  AuthzChecker
	logger *zap.Logger
	now    func() time.Time
}

func NewBillingHandler(st store.BillingStore, az AuthzChecker, logger *zap.Logger) *BillingHandler {
	return &BillingHandler{store: st, authz: az, logger: logger, now: serverNow}
}

func (h *BillingHandler) WithClock(now func() time.Time) *BillingHandler {
	h.now = now
	return h
}

func RegisterBillingRoutes(r chi.Router, h *BillingHandler) {
	r.Post("/v1/commercial/billing-accounts", h.OpenBillingAccount)
	r.Get("/v1/commercial/billing-accounts/{organizationID}", h.GetBillingAccount)
	r.Post("/v1/commercial/invoice-candidates:generate", h.GenerateInvoiceCandidate)
	r.Route("/v1/commercial/invoice-candidates", func(r chi.Router) {
		r.Get("/{id}", h.GetInvoiceCandidate)
		r.Post("/{id}", h.InvoiceCandidateAction)
	})
	r.Route("/v1/commercial/invoices", func(r chi.Router) {
		r.Get("/{id}", h.GetInvoice)
		r.Get("/{id}/basis", h.GetInvoiceBasis)
	})
}

func (h *BillingHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeFailure(w, r, h.logger, err)
}

func (h *BillingHandler) principal(w http.ResponseWriter, r *http.Request) (string, bool) {
	p := strings.TrimSpace(r.Header.Get("X-Principal-Id"))
	if p == "" {
		writeProblem(w, r, Problem{Status: http.StatusUnauthorized, Code: CodeUnauthenticated, Detail: "X-Principal-Id is required"})
		return "", false
	}
	return p, true
}

func (h *BillingHandler) authorizeAt(w http.ResponseWriter, r *http.Request, principal, scope, action string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principal, scope, action); err != nil {
		writeProblem(w, r, Problem{Status: http.StatusForbidden, Code: CodeAuthorizationDenied, Detail: "not authorized: " + action})
		return false
	}
	return true
}

func (h *BillingHandler) sellerPrincipal(w http.ResponseWriter, r *http.Request, action string) (string, bool) {
	principal, ok := h.principal(w, r)
	if !ok {
		return "", false
	}
	return principal, h.authorizeAt(w, r, principal, platformScopeID, action)
}

// readScope resolves who an invoice/billing-account read is about: an
// operator naming an organization (needs the platform read grant), or the
// caller's own verified tenant.
// readScope resolves who a read is about. orgFromPath is only "an operator
// naming another organization" when it actually names a DIFFERENT
// organization than the caller's own verified tenant — naming your own
// organization in the URL (or omitting it) is still a self-read, and must
// not be forced through the platform-wide grant a plain tenant will never
// hold. Only a genuine cross-organization read requires it.
func (h *BillingHandler) readScope(w http.ResponseWriter, r *http.Request, orgFromPath string) (string, bool) {
	principal, ok := h.principal(w, r)
	if !ok {
		return "", false
	}
	callerOrg := svcmiddleware.TenantFromContext(r.Context())
	if orgFromPath != "" && orgFromPath != callerOrg {
		if !h.authorizeAt(w, r, principal, platformScopeID, ActionInvoiceRead) {
			return "", false
		}
		return orgFromPath, true
	}
	org := orgFromPath
	if org == "" {
		org = callerOrg
	}
	if org == "" {
		writeProblem(w, r, Problem{Status: http.StatusUnauthorized, Code: CodeUnauthenticated, Detail: "X-Tenant-Id is required"})
		return "", false
	}
	if !h.authorizeAt(w, r, principal, org, ActionInvoiceRead) {
		return "", false
	}
	return org, true
}

// ── Billing accounts ─────────────────────────────────────────────────────────

type openBillingAccountRequest struct {
	OrganizationID          string `json:"organization_id"`
	SellingEntity           string `json:"selling_entity"`
	BillingCurrencyCode     string `json:"billing_currency_code"`
	InvoiceNumberingProfile string `json:"invoice_numbering_profile"`
	PaymentProviderRef      string `json:"payment_provider_ref"`
	AccountingMappingKey    string `json:"accounting_mapping_key"`
}

func (h *BillingHandler) OpenBillingAccount(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.sellerPrincipal(w, r, ActionBillingAccountManage)
	if !ok {
		return
	}
	var req openBillingAccountRequest
	raw, ok := readBody(w, r, &req, false)
	if !ok {
		return
	}
	if _, err := parseUUID(req.OrganizationID); err != nil {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: "organization_id", Detail: "must be a UUID"})
		return
	}
	b := &domain.BillingAccount{
		BillingAccountID: domain.NewCommercialID(domain.PrefixBillingAccount), OrganizationID: req.OrganizationID,
		SellingEntity: req.SellingEntity, BillingCurrencyCode: req.BillingCurrencyCode,
		InvoiceNumberingProfile: req.InvoiceNumberingProfile, PaymentProviderRef: req.PaymentProviderRef,
		AccountingMappingKey: req.AccountingMappingKey,
		CreatedAt:            h.now(), CreatedByPrincipalID: principal,
	}
	if err := domain.ValidateBillingAccount(b); err != nil {
		h.fail(w, r, err)
		return
	}
	cmd, ok := commandFor(w, r, domain.SellerScope, principal, "OpenBillingAccount", req.OrganizationID, raw, false)
	if !ok {
		return
	}
	created, err := h.store.OpenBillingAccount(r.Context(), b, cmd.claim)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (h *BillingHandler) GetBillingAccount(w http.ResponseWriter, r *http.Request) {
	org, ok := h.readScope(w, r, chi.URLParam(r, "organizationID"))
	if !ok {
		return
	}
	b, err := h.store.GetBillingAccount(svcmiddleware.WithTenant(r.Context(), org), org)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// ── Invoice candidates ───────────────────────────────────────────────────────

type generateInvoiceCandidateRequest struct {
	OrganizationID      string `json:"organization_id"`
	SubscriptionID      string `json:"subscription_id"`
	TermNo              int    `json:"term_no"`
	TaxJurisdictionCode string `json:"tax_jurisdiction_code"`
	TaxRateBasisPoints  int    `json:"tax_rate_basis_points"`
	TaxAmount           string `json:"tax_amount"`
}

func (h *BillingHandler) GenerateInvoiceCandidate(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.sellerPrincipal(w, r, ActionInvoiceGenerate)
	if !ok {
		return
	}
	var req generateInvoiceCandidateRequest
	raw, ok := readBody(w, r, &req, false)
	if !ok {
		return
	}
	if _, err := parseUUID(req.OrganizationID); err != nil {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: "organization_id", Detail: "must be a UUID"})
		return
	}
	subID, ok := parseID(w, r, domain.PrefixSubscription, req.SubscriptionID)
	if !ok {
		return
	}
	candidateID := domain.NewCommercialID(domain.PrefixInvoiceCandidate)
	domainReq := domain.GenerateInvoiceCandidateRequest{
		CandidateID: candidateID, OrganizationID: req.OrganizationID, SubscriptionID: subID, TermNo: req.TermNo,
		TaxJurisdictionCode: req.TaxJurisdictionCode, TaxRateBasisPoints: req.TaxRateBasisPoints,
		TaxAmount: req.TaxAmount, CreatedByPrincipalID: principal,
	}
	if err := domainReq.Validate(); err != nil {
		h.fail(w, r, err)
		return
	}
	// The claim's resource id is the minted candidate id, not the
	// subscription/term pair: a replay must resolve back to the exact
	// candidate row this call would have created, and only the id serves
	// that (subscription/term is not unique once a later term is billed).
	cmd, ok := commandFor(w, r, domain.SellerScope, principal, "GenerateInvoiceCandidate", candidateID, raw, false)
	if !ok {
		return
	}
	c, err := h.store.GenerateInvoiceCandidate(r.Context(), domainReq, cmd.claim)
	if err != nil {
		var replay *domain.IdempotentReplayError
		if errors.As(err, &replay) {
			if existing, gerr := h.store.GetInvoiceCandidate(r.Context(), replay.ResourceID); gerr == nil {
				w.Header().Set("Idempotent-Replayed", "true")
				writeJSON(w, http.StatusCreated, existing)
				return
			}
		}
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (h *BillingHandler) GetInvoiceCandidate(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, domain.PrefixInvoiceCandidate, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	org, ok := h.readScope(w, r, r.URL.Query().Get("organization_id"))
	if !ok {
		return
	}
	c, err := h.store.GetInvoiceCandidate(svcmiddleware.WithTenant(r.Context(), org), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// InvoiceCandidateAction dispatches POST /invoice-candidates/{id}:{approve|issue}.
func (h *BillingHandler) InvoiceCandidateAction(w http.ResponseWriter, r *http.Request) {
	rawID, action, found := strings.Cut(chi.URLParam(r, "id"), ":")
	if !found || (action != "approve" && action != "issue") {
		writeProblem(w, r, Problem{Status: http.StatusNotFound, Code: CodeNotFound, Detail: "expected :approve or :issue"})
		return
	}
	id, ok := parseID(w, r, domain.PrefixInvoiceCandidate, rawID)
	if !ok {
		return
	}
	authAction := ActionInvoiceApprove
	if action == "issue" {
		authAction = ActionInvoiceIssue
	}
	principal, ok := h.sellerPrincipal(w, r, authAction)
	if !ok {
		return
	}
	if _, ok := readBody(w, r, &struct{}{}, true); !ok {
		return
	}
	cmd, ok := commandFor(w, r, domain.SellerScope, principal, "InvoiceCandidate:"+action, id, nil, false)
	if !ok {
		return
	}
	if action == "approve" {
		c, err := h.store.ApproveInvoiceCandidate(r.Context(), id, principal, h.now(), cmd.claim)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, c)
		return
	}
	inv, err := h.store.IssueInvoice(r.Context(), id, principal, h.now(), cmd.claim)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, inv)
}

// ── Invoices ─────────────────────────────────────────────────────────────────

func (h *BillingHandler) GetInvoice(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, domain.PrefixInvoice, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	org, ok := h.readScope(w, r, r.URL.Query().Get("organization_id"))
	if !ok {
		return
	}
	inv, err := h.store.GetInvoice(svcmiddleware.WithTenant(r.Context(), org), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, inv)
}

func (h *BillingHandler) GetInvoiceBasis(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, domain.PrefixInvoice, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	org, ok := h.readScope(w, r, r.URL.Query().Get("organization_id"))
	if !ok {
		return
	}
	basis, err := h.store.GetInvoiceBasis(svcmiddleware.WithTenant(r.Context(), org), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, basis)
}

func parseUUID(s string) (string, error) {
	_, err := uuid.Parse(s)
	return s, err
}
