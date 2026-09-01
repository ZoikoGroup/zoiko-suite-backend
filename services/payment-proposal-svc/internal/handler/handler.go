// Package handler exposes payment-proposal-svc's REST API — AP-09.
package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/payment-proposal-svc/internal/accountspayable"
	authzpkg "zoiko.io/payment-proposal-svc/internal/authz"
	"zoiko.io/payment-proposal-svc/internal/domain"
	"zoiko.io/payment-proposal-svc/internal/events"
	svcmiddleware "zoiko.io/payment-proposal-svc/internal/middleware"
	"zoiko.io/payment-proposal-svc/internal/payableopenitem"
	"zoiko.io/payment-proposal-svc/internal/store"
	"zoiko.io/payment-proposal-svc/internal/supplierprofile"
	"zoiko.io/payment-proposal-svc/internal/tax"
)

// Action constants — AP-09's own contract's "Authorization / permissions"
// line ("paymentproposal.read/create/manage/freeze;
// paymentproposal.exception.resolve"), adapted to this platform's
// SCREAMING_SNAKE_CASE convention (see
// master-register-findings-2026-08-27.md §2.5). Create/AddEligiblePayable/
// RemovePayable/Recalculate/Submit/Cancel all reuse Manage; Freeze and any
// AddEligiblePayable that overrides an ON_HOLD supplier are separate,
// stronger actions.
const (
	ProposalRead             = "PAYMENT_PROPOSAL_READ"
	ProposalManage           = "PAYMENT_PROPOSAL_MANAGE"
	ProposalFreeze           = "PAYMENT_PROPOSAL_FREEZE"
	ProposalExceptionResolve = "PAYMENT_PROPOSAL_EXCEPTION_RESOLVE"
)

type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
	CheckAllowedOwnObject(ctx context.Context, principalID, legalEntityID, actionType, resourceOwnerPrincipalID string) error
}

type Handler struct {
	store    store.Store
	pub      events.Publisher
	authz    AuthzChecker
	ap       accountspayable.Client
	payables payableopenitem.Client
	supplier supplierprofile.Client
	tax      tax.Client
	log      *zap.Logger
}

func New(st store.Store, pub events.Publisher, az AuthzChecker, ap accountspayable.Client, payables payableopenitem.Client, supplier supplierprofile.Client, taxClient tax.Client, log *zap.Logger) *Handler {
	return &Handler{store: st, pub: pub, authz: az, ap: ap, payables: payables, supplier: supplier, tax: taxClient, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/ap09/proposals", func(r chi.Router) {
		r.Post("/", h.CreateProposal)
		r.Get("/{proposalID}", h.GetProposal)
		r.Post("/{proposalID}/items", h.AddEligiblePayable)
		r.Get("/{proposalID}/items", h.ListProposalItems)
		r.Post("/{proposalID}/items/{itemID}/remove", h.RemovePayable)
		r.Post("/{proposalID}/recalculate", h.RecalculatePaymentProposal)
		r.Post("/{proposalID}/submit-for-review", h.SubmitProposalForReview)
		r.Post("/{proposalID}/freeze", h.FreezePaymentProposal)
		r.Post("/{proposalID}/cancel", h.CancelPaymentProposal)
		r.Get("/{proposalID}/selection-rationale", h.GetSelectionRationale)
		r.Get("/{proposalID}/exception-flags", h.GetExceptionFlags)
		r.Get("/{proposalID}/fingerprint", h.GetFingerprint)
		r.Get("/{proposalID}/available-actions", h.GetAvailableActions)
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

func (h *Handler) fetchProposalForAuth(w http.ResponseWriter, r *http.Request, proposalID string) (*domain.PaymentProposal, bool) {
	p, err := h.store.FindProposal(r.Context(), proposalID)
	if err != nil {
		if errors.Is(err, domain.ErrProposalNotFound) {
			writeError(w, http.StatusNotFound, "payment proposal not found")
			return nil, false
		}
		h.log.Error("fetchProposalForAuth: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return nil, false
	}
	return p, true
}

// ── proposals ────────────────────────────────────────────────────────────────

func (h *Handler) CreateProposal(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateProposalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.PayingBankAccountRef == "" || req.Currency == "" || req.PaymentMethod == "" || req.PaymentDate.IsZero() {
		writeError(w, http.StatusBadRequest, "legal_entity_id, paying_bank_account_ref, currency, payment_method and payment_date are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, req.LegalEntityID, ProposalManage) {
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	p, err := h.store.CreateProposal(r.Context(), verifiedTenant, req, principalID)
	if err != nil {
		h.log.Error("CreateProposal: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: domain.EventProposalCreated, EntityID: p.ProposalID, TenantID: verifiedTenant,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: p,
	})
	writeJSON(w, http.StatusCreated, p)
}

func (h *Handler) GetProposal(w http.ResponseWriter, r *http.Request) {
	proposalID := chi.URLParam(r, "proposalID")
	p, ok := h.fetchProposalForAuth(w, r, proposalID)
	if !ok {
		return
	}
	items, err := h.store.ListItems(r.Context(), proposalID)
	if err != nil {
		h.log.Error("GetProposal: failed to list items", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if items == nil {
		items = []domain.ProposalItem{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"proposal": p, "items": items})
}

func (h *Handler) ListProposalItems(w http.ResponseWriter, r *http.Request) {
	proposalID := chi.URLParam(r, "proposalID")
	if _, ok := h.fetchProposalForAuth(w, r, proposalID); !ok {
		return
	}
	items, err := h.store.ListItems(r.Context(), proposalID)
	if err != nil {
		h.log.Error("ListProposalItems: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if items == nil {
		items = []domain.ProposalItem{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": items, "count": len(items)})
}

// AddEligiblePayable is where negative-path scenarios #1 and #2 are
// enforced. #1 (a held payable force-added without exception): an
// AP_INVOICE item whose supplier-financial-profile-svc profile is ON_HOLD,
// or an EXPENSE_CLAIM item whose AP-08 payable is IsHeld/IsDisputed, is
// rejected unless the caller supplies a non-empty ExceptionRef, which
// itself requires the stronger PAYMENT_PROPOSAL_EXCEPTION_RESOLVE
// authority, not ordinary PAYMENT_PROPOSAL_MANAGE. #2 (duplicate/in-flight
// payable) is enforced by the store's own database constraint.
func (h *Handler) AddEligiblePayable(w http.ResponseWriter, r *http.Request) {
	proposalID := chi.URLParam(r, "proposalID")
	var req domain.AddEligiblePayableRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !domain.ValidPayableSource(req.PayableSource) || req.PayableID == "" {
		writeError(w, http.StatusBadRequest, "payable_source must be AP_INVOICE or EXPENSE_CLAIM, and payable_id is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	proposal, ok := h.fetchProposalForAuth(w, r, proposalID)
	if !ok {
		return
	}
	if !domain.CanMutateItems(proposal.Status) {
		writeError(w, http.StatusConflict, "proposal is not in a state that accepts new items")
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	item := domain.ProposalItem{ProposalID: proposalID, PayableSource: req.PayableSource, PayableID: req.PayableID}
	isHeldOverride := false

	switch req.PayableSource {
	case domain.SourceAPInvoice:
		inv, err := h.ap.GetEligibleInvoice(r.Context(), verifiedTenant, proposal.LegalEntityID, req.PayableID)
		if err != nil {
			h.writePayableErr(w, err)
			return
		}
		profile, err := h.supplier.FindActiveProfile(r.Context(), verifiedTenant, proposal.LegalEntityID, inv.VendorID)
		if err != nil {
			h.writePayeeErr(w, err)
			return
		}
		if profile.Status == "ON_HOLD" {
			if req.ExceptionRef == "" {
				writeError(w, http.StatusConflict, "payee is on hold; an exception reference is required to add this payable")
				return
			}
			isHeldOverride = true
		}

		var withholding float64
		var taxDetID string
		if req.ApplyWithholding {
			if profile.TaxWithholdingRef == "" {
				writeError(w, http.StatusBadRequest, "payee has no tax_withholding_ref on file; apply_withholding cannot be used")
				return
			}
			if req.JurisdictionID == "" || req.TaxCategory == "" {
				writeError(w, http.StatusBadRequest, "jurisdiction_id and tax_category are required when apply_withholding is true")
				return
			}
			result, err := h.tax.Determine(r.Context(), principalID, tax.DetermineRequest{
				TransactionID: req.PayableID, LegalEntityID: proposal.LegalEntityID, JurisdictionID: req.JurisdictionID,
				TaxCategory: req.TaxCategory, GrossAmount: inv.Amount, Currency: inv.CurrencyCode,
				EffectiveFrom: time.Now().UTC().Format("2006-01-02"),
			})
			if err != nil {
				h.log.Warn("AddEligiblePayable: withholding determination failed — blocking", zap.Error(err))
				writeError(w, http.StatusUnprocessableEntity, "tax determination failed for withholding; payable not added")
				return
			}
			withholding = result.CalculatedTaxAmount
			taxDetID = result.DeterminationID
		}

		item.PayeeRef = inv.VendorID
		item.GrossAmount = inv.Amount
		item.WithholdingAmount = withholding
		item.NetAmount = inv.Amount - withholding
		item.Currency = inv.CurrencyCode
		item.DueDate = inv.DueDate
		snapshot := profile.UpdatedAt
		item.PayeeSnapshotAt = &snapshot
		item.TaxDeterminationID = taxDetID
		item.ExceptionRef = req.ExceptionRef

	case domain.SourceExpenseClaim:
		payable, err := h.payables.GetEligiblePayable(r.Context(), verifiedTenant, proposal.LegalEntityID, req.PayableID)
		if err != nil {
			h.writePayableErr(w, err)
			return
		}
		if payable.IsHeld || payable.IsDisputed {
			if req.ExceptionRef == "" {
				writeError(w, http.StatusConflict, "payee is on hold; an exception reference is required to add this payable")
				return
			}
			isHeldOverride = true
		}
		item.PayeeRef = payable.PayeeRef
		item.GrossAmount = payable.ResidualAmount
		item.NetAmount = payable.ResidualAmount
		item.Currency = payable.Currency
		item.ExceptionRef = req.ExceptionRef
		// AP-08 has no due-date concept for a reimbursement either — the
		// claim became payable the moment expense-claim-svc created it there.
		item.DueDate = time.Now().UTC()
	}

	requiredAction := ProposalManage
	if isHeldOverride {
		requiredAction = ProposalExceptionResolve
	}
	if !h.authorize(w, r, principalID, proposal.LegalEntityID, requiredAction) {
		return
	}

	added, err := h.store.AddItem(r.Context(), item)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrPayableAlreadyInProposal):
			writeError(w, http.StatusConflict, "payable is already an active item on another proposal")
		case errors.Is(err, domain.ErrInvalidTransition):
			writeError(w, http.StatusConflict, "proposal is not in a state that accepts new items")
		case errors.Is(err, domain.ErrProposalNotFound):
			writeError(w, http.StatusNotFound, "payment proposal not found")
		default:
			h.log.Error("AddEligiblePayable: store unavailable", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store unavailable")
		}
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: domain.EventProposalChanged, EntityID: proposalID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: added,
	})
	writeJSON(w, http.StatusCreated, added)
}

func (h *Handler) writePayableErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrPayableNotEligible):
		writeError(w, http.StatusBadRequest, "payable does not exist, does not belong to this legal entity, or is not in an eligible status")
	default:
		h.log.Error("upstream payable lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "upstream payable service unavailable")
	}
}

func (h *Handler) writePayeeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrPayeeNotFound):
		writeError(w, http.StatusBadRequest, "no supplier financial profile found for this invoice's vendor")
	default:
		h.log.Error("supplier-financial-profile-svc lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "supplier-financial-profile-svc unavailable")
	}
}

func (h *Handler) RemovePayable(w http.ResponseWriter, r *http.Request) {
	proposalID := chi.URLParam(r, "proposalID")
	itemID := chi.URLParam(r, "itemID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	proposal, ok := h.fetchProposalForAuth(w, r, proposalID)
	if !ok {
		return
	}
	if !domain.CanMutateItems(proposal.Status) {
		writeError(w, http.StatusConflict, "proposal is not in a state that allows removing items")
		return
	}
	if !h.authorize(w, r, principalID, proposal.LegalEntityID, ProposalManage) {
		return
	}

	item, err := h.store.FindItem(r.Context(), itemID)
	if err != nil || item.ProposalID != proposalID {
		writeError(w, http.StatusNotFound, "proposal item not found")
		return
	}

	if err := h.store.RemoveItem(r.Context(), itemID); err != nil {
		if errors.Is(err, domain.ErrItemNotFound) {
			writeError(w, http.StatusNotFound, "proposal item not found")
			return
		}
		h.log.Error("RemovePayable: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: domain.EventProposalChanged, EntityID: proposalID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: map[string]string{"removed_item_id": itemID},
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

func (h *Handler) RecalculatePaymentProposal(w http.ResponseWriter, r *http.Request) {
	proposalID := chi.URLParam(r, "proposalID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	proposal, ok := h.fetchProposalForAuth(w, r, proposalID)
	if !ok {
		return
	}
	if !domain.CanRecalculate(proposal.Status) {
		writeError(w, http.StatusConflict, "proposal is not in a recalculable state")
		return
	}
	if !h.authorize(w, r, principalID, proposal.LegalEntityID, ProposalManage) {
		return
	}

	items, err := h.store.ListItems(r.Context(), proposalID)
	if err != nil {
		h.log.Error("RecalculatePaymentProposal: failed to list items", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if len(items) == 0 {
		writeError(w, http.StatusBadRequest, "proposal has no items to calculate")
		return
	}
	var gross, withholding float64
	for _, it := range items {
		gross += it.GrossAmount
		withholding += it.WithholdingAmount
	}
	net := gross - withholding

	updated, err := h.store.RecalculateProposal(r.Context(), proposalID, gross, withholding, net, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "proposal is not in a recalculable state")
			return
		}
		h.log.Error("RecalculatePaymentProposal: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: domain.EventProposalCalculated, EntityID: updated.ProposalID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: updated,
	})
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) SubmitProposalForReview(w http.ResponseWriter, r *http.Request) {
	proposalID := chi.URLParam(r, "proposalID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	proposal, ok := h.fetchProposalForAuth(w, r, proposalID)
	if !ok {
		return
	}
	if !domain.CanSubmitForReview(proposal.Status) {
		writeError(w, http.StatusConflict, "proposal must be CALCULATED to submit for review")
		return
	}
	if !h.authorize(w, r, principalID, proposal.LegalEntityID, ProposalManage) {
		return
	}

	updated, err := h.store.SubmitForReview(r.Context(), proposalID, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "proposal must be CALCULATED to submit for review")
			return
		}
		h.log.Error("SubmitProposalForReview: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// FreezePaymentProposal is AP-09's last checkpoint before a (not-yet-built)
// AP-10 would authorize it. Three things are enforced for real here: the
// maker-checker SoD line (the proposal's own preparer cannot be the one who
// freezes it — the fourth reuse of authorization-svc's dynamic own-object
// layer this session); negative-path #4 (a live re-check that each
// AP_INVOICE item's supplier profile has not changed since it was added —
// updated_at is the only staleness signal supplier-financial-profile-svc
// has); and negative-path #1's defense-in-depth half (re-checking ON_HOLD
// in case a supplier went on hold after the item was added without an
// exception).
func (h *Handler) FreezePaymentProposal(w http.ResponseWriter, r *http.Request) {
	proposalID := chi.URLParam(r, "proposalID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	proposal, ok := h.fetchProposalForAuth(w, r, proposalID)
	if !ok {
		return
	}
	if !domain.CanFreeze(proposal.Status) {
		writeError(w, http.StatusConflict, "proposal must be in REVIEW to freeze")
		return
	}
	if err := h.authz.CheckAllowedOwnObject(r.Context(), principalID, proposal.LegalEntityID, ProposalFreeze, proposal.CreatedByPrincipalID); err != nil {
		h.handleAuthzErr(w, err)
		return
	}

	items, err := h.store.ListItems(r.Context(), proposalID)
	if err != nil {
		h.log.Error("FreezePaymentProposal: failed to list items", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if len(items) == 0 {
		writeError(w, http.StatusBadRequest, "proposal has no items to freeze")
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	for _, it := range items {
		if it.PayableSource != domain.SourceAPInvoice || it.PayeeSnapshotAt == nil {
			continue
		}
		profile, err := h.supplier.FindActiveProfile(r.Context(), verifiedTenant, proposal.LegalEntityID, it.PayeeRef)
		if err != nil {
			h.writePayeeErr(w, err)
			return
		}
		if !profile.UpdatedAt.Equal(*it.PayeeSnapshotAt) {
			writeError(w, http.StatusConflict, "payee identity has changed since a payable was added; recalculate before freezing")
			return
		}
		if profile.Status == "ON_HOLD" && it.ExceptionRef == "" {
			writeError(w, http.StatusConflict, "a payee is now on hold with no exception recorded; remove or override the item before freezing")
			return
		}
	}

	updated, err := h.store.FreezeProposal(r.Context(), proposalID, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "proposal must be in REVIEW to freeze")
			return
		}
		h.log.Error("FreezePaymentProposal: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: domain.EventProposalFrozen, EntityID: updated.ProposalID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: updated,
	})
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) CancelPaymentProposal(w http.ResponseWriter, r *http.Request) {
	proposalID := chi.URLParam(r, "proposalID")
	var req domain.CancelProposalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	proposal, ok := h.fetchProposalForAuth(w, r, proposalID)
	if !ok {
		return
	}
	if !domain.CanCancel(proposal.Status) {
		writeError(w, http.StatusConflict, "proposal is not in a cancellable state")
		return
	}
	if !h.authorize(w, r, principalID, proposal.LegalEntityID, ProposalManage) {
		return
	}

	updated, err := h.store.CancelProposal(r.Context(), proposalID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "proposal is not in a cancellable state")
			return
		}
		h.log.Error("CancelPaymentProposal: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: domain.EventProposalCancelled, EntityID: updated.ProposalID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: updated,
	})
	writeJSON(w, http.StatusOK, updated)
}

// ── queries ──────────────────────────────────────────────────────────────────

func (h *Handler) GetSelectionRationale(w http.ResponseWriter, r *http.Request) {
	proposalID := chi.URLParam(r, "proposalID")
	p, ok := h.fetchProposalForAuth(w, r, proposalID)
	if !ok {
		return
	}
	items, err := h.store.ListItems(r.Context(), proposalID)
	if err != nil {
		h.log.Error("GetSelectionRationale: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if items == nil {
		items = []domain.ProposalItem{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"proposal_id": proposalID, "status": p.Status, "items": items,
		"gross_amount": p.GrossAmount, "withholding_amount": p.WithholdingAmount, "net_amount": p.NetAmount,
	})
}

func (h *Handler) GetExceptionFlags(w http.ResponseWriter, r *http.Request) {
	proposalID := chi.URLParam(r, "proposalID")
	if _, ok := h.fetchProposalForAuth(w, r, proposalID); !ok {
		return
	}
	items, err := h.store.ListItems(r.Context(), proposalID)
	if err != nil {
		h.log.Error("GetExceptionFlags: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	var flagged []domain.ProposalItem
	for _, it := range items {
		if it.ExceptionRef != "" {
			flagged = append(flagged, it)
		}
	}
	if flagged == nil {
		flagged = []domain.ProposalItem{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": flagged, "count": len(flagged)})
}

// GetFingerprint computes a real SHA-256 digest over the proposal's own
// current composition (item IDs and amounts) — the "subject fingerprint"
// AP-09's own contract names it owns. This is the first fingerprint of this
// exact kind in this codebase (there is prior art for content-addressed
// hashing — tax-determination-svc's rule snapshot — but nothing named
// "subject fingerprint" to follow verbatim, so this is a new, real
// computation, not a fabricated placeholder).
func (h *Handler) GetFingerprint(w http.ResponseWriter, r *http.Request) {
	proposalID := chi.URLParam(r, "proposalID")
	p, ok := h.fetchProposalForAuth(w, r, proposalID)
	if !ok {
		return
	}
	items, err := h.store.ListItems(r.Context(), proposalID)
	if err != nil {
		h.log.Error("GetFingerprint: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ItemID < items[j].ItemID })
	h256 := sha256.New()
	fmt.Fprintf(h256, "%s|%s|%.2f|%.2f|%.2f", p.ProposalID, p.Status, p.GrossAmount, p.WithholdingAmount, p.NetAmount)
	for _, it := range items {
		fmt.Fprintf(h256, "|%s:%s:%.2f:%.2f", it.PayableSource, it.PayableID, it.GrossAmount, it.NetAmount)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"proposal_id": proposalID, "status": p.Status, "fingerprint": "sha256:" + hex.EncodeToString(h256.Sum(nil)),
	})
}

func (h *Handler) GetAvailableActions(w http.ResponseWriter, r *http.Request) {
	proposalID := chi.URLParam(r, "proposalID")
	p, ok := h.fetchProposalForAuth(w, r, proposalID)
	if !ok {
		return
	}
	var actions []string
	if domain.CanMutateItems(p.Status) {
		actions = append(actions, "AddEligiblePayable", "RemovePayable")
	}
	if domain.CanRecalculate(p.Status) {
		actions = append(actions, "RecalculatePaymentProposal")
	}
	if domain.CanSubmitForReview(p.Status) {
		actions = append(actions, "SubmitProposalForReview")
	}
	if domain.CanFreeze(p.Status) {
		actions = append(actions, "FreezePaymentProposal")
	}
	if domain.CanCancel(p.Status) {
		actions = append(actions, "CancelPaymentProposal")
	}
	if actions == nil {
		actions = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"proposal_id": proposalID, "status": p.Status, "available_actions": actions})
}
