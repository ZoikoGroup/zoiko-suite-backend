package context

import (
	"encoding/json"
	"errors"
	"net/http"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/webauthn"
)

// Passkey routes (AU-11). Like the TOTP routes, each identifies its caller by
// the session envelope in Authorization (mfaCaller), never by X-Principal-Id.
// The browser half is navigator.credentials.create / .get: begin returns the
// options to pass it and a ceremony handle, finish takes the handle and the
// authenticator's response as the browser serialised it (base64url fields).

// Error codes on the passkey routes' bodies.
const (
	errCodePasskeysNotConfigured = "passkeys_not_configured"
	errCodeCeremonyInvalid       = "ceremony_invalid"
	errCodePasskeyRejected       = "passkey_rejected"
	errCodePasskeyDuplicate      = "passkey_duplicate"
)

// passkeyError maps the passkey errors, then falls back to the MFA mapping.
func (h *Handler) passkeyError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrPasskeysNotConfigured):
		writeCoded(w, r, http.StatusNotImplemented, errCodePasskeysNotConfigured, err.Error())
	case errors.Is(err, webauthn.ErrNoPasskey):
		writeCoded(w, r, http.StatusConflict, errCodeMFANotEnrolled, err.Error())
	case errors.Is(err, webauthn.ErrCeremonyInvalid):
		writeCoded(w, r, http.StatusBadRequest, errCodeCeremonyInvalid, err.Error())
	case errors.Is(err, webauthn.ErrRejected):
		writeCoded(w, r, http.StatusUnauthorized, errCodePasskeyRejected, err.Error())
	case errors.Is(err, webauthn.ErrDuplicate):
		writeCoded(w, r, http.StatusConflict, errCodePasskeyDuplicate, err.Error())
	case errors.Is(err, webauthn.ErrInvalid):
		writeError(w, r, http.StatusBadRequest, err.Error())
	case errors.Is(err, webauthn.ErrUnavailable):
		writeCoded(w, r, http.StatusServiceUnavailable, errCodeMFAUnavailable, "passkeys are temporarily unavailable")
	default:
		h.writeMFAError(w, r, err)
	}
}

// passkeyCeremony is a begin route's answer.
type passkeyCeremony struct {
	// Options is PublicKeyCredentialCreationOptions (registration) or
	// PublicKeyCredentialRequestOptions (assertion), under "publicKey".
	Options  json.RawMessage `json:"options"`
	Ceremony string          `json:"ceremony"`
}

type finishPasskeyRequest struct {
	Ceremony      string          `json:"ceremony"`
	Nickname      string          `json:"nickname"`
	Credential    json.RawMessage `json:"credential"`
	CorrelationID string          `json:"correlation_id"`
}

func decodeFinish(w http.ResponseWriter, r *http.Request) (*finishPasskeyRequest, bool) {
	var req finishPasskeyRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid request body")
		return nil, false
	}
	if req.Ceremony == "" || len(req.Credential) == 0 {
		writeError(w, r, http.StatusBadRequest, "ceremony and credential are required")
		return nil, false
	}
	return &req, true
}

// ── POST /v1/mfa/passkeys/register/begin ─────────────────────────────────────
//
// 200 {options, ceremony}. 401 reauthentication_required for a first factor
// on a stale login; 403 step_up_required when the caller already holds a
// factor and has not stepped up recently.
func (h *Handler) BeginPasskeyRegistration(w http.ResponseWriter, r *http.Request) {
	c, ok := h.mfaCaller(w, r)
	if !ok {
		return
	}
	opts, handle, err := h.mfa.BeginPasskeyRegistration(r.Context(), c, r.Header.Get("X-Correlation-ID"))
	if err != nil {
		h.passkeyError(w, r, err)
		return
	}
	noStore(w)
	writeJSON(w, http.StatusOK, passkeyCeremony{Options: opts, Ceremony: handle})
}

// ── POST /v1/mfa/passkeys/register/finish ────────────────────────────────────
//
// Body {ceremony, nickname, credential}. 201 with the registered passkey
// (no key material). 400 ceremony_invalid, 401 passkey_rejected, 409
// passkey_duplicate.
func (h *Handler) FinishPasskeyRegistration(w http.ResponseWriter, r *http.Request) {
	c, ok := h.mfaCaller(w, r)
	if !ok {
		return
	}
	req, ok := decodeFinish(w, r)
	if !ok {
		return
	}
	cred, err := h.mfa.FinishPasskeyRegistration(r.Context(), c, req.Ceremony, req.Nickname, req.Credential,
		mfaCorrelation(r, req.CorrelationID))
	if err != nil {
		h.passkeyError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, cred)
}

// ── GET /v1/mfa/passkeys ─────────────────────────────────────────────────────
//
// The caller's own active passkeys.
func (h *Handler) ListPasskeys(w http.ResponseWriter, r *http.Request) {
	c, ok := h.mfaCaller(w, r)
	if !ok {
		return
	}
	out, err := h.mfa.ListPasskeys(r.Context(), c)
	if err != nil {
		h.passkeyError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"passkeys": out, "count": len(out)})
}

type revokePasskeyRequest struct {
	CredentialID  string `json:"credential_id"`
	ReasonCode    string `json:"reason_code"`
	CorrelationID string `json:"correlation_id"`
}

// ── POST /v1/mfa/passkeys/revoke ─────────────────────────────────────────────
//
// Body {credential_id, reason_code}. Needs a recent step-up (403
// step_up_required); 404 factor_not_found for a passkey the caller does not
// hold.
func (h *Handler) RevokePasskey(w http.ResponseWriter, r *http.Request) {
	c, ok := h.mfaCaller(w, r)
	if !ok {
		return
	}
	var req revokePasskeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := h.mfa.RevokePasskey(r.Context(), c, req.CredentialID, req.ReasonCode, mfaCorrelation(r, req.CorrelationID)); err != nil {
		h.passkeyError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"credential_id": req.CredentialID, "status": domain.CredentialStatusRetired})
}

// ── POST /v1/authenticate/step-up/passkey/begin ──────────────────────────────
//
// 200 {options, ceremony}; 409 mfa_not_enrolled when the caller has no
// passkey.
func (h *Handler) BeginPasskeyStepUp(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.throttle(w, r, "step-up", ""); !ok {
		return
	}
	c, ok := h.mfaCaller(w, r)
	if !ok {
		return
	}
	opts, handle, err := h.mfa.BeginPasskeyStepUp(r.Context(), c, r.Header.Get("X-Correlation-ID"))
	if err != nil {
		h.passkeyError(w, r, err)
		return
	}
	noStore(w)
	writeJSON(w, http.StatusOK, passkeyCeremony{Options: opts, Ceremony: handle})
}

// ── POST /v1/authenticate/step-up/passkey/finish ─────────────────────────────
//
// Body {ceremony, credential}. 200 with a new login token (amr
// ["pwd","hwk"], auth_time = now) that /v1/context/resolve exchanges for an
// envelope with mfa_verified=true and assurance PHISHING_RESISTANT.
func (h *Handler) FinishPasskeyStepUp(w http.ResponseWriter, r *http.Request) {
	ip, ok := h.throttle(w, r, "step-up", "")
	if !ok {
		return
	}
	c, ok := h.mfaCaller(w, r)
	if !ok {
		return
	}
	req, ok := decodeFinish(w, r)
	if !ok {
		return
	}
	resp, err := h.mfa.FinishPasskeyStepUp(r.Context(), c, req.Ceremony, req.Credential, mfaCorrelation(r, req.CorrelationID))
	if err != nil && h.limiter != nil && errors.Is(err, webauthn.ErrRejected) {
		h.limiter.Failure(r.Context(), ip, c.Principal.TenantID)
	}
	if err != nil {
		h.passkeyError(w, r, err)
		return
	}
	noStore(w)
	writeJSON(w, http.StatusOK, resp)
}
