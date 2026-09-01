// Package handler exposes payable-open-item-svc's REST API — AP-08.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/payable-open-item-svc/internal/authz"
	"zoiko.io/payable-open-item-svc/internal/domain"
	"zoiko.io/payable-open-item-svc/internal/events"
	svcmiddleware "zoiko.io/payable-open-item-svc/internal/middleware"
	"zoiko.io/payable-open-item-svc/internal/store"
)

// Action constants — AP-08's own contract's "Authorization / permissions"
// line ("payable.read/managehold/dispute; payable.adjustment.approve;
// settlement application mostly system-owned"), adapted to this platform's
// SCREAMING_SNAKE_CASE convention. CreatePayableFromApprovedSource reuses
// Read (it's a system-to-system ingestion call from an already-approved
// upstream source, not itself an approval decision); ClosePayable and
// settlement application each get their own dedicated action since neither
// is named precisely enough in the spec's own permission line to safely
// reuse another.
const (
	PayableRead              = "PAYABLE_READ"
	PayableManageHold        = "PAYABLE_MANAGE_HOLD"
	PayableManageDispute     = "PAYABLE_MANAGE_DISPUTE"
	PayableAdjustmentApprove = "PAYABLE_ADJUSTMENT_APPROVE"
	PayableSettlementApply   = "PAYABLE_SETTLEMENT_APPLY"
	PayableClose             = "PAYABLE_CLOSE"
)

type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type Handler struct {
	store store.Store
	pub   events.Publisher
	authz AuthzChecker
	log   *zap.Logger
}

func New(st store.Store, pub events.Publisher, az AuthzChecker, log *zap.Logger) *Handler {
	return &Handler{store: st, pub: pub, authz: az, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/ap08/payables", func(r chi.Router) {
		r.Post("/", h.CreatePayableFromApprovedSource)
		r.Get("/", h.ListOpenPayables)
		r.Get("/{payableID}", h.GetPayable)
		r.Get("/{payableID}/history", h.GetPayableHistory)
		r.Get("/{payableID}/available-actions", h.GetAvailableActions)
		r.Post("/{payableID}/apply-supplier-credit", h.ApplySupplierCredit)
		r.Post("/{payableID}/hold", h.PlacePayableHold)
		r.Post("/{payableID}/release-hold", h.ReleasePayableHold)
		r.Post("/{payableID}/dispute", h.OpenPayableDispute)
		r.Post("/{payableID}/resolve-dispute", h.ResolvePayableDispute)
		r.Post("/{payableID}/apply-confirmed-payment", h.ApplyConfirmedPayment)
		r.Post("/{payableID}/apply-recovery", h.ApplyRecovery)
		r.Post("/{payableID}/close", h.ClosePayable)
	})
	r.Get("/ap08/supplier-balance", h.GetSupplierBalance)
	r.Get("/ap08/payment-eligibility", h.GetPaymentEligibility)
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

func (h *Handler) fetchPayableForAuth(w http.ResponseWriter, r *http.Request, payableID string) (*domain.PayableOpenItem, bool) {
	p, err := h.store.FindPayable(r.Context(), payableID)
	if err != nil {
		if errors.Is(err, domain.ErrPayableNotFound) {
			writeError(w, http.StatusNotFound, "payable not found")
			return nil, false
		}
		h.log.Error("fetchPayableForAuth: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return nil, false
	}
	return p, true
}

// CreatePayableFromApprovedSource is the real consumer expense-claim-svc
// (AP-07) never had before this service existed — see internal/domain's
// package doc. Idempotent on (source_type, source_reference): a repeat
// call for the same already-recorded source returns the existing payable
// rather than creating a duplicate, the direct fix for negative-path #4.
func (h *Handler) CreatePayableFromApprovedSource(w http.ResponseWriter, r *http.Request) {
	var req domain.CreatePayableRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.SourceType == "" || req.SourceReference == "" || req.PayeeRef == "" ||
		req.OriginalAmount <= 0 || req.Currency == "" || req.DueDate.IsZero() {
		writeError(w, http.StatusBadRequest, "legal_entity_id, source_type, source_reference, payee_ref, a positive original_amount, currency and due_date are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, req.LegalEntityID, PayableRead) {
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	created, err := h.store.CreatePayable(r.Context(), verifiedTenant, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrDuplicateSource) {
			existing, findErr := h.store.FindBySource(r.Context(), req.SourceType, req.SourceReference)
			if findErr == nil && existing != nil {
				writeJSON(w, http.StatusOK, existing)
				return
			}
			writeError(w, http.StatusConflict, domain.ErrDuplicateSource.Error())
			return
		}
		h.log.Error("CreatePayableFromApprovedSource: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: domain.EventPayableCreated, EntityID: created.PayableID, TenantID: verifiedTenant,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: created,
	})
	writeJSON(w, http.StatusCreated, created)
}

func (h *Handler) GetPayable(w http.ResponseWriter, r *http.Request) {
	payableID := chi.URLParam(r, "payableID")
	p, ok := h.fetchPayableForAuth(w, r, payableID)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *Handler) ListOpenPayables(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id query parameter is required")
		return
	}
	list, err := h.store.ListOpenPayables(r.Context(), legalEntityID)
	if err != nil {
		h.log.Error("ListOpenPayables: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if list == nil {
		list = []domain.PayableOpenItem{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": list, "count": len(list)})
}

// GetPaymentEligibility is the literal enforcement of negative-path #2
// ("disputed payable included as eligible payment") at the query layer —
// AP-09 (once wired to this service) is meant to call exactly this, not
// ListOpenPayables plus its own filtering, so the eligibility rule lives
// in one place.
func (h *Handler) GetPaymentEligibility(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id query parameter is required")
		return
	}
	all, err := h.store.ListOpenPayables(r.Context(), legalEntityID)
	if err != nil {
		h.log.Error("GetPaymentEligibility: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	eligible := []domain.PayableOpenItem{}
	for _, p := range all {
		if domain.IsEligibleForPayment(p.Status, p.IsHeld, p.IsDisputed, p.ResidualAmount) {
			eligible = append(eligible, p)
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": eligible, "count": len(eligible)})
}

func (h *Handler) GetSupplierBalance(w http.ResponseWriter, r *http.Request) {
	payeeRef := r.URL.Query().Get("payee_ref")
	if payeeRef == "" {
		writeError(w, http.StatusBadRequest, "payee_ref query parameter is required")
		return
	}
	total, err := h.store.GetSupplierBalance(r.Context(), payeeRef)
	if err != nil {
		h.log.Error("GetSupplierBalance: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"payee_ref": payeeRef, "open_balance": total})
}

func (h *Handler) GetPayableHistory(w http.ResponseWriter, r *http.Request) {
	payableID := chi.URLParam(r, "payableID")
	if _, ok := h.fetchPayableForAuth(w, r, payableID); !ok {
		return
	}
	apps, err := h.store.ListApplications(r.Context(), payableID)
	if err != nil {
		h.log.Error("GetPayableHistory: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if apps == nil {
		apps = []domain.SettlementApplication{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": apps, "count": len(apps)})
}

func (h *Handler) GetAvailableActions(w http.ResponseWriter, r *http.Request) {
	payableID := chi.URLParam(r, "payableID")
	p, ok := h.fetchPayableForAuth(w, r, payableID)
	if !ok {
		return
	}
	var actions []string
	if p.ClosedAt == nil {
		if domain.CanApplySettlement(p.Status) {
			actions = append(actions, "ApplySupplierCredit", "ApplyConfirmedPayment", "ApplyRecovery")
		}
		if !p.IsHeld {
			actions = append(actions, "PlacePayableHold")
		} else {
			actions = append(actions, "ReleasePayableHold")
		}
		if !p.IsDisputed {
			actions = append(actions, "OpenPayableDispute")
		} else {
			actions = append(actions, "ResolvePayableDispute")
		}
		if domain.CanClose(p.Status, p.IsHeld, p.IsDisputed) {
			actions = append(actions, "ClosePayable")
		}
	}
	if actions == nil {
		actions = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"payable_id": payableID, "status": p.Status, "available_actions": actions})
}

// ── mutations ────────────────────────────────────────────────────────────────

func (h *Handler) ApplySupplierCredit(w http.ResponseWriter, r *http.Request) {
	payableID := chi.URLParam(r, "payableID")
	var req domain.ApplySupplierCreditRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Amount <= 0 {
		writeError(w, http.StatusBadRequest, "amount must be positive")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	p, ok := h.fetchPayableForAuth(w, r, payableID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, p.LegalEntityID, PayableAdjustmentApprove) {
		return
	}

	updated, err := h.store.ApplySupplierCredit(r.Context(), payableID, req, principalID)
	if h.writePayableMutationErr(w, err, "apply supplier credit") {
		return
	}
	h.publishSettlementEvent(r, updated, principalID)
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) PlacePayableHold(w http.ResponseWriter, r *http.Request) {
	payableID := chi.URLParam(r, "payableID")
	var req domain.PlaceHoldRequest
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
	p, ok := h.fetchPayableForAuth(w, r, payableID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, p.LegalEntityID, PayableManageHold) {
		return
	}

	updated, err := h.store.PlaceHold(r.Context(), payableID, req, principalID)
	if h.writePayableMutationErr(w, err, "place hold") {
		return
	}
	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: domain.EventPayableHeld, EntityID: updated.PayableID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: updated,
	})
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) ReleasePayableHold(w http.ResponseWriter, r *http.Request) {
	payableID := chi.URLParam(r, "payableID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	p, ok := h.fetchPayableForAuth(w, r, payableID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, p.LegalEntityID, PayableManageHold) {
		return
	}

	updated, err := h.store.ReleaseHold(r.Context(), payableID, principalID)
	if h.writePayableMutationErr(w, err, "release hold") {
		return
	}
	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: domain.EventPayableHoldReleased, EntityID: updated.PayableID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: updated,
	})
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) OpenPayableDispute(w http.ResponseWriter, r *http.Request) {
	payableID := chi.URLParam(r, "payableID")
	var req domain.OpenDisputeRequest
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
	p, ok := h.fetchPayableForAuth(w, r, payableID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, p.LegalEntityID, PayableManageDispute) {
		return
	}

	updated, err := h.store.OpenDispute(r.Context(), payableID, req, principalID)
	if h.writePayableMutationErr(w, err, "open dispute") {
		return
	}
	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: domain.EventPayableDisputeOpened, EntityID: updated.PayableID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: updated,
	})
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) ResolvePayableDispute(w http.ResponseWriter, r *http.Request) {
	payableID := chi.URLParam(r, "payableID")
	var req domain.ResolveDisputeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Resolution == "" {
		writeError(w, http.StatusBadRequest, "resolution is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	p, ok := h.fetchPayableForAuth(w, r, payableID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, p.LegalEntityID, PayableManageDispute) {
		return
	}

	updated, err := h.store.ResolveDispute(r.Context(), payableID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "payable is not disputed")
			return
		}
		h.log.Error("ResolvePayableDispute: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: domain.EventPayableDisputeResolved, EntityID: updated.PayableID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: updated,
	})
	writeJSON(w, http.StatusOK, updated)
}

// ApplyConfirmedPayment is caller-attested for now — no real caller wired
// yet (would come from AP-11/BNK-07's settlement chain). Idempotent on
// ProviderPaymentRef: a repeat call with the same reference is a genuine
// no-op, the direct fix for negative-path #3 ("confirmed payment applied
// twice").
func (h *Handler) ApplyConfirmedPayment(w http.ResponseWriter, r *http.Request) {
	payableID := chi.URLParam(r, "payableID")
	var req domain.ApplyConfirmedPaymentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Amount <= 0 || req.ProviderPaymentRef == "" {
		writeError(w, http.StatusBadRequest, "a positive amount and provider_payment_ref are required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	p, ok := h.fetchPayableForAuth(w, r, payableID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, p.LegalEntityID, PayableSettlementApply) {
		return
	}

	updated, applied, err := h.store.ApplyConfirmedPayment(r.Context(), payableID, req, principalID)
	if h.writePayableMutationErr(w, err, "apply confirmed payment") {
		return
	}
	if !applied {
		writeJSON(w, http.StatusOK, map[string]interface{}{"payable": updated, "applied": false, "note": domain.ErrSettlementAlreadyApplied.Error()})
		return
	}
	h.publishSettlementEvent(r, updated, principalID)
	writeJSON(w, http.StatusOK, map[string]interface{}{"payable": updated, "applied": true})
}

func (h *Handler) ApplyRecovery(w http.ResponseWriter, r *http.Request) {
	payableID := chi.URLParam(r, "payableID")
	var req domain.ApplyRecoveryRequest
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
	p, ok := h.fetchPayableForAuth(w, r, payableID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, p.LegalEntityID, PayableAdjustmentApprove) {
		return
	}

	updated, err := h.store.ApplyRecovery(r.Context(), payableID, req, principalID)
	if h.writePayableMutationErr(w, err, "apply recovery") {
		return
	}
	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: domain.EventPayableRecoveryApplied, EntityID: updated.PayableID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: updated,
	})
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) ClosePayable(w http.ResponseWriter, r *http.Request) {
	payableID := chi.URLParam(r, "payableID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	p, ok := h.fetchPayableForAuth(w, r, payableID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, p.LegalEntityID, PayableClose) {
		return
	}

	updated, err := h.store.ClosePayable(r.Context(), payableID, principalID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrPayableHeldOrDisputed):
			writeError(w, http.StatusConflict, domain.ErrPayableHeldOrDisputed.Error())
		case errors.Is(err, domain.ErrPayableNotFullySettled):
			writeError(w, http.StatusConflict, domain.ErrPayableNotFullySettled.Error())
		case errors.Is(err, domain.ErrPayableNotFound):
			writeError(w, http.StatusNotFound, "payable not found")
		default:
			h.log.Error("ClosePayable: store unavailable", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store unavailable")
		}
		return
	}
	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: domain.EventPayableClosed, EntityID: updated.PayableID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: updated,
	})
	writeJSON(w, http.StatusOK, updated)
}

// writePayableMutationErr writes the appropriate HTTP response for the
// errors shared by every residual-changing command and returns true if it
// did (caller should return immediately).
func (h *Handler) writePayableMutationErr(w http.ResponseWriter, err error, action string) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, domain.ErrPayableNotFound):
		writeError(w, http.StatusNotFound, "payable not found")
	case errors.Is(err, domain.ErrInvalidTransition):
		writeError(w, http.StatusConflict, "payable is not in a state that accepts "+action)
	case errors.Is(err, domain.ErrResidualWouldGoNegative):
		writeError(w, http.StatusConflict, domain.ErrResidualWouldGoNegative.Error())
	default:
		h.log.Error(action+": store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
	}
	return true
}

func (h *Handler) publishSettlementEvent(r *http.Request, p *domain.PayableOpenItem, principalID string) {
	eventType := domain.EventPayablePartiallySettled
	if p.Status == domain.StatusSettled {
		eventType = domain.EventPayableSettled
	}
	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: eventType, EntityID: p.PayableID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: p,
	})
}
