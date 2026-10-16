package handler

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/identity"
	"zoiko.io/authorization-svc/internal/siem"
)

// Session issue: step-up (register A-03 / S-3) and maximum lengths (B-01 /
// S-4), ZS-IAM-001 §13 and §14.
//
// §3: "MFA required according to risk/policy; no password-only privileged
// sessions"; "Step-up creates a new assurance assertion with bounded age".
// §13 (JIT): "Phishing-resistant or tenant/security-approved high-assurance
// authentication before session issue." §14 (break-glass): "High-assurance
// step-up; compromised/unverified session cannot break glass."
//
// Before this, a JIT or break-glass session was issued to anyone holding the
// permission, on the strength of whatever authenticated the request — in
// practice a header. Now the caller's identity envelope, verified by
// internal/identity, must carry mfa_verified with an auth_time no older than
// the configured bound. The facts are read from the verified assertion on the
// request context, never from X-Assurance-Level or X-Authn-Age-Seconds, which
// a direct caller could write.

// SessionStepUpPolicy configures the step-up gate. The ZERO VALUE ENFORCES
// with the default bound: a deployment must opt out explicitly (Off, which
// config only allows with ENV=local), never forget to opt in.
type SessionStepUpPolicy struct {
	Off    bool
	MaxAge time.Duration
}

// DefaultSessionStepUpMaxAge is the "bounded age" of a step-up when none is
// configured: fifteen minutes.
const DefaultSessionStepUpMaxAge = 15 * time.Minute

// RequiredSessionAssurance is the level a step-up must reach. TOTP is the only
// second factor in the estate (identity-context-svc 000012); a phishing-
// resistant level waits on a WebAuthn factor and the §31 assurance taxonomy.
const RequiredSessionAssurance = "MFA"

// ActionSessionStepUp is the access_decision_log action every step-up check is
// recorded under.
const ActionSessionStepUp = "iam.session.step_up"

// SetSessionStepUp sets the step-up policy.
func (h *Handler) SetSessionStepUp(p SessionStepUpPolicy) {
	if p.MaxAge <= 0 {
		p.MaxAge = DefaultSessionStepUpMaxAge
	}
	h.sessionStepUp = p
}

// SessionLimits are the maximum session lengths in seconds. Zero leaves the
// store's built-in ceiling (24 h, 2 h, 24 h) in force.
type SessionLimits struct {
	JIT        int
	BreakGlass int
	Support    int
}

// SetSessionLimits sets the maximum session lengths.
func (h *Handler) SetSessionLimits(l SessionLimits) { h.sessionLimits = l }

func (l SessionLimits) forKind(kind string) int {
	switch kind {
	case "privileged":
		return l.JIT
	case "break_glass":
		return l.BreakGlass
	case "support":
		return l.Support
	}
	return 0
}

// refuseOverLongSession answers 400 when duration exceeds the kind's maximum.
// Never shortened (B-01).
func (h *Handler) refuseOverLongSession(w http.ResponseWriter, kind string, duration int) bool {
	max := h.sessionLimits.forKind(kind)
	if max <= 0 || duration <= max {
		return false
	}
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"error":                "duration_exceeds_maximum",
		"field":                "duration_seconds",
		"requested_seconds":    duration,
		"max_duration_seconds": max,
		"message": fmt.Sprintf("a %s session may last at most %d seconds; request a shorter one (it is never shortened for you)",
			sessionKindLabel(kind), max),
	})
	return true
}

func sessionKindLabel(kind string) string {
	switch kind {
	case "privileged":
		return "JIT"
	case "break_glass":
		return "break-glass"
	}
	return kind
}

// requireSessionStepUp is the A-03 gate. act names what is being done
// ("issue_jit", "approve_jit", "issue_break_glass"). It records the check
// either way and returns the evidence to store on the session.
//
// Refusals are 401 step_up_required: the remedy is to authenticate again with
// the second factor (identity-context-svc POST /v1/authenticate/step-up) and
// retry with the new envelope.
func (h *Handler) requireSessionStepUp(w http.ResponseWriter, r *http.Request, caller, tenant, act string) (*domain.StepUpEvidence, bool) {
	a, _ := identity.FromContext(r.Context())
	maxAge := h.sessionStepUp.MaxAge
	if maxAge <= 0 {
		maxAge = DefaultSessionStepUpMaxAge
	}

	reason := ""
	var age time.Duration
	switch {
	case a == nil:
		reason = "no_verified_identity"
	case a.Basis != identity.BasisEnvelope:
		// A delegate service vouches for who; it cannot vouch for how
		// recently that person proved it.
		reason = "delegated_caller"
	case a.PrincipalID != caller:
		reason = "assertion_principal_mismatch"
	case !a.MFAVerified:
		reason = "single_factor"
	case a.AuthTime.IsZero():
		reason = "authn_time_unknown"
	default:
		age = time.Since(a.AuthTime)
		if age > maxAge {
			reason = "step_up_stale"
		} else if age < -time.Minute {
			reason = "authn_time_in_future"
		}
	}

	if reason != "" && h.sessionStepUp.Off {
		// ENV=local only (config refuses it elsewhere). Recorded as such, and
		// whatever WAS verified is still stored, so a local session is never
		// mistaken for a stepped-up one.
		h.recordStepUp(r, caller, tenant, act, "GRANTED", "step_up_disabled:"+reason)
		h.log.Warn("session step-up skipped (AUTHZ_SESSION_STEP_UP=off)", zap.String("act", act), zap.String("reason", reason))
		if a != nil && a.Basis == identity.BasisEnvelope && !a.AuthTime.IsZero() {
			return &domain.StepUpEvidence{Assurance: a.AssuranceLevel(), AuthnAt: a.AuthTime, SessionContextID: a.SessionContextID}, true
		}
		return nil, true
	}

	if reason != "" {
		if !h.recordStepUp(r, caller, tenant, act, "DENIED", "step_up_required:"+reason) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			return nil, false
		}
		h.siem.Stream(r.Context(), tenant, "authorization.session_step_up_required", siem.SeverityHigh,
			"Session "+act+" by "+caller+" refused: "+reason)
		w.Header().Set("WWW-Authenticate", `Bearer realm="authorization-svc", error="insufficient_user_authentication", acr_values="MFA", max_age="`+
			strconv.Itoa(int(maxAge.Seconds()))+`"`)
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error":                 "step_up_required",
			"reason":                reason,
			"required_assurance":    RequiredSessionAssurance,
			"max_authn_age_seconds": int(maxAge.Seconds()),
			"message": "issuing or approving a privileged or break-glass session needs a recent second-factor authentication " +
				"(ZS-IAM-001 §13/§14): step up at identity-context-svc POST /v1/authenticate/step-up and retry with the new envelope",
		})
		return nil, false
	}

	ev := &domain.StepUpEvidence{Assurance: RequiredSessionAssurance, AuthnAt: a.AuthTime, SessionContextID: a.SessionContextID}
	if !h.recordStepUp(r, caller, tenant, act, "GRANTED",
		fmt.Sprintf("assurance:%s authn_age_seconds:%d", RequiredSessionAssurance, int(age.Seconds()))) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return nil, false
	}
	return ev, true
}

// recordStepUp writes the step-up check to access_decision_log. A failure to
// record refuses the act: a privileged session whose step-up left no evidence
// is the thing §13 forbids.
func (h *Handler) recordStepUp(r *http.Request, caller, tenant, act, outcome, basis string) bool {
	if _, err := h.store.RecordAccessDecision(r.Context(), domain.RecordAccessDecisionParams{
		PrincipalID:   caller,
		LegalEntityID: tenant,
		ActionType:    ActionSessionStepUp + "." + act,
		Outcome:       outcome,
		Basis:         basis,
		CorrelationID: r.Header.Get("X-Correlation-ID"),
		TenantID:      tenant,
	}); err != nil {
		h.log.Error("session step-up decision could not be recorded — refusing", zap.Error(err))
		return false
	}
	return true
}
