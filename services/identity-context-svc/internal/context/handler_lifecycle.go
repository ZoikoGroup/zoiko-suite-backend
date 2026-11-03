package context

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/lifecycle"
	"zoiko.io/identity-context-svc/internal/store"
)

// ── POST /v1/principals ──────────────────────────────────────────────────────

// CreateSubject creates a PENDING_ACTIVATION subject (a person) or an ACTIVE
// workload, and sends the activation link for a person.
//
// Authorized at IDENTITY_PRINCIPAL_CREATE. Mutations carry Idempotency-Key,
// honoured by the idempotency middleware; a replay returns the first response.
func (h *Handler) CreateSubject(w http.ResponseWriter, r *http.Request) {
	if h.lifecycle == nil {
		writeCoded(w, r, http.StatusNotImplemented, domain.ErrCodeUpstreamUnavailable,
			"subject creation is not configured in this deployment")
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	callerPrincipalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, callerPrincipalID, tenantID, IdentityPrincipalCreate) {
		return
	}

	var req lifecycle.CreateSubjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}

	p, err := h.lifecycle.CreateSubject(r.Context(), lifecycle.Actor{
		PrincipalID:   callerPrincipalID,
		TenantID:      tenantID,
		CorrelationID: r.Header.Get("X-Correlation-ID"),
	}, req, "")
	if err != nil {
		h.lifecycleError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

// ── POST /v1/principals/{principalID}/invite ─────────────────────────────────

// InviteSubject (re)sends an activation link to a PENDING_ACTIVATION person.
//
// Authorized at IDENTITY_PRINCIPAL_CREATE.
func (h *Handler) InviteSubject(w http.ResponseWriter, r *http.Request) {
	if h.lifecycle == nil {
		writeCoded(w, r, http.StatusNotImplemented, domain.ErrCodeUpstreamUnavailable,
			"subject invitations are not configured in this deployment")
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	callerPrincipalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, callerPrincipalID, tenantID, IdentityPrincipalCreate) {
		return
	}

	principalID := chi.URLParam(r, "principalID")
	if err := h.lifecycle.Invite(r.Context(), lifecycle.Actor{
		PrincipalID:   callerPrincipalID,
		TenantID:      tenantID,
		CorrelationID: r.Header.Get("X-Correlation-ID"),
	}, principalID); err != nil {
		h.lifecycleError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── POST /v1/account/activate ────────────────────────────────────────────────

type activateRequest struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

// ActivateAccount redeems an activation link: the person sets their password
// and the subject becomes ACTIVE. Pre-session — no verified caller.
func (h *Handler) ActivateAccount(w http.ResponseWriter, r *http.Request) {
	if h.lifecycle == nil {
		writeCoded(w, r, http.StatusNotImplemented, domain.ErrCodeUpstreamUnavailable,
			"account activation is not configured in this deployment")
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// Pre-session routes are throttled on source IP within the tenant.
	if _, ok := h.throttle(w, r, "activate", tenantID); !ok {
		return
	}

	var req activateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}

	p, err := h.lifecycle.Activate(r.Context(), req.Token, req.Password, r.Header.Get("X-Correlation-ID"))
	if err != nil {
		h.lifecycleError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// ── POST /v1/account/password ────────────────────────────────────────────────

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NextPassword    string `json:"next_password"`
}

// ChangePassword is the self-service password change: the current password
// proves possession. The URL carries no principal — the caller changes their
// own password, so no administrator grant is needed.
func (h *Handler) ChangePassword(w http.ResponseWriter, r *http.Request) {
	if h.lifecycle == nil {
		writeCoded(w, r, http.StatusNotImplemented, domain.ErrCodeUpstreamUnavailable,
			"self-service password change is not configured in this deployment")
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	callerPrincipalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.throttle(w, r, "change_password", tenantID); !ok {
		return
	}

	var req changePasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := h.lifecycle.ChangePassword(r.Context(), lifecycle.Actor{
		PrincipalID:   callerPrincipalID,
		TenantID:      tenantID,
		CorrelationID: r.Header.Get("X-Correlation-ID"),
	}, req.CurrentPassword, req.NextPassword); err != nil {
		h.lifecycleError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── POST /v1/account/password/forgot ─────────────────────────────────────────

type forgotPasswordRequest struct {
	Email string `json:"email"`
}

// ForgotPassword sends a reset link to an active person's own address. It
// answers the same whether or not the account exists (no enumeration), and is
// throttled per source IP.
func (h *Handler) ForgotPassword(w http.ResponseWriter, r *http.Request) {
	if h.lifecycle == nil {
		writeCoded(w, r, http.StatusNotImplemented, domain.ErrCodeUpstreamUnavailable,
			"forgotten password is not configured in this deployment")
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if _, ok := h.throttle(w, r, "forgot_password", tenantID); !ok {
		return
	}

	var req forgotPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}

	clientIP := clientIP(r)
	h.lifecycle.ForgotPassword(r.Context(), tenantID, req.Email, clientIP, r.Header.Get("X-Correlation-ID"))
	// Always 202: never confirm or deny that an account exists.
	writeCoded(w, r, http.StatusAccepted, "", "reset requested")
}

// ── POST /v1/account/password/reset ──────────────────────────────────────────

type completeResetRequest struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

// CompleteReset redeems a reset link and ends every session the person held.
// Pre-session — no verified caller.
func (h *Handler) CompleteReset(w http.ResponseWriter, r *http.Request) {
	if h.lifecycle == nil {
		writeCoded(w, r, http.StatusNotImplemented, domain.ErrCodeUpstreamUnavailable,
			"password reset completion is not configured in this deployment")
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if _, ok := h.throttle(w, r, "complete_reset", tenantID); !ok {
		return
	}

	var req completeResetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}

	p, err := h.lifecycle.CompleteReset(r.Context(), req.Token, req.Password, r.Header.Get("X-Correlation-ID"))
	if err != nil {
		h.lifecycleError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// ── POST /v1/principals/{principalID}/password-resets ────────────────────────

type requestResetRequest struct {
	Reason string `json:"reason"`
}

// RequestReset opens an administrator-initiated reset. It issues nothing until
// independently approved. Authorized at IDENTITY_PASSWORD_RESET_REQUEST.
func (h *Handler) RequestReset(w http.ResponseWriter, r *http.Request) {
	if h.lifecycle == nil {
		writeCoded(w, r, http.StatusNotImplemented, domain.ErrCodeUpstreamUnavailable,
			"administrator-initiated reset is not configured in this deployment")
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	callerPrincipalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, callerPrincipalID, tenantID, IdentityPasswordResetRequest) {
		return
	}

	principalID := chi.URLParam(r, "principalID")
	var req requestResetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}

	rr, err := h.lifecycle.RequestReset(r.Context(), lifecycle.Actor{
		PrincipalID:   callerPrincipalID,
		TenantID:      tenantID,
		CorrelationID: r.Header.Get("X-Correlation-ID"),
	}, principalID, req.Reason)
	if err != nil {
		h.lifecycleError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, rr)
}

// ── POST /v1/password-resets/{resetID}/decide ────────────────────────────────

type decideResetRequest struct {
	Decision string `json:"decision"`
}

// DecideReset records an approver's decision (APPROVE or REJECT). The
// approver's second factor is read from their verified envelope
// (steppedUpActor); the lifecycle service refuses a stale or missing one.
func (h *Handler) DecideReset(w http.ResponseWriter, r *http.Request) {
	if h.lifecycle == nil {
		writeCoded(w, r, http.StatusNotImplemented, domain.ErrCodeUpstreamUnavailable,
			"reset decisions are not configured in this deployment")
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	callerPrincipalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, callerPrincipalID, tenantID, IdentityPasswordResetApprove) {
		return
	}

	resetID := chi.URLParam(r, "resetID")
	var req decideResetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}

	actor, ok := h.steppedUpActor(w, r, tenantID, callerPrincipalID)
	if !ok {
		return
	}
	rr, err := h.lifecycle.DecideReset(r.Context(), actor, resetID, req.Decision)
	if err != nil {
		h.lifecycleError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rr)
}

// ── GET /v1/password-resets/{resetID} ────────────────────────────────────────

// GetReset reads an administrator-initiated reset request. A read of an
// existing aggregate, authorized at IDENTITY_PRINCIPAL_READ.
func (h *Handler) GetReset(w http.ResponseWriter, r *http.Request) {
	if h.lifecycle == nil {
		writeCoded(w, r, http.StatusNotImplemented, domain.ErrCodeUpstreamUnavailable,
			"reset requests are not configured in this deployment")
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	callerPrincipalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, callerPrincipalID, tenantID, IdentityPrincipalRead) {
		return
	}

	resetID := chi.URLParam(r, "resetID")
	rr, err := h.lifecycle.GetReset(r.Context(), lifecycle.Actor{
		PrincipalID:   callerPrincipalID,
		TenantID:      tenantID,
		CorrelationID: r.Header.Get("X-Correlation-ID"),
	}, resetID)
	if err != nil {
		h.lifecycleError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rr)
}

// ── helpers ──────────────────────────────────────────────────────────────────

func (h *Handler) lifecycleError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, lifecycle.ErrInvalid):
		writeError(w, r, http.StatusBadRequest, err.Error())
	case errors.Is(err, lifecycle.ErrNotFound):
		writeError(w, r, http.StatusNotFound, err.Error())
	case errors.Is(err, lifecycle.ErrConflict):
		writeCoded(w, r, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, lifecycle.ErrForbidden):
		writeCoded(w, r, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, lifecycle.ErrStepUp):
		writeCoded(w, r, http.StatusUnauthorized, "step_up_required", err.Error())
	case errors.Is(err, lifecycle.ErrStale):
		writeCoded(w, r, http.StatusPreconditionFailed, "stale_version", err.Error())
	case errors.Is(err, lifecycle.ErrBadCredential):
		writeCoded(w, r, http.StatusUnauthorized, "bad_credential", err.Error())
	case errors.Is(err, lifecycle.ErrLinkInvalid):
		writeCoded(w, r, http.StatusBadRequest, "link_invalid", err.Error())
	case errors.Is(err, lifecycle.ErrNotifierUnconfigured):
		writeCoded(w, r, http.StatusNotImplemented, domain.ErrCodeUpstreamUnavailable,
			"notification is not configured in this deployment")
	case errors.Is(err, store.ErrPrincipalConflict):
		writeCoded(w, r, http.StatusConflict, "principal_conflict", err.Error())
	default:
		h.log.Error("lifecycle command failed", zap.Error(err))
		writeError(w, r, http.StatusInternalServerError, "lifecycle command failed")
	}
}

func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		return fwd
	}
	host, _, _ := splitHostPort(r.RemoteAddr)
	return host
}

func splitHostPort(addr string) (string, string, error) {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i], addr[i+1:], nil
		}
	}
	return addr, "", nil
}
