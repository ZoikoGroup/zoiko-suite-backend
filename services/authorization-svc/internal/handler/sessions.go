package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/siem"
)

// Session controls (ZS-IAM-001 §13-15; migration 000030). The final re-audit
// (12 Oct 2026) found an iam.pam.manage holder could issue themselves a ["*"]
// JIT session and act on it at once, break-glass likewise, and a read-only
// support session granted PAYMENT_RELEASE. What is enforced now:
//
//	JIT (§13)          subject holds iam.pam.eligible; resource, environment
//	                   and duration named; no wildcard; any high-impact action
//	                   (iam.*, protected, payment / posting / filing) makes the
//	                   session PENDING_APPROVAL until an independent holder of
//	                   iam.pam.approve — never the subject or the requester —
//	                   approves it.
//	break-glass (§14)  subject holds iam.break_glass.eligible; no wildcard;
//	                   no payment / posting / filing power (no emergency policy
//	                   allows one); CRITICAL SIEM alert on issue.
//	support (§15)      no wildcard; consent stated; read-only grants reads
//	                   only, a writable session only the writes it names.
//	all                §16 reason on revoke; §23 started/ended events from the
//	                   outbox; JIT high-impact and break-glass sessions open a
//	                   review when they end, attested by iam.session.review.

// sessionRequest is what checkSessionRequest validates.
type sessionRequest struct {
	kind          string // "privileged", "break_glass", "support"
	subject       string
	tenant        string
	actions       []string
	resourceScope string
	environment   string
	duration      int
}

// checkSessionRequest applies the request-time controls. It writes the
// refusal and returns false.
func (h *Handler) checkSessionRequest(w http.ResponseWriter, r *http.Request, req sessionRequest) bool {
	for _, a := range req.actions {
		if domain.IsWildcardAction(a) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": "requested_actions",
				"message": "a session names the narrowest actions necessary; a wildcard is not accepted (ZS-IAM-001 §13, §14)"})
			return false
		}
	}
	switch req.kind {
	case "privileged":
		missing := ""
		switch {
		case strings.TrimSpace(req.resourceScope) == "":
			missing = "resource_scope"
		case strings.TrimSpace(req.environment) == "":
			missing = "target_environment"
		case req.duration <= 0:
			missing = "duration_seconds"
		}
		if missing != "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": missing,
				"message": "a JIT request names resource, actions, reason/ticket, duration and target environment (ZS-IAM-001 §13)"})
			return false
		}
		return h.requireSubjectEligible(w, r, req.subject, req.tenant, domain.PermissionPAMEligible)
	case "break_glass":
		for _, a := range req.actions {
			if domain.IsFinancialAction(a) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": "requested_actions",
					"message": a + " is a payment, posting or filing power; those stay separately constrained under break-glass (ZS-IAM-001 §14)"})
				return false
			}
		}
		return h.requireSubjectEligible(w, r, req.subject, req.tenant, domain.PermissionBreakGlassEligible)
	}
	return true
}

// requireSubjectEligible: the session's SUBJECT — not the issuer — must hold
// the eligibility permission (§13 "eligibility role"; §14 "small named
// population").
func (h *Handler) requireSubjectEligible(w http.ResponseWriter, r *http.Request, subject, tenant, permission string) bool {
	ok, err := h.holdsPermission(r, subject, tenant, permission)
	if err != nil {
		h.log.Error("session eligibility check failed — refusing", zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return false
	}
	if !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "authorization_denied",
			"message": "the session's subject does not hold " + permission})
		return false
	}
	return true
}

// ApprovePrivilegedSession — POST …/privileged-sessions/{session_id}/approve.
func (h *Handler) ApprovePrivilegedSession(w http.ResponseWriter, r *http.Request) {
	h.decidePrivilegedSession(w, r, true)
}

// RejectPrivilegedSession — POST …/privileged-sessions/{session_id}/reject.
func (h *Handler) RejectPrivilegedSession(w http.ResponseWriter, r *http.Request) {
	h.decidePrivilegedSession(w, r, false)
}

func (h *Handler) decidePrivilegedSession(w http.ResponseWriter, r *http.Request, approve bool) {
	approver, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if !h.requirePermission(w, r, approver, tenantScope, domain.PermissionPAMApprove) {
		return
	}
	if _, ok := h.readCommand(w, &r); !ok {
		return
	}
	sessionID := chi.URLParam(r, "session_id")
	ps, err := h.store.FindPrivilegedSessionByID(r.Context(), sessionID, tenantScope)
	if errors.Is(err, domain.ErrPrivilegedSessionNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "privileged_session_not_found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	// §13 "independent approval": never the subject, never the requester
	// (also a CHECK in 000030).
	if approver == ps.PrincipalID || (ps.RequestedBy != nil && approver == *ps.RequestedBy) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "checker_not_independent",
			"message": "a JIT session is approved by someone other than its subject and its requester"})
		return
	}
	decided, err := h.store.DecidePrivilegedSession(r.Context(), sessionID, tenantScope, approver, approve)
	switch {
	case errors.Is(err, domain.ErrSessionNotPending):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "approval_not_pending",
			"message": "the session is not awaiting approval, or its approval window has passed"})
		return
	case err != nil:
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, decided)
}

type sessionReviewRequest struct {
	Outcome string `json:"outcome"` // ATTESTED or FLAGGED
	Notes   string `json:"notes"`
}

// ReviewPrivilegedSession — POST …/privileged-sessions/{session_id}/review.
func (h *Handler) ReviewPrivilegedSession(w http.ResponseWriter, r *http.Request) {
	h.reviewSession(w, r, "privileged")
}

// ReviewBreakGlassSession — POST …/break-glass-sessions/{session_id}/review.
func (h *Handler) ReviewBreakGlassSession(w http.ResponseWriter, r *http.Request) {
	h.reviewSession(w, r, "break_glass")
}

// reviewSession records the §13/§14 retrospective review. The reviewer holds
// iam.session.review and is neither the subject nor the requester; FLAGGED
// raises a CRITICAL SIEM alert (§13 "suspicious actions create security
// incident workflow").
func (h *Handler) reviewSession(w http.ResponseWriter, r *http.Request, kind string) {
	reviewer, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if !h.requirePermission(w, r, reviewer, tenantScope, domain.PermissionSessionReview) {
		return
	}
	var req sessionReviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	outcome := strings.ToUpper(strings.TrimSpace(req.Outcome))
	if outcome != domain.SessionReviewAttested && outcome != domain.SessionReviewFlagged {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": "outcome",
			"message": "outcome is ATTESTED or FLAGGED"})
		return
	}
	if outcome == domain.SessionReviewFlagged && strings.TrimSpace(req.Notes) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "notes",
			"message": "a flagged session says what was found"})
		return
	}

	sessionID := chi.URLParam(r, "session_id")
	subject, requester, err := h.sessionParties(r, kind, sessionID, tenantScope)
	if err != nil {
		if errors.Is(err, domain.ErrPrivilegedSessionNotFound) || errors.Is(err, domain.ErrBreakGlassSessionNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	if reviewer == subject || reviewer == requester {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "checker_not_independent",
			"message": "a session is reviewed by someone other than its subject and its requester"})
		return
	}
	switch err := h.store.ReviewSession(r.Context(), kind, sessionID, tenantScope, reviewer, outcome, req.Notes); {
	case errors.Is(err, domain.ErrSessionReviewNotPending):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "invalid_state",
			"message": "this session has no pending review (it is still running, needs none, or was already reviewed)"})
		return
	case err != nil:
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	if outcome == domain.SessionReviewFlagged {
		h.siem.Stream(r.Context(), tenantScope, "security.session.review_flagged", siem.SeverityCritical,
			kind+" session "+sessionID+" flagged by "+reviewer+": "+req.Notes)
	}
	writeJSON(w, http.StatusOK, map[string]string{"session_id": sessionID, "review_status": outcome, "reviewed_by": reviewer})
}

func (h *Handler) sessionParties(r *http.Request, kind, sessionID, tenantID string) (subject, requester string, err error) {
	if kind == "break_glass" {
		bg, err := h.store.FindBreakGlassSessionByID(r.Context(), sessionID, tenantID)
		if err != nil {
			return "", "", err
		}
		if bg.RequestedBy != nil {
			requester = *bg.RequestedBy
		}
		return bg.PrincipalID, requester, nil
	}
	ps, err := h.store.FindPrivilegedSessionByID(r.Context(), sessionID, tenantID)
	if err != nil {
		return "", "", err
	}
	if ps.RequestedBy != nil {
		requester = *ps.RequestedBy
	}
	return ps.PrincipalID, requester, nil
}
