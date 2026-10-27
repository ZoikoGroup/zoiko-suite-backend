// Package handler exposes purchase-request-svc's REST API (AP-02).
//
// Commands (all writes carry the canonical envelope and honour Idempotency-Key
// — see internal/idempotency):
//
//	POST /v1/purchase-requests                       CreateRequisition (-> DRAFT)
//	POST /v1/purchase-requests/{id}/amend            AmendRequisition
//	POST /v1/purchase-requests/{id}/submit           SubmitRequisition (budget check)
//	POST /v1/purchase-requests/{id}/approve          ApproveRequisition (expected_version required)
//	POST /v1/purchase-requests/{id}/reject           RejectRequisition  (expected_version required)
//	POST /v1/purchase-requests/{id}/cancel           CancelRequisition
//	POST /v1/purchase-requests/{id}/convert-to-purchase-order   ConvertToPurchaseOrder
//
// Queries: GET /v1/purchase-requests, /{id}, /{id}/approval-status,
// /{id}/available-actions, /{id}/history. Every read is authorized
// (PR_REQUEST_READ) and tenant-scoped.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/purchase-request-svc/internal/domain"
	svcmiddleware "zoiko.io/purchase-request-svc/internal/middleware"
	"zoiko.io/purchase-request-svc/internal/purchaseorder"
	"zoiko.io/purchase-request-svc/internal/spendcontrols"
	"zoiko.io/purchase-request-svc/internal/store"
)

// Store is the persistence contract the handler depends on.
type Store interface {
	CreateRequest(ctx context.Context, r *domain.PurchaseRequest) (created bool, err error)
	GetRequest(ctx context.Context, requestID string) (*domain.PurchaseRequest, error)
	ListRequests(ctx context.Context, filter domain.ListRequestsFilter) ([]domain.PurchaseRequest, error)
	GetHistory(ctx context.Context, requestID string) ([]domain.HistoryEntry, error)
	Apply(ctx context.Context, t store.Transition) (*domain.PurchaseRequest, error)
	AmendRequest(ctx context.Context, a store.Amendment) (*domain.PurchaseRequest, bool, error)
}

// AuthZClient is the authorization contract the handler depends on.
type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
	CheckAllowedOwnObject(ctx context.Context, principalID, legalEntityID, actionType, resourceOwnerPrincipalID string) error
}

// Action types checked against authorization-svc (spec §5:
// requisition.read/create/manage; .submit; .approve; .cancel).
const (
	actionReadRequest    = "PR_REQUEST_READ"
	actionCreateRequest  = "PR_REQUEST_CREATE"
	actionManageRequest  = "PR_REQUEST_MANAGE"
	actionSubmitRequest  = "PR_REQUEST_SUBMIT"
	actionApproveRequest = "PR_REQUEST_APPROVE"
	actionRejectRequest  = "PR_REQUEST_REJECT"
	actionCancelRequest  = "PR_REQUEST_CANCEL"
	actionConvertRequest = "PR_REQUEST_CONVERT"
)

// Stable machine-readable error codes (spec §16).
const (
	codeValidation     = "VALIDATION_FAILED"
	codeForbidden      = "FORBIDDEN"
	codeSoD            = "SOD_CONFLICT"
	codeStale          = "STALE_VERSION"
	codeInvalidTrans   = "INVALID_TRANSITION"
	codeNotFound       = "REQUEST_NOT_FOUND"
	codeStore          = "STORE_UNAVAILABLE"
	codeDependency     = "DEPENDENCY_UNAVAILABLE"
	codeBudgetBlocked  = "BUDGET_BLOCKED"
	codeBudgetRequired = "BUDGET_CHECK_REQUIRED"
	codeExpired        = "REQUISITION_EXPIRED"
	codePORefused      = "PURCHASE_ORDER_REFUSED"
	codeTenant         = "TENANT_SCOPE_INVALID"
	codeIdentity       = "IDENTITY_MISSING"
)

// Config is the policy the handler applies.
type Config struct {
	// ControlledCategories: categories needing an ALLOWED budget decision;
	// "*" = all. Empty = none (budget check skipped).
	ControlledCategories []string
	// ApprovalThreshold: above it the approver must be independent of every
	// maker. 0 = no threshold.
	ApprovalThreshold float64
	// MakerChecker: requester can never approve own requisition (any amount).
	MakerChecker bool
}

type Handler struct {
	store Store
	authz AuthZClient
	spend spendcontrols.Client // nil = no budget service configured (controlled categories then fail closed)
	po    purchaseorder.Client
	cfg   Config
	log   *zap.Logger
	now   func() time.Time
}

func New(store Store, authz AuthZClient, spend spendcontrols.Client, po purchaseorder.Client, cfg Config, log *zap.Logger) *Handler {
	return &Handler{store: store, authz: authz, spend: spend, po: po, cfg: cfg, log: log, now: func() time.Time { return time.Now().UTC() }}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/purchase-requests", func(r chi.Router) {
		r.Post("/", h.CreateRequest)
		r.Get("/", h.ListRequests)
		r.Get("/{request_id}", h.GetRequest)
		r.Get("/{request_id}/approval-status", h.GetApprovalStatus)
		r.Get("/{request_id}/available-actions", h.GetAvailableActions)
		r.Get("/{request_id}/history", h.GetRequisitionHistory)
		r.Post("/{request_id}/amend", h.AmendRequest)
		r.Post("/{request_id}/submit", h.SubmitRequest)
		r.Post("/{request_id}/approve", h.ApproveRequest)
		r.Post("/{request_id}/reject", h.RejectRequest)
		r.Post("/{request_id}/cancel", h.CancelRequest)
		r.Post("/{request_id}/convert-to-purchase-order", h.ConvertToPurchaseOrder)
	})
}

// ── validation ───────────────────────────────────────────────────────────────

type validationError string

func (e validationError) Error() string { return string(e) }

func vErr(format string, a ...any) error { return validationError(fmt.Sprintf(format, a...)) }

func validDate(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// buildLines converts caller input into validated lines. Every line carries the
// requisition's currency (a requisition is single-currency: the budget check and
// the PO both compare amounts in one currency).
func buildLines(in []domain.RequestLineInput, currency string) ([]domain.RequestLine, error) {
	if len(in) == 0 {
		return nil, vErr("at least one line is required")
	}
	out := make([]domain.RequestLine, 0, len(in))
	for i, l := range in {
		n := i + 1
		if l.Quantity <= 0 {
			return nil, vErr("line %d: quantity must be greater than zero", n)
		}
		if l.Amount <= 0 {
			return nil, vErr("line %d: amount must be greater than zero", n)
		}
		if l.ItemRef == "" && l.Description == "" {
			return nil, vErr("line %d: item_ref or description is required", n)
		}
		cur := l.CurrencyCode
		if cur == "" {
			cur = currency
		}
		if cur != currency {
			return nil, vErr("line %d: currency_code %q differs from the requisition currency %q", n, cur, currency)
		}
		if l.RequiredDate != "" && !validDate(l.RequiredDate) {
			return nil, vErr("line %d: required_date must be YYYY-MM-DD", n)
		}
		cat := strings.TrimSpace(l.Category)
		if cat == "" {
			cat = "GENERAL"
		}
		attach := l.AttachmentRefs
		if attach == nil {
			attach = []string{}
		}
		out = append(out, domain.RequestLine{
			LineNumber: n, ItemRef: l.ItemRef, Description: l.Description, Category: cat,
			Quantity: l.Quantity, UnitOfMeasure: l.UnitOfMeasure, Amount: domain.Round2(l.Amount), CurrencyCode: cur,
			RequiredDate: strPtrOrNil(l.RequiredDate), CostCenter: l.CostCenter, ProjectRef: l.ProjectRef,
			BudgetRef: l.BudgetRef, PreferredSupplierRef: l.PreferredSupplierRef, AttachmentRefs: attach,
		})
	}
	return out, nil
}

// ── POST /v1/purchase-requests ───────────────────────────────────────────────

// CreateRequest files a requisition as a DRAFT. The original header-only body
// (amount, no lines) is still accepted: it becomes a single GENERAL line.
func (h *Handler) CreateRequest(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateRequestRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if missing := requiredFieldMissing(req); missing != "" {
		writeError(w, http.StatusBadRequest, "missing_field", missing)
		return
	}
	if len(req.Lines) == 0 && req.Amount <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_field", "amount must be greater than zero (or supply lines)")
		return
	}
	if req.RequiredDate != "" && !validDate(req.RequiredDate) {
		writeError(w, http.StatusBadRequest, "invalid_field", "required_date must be YYYY-MM-DD")
		return
	}

	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// tenant_id in the body is accepted only when it agrees with the verified
	// scope. It used to be the ONLY source of the stored tenant_id — and the
	// store handed that same body value to set_config('app.tenant_id'), so the
	// tenant the request named satisfied the RLS policy on the way past.
	if req.TenantID != "" && req.TenantID != tenantID {
		writeError(w, http.StatusForbidden, "tenant_scope_mismatch", domain.ErrTenantScopeMismatch.Error())
		return
	}
	// legal_entity_id reaches a uuid column on insert; a malformed one died
	// inside the driver as 22P02 and surfaced as 503 store_unavailable.
	if !isUUID(req.LegalEntityID) {
		writeError(w, http.StatusBadRequest, "invalid_field", "legal_entity_id must be a UUID")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionCreateRequest); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	lines := req.Lines
	if len(lines) == 0 {
		lines = []domain.RequestLineInput{{Description: req.Description, Category: "GENERAL", Quantity: 1, Amount: req.Amount}}
	}
	built, err := buildLines(lines, req.CurrencyCode)
	if err != nil {
		writeCoded(w, http.StatusBadRequest, "invalid_field", codeValidation, err.Error())
		return
	}
	purpose := req.BusinessPurpose
	if purpose == "" {
		purpose = req.Description
	}
	attach := req.AttachmentRefs
	if attach == nil {
		attach = []string{}
	}
	pr := &domain.PurchaseRequest{
		RequestID:              uuid.NewString(),
		TenantID:               tenantID,
		LegalEntityID:          req.LegalEntityID,
		RequestedByPrincipalID: principalID,
		Description:            req.Description,
		CurrencyCode:           req.CurrencyCode,
		BusinessPurpose:        purpose,
		CostCenter:             req.CostCenter,
		ProjectRef:             req.ProjectRef,
		BudgetRef:              req.BudgetRef,
		PreferredSupplierRef:   req.PreferredSupplierRef,
		RequiredDate:           strPtrOrNil(req.RequiredDate),
		AttachmentRefs:         attach,
		ExpiresAt:              req.ExpiresAt,
		Lines:                  built,
		CorrelationID:          req.CorrelationID,
	}
	created, err := h.store.CreateRequest(r.Context(), pr)
	if err != nil {
		h.writeStoreErr(w, "CreateRequest", err)
		return
	}
	if !created {
		// Replay of a prior request with the same correlation_id — return the
		// original request; the created event was emitted when it was created.
		writeJSON(w, http.StatusOK, pr)
		return
	}
	writeJSON(w, http.StatusCreated, pr)
}

// ── reads ────────────────────────────────────────────────────────────────────

// authorizeRead authorizes a read of an object in legalEntityID. A request that
// carries no principal is served ONLY when it declares itself an internal
// service call (X-Source-Channel: system — e.g. purchase-order-svc verifying a
// requisition is APPROVED); it is still tenant-scoped. Anything else must have
// a principal holding PR_REQUEST_READ.
func (h *Handler) authorizeRead(w http.ResponseWriter, r *http.Request, scope string) bool {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		if strings.EqualFold(r.Header.Get("X-Source-Channel"), "system") {
			return true
		}
		writeCoded(w, http.StatusUnauthorized, "identity_missing", codeIdentity, domain.ErrIdentityMissing.Error())
		return false
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, scope, actionReadRequest); err != nil {
		h.writeAuthzErr(w, err)
		return false
	}
	return true
}

// loadForRead resolves {request_id} within the caller's tenant and authorizes
// the read against the request's legal entity.
func (h *Handler) loadForRead(w http.ResponseWriter, r *http.Request) (*domain.PurchaseRequest, bool) {
	if _, ok := h.requireTenant(w, r); !ok {
		return nil, false
	}
	pr, err := h.store.GetRequest(r.Context(), chi.URLParam(r, "request_id"))
	if err != nil {
		h.writeStoreErr(w, "GetRequest", err)
		return nil, false
	}
	if pr == nil {
		writeCoded(w, http.StatusNotFound, "request_not_found", codeNotFound, "")
		return nil, false
	}
	if !h.authorizeRead(w, r, pr.LegalEntityID) {
		return nil, false
	}
	return pr, true
}

// GET /v1/purchase-requests/{request_id}
func (h *Handler) GetRequest(w http.ResponseWriter, r *http.Request) {
	if pr, ok := h.loadForRead(w, r); ok {
		writeJSON(w, http.StatusOK, pr)
	}
}

// ListRequests returns the caller's own tenant's register.
//
// The scope comes from the verified X-Tenant-Id header. It used to come from
// ?tenant_id=, which the store both filtered on AND set app.tenant_id from — so
// `?tenant_id=<any-uuid>` returned that tenant's entire register.
func (h *Handler) ListRequests(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	if claimed := q.Get("tenant_id"); claimed != "" && claimed != tenantID {
		writeError(w, http.StatusForbidden, "tenant_scope_mismatch", domain.ErrTenantScopeMismatch.Error())
		return
	}
	if status := q.Get("status"); status != "" && !domain.ValidRequestStatus(status) {
		writeError(w, http.StatusBadRequest, "invalid_field", "status is not a recognised purchase request status")
		return
	}
	// legal_entity_id is compared as `legal_entity_id::text = $n`, so a
	// malformed value would silently match nothing. Refused here instead.
	legalEntityID := q.Get("legal_entity_id")
	if legalEntityID != "" && !isUUID(legalEntityID) {
		writeError(w, http.StatusBadRequest, "invalid_field", "legal_entity_id must be a UUID")
		return
	}
	scope := legalEntityID
	if scope == "" {
		scope = tenantID
	}
	if !h.authorizeRead(w, r, scope) {
		return
	}
	filter := domain.ListRequestsFilter{TenantID: tenantID, LegalEntityID: legalEntityID, Status: q.Get("status")}
	list, err := h.store.ListRequests(r.Context(), filter)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidIdentifier) {
			writeError(w, http.StatusBadRequest, "invalid_field", "tenant scope must be a UUID")
			return
		}
		h.writeStoreErr(w, "ListRequests", err)
		return
	}
	if list == nil {
		list = []domain.PurchaseRequest{}
	}
	writeJSON(w, http.StatusOK, list)
}

// GET /{id}/approval-status
func (h *Handler) GetApprovalStatus(w http.ResponseWriter, r *http.Request) {
	pr, ok := h.loadForRead(w, r)
	if !ok {
		return
	}
	above := h.aboveThreshold(pr)
	writeJSON(w, http.StatusOK, domain.ApprovalStatus{
		RequestID: pr.RequestID, Status: pr.Status, Version: pr.Version, Amount: pr.Amount, CurrencyCode: pr.CurrencyCode,
		ApprovalThreshold: h.cfg.ApprovalThreshold, AboveThreshold: above,
		IndependentApproverNeeded: above, BudgetDecision: pr.BudgetDecision,
		ApprovedBy: pr.ApprovedByPrincipalID, ApprovedAt: pr.ApprovedAt, RejectedBy: pr.RejectedByPrincipalID,
		ApprovalInvalidatedCount: pr.ApprovalInvalidatedCount, ConvertedPurchaseOrderID: pr.ConvertedPurchaseOrderID,
	})
}

// GET /{id}/available-actions — what the CALLER may do next, given the state
// and segregation of duties (it hides Approve from the requester).
func (h *Handler) GetAvailableActions(w http.ResponseWriter, r *http.Request) {
	pr, ok := h.loadForRead(w, r)
	if !ok {
		return
	}
	actions := []string{}
	caller := r.Header.Get("X-Principal-Id")
	switch pr.Status {
	case domain.RequestStatusDraft:
		actions = append(actions, "AmendRequisition", "SubmitRequisition", "CancelRequisition")
	case domain.RequestStatusPending:
		if h.sodViolation(pr, caller) == nil {
			actions = append(actions, "ApproveRequisition")
		}
		if caller != pr.RequestedByPrincipalID {
			actions = append(actions, "RejectRequisition")
		}
		actions = append(actions, "AmendRequisition", "CancelRequisition")
	case domain.RequestStatusApproved:
		actions = append(actions, "ConvertToPurchaseOrder", "AmendRequisition", "CancelRequisition")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"request_id": pr.RequestID, "status": pr.Status, "version": pr.Version, "available_actions": actions,
	})
}

// GET /{id}/history
func (h *Handler) GetRequisitionHistory(w http.ResponseWriter, r *http.Request) {
	pr, ok := h.loadForRead(w, r)
	if !ok {
		return
	}
	hist, err := h.store.GetHistory(r.Context(), pr.RequestID)
	if err != nil {
		h.writeStoreErr(w, "GetHistory", err)
		return
	}
	if hist == nil {
		hist = []domain.HistoryEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"request_id": pr.RequestID, "history": hist})
}

// ── commands ─────────────────────────────────────────────────────────────────

// commandCtx is what every command needs before it touches the object.
type commandCtx struct {
	tenantID  string
	principal string
	pr        *domain.PurchaseRequest
}

// prepare resolves tenant, principal and the request. It does NOT authorize —
// each command authorizes the specific action against the request's entity.
func (h *Handler) prepare(w http.ResponseWriter, r *http.Request) (*commandCtx, bool) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return nil, false
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return nil, false
	}
	pr, err := h.store.GetRequest(r.Context(), chi.URLParam(r, "request_id"))
	if err != nil {
		h.writeStoreErr(w, "GetRequest", err)
		return nil, false
	}
	if pr == nil {
		writeCoded(w, http.StatusNotFound, "request_not_found", codeNotFound, "")
		return nil, false
	}
	return &commandCtx{tenantID: tenantID, principal: principalID, pr: pr}, true
}

func (h *Handler) meta(r *http.Request, actor, reason string) store.Meta {
	corr := r.Header.Get("X-Correlation-ID")
	return store.Meta{Actor: actor, CorrelationID: corr, Reason: reason}
}

// expectedVersion reads expected_version from the body, falling back to the
// X-Expected-Version envelope header. required=true makes absence a 400.
func expectedVersion(w http.ResponseWriter, r *http.Request, body *int, required bool) (*int, bool) {
	v := body
	if v == nil {
		if hv := strings.TrimSpace(r.Header.Get("X-Expected-Version")); hv != "" {
			n, err := strconv.Atoi(hv)
			if err != nil || n < 1 {
				writeCoded(w, http.StatusBadRequest, "invalid_field", codeValidation, "X-Expected-Version must be a positive integer")
				return nil, false
			}
			v = &n
		}
	}
	if v == nil && required {
		writeCoded(w, http.StatusBadRequest, "expected_version_required", codeValidation,
			"expected_version is required for this transition; read it from the requisition's version")
		return nil, false
	}
	if v != nil && *v < 1 {
		writeCoded(w, http.StatusBadRequest, "invalid_field", codeValidation, "expected_version must be a positive integer")
		return nil, false
	}
	return v, true
}

// POST /{id}/amend
//
// Any amendment of a PENDING_APPROVAL or APPROVED requisition drops it back to
// DRAFT and invalidates the approval (stricter than "material changes only":
// the approved content is exactly what was approved, or it is not approved).
func (h *Handler) AmendRequest(w http.ResponseWriter, r *http.Request) {
	var req domain.AmendRequestRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	cc, ok := h.prepare(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), cc.principal, cc.pr.LegalEntityID, actionManageRequest); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	ev, ok := expectedVersion(w, r, req.ExpectedVersion, false)
	if !ok {
		return
	}
	if req.RequiredDate != nil && *req.RequiredDate != "" && !validDate(*req.RequiredDate) {
		writeError(w, http.StatusBadRequest, "invalid_field", "required_date must be YYYY-MM-DD")
		return
	}
	m := h.meta(r, cc.principal, req.Reason)
	upd, invalidated, err := h.store.AmendRequest(r.Context(), store.Amendment{
		TenantID: cc.tenantID, RequestID: cc.pr.RequestID, ExpectedVersion: ev, Meta: m,
		Apply: func(w2 *domain.PurchaseRequest) error {
			if req.Description != nil {
				w2.Description = *req.Description
			}
			if req.BusinessPurpose != nil {
				w2.BusinessPurpose = *req.BusinessPurpose
			}
			if req.CostCenter != nil {
				w2.CostCenter = *req.CostCenter
			}
			if req.ProjectRef != nil {
				w2.ProjectRef = *req.ProjectRef
			}
			if req.BudgetRef != nil {
				w2.BudgetRef = *req.BudgetRef
			}
			if req.PreferredSupplierRef != nil {
				w2.PreferredSupplierRef = *req.PreferredSupplierRef
			}
			if req.RequiredDate != nil {
				w2.RequiredDate = strPtrOrNil(*req.RequiredDate)
			}
			if req.AttachmentRefs != nil {
				w2.AttachmentRefs = *req.AttachmentRefs
			}
			if req.ExpiresAt != nil {
				w2.ExpiresAt = req.ExpiresAt
			}
			if req.Lines != nil {
				lines, err := buildLines(*req.Lines, w2.CurrencyCode)
				if err != nil {
					return err
				}
				w2.Lines = lines
			}
			if w2.Description == "" {
				return vErr("description must not be empty")
			}
			return nil
		},
	})
	if err != nil {
		h.writeDomainErr(w, "AmendRequest", err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		*domain.PurchaseRequest
		ApprovalInvalidated bool `json:"approval_invalidated"`
	}{upd, invalidated})
}

// controlled reports whether category needs an ALLOWED budget decision.
func (h *Handler) controlled(category string) bool {
	for _, c := range h.cfg.ControlledCategories {
		if c == "*" || strings.EqualFold(c, category) {
			return true
		}
	}
	return false
}

// POST /{id}/submit — DRAFT -> PENDING_APPROVAL, after the budget/policy check.
//
// Every controlled category is checked against spend-controls-svc. A BLOCKED
// decision refuses the submission; an unreachable service (or none configured)
// refuses it too — the check is never skipped for a controlled category.
func (h *Handler) SubmitRequest(w http.ResponseWriter, r *http.Request) {
	var req domain.VersionedRequest
	if !decodeOptionalJSON(w, r, &req) {
		return
	}
	cc, ok := h.prepare(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), cc.principal, cc.pr.LegalEntityID, actionSubmitRequest); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	ev, ok := expectedVersion(w, r, req.ExpectedVersion, false)
	if !ok {
		return
	}
	pr := cc.pr
	if ev != nil && pr.Version != *ev {
		writeCoded(w, http.StatusConflict, "stale_version", codeStale, "")
		return
	}
	if pr.Status != domain.RequestStatusDraft {
		h.writeDomainErr(w, "SubmitRequest", domain.ErrInvalidTransition)
		return
	}

	outcome, err := h.budgetCheck(r.Context(), cc, pr)
	if err != nil {
		h.writeDomainErr(w, "SubmitRequest", err)
		return
	}

	version := pr.Version
	m := h.meta(r, cc.principal, "")
	m.Details = map[string]any{"budget_decision": outcome.Decision, "budget_basis": outcome.Basis}
	upd, err := h.store.Apply(r.Context(), store.Transition{
		TenantID: cc.tenantID, RequestID: pr.RequestID, ExpectedVersion: &version,
		From: []domain.RequestStatus{domain.RequestStatusDraft}, To: domain.RequestStatusPending,
		Action: "SUBMITTED", Meta: m, Budget: outcome,
		Events: []store.EventSpec{{Type: "PurchaseRequisitionSubmitted"}},
	})
	if err != nil {
		h.writeDomainErr(w, "SubmitRequest", err)
		return
	}
	writeJSON(w, http.StatusOK, upd)
}

// budgetCheck runs the per-category spend checks and folds them into one
// recorded outcome.
func (h *Handler) budgetCheck(ctx context.Context, cc *commandCtx, pr *domain.PurchaseRequest) (*store.BudgetOutcome, error) {
	byCat := map[string]float64{}
	order := []string{}
	for _, l := range pr.Lines {
		if !h.controlled(l.Category) {
			continue
		}
		key := strings.ToUpper(l.Category)
		if _, seen := byCat[key]; !seen {
			order = append(order, key)
		}
		byCat[key] += l.Amount
	}
	if len(order) == 0 {
		return &store.BudgetOutcome{Decision: domain.BudgetNotRequired, Basis: "no controlled category on this requisition"}, nil
	}
	if h.spend == nil {
		return nil, domain.ErrBudgetUnavailable
	}
	var basis []string
	for _, cat := range order {
		res, err := h.spend.Check(ctx, spendcontrols.CheckInput{
			TenantID: cc.tenantID, PrincipalID: cc.principal, LegalEntityID: pr.LegalEntityID,
			Category: cat, CurrencyCode: pr.CurrencyCode, Amount: domain.Round2(byCat[cat]),
			CorrelationID:   fmt.Sprintf("pr:%s:v%d:%s", pr.RequestID, pr.Version, cat),
			SourceReference: pr.RequestID,
		})
		if err != nil {
			h.log.Error("budget check unavailable — controlled category cannot be submitted",
				zap.String("request_id", pr.RequestID), zap.String("category", cat), zap.Error(err))
			return nil, domain.ErrBudgetUnavailable
		}
		if res.Outcome != "ALLOWED" {
			return nil, fmt.Errorf("%w: %s (%s)", domain.ErrBudgetBlocked, cat, res.Basis)
		}
		basis = append(basis, cat+": "+res.Basis)
	}
	return &store.BudgetOutcome{Decision: domain.BudgetAllowed, Basis: strings.Join(basis, "; ")}, nil
}

// aboveThreshold: the requisition's amount is over the configured approval
// threshold (0 = no threshold configured).
func (h *Handler) aboveThreshold(pr *domain.PurchaseRequest) bool {
	return h.cfg.ApprovalThreshold > 0 && pr.Amount > h.cfg.ApprovalThreshold
}

// sodViolation is the segregation-of-duties rule for approving/rejecting:
//   - maker-checker (default on): the requester never approves their own;
//   - above the threshold, the approver must also be independent of the last
//     amender and the submitter (an independent approver).
//
// With maker-checker off, the requester may self-approve only at or below the
// threshold.
func (h *Handler) sodViolation(pr *domain.PurchaseRequest, principal string) error {
	above := h.aboveThreshold(pr)
	if principal == pr.RequestedByPrincipalID && (h.cfg.MakerChecker || above) {
		return domain.ErrSelfApprovalNotAllowed
	}
	if above {
		if pr.SubmittedByPrincipalID != nil && *pr.SubmittedByPrincipalID == principal {
			return domain.ErrIndependentApproverRequired
		}
		if pr.LastAmendedByPrincipalID != nil && *pr.LastAmendedByPrincipalID == principal {
			return domain.ErrIndependentApproverRequired
		}
	}
	return nil
}

// expireIfDue moves an expired requisition to EXPIRED and reports true.
func (h *Handler) expireIfDue(ctx context.Context, cc *commandCtx) bool {
	pr := cc.pr
	if pr.ExpiresAt == nil || h.now().Before(*pr.ExpiresAt) {
		return false
	}
	if pr.Status != domain.RequestStatusPending && pr.Status != domain.RequestStatusApproved {
		return false
	}
	v := pr.Version
	_, err := h.store.Apply(ctx, store.Transition{
		TenantID: cc.tenantID, RequestID: pr.RequestID, ExpectedVersion: &v,
		From: []domain.RequestStatus{domain.RequestStatusPending, domain.RequestStatusApproved}, To: domain.RequestStatusExpired,
		Action: "EXPIRED", Meta: store.Meta{Actor: "system:expiry", Reason: "requisition validity window elapsed"},
		Events: []store.EventSpec{{Type: "PurchaseRequisitionExpired"}},
	})
	if err != nil {
		h.log.Warn("lazy expiry failed", zap.String("request_id", pr.RequestID), zap.Error(err))
	}
	return true
}

// POST /{id}/approve — PENDING_APPROVAL -> APPROVED. expected_version required.
func (h *Handler) ApproveRequest(w http.ResponseWriter, r *http.Request) {
	var req domain.VersionedRequest
	if !decodeOptionalJSON(w, r, &req) {
		return
	}
	cc, ok := h.prepare(w, r)
	if !ok {
		return
	}
	pr := cc.pr
	// authorization-svc's own-object SoD layer first (a maker acting as their
	// own checker is denied there too), then the local rule, which also knows
	// the threshold.
	if err := h.authz.CheckAllowedOwnObject(r.Context(), cc.principal, pr.LegalEntityID, actionApproveRequest, pr.RequestedByPrincipalID); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	ev, ok := expectedVersion(w, r, req.ExpectedVersion, true)
	if !ok {
		return
	}
	if h.expireIfDue(r.Context(), cc) {
		writeCoded(w, http.StatusConflict, "requisition_expired", codeExpired, "the requisition's validity window has elapsed")
		return
	}
	m := h.meta(r, cc.principal, "")
	upd, err := h.store.Apply(r.Context(), store.Transition{
		TenantID: cc.tenantID, RequestID: pr.RequestID, ExpectedVersion: ev,
		From: []domain.RequestStatus{domain.RequestStatusPending}, To: domain.RequestStatusApproved,
		Action: "APPROVED", Meta: m,
		Guard: func(cur *domain.PurchaseRequest) error {
			if err := h.sodViolation(cur, cc.principal); err != nil {
				return err
			}
			// A controlled requisition is approvable only on the strength of a
			// recorded ALLOWED budget decision — a bypassed check cannot be
			// approved around.
			for _, l := range cur.Lines {
				if h.controlled(l.Category) && cur.BudgetDecision != domain.BudgetAllowed {
					return domain.ErrBudgetNotAllowed
				}
			}
			return nil
		},
		Events: []store.EventSpec{
			{Type: "PurchaseRequisitionApproved"},
			// Alias: the pre-AP-02 name other consumers read.
			{Type: "purchase.request.approved", Payload: func(r *domain.PurchaseRequest, _ store.Meta) any {
				return map[string]any{"request_id": r.RequestID}
			}},
		},
	})
	if err != nil {
		h.writeDomainErr(w, "ApproveRequest", err)
		return
	}
	writeJSON(w, http.StatusOK, upd)
}

// POST /{id}/reject — PENDING_APPROVAL -> REJECTED. Requires a reason (a
// rejection without a stated reason isn't useful evidence) and expected_version.
func (h *Handler) RejectRequest(w http.ResponseWriter, r *http.Request) {
	var req domain.RejectRequestRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "reason")
		return
	}
	cc, ok := h.prepare(w, r)
	if !ok {
		return
	}
	pr := cc.pr
	if err := h.authz.CheckAllowedOwnObject(r.Context(), cc.principal, pr.LegalEntityID, actionRejectRequest, pr.RequestedByPrincipalID); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	ev, ok := expectedVersion(w, r, req.ExpectedVersion, true)
	if !ok {
		return
	}
	if cc.principal == pr.RequestedByPrincipalID {
		writeCoded(w, http.StatusForbidden, "self_approval_not_allowed", codeSoD, domain.ErrSelfApprovalNotAllowed.Error())
		return
	}
	m := h.meta(r, cc.principal, req.Reason)
	upd, err := h.store.Apply(r.Context(), store.Transition{
		TenantID: cc.tenantID, RequestID: pr.RequestID, ExpectedVersion: ev,
		From: []domain.RequestStatus{domain.RequestStatusPending}, To: domain.RequestStatusRejected,
		Action: "REJECTED", Meta: m,
		Events: []store.EventSpec{
			{Type: "PurchaseRequisitionRejected"},
			{Type: "purchase.request.rejected", Payload: func(r *domain.PurchaseRequest, m store.Meta) any {
				return map[string]any{"request_id": r.RequestID, "reason": r.RejectionReason}
			}},
		},
	})
	if err != nil {
		h.writeDomainErr(w, "RejectRequest", err)
		return
	}
	writeJSON(w, http.StatusOK, upd)
}

// POST /{id}/cancel — DRAFT/PENDING_APPROVAL/APPROVED -> CANCELLED.
func (h *Handler) CancelRequest(w http.ResponseWriter, r *http.Request) {
	var req domain.CancelRequestRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "reason")
		return
	}
	cc, ok := h.prepare(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), cc.principal, cc.pr.LegalEntityID, actionCancelRequest); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	ev, ok := expectedVersion(w, r, req.ExpectedVersion, false)
	if !ok {
		return
	}
	upd, err := h.store.Apply(r.Context(), store.Transition{
		TenantID: cc.tenantID, RequestID: cc.pr.RequestID, ExpectedVersion: ev,
		From: []domain.RequestStatus{domain.RequestStatusDraft, domain.RequestStatusPending, domain.RequestStatusApproved},
		To:   domain.RequestStatusCancelled, Action: "CANCELLED", Meta: h.meta(r, cc.principal, req.Reason),
		Events: []store.EventSpec{{Type: "PurchaseRequisitionCancelled"}},
	})
	if err != nil {
		h.writeDomainErr(w, "CancelRequest", err)
		return
	}
	writeJSON(w, http.StatusOK, upd)
}

// POST /{id}/convert-to-purchase-order — APPROVED -> CONVERTED.
//
// Asks purchase-order-svc to create the PO (a DRAFT, keyed by this requisition,
// so a retry resolves to the same PO), then records its id on the requisition.
// Only an APPROVED requisition converts: cancelled, rejected, expired or
// still-pending ones are refused. Converting an already-converted requisition is
// an idempotent replay of the recorded link.
func (h *Handler) ConvertToPurchaseOrder(w http.ResponseWriter, r *http.Request) {
	var req domain.ConvertRequestRequest
	if !decodeOptionalJSON(w, r, &req) {
		return
	}
	cc, ok := h.prepare(w, r)
	if !ok {
		return
	}
	pr := cc.pr
	if err := h.authz.CheckAllowed(r.Context(), cc.principal, pr.LegalEntityID, actionConvertRequest); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	ev, ok := expectedVersion(w, r, req.ExpectedVersion, false)
	if !ok {
		return
	}
	if pr.Status == domain.RequestStatusConverted && pr.ConvertedPurchaseOrderID != nil {
		w.Header().Set("Idempotent-Replay", "true")
		writeJSON(w, http.StatusOK, pr)
		return
	}
	if ev != nil && pr.Version != *ev {
		writeCoded(w, http.StatusConflict, "stale_version", codeStale, "")
		return
	}
	if h.expireIfDue(r.Context(), cc) {
		writeCoded(w, http.StatusConflict, "requisition_expired", codeExpired, "the requisition's validity window has elapsed")
		return
	}
	if pr.Status != domain.RequestStatusApproved {
		writeCoded(w, http.StatusUnprocessableEntity, "not_convertible", codeInvalidTrans,
			fmt.Sprintf("%s: requisition is %s", domain.ErrNotConvertible.Error(), pr.Status))
		return
	}
	supplier := firstNonEmpty(req.SupplierRef, req.VendorProfileID, pr.PreferredSupplierRef, firstLineSupplier(pr))
	if supplier == "" {
		writeCoded(w, http.StatusBadRequest, "supplier_required", codeValidation, domain.ErrSupplierRequired.Error())
		return
	}

	lines := make([]purchaseorder.Line, 0, len(pr.Lines))
	for _, l := range pr.Lines {
		lines = append(lines, purchaseorder.Line{
			ItemRef: l.ItemRef, Description: l.Description, Quantity: l.Quantity,
			UnitPrice: domain.Round2(l.Amount / l.Quantity), UOM: l.UnitOfMeasure, LineAmount: l.Amount,
		})
	}
	created, err := h.po.CreateDraft(r.Context(), purchaseorder.DraftInput{
		TenantID: cc.tenantID, PrincipalID: cc.principal, LegalEntityID: pr.LegalEntityID,
		PurchaseRequestID: pr.RequestID, SupplierRef: supplier, CurrencyCode: pr.CurrencyCode,
		CorrelationID: "pr-convert-" + pr.RequestID, Lines: lines,
	})
	if err != nil {
		var refused *purchaseorder.RefusedError
		if errors.As(err, &refused) {
			writeCoded(w, http.StatusUnprocessableEntity, "purchase_order_refused", codePORefused, refused.Error())
			return
		}
		h.log.Error("purchase-order-svc unavailable during conversion", zap.Error(err))
		writeCoded(w, http.StatusServiceUnavailable, "purchase_order_service_unavailable", codeDependency, "")
		return
	}

	version := pr.Version
	m := h.meta(r, cc.principal, "")
	m.Details = map[string]any{"purchase_order_id": created.PurchaseOrderID, "po_number": created.PONumber, "supplier_ref": supplier}
	upd, err := h.store.Apply(r.Context(), store.Transition{
		TenantID: cc.tenantID, RequestID: pr.RequestID, ExpectedVersion: &version,
		From: []domain.RequestStatus{domain.RequestStatusApproved}, To: domain.RequestStatusConverted,
		Action: "CONVERTED", Meta: m, PurchaseOrderID: created.PurchaseOrderID,
		Events: []store.EventSpec{{Type: "PurchaseRequisitionConverted"}},
	})
	if err != nil {
		h.writeDomainErr(w, "ConvertToPurchaseOrder", err)
		return
	}
	writeJSON(w, http.StatusOK, upd)
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func firstLineSupplier(pr *domain.PurchaseRequest) string {
	for _, l := range pr.Lines {
		if l.PreferredSupplierRef != "" {
			return l.PreferredSupplierRef
		}
	}
	return ""
}

// ── helpers ──────────────────────────────────────────────────────────────────

func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrAuthorizationDenied):
		writeCoded(w, http.StatusForbidden, "authorization_denied", codeForbidden, "")
	default:
		h.log.Error("authorization check failed — failing closed", zap.Error(err))
		writeCoded(w, http.StatusServiceUnavailable, "authorization_service_unavailable", codeDependency, "")
	}
}

// writeStoreErr maps read-path store failures.
func (h *Handler) writeStoreErr(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, domain.ErrInvalidIdentifier):
		writeCoded(w, http.StatusBadRequest, "invalid_identifier", codeValidation, "")
	case errors.Is(err, domain.ErrTenantScopeMissing):
		writeCoded(w, http.StatusUnauthorized, "tenant_scope_missing", codeTenant, domain.ErrTenantScopeMissing.Error())
	default:
		h.log.Error(op+": store unavailable", zap.Error(err))
		writeCoded(w, http.StatusServiceUnavailable, "store_unavailable", codeStore, "")
	}
}

// writeDomainErr maps command failures onto stable codes.
func (h *Handler) writeDomainErr(w http.ResponseWriter, op string, err error) {
	var ve validationError
	switch {
	case errors.As(err, &ve):
		writeCoded(w, http.StatusBadRequest, "invalid_field", codeValidation, ve.Error())
	case errors.Is(err, domain.ErrRequestNotFound):
		writeCoded(w, http.StatusNotFound, "request_not_found", codeNotFound, "")
	case errors.Is(err, domain.ErrStaleVersion):
		writeCoded(w, http.StatusConflict, "stale_version", codeStale, domain.ErrStaleVersion.Error())
	case errors.Is(err, domain.ErrInvalidTransition):
		writeCoded(w, http.StatusUnprocessableEntity, "invalid_transition", codeInvalidTrans, err.Error())
	case errors.Is(err, domain.ErrSelfApprovalNotAllowed):
		writeCoded(w, http.StatusForbidden, "self_approval_not_allowed", codeSoD, err.Error())
	case errors.Is(err, domain.ErrIndependentApproverRequired):
		writeCoded(w, http.StatusForbidden, "independent_approver_required", codeSoD, err.Error())
	case errors.Is(err, domain.ErrBudgetBlocked):
		writeCoded(w, http.StatusUnprocessableEntity, "budget_blocked", codeBudgetBlocked, err.Error())
	case errors.Is(err, domain.ErrBudgetUnavailable):
		writeCoded(w, http.StatusServiceUnavailable, "budget_check_unavailable", codeDependency, err.Error())
	case errors.Is(err, domain.ErrBudgetNotAllowed):
		writeCoded(w, http.StatusUnprocessableEntity, "budget_check_required", codeBudgetRequired, err.Error())
	default:
		h.writeStoreErr(w, op, err)
	}
}

// requiredFieldMissing deliberately does NOT require tenant_id: the tenant is
// the caller's verified scope, so a body omitting it is fine and a body
// disagreeing with it is a 403, not a missing field.
func requiredFieldMissing(req domain.CreateRequestRequest) string {
	switch {
	case req.LegalEntityID == "":
		return "legal_entity_id"
	case req.Description == "":
		return "description"
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
		writeCoded(w, http.StatusUnauthorized, "tenant_scope_missing", codeTenant, domain.ErrTenantScopeMissing.Error())
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
		writeCoded(w, http.StatusUnauthorized, "identity_missing", codeIdentity, domain.ErrIdentityMissing.Error())
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
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

// stableCode derives the machine-readable code for the older writeError call
// sites from the error slug.
func stableCode(status int, slug string) string {
	switch slug {
	case "self_approval_not_allowed":
		return codeSoD
	case "authorization_denied":
		return codeForbidden
	case "tenant_scope_mismatch":
		return codeForbidden
	case "tenant_scope_missing", "identity_missing":
		return codeIdentity
	case "store_unavailable":
		return codeStore
	}
	switch {
	case status == http.StatusBadRequest, status == http.StatusRequestEntityTooLarge:
		return codeValidation
	case status == http.StatusForbidden:
		return codeForbidden
	case status >= 500:
		return codeDependency
	}
	return codeValidation
}

func writeError(w http.ResponseWriter, status int, slug, detail string) {
	writeCoded(w, status, slug, stableCode(status, slug), detail)
}

// writeCoded writes {"error": slug, "code": STABLE, "detail": ...}. `error` is
// kept for existing clients; clients must branch on `code`.
func writeCoded(w http.ResponseWriter, status int, slug, code, detail string) {
	writeJSON(w, status, errorResponse{Error: slug, Code: code, Detail: detail})
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

// decodeOptionalJSON is decodeJSON for commands whose body is optional (an
// empty body is fine; a malformed one is not).
func decodeOptionalJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	err := json.NewDecoder(r.Body).Decode(dst)
	if err == nil || errors.Is(err, io.EOF) {
		return true
	}
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "")
		return false
	}
	writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
	return false
}
