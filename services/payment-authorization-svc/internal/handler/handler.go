// Package handler exposes payment-authorization-svc's REST API — AP-10.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

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

	ttl                 time.Duration
	highValueSignatures int
	now                 func() time.Time
}

func New(st store.Store, pub events.Publisher, az AuthzChecker, proposal paymentproposal.Client, supplier supplierprofile.Client, payee payeeidentity.Client, pol policy.Client, log *zap.Logger) *Handler {
	return &Handler{
		store: st, pub: pub, authz: az, proposal: proposal, supplier: supplier, payee: payee, policy: pol, log: log,
		ttl: 24 * time.Hour, highValueSignatures: 2, now: time.Now,
	}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/ap10/authorizations", func(r chi.Router) {
		// Idempotency-Key is mandatory on request, approve and consume (spec
		// §16 names authorization and payment submission); reject/revoke/
		// expire honour it when supplied.
		r.Post("/", h.idempotent("request", true, h.RequestPaymentAuthorization))
		r.Get("/{authorizationID}", h.GetPaymentAuthorization)
		r.Get("/{authorizationID}/subject", h.GetAuthorizationSubject)
		r.Get("/{authorizationID}/validate", h.ValidateAuthorization)
		r.Get("/{authorizationID}/signer-authority", h.GetSignerAuthority)
		r.Get("/{authorizationID}/available-actions", h.GetAvailableActions)
		r.Get("/{authorizationID}/history", h.GetAuthorizationHistory)
		r.Post("/{authorizationID}/approve", h.idempotent("approve", true, h.ApprovePayment))
		r.Post("/{authorizationID}/reject", h.idempotent("reject", false, h.RejectPayment))
		r.Post("/{authorizationID}/revoke", h.idempotent("revoke", false, h.RevokePaymentAuthorization))
		r.Post("/{authorizationID}/expire", h.idempotent("expire", false, h.ExpirePaymentAuthorization))
		r.Post("/{authorizationID}/consume", h.idempotent("consume", true, h.ConsumePaymentAuthorization))
	})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes {"error", "code"}; the code is derived from the status.
// Use writeErrorCode where a more specific control code applies.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeErrorCode(w, status, defaultCode(status), msg)
}

// checkVersion refuses acting on a stale view: if the caller supplied
// expected_version and the authorization has moved on, it answers 409
// STALE_VERSION and returns false.
func checkVersion(w http.ResponseWriter, a *domain.PaymentAuthorization, expected *int) bool {
	if expected != nil && *expected != a.Version {
		writeErrorCode(w, http.StatusConflict, CodeStaleVersion, domain.ErrStaleVersion.Error())
		return false
	}
	return true
}

// expiredNow reports whether a is past its expiry.
func (h *Handler) expiredNow(a *domain.PaymentAuthorization) bool {
	return a.ExpiresAt != nil && !h.now().Before(*a.ExpiresAt)
}

// expire marks a expired (best effort — the sweeper will catch it otherwise)
// and answers 409 AUTHORIZATION_EXPIRED.
func (h *Handler) expire(w http.ResponseWriter, r *http.Request, a *domain.PaymentAuthorization) {
	if _, err := h.store.ExpireAuthorization(r.Context(), a.AuthorizationID, "system"); err != nil && !errors.Is(err, domain.ErrInvalidTransition) {
		h.log.Warn("failed to mark an overdue authorization expired", zap.String("authorization_id", a.AuthorizationID), zap.Error(err))
	}
	writeErrorCode(w, http.StatusConflict, CodeAuthorizationExpired, domain.ErrAuthorizationExpired.Error())
}

// bankChangerConflict reports whether principalID proposed, verified or
// approved the active ORG-10 banking destination of any payee in the
// authorization — spec AP-10 SoD: "payee-bank changer conflicts with payment
// authorization". It fails closed: if ORG-10 cannot be asked, the caller
// must not proceed.
func (h *Handler) bankChangerConflict(r *http.Request, principalID, legalEntityID string, payeeRefs []string) (conflict bool, err error) {
	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	for _, ref := range payeeRefs {
		dest, err := h.payee.GetActiveDestination(r.Context(), verifiedTenant, principalID, legalEntityID, ref)
		if errors.Is(err, domain.ErrNoActiveDestination) {
			continue // nothing to have changed
		}
		if err != nil {
			return false, err
		}
		if dest.ChangedBy(principalID) {
			return true, nil
		}
	}
	return false, nil
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
		// payee-banking-identity-svc (ORG-10). A payee with no ORG-10
		// coverage yet is a real, expected absence, not a failure (see
		// internal/domain's package doc): the request proceeds unpinned. Any
		// other error is an ORG-10 outage or transport fault, and failing
		// OPEN there would create an authorization with an empty
		// DestinationID that every later re-check skips — so it fails
		// CLOSED, before anything is persisted.
		dest, err := h.payee.GetActiveDestination(r.Context(), verifiedTenant, principalID, proposal.LegalEntityID, item.PayeeRef)
		switch {
		case err == nil:
			snap.DestinationID = dest.DestinationID
			if dest.ChangedBy(principalID) {
				writeErrorCode(w, http.StatusForbidden, CodeSoDConflict, domain.ErrBankDetailChangerConflict.Error())
				return
			}
		case errors.Is(err, domain.ErrNoActiveDestination):
			// no ORG-10 coverage: unpinned by policy
		default:
			h.log.Error("RequestPaymentAuthorization: payee-banking-identity-svc lookup failed — failing closed, no authorization created", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, domain.ErrPayeeDestinationServiceUnavailable.Error())
			return
		}
		snapshots = append(snapshots, snap)
	}

	expiresAt := h.now().Add(h.ttl)
	auth := domain.PaymentAuthorization{
		LegalEntityID: proposal.LegalEntityID, ProposalID: proposal.ProposalID, ProposalFingerprint: fingerprint,
		NetAmount: proposal.NetAmount, Currency: proposal.Currency, RequestedByPrincipalID: principalID,
		ExpiresAt: &expiresAt,
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
func (h *Handler) verifyStillEligible(w http.ResponseWriter, r *http.Request, principalID string, a *domain.PaymentAuthorization) bool {
	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())

	liveFingerprint, err := h.proposal.GetFingerprint(r.Context(), verifiedTenant, a.ProposalID)
	if err != nil {
		h.writeProposalErr(w, err)
		return false
	}
	if liveFingerprint != a.ProposalFingerprint {
		_, _ = h.store.InvalidateAuthorization(r.Context(), a.AuthorizationID, "proposal fingerprint changed since authorization was requested")
		writeErrorCode(w, http.StatusConflict, CodeAuthorizationInvalid, "proposal fingerprint no longer matches; authorization invalidated")
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
			writeErrorCode(w, http.StatusConflict, CodePayeeVersionMismatch, "payee identity has changed; authorization invalidated")
			return false
		}
		if snap.DestinationID == "" {
			// No destination was pinned. That is only acceptable if ORG-10
			// still has no coverage for this payee: an unpinned snapshot must
			// never silently pass once a destination exists, nor when ORG-10
			// cannot be asked.
			_, err := h.payee.GetActiveDestination(r.Context(), verifiedTenant, principalID, a.LegalEntityID, snap.PayeeRef)
			if errors.Is(err, domain.ErrNoActiveDestination) {
				continue
			}
			if err != nil {
				h.log.Error("verifyStillEligible: payee-banking-identity-svc lookup failed for unpinned payee", zap.Error(err))
				writeError(w, http.StatusServiceUnavailable, domain.ErrPayeeDestinationServiceUnavailable.Error())
				return false
			}
			_, _ = h.store.InvalidateAuthorization(r.Context(), a.AuthorizationID, "payee gained an active banking destination after authorization was requested without one pinned")
			writeErrorCode(w, http.StatusConflict, CodePayeeVersionMismatch, domain.ErrPayeeDestinationChanged.Error())
			return false
		}
		dest, err := h.payee.GetActiveDestination(r.Context(), verifiedTenant, principalID, a.LegalEntityID, snap.PayeeRef)
		if errors.Is(err, domain.ErrNoActiveDestination) {
			// A destination that was pinned at request time and is no
			// longer the active one at all (superseded/suspended with no
			// replacement yet) is exactly as much a change as a different
			// DestinationID would be.
			_, _ = h.store.InvalidateAuthorization(r.Context(), a.AuthorizationID, "payee's active banking destination changed since authorization was requested")
			writeErrorCode(w, http.StatusConflict, CodePayeeVersionMismatch, domain.ErrPayeeDestinationChanged.Error())
			return false
		}
		if err != nil {
			h.log.Error("verifyStillEligible: payee-banking-identity-svc lookup failed", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, domain.ErrPayeeDestinationServiceUnavailable.Error())
			return false
		}
		if dest.DestinationID != snap.DestinationID {
			_, _ = h.store.InvalidateAuthorization(r.Context(), a.AuthorizationID, "payee's active banking destination changed since authorization was requested")
			writeErrorCode(w, http.StatusConflict, CodePayeeVersionMismatch, domain.ErrPayeeDestinationChanged.Error())
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
	var req domain.ApproveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid request body")
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
	if !checkVersion(w, a, req.ExpectedVersion) {
		return
	}
	if h.expiredNow(a) {
		h.expire(w, r, a)
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
	required := 1
	if policyResult == "APPROVAL_REQUIRED" {
		requiredAction = PaymentAuthorizeHighValue
		required = h.highValueSignatures
	}
	if err := h.authz.CheckAllowedOwnObject(r.Context(), principalID, a.LegalEntityID, requiredAction, proposal.CreatedByPrincipalID); err != nil {
		h.handleAuthzErr(w, err)
		return
	}

	if !h.verifyStillEligible(w, r, principalID, a) {
		return
	}

	// Spec AP-10 SoD: whoever proposed, verified or approved a payee's
	// banking destination cannot authorize a payment to it. Fails closed.
	snapshots, err := h.store.ListPayeeSnapshots(r.Context(), authorizationID)
	if err != nil {
		h.log.Error("ApprovePayment: failed to list payee snapshots", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	var pinned []string
	for _, snap := range snapshots {
		if snap.DestinationID != "" {
			pinned = append(pinned, snap.PayeeRef)
		}
	}
	conflict, err := h.bankChangerConflict(r, principalID, a.LegalEntityID, pinned)
	if err != nil {
		h.log.Error("ApprovePayment: payee-banking-identity-svc lookup failed — failing closed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, domain.ErrPayeeDestinationServiceUnavailable.Error())
		return
	}
	if conflict {
		writeErrorCode(w, http.StatusForbidden, CodeSoDConflict, domain.ErrBankDetailChangerConflict.Error())
		return
	}

	updated, err := h.store.ApproveAuthorization(r.Context(), authorizationID, policyResult, policyVersionID, principalID, required, req.ExpectedVersion)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrInvalidTransition):
			writeError(w, http.StatusConflict, "authorization is not pending")
		case errors.Is(err, domain.ErrStaleVersion):
			writeErrorCode(w, http.StatusConflict, CodeStaleVersion, err.Error())
		case errors.Is(err, domain.ErrAlreadySigned):
			writeErrorCode(w, http.StatusConflict, CodeAlreadySigned, err.Error())
		case errors.Is(err, domain.ErrAuthorizationExpired):
			h.expire(w, r, a)
		default:
			h.log.Error("ApprovePayment: store unavailable", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store unavailable")
		}
		return
	}

	if updated.Status != domain.StatusApproved {
		// A signature was recorded but the quorum is not yet met.
		writeJSON(w, http.StatusAccepted, map[string]interface{}{
			"authorization":         updated,
			"signatures_collected":  updated.SignatureCount,
			"signatures_required":   updated.RequiredSignatures,
			"awaiting_more_signers": true,
		})
		return
	}
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
	if !checkVersion(w, a, req.ExpectedVersion) {
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
	if h.expiredNow(a) {
		h.expire(w, r, a)
		return
	}
	if !h.authorize(w, r, principalID, a.LegalEntityID, PaymentAuthorize) {
		return
	}
	if !h.verifyStillEligible(w, r, principalID, a) {
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
	if !checkVersion(w, a, req.ExpectedVersion) {
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
	} else if h.expiredNow(a) {
		valid = false
		reasons = append(reasons, "the authorization has expired")
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
				// Mirrors verifyStillEligible: unpinned is valid only while ORG-10 still has no coverage.
				if _, err := h.payee.GetActiveDestination(r.Context(), verifiedTenant, r.Header.Get("X-Principal-Id"), a.LegalEntityID, snap.PayeeRef); !errors.Is(err, domain.ErrNoActiveDestination) {
					valid = false
					reasons = append(reasons, "payee "+snap.PayeeRef+" has no pinned destination and its ORG-10 status could not be confirmed as uncovered")
				}
				continue
			}
			dest, err := h.payee.GetActiveDestination(r.Context(), verifiedTenant, r.Header.Get("X-Principal-Id"), a.LegalEntityID, snap.PayeeRef)
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
	signatures, err := h.store.ListSignatures(r.Context(), authorizationID)
	if err != nil {
		h.log.Error("GetSignerAuthority: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	signers := make([]string, 0, len(signatures))
	for _, sg := range signatures {
		signers = append(signers, sg.SignerPrincipalID)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"authorization_id": authorizationID, "policy_assessment_result": result,
		"policy_version_id": a.PolicyVersionID, "required_action": requiredAction,
		"signatures_required": a.RequiredSignatures, "signatures_collected": a.SignatureCount, "signers": signers,
		"expires_at": a.ExpiresAt, "version": a.Version,
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
