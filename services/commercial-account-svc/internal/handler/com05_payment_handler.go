// COM-05 Platform Commercial Billing HTTP surface, part 5b (ZS-SVC-Q-001
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

// Collecting payment and recording a provider's outcome are platform
// billing-operations/workload authority — the same class of grant COM-04's
// usage ingestion uses, since this platform has no per-caller mTLS identity
// extraction for a provider-adapter workload to present instead.
const (
	ActionPaymentCollect       = "COMMERCIAL_PAYMENT_COLLECT"
	ActionPaymentOutcomeRecord = "COMMERCIAL_PAYMENT_OUTCOME_RECORD"
)

const (
	CodeInvoiceAlreadyPaid     = "INVOICE_ALREADY_PAID"
	CodePaymentAttemptNotFound = "PAYMENT_ATTEMPT_NOT_FOUND"
	CodePaymentAttemptInvalid  = "PAYMENT_ATTEMPT_INVALID_STATE"
	CodeDuplicateProviderEvent = "DUPLICATE_PROVIDER_EVENT"
)

// paymentFailure maps COM-05 (part 5b) errors; writeFailure consults it last.
func paymentFailure(err error) (int, string, bool) {
	switch {
	case errors.Is(err, domain.ErrInvoiceAlreadyPaid):
		return http.StatusConflict, CodeInvoiceAlreadyPaid, true
	case errors.Is(err, domain.ErrPaymentAttemptNotFound):
		return http.StatusNotFound, CodePaymentAttemptNotFound, true
	case errors.Is(err, domain.ErrPaymentAttemptInvalidState):
		return http.StatusConflict, CodePaymentAttemptInvalid, true
	case errors.Is(err, domain.ErrDuplicateProviderEvent):
		return http.StatusConflict, CodeDuplicateProviderEvent, true
	}
	return 0, "", false
}

type PaymentHandler struct {
	store  store.PaymentStore
	authz  AuthzChecker
	logger *zap.Logger
	now    func() time.Time
}

func NewPaymentHandler(st store.PaymentStore, az AuthzChecker, logger *zap.Logger) *PaymentHandler {
	return &PaymentHandler{store: st, authz: az, logger: logger, now: serverNow}
}

func (h *PaymentHandler) WithClock(now func() time.Time) *PaymentHandler {
	h.now = now
	return h
}

func RegisterPaymentRoutes(r chi.Router, h *PaymentHandler) {
	r.Post("/v1/commercial/invoices/{invoiceID}:collect", h.CollectPayment)
	r.Post("/v1/commercial/payment-attempts/{id}:record-outcome", h.RecordProviderOutcome)
	r.Get("/v1/commercial/payment-attempts/{id}", h.GetPaymentAttempt)
	r.Get("/v1/commercial/invoices/{invoiceID}/payment-attempts", h.GetPaymentAttempts)
	r.Get("/v1/commercial/invoices/{invoiceID}/collection-state", h.GetCollectionState)
}

func (h *PaymentHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeFailure(w, r, h.logger, err)
}

func (h *PaymentHandler) principal(w http.ResponseWriter, r *http.Request) (string, bool) {
	p := strings.TrimSpace(r.Header.Get("X-Principal-Id"))
	if p == "" {
		writeProblem(w, r, Problem{Status: http.StatusUnauthorized, Code: CodeUnauthenticated, Detail: "X-Principal-Id is required"})
		return "", false
	}
	return p, true
}

func (h *PaymentHandler) authorizeAt(w http.ResponseWriter, r *http.Request, principal, scope, action string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principal, scope, action); err != nil {
		writeProblem(w, r, Problem{Status: http.StatusForbidden, Code: CodeAuthorizationDenied, Detail: "not authorized: " + action})
		return false
	}
	return true
}

func (h *PaymentHandler) sellerPrincipal(w http.ResponseWriter, r *http.Request, action string) (string, bool) {
	principal, ok := h.principal(w, r)
	if !ok {
		return "", false
	}
	return principal, h.authorizeAt(w, r, principal, platformScopeID, action)
}

// readScope resolves who a read is about. orgFromQuery only means "an
// operator naming another organization" when it actually differs from the
// caller's own verified tenant — a client that defensively echoes its own
// org id as a query parameter is still doing a self-read.
func (h *PaymentHandler) readScope(w http.ResponseWriter, r *http.Request, orgFromQuery string) (string, bool) {
	principal, ok := h.principal(w, r)
	if !ok {
		return "", false
	}
	callerOrg := svcmiddleware.TenantFromContext(r.Context())
	if orgFromQuery != "" && orgFromQuery != callerOrg {
		if !h.authorizeAt(w, r, principal, platformScopeID, ActionInvoiceRead) {
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
	if !h.authorizeAt(w, r, principal, org, ActionInvoiceRead) {
		return "", false
	}
	return org, true
}

// ── Collect ──────────────────────────────────────────────────────────────────

func (h *PaymentHandler) CollectPayment(w http.ResponseWriter, r *http.Request) {
	invoiceID, ok := parseID(w, r, domain.PrefixInvoice, chi.URLParam(r, "invoiceID"))
	if !ok {
		return
	}
	principal, ok := h.sellerPrincipal(w, r, ActionPaymentCollect)
	if !ok {
		return
	}
	if _, ok := readBody(w, r, &struct{}{}, true); !ok {
		return
	}
	attemptID := domain.NewCommercialID(domain.PrefixPaymentAttempt)
	// The claim's resource id is the minted attempt id: CollectPayment's own
	// "no blind retry" behavior already answers a genuine second call with
	// the existing attempt (see com05_payment_store.go), so this claim only
	// protects against a literal duplicate submission of the same request.
	cmd, ok := commandFor(w, r, domain.SellerScope, principal, "CollectPayment", attemptID, nil, false)
	if !ok {
		return
	}
	a, err := h.store.CollectPayment(r.Context(), attemptID, invoiceID, principal, h.now(), cmd.claim)
	if err != nil {
		var replay *domain.IdempotentReplayError
		if errors.As(err, &replay) {
			if existing, gerr := h.store.GetPaymentAttempt(r.Context(), replay.ResourceID); gerr == nil {
				w.Header().Set("Idempotent-Replayed", "true")
				writeJSON(w, http.StatusCreated, existing)
				return
			}
		}
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

// ── Provider outcome ─────────────────────────────────────────────────────────

type recordOutcomeRequest struct {
	Outcome            string     `json:"outcome"`
	ProviderAttemptRef *string    `json:"provider_attempt_ref,omitempty"`
	ProviderEventID    *string    `json:"provider_event_id,omitempty"`
	SettlementRef      *string    `json:"settlement_ref,omitempty"`
	FailureReason      *string    `json:"failure_reason,omitempty"`
	OccurredAt         *time.Time `json:"occurred_at,omitempty"`
}

func (h *PaymentHandler) RecordProviderOutcome(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, domain.PrefixPaymentAttempt, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	principal, ok := h.sellerPrincipal(w, r, ActionPaymentOutcomeRecord)
	if !ok {
		return
	}
	var req recordOutcomeRequest
	raw, ok := readBody(w, r, &req, false)
	if !ok {
		return
	}
	occurredAt := h.now()
	if req.OccurredAt != nil {
		occurredAt = req.OccurredAt.UTC().Truncate(time.Microsecond)
	}
	domainReq := domain.RecordOutcomeRequest{
		AttemptID: id, Outcome: domain.PaymentAttemptStatus(req.Outcome), ProviderAttemptRef: req.ProviderAttemptRef,
		ProviderEventID: req.ProviderEventID, SettlementRef: req.SettlementRef, FailureReason: req.FailureReason,
		OccurredAt: occurredAt, ActorPrincipalID: principal,
	}
	if err := domainReq.Validate(); err != nil {
		h.fail(w, r, err)
		return
	}
	cmd, ok := commandFor(w, r, domain.SellerScope, principal, "RecordProviderOutcome", id, raw, false)
	if !ok {
		return
	}
	a, err := h.store.RecordProviderOutcome(r.Context(), domainReq, cmd.claim)
	if err != nil {
		if HandleIdempotentReplay(w, r, err, func(resourceID string) (any, error) {
			return h.store.GetPaymentAttempt(r.Context(), resourceID)
		}) {
			return
		}
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// ── Queries ──────────────────────────────────────────────────────────────────

func (h *PaymentHandler) GetPaymentAttempt(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, domain.PrefixPaymentAttempt, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	org, ok := h.readScope(w, r, r.URL.Query().Get("organization_id"))
	if !ok {
		return
	}
	a, err := h.store.GetPaymentAttempt(svcmiddleware.WithTenant(r.Context(), org), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (h *PaymentHandler) GetPaymentAttempts(w http.ResponseWriter, r *http.Request) {
	invoiceID, ok := parseID(w, r, domain.PrefixInvoice, chi.URLParam(r, "invoiceID"))
	if !ok {
		return
	}
	org, ok := h.readScope(w, r, r.URL.Query().Get("organization_id"))
	if !ok {
		return
	}
	attempts, err := h.store.GetPaymentAttempts(svcmiddleware.WithTenant(r.Context(), org), invoiceID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if attempts == nil {
		attempts = []domain.PaymentAttemptRef{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"invoice_id": invoiceID, "attempts": attempts})
}

func (h *PaymentHandler) GetCollectionState(w http.ResponseWriter, r *http.Request) {
	invoiceID, ok := parseID(w, r, domain.PrefixInvoice, chi.URLParam(r, "invoiceID"))
	if !ok {
		return
	}
	org, ok := h.readScope(w, r, r.URL.Query().Get("organization_id"))
	if !ok {
		return
	}
	state, err := h.store.GetCollectionState(svcmiddleware.WithTenant(r.Context(), org), invoiceID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invoice_id": invoiceID, "collection_state": state})
}
