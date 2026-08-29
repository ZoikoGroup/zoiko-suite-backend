// Package handler exposes goods-service-receipt-svc's REST API — AP-04.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/goods-service-receipt-svc/internal/authz"
	"zoiko.io/goods-service-receipt-svc/internal/domain"
	"zoiko.io/goods-service-receipt-svc/internal/events"
	"zoiko.io/goods-service-receipt-svc/internal/ledger"
	svcmiddleware "zoiko.io/goods-service-receipt-svc/internal/middleware"
	"zoiko.io/goods-service-receipt-svc/internal/purchaseorder"
	"zoiko.io/goods-service-receipt-svc/internal/store"
)

// Action constants — AP-04's own contract's "Authorization / permissions"
// line ("receipt.read/create/confirm/reverse; service.accept"), adapted to
// this platform's SCREAMING_SNAKE_CASE convention (see
// master-register-findings-2026-08-27.md §2.5). RejectReceipt and
// AmendReceiptDraft/AttachReceiptEvidence are not separately named in the
// spec's permission list — Reject reuses the Confirm action (both are the
// decision on a pending receipt) and the draft-stage mutations reuse the
// Create action.
const (
	ReceiptRead    = "GOODS_SERVICE_RECEIPT_READ"
	ReceiptCreate  = "GOODS_SERVICE_RECEIPT_CREATE"
	ReceiptConfirm = "GOODS_SERVICE_RECEIPT_CONFIRM"
	ReceiptReverse = "GOODS_SERVICE_RECEIPT_REVERSE"
	ServiceAccept  = "SERVICE_ACCEPT"
)

// AuthzChecker is the real dependency on authorization-svc, including its
// dynamic own-object SoD layer — see internal/authz's package doc comment.
type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
	CheckAllowedOwnObject(ctx context.Context, principalID, legalEntityID, actionType, resourceOwnerPrincipalID string) error
}

// Config is the subset of internal/config the handler needs.
type Config struct {
	GRNIDebitAccountCode    string
	GRNICreditAccountCode   string
	OverReceiptTolerancePct float64
}

type Handler struct {
	store  store.Store
	pub    events.Publisher
	authz  AuthzChecker
	po     purchaseorder.Client
	ledger ledger.Client
	cfg    Config
	log    *zap.Logger
}

func New(st store.Store, pub events.Publisher, az AuthzChecker, po purchaseorder.Client, gl ledger.Client, cfg Config, log *zap.Logger) *Handler {
	return &Handler{store: st, pub: pub, authz: az, po: po, ledger: gl, cfg: cfg, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/ap04/receipts", func(r chi.Router) {
		r.Post("/", h.CreateReceipt)
		r.Get("/{receiptID}", h.GetReceipt)
		r.Post("/{receiptID}/amend", h.AmendReceiptDraft)
		r.Post("/{receiptID}/confirm", h.ConfirmReceipt)
		r.Post("/{receiptID}/reject", h.RejectReceipt)
		r.Post("/{receiptID}/reverse", h.ReverseReceipt)
		r.Post("/{receiptID}/service-acceptance", h.RecordServiceAcceptance)
		r.Post("/{receiptID}/evidence", h.AttachReceiptEvidence)
		r.Get("/{receiptID}/evidence", h.ListReceiptEvidence)
		r.Get("/{receiptID}/available-actions", h.GetAvailableActions)
		r.Get("/{receiptID}/accounting-status", h.GetReceiptAccountingStatus)
	})
	r.Route("/ap04/purchase-orders/{purchaseOrderID}", func(r chi.Router) {
		r.Get("/receipts", h.ListReceiptsForPO)
		r.Get("/received-to-date", h.GetReceivedToDate)
	})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, legalEntityID, actionType string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionType); err != nil {
		return h.handleAuthzErr(w, err)
	}
	return true
}

func (h *Handler) handleAuthzErr(w http.ResponseWriter, err error) bool {
	if errors.Is(err, authzpkg.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "not authorized to perform this action")
		return false
	}
	h.log.Error("authorization check failed", zap.Error(err))
	writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
	return false
}

func (h *Handler) fetchReceiptForAuth(w http.ResponseWriter, r *http.Request, receiptID string) (*domain.GoodsServiceReceipt, bool) {
	rcpt, err := h.store.FindReceipt(r.Context(), receiptID)
	if err != nil {
		if errors.Is(err, domain.ErrReceiptNotFound) {
			writeError(w, http.StatusNotFound, "goods/service receipt not found")
			return nil, false
		}
		h.log.Error("fetchReceiptForAuth: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return nil, false
	}
	return rcpt, true
}

func (h *Handler) writePurchaseOrderErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrPurchaseOrderNotFound):
		writeError(w, http.StatusBadRequest, "purchase order not found")
	case errors.Is(err, domain.ErrPurchaseOrderMismatch):
		writeError(w, http.StatusForbidden, "purchase order does not belong to the caller's tenant/legal entity")
	case errors.Is(err, domain.ErrPurchaseOrderNotOpen):
		writeError(w, http.StatusConflict, "purchase order is closed")
	default:
		h.log.Error("purchase-order-svc lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "purchase-order-svc unavailable")
	}
}

// ── receipts ─────────────────────────────────────────────────────────────────

func (h *Handler) CreateReceipt(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateReceiptRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.PurchaseOrderID == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id and purchase_order_id are required")
		return
	}
	if !domain.ValidReceiptType(req.ReceiptType) {
		writeError(w, http.StatusBadRequest, "receipt_type must be GOODS or SERVICE")
		return
	}
	if req.Amount <= 0 || req.Quantity <= 0 {
		writeError(w, http.StatusBadRequest, "amount and quantity must be positive")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, req.LegalEntityID, ReceiptCreate) {
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())

	// Negative-path scenario #2's create-time half: a receipt can never be
	// opened against a PO that isn't real, isn't the caller's, or is
	// already closed.
	if _, err := h.po.GetOpenOrder(r.Context(), verifiedTenant, req.LegalEntityID, req.PurchaseOrderID); err != nil {
		h.writePurchaseOrderErr(w, err)
		return
	}

	rcpt, err := h.store.CreateReceipt(r.Context(), verifiedTenant, req, principalID)
	if err != nil {
		h.log.Error("CreateReceipt: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: domain.EventReceiptCreated, EntityID: rcpt.ReceiptID, TenantID: verifiedTenant,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: rcpt,
	})
	writeJSON(w, http.StatusCreated, rcpt)
}

func (h *Handler) GetReceipt(w http.ResponseWriter, r *http.Request) {
	receiptID := chi.URLParam(r, "receiptID")
	rcpt, ok := h.fetchReceiptForAuth(w, r, receiptID)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, rcpt)
}

func (h *Handler) ListReceiptsForPO(w http.ResponseWriter, r *http.Request) {
	purchaseOrderID := chi.URLParam(r, "purchaseOrderID")
	receipts, err := h.store.ListReceiptsForPO(r.Context(), purchaseOrderID)
	if err != nil {
		h.log.Error("ListReceiptsForPO: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if receipts == nil {
		receipts = []domain.GoodsServiceReceipt{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": receipts, "count": len(receipts)})
}

func (h *Handler) AmendReceiptDraft(w http.ResponseWriter, r *http.Request) {
	receiptID := chi.URLParam(r, "receiptID")
	var req domain.AmendReceiptDraftRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	existing, ok := h.fetchReceiptForAuth(w, r, receiptID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, existing.LegalEntityID, ReceiptCreate) {
		return
	}

	rcpt, err := h.store.AmendReceiptDraft(r.Context(), receiptID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "receipt is not in the required DRAFT state")
			return
		}
		h.log.Error("AmendReceiptDraft: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, rcpt)
}

// ConfirmReceipt handles POST .../confirm. Enforces, for real: negative-path
// #1 (over-tolerance without an approved exception, checked against a live
// SumNetConfirmedAmountForPO aggregate — see internal/domain's package doc
// for why this is amount- rather than line-level), negative-path #2's
// confirm-time half (a live re-check that the PO is still open — it may
// have closed between CreateReceipt and now), and the SoD line ("receiver
// cannot self-certify sensitive services where policy requires independent
// acceptance") via authorization-svc's own-object layer. GRNI posting is
// attempted afterwards, best-effort — see postGRNI.
func (h *Handler) ConfirmReceipt(w http.ResponseWriter, r *http.Request) {
	receiptID := chi.URLParam(r, "receiptID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	existing, ok := h.fetchReceiptForAuth(w, r, receiptID)
	if !ok {
		return
	}
	if !domain.CanConfirm(existing.Status) {
		writeError(w, http.StatusConflict, "receipt is not in a confirmable state")
		return
	}

	if existing.RequiresIndependentAcceptance {
		if err := h.authz.CheckAllowedOwnObject(r.Context(), principalID, existing.LegalEntityID, ReceiptConfirm, existing.ReceiverPrincipalID); err != nil {
			h.handleAuthzErr(w, err)
			return
		}
	} else if !h.authorize(w, r, principalID, existing.LegalEntityID, ReceiptConfirm) {
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())

	po, err := h.po.GetOpenOrder(r.Context(), verifiedTenant, existing.LegalEntityID, existing.PurchaseOrderID)
	if err != nil {
		h.writePurchaseOrderErr(w, err)
		return
	}

	// Negative-path scenario #1: block an over-tolerance receipt unless an
	// approved exception was recorded at CreateReceipt time.
	if existing.ToleranceExceptionRef == "" {
		netToDate, err := h.store.SumNetConfirmedAmountForPO(r.Context(), existing.PurchaseOrderID)
		if err != nil {
			h.log.Error("ConfirmReceipt: tolerance check failed", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store unavailable")
			return
		}
		ceiling := po.TotalAmount * (1 + h.cfg.OverReceiptTolerancePct)
		if netToDate+existing.Amount > ceiling+0.0001 {
			writeError(w, http.StatusConflict, "receipt amount exceeds purchase order tolerance without an approved exception")
			return
		}
	}

	rcpt, err := h.store.ConfirmReceipt(r.Context(), receiptID, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "receipt is not in a confirmable state")
			return
		}
		h.log.Error("ConfirmReceipt: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: domain.EventReceiptConfirmed, EntityID: rcpt.ReceiptID, TenantID: verifiedTenant,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: rcpt,
	})

	acctEvent := h.postGRNI(r.Context(), verifiedTenant, rcpt, principalID)
	writeJSON(w, http.StatusOK, map[string]interface{}{"receipt": rcpt, "accounting_event": acctEvent})
}

// postGRNI is deliberately best-effort — see internal/ledger's package doc
// and internal/domain's package doc. Any failure is recorded as an
// EXCEPTION accounting event and returned to the caller for visibility;
// it never undoes the receipt confirmation that already succeeded.
func (h *Handler) postGRNI(ctx context.Context, tenantID string, rcpt *domain.GoodsServiceReceipt, principalID string) *domain.ReceiptAccountingEvent {
	journalID, err := h.ledger.PostGRNI(ctx, ledger.PostGRNIParams{
		TenantID: tenantID, LegalEntityID: rcpt.LegalEntityID, CorrelationID: rcpt.ReceiptID,
		PrincipalID: principalID, Amount: rcpt.Amount,
		DebitAccountCode: h.cfg.GRNIDebitAccountCode, CreditAccountCode: h.cfg.GRNICreditAccountCode,
		Description: "GRNI accrual for goods/service receipt " + rcpt.ReceiptID,
	})
	var event *domain.ReceiptAccountingEvent
	if err != nil {
		h.log.Warn("GRNI posting failed — recording EXCEPTION accounting event, receipt confirmation stands", zap.Error(err))
		event, err = h.store.RecordAccountingEvent(ctx, rcpt.ReceiptID, domain.AccountingException, nil, err.Error())
	} else {
		jid := journalID
		event, err = h.store.RecordAccountingEvent(ctx, rcpt.ReceiptID, domain.AccountingPosted, &jid, "")
	}
	if err != nil {
		h.log.Error("failed to record accounting event", zap.Error(err))
		return nil
	}
	return event
}

func (h *Handler) RejectReceipt(w http.ResponseWriter, r *http.Request) {
	receiptID := chi.URLParam(r, "receiptID")
	var req domain.RejectReceiptRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "reason is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	existing, ok := h.fetchReceiptForAuth(w, r, receiptID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, existing.LegalEntityID, ReceiptConfirm) {
		return
	}

	rcpt, err := h.store.RejectReceipt(r.Context(), receiptID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "receipt is not in a rejectable state")
			return
		}
		h.log.Error("RejectReceipt: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, rcpt)
}

// ReverseReceipt handles POST .../reverse — the platform's only sanctioned
// correction mechanism for a confirmed receipt, directly enforcing
// negative-path scenario #3 ("confirmed receipt deleted to fix mismatch"
// must be blocked; the immutability trigger blocks DELETE unconditionally,
// and this is the real correction path instead).
func (h *Handler) ReverseReceipt(w http.ResponseWriter, r *http.Request) {
	receiptID := chi.URLParam(r, "receiptID")
	var req domain.ReverseReceiptRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ReversedAmount <= 0 || req.Reason == "" {
		writeError(w, http.StatusBadRequest, "reversed_amount (positive) and reason are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	existing, ok := h.fetchReceiptForAuth(w, r, receiptID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, existing.LegalEntityID, ReceiptReverse) {
		return
	}

	rcpt, err := h.store.ReverseReceipt(r.Context(), receiptID, req, principalID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrOverReversal):
			writeError(w, http.StatusConflict, "reversal amount exceeds remaining unreversed receipt amount")
		case errors.Is(err, domain.ErrInvalidTransition):
			writeError(w, http.StatusConflict, "receipt is not in a reversible state")
		default:
			h.log.Error("ReverseReceipt: store unavailable", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store unavailable")
		}
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: domain.EventReceiptReversed, EntityID: rcpt.ReceiptID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: rcpt,
	})
	writeJSON(w, http.StatusOK, rcpt)
}

// RecordServiceAcceptance handles POST .../service-acceptance. When the
// receipt is flagged RequiresIndependentAcceptance, the accepting principal
// is checked against authorization-svc's own-object SoD layer with the
// original receiver as resource_owner_principal_id — the direct
// enforcement of AP-04's SoD line ("receiver/acceptor cannot self-certify
// sensitive services where policy requires independent acceptance").
func (h *Handler) RecordServiceAcceptance(w http.ResponseWriter, r *http.Request) {
	receiptID := chi.URLParam(r, "receiptID")
	var req domain.RecordServiceAcceptanceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	existing, ok := h.fetchReceiptForAuth(w, r, receiptID)
	if !ok {
		return
	}
	if existing.ReceiptType != domain.ReceiptTypeService {
		writeError(w, http.StatusBadRequest, "service acceptance only applies to SERVICE receipts")
		return
	}

	if existing.RequiresIndependentAcceptance {
		if err := h.authz.CheckAllowedOwnObject(r.Context(), principalID, existing.LegalEntityID, ServiceAccept, existing.ReceiverPrincipalID); err != nil {
			h.handleAuthzErr(w, err)
			return
		}
	} else if !h.authorize(w, r, principalID, existing.LegalEntityID, ServiceAccept) {
		return
	}

	rcpt, err := h.store.RecordServiceAcceptance(r.Context(), receiptID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "receipt is not in the required DRAFT state")
			return
		}
		h.log.Error("RecordServiceAcceptance: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: domain.EventServiceAcceptanceRecorded, EntityID: rcpt.ReceiptID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: rcpt,
	})
	writeJSON(w, http.StatusOK, rcpt)
}

// ── evidence ─────────────────────────────────────────────────────────────────

func (h *Handler) AttachReceiptEvidence(w http.ResponseWriter, r *http.Request) {
	receiptID := chi.URLParam(r, "receiptID")
	var req domain.AttachReceiptEvidenceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.EvidenceRef == "" {
		writeError(w, http.StatusBadRequest, "evidence_ref is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	existing, ok := h.fetchReceiptForAuth(w, r, receiptID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, existing.LegalEntityID, ReceiptCreate) {
		return
	}

	e, err := h.store.AttachReceiptEvidence(r.Context(), receiptID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrReceiptNotFound) {
			writeError(w, http.StatusNotFound, "goods/service receipt not found")
			return
		}
		h.log.Error("AttachReceiptEvidence: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusCreated, e)
}

func (h *Handler) ListReceiptEvidence(w http.ResponseWriter, r *http.Request) {
	receiptID := chi.URLParam(r, "receiptID")
	evidence, err := h.store.ListReceiptEvidence(r.Context(), receiptID)
	if err != nil {
		h.log.Error("ListReceiptEvidence: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if evidence == nil {
		evidence = []domain.ReceiptEvidence{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": evidence, "count": len(evidence)})
}

// ── queries ──────────────────────────────────────────────────────────────────

func (h *Handler) GetAvailableActions(w http.ResponseWriter, r *http.Request) {
	receiptID := chi.URLParam(r, "receiptID")
	rcpt, ok := h.fetchReceiptForAuth(w, r, receiptID)
	if !ok {
		return
	}
	var actions []string
	if domain.CanAmendDraft(rcpt.Status) {
		actions = append(actions, "AmendReceiptDraft")
	}
	if domain.CanConfirm(rcpt.Status) {
		actions = append(actions, "ConfirmReceipt")
	}
	if domain.CanReject(rcpt.Status) {
		actions = append(actions, "RejectReceipt")
	}
	if domain.CanReverse(rcpt.Status) {
		actions = append(actions, "ReverseReceipt")
	}
	if rcpt.Status == domain.StatusDraft && rcpt.ReceiptType == domain.ReceiptTypeService {
		actions = append(actions, "RecordServiceAcceptance")
	}
	if actions == nil {
		actions = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"receipt_id": receiptID, "status": rcpt.Status, "available_actions": actions})
}

func (h *Handler) GetReceiptAccountingStatus(w http.ResponseWriter, r *http.Request) {
	receiptID := chi.URLParam(r, "receiptID")
	if _, ok := h.fetchReceiptForAuth(w, r, receiptID); !ok {
		return
	}
	event, err := h.store.GetLatestAccountingEvent(r.Context(), receiptID)
	if err != nil {
		h.log.Error("GetReceiptAccountingStatus: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if event == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"receipt_id": receiptID, "status": "NOT_APPLICABLE"})
		return
	}
	writeJSON(w, http.StatusOK, event)
}

// GetReceivedToDate reports the real aggregate net-confirmed-amount against
// purchaseOrderID alongside the PO's own total_amount from purchase-order-
// svc — the amount-level equivalent of "PO open quantity/amount" (see
// internal/domain's package doc for why line/quantity isn't available).
func (h *Handler) GetReceivedToDate(w http.ResponseWriter, r *http.Request) {
	purchaseOrderID := chi.URLParam(r, "purchaseOrderID")
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())

	netToDate, err := h.store.SumNetConfirmedAmountForPO(r.Context(), purchaseOrderID)
	if err != nil {
		h.log.Error("GetReceivedToDate: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	resp := map[string]interface{}{"purchase_order_id": purchaseOrderID, "net_confirmed_amount": netToDate}
	// The PO's own total_amount/status is best-effort enrichment, not an
	// authorization gate (this service's own receipts are already
	// tenant-scoped by RLS) — it needs a legal_entity_id to check against,
	// which this read-only query doesn't otherwise have, so it's borrowed
	// from any receipt already on file against this PO. No receipts yet
	// means no enrichment, not an error.
	if receipts, err := h.store.ListReceiptsForPO(r.Context(), purchaseOrderID); err == nil && len(receipts) > 0 {
		if po, err := h.po.GetOrder(r.Context(), verifiedTenant, receipts[0].LegalEntityID, purchaseOrderID); err == nil {
			resp["po_total_amount"] = po.TotalAmount
			resp["po_status"] = po.Status
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
