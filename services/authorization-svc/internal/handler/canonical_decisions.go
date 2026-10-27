package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/abac"
	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/siem"
)

const (
	CanonicalDecisionsPath   = "/internal/authorization/decisions"
	DefaultPolicySetVersion  = "2026.08.24.4"
	DefaultCoolingWindowSecs = 86400 // 24 hours
	DefaultMaxAuthnAgeSecs   = 300   // 5 minutes
)

type evalContext struct {
	PrincipalID         string
	PrincipalType       string
	TenantID            string
	LegalEntityID       string
	BookID              string
	OrgUnitID           string
	ResourceType        string
	ResourceID          string
	ActionType          string
	ResourceOwnerID     string
	Attributes          map[string]string
	Environment         domain.EnvironmentContext
	PrivilegedSessionID string
	BreakGlassSessionID string
	SupportSessionID    string
	CorrelationID       string
	InitiatingSubjectID string
	ResourceVersion     string

	// SkipAvailable: evaluate the one requested action only. /v1/authorize and
	// the per-action evaluations computeAvailableActions makes set it.
	SkipAvailable bool
}

// boundWorkloadFinder is the store capability that says whether a principal
// is a bound workload anywhere (N-09). Optional: without it only a declared
// workload is evaluated as one.
type boundWorkloadFinder interface {
	IsBoundWorkload(ctx context.Context, principalID string) (bool, error)
}

type evalResult struct {
	Decision         string // PERMIT, DENY, STEP_UP, REQUIRE_APPROVAL (ZS-IAM-001 §7 stage 9)
	Outcome          string // GRANTED, DENIED, STEP_UP — the /v1/authorize decision_outcome vocabulary
	Basis            string
	Reason           string
	ReasonCodes      []string
	MatchedGrants    []string
	NegativeControls []string
	Obligations      []string
	StepUp           *domain.StepUpRequirement
	AvailableActions []string
	DeniedActions    []domain.DeniedActionInfo
	StepUpActions    []domain.StepUpActionInfo
	AllHeldActions   []string
}

// evaluateCore is the central authorization evaluation engine shared by
// Authorize (/v1/authorize), Canonical Decisions (POST /internal/authorization/decisions),
// and Available Actions (GET /v1/{resource}/{id}/available-actions).
//
// ONE engine for all three, deliberately. /v1/authorize used to carry its own
// inline copy of the pipeline and the two had drifted (session-store errors
// were a 503 in one and silently ignored in the other), and the available
// actions were computed without SoD, ABAC or authority limits — so the UI
// could be offered actions /v1/authorize then refused (GOV-03 negative path
// #4: "AvailableActions equals backend-authorized actions").
func (h *Handler) evaluateCore(ctx context.Context, in evalContext, evaluationEntityID string) (*evalResult, error) {
	res, err := h.evaluateDecision(ctx, in, evaluationEntityID)
	if err != nil {
		return nil, err
	}
	if !in.SkipAvailable {
		if err := h.computeAvailableActions(ctx, in, evaluationEntityID, res); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// evaluateDecision runs the decision pipeline for in.ActionType alone.
func (h *Handler) evaluateDecision(ctx context.Context, in evalContext, evaluationEntityID string) (*evalResult, error) {
	res := &evalResult{
		MatchedGrants:    make([]string, 0),
		NegativeControls: make([]string, 0),
		Obligations:      make([]string, 0),
		ReasonCodes:      make([]string, 0),
		AvailableActions: make([]string, 0),
		DeniedActions:    make([]domain.DeniedActionInfo, 0),
		StepUpActions:    make([]domain.StepUpActionInfo, 0),
		AllHeldActions:   make([]string, 0),
	}

	// ── Layer 0.0: Workload Identity Validation (ZS-IAM-001 §16, Scenarios A18 & A19) ─
	isWorkload := strings.EqualFold(in.PrincipalType, "WORKLOAD") ||
		(in.Attributes != nil && strings.EqualFold(in.Attributes["principal_type"], "WORKLOAD")) ||
		(in.Attributes != nil && in.Attributes["workload_id"] != "")
	// Not opt-in (Governance audit N-09): a principal that IS a bound
	// workload in some tenant is evaluated as one whether or not the request
	// says so. Before, omitting principal_type stepped a workload outside its
	// binding and into whatever RBAC grants its id held.
	if !isWorkload {
		if f, ok := h.store.(boundWorkloadFinder); ok {
			bound, err := f.IsBoundWorkload(ctx, in.PrincipalID)
			if err != nil {
				return nil, fmt.Errorf("workload lookup: %w", err)
			}
			isWorkload = bound
		}
	}

	if isWorkload {
		workloadID := in.PrincipalID
		if in.Attributes != nil && in.Attributes["workload_id"] != "" {
			workloadID = in.Attributes["workload_id"]
		}

		binding, err := h.store.FindWorkloadBinding(ctx, workloadID, in.TenantID)
		if err != nil || binding == nil {
			// Scenario A19: Workload attempts to invent or widen tenant context outside its trusted binding.
			res.Decision = domain.CanonicalDecisionDeny
			res.Outcome = domain.OutcomeDenied
			res.Basis = "workload:tenant_context_unbound"
			res.Reason = "UNBOUND_TENANT_CONTEXT"
			res.ReasonCodes = []string{"UNBOUND_TENANT_CONTEXT"}
			res.NegativeControls = []string{"workload:tenant_context_unbound"}
			// High-severity SIEM/security event
			h.siem.Stream(ctx, in.TenantID, "security.workload.unbound_tenant", siem.SeverityHigh,
				fmt.Sprintf("Workload %s attempted access with unbound tenant %s", workloadID, in.TenantID))
			return res, nil
		}

		if !binding.ActiveFlag {
			res.Decision = domain.CanonicalDecisionDeny
			res.Outcome = domain.OutcomeDenied
			res.Basis = "workload:inactive_binding"
			res.Reason = "WORKLOAD_BINDING_INACTIVE"
			res.ReasonCodes = []string{"WORKLOAD_BINDING_INACTIVE"}
			res.NegativeControls = []string{"workload:inactive_binding"}
			return res, nil
		}

		aud := in.Environment.Audience
		if aud == "" && in.Attributes != nil {
			aud = in.Attributes["audience"]
			if aud == "" {
				aud = in.Attributes["aud"]
			}
		}

		if binding.AllowedAudience != "" && binding.AllowedAudience != "*" {
			if aud == "" || aud != binding.AllowedAudience {
				// Scenario A18: Workload presents a valid credential but the audience is incorrect.
				res.Decision = domain.CanonicalDecisionDeny
				res.Outcome = domain.OutcomeDenied
				res.Basis = "workload:audience_mismatch"
				res.Reason = "TOKEN_AUDIENCE_MISMATCH"
				res.ReasonCodes = []string{"TOKEN_AUDIENCE_MISMATCH"}
				res.NegativeControls = []string{"workload:audience_mismatch"}
				return res, nil
			}
		}

		// The binding's actions bound what the workload may do in this
		// tenant (N-09: allowed_actions was stored and never read). An
		// action outside them is denied whatever RBAC would say; one inside
		// still needs its grant below — the binding narrows, never grants.
		if !contains(binding.AllowedActions, in.ActionType) {
			res.Decision = domain.CanonicalDecisionDeny
			res.Outcome = domain.OutcomeDenied
			res.Basis = "workload:action_not_bound"
			res.Reason = "WORKLOAD_ACTION_NOT_BOUND"
			res.ReasonCodes = []string{"WORKLOAD_ACTION_NOT_BOUND"}
			res.NegativeControls = []string{"workload:action_not_bound"}
			return res, nil
		}
	}

	// ── Layer 0: Principal Status ─────────────────────────────────────────────
	principalStatus, err := h.store.FindPrincipalStatus(ctx, in.PrincipalID, in.TenantID)
	if err != nil {
		return nil, fmt.Errorf("principal status lookup: %w", err)
	}
	if principalStatus != domain.PrincipalStatusActive {
		res.Decision = domain.CanonicalDecisionDeny
		res.Outcome = domain.OutcomeDenied
		res.Basis = principalStatusBasisPrefix + principalStatus
		res.Reason = "Principal account is " + principalStatus
		res.ReasonCodes = []string{"PRINCIPAL_SUSPENDED"}
		res.NegativeControls = []string{principalStatusBasisPrefix + principalStatus}
		return res, nil
	}

	// ── Layer 0.1: Legal entity standing (Doc 03 §8.3 entity.scope.updated) ──
	// A SUSPENDED or DISSOLVED entity can be acted in by nobody: the negative
	// control layer overrides grants (ZS-IAM-001 §7 stage 6). DORMANT is not a
	// denial — a dormant company still files dormant accounts — so it permits
	// with an obligation the PEP must honour. No projected status means the
	// registry has said nothing about this entity, which changes nothing.
	entityStatus := ""
	if !h.isPlatformScopeEntity(evaluationEntityID) {
		entityStatus, err = h.store.FindEntityStatus(ctx, evaluationEntityID, in.TenantID)
		if err != nil {
			return nil, fmt.Errorf("entity status lookup: %w", err)
		}
	}
	if entityStatus == domain.EntityStatusSuspended || entityStatus == domain.EntityStatusDissolved {
		res.Decision = domain.CanonicalDecisionDeny
		res.Outcome = domain.OutcomeDenied
		res.Basis = entityStatusBasisPrefix + entityStatus
		res.Reason = "Legal entity is " + entityStatus
		res.ReasonCodes = []string{"ENTITY_NOT_OPERATIONAL"}
		res.NegativeControls = []string{entityStatusBasisPrefix + entityStatus}
		return res, nil
	}

	// ── Layer 1: RBAC Granted Actions ─────────────────────────────────────────
	rbacActions, rbacBasis, err := h.store.FindGrantedActionsScoped(ctx, in.PrincipalID, evaluationEntityID, in.TenantID, in.BookID, in.OrgUnitID)
	if err != nil {
		return nil, fmt.Errorf("rbac lookup: %w", err)
	}

	granted := contains(rbacActions, in.ActionType)
	basis := rbacBasis
	allHeldActions := append([]string{}, rbacActions...)
	if granted {
		res.MatchedGrants = append(res.MatchedGrants, rbacBasis)
	}

	// ── Layer 2: Delegated Authority ──────────────────────────────────────────
	delegatedActions, delegatedBasis, err := h.store.FindDelegatedActionsScoped(ctx, in.PrincipalID, evaluationEntityID, in.TenantID, in.BookID, in.OrgUnitID)
	if err != nil {
		return nil, fmt.Errorf("delegation lookup: %w", err)
	}
	allHeldActions = append(allHeldActions, delegatedActions...)
	grantedViaDelegation := false
	if !granted && contains(delegatedActions, in.ActionType) {
		granted = true
		basis = delegatedBasis
		grantedViaDelegation = true
		res.MatchedGrants = append(res.MatchedGrants, delegatedBasis)
	}

	// ── Layer 3: Privileged Access Management (JIT Elevation) ─────────────────
	if in.PrivilegedSessionID != "" {
		ps, err := h.store.FindPrivilegedSessionByID(ctx, in.PrivilegedSessionID, in.TenantID)
		if err != nil && !errors.Is(err, domain.ErrPrivilegedSessionNotFound) {
			return nil, fmt.Errorf("privileged session lookup: %w", err)
		}
		if err == nil && ps != nil {
			if ps.PrincipalID == in.PrincipalID && ps.Status == domain.PrivilegedSessionStatusActive && time.Now().UTC().Before(ps.ExpiresAt) {
				allHeldActions = append(allHeldActions, ps.RequestedActions...)
				if !granted && contains(ps.RequestedActions, in.ActionType) {
					granted = true
					basis = fmt.Sprintf("pam:session=%s:ticket=%s", ps.SessionID, ps.TicketRef)
					res.MatchedGrants = append(res.MatchedGrants, basis)
				}
			}
		}
	}

	// ── Layer 3.1: Break-Glass Emergency Session ──────────────────────────────
	if in.BreakGlassSessionID != "" {
		bg, err := h.store.FindBreakGlassSessionByID(ctx, in.BreakGlassSessionID, in.TenantID)
		if err != nil && !errors.Is(err, domain.ErrBreakGlassSessionNotFound) {
			return nil, fmt.Errorf("break-glass session lookup: %w", err)
		}
		if err == nil && bg != nil {
			if bg.PrincipalID == in.PrincipalID && bg.IncidentID != "" && bg.Status == domain.BreakGlassSessionStatusActive && time.Now().UTC().Before(bg.ExpiresAt) {
				allHeldActions = append(allHeldActions, bg.RequestedActions...)
				if !granted && contains(bg.RequestedActions, in.ActionType) {
					granted = true
					basis = fmt.Sprintf("break_glass:session=%s:incident=%s", bg.SessionID, bg.IncidentID)
					res.MatchedGrants = append(res.MatchedGrants, basis)
				}
			}
		}
	}

	// ── Layer 3.2: Support Session ────────────────────────────────────────────
	if in.SupportSessionID != "" {
		ss, err := h.store.FindSupportSessionByID(ctx, in.SupportSessionID, in.TenantID)
		if err != nil && !errors.Is(err, domain.ErrSupportSessionNotFound) {
			return nil, fmt.Errorf("support session lookup: %w", err)
		}
		if err == nil && ss != nil {
			if ss.SupportOperatorID == in.PrincipalID && ss.Status == domain.SupportSessionStatusActive && time.Now().UTC().Before(ss.ExpiresAt) {
				// §15: consent stated, export off by default, read-only by
				// default, and writes only when named. "Write" is anything that
				// is not a read verb — so a legacy name like PAYMENT_RELEASE is
				// a write, which the old suffix list (.create, .post, ...) missed.
				if !ss.TenantConsentObtained {
					res.Decision = domain.CanonicalDecisionDeny
					res.Outcome = domain.OutcomeDenied
					res.Basis = "support:consent_not_obtained"
					res.Reason = "Support session has no tenant consent or support agreement on record"
					res.ReasonCodes = []string{"SUPPORT_CONSENT_NOT_OBTAINED"}
					res.NegativeControls = []string{"support:consent_not_obtained"}
					return res, nil
				}
				// Whole tokens (N-16): "export" is in SIEM_EXPORTER_READ, which
				// names an exporter and moves no data.
				exporting := isExportAction(in.ActionType)
				isMutationAction := !domain.IsReadAction(in.ActionType)

				if exporting && !ss.AllowBulkExport && !contains(ss.AllowedActions, in.ActionType) {
					res.Decision = domain.CanonicalDecisionDeny
					res.Outcome = domain.OutcomeDenied
					res.Basis = "support:bulk_export_prohibited"
					res.Reason = "Bulk export prohibited in support session"
					res.ReasonCodes = []string{"SUPPORT_BULK_EXPORT_PROHIBITED"}
					res.NegativeControls = []string{"support:bulk_export_prohibited"}
					return res, nil
				}

				if isMutationAction && (ss.ReadOnly || !contains(ss.AllowedActions, in.ActionType)) {
					res.Decision = domain.CanonicalDecisionDeny
					res.Outcome = domain.OutcomeDenied
					res.Basis = "support:read_only_session"
					res.Reason = "Write operation prohibited in read-only support session"
					res.ReasonCodes = []string{"SUPPORT_READ_ONLY_VIOLATION"}
					res.NegativeControls = []string{"support:read_only_session"}
					return res, nil
				}

				if !isMutationAction || contains(ss.AllowedActions, in.ActionType) {
					granted = true
					basis = fmt.Sprintf("support:session=%s:operator=%s:ticket=%s", ss.SessionID, ss.SupportOperatorID, ss.TicketRef)
					res.MatchedGrants = append(res.MatchedGrants, basis)
					allHeldActions = append(allHeldActions, in.ActionType)
				}
			}
		}
	}

	res.AllHeldActions = deduplicate(allHeldActions)

	if !granted {
		res.Decision = domain.CanonicalDecisionDeny
		res.Outcome = domain.OutcomeDenied
		res.Basis = "no_grant"
		res.Reason = "No matching permission grant found for principal"
		res.ReasonCodes = []string{"NO_MATCHING_GRANT"}
		res.NegativeControls = []string{"no_grant"}

		// Compute available actions from other held actions
		return res, nil
	}

	// ── Layer 4: Static SoD Conflicts ─────────────────────────────────────────
	// A conflict covered by an active GOV-04 compensating-control exception
	// for this principal and pair does not deny; the next conflicting pair, if
	// any, is then checked, so an exception for one pair never hides another.
	others := removeAll(allHeldActions, in.ActionType)
	var excepted []*domain.SoDException
	conflicting, hasConflict := "", false
	for {
		var err error
		conflicting, hasConflict, err = h.store.CheckSoDConflict(ctx, others, in.ActionType, in.TenantID)
		if err != nil {
			return nil, fmt.Errorf("sod check: %w", err)
		}
		if !hasConflict {
			break
		}
		finder, ok := h.store.(sodExceptionFinder)
		if !ok {
			break
		}
		exc, err := finder.FindActiveSoDException(ctx, in.PrincipalID, in.TenantID, in.ActionType, conflicting)
		if err != nil {
			return nil, fmt.Errorf("sod exception lookup: %w", err)
		}
		if exc == nil {
			break
		}
		excepted = append(excepted, exc)
		others = removeAll(others, conflicting)
	}
	if hasConflict {
		res.Decision = domain.CanonicalDecisionDeny
		res.Outcome = domain.OutcomeDenied
		res.Basis = "sod:conflict_with=" + conflicting
		res.Reason = fmt.Sprintf("Static Segregation of Duties conflict with held action %s", conflicting)
		res.ReasonCodes = []string{"SOD_STATIC_CONFLICT"}
		res.NegativeControls = []string{"sod:conflict_with=" + conflicting}

		return res, nil
	}

	// ── Layer 4.1: Dynamic SoD — Own-Object / Preparer Conflict (§10.2) ───────
	isPreparer := (in.ResourceOwnerID != "" && in.ResourceOwnerID == in.PrincipalID) ||
		(in.Attributes != nil && (in.Attributes["preparer_id"] == in.PrincipalID || in.Attributes["created_by"] == in.PrincipalID))
	if isPreparer {
		isOwnObjectForbidden, err := h.store.CheckOwnObjectSoD(ctx, in.ActionType, in.TenantID)
		if err != nil {
			return nil, fmt.Errorf("own-object sod check: %w", err)
		}
		if isOwnObjectForbidden {
			res.Decision = domain.CanonicalDecisionDeny
			res.Outcome = domain.OutcomeDenied
			res.Basis = "sod:own_object_forbidden"
			res.Reason = "Preparer cannot approve or release their own prepared object (Dynamic SoD §10.2)"
			res.ReasonCodes = []string{"SOD_DYNAMIC_CONFLICT", "OWN_OBJECT_FORBIDDEN"}
			res.NegativeControls = []string{"dynamic_sod:own_object_forbidden"}

			return res, nil
		}
	}

	// ── Layer 4.1b: Dynamic SoD with unknown participation (GOV-04) ─────────
	// An own-object rule exists for this action but the caller did not say who
	// prepared the object. GOV-04: "Unknown participation/history on material
	// action => block or require independent review; never silently assume no
	// conflict." Before this the check was simply skipped, so a PEP that omitted
	// the preparer passed every own-object rule.
	ownerKnown := in.ResourceOwnerID != "" ||
		(in.Attributes != nil && (in.Attributes["preparer_id"] != "" || in.Attributes["created_by"] != ""))
	if !ownerKnown {
		ownObjectRuled, err := h.store.CheckOwnObjectSoD(ctx, in.ActionType, in.TenantID)
		if err != nil {
			return nil, fmt.Errorf("own-object sod check: %w", err)
		}
		if ownObjectRuled {
			res.Decision = domain.CanonicalDecisionRequireApproval
			res.Outcome = domain.OutcomeDenied
			res.Basis = "sod:participation_unknown"
			res.Reason = "An own-object SoD rule applies and the object's preparer was not supplied"
			res.ReasonCodes = []string{"SOD_PARTICIPATION_UNKNOWN", "APPROVAL_REQUIRED"}
			res.NegativeControls = []string{"dynamic_sod:participation_unknown"}
			res.Obligations = []string{"REQUIRE_INDEPENDENT_REVIEW"}
			return res, nil
		}
	}

	// ── Layer 4.2: Dynamic SoD — Supplier Bank Change Proximity (Scenario A06, §10.2)
	// Predicate: subject changed supplier bank details within configured cooling window AND attempts release
	// Result: DENY or require independent high-assurance control per policy.
	if isReleaseAction(in.ActionType) && in.Attributes != nil {
		bankChangedBy := in.Attributes["supplier_bank_changed_by"]
		if bankChangedBy == "" {
			bankChangedBy = in.Attributes["bank_account_changed_by"]
		}
		coolingViolation := in.Attributes["cooling_window_violation"] == "true" ||
			in.Attributes["supplier_bank_cooling_window_active"] == "true" ||
			in.Attributes["cooling_window_conflict"] == "true" ||
			in.Attributes["supplier_bank_cooling_active"] == "true" ||
			in.Attributes["cooling_window_active"] == "true"

		if !coolingViolation && in.Attributes["supplier_bank_changed_at"] != "" {
			coolingViolation = checkCoolingWindowActive(in.Attributes["supplier_bank_changed_at"])
		}

		if (bankChangedBy == in.PrincipalID && coolingViolation) || in.Attributes["cooling_window_conflict"] == "true" {
			res.Decision = domain.CanonicalDecisionDeny
			res.Outcome = domain.OutcomeDenied
			res.Basis = "sod:cooling_window_conflict"
			res.Reason = "Payment releaser also changed supplier bank account within cooling window (Scenario A06)"
			res.ReasonCodes = []string{"SOD_DYNAMIC_CONFLICT", "COOLING_WINDOW_CONFLICT"}
			res.NegativeControls = []string{"dynamic_sod:cooling_window_conflict"}

			return res, nil
		}
	}

	// ── Layer 4.3: Dynamic SoD — Requestor Self-Approval (§10.2) ──────────────
	if isApprovalAction(in.ActionType) && in.Attributes != nil {
		requestorID := in.Attributes["requestor_id"]
		if requestorID == "" {
			requestorID = in.Attributes["expense.requestor_id"]
		}
		if requestorID != "" && requestorID == in.PrincipalID {
			res.Decision = domain.CanonicalDecisionDeny
			res.Outcome = domain.OutcomeDenied
			res.Basis = "sod:requestor_self_approval"
			res.Reason = "Requestor cannot approve own request (Dynamic SoD §10.2)"
			res.ReasonCodes = []string{"SOD_DYNAMIC_CONFLICT", "REQUESTOR_SELF_APPROVAL"}
			res.NegativeControls = []string{"dynamic_sod:requestor_self_approval"}

			return res, nil
		}
	}

	// ── Layer 4.4: Dynamic SoD — Own Access Elevation (§10.2) ─────────────────
	if isGrantAction(in.ActionType) && in.Attributes != nil {
		targetSubjectID := in.Attributes["target_subject_id"]
		if targetSubjectID == "" {
			targetSubjectID = in.Attributes["target_principal_id"]
		}
		if targetSubjectID != "" && targetSubjectID == in.PrincipalID {
			res.Decision = domain.CanonicalDecisionDeny
			res.Outcome = domain.OutcomeDenied
			res.Basis = "sod:own_access_elevation"
			res.Reason = "Principal cannot grant access elevation to self (Dynamic SoD §10.2)"
			res.ReasonCodes = []string{"SOD_DYNAMIC_CONFLICT", "OWN_ACCESS_ELEVATION"}
			res.NegativeControls = []string{"dynamic_sod:own_access_elevation"}

			return res, nil
		}
	}

	// ── Layer 4.5: Dynamic SoD — Prior Rejected Reviewer (§10.2 Pattern 5) ───
	if isApprovalAction(in.ActionType) && in.Attributes != nil {
		priorRejectedBy := in.Attributes["prior_rejected_by"]
		if priorRejectedBy == "" {
			priorRejectedBy = in.Attributes["rejected_by"]
		}
		isResubmission := in.Attributes["is_resubmission"] == "true" || in.Attributes["resubmitted"] == "true" || priorRejectedBy != ""
		if priorRejectedBy != "" && priorRejectedBy == in.PrincipalID && isResubmission {
			res.Decision = domain.CanonicalDecisionDeny
			res.Outcome = domain.OutcomeDenied
			res.Basis = "sod:prior_rejected_reviewer"
			res.Reason = "Same reviewer cannot approve after prior rejection and material resubmission (Dynamic SoD §10.2)"
			res.ReasonCodes = []string{"SOD_DYNAMIC_CONFLICT", "PRIOR_REJECTED_REVIEWER_CONFLICT"}
			res.NegativeControls = []string{"dynamic_sod:prior_rejected_reviewer"}

			return res, nil
		}
	}

	// ── Layer 4.6: Dynamic SoD — Related-Party Conflict (§10.2 Pattern 6) ────
	if in.Attributes != nil {
		relatedPartySubject := in.Attributes["related_party_subject_id"]
		if relatedPartySubject == "" {
			relatedPartySubject = in.Attributes["vendor_related_party_subject_id"]
		}
		isRelatedParty := in.Attributes["is_related_party"] == "true" || in.Attributes["related_party_conflict"] == "true" ||
			(relatedPartySubject != "" && relatedPartySubject == in.PrincipalID)
		if isRelatedParty {
			res.Decision = domain.CanonicalDecisionDeny
			res.Outcome = domain.OutcomeDenied
			res.Basis = "sod:related_party_conflict"
			res.Reason = "Subject has a declared related-party conflict of interest (Dynamic SoD §10.2)"
			res.ReasonCodes = []string{"SOD_DYNAMIC_CONFLICT", "RELATED_PARTY_CONFLICT"}
			res.NegativeControls = []string{"dynamic_sod:related_party_conflict"}

			return res, nil
		}
	}

	// ── Layer 5: ABAC Attribute Rules ─────────────────────────────────────────
	rules, err := h.store.FindABACRules(ctx, in.ActionType, in.TenantID)
	if err != nil {
		return nil, fmt.Errorf("abac lookup: %w", err)
	}
	if denial, denied := abac.Evaluate(rules, in.Attributes); denied {
		res.Decision = domain.CanonicalDecisionDeny
		res.Outcome = domain.OutcomeDenied
		res.Basis = denial.Basis()
		res.Reason = "ABAC rule condition failed: " + denial.Basis()
		res.ReasonCodes = []string{"ABAC_RULE_VIOLATION"}
		res.NegativeControls = []string{denial.Basis()}

		return res, nil
	}

	// ── Layer 5.9: the ceiling of the delegation the grant rests on (ORG-06) ──
	if grantedViaDelegation {
		ceilingDenied, ceilingBasis, ceilingReason, err := h.evaluateDelegationCeiling(ctx, in, evaluationEntityID)
		if err != nil {
			return nil, fmt.Errorf("delegation ceiling evaluation: %w", err)
		}
		if ceilingDenied {
			res.Decision = domain.CanonicalDecisionDeny
			res.Outcome = domain.OutcomeDenied
			res.Basis = ceilingBasis
			res.Reason = ceilingReason
			res.ReasonCodes = []string{"DELEGATION_LIMIT_EXCEEDED"}
			res.NegativeControls = []string{ceilingBasis}
			return res, nil
		}
	}

	// ── Layer 6.0: Monetary Authority Limits & FX Basis (Phase 3.6 & 3.7, Scenario A09) ──
	limitDenied, limitBasis, limitReason, limitReasonCodes, limitNC, err := h.evaluateAuthorityLimits(ctx, in, evaluationEntityID)
	if err != nil {
		return nil, fmt.Errorf("authority limit evaluation: %w", err)
	}
	if limitDenied {
		res.Decision = domain.CanonicalDecisionDeny
		res.Outcome = domain.OutcomeDenied
		res.Basis = limitBasis
		res.Reason = limitReason
		res.ReasonCodes = limitReasonCodes
		res.NegativeControls = limitNC

		return res, nil
	}

	// ── Layer 6.1: Quorum / Dual Approval (Phase 3.8) ─────────────────────────
	quorumDenied, qBasis, qReason, qReasonCodes, qNC := h.evaluateQuorum(in)
	if quorumDenied {
		res.Decision = domain.CanonicalDecisionDeny
		res.Outcome = domain.OutcomeDenied
		res.Basis = qBasis
		res.Reason = qReason
		res.ReasonCodes = qReasonCodes
		res.NegativeControls = qNC

		return res, nil
	}

	// ── Layer 6.2: Execution-Time Fact-Hash Revalidation (Phase 3.9, Scenario A10) ──
	revalDenied, rBasis, rReason, rReasonCodes, rNC := h.evaluateExecutionRevalidation(in)
	if revalDenied {
		res.Decision = domain.CanonicalDecisionDeny
		res.Outcome = domain.OutcomeDenied
		res.Basis = rBasis
		res.Reason = rReason
		res.ReasonCodes = rReasonCodes
		res.NegativeControls = rNC

		return res, nil
	}

	// ── Layer 7: Stage 7 Assurance / Step-Up Handling (Scenario A26, §7, §8.2) ─
	// Scenario A26: Session authentication too old for period reopen -> STEP_UP
	if h.isStepUpRequired(in.ActionType, in.Attributes, in.Environment) {
		res.Decision = domain.CanonicalDecisionStepUp
		res.Outcome = domain.OutcomeStepUp
		res.Basis = "assurance:recent_authn_required"
		res.Reason = "Session authentication too old for period reopen (Scenario A26)"
		res.ReasonCodes = []string{"STEP_UP_REQUIRED", "AUTHENTICATION_ASSURANCE_REQUIRED"}
		res.Obligations = []string{"REQUIRE_RECENT_AUTHN"}
		res.StepUp = &domain.StepUpRequirement{
			RequiredAssurance:  "PHISHING_RESISTANT",
			MaxAuthnAgeSeconds: h.stepUpPolicy().MaxAuthnAgeSeconds,
			Reason:             "PERIOD_REOPEN_REQUIRES_RECENT_AUTHN",
		}

		return res, nil
	}

	// ── Layer 8: Domain Guards — Resource Lifecycle State (ZS-STATE-001) ──────
	if in.Attributes != nil {
		status := getResourceLifecycleStatus(in.Attributes)
		if isTerminalLifecycleState(status) && isMutationOrApprovalAction(in.ActionType) {
			res.Decision = domain.CanonicalDecisionDeny
			res.Outcome = domain.OutcomeDenied
			res.Basis = "state:terminal_status=" + status
			res.Reason = fmt.Sprintf("Action not allowed on resource in terminal status %s (ZS-STATE-001)", status)
			res.ReasonCodes = []string{"RESOURCE_STATE_CONFLICT"}
			res.NegativeControls = []string{"state:terminal_status=" + status}

			return res, nil
		}
	}

	// ── Final ALLOW Decision ──────────────────────────────────────────────────
	res.Decision = domain.CanonicalDecisionPermit
	res.Outcome = domain.OutcomeGranted
	res.Basis = basis
	res.Reason = "Access permitted per active policy and scope"
	res.ReasonCodes = []string{"PERMISSION_GRANTED", "SOD_CLEAR", "AUTHORITY_WITHIN_LIMIT"}
	if grantedViaDelegation {
		res.ReasonCodes = append(res.ReasonCodes, "DELEGATED_AUTHORITY")
	}
	if entityStatus == domain.EntityStatusDormant {
		res.ReasonCodes = append(res.ReasonCodes, "ENTITY_DORMANT")
		res.Obligations = append(res.Obligations, "ENTITY_DORMANT_REVIEW")
	}
	if len(excepted) > 0 {
		// The conflict was not clear: it was excepted. Say so, and hand the PEP
		// the compensating control it must apply.
		res.ReasonCodes[1] = "SOD_EXCEPTION_APPLIED"
		for _, e := range excepted {
			res.Obligations = append(res.Obligations, "COMPENSATING_CONTROL:"+e.CompensatingControl)
			res.MatchedGrants = append(res.MatchedGrants, "sod_exception:"+e.SoDExceptionID)
		}
	}

	// High risk obligation per §8.2
	if in.Environment.Risk == "HIGH" || strings.ToLower(in.Attributes["risk"]) == "high" {
		res.Obligations = append(res.Obligations, "RECORD_HIGH_RISK_EVIDENCE")
	}

	return res, nil
}

// computeAvailableActions populates available_actions, denied_actions and
// step_up_actions for the subject in this resource/scope context.
//
// Every held action is classified by the SAME pipeline /v1/authorize runs —
// static and dynamic SoD, ABAC, authority limits, assurance, domain guards —
// not by a partial re-implementation of some of them. That is what makes the
// list equal to what the backend would authorize (GOV-03 negative path #4).
func (h *Handler) computeAvailableActions(ctx context.Context, in evalContext, evaluationEntityID string, res *evalResult) error {
	for _, act := range deduplicate(res.AllHeldActions) {
		r := res
		if act != in.ActionType {
			sub := in
			sub.ActionType = act
			sub.SkipAvailable = true
			var err error
			r, err = h.evaluateDecision(ctx, sub, evaluationEntityID)
			if err != nil {
				return err
			}
		}
		switch r.Decision {
		case domain.CanonicalDecisionPermit:
			res.AvailableActions = append(res.AvailableActions, act)
		case domain.CanonicalDecisionStepUp:
			res.StepUpActions = append(res.StepUpActions, domain.StepUpActionInfo{
				Action:             act,
				RequiredAssurance:  "PHISHING_RESISTANT",
				MaxAuthnAgeSeconds: h.stepUpPolicy().MaxAuthnAgeSeconds,
				Reason:             r.Reason,
			})
		default:
			res.DeniedActions = append(res.DeniedActions, domain.DeniedActionInfo{
				Action:     act,
				ReasonCode: firstReasonCode(r.ReasonCodes),
				Basis:      r.Basis,
			})
		}
	}
	res.AvailableActions = deduplicate(res.AvailableActions)
	return nil
}

// HandleCanonicalDecision handles POST /internal/authorization/decisions (ZS-IAM-001 §8.1 & §8.2).
func (h *Handler) HandleCanonicalDecision(w http.ResponseWriter, r *http.Request) {
	var req domain.CanonicalDecisionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid_json",
			"message": err.Error(),
		})
		return
	}

	// Principal/Subject ID: accept subject_id or fallback to header
	subjectID := req.SubjectID
	if subjectID == "" {
		subjectID = r.Header.Get("X-Principal-Id")
	}
	if subjectID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": "subject_id",
		})
		return
	}

	// Action is required
	if req.Action == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": "action",
		})
		return
	}

	// Legal Entity ID: accept body or fallback to header
	legalEntityID := req.LegalEntityID
	if legalEntityID == "" {
		legalEntityID = r.Header.Get("X-Legal-Entity-Id")
	}
	if legalEntityID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": "legal_entity_id",
		})
		return
	}

	correlationID := req.CorrelationID
	if correlationID == "" {
		correlationID = r.Header.Get("X-Correlation-ID")
	}

	evaluationEntityID, ok := h.resolvePlatformScope(w, legalEntityID, correlationID)
	if !ok {
		return
	}

	tenantScope, ok := h.resolveTenantScope(w, r, subjectID, req.TenantID, evaluationEntityID)
	if !ok {
		return
	}

	bookID := req.BookID
	if bookID == "" {
		bookID = r.Header.Get("X-Book-Id")
	}
	orgUnitID := req.OrgUnitID
	if orgUnitID == "" {
		orgUnitID = r.Header.Get("X-Org-Unit-Id")
	}

	resourceOwnerID := ""
	if req.ResourceAttributes != nil {
		resourceOwnerID = req.ResourceAttributes["preparer_id"]
		if resourceOwnerID == "" {
			resourceOwnerID = req.ResourceAttributes["resource_owner_principal_id"]
		}
	}

	initiatingSubjectID := req.InitiatingSubjectID
	if initiatingSubjectID == "" && req.ResourceAttributes != nil {
		initiatingSubjectID = req.ResourceAttributes["initiating_subject_id"]
		if initiatingSubjectID == "" {
			initiatingSubjectID = req.ResourceAttributes["initiating_principal_id"]
			if initiatingSubjectID == "" {
				initiatingSubjectID = req.ResourceAttributes["on_behalf_of"]
			}
		}
	}
	if initiatingSubjectID == "" {
		initiatingSubjectID = r.Header.Get("X-Initiating-Subject-Id")
		if initiatingSubjectID == "" {
			initiatingSubjectID = r.Header.Get("X-On-Behalf-Of")
		}
	}

	if req.Environment.Audience == "" {
		aud := ""
		if req.ResourceAttributes != nil {
			aud = req.ResourceAttributes["audience"]
			if aud == "" {
				aud = req.ResourceAttributes["aud"]
			}
		}
		if aud == "" {
			aud = r.Header.Get("X-Audience")
			if aud == "" {
				aud = r.Header.Get("X-Token-Audience")
			}
		}
		req.Environment.Audience = aud
	}

	in := evalContext{
		PrincipalID:         subjectID,
		PrincipalType:       req.PrincipalType,
		TenantID:            tenantScope,
		LegalEntityID:       legalEntityID,
		BookID:              bookID,
		OrgUnitID:           orgUnitID,
		ResourceType:        req.ResourceType,
		ResourceID:          req.ResourceID,
		ActionType:          req.Action,
		ResourceOwnerID:     resourceOwnerID,
		Attributes:          req.ResourceAttributes,
		Environment:         req.Environment,
		PrivilegedSessionID: req.SessionID,
		BreakGlassSessionID: req.BreakGlassSessionID,
		SupportSessionID:    req.SupportSessionID,
		CorrelationID:       correlationID,
		InitiatingSubjectID: initiatingSubjectID,
	}

	evalRes, err := h.evaluateCore(r.Context(), in, evaluationEntityID)
	if err != nil {
		h.log.Error("CanonicalDecision: evaluation failed", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	// Persist the decision artifact with its full evidence.
	if req.ResourceVersion != 0 {
		in.ResourceVersion = strconv.Itoa(req.ResourceVersion)
	}
	decision, ok := h.recordDecision(w, r, in, evaluationEntityID, evalRes)
	if !ok {
		return
	}

	// Publish domain events & SIEM telemetry
	h.emitDecisionTelemetry(r.Context(), req.Action, subjectID, evaluationEntityID, tenantScope, correlationID, evalRes, decision)

	expiresAt := ""
	if decision.ExpiresAt != nil {
		expiresAt = decision.ExpiresAt.UTC().Format(time.RFC3339)
	}

	resp := domain.CanonicalDecisionResponse{
		Decision:   evalRes.Decision,
		DecisionID: decision.AccessDecisionID,
		// The configuration watermark actually in force, recorded with the
		// decision — not the constant this used to report for every decision.
		PolicySetVersion: decision.PolicySetVersion,
		MatchedGrants:    evalRes.MatchedGrants,
		NegativeControls: evalRes.NegativeControls,
		Obligations:      evalRes.Obligations,
		ExpiresAt:        expiresAt,
		ReasonCodes:      evalRes.ReasonCodes,
		AvailableActions: evalRes.AvailableActions,
		StepUp:           evalRes.StepUp,
		Reason:           evalRes.Reason,
		Basis:            evalRes.Basis,
		AuthorizationContext: map[string]interface{}{
			"tenant_id":       tenantScope,
			"legal_entity_id": evaluationEntityID,
			"book_id":         bookID,
			"org_unit_id":     orgUnitID,
			"resource_type":   req.ResourceType,
			"resource_id":     req.ResourceID,
			"principal_type":  req.PrincipalType,
		},
	}

	if initiatingSubjectID != "" {
		resp.AuthorizationContext["initiating_subject_id"] = initiatingSubjectID
		resp.AuthorizationContext["initiating_principal_id"] = initiatingSubjectID
	}
	if correlationID != "" {
		resp.AuthorizationContext["correlation_id"] = correlationID
	}
	if strings.EqualFold(req.PrincipalType, "WORKLOAD") || (req.ResourceAttributes != nil && req.ResourceAttributes["workload_id"] != "") {
		resp.AuthorizationContext["workload_id"] = subjectID
	}

	writeJSON(w, http.StatusOK, resp)
}

// GetAvailableActions handles GET /v1/{resource}/{id}/available-actions (ZS-IAM-001 §21).
func (h *Handler) GetAvailableActions(w http.ResponseWriter, r *http.Request) {
	resourceType := chi.URLParam(r, "resource")
	if resourceType == "" {
		resourceType = chi.URLParam(r, "resource_type")
	}
	resourceID := chi.URLParam(r, "id")
	if resourceID == "" {
		resourceID = chi.URLParam(r, "resource_id")
	}

	// The caller and tenant are the verified headers. Both used to fall back
	// to query parameters, so anyone could ask for anyone's actions in any
	// tenant (GOV invariant #2; ZS-IAM-001 §32 "client-supplied tenant").
	callerPrincipal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	principalID := callerPrincipal
	if q := strings.TrimSpace(r.URL.Query().Get("principal_id")); q != "" && q != callerPrincipal {
		// Another principal's actions are their grant map: iam.assignment.read,
		// as on /v1/entity-scope/validate.
		if !h.requirePermission(w, r, callerPrincipal, tenantScope, "iam.assignment.read") {
			return
		}
		principalID = q
	}

	legalEntityID := r.Header.Get("X-Legal-Entity-Id")
	if legalEntityID == "" {
		legalEntityID = r.URL.Query().Get("legal_entity_id")
	}
	if legalEntityID == "" {
		legalEntityID = PlatformScopeSentinel
	}

	correlationID := r.Header.Get("X-Correlation-ID")
	evaluationEntityID, ok := h.resolvePlatformScope(w, legalEntityID, correlationID)
	if !ok {
		return
	}
	// The entity must be in the caller's tenant, as on /v1/authorize
	// (Governance audit N-06). Without this, tenant A's tenant-wide grants
	// were listed as usable on tenant B's entity: the UI offered actions
	// /v1/authorize then refused (GOV-03 negative path #4), and a foreign
	// entity id could be probed for what it would allow.
	if _, ok := h.resolveTenantScope(w, r, principalID, "", evaluationEntityID); !ok {
		return
	}

	bookID := r.Header.Get("X-Book-Id")
	if bookID == "" {
		bookID = r.URL.Query().Get("book_id")
	}
	orgUnitID := r.Header.Get("X-Org-Unit-Id")
	if orgUnitID == "" {
		orgUnitID = r.URL.Query().Get("org_unit_id")
	}

	attrs := make(map[string]string)
	for k, v := range r.URL.Query() {
		if len(v) > 0 {
			attrs[k] = v[0]
		}
	}

	authnAge, authnAgeKnown := 0, false
	if rawAge := r.URL.Query().Get("authn_age_seconds"); rawAge != "" {
		authnAge, authnAgeKnown = parseAuthnAge(rawAge)
	}

	env := domain.EnvironmentContext{
		AuthnAgeSeconds: authnAge,
		AuthnAgeKnown:   authnAgeKnown,
		Assurance:       r.URL.Query().Get("assurance"),
		Risk:            r.URL.Query().Get("risk"),
	}

	in := evalContext{
		PrincipalID:     principalID,
		TenantID:        tenantScope,
		LegalEntityID:   legalEntityID,
		BookID:          bookID,
		OrgUnitID:       orgUnitID,
		ResourceType:    resourceType,
		ResourceID:      resourceID,
		ActionType:      "", // Will evaluate all held actions
		ResourceOwnerID: attrs["preparer_id"],
		Attributes:      attrs,
		Environment:     env,
		CorrelationID:   correlationID,
	}

	res := &evalResult{
		AvailableActions: make([]string, 0),
		DeniedActions:    make([]domain.DeniedActionInfo, 0),
		StepUpActions:    make([]domain.StepUpActionInfo, 0),
	}

	rbacActions, _, err := h.store.FindGrantedActionsScoped(r.Context(), principalID, evaluationEntityID, tenantScope, bookID, orgUnitID)
	if err != nil {
		h.log.Error("GetAvailableActions: rbac lookup failed", zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	delegatedActions, _, err := h.store.FindDelegatedActionsScoped(r.Context(), principalID, evaluationEntityID, tenantScope, bookID, orgUnitID)
	if err != nil {
		h.log.Error("GetAvailableActions: delegation lookup failed", zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	res.AllHeldActions = deduplicate(append(rbacActions, delegatedActions...))
	if err := h.computeAvailableActions(r.Context(), in, evaluationEntityID, res); err != nil {
		h.log.Error("GetAvailableActions: evaluation failed", zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	resp := domain.AvailableActionsResponse{
		ResourceType:     resourceType,
		ResourceID:       resourceID,
		PrincipalID:      principalID,
		AvailableActions: res.AvailableActions,
		DeniedActions:    res.DeniedActions,
		StepUpActions:    res.StepUpActions,
	}

	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) emitDecisionTelemetry(
	ctx context.Context,
	actionType, principalID, evaluationEntityID, tenantScope, correlationID string,
	evalRes *evalResult,
	decision *domain.AccessDecisionLog,
) {
	// With the outbox on, these events were committed with the decision.
	direct := h.decisionEvents == nil
	if evalRes.Outcome == domain.OutcomeGranted {
		if direct {
			if pubErr := h.publisher.PublishAuthorizationGranted(ctx, *decision); pubErr != nil {
				h.log.Error("emitTelemetry: failed to publish authorization.granted", zap.Error(pubErr))
			}
		}
	} else {
		if direct {
			if pubErr := h.publisher.PublishAuthorizationDenied(ctx, *decision); pubErr != nil {
				h.log.Error("emitTelemetry: failed to publish authorization.denied", zap.Error(pubErr))
			}
		}

		severity := siem.SeverityMedium
		isSoD := strings.HasPrefix(evalRes.Basis, "sod:")
		isWorkloadUnbound := evalRes.Basis == "workload:tenant_context_unbound"
		if isSoD || strings.HasPrefix(evalRes.Basis, principalStatusBasisPrefix) || isWorkloadUnbound {
			severity = siem.SeverityHigh
		}
		if evalRes.Outcome == domain.OutcomeStepUp {
			severity = siem.SeverityLow
		}

		h.siem.Stream(ctx, tenantScope, "authorization.denied", severity,
			fmt.Sprintf("Authorization decision for principal %s, action %s: %s (%s)", principalID, actionType, evalRes.Outcome, evalRes.Basis))

		if isSoD && direct {
			conflictingAction := actionType
			if strings.HasPrefix(evalRes.Basis, "sod:conflict_with=") {
				conflictingAction = evalRes.Basis[len("sod:conflict_with="):]
			}
			if pubErr := h.publisher.PublishSoDViolationDetected(ctx, *decision, conflictingAction); pubErr != nil {
				h.log.Error("emitTelemetry: failed to publish sod.violation.detected", zap.Error(pubErr))
			}
		}
	}
}

// ── Helper Predicates ────────────────────────────────────────────────────────

// The verb classifiers match whole tokens of the action name (split on "." and
// "_"), not substrings (Governance audit N-16): "release" is in "prerelease",
// "post" in "postcode".

func isApprovalAction(action string) bool {
	return domain.HasActionToken(action, "APPROVE", "APPROVAL")
}

func isReleaseAction(action string) bool {
	return domain.HasActionToken(action, "RELEASE")
}

func isExportAction(action string) bool {
	return domain.HasActionToken(action, "EXPORT", "EXPORTS")
}

func isGrantAction(action string) bool {
	lower := strings.ToLower(action)
	return strings.HasSuffix(lower, ".grant") || strings.HasSuffix(lower, ".assign") || strings.Contains(lower, "access_assign")
}

// StepUpPolicy is the server's own step-up rule (ZS-IAM-001 §7 stage 7,
// Scenario A26; Governance audit N-07).
//
// It used to live in the request: the PEP could raise max_authn_age_seconds to
// any value, and an absent authentication age read as a login zero seconds
// old, so a caller that sent nothing always passed. Now:
//   - which actions need step-up is the server's: any "reopen" action plus
//     Actions (exact names, case-insensitive). A caller may ADD the
//     requirement (step_up_required / requires_recent_authn) but not remove it;
//   - the maximum age is MaxAuthnAgeSeconds. A caller may only tighten it;
//   - an age the caller did not state is unknown, and UnknownAgeStepsUp
//     decides it: true (the default) answers STEP_UP. False keeps the old
//     reading, for a deployment whose PEPs cannot yet forward an age —
//     gateway-auth-svc does not assert one (register A-03).
type StepUpPolicy struct {
	Actions            []string
	MaxAuthnAgeSeconds int
	UnknownAgeStepsUp  bool
}

// DefaultStepUpPolicy: five minutes, and an unknown age steps up.
func DefaultStepUpPolicy() StepUpPolicy {
	return StepUpPolicy{MaxAuthnAgeSeconds: DefaultMaxAuthnAgeSecs, UnknownAgeStepsUp: true}
}

// SetStepUpPolicy replaces the step-up policy. A non-positive age keeps the
// default.
func (h *Handler) SetStepUpPolicy(p StepUpPolicy) {
	if p.MaxAuthnAgeSeconds <= 0 {
		p.MaxAuthnAgeSeconds = DefaultMaxAuthnAgeSecs
	}
	h.stepUp = &p
}

func (h *Handler) stepUpPolicy() StepUpPolicy {
	if h.stepUp == nil {
		return DefaultStepUpPolicy()
	}
	return *h.stepUp
}

func (h *Handler) isStepUpRequired(action string, attrs map[string]string, env domain.EnvironmentContext) bool {
	p := h.stepUpPolicy()
	required := strings.Contains(strings.ToLower(action), "reopen")
	for _, a := range p.Actions {
		if strings.EqualFold(strings.TrimSpace(a), action) {
			required = true
		}
	}
	if attrs != nil && (attrs["requires_recent_authn"] == "true" || attrs["step_up_required"] == "true") {
		required = true
	}
	if !required {
		return false
	}

	maxAge := p.MaxAuthnAgeSeconds
	if attrs != nil && attrs["max_authn_age_seconds"] != "" {
		// Tighten only: a caller asking for a fresher login is honoured, one
		// asking to accept an older login is not.
		if parsed, err := strconv.Atoi(attrs["max_authn_age_seconds"]); err == nil && parsed > 0 && parsed < maxAge {
			maxAge = parsed
		}
	}
	if env.Assurance == "NONE" {
		return true
	}
	if !env.AuthnAgeKnown {
		return p.UnknownAgeStepsUp
	}
	return env.AuthnAgeSeconds > maxAge
}

// parseAuthnAge reads a stated authentication age. Anything that is not a
// non-negative whole number of seconds is NOT a known age (second-pass scan,
// 13 Oct 2026): "abc" used to parse as 0 and count as a login zero seconds
// old, and "-600" as one younger than now — both passed step-up, the same
// fail-open N-07 closed for an absent age. An unknown age is decided by
// StepUpPolicy.UnknownAgeStepsUp.
func parseAuthnAge(raw string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func checkCoolingWindowActive(changedAtStr string) bool {
	t, err := time.Parse(time.RFC3339, changedAtStr)
	if err != nil {
		t, err = time.Parse(time.RFC3339Nano, changedAtStr)
	}
	if err != nil {
		return true // If cannot parse, fail closed
	}
	return time.Since(t) < DefaultCoolingWindowSecs*time.Second
}

func firstReasonCode(codes []string) string {
	if len(codes) > 0 {
		return codes[0]
	}
	return "UNKNOWN"
}

func deduplicate(items []string) []string {
	seen := make(map[string]struct{}, len(items))
	result := make([]string, 0, len(items))
	for _, it := range items {
		if _, ok := seen[it]; !ok && it != "" {
			seen[it] = struct{}{}
			result = append(result, it)
		}
	}
	return result
}

func getResourceLifecycleStatus(attrs map[string]string) string {
	if attrs == nil {
		return ""
	}
	status := strings.ToUpper(strings.TrimSpace(attrs["status"]))
	if status == "" {
		status = strings.ToUpper(strings.TrimSpace(attrs["resource_status"]))
	}
	if status == "" {
		status = strings.ToUpper(strings.TrimSpace(attrs["lifecycle_state"]))
	}
	return status
}

func isTerminalLifecycleState(status string) bool {
	switch status {
	case "VOIDED", "CANCELLED", "ARCHIVED", "CLOSED", "EXPIRED", "TERMINATED":
		return true
	default:
		return false
	}
}

func isMutationOrApprovalAction(action string) bool {
	return domain.HasActionToken(action, "EDIT", "MODIFY", "UPDATE", "APPROVE", "APPROVAL", "RELEASE",
		"POST", "POSTING", "CANCEL", "VOID", "REOPEN")
}

// GetMyCapabilities handles GET /v1/me/capabilities (ZS-IAM-001 §21).
func (h *Handler) GetMyCapabilities(w http.ResponseWriter, r *http.Request) {
	// "me": the verified caller only (§21 "for current user"). The
	// principal_id / tenant_id query fallbacks let anyone read anyone's
	// capabilities in any tenant.
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	legalEntityID := r.Header.Get("X-Legal-Entity-Id")
	if legalEntityID == "" {
		legalEntityID = r.URL.Query().Get("legal_entity_id")
	}
	if legalEntityID == "" {
		legalEntityID = PlatformScopeSentinel
	}

	correlationID := r.Header.Get("X-Correlation-ID")
	evaluationEntityID, ok := h.resolvePlatformScope(w, legalEntityID, correlationID)
	if !ok {
		return
	}
	// The entity must be in the caller's tenant, as on /v1/authorize
	// (Governance audit N-06). Without this, tenant A's tenant-wide grants
	// were listed as usable on tenant B's entity: the UI offered actions
	// /v1/authorize then refused (GOV-03 negative path #4), and a foreign
	// entity id could be probed for what it would allow.
	if _, ok := h.resolveTenantScope(w, r, principalID, "", evaluationEntityID); !ok {
		return
	}

	bookID := r.Header.Get("X-Book-Id")
	if bookID == "" {
		bookID = r.URL.Query().Get("book_id")
	}
	orgUnitID := r.Header.Get("X-Org-Unit-Id")
	if orgUnitID == "" {
		orgUnitID = r.URL.Query().Get("org_unit_id")
	}

	principalStatus, err := h.store.FindPrincipalStatus(r.Context(), principalID, tenantScope)
	if err != nil {
		h.log.Error("GetMyCapabilities: principal status lookup failed", zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	if principalStatus != domain.PrincipalStatusActive {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "principal_not_active",
			"message": "Principal account is " + principalStatus,
		})
		return
	}

	rbacActions, _, err := h.store.FindGrantedActionsScoped(r.Context(), principalID, evaluationEntityID, tenantScope, bookID, orgUnitID)
	if err != nil {
		h.log.Error("GetMyCapabilities: rbac lookup failed", zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	delegatedActions, _, err := h.store.FindDelegatedActionsScoped(r.Context(), principalID, evaluationEntityID, tenantScope, bookID, orgUnitID)
	if err != nil {
		h.log.Error("GetMyCapabilities: delegation lookup failed", zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	allPermissions := deduplicate(append(rbacActions, delegatedActions...))
	moduleSet := make(map[string]struct{})
	features := make(map[string]bool)

	for _, perm := range allPermissions {
		parts := strings.Split(perm, ".")
		if len(parts) > 1 {
			moduleSet[parts[0]] = struct{}{}
		} else {
			partsUnder := strings.Split(perm, "_")
			if len(partsUnder) > 1 {
				moduleSet[strings.ToLower(partsUnder[0])] = struct{}{}
			}
		}

		// Whole tokens, like every other classifier here (N-16). These used
		// substrings, so ADDRESS_POSTCODE_UPDATE advertised can_post_journals
		// and SIEM_EXPORTER_READ can_export — the console then offered actions
		// /v1/authorize refuses.
		if isReleaseAction(perm) {
			features["can_release_payments"] = true
		}
		if isApprovalAction(perm) {
			features["can_approve"] = true
		}
		if domain.HasActionToken(perm, "POST", "POSTING") {
			features["can_post_journals"] = true
		}
		if isExportAction(perm) {
			features["can_export"] = true
		}
	}

	modules := make([]string, 0, len(moduleSet))
	for m := range moduleSet {
		modules = append(modules, m)
	}

	resp := domain.CapabilitiesResponse{
		PrincipalID:   principalID,
		TenantID:      tenantScope,
		LegalEntityID: evaluationEntityID,
		BookID:        bookID,
		OrgUnitID:     orgUnitID,
		Permissions:   allPermissions,
		Modules:       modules,
		Features:      features,
	}

	writeJSON(w, http.StatusOK, resp)
}
