package context

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
)

// ActionResetPrincipalMFA is the authorization-svc action an administrator
// needs to reset someone else's second factor (credential recovery, ZS-IAM-001
// §3 "separately controlled").
const ActionResetPrincipalMFA = "IDENTITY_MFA_RESET"

// Error codes on the MFA routes' bodies.
const (
	errCodeMFAUnavailable   = "mfa_unavailable"
	errCodeSessionInvalid   = "session_invalid"
	errCodeReauthRequired   = "reauthentication_required"
	errCodeStepUpRequired   = "step_up_required"
	errCodeMFANotEnrolled   = "mfa_not_enrolled"
	errCodeFactorNotFound   = "factor_not_found"
	errCodeInvalidCode      = "invalid_code"
	errCodeIdentityMismatch = "identity_mismatch"
)

// mfaCaller verifies the session envelope in Authorization and checks that
// the contract headers, when present, name the same principal and tenant.
//
// The headers are required by the canonical input contract on these routes,
// as on /v1/context/resolve, because the caller already holds a verified
// session and can name itself honestly. They are never the source of identity:
// a header naming anyone but the envelope's principal is refused, not obeyed.
func (h *Handler) mfaCaller(w http.ResponseWriter, r *http.Request) (*MFACaller, bool) {
	if h.mfa == nil {
		writeError(w, r, http.StatusNotImplemented, "second factor is not wired on this deployment")
		return nil, false
	}
	bearer, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		writeCoded(w, r, http.StatusUnauthorized, errCodeSessionInvalid,
			"Authorization: Bearer <identity envelope> is required")
		return nil, false
	}
	c, err := h.mfa.Authenticate(r.Context(), bearer)
	if err != nil {
		h.writeMFAError(w, r, err)
		return nil, false
	}
	if hp := strings.TrimSpace(r.Header.Get("X-Principal-Id")); hp != "" && hp != c.Principal.PrincipalID {
		writeCoded(w, r, http.StatusForbidden, errCodeIdentityMismatch, "X-Principal-Id does not match the session")
		return nil, false
	}
	if ht := strings.TrimSpace(r.Header.Get("X-Tenant-Id")); ht != "" && ht != c.Principal.TenantID {
		writeCoded(w, r, http.StatusForbidden, errCodeIdentityMismatch, "X-Tenant-Id does not match the session")
		return nil, false
	}
	return c, true
}

// bearerToken extracts the token from an Authorization header.
func bearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return "", false
	}
	return strings.TrimSpace(token), true
}

func mfaCorrelation(r *http.Request, body string) string {
	if body != "" {
		return body
	}
	return r.Header.Get("X-Correlation-ID")
}

func (h *Handler) writeMFAError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrMFAUnavailable):
		writeCoded(w, r, http.StatusServiceUnavailable, errCodeMFAUnavailable, err.Error())
	case errors.Is(err, ErrMFASessionInvalid):
		writeCoded(w, r, http.StatusUnauthorized, errCodeSessionInvalid, err.Error())
	case errors.Is(err, ErrReauthRequired):
		writeCoded(w, r, http.StatusUnauthorized, errCodeReauthRequired, err.Error())
	case errors.Is(err, ErrMFAStepUpRequired):
		writeCoded(w, r, http.StatusForbidden, errCodeStepUpRequired, err.Error())
	case errors.Is(err, ErrMFANotEnrolled):
		writeCoded(w, r, http.StatusConflict, errCodeMFANotEnrolled, err.Error())
	case errors.Is(err, ErrMFAFactorNotFound):
		writeCoded(w, r, http.StatusNotFound, errCodeFactorNotFound, err.Error())
	case errors.Is(err, ErrInvalidCode):
		writeCoded(w, r, http.StatusUnauthorized, errCodeInvalidCode, err.Error())
	case errors.Is(err, ErrRequestInvalid):
		writeError(w, r, http.StatusBadRequest, err.Error())
	case errors.Is(err, domain.ErrPrincipalNotFound):
		writeError(w, r, http.StatusNotFound, "principal not found")
	case errors.Is(err, ErrAuthUnavailable):
		h.log.Error("mfa command unavailable", zap.Error(err))
		writeError(w, r, http.StatusServiceUnavailable, "second factor unavailable")
	default:
		h.log.Error("unexpected mfa error", zap.Error(err))
		writeError(w, r, http.StatusInternalServerError, "internal error")
	}
}

// noStore marks a response that carries a credential or a secret.
func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
}

// ── POST /v1/authenticate/step-up ────────────────────────────────────────────
//
// Body {code}. 200 with a new login token (mfa_done, auth_time = now, amr
// ["pwd","otp"]) that /v1/context/resolve exchanges for an envelope with
// mfa_verified=true. 401 invalid_code / session_invalid, 409 mfa_not_enrolled,
// 503 when no factor can be checked.
func (h *Handler) StepUp(w http.ResponseWriter, r *http.Request) {
	ip, ok := h.throttle(w, r, "step-up", "")
	if !ok {
		return
	}
	c, ok := h.mfaCaller(w, r)
	if !ok {
		return
	}
	var req domain.StepUpRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}
	resp, err := h.mfa.StepUp(r.Context(), c, req.Code, mfaCorrelation(r, req.CorrelationID))
	if err != nil && h.limiter != nil && errors.Is(err, ErrInvalidCode) {
		h.limiter.Failure(r.Context(), ip, c.Principal.TenantID)
	}
	if err != nil {
		h.writeMFAError(w, r, err)
		return
	}
	noStore(w)
	writeJSON(w, http.StatusOK, resp)
}

// ── POST /v1/mfa/totp/enroll ─────────────────────────────────────────────────
//
// 201 with the factor id, the secret and its otpauth:// URI, exactly once. The
// factor is PENDING until activated. 401 reauthentication_required for a first
// factor on a stale login; 403 step_up_required to replace an active factor.
func (h *Handler) EnrollTOTP(w http.ResponseWriter, r *http.Request) {
	c, ok := h.mfaCaller(w, r)
	if !ok {
		return
	}
	resp, err := h.mfa.Enrol(r.Context(), c, r.Header.Get("X-Correlation-ID"))
	if err != nil {
		h.writeMFAError(w, r, err)
		return
	}
	noStore(w)
	writeJSON(w, http.StatusCreated, resp)
}

type activateTOTPRequest struct {
	FactorID      string `json:"factor_id"`
	Code          string `json:"code"`
	CorrelationID string `json:"correlation_id"`
}

// ── POST /v1/mfa/totp/activate ───────────────────────────────────────────────
//
// Body {factor_id, code}. 200 with the now-ACTIVE factor; any previous factor
// is retired in the same transaction.
func (h *Handler) ActivateTOTP(w http.ResponseWriter, r *http.Request) {
	c, ok := h.mfaCaller(w, r)
	if !ok {
		return
	}
	var req activateTOTPRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}
	f, err := h.mfa.Activate(r.Context(), c, req.FactorID, req.Code, mfaCorrelation(r, req.CorrelationID))
	if err != nil {
		h.writeMFAError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

type revokeTOTPRequest struct {
	FactorID      string `json:"factor_id"`
	ReasonCode    string `json:"reason_code"`
	CorrelationID string `json:"correlation_id"`
}

// ── POST /v1/mfa/totp/revoke ─────────────────────────────────────────────────
//
// Body {factor_id, reason_code}. Needs a recent step-up (403 step_up_required).
func (h *Handler) RevokeTOTP(w http.ResponseWriter, r *http.Request) {
	c, ok := h.mfaCaller(w, r)
	if !ok {
		return
	}
	var req revokeTOTPRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := h.mfa.Revoke(r.Context(), c, req.FactorID, req.ReasonCode, mfaCorrelation(r, req.CorrelationID)); err != nil {
		h.writeMFAError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"factor_id": req.FactorID, "status": domain.CredentialStatusRetired})
}

// ── GET /v1/mfa/factors ──────────────────────────────────────────────────────
//
// The caller's own factors, without secrets.
func (h *Handler) ListMFAFactors(w http.ResponseWriter, r *http.Request) {
	c, ok := h.mfaCaller(w, r)
	if !ok {
		return
	}
	out, err := h.mfa.List(r.Context(), c)
	if err != nil {
		h.writeMFAError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"factors": out, "count": len(out)})
}

type resetMFARequest struct {
	ReasonCode string `json:"reason_code"`
	Purpose    string `json:"purpose"`
}

// ── POST /v1/principals/{principalID}/mfa/reset ──────────────────────────────
//
// An administrator retires every factor of another principal (a lost device).
// The administrator is identified by their own session envelope, must hold
// IDENTITY_MFA_RESET in authorization-svc, must have stepped up recently, and
// may not reset themselves (self-service revoke exists for that, behind the
// same step-up). Body {reason_code, purpose}; reason_code is mandatory.
func (h *Handler) ResetPrincipalMFA(w http.ResponseWriter, r *http.Request) {
	c, ok := h.mfaCaller(w, r)
	if !ok {
		return
	}
	subject := chi.URLParam(r, "principalID")
	if subject == c.Principal.PrincipalID {
		writeCoded(w, r, http.StatusForbidden, "self_reset_refused",
			"an administrator cannot reset their own second factor; use /v1/mfa/totp/revoke")
		return
	}
	var req resetMFARequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.ReasonCode) == "" {
		writeError(w, r, http.StatusBadRequest, "reason_code is required")
		return
	}
	if !h.authorize(w, r, c.Principal.PrincipalID, c.Principal.TenantID, ActionResetPrincipalMFA) {
		return
	}
	n, err := h.mfa.ResetFactors(r.Context(), c, subject, req.ReasonCode, req.Purpose, r.Header.Get("X-Correlation-ID"))
	if err != nil {
		h.writeMFAError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"principal_id": subject, "factors_retired": n})
}
