// Package handler exposes accounts-receivable-svc's REST API.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/domain"
)

// Store is the persistence contract.
type Store interface {
	CreateInvoice(ctx context.Context, inv *domain.CustomerInvoice) error
	GetInvoice(ctx context.Context, invoiceID string) (*domain.CustomerInvoice, error)
	ListInvoices(ctx context.Context, filter domain.ListInvoicesFilter) ([]domain.CustomerInvoice, error)
	TransitionInvoice(ctx context.Context, tenantID, invoiceID string, fromStatus, toStatus domain.InvoiceStatus, actorPrincipalID string) error
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

const (
	actionIssueInvoice   = "AR_INVOICE_ISSUE"
	actionSendInvoice    = "AR_INVOICE_SEND"
	actionMarkOverdue    = "AR_MARK_OVERDUE"
	actionPaymentReceive = "AR_PAYMENT_RECEIVE"
)

type Handler struct {
	store      Store
	publisher  Publisher
	authz      AuthZClient
	httpClient *http.Client
	ledgerURL  string
	log        *zap.Logger
}

func New(store Store, publisher Publisher, authz AuthZClient, ledgerURL string, log *zap.Logger) *Handler {
	return &Handler{
		store:      store,
		publisher:  publisher,
		authz:      authz,
		httpClient: &http.Client{Timeout: 3 * time.Second, Transport: newRetryTransport()},
		ledgerURL:  ledgerURL,
		log:        log,
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
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionIssueInvoice); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	inv := &domain.CustomerInvoice{
		InvoiceID:            uuid.NewString(),
		TenantID:             req.TenantID,
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

	if err := h.store.CreateInvoice(r.Context(), inv); err != nil {
		h.log.Error("CreateInvoice: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}

	h.publisher.PublishInvoiceIssued(r.Context(), *inv)
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
		CustomerID:    q.Get("customer_id"),
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

// ── POST /v1/invoices/{invoice_id}/send ──────────────────────────────────────
func (h *Handler) SendInvoice(w http.ResponseWriter, r *http.Request) {
	invoiceID := chi.URLParam(r, "invoice_id")
	inv, err := h.store.GetInvoice(r.Context(), invoiceID)
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

	if err := h.store.TransitionInvoice(r.Context(), inv.TenantID, invoiceID,
		domain.InvoiceStatusIssued, domain.InvoiceStatusSent, principalID); err != nil {
		h.handleTransitionErr(w, err)
		return
	}

	inv.Status = domain.InvoiceStatusSent
	h.publisher.PublishInvoiceSent(r.Context(), *inv)
	writeJSON(w, http.StatusOK, inv)
}

// ── POST /v1/invoices/{invoice_id}/overdue ───────────────────────────────────
func (h *Handler) MarkOverdue(w http.ResponseWriter, r *http.Request) {
	invoiceID := chi.URLParam(r, "invoice_id")
	inv, err := h.store.GetInvoice(r.Context(), invoiceID)
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

	if err := h.store.TransitionInvoice(r.Context(), inv.TenantID, invoiceID,
		domain.InvoiceStatusSent, domain.InvoiceStatusOverdue, principalID); err != nil {
		h.handleTransitionErr(w, err)
		return
	}

	inv.Status = domain.InvoiceStatusOverdue
	h.publisher.PublishReceivableOverdue(r.Context(), *inv)
	writeJSON(w, http.StatusOK, inv)
}

// ── POST /v1/invoices/{invoice_id}/pay ───────────────────────────────────────
func (h *Handler) ReceivePayment(w http.ResponseWriter, r *http.Request) {
	invoiceID := chi.URLParam(r, "invoice_id")
	inv, err := h.store.GetInvoice(r.Context(), invoiceID)
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

	// Downstream validation check against general-ledger-svc.
	// Requires a finalized journal entry referencing this invoice (correlation_id = invoice_id).
	if err := h.verifyLedgerFinalized(r.Context(), inv.TenantID, inv.LegalEntityID, inv.InvoiceID); err != nil {
		if errors.Is(err, domain.ErrLedgerVerificationFailed) {
			writeError(w, http.StatusBadRequest, "ledger_verification_failed", err.Error())
		} else {
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

	if err := h.store.TransitionInvoice(r.Context(), inv.TenantID, invoiceID,
		fromStatus, domain.InvoiceStatusPaid, principalID); err != nil {
		h.handleTransitionErr(w, err)
		return
	}

	inv.Status = domain.InvoiceStatusPaid
	h.publisher.PublishPaymentReceived(r.Context(), *inv)
	writeJSON(w, http.StatusOK, inv)
}

// ── helpers ──────────────────────────────────────────────────────────────────

type journalHeader struct {
	JournalID     string `json:"journal_id"`
	CorrelationID string `json:"correlation_id"`
	Status        string `json:"status"`
}

func (h *Handler) verifyLedgerFinalized(ctx context.Context, tenantID, legalEntityID, invoiceID string) error {
	params := url.Values{}
	params.Set("tenant_id", tenantID)
	params.Set("legal_entity_id", legalEntityID)
	params.Set("status", "FINALIZED")

	u := fmt.Sprintf("%s/v1/journals?%s", h.ledgerURL, params.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ledger service returned status code %d", resp.StatusCode)
	}

	var journals []journalHeader
	if err := json.NewDecoder(resp.Body).Decode(&journals); err != nil {
		return err
	}

	for _, j := range journals {
		if j.CorrelationID == invoiceID {
			return nil
		}
	}
	return domain.ErrLedgerVerificationFailed
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

func requiredInvoiceFieldMissing(req domain.CreateCustomerInvoiceRequest) string {
	switch {
	case req.TenantID == "":
		return "tenant_id"
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
	default:
		return ""
	}
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
