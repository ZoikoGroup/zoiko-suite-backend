// Package handler exposes goods-service-receipt-svc's REST API — AP-04.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authzpkg "zoiko.io/goods-service-receipt-svc/internal/authz"
	"zoiko.io/goods-service-receipt-svc/internal/domain"
	svcmiddleware "zoiko.io/goods-service-receipt-svc/internal/middleware"
	"zoiko.io/goods-service-receipt-svc/internal/purchaseorder"
	"zoiko.io/goods-service-receipt-svc/internal/store"
)

// Action constants — AP-04's own contract's "Authorization / permissions"
// line ("receipt.read/create/confirm/reverse; service.accept"), adapted to this
// platform's SCREAMING_SNAKE_CASE convention. RejectReceipt reuses the Confirm
// action (both are the decision on a pending receipt); the draft-stage
// mutations and evidence reuse Create. ToleranceOverride is the spec's "approved
// exception": a receipt confirmer cannot exceed PO tolerance without it.
const (
	ReceiptRead       = "GOODS_SERVICE_RECEIPT_READ"
	ReceiptCreate     = "GOODS_SERVICE_RECEIPT_CREATE"
	ReceiptConfirm    = "GOODS_SERVICE_RECEIPT_CONFIRM"
	ReceiptReverse    = "GOODS_SERVICE_RECEIPT_REVERSE"
	ServiceAccept     = "SERVICE_ACCEPT"
	ToleranceOverride = "GOODS_SERVICE_RECEIPT_TOLERANCE_OVERRIDE"
)

// Stable machine-readable error codes (spec section 16).
const (
	codeValidation   = "VALIDATION_FAILED"
	codeForbidden    = "FORBIDDEN"
	codeNotFound     = "RECEIPT_NOT_FOUND"
	codeInvalidTrans = "INVALID_TRANSITION"
	codeStale        = "STALE_VERSION"
	codeVersionReq   = "EXPECTED_VERSION_REQUIRED"
	codeTolerance    = "OVER_RECEIPT_TOLERANCE"
	codeOverReversal = "OVER_REVERSAL"
	codePONotOpen    = "PO_NOT_OPEN"
	codePONotFound   = "PO_NOT_FOUND"
	codePOMismatch   = "PO_MISMATCH"
	codePOLine       = "PO_LINE_INVALID"
	codeCurrency     = "CURRENCY_MISMATCH"
	codeDependency   = "DEPENDENCY_UNAVAILABLE"
	codeStore        = "STORE_UNAVAILABLE"
	codeIdentity     = "IDENTITY_MISSING"
	codeTenant       = "TENANT_SCOPE_INVALID"
)

// AuthzChecker is the real dependency on authorization-svc, including its
// dynamic own-object SoD layer — see internal/authz's package doc comment.
type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
	CheckAllowedOwnObject(ctx context.Context, principalID, legalEntityID, actionType, resourceOwnerPrincipalID string) error
}

// Config is the policy the handler applies.
type Config struct {
	// OverReceiptTolerancePct is how far a receipt may exceed the open quantity
	// (or PO total) without an approved exception, as a PERCENTAGE of the ordered
	// quantity (5 = 5%) — the same unit as purchase-order-svc's
	// PO_OVER_TOLERANCE_PERCENT.
	OverReceiptTolerancePct float64
}

type Handler struct {
	store store.Store
	authz AuthzChecker
	po    purchaseorder.Client
	cfg   Config
	log   *zap.Logger
}

func New(st store.Store, az AuthzChecker, po purchaseorder.Client, cfg Config, log *zap.Logger) *Handler {
	return &Handler{store: st, authz: az, po: po, cfg: cfg, log: log}
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
		r.Post("/{receiptID}/accounting/requeue", h.RequeueAccounting)
	})
	r.Route("/ap04/purchase-orders/{purchaseOrderID}", func(r chi.Router) {
		r.Get("/receipts", h.ListReceiptsForPO)
		r.Get("/received-to-date", h.GetReceivedToDate)
	})
}

// ── responses ────────────────────────────────────────────────────────────────

type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError keeps the existing `error` message and adds the stable code.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorBody{Error: msg, Code: code})
}

const maxBody = 1 << 20

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, codeValidation, "invalid request body")
		return false
	}
	return true
}

// decodeOptional accepts an empty body (commands whose body is optional).
func decodeOptional(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	err := json.NewDecoder(r.Body).Decode(dst)
	if err != nil && err.Error() != "EOF" {
		writeError(w, http.StatusBadRequest, codeValidation, "invalid request body")
		return false
	}
	return true
}

func isUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}

// ── identity, tenant, authorization ──────────────────────────────────────────

func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	t := svcmiddleware.TenantFromContext(r.Context())
	if t == "" {
		writeError(w, http.StatusUnauthorized, codeTenant, domain.ErrTenantScopeMissing.Error())
		return "", false
	}
	return t, true
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, codeIdentity, "X-Principal-Id header is required")
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
		writeError(w, http.StatusForbidden, codeForbidden, "not authorized to perform this action")
		return false
	}
	h.log.Error("authorization check failed", zap.Error(err))
	writeError(w, http.StatusServiceUnavailable, codeDependency, "authorization service unavailable")
	return false
}

// authorizeRead authorizes a read in scope (a legal entity, or the tenant when
// none is known yet). A request with no principal is served ONLY when it
// declares itself an internal service call (X-Source-Channel: system — for
// example accounts-payable-svc's matching reading the received-to-date basis);
// it is still tenant-scoped.
func (h *Handler) authorizeRead(w http.ResponseWriter, r *http.Request, scope string) bool {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		if strings.EqualFold(r.Header.Get("X-Source-Channel"), "system") {
			return true
		}
		writeError(w, http.StatusUnauthorized, codeIdentity, "X-Principal-Id header is required")
		return false
	}
	return h.authorize(w, r, principalID, scope, ReceiptRead)
}

// expectedVersion reads expected_version from the body, falling back to the
// X-Expected-Version envelope header. required=true makes absence a 400.
func expectedVersion(w http.ResponseWriter, r *http.Request, body *int, required bool) (*int, bool) {
	v := body
	if v == nil {
		if hv := strings.TrimSpace(r.Header.Get("X-Expected-Version")); hv != "" {
			n, err := strconv.Atoi(hv)
			if err != nil || n < 1 {
				writeError(w, http.StatusBadRequest, codeValidation, "X-Expected-Version must be a positive integer")
				return nil, false
			}
			v = &n
		}
	}
	if v == nil && required {
		writeError(w, http.StatusBadRequest, codeVersionReq, "expected_version is required for this transition; read it from the receipt's version")
		return nil, false
	}
	if v != nil && *v < 1 {
		writeError(w, http.StatusBadRequest, codeValidation, "expected_version must be a positive integer")
		return nil, false
	}
	return v, true
}

func (h *Handler) command(r *http.Request, principalID string, ev *int) domain.Command {
	return domain.Command{PrincipalID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), ExpectedVersion: ev}
}

func (h *Handler) fetchReceipt(w http.ResponseWriter, r *http.Request, receiptID string) (*domain.GoodsServiceReceipt, bool) {
	if _, ok := h.requireTenant(w, r); !ok {
		return nil, false
	}
	rcpt, err := h.store.FindReceipt(r.Context(), receiptID)
	if err != nil {
		h.writeStoreErr(w, "FindReceipt", err)
		return nil, false
	}
	return rcpt, true
}

// writeStoreErr maps store failures onto the response; commands add their own
// cases first.
func (h *Handler) writeStoreErr(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, domain.ErrReceiptNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, "goods/service receipt not found")
	case errors.Is(err, domain.ErrTenantScopeMissing):
		writeError(w, http.StatusUnauthorized, codeTenant, domain.ErrTenantScopeMissing.Error())
	case errors.Is(err, domain.ErrStaleVersion):
		writeError(w, http.StatusConflict, codeStale, "receipt version does not match expected_version")
	default:
		h.log.Error(op+": store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, codeStore, "store unavailable")
	}
}

func (h *Handler) writePurchaseOrderErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrPurchaseOrderNotFound):
		writeError(w, http.StatusBadRequest, codePONotFound, "purchase order not found")
	case errors.Is(err, domain.ErrPurchaseOrderMismatch):
		writeError(w, http.StatusForbidden, codePOMismatch, "purchase order does not belong to the caller's tenant/legal entity")
	case errors.Is(err, domain.ErrPurchaseOrderNotOpen):
		// Receipt against a draft, held, cancelled or closed PO is refused.
		writeError(w, http.StatusConflict, codePONotOpen, "purchase order is closed")
	default:
		h.log.Error("purchase-order-svc lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, codeDependency, "purchase-order-svc unavailable")
	}
}

// ── purchase-order checks shared by create and confirm ───────────────────────

// poContext is what the handler learns from AP-03 about the order a receipt is
// against.
type poContext struct {
	order *purchaseorder.Summary
	line  *purchaseorder.Line
	open  float64 // AP-03's open_receipt_quantity for the line
}

// loadPO verifies the PO is real, the caller's and ISSUED, the currency agrees,
// and (for a line receipt) the line belongs to it. It writes the response and
// returns false on any refusal; every failure path is closed.
func (h *Handler) loadPO(w http.ResponseWriter, r *http.Request, tenantID, legalEntityID, poID, currency string, lineID *string) (*poContext, bool) {
	order, err := h.po.GetOpenOrder(r.Context(), tenantID, legalEntityID, poID)
	if err != nil {
		h.writePurchaseOrderErr(w, err)
		return nil, false
	}
	if order.CurrencyCode != "" && currency != "" && !strings.EqualFold(order.CurrencyCode, currency) {
		writeError(w, http.StatusUnprocessableEntity, codeCurrency, domain.ErrCurrencyMismatch.Error())
		return nil, false
	}
	pc := &poContext{order: order}
	if lineID == nil || *lineID == "" {
		return pc, true
	}
	line, ok := order.Line(*lineID)
	if !ok {
		writeError(w, http.StatusUnprocessableEntity, codePOLine, domain.ErrPurchaseOrderLineInvalid.Error())
		return nil, false
	}
	pc.line = &line
	open, err := h.po.GetOpenQuantity(r.Context(), tenantID, legalEntityID, poID)
	if err != nil {
		h.writePurchaseOrderErr(w, err)
		return nil, false
	}
	found := false
	for _, l := range open {
		if strings.EqualFold(l.LineID, line.LineID) {
			pc.open, found = l.OpenReceiptQuantity, true
		}
	}
	if !found {
		// AP-03 reports no open quantity for a line it listed: cannot verify.
		writeError(w, http.StatusServiceUnavailable, codeDependency, "purchase-order-svc did not report the line's open quantity")
		return nil, false
	}
	return pc, true
}

func (h *Handler) limits(pc *poContext) domain.ConfirmLimits {
	l := domain.ConfirmLimits{POTotalAmount: pc.order.TotalAmount, TolerancePct: h.cfg.OverReceiptTolerancePct}
	if pc.line != nil {
		l.LineOrderedQuantity, l.LineOpenReceiptQuantity = pc.line.Quantity, pc.open
	}
	return l
}

// ── receipts ─────────────────────────────────────────────────────────────────

func (h *Handler) CreateReceipt(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateReceiptRequest
	if !decode(w, r, &req) {
		return
	}
	switch {
	case req.LegalEntityID == "" || req.PurchaseOrderID == "":
		writeError(w, http.StatusBadRequest, codeValidation, "legal_entity_id and purchase_order_id are required")
		return
	case !isUUID(req.LegalEntityID) || !isUUID(req.PurchaseOrderID) || (req.POLineID != "" && !isUUID(req.POLineID)):
		writeError(w, http.StatusBadRequest, codeValidation, "legal_entity_id, purchase_order_id and po_line_id must be UUIDs")
		return
	case !domain.ValidReceiptType(req.ReceiptType):
		writeError(w, http.StatusBadRequest, codeValidation, "receipt_type must be GOODS or SERVICE")
		return
	case req.Amount <= 0 || req.Quantity <= 0:
		writeError(w, http.StatusBadRequest, codeValidation, "amount and quantity must be positive")
		return
	case req.CurrencyCode == "":
		writeError(w, http.StatusBadRequest, codeValidation, "currency_code is required")
		return
	case req.ReceiptDate.IsZero():
		writeError(w, http.StatusBadRequest, codeValidation, "receipt_date is required")
		return
	}

	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, req.LegalEntityID, ReceiptCreate) {
		return
	}
	if req.ToleranceExceptionRef != "" && !h.authorize(w, r, principalID, req.LegalEntityID, ToleranceOverride) {
		return
	}

	// Negative path 2's create-time half: a receipt can never be opened against a
	// PO that isn't real, isn't the caller's, or isn't ISSUED.
	var lineID *string
	if req.POLineID != "" {
		lineID = &req.POLineID
	}
	pc, ok := h.loadPO(w, r, tenantID, req.LegalEntityID, req.PurchaseOrderID, req.CurrencyCode, lineID)
	if !ok {
		return
	}

	// Advisory over-receipt check (the authoritative one runs at confirmation,
	// under a lock). Refusing early saves the caller a doomed draft.
	if pc.line != nil && req.ToleranceExceptionRef == "" {
		pending, err := h.store.PendingLineQuantity(r.Context(), *lineID)
		if err != nil {
			h.writeStoreErr(w, "CreateReceipt", err)
			return
		}
		lim := h.limits(pc)
		if req.Quantity > (lim.LineOpenReceiptQuantity-pending)+lim.LineOrderedQuantity*lim.TolerancePct/100+0.0001 {
			writeError(w, http.StatusConflict, codeTolerance, domain.ErrOverReceiptTolerance.Error())
			return
		}
	}

	rcpt, err := h.store.CreateReceipt(r.Context(), tenantID, req, h.command(r, principalID, nil))
	if err != nil {
		h.writeStoreErr(w, "CreateReceipt", err)
		return
	}
	writeJSON(w, http.StatusCreated, rcpt)
}

func (h *Handler) GetReceipt(w http.ResponseWriter, r *http.Request) {
	rcpt, ok := h.fetchReceipt(w, r, chi.URLParam(r, "receiptID"))
	if !ok || !h.authorizeRead(w, r, rcpt.LegalEntityID) {
		return
	}
	writeJSON(w, http.StatusOK, rcpt)
}

// scopeForPO is the authorization scope for a PO-level read: the legal entity of
// a receipt already on file against the PO, else the tenant.
func (h *Handler) scopeForPO(receipts []domain.GoodsServiceReceipt, tenantID string) string {
	if len(receipts) > 0 {
		return receipts[0].LegalEntityID
	}
	return tenantID
}

func (h *Handler) ListReceiptsForPO(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	receipts, err := h.store.ListReceiptsForPO(r.Context(), chi.URLParam(r, "purchaseOrderID"))
	if err != nil {
		h.writeStoreErr(w, "ListReceiptsForPO", err)
		return
	}
	if !h.authorizeRead(w, r, h.scopeForPO(receipts, tenantID)) {
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
	if !decode(w, r, &req) {
		return
	}
	if (req.Quantity != nil && *req.Quantity <= 0) || (req.Amount != nil && *req.Amount <= 0) {
		writeError(w, http.StatusBadRequest, codeValidation, "amount and quantity must be positive")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	existing, ok := h.fetchReceipt(w, r, receiptID)
	if !ok || !h.authorize(w, r, principalID, existing.LegalEntityID, ReceiptCreate) {
		return
	}
	ev, ok := expectedVersion(w, r, req.ExpectedVersion, false)
	if !ok {
		return
	}
	rcpt, err := h.store.AmendReceiptDraft(r.Context(), receiptID, req, h.command(r, principalID, ev))
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, codeInvalidTrans, "receipt is not in the required DRAFT state")
			return
		}
		h.writeStoreErr(w, "AmendReceiptDraft", err)
		return
	}
	writeJSON(w, http.StatusOK, rcpt)
}

// ConfirmReceipt handles POST .../confirm. Enforces: negative path 1 (over
// tolerance without an approved exception — re-checked authoritatively inside the
// confirmation transaction), negative path 2's confirm-time half (a live
// re-check that the PO is still ISSUED — it may have been cancelled or closed
// since the draft), the "approved exception" rule (an exception reference needs
// the tolerance-override permission), and the SoD line (a receiver cannot
// self-certify where independent acceptance is required) via authorization-svc's
// own-object layer. GRNI posting is queued in the same transaction.
func (h *Handler) ConfirmReceipt(w http.ResponseWriter, r *http.Request) {
	receiptID := chi.URLParam(r, "receiptID")
	var req domain.ConfirmReceiptRequest
	if !decodeOptional(w, r, &req) {
		return
	}
	if req.POLineID != "" && !isUUID(req.POLineID) {
		writeError(w, http.StatusBadRequest, codeValidation, "po_line_id must be a UUID")
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	existing, ok := h.fetchReceipt(w, r, receiptID)
	if !ok {
		return
	}
	if !domain.CanConfirm(existing.Status) {
		writeError(w, http.StatusConflict, codeInvalidTrans, "receipt is not in a confirmable state")
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
	exception := firstNonEmpty(req.ToleranceExceptionRef, existing.ToleranceExceptionRef)
	if exception != "" && !h.authorize(w, r, principalID, existing.LegalEntityID, ToleranceOverride) {
		return
	}
	ev, ok := expectedVersion(w, r, req.ExpectedVersion, true)
	if !ok {
		return
	}
	if ev != nil && existing.Version != *ev {
		writeError(w, http.StatusConflict, codeStale, "receipt version does not match expected_version")
		return
	}

	lineID := existing.POLineID
	if req.POLineID != "" {
		lineID = &req.POLineID
	}
	pc, ok := h.loadPO(w, r, tenantID, existing.LegalEntityID, existing.PurchaseOrderID, existing.CurrencyCode, lineID)
	if !ok {
		return
	}

	rev := pc.order.Revision
	res, err := h.store.ConfirmReceipt(r.Context(), receiptID, domain.ConfirmInput{
		POLineID: strPtr(req.POLineID), PORevision: &rev, ToleranceExceptionRef: req.ToleranceExceptionRef, Limits: h.limits(pc),
	}, h.command(r, principalID, ev))
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrInvalidTransition):
			writeError(w, http.StatusConflict, codeInvalidTrans, "receipt is not in a confirmable state")
		case errors.Is(err, domain.ErrOverReceiptTolerance):
			writeError(w, http.StatusConflict, codeTolerance, "receipt amount exceeds purchase order tolerance without an approved exception")
		case errors.Is(err, domain.ErrPurchaseOrderLineInvalid):
			writeError(w, http.StatusUnprocessableEntity, codePOLine, domain.ErrPurchaseOrderLineInvalid.Error())
		default:
			h.writeStoreErr(w, "ConfirmReceipt", err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"receipt": res.Receipt, "accounting_event": res.Accounting})
}

func (h *Handler) RejectReceipt(w http.ResponseWriter, r *http.Request) {
	receiptID := chi.URLParam(r, "receiptID")
	var req domain.RejectReceiptRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, codeValidation, "reason is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	existing, ok := h.fetchReceipt(w, r, receiptID)
	if !ok || !h.authorize(w, r, principalID, existing.LegalEntityID, ReceiptConfirm) {
		return
	}
	ev, ok := expectedVersion(w, r, req.ExpectedVersion, false)
	if !ok {
		return
	}
	rcpt, err := h.store.RejectReceipt(r.Context(), receiptID, req, h.command(r, principalID, ev))
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, codeInvalidTrans, "receipt is not in a rejectable state")
			return
		}
		h.writeStoreErr(w, "RejectReceipt", err)
		return
	}
	writeJSON(w, http.StatusOK, rcpt)
}

// ReverseReceipt handles POST .../reverse — the platform's only sanctioned
// correction mechanism for a confirmed receipt, directly enforcing negative path
// 3 ("confirmed receipt deleted to fix mismatch" must be blocked; the
// immutability trigger blocks DELETE and edits unconditionally, and this linked
// reversal is the real correction path).
func (h *Handler) ReverseReceipt(w http.ResponseWriter, r *http.Request) {
	receiptID := chi.URLParam(r, "receiptID")
	var req domain.ReverseReceiptRequest
	if !decode(w, r, &req) {
		return
	}
	if req.ReversedAmount <= 0 || req.Reason == "" {
		writeError(w, http.StatusBadRequest, codeValidation, "reversed_amount (positive) and reason are required")
		return
	}
	if req.ReversedQuantity != nil && *req.ReversedQuantity < 0 {
		writeError(w, http.StatusBadRequest, codeValidation, "reversed_quantity must not be negative")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	existing, ok := h.fetchReceipt(w, r, receiptID)
	if !ok || !h.authorize(w, r, principalID, existing.LegalEntityID, ReceiptReverse) {
		return
	}
	ev, ok := expectedVersion(w, r, req.ExpectedVersion, false)
	if !ok {
		return
	}
	rcpt, reversal, err := h.store.ReverseReceipt(r.Context(), receiptID, req, h.command(r, principalID, ev))
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrOverReversal):
			writeError(w, http.StatusConflict, codeOverReversal, "reversal amount exceeds remaining unreversed receipt amount")
		case errors.Is(err, domain.ErrInvalidTransition):
			writeError(w, http.StatusConflict, codeInvalidTrans, "receipt is not in a reversible state")
		default:
			h.writeStoreErr(w, "ReverseReceipt", err)
		}
		return
	}
	writeJSON(w, http.StatusOK, struct {
		*domain.GoodsServiceReceipt
		Reversal *domain.ReceiptReversal `json:"reversal"`
	}{rcpt, reversal})
}

// RecordServiceAcceptance handles POST .../service-acceptance. When the receipt
// requires independent acceptance, the accepting principal is checked against
// authorization-svc's own-object SoD layer with the original receiver as owner —
// the direct enforcement of AP-04's SoD line ("receiver/acceptor cannot
// self-certify sensitive services where policy requires independent acceptance").
func (h *Handler) RecordServiceAcceptance(w http.ResponseWriter, r *http.Request) {
	receiptID := chi.URLParam(r, "receiptID")
	var req domain.RecordServiceAcceptanceRequest
	if !decodeOptional(w, r, &req) {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	existing, ok := h.fetchReceipt(w, r, receiptID)
	if !ok {
		return
	}
	if existing.ReceiptType != domain.ReceiptTypeService {
		writeError(w, http.StatusBadRequest, codeValidation, "service acceptance only applies to SERVICE receipts")
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
	ev, ok := expectedVersion(w, r, req.ExpectedVersion, false)
	if !ok {
		return
	}
	rcpt, err := h.store.RecordServiceAcceptance(r.Context(), receiptID, req, h.command(r, principalID, ev))
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, codeInvalidTrans, "receipt is not in the required DRAFT state")
			return
		}
		h.writeStoreErr(w, "RecordServiceAcceptance", err)
		return
	}
	writeJSON(w, http.StatusOK, rcpt)
}

// ── evidence ─────────────────────────────────────────────────────────────────

func (h *Handler) AttachReceiptEvidence(w http.ResponseWriter, r *http.Request) {
	receiptID := chi.URLParam(r, "receiptID")
	var req domain.AttachReceiptEvidenceRequest
	if !decode(w, r, &req) {
		return
	}
	if req.EvidenceRef == "" {
		writeError(w, http.StatusBadRequest, codeValidation, "evidence_ref is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	existing, ok := h.fetchReceipt(w, r, receiptID)
	if !ok || !h.authorize(w, r, principalID, existing.LegalEntityID, ReceiptCreate) {
		return
	}
	e, err := h.store.AttachReceiptEvidence(r.Context(), receiptID, req, principalID)
	if err != nil {
		h.writeStoreErr(w, "AttachReceiptEvidence", err)
		return
	}
	writeJSON(w, http.StatusCreated, e)
}

func (h *Handler) ListReceiptEvidence(w http.ResponseWriter, r *http.Request) {
	receiptID := chi.URLParam(r, "receiptID")
	existing, ok := h.fetchReceipt(w, r, receiptID)
	if !ok || !h.authorizeRead(w, r, existing.LegalEntityID) {
		return
	}
	evidence, err := h.store.ListReceiptEvidence(r.Context(), receiptID)
	if err != nil {
		h.writeStoreErr(w, "ListReceiptEvidence", err)
		return
	}
	if evidence == nil {
		evidence = []domain.ReceiptEvidence{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": evidence, "count": len(evidence)})
}

// ── queries ──────────────────────────────────────────────────────────────────

// GetAvailableActions lists what the CALLER may do next, given the state and
// segregation of duties (Confirm is hidden from a receiver who must not
// self-certify).
func (h *Handler) GetAvailableActions(w http.ResponseWriter, r *http.Request) {
	receiptID := chi.URLParam(r, "receiptID")
	rcpt, ok := h.fetchReceipt(w, r, receiptID)
	if !ok || !h.authorizeRead(w, r, rcpt.LegalEntityID) {
		return
	}
	caller := r.Header.Get("X-Principal-Id")
	actions := []string{}
	if domain.CanAmendDraft(rcpt.Status) {
		actions = append(actions, "AmendReceiptDraft")
	}
	selfCertify := rcpt.RequiresIndependentAcceptance && caller != "" && caller == rcpt.ReceiverPrincipalID
	if domain.CanConfirm(rcpt.Status) && !selfCertify {
		actions = append(actions, "ConfirmReceipt")
	}
	if domain.CanReject(rcpt.Status) {
		actions = append(actions, "RejectReceipt")
	}
	if domain.CanReverse(rcpt.Status) {
		actions = append(actions, "ReverseReceipt")
	}
	if rcpt.Status == domain.StatusDraft && rcpt.ReceiptType == domain.ReceiptTypeService && !selfCertify {
		actions = append(actions, "RecordServiceAcceptance")
	}
	if rcpt.Status != domain.StatusRejected {
		actions = append(actions, "AttachReceiptEvidence")
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"receipt_id": receiptID, "status": rcpt.Status, "version": rcpt.Version, "available_actions": actions,
	})
}

// GetReceiptAccountingStatus reports the true state of the receipt's GRNI
// posting request(s): PENDING, POSTED, FAILED or QUARANTINED, with attempts and
// the last error. An accounting failure is visible here, never silent.
func (h *Handler) GetReceiptAccountingStatus(w http.ResponseWriter, r *http.Request) {
	receiptID := chi.URLParam(r, "receiptID")
	rcpt, ok := h.fetchReceipt(w, r, receiptID)
	if !ok || !h.authorizeRead(w, r, rcpt.LegalEntityID) {
		return
	}
	event, err := h.store.GetLatestAccountingEvent(r.Context(), receiptID)
	if err != nil {
		h.writeStoreErr(w, "GetReceiptAccountingStatus", err)
		return
	}
	if event == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"receipt_id": receiptID, "status": "NOT_APPLICABLE"})
		return
	}
	writeJSON(w, http.StatusOK, event)
}

// RequeueAccounting puts the receipt's FAILED/QUARANTINED posting requests back
// to PENDING once an operator fixed the cause. A POSTED request is never
// touched; a requeue re-submits the same source_event_id, which the ledger
// treats idempotently.
func (h *Handler) RequeueAccounting(w http.ResponseWriter, r *http.Request) {
	receiptID := chi.URLParam(r, "receiptID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	rcpt, ok := h.fetchReceipt(w, r, receiptID)
	if !ok || !h.authorize(w, r, principalID, rcpt.LegalEntityID, ReceiptConfirm) {
		return
	}
	n, err := h.store.RequeueAccounting(r.Context(), receiptID)
	if err != nil {
		h.writeStoreErr(w, "RequeueAccounting", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"receipt_id": receiptID, "requeued": n})
}

// GetReceivedToDate reports the net confirmed receipt of a PO — per line
// (quantity and amount, less reversals) and in total — alongside the PO's own
// total from purchase-order-svc. It is the receipt basis AP-06 matching reads.
func (h *Handler) GetReceivedToDate(w http.ResponseWriter, r *http.Request) {
	purchaseOrderID := chi.URLParam(r, "purchaseOrderID")
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	receipts, err := h.store.ListReceiptsForPO(r.Context(), purchaseOrderID)
	if err != nil {
		h.writeStoreErr(w, "GetReceivedToDate", err)
		return
	}
	if !h.authorizeRead(w, r, h.scopeForPO(receipts, tenantID)) {
		return
	}
	netToDate, err := h.store.SumNetConfirmedAmountForPO(r.Context(), purchaseOrderID)
	if err != nil {
		h.writeStoreErr(w, "GetReceivedToDate", err)
		return
	}
	lines, err := h.store.ReceivedToDate(r.Context(), purchaseOrderID)
	if err != nil {
		h.writeStoreErr(w, "GetReceivedToDate", err)
		return
	}
	if lines == nil {
		lines = []domain.LineReceived{}
	}
	resp := map[string]interface{}{"purchase_order_id": purchaseOrderID, "net_confirmed_amount": netToDate, "lines": lines}
	// The PO's own total/status is best-effort enrichment, not a gate; it needs a
	// legal entity, borrowed from a receipt already on file. No receipts means no
	// enrichment, not an error.
	if len(receipts) > 0 {
		if po, err := h.po.GetOrder(r.Context(), tenantID, receipts[0].LegalEntityID, purchaseOrderID); err == nil {
			resp["po_total_amount"] = po.TotalAmount
			resp["po_status"] = po.Status
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
