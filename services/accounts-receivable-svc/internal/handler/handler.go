// Package handler exposes accounts-receivable-svc's REST API.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/entity"
	"zoiko.io/accounts-receivable-svc/internal/ledger"
	svcmiddleware "zoiko.io/accounts-receivable-svc/internal/middleware"
)

// Store is the persistence contract.
//
// Every method takes the caller's verified tenant explicitly. It used to be
// read from the context inside the store, with a fall back to whatever the
// request body or query string had said when the context carried none — so a
// caller who simply omitted X-Tenant-Id chose their own scope. The tenant is
// now resolved once, in one place (requireTenant), and passed down.
type Store interface {
	CreateInvoice(ctx context.Context, inv *domain.CustomerInvoice) (created bool, err error)
	GetInvoice(ctx context.Context, tenantID, invoiceID string) (*domain.CustomerInvoice, error)
	ListInvoices(ctx context.Context, filter domain.ListInvoicesFilter) ([]domain.CustomerInvoice, error)
	TransitionInvoice(ctx context.Context, tenantID, invoiceID string, fromStatus, toStatus domain.InvoiceStatus, actorPrincipalID string) (*domain.CustomerInvoice, error)
}

// Publisher is the event publisher contract.
type Publisher interface {
	PublishInvoiceIssued(ctx context.Context, inv domain.CustomerInvoice)
	PublishInvoiceSent(ctx context.Context, inv domain.CustomerInvoice)
	PublishReceivableOverdue(ctx context.Context, inv domain.CustomerInvoice)
	PublishPaymentReceived(ctx context.Context, inv domain.CustomerInvoice)
}

// AuthZClient is the authorization checker.
type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// LedgerClient verifies that the books account for an invoice before payment is
// recorded against it. See internal/ledger.
type LedgerClient interface {
	Verify(ctx context.Context, tenantID, legalEntityID, invoiceID string, amount float64) error
}

// EntityClient reconciles a caller-supplied legal entity with the caller's
// verified tenant. See internal/entity.
type EntityClient interface {
	VerifyInTenant(ctx context.Context, tenantID, legalEntityID string) error
}

const (
	actionIssueInvoice   = "AR_INVOICE_ISSUE"
	actionSendInvoice    = "AR_INVOICE_SEND"
	actionMarkOverdue    = "AR_MARK_OVERDUE"
	actionPaymentReceive = "AR_PAYMENT_RECEIVE"
)

type Handler struct {
	store     Store
	publisher Publisher
	authz     AuthZClient
	ledger    LedgerClient
	entities  EntityClient
	log       *zap.Logger
	// clock is nil in production and set by tests; see (*Handler).now.
	clock func() time.Time
}

// WithClock overrides the clock the overdue check reads. Test-only.
func (h *Handler) WithClock(clock func() time.Time) *Handler {
	h.clock = clock
	return h
}

// New builds the handler.
//
// The ledger check used to be an inline method on this type over a bare
// *http.Client and a base URL, and it neither bounded its read nor compared the
// journal's amount to the invoice's. It is now a real client behind an interface
// (internal/ledger), which is also what makes the amount check testable.
func New(
	store Store,
	publisher Publisher,
	authz AuthZClient,
	ledger LedgerClient,
	entities EntityClient,
	log *zap.Logger,
) *Handler {
	return &Handler{
		store:     store,
		publisher: publisher,
		authz:     authz,
		ledger:    ledger,
		entities:  entities,
		log:       log,
	}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/invoices", func(r chi.Router) {
		r.Post("/", h.CreateInvoice)
		r.Get("/", h.ListInvoices)
		r.Get("/{invoice_id}", h.GetInvoice)
		r.Post("/{invoice_id}/send", h.SendInvoice)
		r.Post("/{invoice_id}/overdue", h.MarkOverdue)
		r.Post("/{invoice_id}/pay", h.ReceivePayment)
	})
}

// ── POST /v1/invoices ────────────────────────────────────────────────────────
func (h *Handler) CreateInvoice(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateCustomerInvoiceRequest
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
	// scope. It used to be the ONLY source of the stored tenant_id, so a body
	// naming another tenant inserted the receivable into that tenant's
	// register — and the RLS policy that should have refused the insert never
	// ran, because the table was not FORCEd and the service connects as its
	// owner. Migration 000003 closes that half; this closes the other.
	if req.TenantID != "" && req.TenantID != tenantID {
		writeError(w, http.StatusForbidden, "tenant_scope_mismatch", domain.ErrTenantScopeMismatch.Error())
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	// The legal entity must belong to the caller's tenant, and this is checked
	// BEFORE authorization rather than after.
	//
	// Order matters. Authority is granted per legal entity, so a principal holding
	// a grant on an entity in another tenant would otherwise pass the authz check
	// and raise an invoice attributed to that entity while the row itself is filed
	// under their own tenant — a receivable whose two halves name different
	// tenants. Reconciling first also means an out-of-tenant entity is refused
	// without disclosing, via the authorization answer, whether the caller happens
	// to hold a grant on it.
	if err := h.entities.VerifyInTenant(r.Context(), tenantID, req.LegalEntityID); err != nil {
		h.writeEntityErr(w, err, req.LegalEntityID, tenantID)
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionIssueInvoice); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	inv := &domain.CustomerInvoice{
		InvoiceID:            uuid.NewString(),
		TenantID:             tenantID,
		LegalEntityID:        req.LegalEntityID,
		CustomerID:           req.CustomerID,
		InvoiceNumber:        req.InvoiceNumber,
		Amount:               req.Amount,
		CurrencyCode:         req.CurrencyCode,
		DueDate:              req.DueDate,
		Status:               domain.InvoiceStatusIssued,
		CreatedByPrincipalID: principalID,
		CorrelationID:        req.CorrelationID,
	}

	created, err := h.store.CreateInvoice(r.Context(), inv)
	if err != nil {
		// A re-keyed invoice number is the caller's mistake and has a remedy they
		// can act on. It used to arrive here indistinguishable from a dead
		// database and answer 503 — the same defect accounts-payable-svc had.
		if errors.Is(err, domain.ErrDuplicateInvoiceNumber) {
			writeError(w, http.StatusConflict, "duplicate_invoice_number",
				"this customer already has an invoice with this number for this tenant")
			return
		}
		if errors.Is(err, domain.ErrInvalidIdentifier) {
			writeError(w, http.StatusBadRequest, "invalid_field", domain.ErrInvalidIdentifier.Error())
			return
		}
		h.log.Error("CreateInvoice: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if !created {
		// Replay of a prior request with the same correlation_id — return
		// the original invoice, do not re-publish the issued event.
		writeJSON(w, http.StatusOK, inv)
		return
	}

	h.publisher.PublishInvoiceIssued(r.Context(), *inv)
	writeJSON(w, http.StatusCreated, inv)
}

// ── GET /v1/invoices/{invoice_id} ────────────────────────────────────────────
func (h *Handler) GetInvoice(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	invoiceID := chi.URLParam(r, "invoice_id")
	inv, err := h.store.GetInvoice(r.Context(), tenantID, invoiceID)
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
// The register is scoped to the caller's VERIFIED tenant, never to a tenant_id
// they supply. This route used to build its filter from ?tenant_id= and hand
// that value to the store, which both filtered on it and set app.tenant_id from
// it — so `?tenant_id=<any-uuid>` returned that tenant's entire receivables
// register, satisfying the RLS policy on the way past. Nothing consulted
// X-Tenant-Id at all.
//
// Reads are scoped rather than authorized, which is the settled posture of the
// two services either side of this one in the Finance domain —
// general-ledger-svc (whose journals this service verifies against) and
// bank-reconciliation-svc. There is deliberately no AR_INVOICE_VIEW action.
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
	// An unrecognised status matched no row and returned an empty list, so a
	// typo was indistinguishable from a tenant with no invoices. Refused
	// instead — the same reasoning as general-ledger-svc's ?limit.
	if status := q.Get("status"); status != "" && !domain.ValidInvoiceStatus(status) {
		writeError(w, http.StatusBadRequest, "invalid_field", domain.ErrInvalidStatusFilter.Error())
		return
	}
	// legal_entity_id is compared as `legal_entity_id::text = $n`, casting the
	// COLUMN to text rather than the parameter to uuid — so a malformed value does
	// not error, it silently matches nothing, and an empty register reads as "this
	// entity has no invoices". Refused here instead. The console was working around
	// this by validating the filter itself before sending it; the check belongs in
	// the service, where a direct caller is held to it too.
	legalEntityID := q.Get("legal_entity_id")
	if legalEntityID != "" && !isUUID(legalEntityID) {
		writeError(w, http.StatusBadRequest, "invalid_field", "legal_entity_id must be a UUID")
		return
	}
	limit, offset, ok := parsePaging(w, q)
	if !ok {
		return
	}
	filter := domain.ListInvoicesFilter{
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		CustomerID:    q.Get("customer_id"),
		Status:        q.Get("status"),
		Limit:         limit,
		Offset:        offset,
	}
	invoices, err := h.store.ListInvoices(r.Context(), filter)
	if err != nil {
		// legal_entity_id is compared as text, so the verified tenant is the only
		// uuid comparison left here. A gateway that forwarded a non-UUID tenant
		// scope is a fault worth naming rather than reporting as a dead store.
		if errors.Is(err, domain.ErrInvalidIdentifier) {
			writeError(w, http.StatusBadRequest, "invalid_field", "tenant scope must be a UUID")
			return
		}
		h.log.Error("ListInvoices: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	// A nil slice marshals to JSON null, which every caller then has to
	// special-case. An empty register is an empty list.
	if invoices == nil {
		invoices = []domain.CustomerInvoice{}
	}
	writeJSON(w, http.StatusOK, invoices)
}

// ── POST /v1/invoices/{invoice_id}/send ──────────────────────────────────────
func (h *Handler) SendInvoice(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	invoiceID := chi.URLParam(r, "invoice_id")
	inv, err := h.store.GetInvoice(r.Context(), tenantID, invoiceID)
	if err != nil {
		h.log.Error("SendInvoice: store unavailable", zap.Error(err))
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
	if err := h.authz.CheckAllowed(r.Context(), principalID, inv.LegalEntityID, actionSendInvoice); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	// The UPDATE's own RETURNING, not the invoice read a moment ago with `.Status`
	// patched by hand. That patched copy carried sent_at: null and
	// sent_by_principal_id: null for the hop it was reporting.
	sent, err := h.store.TransitionInvoice(r.Context(), tenantID, invoiceID,
		domain.InvoiceStatusIssued, domain.InvoiceStatusSent, principalID)
	if err != nil {
		h.handleTransitionErr(w, err)
		return
	}

	h.publisher.PublishInvoiceSent(r.Context(), *sent)
	writeJSON(w, http.StatusOK, sent)
}

// ── POST /v1/invoices/{invoice_id}/overdue ───────────────────────────────────
func (h *Handler) MarkOverdue(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	invoiceID := chi.URLParam(r, "invoice_id")
	inv, err := h.store.GetInvoice(r.Context(), tenantID, invoiceID)
	if err != nil {
		h.log.Error("MarkOverdue: store unavailable", zap.Error(err))
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
	if err := h.authz.CheckAllowed(r.Context(), principalID, inv.LegalEntityID, actionMarkOverdue); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	// An invoice is late only once its due date has gone by. Nothing checked
	// this, so a SENT invoice could be marked OVERDUE the moment it was sent —
	// and receivable.overdue is not a display concern: it is what aging and
	// impairment downstream count. due_date is a DATE, i.e. the last day on
	// which payment is still on time, so the invoice turns overdue at the start
	// of the following day.
	if overdueFrom := inv.DueDate.AddDate(0, 0, 1); h.now().Before(overdueFrom) {
		writeError(w, http.StatusUnprocessableEntity, "not_yet_due", domain.ErrNotYetDue.Error())
		return
	}

	overdue, err := h.store.TransitionInvoice(r.Context(), tenantID, invoiceID,
		domain.InvoiceStatusSent, domain.InvoiceStatusOverdue, principalID)
	if err != nil {
		h.handleTransitionErr(w, err)
		return
	}

	h.publisher.PublishReceivableOverdue(r.Context(), *overdue)
	writeJSON(w, http.StatusOK, overdue)
}

// ── POST /v1/invoices/{invoice_id}/pay ───────────────────────────────────────
func (h *Handler) ReceivePayment(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	invoiceID := chi.URLParam(r, "invoice_id")
	inv, err := h.store.GetInvoice(r.Context(), tenantID, invoiceID)
	if err != nil {
		h.log.Error("ReceivePayment: store unavailable", zap.Error(err))
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
	if err := h.authz.CheckAllowed(r.Context(), principalID, inv.LegalEntityID, actionPaymentReceive); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	// The books must already account for this invoice, FOR ITS AMOUNT.
	//
	// This used to accept any FINALIZED journal correlated to the invoice without
	// looking at the figure, so a journal for a penny discharged a receivable for
	// any sum — the gate existed and measured nothing. See internal/ledger for what
	// it now proves and, as importantly, what it still cannot (currency, and which
	// accounts were moved).
	if err := h.ledger.Verify(r.Context(), tenantID, inv.LegalEntityID, inv.InvoiceID, inv.Amount); err != nil {
		switch {
		case errors.Is(err, ledger.ErrAmountMismatch):
			// Separated from "no journal" because the remedy is different: an entry
			// exists and disagrees, which is a bookkeeping error somebody has to
			// look at rather than a missing posting.
			h.log.Warn("payment refused: ledger disagrees with the invoice amount",
				zap.String("invoice_id", inv.InvoiceID), zap.Error(err))
			writeError(w, http.StatusBadRequest, "ledger_amount_mismatch", err.Error())
		case errors.Is(err, ledger.ErrJournalNotFound):
			writeError(w, http.StatusBadRequest, "ledger_verification_failed", domain.ErrLedgerVerificationFailed.Error())
		default:
			h.log.Error("ledger verification unavailable — failing closed", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "ledger_service_unavailable", "")
		}
		return
	}

	// The transition can occur from either SENT or OVERDUE.
	fromStatus := inv.Status
	if fromStatus != domain.InvoiceStatusSent && fromStatus != domain.InvoiceStatusOverdue {
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", "must be in SENT or OVERDUE status to record payment")
		return
	}

	paid, err := h.store.TransitionInvoice(r.Context(), tenantID, invoiceID,
		fromStatus, domain.InvoiceStatusPaid, principalID)
	if err != nil {
		h.handleTransitionErr(w, err)
		return
	}

	h.publisher.PublishPaymentReceived(r.Context(), *paid)
	writeJSON(w, http.StatusOK, paid)
}

// ── helpers ──────────────────────────────────────────────────────────────────

// Paging bounds for the register.
//
// The read used to be unbounded: every invoice this tenant has ever raised, on
// every dashboard load. maxLimit is a cap on what one request can ask for, and
// defaultLimit is what it gets when it asks for nothing — a default rather than
// "everything", because an unbounded default is an unbounded read that nobody
// wrote down. Both mirror the ranges the rest of this platform's registers use.
const (
	defaultLimit = 100
	maxLimit     = 500
)

// parsePaging reads ?limit and ?offset. An out-of-range value is REFUSED rather
// than silently clamped: a caller who asked for a specific page size and got
// another one has no way to notice, and would read a truncated register as a
// complete one. Same reasoning as general-ledger-svc's ?limit.
func parsePaging(w http.ResponseWriter, q url.Values) (limit, offset int, ok bool) {
	limit = defaultLimit
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > maxLimit {
			writeError(w, http.StatusBadRequest, "invalid_field",
				fmt.Sprintf("limit must be an integer between 1 and %d", maxLimit))
			return 0, 0, false
		}
		limit = n
	}
	if raw := q.Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid_field", "offset must not be negative")
			return 0, 0, false
		}
		offset = n
	}
	return limit, offset, true
}

// isUUID reports whether s parses as a UUID. Used to refuse filter values that
// would otherwise match nothing and read as an empty register.
func isUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}

// writeEntityErr answers a failed legal-entity reconciliation.
//
// "Not found" and "belongs to another tenant" deliberately collapse into ONE
// answer. The caller is not entitled to learn which: distinguishing them turns
// this endpoint into an oracle for whether a given entity id exists in some other
// tenant. The distinction is kept in this service's own log, where it matters,
// because a typo and a cross-tenant attempt want different responses from an
// operator.
func (h *Handler) writeEntityErr(w http.ResponseWriter, err error, legalEntityID, tenantID string) {
	switch {
	case errors.Is(err, entity.ErrForeignTenant):
		h.log.Warn("refused: legal entity belongs to another tenant",
			zap.String("legal_entity_id", legalEntityID), zap.String("caller_tenant_id", tenantID))
		writeError(w, http.StatusForbidden, "legal_entity_not_in_tenant", domain.ErrLegalEntityNotInTenant.Error())
	case errors.Is(err, entity.ErrNotFound):
		h.log.Info("refused: unknown legal entity",
			zap.String("legal_entity_id", legalEntityID), zap.String("caller_tenant_id", tenantID))
		writeError(w, http.StatusForbidden, "legal_entity_not_in_tenant", domain.ErrLegalEntityNotInTenant.Error())
	case errors.Is(err, entity.ErrNotActive):
		writeError(w, http.StatusUnprocessableEntity, "legal_entity_not_active", err.Error())
	default:
		// Fails closed, like every other cross-service dependency here: an invoice
		// whose attribution could not be checked is not written.
		h.log.Error("legal entity reconciliation unavailable — failing closed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "entity_registry_unavailable", "")
	}
}

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

// tenant_id is deliberately NOT required here. It comes from the verified
// X-Tenant-Id scope, and is only checked for agreement when the body carries it
// (see CreateInvoice) — demanding it would ask the caller for a value they are
// not allowed to choose.
func requiredInvoiceFieldMissing(req domain.CreateCustomerInvoiceRequest) string {
	switch {
	case req.LegalEntityID == "":
		return "legal_entity_id"
	case req.CustomerID == "":
		return "customer_id"
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

// requireTenant reads the caller's verified tenant scope from X-Tenant-Id, set
// by gateway-auth-svc's ForwardAuth check and carried into the context by
// TenantContext. Absent, there is no honest answer to give: the two reads used
// to treat a missing scope as "every tenant" (the register) and as "not found"
// (a single invoice), and both writes fell back to the tenant named in the
// request body.
func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		writeError(w, http.StatusUnauthorized, "tenant_scope_missing", domain.ErrTenantScopeMissing.Error())
		return "", false
	}
	return tenantID, true
}

// now is the clock the overdue check reads, injectable so a test can place an
// invoice either side of its due date without sleeping.
func (h *Handler) now() time.Time {
	if h.clock != nil {
		return h.clock()
	}
	return time.Now().UTC()
}

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
