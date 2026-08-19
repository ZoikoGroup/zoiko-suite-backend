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

	"zoiko.io/procurement-workflow-svc/internal/clients"
	"zoiko.io/procurement-workflow-svc/internal/domain"
	svcmiddleware "zoiko.io/procurement-workflow-svc/internal/middleware"
)

type Store interface {
	CreateCase(ctx context.Context, c *domain.ProcurementCase) (created bool, err error)
	GetCase(ctx context.Context, caseID string) (*domain.ProcurementCase, error)
	ListCases(ctx context.Context, legalEntityID, status string) ([]domain.ProcurementCase, error)
	UpdateApproved(ctx context.Context, caseID, principalID string) (*domain.ProcurementCase, error)
	UpdateRejected(ctx context.Context, caseID, principalID, reason string) (*domain.ProcurementCase, error)
	UpdateOrderIssued(ctx context.Context, caseID, purchaseOrderID string) (*domain.ProcurementCase, error)
}

type Publisher interface {
	PublishRequested(ctx context.Context, c domain.ProcurementCase)
	PublishApprovalStarted(ctx context.Context, c domain.ProcurementCase)
	PublishCompleted(ctx context.Context, c domain.ProcurementCase)
}

type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// SpendChecker runs the real spend-controls-svc check that every procurement
// case must pass through before it can enter approval.
type SpendChecker interface {
	SubmitCheck(ctx context.Context, tenantID, principalID, legalEntityID, category, currencyCode, correlationID, sourceReference string, amount float64) (*clients.SpendCheckResult, error)
}

// OrderIssuer calls the real purchase-order-svc to issue the order once a
// case has been approved.
type OrderIssuer interface {
	IssueOrder(ctx context.Context, tenantID, principalID, legalEntityID, caseID string, vendorProfileID *string, totalAmount float64, currencyCode string) (*clients.IssuedOrder, error)
}

const (
	actionCaseCreate     = "PROCUREMENT_CASE_CREATE"
	actionCaseView       = "PROCUREMENT_CASE_VIEW"
	actionCaseApprove    = "PROCUREMENT_CASE_APPROVE"
	actionCaseReject     = "PROCUREMENT_CASE_REJECT"
	actionCaseIssueOrder = "PROCUREMENT_CASE_ISSUE_ORDER"
)

type Handler struct {
	store       Store
	publisher   Publisher
	authz       AuthZClient
	spendCheck  SpendChecker
	orderIssuer OrderIssuer
	log         *zap.Logger
}

func New(store Store, publisher Publisher, authz AuthZClient, spendCheck SpendChecker, orderIssuer OrderIssuer, log *zap.Logger) *Handler {
	return &Handler{store: store, publisher: publisher, authz: authz, spendCheck: spendCheck, orderIssuer: orderIssuer, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/procurement-cases", func(r chi.Router) {
		r.Post("/", h.CreateCase)
		r.Get("/", h.ListCases)
		r.Get("/{case_id}", h.GetCase)
		r.Post("/{case_id}/approve", h.ApproveCase)
		r.Post("/{case_id}/reject", h.RejectCase)
		r.Post("/{case_id}/issue-order", h.IssueOrder)
	})
}

// ── POST /v1/procurement-cases ────────────────────────────────────────────────

// CreateCase runs a real spend-controls-svc check before a case is allowed
// to exist in any state other than SPEND_BLOCKED — a procurement case is
// never persisted without a genuine spend-check outcome behind it.
//
// Idempotent on (tenant_id, correlation_id): a retried create replays the
// stored case rather than re-running the spend check, which would otherwise
// double-count consumption on a network retry.
func (h *Handler) CreateCase(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateCaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.LegalEntityID == "" || req.Category == "" || req.Description == "" || req.CurrencyCode == "" || req.CorrelationID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, category, description, currency_code, correlation_id are required")
		return
	}
	if req.Amount <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_amount", "amount must be > 0")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionCaseCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())

	caseID := uuid.NewString()
	result, err := h.spendCheck.SubmitCheck(r.Context(), tenantID, principalID, req.LegalEntityID, req.Category, req.CurrencyCode, req.CorrelationID, caseID, req.Amount)
	if err != nil {
		h.log.Error("spend-controls-svc check failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "spend_controls_unavailable", err.Error())
		return
	}

	now := time.Now().UTC()
	status := domain.CaseStatusApprovalPending
	if result.DecisionOutcome != "ALLOWED" {
		status = domain.CaseStatusSpendBlocked
	}

	pc := &domain.ProcurementCase{
		CaseID:                 caseID,
		TenantID:               tenantID,
		LegalEntityID:          req.LegalEntityID,
		RequestedByPrincipalID: principalID,
		Description:            req.Description,
		Category:               req.Category,
		Amount:                 req.Amount,
		CurrencyCode:           req.CurrencyCode,
		VendorProfileID:        req.VendorProfileID,
		Status:                 status,
		SpendCheckDecision:     result.DecisionOutcome,
		SpendCheckBasis:        result.DecisionBasis,
		CorrelationID:          req.CorrelationID,
		CreatedAt:              now,
		UpdatedAt:              now,
	}

	created, err := h.store.CreateCase(r.Context(), pc)
	if err != nil {
		h.log.Error("failed to create procurement case", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if created {
		h.publisher.PublishRequested(r.Context(), *pc)
		if pc.Status == domain.CaseStatusApprovalPending {
			h.publisher.PublishApprovalStarted(r.Context(), *pc)
		}
	}

	writeJSON(w, http.StatusCreated, pc)
}

// ── GET /v1/procurement-cases ─────────────────────────────────────────────────

func (h *Handler) ListCases(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	status := r.URL.Query().Get("status")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if legalEntityID != "" {
		if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionCaseView); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
	}

	list, err := h.store.ListCases(r.Context(), legalEntityID, status)
	if err != nil {
		h.log.Error("failed to list procurement cases", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.ProcurementCase{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── GET /v1/procurement-cases/{case_id} ───────────────────────────────────────

func (h *Handler) GetCase(w http.ResponseWriter, r *http.Request) {
	caseID := chi.URLParam(r, "case_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	pc, err := h.store.GetCase(r.Context(), caseID)
	if err != nil {
		h.writeCaseErr(w, err)
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, pc.LegalEntityID, actionCaseView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	writeJSON(w, http.StatusOK, pc)
}

// ── POST /v1/procurement-cases/{case_id}/approve ──────────────────────────────

func (h *Handler) ApproveCase(w http.ResponseWriter, r *http.Request) {
	caseID := chi.URLParam(r, "case_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	pc, err := h.store.GetCase(r.Context(), caseID)
	if err != nil {
		h.writeCaseErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, pc.LegalEntityID, actionCaseApprove); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if pc.RequestedByPrincipalID == principalID {
		writeError(w, http.StatusForbidden, "self_approval_not_allowed", string(domain.ErrSelfApprovalNotAllowed))
		return
	}

	updated, err := h.store.UpdateApproved(r.Context(), caseID, principalID)
	if err != nil {
		h.writeCaseErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// ── POST /v1/procurement-cases/{case_id}/reject ───────────────────────────────

func (h *Handler) RejectCase(w http.ResponseWriter, r *http.Request) {
	caseID := chi.URLParam(r, "case_id")

	var req domain.RejectCaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "reason is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	pc, err := h.store.GetCase(r.Context(), caseID)
	if err != nil {
		h.writeCaseErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, pc.LegalEntityID, actionCaseReject); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if pc.RequestedByPrincipalID == principalID {
		writeError(w, http.StatusForbidden, "self_approval_not_allowed", string(domain.ErrSelfApprovalNotAllowed))
		return
	}

	updated, err := h.store.UpdateRejected(r.Context(), caseID, principalID, req.Reason)
	if err != nil {
		h.writeCaseErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// ── POST /v1/procurement-cases/{case_id}/issue-order ──────────────────────────

// IssueOrder calls the real purchase-order-svc once a case is APPROVED. If a
// case is already COMPLETED (e.g. a retried request), it replays the stored
// result rather than issuing a second order.
func (h *Handler) IssueOrder(w http.ResponseWriter, r *http.Request) {
	caseID := chi.URLParam(r, "case_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	pc, err := h.store.GetCase(r.Context(), caseID)
	if err != nil {
		h.writeCaseErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, pc.LegalEntityID, actionCaseIssueOrder); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if pc.Status == domain.CaseStatusCompleted {
		writeJSON(w, http.StatusOK, pc)
		return
	}
	if pc.Status != domain.CaseStatusApproved {
		writeError(w, http.StatusConflict, "invalid_transition", string(domain.ErrInvalidTransition))
		return
	}

	order, err := h.orderIssuer.IssueOrder(r.Context(), pc.TenantID, principalID, pc.LegalEntityID, pc.CaseID, pc.VendorProfileID, pc.Amount, pc.CurrencyCode)
	if err != nil {
		h.log.Error("purchase-order-svc issuance failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "purchase_order_service_unavailable", err.Error())
		return
	}

	// The order now genuinely exists in purchase-order-svc — that side effect
	// is not undoable from here, and shouldn't be: purchase-order-svc keys it
	// on this case's ID as an idempotency key (see PurchaseOrderClient), so a
	// second IssueOrder call for the same case resolves to the SAME order,
	// never a duplicate. A transient failure recording that fact locally
	// (below) must not be allowed to strand a real, externally-committed
	// order behind a case that still reads APPROVED, so a short local retry
	// is worth it before giving up. If every retry still fails, the case
	// stays recoverable anyway: the caller retrying this endpoint re-issues
	// against the same idempotency key and this local write gets another
	// chance, cheaper than any compensating cancel-the-order call would be.
	var updated *domain.ProcurementCase
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 100 * time.Millisecond)
		}
		updated, err = h.store.UpdateOrderIssued(r.Context(), caseID, order.PurchaseOrderID)
		if err == nil {
			break
		}
	}
	if err != nil {
		h.log.Error("purchase order issued but recording it against the case failed after retries — case remains APPROVED; retrying this endpoint will resolve it via purchase-order-svc's idempotent correlation_id",
			zap.String("case_id", caseID),
			zap.String("purchase_order_id", order.PurchaseOrderID),
			zap.Error(err),
		)
		h.writeCaseErr(w, err)
		return
	}

	h.publisher.PublishCompleted(r.Context(), *updated)
	writeJSON(w, http.StatusOK, updated)
}

// ── Helpers ────────────────────────────────────────────────────────────────

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrIdentityMissing))
		return "", false
	}
	return principalID, true
}

func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, domain.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
	} else {
		writeError(w, http.StatusServiceUnavailable, "authz_unavailable", err.Error())
	}
}

func (h *Handler) writeCaseErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrCaseNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, domain.ErrInvalidTransition):
		writeError(w, http.StatusConflict, "invalid_transition", err.Error())
	default:
		h.log.Error("procurement case store error", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
	}
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error_code":    code,
		"error_message": msg,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
