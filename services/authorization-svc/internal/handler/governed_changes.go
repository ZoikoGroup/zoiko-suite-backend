package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/siem"
)

// Governed changes — the four auth-service audit rows this service owned
// after 14 Oct 2026 (migrations 000034-000036):
//
//	AU-02  break-glass is a request until independent, stepped-up approvers
//	       approve it (GOV-12 Requested -> Approved -> Active; decision D-3:
//	       quorum AUTHZ_BREAK_GLASS_APPROVALS, default 1)
//	AU-03  authority limits are administered: create, change and retire are
//	       requests; a different person approves (ZS-IAM-001 §12, §21-23)
//	AU-13  every decision made in a JIT, break-glass or support session names
//	       it, break-glass decisions carry the BREAK_GLASS tag, and a session's
//	       activity is readable for its review (§13-15, §20)
//	AU-17  SoD and ABAC rule changes take effect only once a different person
//	       publishes them (§8 PAP "no direct runtime mutation without
//	       controlled publication")

// governedStore is the store capability behind these routes; cache.Store
// forwards it (internal/cache/governed.go).
type governedStore interface {
	ApproveBreakGlassSession(ctx context.Context, sessionID, tenantID, approver string, stepUp *domain.StepUpEvidence) (*domain.BreakGlassSession, error)
	RejectBreakGlassSession(ctx context.Context, sessionID, tenantID, decider string) (*domain.BreakGlassSession, error)
	FindSoDRule(ctx context.Context, sodRuleID, tenantID string) (*domain.SoDRule, error)
	RequestSoDRuleChange(ctx context.Context, sodRuleID, tenantID, change, requester, reason string, expectedVersion int64) (*domain.SoDRule, bool, error)
	DecideSoDRuleChange(ctx context.Context, sodRuleID, tenantID string, platformWide bool, decider string, approve bool) (*domain.SoDRule, error)
	FindABACRule(ctx context.Context, abacRuleID, tenantID string) (*domain.ABACRule, error)
	RequestABACRuleChange(ctx context.Context, abacRuleID, tenantID, change, requester, reason string, expectedVersion int64) (*domain.ABACRule, bool, error)
	DecideABACRuleChange(ctx context.Context, abacRuleID, tenantID string, platformWide bool, decider string, approve bool) (*domain.ABACRule, error)
	RequestAuthorityLimitRetire(ctx context.Context, limitID, tenantID, requester, reason string, expectedVersion int64) (*domain.AuthorityLimit, error)
	DecideAuthorityLimit(ctx context.Context, limitID, tenantID, decider string, approve bool) (*domain.AuthorityLimit, error)
}

func (h *Handler) governedStore(w http.ResponseWriter, r *http.Request) (governedStore, bool) {
	g, ok := h.store.(governedStore)
	if !ok {
		writeError(w, r, http.StatusServiceUnavailable, "store_unavailable", "", nil)
	}
	return g, ok
}

// SetBreakGlassApprovals sets the break-glass quorum (AUTHZ_BREAK_GLASS_APPROVALS).
// Below 1 is read as 1: break-glass is never self-activating.
func (h *Handler) SetBreakGlassApprovals(n int) {
	h.breakGlassApprovals = n
}

func (h *Handler) breakGlassQuorum() int {
	if h.breakGlassApprovals < 1 {
		return 1
	}
	return h.breakGlassApprovals
}

// governedErr maps the store's diagnosis of a refused governed change.
func (h *Handler) writeGovernedErr(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, domain.ErrBreakGlassSessionNotFound), errors.Is(err, domain.ErrSoDRuleNotFound),
		errors.Is(err, domain.ErrABACRuleNotFound), errors.Is(err, domain.ErrAuthorityLimitNotFound):
		writeError(w, r, http.StatusNotFound, "not_found", "", nil)
	case errors.Is(err, domain.ErrCheckerNotIndependent):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "checker_not_independent",
			"message": "a change is approved or rejected by someone other than the person who asked for it, and other than the person it is about"})
	case errors.Is(err, domain.ErrSessionNotPending), errors.Is(err, domain.ErrNoPendingChange):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "approval_not_pending",
			"message": "nothing is awaiting a decision here — it was already decided, or its approval window passed"})
	case errors.Is(err, domain.ErrAlreadyApproved):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "already_approved",
			"message": "you have already approved this; a quorum needs a different approver"})
	case errors.Is(err, domain.ErrChangePending):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "change_pending",
			"message": "a change is already awaiting approval; it must be decided first"})
	case errors.Is(err, domain.ErrRuleRejected):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "rule_rejected",
			"message": "this rule was rejected when it was proposed and can never take effect; propose a new one"})
	case errors.Is(err, domain.ErrVersionConflict):
		writeVersionConflict(w)
	case errors.Is(err, domain.ErrInvalidTransition):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "invalid_transition",
			"message": "only a limit in force can be retired"})
	case errors.Is(err, domain.ErrConflict):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "superseded_limit_changed",
			"message": "the limit this change replaces is no longer in force; reject this change and request a new one"})
	case errors.Is(err, domain.ErrAuthorityLimitInvalid):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_authority_limit", "message": err.Error()})
	default:
		h.log.Error(op+": store unavailable", zap.Error(err))
		writeError(w, r, http.StatusServiceUnavailable, "store_unavailable", "", nil)
	}
}

// ── AU-13: the session a decision is made in ─────────────────────────────────

// requestSessions are the sessions a request named that are ACTIVE, in time,
// and the requesting principal's own. A named session that is none of those
// confers nothing and is not recorded as the decision's session.
type requestSessions struct {
	privileged *domain.PrivilegedSession
	breakGlass *domain.BreakGlassSession
	support    *domain.SupportSession
}

func (h *Handler) resolveSessions(ctx context.Context, in evalContext) (requestSessions, error) {
	var out requestSessions
	now := time.Now().UTC()
	if in.PrivilegedSessionID != "" {
		ps, err := h.store.FindPrivilegedSessionByID(ctx, in.PrivilegedSessionID, in.TenantID)
		if err != nil && !errors.Is(err, domain.ErrPrivilegedSessionNotFound) {
			return out, fmt.Errorf("privileged session lookup: %w", err)
		}
		if err == nil && ps != nil && ps.PrincipalID == in.PrincipalID && ps.Status == domain.PrivilegedSessionStatusActive && now.Before(ps.ExpiresAt) {
			out.privileged = ps
		}
	}
	if in.BreakGlassSessionID != "" {
		bg, err := h.store.FindBreakGlassSessionByID(ctx, in.BreakGlassSessionID, in.TenantID)
		if err != nil && !errors.Is(err, domain.ErrBreakGlassSessionNotFound) {
			return out, fmt.Errorf("break-glass session lookup: %w", err)
		}
		// ACTIVE only: a PENDING_APPROVAL request (AU-02) grants nothing.
		if err == nil && bg != nil && bg.PrincipalID == in.PrincipalID && bg.IncidentID != "" && bg.Status == domain.BreakGlassSessionStatusActive && now.Before(bg.ExpiresAt) {
			out.breakGlass = bg
		}
	}
	if in.SupportSessionID != "" {
		ss, err := h.store.FindSupportSessionByID(ctx, in.SupportSessionID, in.TenantID)
		if err != nil && !errors.Is(err, domain.ErrSupportSessionNotFound) {
			return out, fmt.Errorf("support session lookup: %w", err)
		}
		if err == nil && ss != nil && ss.SupportOperatorID == in.PrincipalID && ss.Status == domain.SupportSessionStatusActive && now.Before(ss.ExpiresAt) {
			out.support = ss
		}
	}
	return out, nil
}

// tagSessionEvidence names the session on the decision. Break-glass ranks
// first (§14 "all commands tagged BREAK_GLASS, with reason, incident ID and
// session ID"), then JIT, then support.
func tagSessionEvidence(res *evalResult, sess requestSessions) {
	switch {
	case sess.breakGlass != nil:
		res.SessionKind, res.SessionID = domain.SessionKindBreakGlass, sess.breakGlass.SessionID
		res.ReasonCodes = appendOnce(res.ReasonCodes, "BREAK_GLASS")
		res.Obligations = appendOnce(res.Obligations, "EVIDENCE_TAG:BREAK_GLASS:incident="+sess.breakGlass.IncidentID)
	case sess.privileged != nil:
		res.SessionKind, res.SessionID = domain.SessionKindPrivileged, sess.privileged.SessionID
		res.ReasonCodes = appendOnce(res.ReasonCodes, "PRIVILEGED_SESSION")
	case sess.support != nil:
		res.SessionKind, res.SessionID = domain.SessionKindSupport, sess.support.SessionID
		res.ReasonCodes = appendOnce(res.ReasonCodes, "SUPPORT_SESSION")
	}
}

func appendOnce(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// maxActivityDecisions bounds one activity read; a longer session reports
// truncated and the decision log pages the rest by session_id.
const maxActivityDecisions = 500

// PrivilegedSessionActivity — GET …/privileged-sessions/{session_id}/activity.
func (h *Handler) PrivilegedSessionActivity(w http.ResponseWriter, r *http.Request) {
	h.sessionActivity(w, r, domain.SessionKindPrivileged)
}

// BreakGlassSessionActivity — GET …/break-glass-sessions/{session_id}/activity.
func (h *Handler) BreakGlassSessionActivity(w http.ResponseWriter, r *http.Request) {
	h.sessionActivity(w, r, domain.SessionKindBreakGlass)
}

// SupportSessionActivity — GET /v1/support/sessions/{session_id}/activity.
func (h *Handler) SupportSessionActivity(w http.ResponseWriter, r *http.Request) {
	h.sessionActivity(w, r, domain.SessionKindSupport)
}

// sessionActivity answers "what was done in this session": every decision
// recorded against it, with a summary of the actions and resources it
// touched (GOV-12 GetEmergencySessionEvidence; §15 support summary). Readable
// by a session reviewer or by whoever administers that kind of session.
func (h *Handler) sessionActivity(w http.ResponseWriter, r *http.Request, kind string) {
	caller, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	manage := map[string]string{
		domain.SessionKindPrivileged: "iam.pam.manage",
		domain.SessionKindBreakGlass: "iam.break_glass.manage",
		domain.SessionKindSupport:    "iam.support.manage",
	}[kind]
	allowed := false
	for _, p := range []string{domain.PermissionSessionReview, manage} {
		held, err := h.holdsPermission(r, caller, tenantScope, p)
		if err != nil {
			writeError(w, r, http.StatusServiceUnavailable, "store_unavailable", "", nil)
			return
		}
		if held {
			allowed = true
			break
		}
	}
	if !allowed {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "authorization_denied",
			"message": domain.PermissionSessionReview + " or " + manage + " is required to read a session's activity"})
		return
	}

	sessionID := chi.URLParam(r, "session_id")
	var err error
	switch kind {
	case domain.SessionKindPrivileged:
		_, err = h.store.FindPrivilegedSessionByID(r.Context(), sessionID, tenantScope)
	case domain.SessionKindBreakGlass:
		_, err = h.store.FindBreakGlassSessionByID(r.Context(), sessionID, tenantScope)
	default:
		_, err = h.store.FindSupportSessionByID(r.Context(), sessionID, tenantScope)
	}
	if errors.Is(err, domain.ErrPrivilegedSessionNotFound) || errors.Is(err, domain.ErrBreakGlassSessionNotFound) || errors.Is(err, domain.ErrSupportSessionNotFound) {
		writeError(w, r, http.StatusNotFound, "session_not_found", "", nil)
		return
	}
	if err != nil {
		writeError(w, r, http.StatusServiceUnavailable, "store_unavailable", "", nil)
		return
	}

	page, err := h.store.ListAccessDecisions(r.Context(), tenantScope, domain.ListAccessDecisionsParams{
		SessionID: sessionID, Limit: maxActivityDecisions,
	})
	if err != nil {
		h.log.Error("sessionActivity: decision read failed", zap.Error(err))
		writeError(w, r, http.StatusServiceUnavailable, "store_unavailable", "", nil)
		return
	}
	act := domain.SessionActivity{SessionKind: kind, SessionID: sessionID, Decisions: page.Decisions,
		Actions: []string{}, Resources: []string{}, Truncated: page.NextCursor != ""}
	actions, resources := map[string]bool{}, map[string]bool{}
	for _, d := range page.Decisions {
		if d.DecisionOutcome == domain.OutcomeGranted {
			act.Granted++
		} else {
			act.Denied++
		}
		actions[d.ActionType] = true
		if d.ResourceType != "" || d.ResourceID != "" {
			resources[strings.Trim(d.ResourceType+":"+d.ResourceID, ":")] = true
		}
	}
	act.Actions, act.Resources = sortedKeys(actions), sortedKeys(resources)
	if act.Decisions == nil {
		act.Decisions = []domain.AccessDecisionLog{}
	}
	writeJSON(w, http.StatusOK, act)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ── AU-02: break-glass approval ──────────────────────────────────────────────

// ApproveBreakGlassSession — POST …/break-glass-sessions/{session_id}/approve.
func (h *Handler) ApproveBreakGlassSession(w http.ResponseWriter, r *http.Request) {
	h.decideBreakGlass(w, r, true)
}

// RejectBreakGlassSession — POST …/break-glass-sessions/{session_id}/reject.
func (h *Handler) RejectBreakGlassSession(w http.ResponseWriter, r *http.Request) {
	h.decideBreakGlass(w, r, false)
}

// decideBreakGlass: GOV-12 ApproveBreakGlass. The approver holds
// iam.break_glass.approve, is neither the subject nor the requester, gives a
// reason (§16), and — to approve — has stepped up within the window, exactly
// as a JIT approver must (§14 "high-assurance step-up"). A rejection grants
// nothing and needs no step-up.
func (h *Handler) decideBreakGlass(w http.ResponseWriter, r *http.Request, approve bool) {
	approver, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if !h.requirePermission(w, r, approver, tenantScope, domain.PermissionBreakGlassApprove) {
		return
	}
	if _, ok := h.readCommand(w, &r); !ok {
		return
	}
	g, ok := h.governedStore(w, r)
	if !ok {
		return
	}
	sessionID := chi.URLParam(r, "session_id")
	bg, err := h.store.FindBreakGlassSessionByID(r.Context(), sessionID, tenantScope)
	if err != nil {
		h.writeGovernedErr(w, r, "decideBreakGlass", err)
		return
	}
	if approver == bg.PrincipalID || (bg.RequestedBy != nil && approver == *bg.RequestedBy) {
		h.writeGovernedErr(w, r, "decideBreakGlass", domain.ErrCheckerNotIndependent)
		return
	}

	var decided *domain.BreakGlassSession
	if approve {
		stepUp, ok := h.requireSessionStepUp(w, r, approver, tenantScope, "approve_break_glass")
		if !ok {
			return
		}
		decided, err = g.ApproveBreakGlassSession(r.Context(), sessionID, tenantScope, approver, stepUp)
	} else {
		decided, err = g.RejectBreakGlassSession(r.Context(), sessionID, tenantScope, approver)
	}
	if err != nil {
		h.writeGovernedErr(w, r, "decideBreakGlass", err)
		return
	}
	if decided.Status == domain.BreakGlassSessionStatusActive {
		// §14 "immediate security/governance alert to independent recipients"
		// — now at activation; the request raised its own alert.
		h.siem.Stream(r.Context(), tenantScope, "security.break_glass.started", siem.SeverityCritical,
			"Break-glass session "+decided.SessionID+" for "+decided.PrincipalID+" (incident "+decided.IncidentID+") activated by "+approver)
	}
	h.log.Warn("break-glass decision recorded", zap.String("session_id", sessionID), zap.Bool("approve", approve),
		zap.String("status", decided.Status), zap.String("decider", approver))
	writeJSON(w, http.StatusOK, decided)
}

// ── AU-17: SoD and ABAC rule publication ─────────────────────────────────────

// ApproveSoDRule — POST /v1/admin/sod-rules/{sod_rule_id}/approve.
func (h *Handler) ApproveSoDRule(w http.ResponseWriter, r *http.Request) { h.decideSoDRule(w, r, true) }

// RejectSoDRule — POST /v1/admin/sod-rules/{sod_rule_id}/reject.
func (h *Handler) RejectSoDRule(w http.ResponseWriter, r *http.Request) { h.decideSoDRule(w, r, false) }

func (h *Handler) decideSoDRule(w http.ResponseWriter, r *http.Request, approve bool) {
	decider, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	g, ok := h.governedStore(w, r)
	if !ok {
		return
	}
	ruleID := chi.URLParam(r, "sod_rule_id")
	rule, err := g.FindSoDRule(r.Context(), ruleID, tenantScope)
	if err != nil {
		h.writeGovernedErr(w, r, "decideSoDRule", err)
		return
	}
	platformWide := rule.TenantID == nil
	// Publishing a platform-wide rule takes the same platform-scope grant as
	// authoring one; a tenant's own rule takes iam.sod_rule.publish there.
	if platformWide {
		if !h.requirePlatformAction(w, r, decider, ActionSoDRuleManageGlobal) {
			return
		}
	} else if !h.requirePermission(w, r, decider, tenantScope, domain.PermissionSoDRulePublish) {
		return
	}
	if _, ok := h.readCommand(w, &r); !ok {
		return
	}
	if rule.ChangeRequestedBy != nil && *rule.ChangeRequestedBy == decider {
		h.writeGovernedErr(w, r, "decideSoDRule", domain.ErrCheckerNotIndependent)
		return
	}
	// The interested-party rule that guards asking for a retirement guards
	// approving one too: a holder of either action may not switch it off.
	if approve && !platformWide && rule.PendingChange != nil && *rule.PendingChange == domain.RuleChangeRetire &&
		h.refuseInterestedSoDRetire(w, r, decider, ruleID, tenantScope) {
		return
	}
	scope := tenantScope
	if platformWide {
		scope = ""
	}
	decided, err := g.DecideSoDRuleChange(r.Context(), ruleID, scope, platformWide, decider, approve)
	if err != nil {
		h.writeGovernedErr(w, r, "decideSoDRule", err)
		return
	}
	h.log.Info("sod rule change decided", zap.String("sod_rule_id", ruleID), zap.Bool("approve", approve),
		zap.Bool("active", decided.ActiveFlag), zap.String("publication_status", decided.PublicationStatus), zap.String("decider", decider))
	writeJSON(w, http.StatusOK, decided)
}

// ApproveABACRule — POST /v1/admin/abac-rules/{abac_rule_id}/approve.
func (h *Handler) ApproveABACRule(w http.ResponseWriter, r *http.Request) {
	h.decideABACRule(w, r, true)
}

// RejectABACRule — POST /v1/admin/abac-rules/{abac_rule_id}/reject.
func (h *Handler) RejectABACRule(w http.ResponseWriter, r *http.Request) {
	h.decideABACRule(w, r, false)
}

func (h *Handler) decideABACRule(w http.ResponseWriter, r *http.Request, approve bool) {
	decider, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	g, ok := h.governedStore(w, r)
	if !ok {
		return
	}
	ruleID := chi.URLParam(r, "abac_rule_id")
	rule, err := g.FindABACRule(r.Context(), ruleID, tenantScope)
	if err != nil {
		h.writeGovernedErr(w, r, "decideABACRule", err)
		return
	}
	platformWide := rule.TenantID == nil
	if platformWide {
		if !h.requirePlatformAction(w, r, decider, ActionABACRuleManageGlobal) {
			return
		}
	} else if !h.requirePermission(w, r, decider, tenantScope, domain.PermissionPolicyPublish) {
		return
	}
	if _, ok := h.readCommand(w, &r); !ok {
		return
	}
	if rule.ChangeRequestedBy != nil && *rule.ChangeRequestedBy == decider {
		h.writeGovernedErr(w, r, "decideABACRule", domain.ErrCheckerNotIndependent)
		return
	}
	scope := tenantScope
	if platformWide {
		scope = ""
	}
	decided, err := g.DecideABACRuleChange(r.Context(), ruleID, scope, platformWide, decider, approve)
	if err != nil {
		h.writeGovernedErr(w, r, "decideABACRule", err)
		return
	}
	h.log.Info("abac rule change decided", zap.String("abac_rule_id", ruleID), zap.Bool("approve", approve),
		zap.Bool("active", decided.ActiveFlag), zap.String("publication_status", decided.PublicationStatus), zap.String("decider", decider))
	writeJSON(w, http.StatusOK, decided)
}

// ── AU-03: authority limits ──────────────────────────────────────────────────

type authorityLimitRequest struct {
	PrincipalID   *string `json:"principal_id,omitempty"`
	RoleID        *string `json:"role_id,omitempty"`
	AuthorityType string  `json:"authority_type"`
	LegalEntityID *string `json:"legal_entity_id,omitempty"`
	BookID        *string `json:"book_id,omitempty"`
	OrgUnitID     *string `json:"org_unit_id,omitempty"`
	Currency      string  `json:"currency"`
	LowerLimit    string  `json:"lower_limit"`
	UpperLimit    string  `json:"upper_limit"`
	EffectiveFrom string  `json:"effective_from,omitempty"`
	EffectiveTo   string  `json:"effective_to,omitempty"`
	// §16 reason, recorded on the request.
	ReasonCode string `json:"reason_code"`
	Purpose    string `json:"purpose"`
	// ExpectedVersion of the limit a change replaces.
	ExpectedVersion int64 `json:"expected_version"`
}

func (req authorityLimitRequest) reason() string {
	return commandFields{ReasonCode: req.ReasonCode, Purpose: req.Purpose}.reason()
}

func nonEmpty(p *string) *string {
	if p == nil || strings.TrimSpace(*p) == "" {
		return nil
	}
	v := strings.TrimSpace(*p)
	return &v
}

// validateLimit checks what the store cannot express usefully: amounts that
// parse, a currency the conversion table knows (an unknown one would fail
// every evaluation closed), and an order to the dates. It writes the 400.
func validateLimit(w http.ResponseWriter, req *authorityLimitRequest) (from time.Time, to *time.Time, ok bool) {
	fail := func(field, msg string) (time.Time, *time.Time, bool) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": field, "message": msg})
		return time.Time{}, nil, false
	}
	req.Currency = strings.ToUpper(strings.TrimSpace(req.Currency))
	if req.Currency == "" {
		req.Currency = "GBP"
	}
	if _, known := StandardReferenceFXRates[req.Currency]; !known {
		return fail("currency", "currency must be one the platform converts: GBP, USD, EUR, CAD, AUD, JPY or CHF")
	}
	if strings.TrimSpace(req.LowerLimit) == "" {
		req.LowerLimit = "0"
	}
	lower, okL := new(big.Rat).SetString(strings.TrimSpace(req.LowerLimit))
	upper, okU := new(big.Rat).SetString(strings.TrimSpace(req.UpperLimit))
	switch {
	case !okL || lower.Sign() < 0:
		return fail("lower_limit", "lower_limit is a non-negative amount, e.g. 0 or 1000.00")
	case strings.TrimSpace(req.UpperLimit) == "":
		return fail("upper_limit", "upper_limit is required")
	case !okU || upper.Sign() <= 0:
		return fail("upper_limit", "upper_limit is a positive amount, e.g. 25000.00")
	case upper.Cmp(lower) < 0:
		return fail("upper_limit", "upper_limit must not be below lower_limit")
	}
	from = time.Now().UTC()
	if s := strings.TrimSpace(req.EffectiveFrom); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return fail("effective_from", "effective_from must be RFC3339, e.g. 2026-11-01T00:00:00Z")
		}
		from = t
	}
	if s := strings.TrimSpace(req.EffectiveTo); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return fail("effective_to", "effective_to must be RFC3339")
		}
		if !t.After(from) {
			return fail("effective_to", "effective_to must be after effective_from")
		}
		to = &t
	}
	return from, to, true
}

// ListAuthorityLimits — GET /v1/admin/authority-limits. Every version, newest
// first; ?status=, ?principal_id=, ?role_id=, ?authority_type= narrow it.
func (h *Handler) ListAuthorityLimits(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if !h.requirePermission(w, r, caller, tenantScope, "iam.policy.read") {
		return
	}
	q := r.URL.Query()
	limits, err := h.store.ListAuthorityLimits(r.Context(), tenantScope, q.Get("principal_id"), q.Get("role_id"), q.Get("authority_type"))
	if err != nil {
		writeError(w, r, http.StatusServiceUnavailable, "store_unavailable", "", nil)
		return
	}
	status := strings.ToUpper(strings.TrimSpace(q.Get("status")))
	out := make([]domain.AuthorityLimit, 0, len(limits))
	for _, l := range limits {
		if status == "" || l.Status == status || (status == "PENDING" && (l.Status == domain.AuthorityLimitPending || l.PendingAction != nil)) {
			out = append(out, l)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"authority_limits": out})
}

// CreateAuthorityLimit — POST /v1/admin/authority-limits. A request (202):
// the limit applies only once someone else approves it.
func (h *Handler) CreateAuthorityLimit(w http.ResponseWriter, r *http.Request) {
	maker, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if !h.requirePermission(w, r, maker, tenantScope, domain.PermissionAuthorityLimitManage) {
		return
	}
	var req authorityLimitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	req.PrincipalID, req.RoleID = nonEmpty(req.PrincipalID), nonEmpty(req.RoleID)
	switch {
	case req.PrincipalID == nil && req.RoleID == nil:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "principal_id",
			"message": "a limit applies to a person (principal_id) or to everyone holding a role (role_id)"})
		return
	case strings.TrimSpace(req.AuthorityType) == "":
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "authority_type",
			"message": "authority_type names what the limit governs, e.g. invoice_approval or payment_release"})
		return
	}
	if !h.applyReason(w, &r, req.reason()) {
		return
	}
	from, to, ok := validateLimit(w, &req)
	if !ok {
		return
	}
	if req.RoleID != nil {
		role, err := h.store.FindRoleByID(r.Context(), *req.RoleID)
		if err != nil || role == nil || role.TenantID != tenantScope {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": "role_id",
				"message": "role_id is not a role of this organisation"})
			return
		}
	}
	al, err := h.store.CreateAuthorityLimit(r.Context(), domain.CreateAuthorityLimitParams{
		TenantID: tenantScope, PrincipalID: req.PrincipalID, RoleID: req.RoleID,
		AuthorityType: strings.TrimSpace(req.AuthorityType), LegalEntityID: nonEmpty(req.LegalEntityID),
		BookID: nonEmpty(req.BookID), OrgUnitID: nonEmpty(req.OrgUnitID), Currency: req.Currency,
		LowerLimit: strings.TrimSpace(req.LowerLimit), UpperLimit: strings.TrimSpace(req.UpperLimit),
		EffectiveFrom: from, EffectiveTo: to, RequestedBy: maker, RequestReason: req.reason(),
	})
	if err != nil {
		h.writeGovernedErr(w, r, "CreateAuthorityLimit", err)
		return
	}
	writeJSON(w, http.StatusAccepted, al)
}

// ChangeAuthorityLimit — POST /v1/admin/authority-limits/{authority_limit_id}/change.
// Proposes new amounts or dates for a limit in force. The proposal is a new
// version naming the one it replaces; the old one keeps limiting until the
// change is approved. Who and where the limit applies to cannot change — that
// is a different limit.
func (h *Handler) ChangeAuthorityLimit(w http.ResponseWriter, r *http.Request) {
	maker, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if !h.requirePermission(w, r, maker, tenantScope, domain.PermissionAuthorityLimitManage) {
		return
	}
	var req authorityLimitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	if !h.applyReason(w, &r, req.reason()) {
		return
	}
	cur, err := h.store.FindAuthorityLimitByID(r.Context(), chi.URLParam(r, "authority_limit_id"), tenantScope)
	if err != nil {
		h.writeGovernedErr(w, r, "ChangeAuthorityLimit", err)
		return
	}
	if req.ExpectedVersion > 0 && req.ExpectedVersion != cur.Version {
		writeVersionConflict(w)
		return
	}
	if cur.Status != domain.AuthorityLimitActive {
		h.writeGovernedErr(w, r, "ChangeAuthorityLimit", domain.ErrInvalidTransition)
		return
	}
	if cur.PendingAction != nil {
		h.writeGovernedErr(w, r, "ChangeAuthorityLimit", domain.ErrChangePending)
		return
	}
	// Unset fields keep their current value.
	if req.Currency == "" {
		req.Currency = cur.Currency
	}
	if req.LowerLimit == "" {
		req.LowerLimit = cur.LowerLimit
	}
	if req.UpperLimit == "" {
		req.UpperLimit = cur.UpperLimit
	}
	if req.EffectiveTo == "" && cur.EffectiveTo != nil {
		req.EffectiveTo = cur.EffectiveTo.UTC().Format(time.RFC3339)
	}
	from, to, ok := validateLimit(w, &req)
	if !ok {
		return
	}
	al, err := h.store.CreateAuthorityLimit(r.Context(), domain.CreateAuthorityLimitParams{
		TenantID: tenantScope, PrincipalID: cur.PrincipalID, RoleID: cur.RoleID, AuthorityType: cur.AuthorityType,
		LegalEntityID: cur.LegalEntityID, BookID: cur.BookID, OrgUnitID: cur.OrgUnitID, Currency: req.Currency,
		LowerLimit: strings.TrimSpace(req.LowerLimit), UpperLimit: strings.TrimSpace(req.UpperLimit),
		EffectiveFrom: from, EffectiveTo: to, RequestedBy: maker, RequestReason: req.reason(), SupersedesID: cur.AuthorityLimitID,
	})
	if err != nil {
		h.writeGovernedErr(w, r, "ChangeAuthorityLimit", err)
		return
	}
	writeJSON(w, http.StatusAccepted, al)
}

// RetireAuthorityLimit — POST /v1/admin/authority-limits/{authority_limit_id}/retire.
// The limit keeps applying until the retirement is approved.
func (h *Handler) RetireAuthorityLimit(w http.ResponseWriter, r *http.Request) {
	maker, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if !h.requirePermission(w, r, maker, tenantScope, domain.PermissionAuthorityLimitManage) {
		return
	}
	expectedVersion, ok := h.readCommand(w, &r)
	if !ok {
		return
	}
	g, ok := h.governedStore(w, r)
	if !ok {
		return
	}
	_, _, reason := domain.AuditFrom(r.Context())
	al, err := g.RequestAuthorityLimitRetire(r.Context(), chi.URLParam(r, "authority_limit_id"), tenantScope, maker, reason, expectedVersion)
	if err != nil {
		h.writeGovernedErr(w, r, "RetireAuthorityLimit", err)
		return
	}
	writeJSON(w, http.StatusAccepted, al)
}

// ApproveAuthorityLimit — POST /v1/admin/authority-limits/{authority_limit_id}/approve.
func (h *Handler) ApproveAuthorityLimit(w http.ResponseWriter, r *http.Request) {
	h.decideAuthorityLimit(w, r, true)
}

// RejectAuthorityLimit — POST /v1/admin/authority-limits/{authority_limit_id}/reject.
func (h *Handler) RejectAuthorityLimit(w http.ResponseWriter, r *http.Request) {
	h.decideAuthorityLimit(w, r, false)
}

// decideAuthorityLimit: the checker holds iam.authority_limit.approve and is
// neither the person who asked for the change nor the person whose limit it
// is — approving your own limit is the self-interest §12 exists to stop.
func (h *Handler) decideAuthorityLimit(w http.ResponseWriter, r *http.Request, approve bool) {
	checker, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if !h.requirePermission(w, r, checker, tenantScope, domain.PermissionAuthorityLimitApprove) {
		return
	}
	if _, ok := h.readCommand(w, &r); !ok {
		return
	}
	g, ok := h.governedStore(w, r)
	if !ok {
		return
	}
	limitID := chi.URLParam(r, "authority_limit_id")
	cur, err := h.store.FindAuthorityLimitByID(r.Context(), limitID, tenantScope)
	if err != nil {
		h.writeGovernedErr(w, r, "decideAuthorityLimit", err)
		return
	}
	if (cur.RequestedBy != nil && *cur.RequestedBy == checker) || (cur.PrincipalID != nil && *cur.PrincipalID == checker) {
		h.writeGovernedErr(w, r, "decideAuthorityLimit", domain.ErrCheckerNotIndependent)
		return
	}
	decided, err := g.DecideAuthorityLimit(r.Context(), limitID, tenantScope, checker, approve)
	if err != nil {
		h.writeGovernedErr(w, r, "decideAuthorityLimit", err)
		return
	}
	h.log.Info("authority limit decision recorded", zap.String("authority_limit_id", limitID),
		zap.Bool("approve", approve), zap.String("status", decided.Status), zap.String("checker", checker))
	writeJSON(w, http.StatusOK, decided)
}
