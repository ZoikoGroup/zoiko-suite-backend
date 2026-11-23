package handler

// REF-07 accounting policy lifecycle (ZS-SVC-I-001 s4.17): Draft -> Review ->
// Approved -> Active -> Superseded/Suspended.
//
//	POST /v1/accounting-policies/versions/{id}/submit-review      author
//	POST /v1/accounting-policies/versions/{id}/approve            ApprovePolicyVersion
//	POST /v1/accounting-policies/versions/{id}/activate           ActivatePolicyVersion
//	POST /v1/accounting-policies/versions/{id}/supersede          SupersedePolicyVersion
//	POST /v1/accounting-policies/versions/{id}/emergency-suspend  EmergencySuspendPolicy
//	POST /v1/accounting-policies/versions/{id}/review-suspension  independent review of a suspension
//	GET  /v1/accounting-policies/versions/{id}/impact             what activating it would touch
//
// "Author cannot solely approve material policy": the author can neither
// approve nor activate their own version, and the suspender cannot review their
// own suspension. "Activation requires designated accounting authority": it is
// its own permission, apart from approving. "Emergency suspension separately
// authorized": its own permission again, honoured only under a live
// break-glass session, so no standing grant can suspend a policy on its own.
//
// Replacing an active version is stated, never a side effect: activate refuses
// while one governs the scope, and supersede names the version it replaces.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/policy-svc/internal/authz"
	"zoiko.io/policy-svc/internal/domain"
	svcmiddleware "zoiko.io/policy-svc/internal/middleware"
)

const (
	codeSupersessionRequired    = "SUPERSESSION_REQUIRED"
	codeEmergencySessionMissing = "EMERGENCY_SESSION_REQUIRED"

	// HeaderBreakGlassSession carries the live break-glass session an
	// emergency suspension is made under.
	HeaderBreakGlassSession = "X-Break-Glass-Session-Id"
)

// accountingImpactStore is the store's impact report, optional like the rest.
type accountingImpactStore interface {
	AccountingImpactOf(ctx context.Context, versionID string) (*domain.AccountingImpact, error)
}

// emergencyAuthorizer is the authorization client's session-bound check. A
// client without it cannot honour an emergency act, so the act is refused.
type emergencyAuthorizer interface {
	CheckEmergency(ctx context.Context, principalID, legalEntityID, actionType, tenantID, breakGlassSessionID string) (string, error)
}

// accountingCommands maps a route to the lifecycle command and the permission
// it needs.
var accountingCommands = map[string]struct{ command, action string }{
	"submit-review":     {domain.CommandSubmitReview, authz.ActionAccountingPolicyDraft},
	"approve":           {domain.CommandApprove, authz.ActionAccountingPolicyApprove},
	"activate":          {domain.CommandActivate, authz.ActionAccountingPolicyActivate},
	"supersede":         {domain.CommandActivate, authz.ActionAccountingPolicyActivate},
	"emergency-suspend": {domain.CommandSuspend, authz.ActionAccountingPolicyEmergencySuspend},
	// Reviewing a suspension is a checker's act, held by approvers.
	"review-suspension": {domain.CommandReviewSuspension, authz.ActionAccountingPolicyApprove},
}

type accountingLifecycleRequest struct {
	lifecycleRequest
	// SupersedesVersionID is required by supersede: the active version it replaces.
	SupersedesVersionID string `json:"supersedes_version_id,omitempty"`
}

// breakGlassBasis reports whether an authorization decision's basis is a
// break-glass session, and that session is the one named.
func breakGlassBasis(basis, sessionID string) bool {
	return strings.HasPrefix(basis, "break_glass:") && strings.Contains(basis, "session="+sessionID)
}

// accountingCommand handles one lifecycle route.
//
// Every command needs the version's row_version (If-Match or expected_version;
// 428 without). emergency-suspend and review-suspension also need reason_code,
// review-suspension an outcome, supersede a supersedes_version_id, and
// emergency-suspend the X-Break-Glass-Session-Id of a live session.
//
//	200 transitioned, or already in the target state (idempotent)
//	400 malformed · 401 no identity · 403 denied, maker = checker, or no emergency session
//	404 not an accounting policy version · 428 no expected_version
//	409 SUPERSESSION_REQUIRED · APPROVAL_REQUIRED · CONCURRENCY_CONFLICT · POLICY_CONFLICTED
func (h *Handler) accountingCommand(route string) http.HandlerFunc {
	spec := accountingCommands[route]
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := h.accounting(w, r)
		if !ok {
			return
		}
		versionID := chi.URLParam(r, "version_id")
		principalID, ok := h.requirePrincipal(w, r)
		if !ok {
			return
		}
		if !requireUUID(w, r, "version_id", versionID) {
			return
		}
		if _, ok := h.requireTenant(w, r); !ok {
			return
		}
		var req accountingLifecycleRequest
		if !decodeOptionalJSON(w, r, &req) {
			return
		}

		expected, ok := ifMatch(r)
		if !ok {
			writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "invalid_if_match", "If-Match must be a row_version", nil)
			return
		}
		if req.ExpectedVersion != nil {
			if expected != nil && *expected != *req.ExpectedVersion {
				writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "expected_version_mismatch", "If-Match and expected_version disagree", nil)
				return
			}
			expected = req.ExpectedVersion
		}
		if expected == nil {
			writeError(w, r, http.StatusPreconditionRequired, codeInvalidRequest, "expected_version_required",
				"send the version's row_version as If-Match or expected_version (GET the version for its ETag)", nil)
			return
		}

		reason := strings.TrimSpace(req.ReasonCode)
		if (spec.command == domain.CommandSuspend || spec.command == domain.CommandReviewSuspension) && reason == "" {
			missingField(w, r, "reason_code")
			return
		}
		outcome := strings.ToUpper(strings.TrimSpace(req.Outcome))
		if spec.command == domain.CommandReviewSuspension && outcome != domain.SuspensionUpheld && outcome != domain.SuspensionOverturned {
			writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "invalid_outcome", "outcome must be UPHELD or OVERTURNED", nil)
			return
		}
		supersedes := strings.TrimSpace(req.SupersedesVersionID)
		switch route {
		case "supersede":
			if supersedes == "" {
				missingField(w, r, "supersedes_version_id")
				return
			}
			if !requireUUID(w, r, "supersedes_version_id", supersedes) {
				return
			}
		default:
			if supersedes != "" {
				writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "unexpected_supersedes_version_id",
					"supersedes_version_id belongs to the supersede command", nil)
				return
			}
		}
		if free := strings.TrimSpace(req.Reason); free != "" {
			if reason != "" {
				reason += ": "
			}
			reason += free
		}
		if tooLong(w, r, "reason", reason, 2000) || tooLong(w, r, "approval_reference", req.ApprovalReference, 256) {
			return
		}

		existing, err := s.GetAccountingPolicyVersion(r.Context(), versionID)
		if err != nil {
			if errors.Is(err, domain.ErrPolicyVersionNotFound) {
				writeError(w, r, http.StatusNotFound, codeNotFound, "policy_version_not_found", "", map[string]any{"policy_version_id": versionID})
				return
			}
			h.storeUnavailable(w, r, "accountingCommand", err)
			return
		}

		// The checker's actions name the maker, so authorization-svc's own-object
		// SoD applies as well as this service's own check.
		switch spec.command {
		case domain.CommandApprove, domain.CommandActivate:
			r = r.WithContext(authz.WithResourceOwner(r.Context(), existing.CreatedByPrincipalID))
		case domain.CommandReviewSuspension:
			if existing.SuspendedByPrincipalID != nil {
				r = r.WithContext(authz.WithResourceOwner(r.Context(), *existing.SuspendedByPrincipalID))
			}
		}

		sessionID := ""
		if route == "emergency-suspend" {
			var ok bool
			if sessionID, ok = h.authorizeEmergency(w, r, principalID, spec.action, existing.LegalEntityID); !ok {
				return
			}
			reason += "; break_glass_session=" + sessionID
		} else if !h.authorize(w, r, principalID, spec.action, existing.LegalEntityID) {
			return
		}

		res, err := h.store.Transition(r.Context(), domain.TransitionCommand{
			PolicyVersionID: versionID, Command: spec.command, ActorID: principalID, ExpectedVersion: expected,
			Reason: reason, ApprovalReference: strings.TrimSpace(req.ApprovalReference), Outcome: outcome,
			SupersedesVersionID: supersedes, BreakGlassSessionID: sessionID, Meta: eventMeta(r, principalID),
		})
		if err != nil {
			h.writeTransitionError(w, r, versionID, err)
			return
		}
		h.log.Info("accounting policy transition",
			zap.String("route", route), zap.String("policy_version_id", versionID),
			zap.String("status", res.Version.VersionStatus), zap.Bool("transitioned", res.Transitioned),
			zap.Int("superseded_count", len(res.Superseded)), zap.String("break_glass_session_id", sessionID),
			zap.String("correlation_id", r.Header.Get("X-Correlation-ID")))

		out, err := s.GetAccountingPolicyVersion(r.Context(), versionID)
		if err != nil {
			h.storeUnavailable(w, r, "accountingCommand", err)
			return
		}
		w.Header().Set("ETag", `"`+strconv.Itoa(out.RowVersion)+`"`)
		writeJSON(w, http.StatusOK, out)
	}
}

// authorizeEmergency authorizes an emergency act. It needs the live
// break-glass session in X-Break-Glass-Session-Id, forwards it to
// authorization-svc, and accepts the grant only if its basis is that session:
// a principal who merely holds the permission is refused, and so is one whose
// session has ended or was never approved. It returns the session id.
func (h *Handler) authorizeEmergency(w http.ResponseWriter, r *http.Request, principalID, action string, legalEntityID *string) (string, bool) {
	sessionID := strings.TrimSpace(r.Header.Get(HeaderBreakGlassSession))
	if sessionID == "" || !validUUID(sessionID) {
		writeError(w, r, http.StatusForbidden, codeEmergencySessionMissing, "emergency_session_required",
			"an emergency suspension is made under a live break-glass session: send its id in "+HeaderBreakGlassSession,
			map[string]any{"action_type": action})
		return "", false
	}
	ea, ok := h.authz.(emergencyAuthorizer)
	if !ok {
		h.log.Error("the authorization client cannot check break-glass sessions — refusing the emergency act", zap.String("action_type", action))
		writeError(w, r, http.StatusServiceUnavailable, codeUnavailable, "authz_unavailable", "", nil)
		return "", false
	}
	basis, err := ea.CheckEmergency(r.Context(), principalID, h.scopeFor(legalEntityID), action, svcmiddleware.TenantFromContext(r.Context()), sessionID)
	switch {
	case err == nil:
	case errors.Is(err, domain.ErrAuthorizationDenied):
		writeError(w, r, http.StatusForbidden, codeAuthorizationDenied, "authorization_denied", "", map[string]any{"action_type": action})
		return "", false
	default:
		h.log.Error("break-glass authorization failed — refusing the request", zap.String("principal_id", principalID), zap.Error(err))
		writeError(w, r, http.StatusServiceUnavailable, codeUnavailable, "authz_unavailable", "", nil)
		return "", false
	}
	if !breakGlassBasis(basis, sessionID) {
		writeError(w, r, http.StatusForbidden, codeEmergencySessionMissing, "emergency_session_required",
			"the grant did not come through that break-glass session: an emergency suspension is honoured only under a live, approved session",
			map[string]any{"action_type": action})
		return "", false
	}
	return sessionID, true
}

// GetAccountingPolicyImpact handles GET /v1/accounting-policies/versions/{version_id}/impact:
// what activating the version would do, worked out from the policies that exist.
func (h *Handler) GetAccountingPolicyImpact(w http.ResponseWriter, r *http.Request) {
	s, ok := h.store.(accountingImpactStore)
	if !ok {
		writeError(w, r, http.StatusServiceUnavailable, codeUnavailable, "store_unavailable", "the store does not support accounting policies", nil)
		return
	}
	id := chi.URLParam(r, "version_id")
	if !requireUUID(w, r, "version_id", id) {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if _, ok := h.gate(w, r, h.opts.ReadAuthz, authz.ActionPolicyRead, nil); !ok {
		return
	}
	impact, err := s.AccountingImpactOf(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrPolicyVersionNotFound) {
			writeError(w, r, http.StatusNotFound, codeNotFound, "policy_version_not_found", "", map[string]any{"policy_version_id": id})
			return
		}
		h.storeUnavailable(w, r, "GetAccountingPolicyImpact", err)
		return
	}
	writeJSON(w, http.StatusOK, impact)
}
