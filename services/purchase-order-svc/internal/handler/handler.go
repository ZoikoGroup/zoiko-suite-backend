// Package handler exposes purchase-order-svc's REST API — AP-03.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/purchase-order-svc/internal/domain"
	svcmiddleware "zoiko.io/purchase-order-svc/internal/middleware"
	"zoiko.io/purchase-order-svc/internal/procurementcase"
	"zoiko.io/purchase-order-svc/internal/purchaserequest"
	"zoiko.io/purchase-order-svc/internal/store"
	"zoiko.io/purchase-order-svc/internal/supplier"
)

// Store is the persistence contract the handler depends on.
type Store interface {
	CreateDraft(ctx context.Context, in store.CreateDraftInput) (*domain.OrderDetail, bool, error)
	CreateIssued(ctx context.Context, in store.IssuedInput) (*domain.OrderDetail, bool, error)
	GetOrder(ctx context.Context, orderID string) (*domain.PurchaseOrder, error)
	GetOrderDetail(ctx context.Context, orderID string) (*domain.OrderDetail, error)
	ListOrders(ctx context.Context, filter domain.ListOrdersFilter) ([]domain.PurchaseOrder, error)
	ListAmendments(ctx context.Context, orderID string) ([]domain.PurchaseOrderAmendment, error)
	ListRevisions(ctx context.Context, orderID string) ([]domain.Revision, error)
	ListEvents(ctx context.Context, orderID string) ([]domain.OrderEvent, error)
	AmendOrder(ctx context.Context, tenantID, orderID, actor string, req domain.AmendOrderRequest) (*store.AmendResult, error)
	Transition(ctx context.Context, in store.TransitionInput) (*domain.OrderDetail, error)
	RecordProgress(ctx context.Context, tenantID, orderID, lineID, actor string, req domain.ProgressRequest, correlationID string) (*domain.ProgressResult, error)
	OpenQuantity(ctx context.Context, orderID string) ([]domain.LineProgress, bool, error)

	BeginIdempotent(ctx context.Context, scope, key, requestHash string) (*domain.IdempotencyRecord, bool, error)
	CompleteIdempotent(ctx context.Context, scope, key string, statusCode int, body []byte) error
	ReleaseIdempotent(ctx context.Context, scope, key string) error
}

// AuthZClient is the authorization contract the handler depends on.
type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
	CheckAllowedOwnObject(ctx context.Context, principalID, legalEntityID, actionType, resourceOwnerPrincipalID string) error
}

// PurchaseRequestClient is the cross-service verification contract the
// handler depends on — see internal/purchaserequest's package doc for why
// this exists.
type PurchaseRequestClient interface {
	GetApprovedRequest(ctx context.Context, tenantID, legalEntityID, requestID string) (*purchaserequest.Summary, error)
}

// Action types checked against authorization-svc. PO_ISSUE, PO_AMEND and
// PO_CLOSE are the original three; the rest are new with the governed
// lifecycle. Reads are scoped to the caller's tenant rather than authorized,
// matching the rest of the commercial-ops chain.
const (
	actionCreateOrder        = "PO_CREATE"
	actionSubmitOrder        = "PO_SUBMIT"
	actionApproveOrder       = "PO_APPROVE"
	actionApproveHighValue   = "PO_APPROVE_HIGHVALUE"
	actionIssueOrder         = "PO_ISSUE"
	actionAmendOrder         = "PO_AMEND"
	actionHoldOrder          = "PO_HOLD"
	actionCancelOrder        = "PO_CANCEL"
	actionCloseOrder         = "PO_CLOSE"
	actionSupplierException  = "PO_SUPPLIER_EXCEPTION"
	actionRecordProgress     = "PO_PROGRESS_RECORD"
	maxLinesPerOrder         = 500
	maxIdempotentRequestBody = 1 << 20
)

// Options tunes behaviour that is configuration, not code.
type Options struct {
	// HighValueThreshold: a PO whose total exceeds it needs PO_APPROVE_HIGHVALUE
	// to approve (0 disables the tier). The spec's "preparer cannot approve above
	// authority".
	HighValueThreshold float64
}

type Handler struct {
	store    Store
	authz    AuthZClient
	prClient PurchaseRequestClient
	supplier supplier.Client
	cases    procurementcase.Client
	opts     Options
	log      *zap.Logger
}

func New(store Store, authz AuthZClient, prClient PurchaseRequestClient, log *zap.Logger) *Handler {
	return &Handler{store: store, authz: authz, prClient: prClient, log: log}
}

// WithSupplierClient enables the AP-01 supplier-eligibility checks.
func (h *Handler) WithSupplierClient(c supplier.Client) *Handler { h.supplier = c; return h }

// WithProcurementCases enables verification of a procurement case as the approval
// basis of the legacy direct-issue endpoint.
func (h *Handler) WithProcurementCases(c procurementcase.Client) *Handler { h.cases = c; return h }

// WithOptions sets the tunables.
func (h *Handler) WithOptions(o Options) *Handler { h.opts = o; return h }

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/purchase-orders", func(r chi.Router) {
		// Legacy direct issue: still here, now only with a verifiable approval.
		r.Post("/", h.idempotent("issue", h.IssueOrder))
		// CreatePurchaseOrder (governed flow).
		r.Post("/draft", h.idempotent("draft", h.CreateDraft))
		r.Get("/", h.ListOrders)

		r.Route("/{purchase_order_id}", func(r chi.Router) {
			r.Get("/", h.GetOrder)
			r.Get("/amendments", h.ListAmendments)
			r.Get("/revisions", h.ListRevisions)
			r.Get("/revisions/{revision}", h.GetRevision)
			r.Get("/history", h.GetHistory)
			r.Get("/open-quantity", h.GetOpenQuantity)
			r.Get("/receipt-status", h.GetReceiptStatus)
			r.Get("/invoice-status", h.GetInvoiceStatus)
			r.Get("/available-actions", h.GetAvailableActions)

			r.Post("/amend", h.idempotent("amend", h.AmendOrder))
			r.Post("/submit", h.idempotent("submit", h.command(store.CmdSubmit)))
			r.Post("/approve", h.idempotent("approve", h.command(store.CmdApprove)))
			r.Post("/issue", h.idempotent("issue-order", h.command(store.CmdIssue)))
			r.Post("/hold", h.idempotent("hold", h.command(store.CmdHold)))
			r.Post("/release-hold", h.idempotent("release-hold", h.command(store.CmdRelease)))
			r.Post("/cancel", h.idempotent("cancel", h.command(store.CmdCancel)))
			r.Post("/close", h.idempotent("close", h.command(store.CmdClose)))
			r.Post("/lines/{line_id}/progress", h.idempotent("progress", h.RecordProgress))
		})
	})
}

// ── create (governed flow) ───────────────────────────────────────────────────

// CreateDraft is CreatePurchaseOrder: a PO in DRAFT with its lines. Idempotent on
// (tenant, correlation_id), so a retried conversion from a requisition resolves
// to the same order.
func (h *Handler) CreateDraft(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateDraftRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if req.TenantID != "" && req.TenantID != tenantID {
		writeError(w, http.StatusForbidden, "tenant_scope_mismatch", domain.ErrTenantScopeMismatch.Error())
		return
	}
	switch {
	case !isUUID(req.LegalEntityID):
		writeError(w, http.StatusBadRequest, "invalid_field", "legal_entity_id must be a UUID")
		return
	case strings.TrimSpace(req.CorrelationID) == "":
		writeError(w, http.StatusBadRequest, "missing_field", "correlation_id")
		return
	case strings.TrimSpace(req.SupplierRef) == "":
		writeError(w, http.StatusBadRequest, "missing_field", "supplier_ref")
		return
	case !validCurrency(req.CurrencyCode):
		writeError(w, http.StatusBadRequest, "invalid_field", "currency_code must be a 3-letter ISO 4217 code")
		return
	}
	lines, err := validateLines(req.Lines, true)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_line", err.Error())
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionCreateOrder); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if req.PurchaseRequestID != nil && *req.PurchaseRequestID != "" {
		if _, err := h.prClient.GetApprovedRequest(r.Context(), tenantID, req.LegalEntityID, *req.PurchaseRequestID); err != nil {
			h.writePurchaseRequestErr(w, err)
			return
		}
	}
	exceptionBy, err := h.verifySupplier(r.Context(), tenantID, principalID, req.LegalEntityID, req.SupplierRef, req.SupplierExceptionRef)
	if err != nil {
		h.writeSupplierErr(w, err)
		return
	}

	d, created, err := h.store.CreateDraft(r.Context(), store.CreateDraftInput{
		TenantID: tenantID, LegalEntityID: req.LegalEntityID, PurchaseRequestID: req.PurchaseRequestID,
		SupplierRef: strings.TrimSpace(req.SupplierRef), CurrencyCode: strings.ToUpper(req.CurrencyCode),
		CorrelationID: req.CorrelationID, DeliveryTerms: req.DeliveryTerms, PaymentTerms: req.PaymentTerms,
		SupplierExceptionRef: req.SupplierExceptionRef, SupplierExceptionBy: exceptionBy,
		PreparedBy: principalID, Lines: lines,
	})
	if err != nil {
		h.writeStoreErr(w, "CreateDraft", err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, d)
}

// ── legacy direct issue ──────────────────────────────────────────────────────

// IssueOrder is the original POST /v1/purchase-orders. It still creates an ISSUED
// order in one call, but a PO can no longer reach ISSUED without approval: the
// approval must be verifiable at its source — an APPROVED purchase requisition,
// or an approved procurement case (whose id is the correlation_id). With neither,
// the call is refused (422 PO_APPROVAL_REQUIRED) and the caller uses the governed
// flow (draft -> submit -> approve -> issue).
func (h *Handler) IssueOrder(w http.ResponseWriter, r *http.Request) {
	var req domain.IssueOrderRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if missing := requiredFieldMissing(req); missing != "" {
		writeError(w, http.StatusBadRequest, "missing_field", missing)
		return
	}
	lines, err := validateLines(req.Lines, false)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_line", err.Error())
		return
	}
	total := req.TotalAmount
	if len(lines) > 0 {
		var sum float64
		for _, l := range lines {
			sum += l.Amount()
		}
		sum = domain.RoundMoney(sum)
		if total > 0 && abs(total-sum) > 0.01 {
			writeError(w, http.StatusBadRequest, "invalid_field", "total_amount does not equal the sum of the lines")
			return
		}
		total = sum
	}
	if total <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_field", "total_amount must be greater than zero")
		return
	}

	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// tenant_id in the body is accepted only when it agrees with the verified
	// scope. It used to be the ONLY source of the stored tenant_id, so a body
	// naming another tenant issued the order into that tenant's register.
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
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionIssueOrder); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	basis, ref, approvedBy, err := h.verifyApprovalBasis(r.Context(), tenantID, principalID, req, total)
	if err != nil {
		h.writeApprovalErr(w, err)
		return
	}
	exceptionBy, err := h.verifySupplier(r.Context(), tenantID, principalID, req.LegalEntityID, req.SupplierRef, req.SupplierExceptionRef)
	if err != nil {
		h.writeSupplierErr(w, err)
		return
	}

	d, created, err := h.store.CreateIssued(r.Context(), store.IssuedInput{
		CreateDraftInput: store.CreateDraftInput{
			TenantID: tenantID, LegalEntityID: req.LegalEntityID, PurchaseRequestID: req.PurchaseRequestID,
			VendorProfileID: req.VendorProfileID, SupplierRef: strings.TrimSpace(req.SupplierRef),
			CurrencyCode: strings.ToUpper(req.CurrencyCode), CorrelationID: req.CorrelationID,
			SupplierExceptionRef: req.SupplierExceptionRef, SupplierExceptionBy: exceptionBy,
			PreparedBy: principalID, Lines: lines, TotalAmount: total,
		},
		ApprovalBasis: basis, ApprovalRef: ref, ApprovedBy: approvedBy,
	})
	if err != nil {
		h.writeStoreErr(w, "IssueOrder", err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, d)
}

// verifyApprovalBasis finds and verifies the approval that lets a direct-issued
// order skip the in-service submit/approve steps. Fail-closed: no basis, or a
// basis that cannot be read, means no order.
func (h *Handler) verifyApprovalBasis(ctx context.Context, tenantID, principalID string, req domain.IssueOrderRequest, total float64) (basis, ref, approvedBy string, err error) {
	if req.PurchaseRequestID != nil && *req.PurchaseRequestID != "" {
		if _, err := h.prClient.GetApprovedRequest(ctx, tenantID, req.LegalEntityID, *req.PurchaseRequestID); err != nil {
			return "", "", "", err
		}
		return domain.ApprovalBasisPurchaseRequest, *req.PurchaseRequestID, "purchase-request-svc", nil
	}
	if h.cases == nil {
		return "", "", "", domain.ErrApprovalRequired
	}
	pc, err := h.cases.VerifyApproved(ctx, tenantID, principalID, req.LegalEntityID, req.CorrelationID, total, req.CurrencyCode)
	if err != nil {
		return "", "", "", err
	}
	return domain.ApprovalBasisProcurementCase, pc.CaseID, *pc.ApprovedByPrincipalID, nil
}

// ── reads ───────────────────────────────────────────────────────────────────

// GetOrder returns the order with its lines.
func (h *Handler) GetOrder(w http.ResponseWriter, r *http.Request) {
	d, ok := h.loadDetail(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (h *Handler) loadDetail(w http.ResponseWriter, r *http.Request) (*domain.OrderDetail, bool) {
	d, err := h.store.GetOrderDetail(r.Context(), chi.URLParam(r, "purchase_order_id"))
	if err != nil {
		h.writeStoreErr(w, "GetOrder", err)
		return nil, false
	}
	if d == nil {
		writeError(w, http.StatusNotFound, "order_not_found", "")
		return nil, false
	}
	return d, true
}

func (h *Handler) loadHeader(w http.ResponseWriter, r *http.Request, op string) (*domain.PurchaseOrder, bool) {
	po, err := h.store.GetOrder(r.Context(), chi.URLParam(r, "purchase_order_id"))
	if err != nil {
		h.writeStoreErr(w, op, err)
		return nil, false
	}
	if po == nil {
		writeError(w, http.StatusNotFound, "order_not_found", "")
		return nil, false
	}
	return po, true
}

// ListOrders returns the caller's own tenant's register.
//
// The scope comes from the verified X-Tenant-Id header. It used to come from
// ?tenant_id=, which the store both filtered on AND set app.tenant_id from — so
// `?tenant_id=<any-uuid>` returned that tenant's entire purchase-order register.
func (h *Handler) ListOrders(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	if claimed := q.Get("tenant_id"); claimed != "" && claimed != tenantID {
		writeError(w, http.StatusForbidden, "tenant_scope_mismatch", domain.ErrTenantScopeMismatch.Error())
		return
	}
	if status := q.Get("status"); status != "" && !domain.ValidOrderStatus(status) {
		writeError(w, http.StatusBadRequest, "invalid_field", "status is not a recognised purchase order status")
		return
	}
	// legal_entity_id is compared as `legal_entity_id::text = $n`, so a malformed
	// value does not error, it silently matches nothing — refused here instead.
	legalEntityID := q.Get("legal_entity_id")
	if legalEntityID != "" && !isUUID(legalEntityID) {
		writeError(w, http.StatusBadRequest, "invalid_field", "legal_entity_id must be a UUID")
		return
	}
	list, err := h.store.ListOrders(r.Context(), domain.ListOrdersFilter{
		TenantID: tenantID, LegalEntityID: legalEntityID, Status: q.Get("status"),
	})
	if err != nil {
		if errors.Is(err, domain.ErrInvalidIdentifier) {
			writeError(w, http.StatusBadRequest, "invalid_field", "tenant scope must be a UUID")
			return
		}
		h.log.Error("ListOrders: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if list == nil {
		list = []domain.PurchaseOrder{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ListAmendments is the append-only amendment ledger for one order, oldest
// first. The order is resolved first so an unknown id is a 404 rather than an
// empty list.
func (h *Handler) ListAmendments(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.loadHeader(w, r, "ListAmendments"); !ok {
		return
	}
	list, err := h.store.ListAmendments(r.Context(), chi.URLParam(r, "purchase_order_id"))
	if err != nil {
		h.writeStoreErr(w, "ListAmendments", err)
		return
	}
	if list == nil {
		list = []domain.PurchaseOrderAmendment{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ListRevisions: the superseded revisions (immutable snapshots), plus which
// revision is current.
func (h *Handler) ListRevisions(w http.ResponseWriter, r *http.Request) {
	po, ok := h.loadHeader(w, r, "ListRevisions")
	if !ok {
		return
	}
	revs, err := h.store.ListRevisions(r.Context(), po.PurchaseOrderID)
	if err != nil {
		h.writeStoreErr(w, "ListRevisions", err)
		return
	}
	if revs == nil {
		revs = []domain.Revision{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"purchase_order_id": po.PurchaseOrderID, "current_revision": po.Revision, "revisions": revs})
}

// GetRevision is GetPORevision: one revision's full snapshot. The current
// revision is served from the live order.
func (h *Handler) GetRevision(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(chi.URLParam(r, "revision"))
	if err != nil || n < 1 {
		writeError(w, http.StatusBadRequest, "invalid_field", "revision must be a positive integer")
		return
	}
	d, ok := h.loadDetail(w, r)
	if !ok {
		return
	}
	if n == d.Revision {
		writeJSON(w, http.StatusOK, map[string]any{"revision": n, "current": true, "status_at_snapshot": d.Status, "snapshot": d})
		return
	}
	revs, err := h.store.ListRevisions(r.Context(), d.PurchaseOrderID)
	if err != nil {
		h.writeStoreErr(w, "GetRevision", err)
		return
	}
	for _, rev := range revs {
		if rev.Revision == n {
			writeJSON(w, http.StatusOK, map[string]any{
				"revision": n, "current": false, "status_at_snapshot": rev.StatusAtSnapshot, "snapshot": rev.Snapshot,
				"approved_by_principal_id": rev.ApprovedByPrincipalID, "approved_at": rev.ApprovedAt, "reason": rev.Reason,
				"created_by_principal_id": rev.CreatedByPrincipalID, "created_at": rev.CreatedAt, "superseded_by_revision": rev.SupersededByRevision,
			})
			return
		}
	}
	writeError(w, http.StatusNotFound, "revision_not_found", "")
}

// GetHistory is the append-only transition history.
func (h *Handler) GetHistory(w http.ResponseWriter, r *http.Request) {
	po, ok := h.loadHeader(w, r, "GetHistory")
	if !ok {
		return
	}
	events, err := h.store.ListEvents(r.Context(), po.PurchaseOrderID)
	if err != nil {
		h.writeStoreErr(w, "GetHistory", err)
		return
	}
	if events == nil {
		events = []domain.OrderEvent{}
	}
	writeJSON(w, http.StatusOK, events)
}

// GetOpenQuantity is the per-line ordered/received/invoiced/open figures
// (cross-service contract #2).
func (h *Handler) GetOpenQuantity(w http.ResponseWriter, r *http.Request) {
	lines, ok, err := h.store.OpenQuantity(r.Context(), chi.URLParam(r, "purchase_order_id"))
	if err != nil {
		h.writeStoreErr(w, "GetOpenQuantity", err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "order_not_found", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
}

// GetReceiptStatus / GetInvoiceStatus: NOT_STARTED / PARTIAL / COMPLETE derived
// from per-line progress. The PO's own status deliberately stays ISSUED while
// goods are received (receipts are accepted only on ISSUED orders).
func (h *Handler) GetReceiptStatus(w http.ResponseWriter, r *http.Request) {
	h.fulfilment(w, r, "receipt", func(l domain.LineProgress) float64 { return l.ReceivedQuantity })
}

func (h *Handler) GetInvoiceStatus(w http.ResponseWriter, r *http.Request) {
	h.fulfilment(w, r, "invoice", func(l domain.LineProgress) float64 { return l.InvoicedQuantity })
}

func (h *Handler) fulfilment(w http.ResponseWriter, r *http.Request, name string, done func(domain.LineProgress) float64) {
	lines, ok, err := h.store.OpenQuantity(r.Context(), chi.URLParam(r, "purchase_order_id"))
	if err != nil {
		h.writeStoreErr(w, "Get"+name+"Status", err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "order_not_found", "")
		return
	}
	var ordered, total float64
	out := make([]map[string]any, 0, len(lines))
	for _, l := range lines {
		ordered += l.OrderedQuantity
		total += done(l)
		out = append(out, map[string]any{"line_id": l.LineID, "line_number": l.LineNumber,
			"ordered_quantity": l.OrderedQuantity, "quantity": done(l), "status": domain.Fulfilment(l.OrderedQuantity, done(l))})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"purchase_order_id": chi.URLParam(r, "purchase_order_id"), "status": domain.Fulfilment(ordered, total),
		"ordered_quantity": ordered, "quantity": total, "lines": out,
	})
}

// GetAvailableActions lists what the CURRENT state allows, minus what
// maker-checker forbids this caller (an order's preparer/submitter is never
// offered Approve).
func (h *Handler) GetAvailableActions(w http.ResponseWriter, r *http.Request) {
	po, ok := h.loadHeader(w, r, "GetAvailableActions")
	if !ok {
		return
	}
	principal := r.Header.Get("X-Principal-Id")
	var a []string
	switch po.Status {
	case domain.OrderStatusDraft:
		a = []string{"AmendPurchaseOrder", "SubmitPOForApproval", "CancelPurchaseOrder"}
	case domain.OrderStatusPendingApproval:
		a = []string{"AmendPurchaseOrder", "CancelPurchaseOrder"}
		if principal == "" || (principal != po.PreparedByPrincipalID && (po.SubmittedByPrincipalID == nil || principal != *po.SubmittedByPrincipalID)) {
			a = append([]string{"ApprovePurchaseOrder"}, a...)
		}
	case domain.OrderStatusApproved:
		a = []string{"IssuePurchaseOrder", "AmendPurchaseOrder", "PlacePOHold", "CancelPurchaseOrder"}
	case domain.OrderStatusIssued:
		a = []string{"AmendPurchaseOrder", "PlacePOHold", "ClosePurchaseOrder", "CancelPurchaseOrder"}
	case domain.OrderStatusOnHold:
		a = []string{"ReleasePOHold", "CancelPurchaseOrder"}
	default:
		a = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"purchase_order_id": po.PurchaseOrderID, "status": po.Status, "version": po.Version, "available_actions": a})
}

// ── amend ───────────────────────────────────────────────────────────────────

// AmendOrder is AmendPurchaseOrder. An approved or issued order is never edited
// in place: the amendment becomes a new revision (the superseded one is
// snapshotted), and a material change — supplier, currency, payment terms,
// price, quantity, UOM, lines added or removed — takes the order back to DRAFT
// for re-approval. A non-material change keeps the status. The legacy body
// ({new_total_amount, reason}) still works for a header-only order.
func (h *Handler) AmendOrder(w http.ResponseWriter, r *http.Request) {
	var req domain.AmendOrderRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "reason")
		return
	}
	if req.NewTotalAmount < 0 {
		writeError(w, http.StatusBadRequest, "invalid_field", "new_total_amount must not be negative")
		return
	}
	if req.CurrencyCode != nil && !validCurrency(*req.CurrencyCode) {
		writeError(w, http.StatusBadRequest, "invalid_field", "currency_code must be a 3-letter ISO 4217 code")
		return
	}
	if req.SupplierRef != nil && strings.TrimSpace(*req.SupplierRef) == "" {
		writeError(w, http.StatusBadRequest, "invalid_field", "supplier_ref must not be empty")
		return
	}
	if req.Lines != nil {
		lines, err := validateLines(req.Lines, true)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_line", err.Error())
			return
		}
		req.Lines = lines
	}
	if req.Lines == nil && req.NewTotalAmount == 0 && req.SupplierRef == nil && req.CurrencyCode == nil && req.DeliveryTerms == nil && req.PaymentTerms == nil {
		writeError(w, http.StatusBadRequest, "missing_field", "nothing to amend: supply lines, new_total_amount or a header field")
		return
	}

	po, ok := h.loadHeader(w, r, "AmendOrder")
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, po.LegalEntityID, actionAmendOrder); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	// A changed supplier must itself be eligible.
	if req.SupplierRef != nil && !strings.EqualFold(*req.SupplierRef, derefString(po.SupplierRef)) {
		if _, err := h.verifySupplier(r.Context(), po.TenantID, principalID, po.LegalEntityID, *req.SupplierRef, ""); err != nil {
			h.writeSupplierErr(w, err)
			return
		}
	}

	res, err := h.store.AmendOrder(r.Context(), po.TenantID, po.PurchaseOrderID, principalID, req)
	if err != nil {
		if errors.Is(err, store.ErrNoChange) {
			writeError(w, http.StatusBadRequest, "no_change", err.Error())
			return
		}
		h.handleTransitionErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		domain.OrderDetail
		Material           bool `json:"material_change"`
		RequiresReapproval bool `json:"requires_reapproval"`
		NewRevision        bool `json:"new_revision"`
	}{res.Order, res.Material, res.RequiresReapproval, res.NewRevision})
}

// ── lifecycle commands ───────────────────────────────────────────────────────

// command builds the handler for one lifecycle command: submit, approve, issue,
// hold, release, cancel, close.
func (h *Handler) command(cmd store.Command) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req domain.CommandRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		if (cmd == store.CmdHold || cmd == store.CmdCancel) && strings.TrimSpace(req.Reason) == "" {
			writeError(w, http.StatusBadRequest, "missing_field", "reason")
			return
		}
		// An approval is of EXACTLY the content the approver reviewed.
		if cmd == store.CmdApprove && req.ExpectedVersion == nil {
			writeError(w, http.StatusBadRequest, "missing_field", "expected_version is required to approve: an approval binds to the exact version reviewed")
			return
		}

		po, ok := h.loadHeader(w, r, string(cmd))
		if !ok {
			return
		}
		principalID, ok := h.requirePrincipal(w, r)
		if !ok {
			return
		}
		if !h.authorizeCommand(w, r, cmd, principalID, po) {
			return
		}

		in := store.TransitionInput{
			TenantID: po.TenantID, OrderID: po.PurchaseOrderID, Actor: principalID, Command: cmd,
			ExpectedVersion: req.ExpectedVersion, Reason: strings.TrimSpace(req.Reason),
		}

		// A supplier that has gone inactive or on hold since the order was
		// created must not be committed to: re-check when it is submitted and
		// when it is issued (the spec's "inactive supplier used in new PO").
		if (cmd == store.CmdSubmit || cmd == store.CmdIssue) && po.SupplierRef != nil {
			exceptionRef := req.SupplierExceptionRef
			if exceptionRef == "" {
				exceptionRef = po.SupplierExceptionRef
			}
			by, err := h.verifySupplier(r.Context(), po.TenantID, principalID, po.LegalEntityID, *po.SupplierRef, exceptionRef)
			if err != nil {
				h.writeSupplierErr(w, err)
				return
			}
			if by != "" {
				in.SupplierExceptionRef, in.SupplierExceptionBy = exceptionRef, by
			}
		}

		d, err := h.store.Transition(r.Context(), in)
		if err != nil {
			h.handleTransitionErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, d)
	}
}

// authorizeCommand maps a command to its permission. Approval additionally goes
// through authorization-svc's own-object SoD layer with the preparer as the
// owner, and above the high-value threshold needs the high-value permission.
func (h *Handler) authorizeCommand(w http.ResponseWriter, r *http.Request, cmd store.Command, principalID string, po *domain.PurchaseOrder) bool {
	var err error
	switch cmd {
	case store.CmdSubmit:
		err = h.authz.CheckAllowed(r.Context(), principalID, po.LegalEntityID, actionSubmitOrder)
	case store.CmdApprove:
		action := actionApproveOrder
		if h.opts.HighValueThreshold > 0 && po.TotalAmount > h.opts.HighValueThreshold {
			action = actionApproveHighValue
		}
		err = h.authz.CheckAllowedOwnObject(r.Context(), principalID, po.LegalEntityID, action, po.PreparedByPrincipalID)
	case store.CmdIssue:
		err = h.authz.CheckAllowed(r.Context(), principalID, po.LegalEntityID, actionIssueOrder)
	case store.CmdHold, store.CmdRelease:
		err = h.authz.CheckAllowed(r.Context(), principalID, po.LegalEntityID, actionHoldOrder)
	case store.CmdCancel:
		err = h.authz.CheckAllowed(r.Context(), principalID, po.LegalEntityID, actionCancelOrder)
	case store.CmdClose:
		err = h.authz.CheckAllowed(r.Context(), principalID, po.LegalEntityID, actionCloseOrder)
	}
	if err != nil {
		h.writeAuthzErr(w, err)
		return false
	}
	return true
}

// ── progress ────────────────────────────────────────────────────────────────

// RecordProgress takes a received/invoiced delta pushed by AP-04/AP-05.
// Idempotent on (tenant, source_ref, kind); it refuses progress beyond what was
// ordered (409 PROGRESS_EXCEEDS_ORDER, permanent) and on an order that is not
// ISSUED (409 ORDER_NOT_ISSUED).
func (h *Handler) RecordProgress(w http.ResponseWriter, r *http.Request) {
	var req domain.ProgressRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Kind = strings.ToUpper(strings.TrimSpace(req.Kind))
	switch {
	case req.Kind != domain.ProgressReceived && req.Kind != domain.ProgressInvoiced:
		writeError(w, http.StatusBadRequest, "invalid_field", "kind must be RECEIVED or INVOICED")
		return
	case req.Quantity <= 0:
		writeError(w, http.StatusBadRequest, "invalid_field", "quantity must be greater than zero")
		return
	case req.Amount < 0:
		writeError(w, http.StatusBadRequest, "invalid_field", "amount must not be negative")
		return
	case req.DeltaSign != 1 && req.DeltaSign != -1:
		writeError(w, http.StatusBadRequest, "invalid_field", "delta_sign must be 1 or -1")
		return
	case strings.TrimSpace(req.SourceRef) == "":
		writeError(w, http.StatusBadRequest, "missing_field", "source_ref")
		return
	}
	lineID := chi.URLParam(r, "line_id")
	if !isUUID(lineID) {
		writeError(w, http.StatusNotFound, "line_not_found", "")
		return
	}

	po, ok := h.loadHeader(w, r, "RecordProgress")
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, po.LegalEntityID, actionRecordProgress); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	res, err := h.store.RecordProgress(r.Context(), po.TenantID, po.PurchaseOrderID, lineID, principalID, req, r.Header.Get("X-Correlation-ID"))
	if err != nil {
		h.handleProgressErr(w, err)
		return
	}
	status := http.StatusCreated
	if res.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, res)
}

// ── supplier eligibility ─────────────────────────────────────────────────────

// verifySupplier checks AP-01's verdict for a NEW commitment to the supplier. An
// ineligible supplier is refused, with one exception: a supplier merely ON HOLD
// may be used when an exception reference is supplied AND the caller holds
// PO_SUPPLIER_EXCEPTION — the spec's "unless an approved exception is supplied".
// It returns the principal recorded as having granted the exception, or "".
// With no supplier_ref (a legacy order) there is nothing to check. It fails closed.
func (h *Handler) verifySupplier(ctx context.Context, tenantID, principalID, legalEntityID, supplierRef, exceptionRef string) (exceptionBy string, err error) {
	if strings.TrimSpace(supplierRef) == "" {
		return "", nil
	}
	if h.supplier == nil {
		// Never silently skip a control that is meant to exist.
		return "", domain.ErrSupplierServiceUnavailable
	}
	el, err := h.supplier.Eligibility(ctx, tenantID, principalID, legalEntityID, strings.TrimSpace(supplierRef))
	if err != nil {
		return "", err
	}
	if el.EligibleForNewCommitments {
		return "", nil
	}
	if el.IsOnHold && strings.TrimSpace(exceptionRef) != "" {
		if err := h.authz.CheckAllowed(ctx, principalID, legalEntityID, actionSupplierException); err != nil {
			if errors.Is(err, domain.ErrAuthorizationDenied) {
				return "", domain.ErrSupplierNotEligible
			}
			return "", err
		}
		return principalID, nil
	}
	return "", domain.ErrSupplierNotEligible
}

// ── errors ───────────────────────────────────────────────────────────────────

func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrAuthorizationDenied):
		writeError(w, http.StatusForbidden, "authorization_denied", "")
	default:
		h.log.Error("authorization check failed — failing closed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "authorization_service_unavailable", "")
	}
}

// writeStoreErr maps store errors to HTTP responses. A malformed identifier
// (e.g. a non-UUID in a path or body field) is a caller error, not an
// infrastructure failure — it must never surface as 503.
func (h *Handler) writeStoreErr(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, domain.ErrInvalidIdentifier):
		writeError(w, http.StatusBadRequest, "invalid_identifier", "")
	default:
		h.log.Error(op+": store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
	}
}

func (h *Handler) writePurchaseRequestErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrPurchaseRequestNotFound):
		writeError(w, http.StatusUnprocessableEntity, "purchase_request_not_found", err.Error())
	case errors.Is(err, domain.ErrPurchaseRequestNotApproved):
		writeError(w, http.StatusUnprocessableEntity, "purchase_request_not_approved", err.Error())
	case errors.Is(err, domain.ErrPurchaseRequestMismatch):
		writeError(w, http.StatusUnprocessableEntity, "purchase_request_mismatch", err.Error())
	default:
		h.log.Error("purchase-request-svc verification failed — failing closed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "purchase_request_service_unavailable", "")
	}
}

// writeApprovalErr covers the approval-basis verification of the legacy issue.
func (h *Handler) writeApprovalErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrPurchaseRequestNotFound), errors.Is(err, domain.ErrPurchaseRequestNotApproved),
		errors.Is(err, domain.ErrPurchaseRequestMismatch), errors.Is(err, domain.ErrPurchaseRequestServiceUnavailable):
		h.writePurchaseRequestErr(w, err)
	case errors.Is(err, domain.ErrApprovalRequired):
		writeError(w, http.StatusUnprocessableEntity, "po_approval_required",
			"supply an APPROVED purchase_request_id or an approved procurement case id as correlation_id, or use the governed flow: POST /v1/purchase-orders/draft, then submit, approve and issue")
	case errors.Is(err, domain.ErrProcurementCaseNotApproved), errors.Is(err, domain.ErrProcurementCaseMismatch):
		writeError(w, http.StatusUnprocessableEntity, "po_approval_required", err.Error())
	case errors.Is(err, domain.ErrProcurementCaseServiceUnavailable):
		writeError(w, http.StatusServiceUnavailable, "procurement_case_service_unavailable", "")
	default:
		h.log.Error("approval verification failed — failing closed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "approval_verification_unavailable", "")
	}
}

func (h *Handler) writeSupplierErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrSupplierNotEligible):
		writeError(w, http.StatusUnprocessableEntity, "supplier_not_eligible", err.Error())
	case errors.Is(err, domain.ErrSupplierUnknown):
		writeError(w, http.StatusUnprocessableEntity, "supplier_unknown", err.Error())
	case errors.Is(err, domain.ErrAuthorizationServiceUnavailable):
		writeError(w, http.StatusServiceUnavailable, "authorization_service_unavailable", "")
	default:
		writeError(w, http.StatusServiceUnavailable, "supplier_service_unavailable", "")
	}
}

func (h *Handler) handleTransitionErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrOrderNotFound), errors.Is(err, domain.ErrInvalidIdentifier):
		writeError(w, http.StatusNotFound, "order_not_found", "")
	case errors.Is(err, domain.ErrStaleVersion):
		writeError(w, http.StatusConflict, "stale_version", err.Error())
	case errors.Is(err, domain.ErrSoDConflict):
		writeError(w, http.StatusForbidden, "sod_conflict", err.Error())
	case errors.Is(err, domain.ErrNoLines):
		writeError(w, http.StatusUnprocessableEntity, "no_lines", err.Error())
	case errors.Is(err, domain.ErrHasProgress):
		writeError(w, http.StatusConflict, "has_progress", err.Error())
	case errors.Is(err, domain.ErrAmendmentBelowProgress):
		writeError(w, http.StatusConflict, "amendment_below_progress", err.Error())
	case errors.Is(err, domain.ErrInvalidLine):
		writeError(w, http.StatusBadRequest, "invalid_line", err.Error())
	case errors.Is(err, domain.ErrInvalidTransition):
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", domain.ErrInvalidTransition.Error())
	default:
		h.log.Error("store: transition failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
	}
}

func (h *Handler) handleProgressErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrOrderNotFound), errors.Is(err, domain.ErrInvalidIdentifier):
		writeError(w, http.StatusNotFound, "order_not_found", "")
	case errors.Is(err, domain.ErrLineNotFound):
		writeError(w, http.StatusNotFound, "line_not_found", err.Error())
	case errors.Is(err, domain.ErrOrderNotIssued):
		writeError(w, http.StatusConflict, "order_not_issued", err.Error())
	case errors.Is(err, domain.ErrProgressExceedsOrder):
		writeError(w, http.StatusConflict, "progress_exceeds_order", err.Error())
	case errors.Is(err, domain.ErrProgressBelowZero):
		writeError(w, http.StatusConflict, "progress_below_zero", err.Error())
	case errors.Is(err, domain.ErrProgressRefReused):
		writeError(w, http.StatusConflict, "progress_ref_reused", err.Error())
	default:
		h.log.Error("RecordProgress: store failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

var currencyRE = regexp.MustCompile(`^[A-Za-z]{3}$`)

func validCurrency(c string) bool { return currencyRE.MatchString(c) }

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// validateLines checks and normalizes request lines. A line's amount is always
// quantity x unit price; a client-supplied line_amount must agree with it.
func validateLines(in []domain.LineInput, required bool) ([]domain.LineInput, error) {
	if len(in) == 0 {
		if required {
			return nil, errors.New("at least one line is required")
		}
		return nil, nil
	}
	if len(in) > maxLinesPerOrder {
		return nil, errors.New("too many lines")
	}
	seen := map[int]bool{}
	out := make([]domain.LineInput, 0, len(in))
	for i, l := range in {
		pos := strconv.Itoa(i + 1)
		switch {
		case strings.TrimSpace(l.ItemRef) == "" && strings.TrimSpace(l.Description) == "":
			return nil, errors.New("line " + pos + ": item_ref or description is required")
		case l.Quantity <= 0:
			return nil, errors.New("line " + pos + ": quantity must be greater than zero")
		case l.UnitPrice < 0:
			return nil, errors.New("line " + pos + ": unit_price must not be negative")
		case strings.TrimSpace(l.UOM) == "":
			return nil, errors.New("line " + pos + ": uom is required")
		case l.LineNumber < 0:
			return nil, errors.New("line " + pos + ": line_number must not be negative")
		}
		if l.LineNumber != 0 {
			if seen[l.LineNumber] {
				return nil, errors.New("line " + pos + ": line_number " + strconv.Itoa(l.LineNumber) + " appears twice")
			}
			seen[l.LineNumber] = true
		}
		if l.LineAmount != 0 && abs(l.LineAmount-l.Amount()) > 0.01 {
			return nil, errors.New("line " + pos + ": line_amount does not equal quantity x unit_price")
		}
		l.LineAmount = l.Amount()
		out = append(out, l)
	}
	return out, nil
}

// requiredFieldMissing deliberately does NOT require tenant_id: the tenant is
// the caller's verified scope, so a body omitting it is fine and a body
// disagreeing with it is a 403, not a missing field.
func requiredFieldMissing(req domain.IssueOrderRequest) string {
	switch {
	case req.LegalEntityID == "":
		return "legal_entity_id"
	case req.CurrencyCode == "":
		return "currency_code"
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
// IdentityContextEnvelope JWT. This service never decodes a JWT itself. A
// request with no resolved principal never passed identity verification — fail
// closed with 401.
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

// errorResponse keeps the existing {"error", "detail"} shape and adds a stable
// upper-case machine "code" (cross-service clients key on it, e.g. AP-04 reads
// PROGRESS_EXCEEDS_ORDER).
type errorResponse struct {
	Error  string `json:"error"`
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

func writeError(w http.ResponseWriter, status int, code, detail string) {
	writeJSON(w, status, errorResponse{Error: code, Code: strings.ToUpper(code), Detail: detail})
}

// maxRequestBytes caps a JSON request body. A bare json.Decoder reads until EOF,
// so without this a single request can make the service allocate whatever the
// client is willing to send.
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
