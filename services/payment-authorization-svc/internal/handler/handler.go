// Package handler exposes payment-authorization-svc's REST API — AP-10.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/payment-authorization-svc/internal/authz"
	"zoiko.io/payment-authorization-svc/internal/domain"
	"zoiko.io/payment-authorization-svc/internal/events"
	svcmiddleware "zoiko.io/payment-authorization-svc/internal/middleware"
	"zoiko.io/payment-authorization-svc/internal/payeeidentity"
	"zoiko.io/payment-authorization-svc/internal/paymentproposal"
	"zoiko.io/payment-authorization-svc/internal/policy"
	"zoiko.io/payment-authorization-svc/internal/store"
	"zoiko.io/payment-authorization-svc/internal/supplierprofile"
)

// Action constants — AP-10's own contract's "Authorization / permissions"
// line ("payment.authorize; payment.authorize.highvalue;
// payment.authorization.revoke/validate"), adapted to this platform's
// SCREAMING_SNAKE_CASE convention. RequestPaymentAuthorization/RejectPayment/
// ConsumePaymentAuthorization reuse the ordinary Authorize action;
// ApprovePayment escalates to HighValue when policy-svc flags
// APPROVAL_REQUIRED; RevokePaymentAuthorization/ExpirePaymentAuthorization
// reuse Revoke.
const (
	PaymentAuthorize           = "PAYMENT_AUTHORIZE"
	PaymentAuthorizeHighValue  = "PAYMENT_AUTHORIZE_HIGHVALUE"
	PaymentAuthorizationRevoke = "PAYMENT_AUTHORIZATION_REVOKE"
)

type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
	CheckAllowedOwnObject(ctx context.Context, principalID, legalEntityID, actionType, resourceOwnerPrincipalID string) error
}

type Handler struct {
	store    store.Store
	pub      events.Publisher
	authz    AuthzChecker
	proposal paymentproposal.Client
	supplier supplierprofile.Client
	payee    payeeidentity.Client
	policy   policy.Client
	log      *zap.Logger
}

func New(st store.Store, pub events.Publisher, az AuthzChecker, proposal paymentproposal.Client, supplier supplierprofile.Client, payee payeeidentity.Client, pol policy.Client, log *zap.Logger) *Handler {
	return &Handler{store: st, pub: pub, authz: az, proposal: proposal, supplier: supplier, payee: payee, policy: pol, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/ap10/authorizations", func(r chi.Router) {
		r.Post("/", h.RequestPaymentAuthorization)
		r.Get("/{authorizationID}", h.GetPaymentAuthorization)
		r.Get("/{authorizationID}/subject", h.GetAuthorizationSubject)
		r.Get("/{authorizationID}/validate", h.ValidateAuthorization)
		r.Get("/{authorizationID}/signer-authority", h.GetSignerAuthority)
		r.Get("/{authorizationID}/available-actions", h.GetAvailableActions)
		r.Get("/{authorizationID}/history", h.GetAuthorizationHistory)
		r.Post("/{authorizationID}/approve", h.ApprovePayment)
		r.Post("/{authorizationID}/reject", h.RejectPayment)
		r.Post("/{authorizationID}/revoke", h.RevokePaymentAuthorization)
		r.Post("/{authorizationID}/expire", h.ExpirePaymentAuthorization)
		r.Post("/{authorizationID}/consume", h.ConsumePaymentAuthorization)
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

func (h *Handler) fetchAuthForAuth(w http.ResponseWriter, r *http.Request, authorizationID string) (*domain.PaymentAuthorization, bool) {
	a, err := h.store.FindAuthorization(r.Context(), authorizationID)
	if err != nil {
		if errors.Is(err, domain.ErrAuthorizationNotFound) {
			writeError(w, http.StatusNotFound, "payment authorization not found")
			return nil, false
		}
		h.log.Error("fetchAuthForAuth: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return nil, false
	}
	return a, true
}

func (h *Handler) writeProposalErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrProposalNotEligible):
		writeError(w, http.StatusBadRequest, "proposal does not exist or does not belong to the caller's tenant")
	default:
		h.log.Error("payment-proposal-svc lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "payment-proposal-svc unavailable")
	}
}

// ── requesting ───────────────────────────────────────────────────────────────

func (h *Handler) RequestPaymentAuthorization(w http.ResponseWriter, r *http.Request) {
	var req domain.RequestAuthorizationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ProposalID == "" {
		writeError(w, http.StatusBadRequest, "proposal_id is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())

	proposal, err := h.proposal.GetProposal(r.Context(), verifiedTenant, req.ProposalID)
	if err != nil {
		h.writeProposalErr(w, err)
		return
	}
	if proposal.Status != "FROZEN" {
		writeError(w, http.StatusConflict, "proposal must be FROZEN to request authorization")
		return
	}
	if !h.authorize(w, r, principalID, proposal.LegalEntityID, PaymentAuthorize) {
		return
	}

	fingerprint, err := h.proposal.GetFingerprint(r.Context(), verifiedTenant, req.ProposalID)
	if err != nil {
		h.writeProposalErr(w, err)
		return
	}

	var snapshots []domain.PayeeSnapshot
	for _, item := range proposal.Items {
		if item.PayeeSnapshotAt == nil {
			continue
		}
		snap := domain.PayeeSnapshot{PayeeRef: item.PayeeRef, PayeeSnapshotAt: *item.PayeeSnapshotAt}
		// payee-banking-identity-svc (ORG-10) — best-effort at request time:
		// a payee with no ORG-10 coverage yet is a real, expected absence,
		// not a failure (see internal/domain's package doc). Only a
		// genuine ORG-10 outage is logged; either way the request proceeds.
		if dest, err := h.payee.GetActiveDestination(r.Context(), verifiedTenant, proposal.LegalEntityID, item.PayeeRef); err == nil {
			snap.DestinationID = dest.DestinationID
		} else if !errors.Is(err, domain.ErrNoActiveDestination) {
			h.log.Warn("RequestPaymentAuthorization: payee-banking-identity-svc lookup failed — proceeding without a pinned destination", zap.Error(err))
		}
		snapshots = append(snapshots, snap)
	}

	auth := domain.PaymentAuthorization{
		LegalEntityID: proposal.LegalEntityID, ProposalID: proposal.ProposalID, ProposalFingerprint: fingerprint,
		NetAmount: proposal.NetAmount, Currency: proposal.Currency, RequestedByPrincipalID: principalID,
	}
	created, err := h.store.RequestAuthorization(r.Context(), verifiedTenant, auth, snapshots)
	if err != nil {
		if errors.Is(err, domain.ErrProposalAlreadyRequested) {
			writeError(w, http.StatusConflict, "proposal already has an active authorization request")
			return
		}
		h.log.Error("RequestPaymentAuthorization: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: domain.EventAuthorizationRequested, EntityID: created.AuthorizationID, TenantID: verifiedTenant,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: created,
	})
	writeJSON(w, http.StatusCreated, created)
}

func (h *Handler) GetPaymentAuthorization(w http.ResponseWriter, r *http.Request) {
	authorizationID := chi.URLParam(r, "authorizationID")
	a, ok := h.fetchAuthForAuth(w, r, authorizationID)
	if !ok {
		return
	}
	snapshots, err := h.store.ListPayeeSnapshots(r.Context(), authorizationID)
	if err != nil {
		h.log.Error("GetPaymentAuthorization: failed to list payee snapshots", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if snapshots == nil {
		snapshots = []domain.PayeeSnapshot{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"authorization": a, "payee_snapshots": snapshots})
}

// ── decisions ────────────────────────────────────────────────────────────────

// verifyStillEligible is the shared, live re-verification run at both
// ApprovePayment and ConsumePaymentAuthorization: negative-path scenario #1
// ("payee bank details changed after approval") applies at both
// checkpoints, not just once. Any mismatch moves the authorization to
// INVALIDATED — a reachable, terminal state, not merely a blocked request —
// matching the state model's own words ("any protected-field mismatch
// invalidates").
func (h *Handler) verifyStillEligible(w http.ResponseWriter, r *http.Request, a *domain.PaymentAuthorization) bool {
	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())

	liveFingerprint, err := h.proposal.GetFingerprint(r.Context(), verifiedTenant, a.ProposalID)
	if err != nil {
		h.writeProposalErr(w, err)
		return false
	}
	if liveFingerprint != a.ProposalFingerprint {
		_, _ = h.store.InvalidateAuthorization(r.Context(), a.AuthorizationID, "proposal fingerprint changed since authorization was requested")
		writeError(w, http.StatusConflict, "proposal fingerprint no longer matches; authorization invalidated")
		return false
	}

	snapshots, err := h.store.ListPayeeSnapshots(r.Context(), a.AuthorizationID)
	if err != nil {
		h.log.Error("verifyStillEligible: failed to list payee snapshots", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return false
	}
	for _, snap := range snapshots {
		profile, err := h.supplier.FindActiveProfile(r.Context(), verifiedTenant, a.LegalEntityID, snap.PayeeRef)
		if err != nil {
			h.log.Error("verifyStillEligible: supplier-financial-profile-svc lookup failed", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "supplier-financial-profile-svc unavailable")
			return false
		}
		if !profile.UpdatedAt.Equal(snap.PayeeSnapshotAt) {
			_, _ = h.store.InvalidateAuthorization(r.Context(), a.AuthorizationID, "payee identity changed since authorization was requested")
			writeError(w, http.StatusConflict, "payee identity has changed; authorization invalidated")
			return false
		}
		if snap.DestinationID == "" {
			continue // no ORG-10 coverage was on file at request time — nothing to re-check
		}
		dest, err := h.payee.GetActiveDestination(r.Context(), verifiedTenant, a.LegalEntityID, snap.PayeeRef)
		if errors.Is(err, domain.ErrNoActiveDestination) {
			// A destination that was pinned at request time and is no
			// longer the active one at all (superseded/suspended with no
			// replacement yet) is exactly as much a change as a different
			// DestinationID would be.
			_, _ = h.store.InvalidateAuthorization(r.Context(), a.AuthorizationID, "payee's active banking destination changed since authorization was requested")
			writeError(w, http.StatusConflict, domain.ErrPayeeDestinationChanged.Error())
			return false
		}
		if err != nil {
			h.log.Error("verifyStillEligible: payee-banking-identity-svc lookup failed", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, domain.ErrPayeeDestinationServiceUnavailable.Error())
			return false
		}
		if dest.DestinationID != snap.DestinationID {
			_, _ = h.store.InvalidateAuthorization(r.Context(), a.AuthorizationID, "payee's active banking destination changed since authorization was requested")
			writeError(w, http.StatusConflict, domain.ErrPayeeDestinationChanged.Error())
			return false
		}
	}
	return true
}

// ApprovePayment is AP-10's central checkpoint: negative-path #3 (the
// proposal's own preparer cannot approve — the fifth reuse of
// authorization-svc's dynamic own-object SoD layer this session),
// negative-path #2 (a signer without PAYMENT_AUTHORIZE_HIGHVALUE cannot
// approve a payment policy-svc's real APPROVAL_THRESHOLD evaluation flags
// as requiring escalated approval), and negative-path #1 (re-verified live
// via verifyStillEligible, not just trusted from request time).
func (h *Handler) ApprovePayment(w http.ResponseWriter, r *http.Request) {
	authorizationID := chi.URLParam(r, "authorizationID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	a, ok := h.fetchAuthForAuth(w, r, authorizationID)
	if !ok {
		return
	}
	if !domain.CanDecide(a.Status) {
		writeError(w, http.StatusConflict, "authorization is not pending")
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	proposal, err := h.proposal.GetProposal(r.Context(), verifiedTenant, a.ProposalID)
	if err != nil {
		h.writeProposalErr(w, err)
		return
	}

	policyResult, policyVersionID, err := h.policy.EvaluateApprovalThreshold(r.Context(), principalID, verifiedTenant, a.LegalEntityID, a.NetAmount)
	if err != nil {
		h.log.Error("ApprovePayment: policy evaluation failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "policy-svc unavailable")
		return
	}
	requiredAction := PaymentAuthorize
	if policyResult == "APPROVAL_REQUIRED" {
		requiredAction = PaymentAuthorizeHighValue
	}
	if err := h.authz.CheckAllowedOwnObject(r.Context(), principalID, a.LegalEntityID, requiredAction, proposal.CreatedByPrincipalID); err != nil {
		h.handleAuthzErr(w, err)
		return
	}

	if !h.verifyStillEligible(w, r, a) {
		return
	}

	updated, err := h.store.ApproveAuthorization(r.Context(), authorizationID, policyResult, policyVersionID, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "authorization is not pending")
			return
		}
		h.log.Error("ApprovePayment: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: domain.EventPaymentAuthorized, EntityID: updated.AuthorizationID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: updated,
	})
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) RejectPayment(w http.ResponseWriter, r *http.Request) {
	authorizationID := chi.URLParam(r, "authorizationID")
	var req domain.RejectPaymentRequest
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
	a, ok := h.fetchAuthForAuth(w, r, authorizationID)
	if !ok {
		return
	}
	if !domain.CanDecide(a.Status) {
		writeError(w, http.StatusConflict, "authorization is not pending")
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	proposal, err := h.proposal.GetProposal(r.Context(), verifiedTenant, a.ProposalID)
	if err != nil {
		h.writeProposalErr(w, err)
		return
	}
	if err := h.authz.CheckAllowedOwnObject(r.Context(), principalID, a.LegalEntityID, PaymentAuthorize, proposal.CreatedByPrincipalID); err != nil {
		h.handleAuthzErr(w, err)
		return
	}

	updated, err := h.store.RejectAuthorization(r.Context(), authorizationID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "authorization is not pending")
			return
		}
		h.log.Error("RejectPayment: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// ConsumePaymentAuthorization has no real caller yet — AP-11 ("Payment
// Run"), the service that would actually execute a payment, does not exist
// in this codebase. Implemented fully and honestly regardless (see
// internal/domain's package doc): negative-path #4's replay protection
// (the store's own WHERE status = 'APPROVED' guard plus the migration's
// terminal-status trigger) and negative-path #1's re-verification both
// apply here exactly as they do at ApprovePayment.
func (h *Handler) ConsumePaymentAuthorization(w http.ResponseWriter, r *http.Request) {
	authorizationID := chi.URLParam(r, "authorizationID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	a, ok := h.fetchAuthForAuth(w, r, authorizationID)
	if !ok {
		return
	}
	if !domain.CanConsume(a.Status) {
		writeError(w, http.StatusConflict, "authorization is not approved")
		return
	}
	if !h.authorize(w, r, principalID, a.LegalEntityID, PaymentAuthorize) {
		return
	}
	if !h.verifyStillEligible(w, r, a) {
		return
	}

	updated, err := h.store.ConsumeAuthorization(r.Context(), authorizationID, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "authorization is not approved")
			return
		}
		h.log.Error("ConsumePaymentAuthorization: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: domain.EventAuthorizationConsumed, EntityID: updated.AuthorizationID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: updated,
	})
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) RevokePaymentAuthorization(w http.ResponseWriter, r *http.Request) {
	authorizationID := chi.URLParam(r, "authorizationID")
	var req domain.RevokeAuthorizationRequest
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
	a, ok := h.fetchAuthForAuth(w, r, authorizationID)
	if !ok {
		return
	}
	if !domain.CanRevoke(a.Status) {
		writeError(w, http.StatusConflict, "authorization is not in a revocable state")
		return
	}
	if !h.authorize(w, r, principalID, a.LegalEntityID, PaymentAuthorizationRevoke) {
		return
	}

	updated, err := h.store.RevokeAuthorization(r.Context(), authorizationID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "authorization is not in a revocable state")
			return
		}
		h.log.Error("RevokePaymentAuthorization: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// ExpirePaymentAuthorization is a real, callable command with no automatic
// trigger — there is no background scheduler anywhere in this codebase.
// See internal/domain's package doc.
func (h *Handler) ExpirePaymentAuthorization(w http.ResponseWriter, r *http.Request) {
	authorizationID := chi.URLParam(r, "authorizationID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	a, ok := h.fetchAuthForAuth(w, r, authorizationID)
	if !ok {
		return
	}
	if !domain.CanExpire(a.Status) {
		writeError(w, http.StatusConflict, "authorization is not in an expirable state")
		return
	}
	if !h.authorize(w, r, principalID, a.LegalEntityID, PaymentAuthorizationRevoke) {
		return
	}

	updated, err := h.store.ExpireAuthorization(r.Context(), authorizationID, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "authorization is not in an expirable state")
			return
		}
		h.log.Error("ExpirePaymentAuthorization: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// ── queries ──────────────────────────────────────────────────────────────────

func (h *Handler) GetAuthorizationSubject(w http.ResponseWriter, r *http.Request) {
	authorizationID := chi.URLParam(r, "authorizationID")
	a, ok := h.fetchAuthForAuth(w, r, authorizationID)
	if !ok {
		return
	}
	snapshots, err := h.store.ListPayeeSnapshots(r.Context(), authorizationID)
	if err != nil {
		h.log.Error("GetAuthorizationSubject: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if snapshots == nil {
		snapshots = []domain.PayeeSnapshot{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"proposal_id": a.ProposalID, "proposal_fingerprint": a.ProposalFingerprint,
		"net_amount": a.NetAmount, "currency": a.Currency, "payee_snapshots": snapshots,
	})
}

// ValidateAuthorization is a read-only version of verifyStillEligible's
// checks — it reports validity without ever invalidating anything itself.
func (h *Handler) ValidateAuthorization(w http.ResponseWriter, r *http.Request) {
	authorizationID := chi.URLParam(r, "authorizationID")
	a, ok := h.fetchAuthForAuth(w, r, authorizationID)
	if !ok {
		return
	}

	valid := a.Status == domain.StatusApproved
	var reasons []string
	if !valid {
		reasons = append(reasons, "status is "+string(a.Status)+", not APPROVED")
	} else {
		verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
		liveFingerprint, err := h.proposal.GetFingerprint(r.Context(), verifiedTenant, a.ProposalID)
		if err != nil {
			h.writeProposalErr(w, err)
			return
		}
		if liveFingerprint != a.ProposalFingerprint {
			valid = false
			reasons = append(reasons, "proposal fingerprint has changed")
		}
		snapshots, err := h.store.ListPayeeSnapshots(r.Context(), authorizationID)
		if err != nil {
			h.log.Error("ValidateAuthorization: store unavailable", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store unavailable")
			return
		}
		for _, snap := range snapshots {
			profile, err := h.supplier.FindActiveProfile(r.Context(), verifiedTenant, a.LegalEntityID, snap.PayeeRef)
			if err != nil {
				valid = false
				reasons = append(reasons, "payee "+snap.PayeeRef+" could not be re-verified")
				continue
			}
			if !profile.UpdatedAt.Equal(snap.PayeeSnapshotAt) {
				valid = false
				reasons = append(reasons, "payee "+snap.PayeeRef+" identity has changed")
			}
			if snap.DestinationID == "" {
				continue
			}
			dest, err := h.payee.GetActiveDestination(r.Context(), verifiedTenant, a.LegalEntityID, snap.PayeeRef)
			if err != nil || dest.DestinationID != snap.DestinationID {
				valid = false
				reasons = append(reasons, "payee "+snap.PayeeRef+"'s active banking destination has changed")
			}
		}
	}
	if reasons == nil {
		reasons = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"authorization_id": authorizationID, "valid": valid, "reasons": reasons})
}

func (h *Handler) GetSignerAuthority(w http.ResponseWriter, r *http.Request) {
	authorizationID := chi.URLParam(r, "authorizationID")
	a, ok := h.fetchAuthForAuth(w, r, authorizationID)
	if !ok {
		return
	}
	result := a.PolicyAssessmentResult
	if result == "" {
		result = "NOT_YET_ASSESSED"
	}
	requiredAction := PaymentAuthorize
	if result == "APPROVAL_REQUIRED" {
		requiredAction = PaymentAuthorizeHighValue
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"authorization_id": authorizationID, "policy_assessment_result": result,
		"policy_version_id": a.PolicyVersionID, "required_action": requiredAction,
	})
}

func (h *Handler) GetAvailableActions(w http.ResponseWriter, r *http.Request) {
	authorizationID := chi.URLParam(r, "authorizationID")
	a, ok := h.fetchAuthForAuth(w, r, authorizationID)
	if !ok {
		return
	}
	var actions []string
	if domain.CanDecide(a.Status) {
		actions = append(actions, "ApprovePayment", "RejectPayment")
	}
	if domain.CanConsume(a.Status) {
		actions = append(actions, "ConsumePaymentAuthorization")
	}
	if domain.CanRevoke(a.Status) {
		actions = append(actions, "RevokePaymentAuthorization")
	}
	if domain.CanExpire(a.Status) {
		actions = append(actions, "ExpirePaymentAuthorization")
	}
	if actions == nil {
		actions = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"authorization_id": authorizationID, "status": a.Status, "available_actions": actions})
}

func (h *Handler) GetAuthorizationHistory(w http.ResponseWriter, r *http.Request) {
	authorizationID := chi.URLParam(r, "authorizationID")
	if _, ok := h.fetchAuthForAuth(w, r, authorizationID); !ok {
		return
	}
	events, err := h.store.ListEvents(r.Context(), authorizationID)
	if err != nil {
		h.log.Error("GetAuthorizationHistory: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if events == nil {
		events = []domain.AuthorizationEvent{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": events, "count": len(events)})
}
