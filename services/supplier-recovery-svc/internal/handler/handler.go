// Package handler exposes supplier-recovery-svc's REST API — AP-12.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/supplier-recovery-svc/internal/authz"
	"zoiko.io/supplier-recovery-svc/internal/bankreconciliation"
	"zoiko.io/supplier-recovery-svc/internal/domain"
	"zoiko.io/supplier-recovery-svc/internal/events"
	svcmiddleware "zoiko.io/supplier-recovery-svc/internal/middleware"
	"zoiko.io/supplier-recovery-svc/internal/payableopenitem"
	"zoiko.io/supplier-recovery-svc/internal/store"
)

// Action constants — AP-12's own contract's "Authorization / permissions"
// line ("supplierrecovery.read/manage; recovery.offset.approve;
// recovery.writeoff.approve"), adapted to this platform's
// SCREAMING_SNAKE_CASE convention. CreateSupplierRecoveryCase,
// RecordSupplierCommitment, EscalateRecovery, and LinkConfirmedSupplierRefund
// reuse Manage; ApplyApprovedOffset and WriteOffRecovery each require their
// own dedicated, stronger action, matching the spec's own permission line
// exactly — neither is safely reducible to ordinary Manage.
const (
	RecoveryRead            = "RECOVERY_READ"
	RecoveryManage          = "RECOVERY_MANAGE"
	RecoveryOffsetApprove   = "RECOVERY_OFFSET_APPROVE"
	RecoveryWriteoffApprove = "RECOVERY_WRITEOFF_APPROVE"
)

type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
	CheckAllowedOwnObject(ctx context.Context, principalID, legalEntityID, actionType, resourceOwnerPrincipalID string) error
}

type Handler struct {
	store    store.Store
	pub      events.Publisher
	authz    AuthzChecker
	payables payableopenitem.Client
	bankrec  bankreconciliation.Client
	log      *zap.Logger
}

func New(st store.Store, pub events.Publisher, az AuthzChecker, payables payableopenitem.Client, bankrec bankreconciliation.Client, log *zap.Logger) *Handler {
	return &Handler{store: st, pub: pub, authz: az, payables: payables, bankrec: bankrec, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/ap12/cases", func(r chi.Router) {
		r.Post("/", h.CreateSupplierRecoveryCase)
		r.Get("/", h.ListOpenRecoveries)
		r.Get("/{caseID}", h.GetRecoveryCase)
		r.Get("/{caseID}/exposure", h.GetRecoveryExposure)
		r.Get("/{caseID}/available-actions", h.GetAvailableActions)
		r.Get("/{caseID}/history", h.GetRecoveryHistory)
		r.Post("/{caseID}/approve", h.ApproveRecoveryPlan)
		r.Post("/{caseID}/commitments", h.RecordSupplierCommitment)
		r.Post("/{caseID}/apply-offset", h.ApplyApprovedOffset)
		r.Post("/{caseID}/link-refund", h.LinkConfirmedSupplierRefund)
		r.Post("/{caseID}/escalate", h.EscalateRecovery)
		r.Post("/{caseID}/close", h.CloseRecoveryCase)
		r.Post("/{caseID}/write-off", h.WriteOffRecovery)
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
		if errors.Is(err, authzpkg.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "not authorized to perform this action")
			return false
		}
		h.log.Error("authorization check failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
		return false
	}
	return true
}

// authorizeOwnObject is the maker-checker gate this session reuses for the
// spec's own explicit self-approval prohibitions — case owner cannot
// approve their own offset or write-off (negative-path #3 for write-off
// specifically, and the spec's own SoD line for offset).
func (h *Handler) authorizeOwnObject(w http.ResponseWriter, r *http.Request, principalID, legalEntityID, actionType, resourceOwnerPrincipalID string) bool {
	if err := h.authz.CheckAllowedOwnObject(r.Context(), principalID, legalEntityID, actionType, resourceOwnerPrincipalID); err != nil {
		if errors.Is(err, authzpkg.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "not authorized to perform this action")
			return false
		}
		h.log.Error("authorization check failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
		return false
	}
	return true
}

func (h *Handler) fetchCaseForAuth(w http.ResponseWriter, r *http.Request, caseID string) (*domain.SupplierRecoveryCase, bool) {
	c, err := h.store.FindCase(r.Context(), caseID)
	if err != nil {
		if errors.Is(err, domain.ErrCaseNotFound) {
			writeError(w, http.StatusNotFound, "supplier recovery case not found")
			return nil, false
		}
		h.log.Error("fetchCaseForAuth: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return nil, false
	}
	return c, true
}

// CreateSupplierRecoveryCase confirms the named source payable genuinely
// exists in AP-08 before creating a case against it — a real integration,
// not an opaque reference, since AP-08's real GetPayable is available.
func (h *Handler) CreateSupplierRecoveryCase(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateCaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.SupplierRef == "" || req.RecoveryBasis == "" || req.SourcePayableID == "" ||
		req.TotalAmount <= 0 || req.Currency == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id, supplier_ref, recovery_basis, source_payable_id, a positive total_amount and currency are required")
		return
	}
	switch req.RecoveryBasis {
	case domain.BasisOverpayment, domain.BasisDuplicatePayment, domain.BasisSupplierCredit, domain.BasisContractual:
	default:
		writeError(w, http.StatusBadRequest, "recovery_basis must be OVERPAYMENT, DUPLICATE_PAYMENT, SUPPLIER_CREDIT, or CONTRACTUAL")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, req.LegalEntityID, RecoveryManage) {
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	payable, err := h.payables.GetPayable(r.Context(), verifiedTenant, req.SourcePayableID)
	if err != nil {
		if errors.Is(err, domain.ErrCaseNotFound) {
			writeError(w, http.StatusBadRequest, "source_payable_id does not exist in payable-open-item-svc")
			return
		}
		h.log.Error("CreateSupplierRecoveryCase: payable-open-item-svc lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, domain.ErrPayableServiceUnavailable.Error())
		return
	}
	if payable.LegalEntityID != req.LegalEntityID {
		writeError(w, http.StatusBadRequest, "source_payable_id does not belong to this legal entity")
		return
	}

	created, err := h.store.CreateCase(r.Context(), verifiedTenant, req, principalID)
	if err != nil {
		h.log.Error("CreateSupplierRecoveryCase: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: domain.EventRecoveryCaseCreated, EntityID: created.CaseID, TenantID: verifiedTenant,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: created,
	})
	writeJSON(w, http.StatusCreated, created)
}

func (h *Handler) GetRecoveryCase(w http.ResponseWriter, r *http.Request) {
	caseID := chi.URLParam(r, "caseID")
	c, ok := h.fetchCaseForAuth(w, r, caseID)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (h *Handler) ListOpenRecoveries(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id query parameter is required")
		return
	}
	list, err := h.store.ListOpenCases(r.Context(), legalEntityID)
	if err != nil {
		h.log.Error("ListOpenRecoveries: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if list == nil {
		list = []domain.SupplierRecoveryCase{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": list, "count": len(list)})
}

func (h *Handler) GetRecoveryExposure(w http.ResponseWriter, r *http.Request) {
	caseID := chi.URLParam(r, "caseID")
	c, ok := h.fetchCaseForAuth(w, r, caseID)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"case_id": caseID, "total_amount": c.TotalAmount, "recovered_amount": c.RecoveredAmount,
		"outstanding_amount": c.TotalAmount - c.RecoveredAmount, "currency": c.Currency, "status": c.Status,
	})
}

func (h *Handler) GetAvailableActions(w http.ResponseWriter, r *http.Request) {
	caseID := chi.URLParam(r, "caseID")
	c, ok := h.fetchCaseForAuth(w, r, caseID)
	if !ok {
		return
	}
	var actions []string
	if domain.CanApprove(c.Status) {
		actions = append(actions, "ApproveRecoveryPlan")
	}
	if domain.CanApplyRecovery(c.Status) {
		actions = append(actions, "RecordSupplierCommitment", "ApplyApprovedOffset", "LinkConfirmedSupplierRefund")
	}
	if domain.CanEscalate(c.Status) {
		actions = append(actions, "EscalateRecovery")
	}
	if domain.CanWriteOff(c.Status) {
		actions = append(actions, "WriteOffRecovery")
	}
	if domain.CanClose(c.Status) {
		actions = append(actions, "CloseRecoveryCase")
	}
	if actions == nil {
		actions = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"case_id": caseID, "status": c.Status, "available_actions": actions})
}

func (h *Handler) GetRecoveryHistory(w http.ResponseWriter, r *http.Request) {
	caseID := chi.URLParam(r, "caseID")
	if _, ok := h.fetchCaseForAuth(w, r, caseID); !ok {
		return
	}
	apps, err := h.store.ListApplications(r.Context(), caseID)
	if err != nil {
		h.log.Error("GetRecoveryHistory: failed to list applications", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	commitments, err := h.store.ListCommitments(r.Context(), caseID)
	if err != nil {
		h.log.Error("GetRecoveryHistory: failed to list commitments", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if apps == nil {
		apps = []domain.RecoveryApplication{}
	}
	if commitments == nil {
		commitments = []domain.RecoveryCommitment{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"case_id": caseID, "applications": apps, "commitments": commitments})
}

func (h *Handler) ApproveRecoveryPlan(w http.ResponseWriter, r *http.Request) {
	caseID := chi.URLParam(r, "caseID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	c, ok := h.fetchCaseForAuth(w, r, caseID)
	if !ok {
		return
	}
	if !h.authorizeOwnObject(w, r, principalID, c.LegalEntityID, RecoveryManage, c.CreatedByPrincipalID) {
		return
	}

	updated, err := h.store.ApproveRecoveryPlan(r.Context(), caseID, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "case is not OPEN")
			return
		}
		h.log.Error("ApproveRecoveryPlan: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: domain.EventRecoveryPlanApproved, EntityID: updated.CaseID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: updated,
	})
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) RecordSupplierCommitment(w http.ResponseWriter, r *http.Request) {
	caseID := chi.URLParam(r, "caseID")
	var req domain.RecordCommitmentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Detail == "" {
		writeError(w, http.StatusBadRequest, "detail is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	c, ok := h.fetchCaseForAuth(w, r, caseID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, c.LegalEntityID, RecoveryManage) {
		return
	}

	commitment, err := h.store.RecordCommitment(r.Context(), caseID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrCaseNotFound) {
			writeError(w, http.StatusNotFound, "supplier recovery case not found")
			return
		}
		h.log.Error("RecordSupplierCommitment: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: domain.EventCommitmentRecorded, EntityID: caseID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: commitment,
	})
	writeJSON(w, http.StatusCreated, commitment)
}

// ApplyApprovedOffset is the first real caller of AP-08's ApplyRecovery
// anywhere in this codebase. It checks this service's own idempotency
// ledger BEFORE calling AP-08 — necessary because AP-08's own uniqueness
// constraint only covers PAYMENT-type applications, not RECOVERY, so this
// service must not rely on AP-08 to reject a replay by itself. Honest
// residual gap: a network failure after AP-08 applies the offset but
// before this service records the application locally could still cause a
// duplicate reduction at AP-08 on a caller's retry — narrow, and
// documented here rather than silently assumed safe.
func (h *Handler) ApplyApprovedOffset(w http.ResponseWriter, r *http.Request) {
	caseID := chi.URLParam(r, "caseID")
	var req domain.ApplyOffsetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Amount <= 0 || req.RecoveryRef == "" {
		writeError(w, http.StatusBadRequest, "a positive amount and recovery_ref are required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	c, ok := h.fetchCaseForAuth(w, r, caseID)
	if !ok {
		return
	}
	if !h.authorizeOwnObject(w, r, principalID, c.LegalEntityID, RecoveryOffsetApprove, c.CreatedByPrincipalID) {
		return
	}
	if !domain.CanApplyRecovery(c.Status) {
		writeError(w, http.StatusConflict, "case is not in a state that accepts a recovery application")
		return
	}

	existing, err := h.alreadyApplied(r.Context(), caseID, "OFFSET", req.RecoveryRef)
	if err != nil {
		h.log.Error("ApplyApprovedOffset: failed to check existing applications", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if existing {
		writeJSON(w, http.StatusOK, map[string]interface{}{"case": c, "applied": false, "note": domain.ErrApplicationAlreadyApplied.Error()})
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	if _, err := h.payables.ApplyRecovery(r.Context(), verifiedTenant, principalID, c.SourcePayableID, payableopenitem.ApplyRecoveryRequest{
		Amount: req.Amount, RecoveryRef: req.RecoveryRef, Reason: "supplier recovery case " + caseID,
	}); err != nil {
		h.log.Error("ApplyApprovedOffset: AP-08 rejected the offset — case unchanged", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, domain.ErrOffsetFailedAtPayable.Error())
		return
	}

	updated, applied, err := h.store.ApplyRecovery(r.Context(), caseID, "OFFSET", req.Amount, req.RecoveryRef, "", principalID)
	if h.writeApplyErr(w, err) {
		return
	}
	if applied {
		_ = h.pub.Publish(r.Context(), events.PublishParams{
			EventType: domain.EventRecoveryOffsetApplied, EntityID: updated.CaseID, ActorID: principalID,
			CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: updated,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"case": updated, "applied": applied})
}

// LinkConfirmedSupplierRefund is the literal enforcement of negative-path
// #1 — the named bank statement line must already be MATCHED before this
// call ever touches the case's recovered amount.
func (h *Handler) LinkConfirmedSupplierRefund(w http.ResponseWriter, r *http.Request) {
	caseID := chi.URLParam(r, "caseID")
	var req domain.LinkRefundRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.StatementLineID == "" {
		writeError(w, http.StatusBadRequest, "statement_line_id is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	c, ok := h.fetchCaseForAuth(w, r, caseID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, c.LegalEntityID, RecoveryManage) {
		return
	}
	if !domain.CanApplyRecovery(c.Status) {
		writeError(w, http.StatusConflict, "case is not in a state that accepts a recovery application")
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	line, err := h.bankrec.GetConfirmedInboundLine(r.Context(), verifiedTenant, c.LegalEntityID, req.StatementLineID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrStatementLineNotFound):
			writeError(w, http.StatusBadRequest, domain.ErrStatementLineNotFound.Error())
		case errors.Is(err, domain.ErrStatementLineNotConfirmed):
			writeError(w, http.StatusConflict, domain.ErrStatementLineNotConfirmed.Error())
		case errors.Is(err, domain.ErrStatementLineMismatch):
			writeError(w, http.StatusBadRequest, domain.ErrStatementLineMismatch.Error())
		default:
			h.log.Error("LinkConfirmedSupplierRefund: bank-reconciliation-svc lookup failed", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, domain.ErrBankReconciliationUnavailable.Error())
		}
		return
	}

	updated, applied, err := h.store.ApplyRecovery(r.Context(), caseID, "REFUND", line.Amount, line.StatementLineID, line.BankReference, principalID)
	if h.writeApplyErr(w, err) {
		return
	}
	if applied {
		_ = h.pub.Publish(r.Context(), events.PublishParams{
			EventType: domain.EventSupplierRefundConfirmed, EntityID: updated.CaseID, ActorID: principalID,
			CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: updated,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"case": updated, "applied": applied})
}

func (h *Handler) EscalateRecovery(w http.ResponseWriter, r *http.Request) {
	caseID := chi.URLParam(r, "caseID")
	var req domain.EscalateRequest
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
	c, ok := h.fetchCaseForAuth(w, r, caseID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, c.LegalEntityID, RecoveryManage) {
		return
	}

	updated, err := h.store.EscalateCase(r.Context(), caseID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "case is not in an escalatable state")
			return
		}
		h.log.Error("EscalateRecovery: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: domain.EventRecoveryEscalated, EntityID: updated.CaseID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: updated,
	})
	writeJSON(w, http.StatusOK, updated)
}

// WriteOffRecovery is the literal enforcement of negative-path #3
// ("recovery write-off self-approved").
func (h *Handler) WriteOffRecovery(w http.ResponseWriter, r *http.Request) {
	caseID := chi.URLParam(r, "caseID")
	var req domain.WriteOffRequest
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
	c, ok := h.fetchCaseForAuth(w, r, caseID)
	if !ok {
		return
	}
	if !h.authorizeOwnObject(w, r, principalID, c.LegalEntityID, RecoveryWriteoffApprove, c.CreatedByPrincipalID) {
		return
	}

	updated, err := h.store.WriteOffCase(r.Context(), caseID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "case is not in a state that accepts write-off")
			return
		}
		h.log.Error("WriteOffRecovery: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: domain.EventRecoveryWrittenOff, EntityID: updated.CaseID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: updated,
	})
	writeJSON(w, http.StatusOK, updated)
}

// CloseRecoveryCase is the literal enforcement of negative-path #4 — the
// store only permits this from RECOVERED, never while any difference
// remains.
func (h *Handler) CloseRecoveryCase(w http.ResponseWriter, r *http.Request) {
	caseID := chi.URLParam(r, "caseID")
	var req domain.CloseCaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	c, ok := h.fetchCaseForAuth(w, r, caseID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, c.LegalEntityID, RecoveryManage) {
		return
	}

	updated, err := h.store.CloseCase(r.Context(), caseID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "case must be fully RECOVERED to close")
			return
		}
		h.log.Error("CloseRecoveryCase: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: domain.EventRecoveryClosed, EntityID: updated.CaseID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: updated,
	})
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) alreadyApplied(ctx context.Context, caseID, appType, idempotencyRef string) (bool, error) {
	apps, err := h.store.ListApplications(ctx, caseID)
	if err != nil {
		return false, err
	}
	for _, a := range apps {
		if a.ApplicationType == appType && a.IdempotencyRef == idempotencyRef {
			return true, nil
		}
	}
	return false, nil
}

func (h *Handler) writeApplyErr(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, domain.ErrCaseNotFound):
		writeError(w, http.StatusNotFound, "supplier recovery case not found")
	case errors.Is(err, domain.ErrInvalidTransition):
		writeError(w, http.StatusConflict, "case is not in a state that accepts a recovery application")
	case errors.Is(err, domain.ErrRecoveryExceedsOutstanding):
		writeError(w, http.StatusConflict, domain.ErrRecoveryExceedsOutstanding.Error())
	default:
		h.log.Error("apply recovery: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
	}
	return true
}
