package handler

import (
	"context"
	"encoding/json"
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
}

type evalResult struct {
	Decision         string // ALLOW, DENY, STEP_UP
	Outcome          string // GRANTED, DENIED, STEP_UP
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
func (h *Handler) evaluateCore(ctx context.Context, in evalContext, evaluationEntityID string) (*evalResult, error) {
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
	if !granted && contains(delegatedActions, in.ActionType) {
		granted = true
		basis = delegatedBasis
		res.MatchedGrants = append(res.MatchedGrants, delegatedBasis)
	}

	// ── Layer 3: Privileged Access Management (JIT Elevation) ─────────────────
	if in.PrivilegedSessionID != "" {
		ps, err := h.store.FindPrivilegedSessionByID(ctx, in.PrivilegedSessionID, in.TenantID)
		if err == nil && ps != nil {
			if ps.PrincipalID == in.PrincipalID && ps.Status == domain.PrivilegedSessionStatusActive && time.Now().UTC().Before(ps.ExpiresAt) {
				allHeldActions = append(allHeldActions, ps.RequestedActions...)
				if !granted && (contains(ps.RequestedActions, in.ActionType) || contains(ps.RequestedActions, "*")) {
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
		if err == nil && bg != nil {
			if bg.PrincipalID == in.PrincipalID && bg.IncidentID != "" && bg.Status == domain.BreakGlassSessionStatusActive && time.Now().UTC().Before(bg.ExpiresAt) {
				allHeldActions = append(allHeldActions, bg.RequestedActions...)
				if !granted && (contains(bg.RequestedActions, in.ActionType) || contains(bg.RequestedActions, "*")) {
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
		if err == nil && ss != nil {
			if ss.SupportOperatorID == in.PrincipalID && ss.Status == domain.SupportSessionStatusActive && time.Now().UTC().Before(ss.ExpiresAt) {
				isExportAction := strings.HasSuffix(in.ActionType, ".export") || in.ActionType == "export" || strings.Contains(in.ActionType, "export")
				isMutationAction := strings.HasSuffix(in.ActionType, ".create") || strings.HasSuffix(in.ActionType, ".edit") || strings.HasSuffix(in.ActionType, ".post") || strings.HasSuffix(in.ActionType, ".delete") || strings.HasSuffix(in.ActionType, ".release") || strings.HasSuffix(in.ActionType, ".revoke")

				if isExportAction && !ss.AllowBulkExport && !contains(ss.AllowedActions, in.ActionType) {
					res.Decision = domain.CanonicalDecisionDeny
					res.Outcome = domain.OutcomeDenied
					res.Basis = "support:bulk_export_prohibited"
					res.Reason = "Bulk export prohibited in support session"
					res.ReasonCodes = []string{"SUPPORT_BULK_EXPORT_PROHIBITED"}
					res.NegativeControls = []string{"support:bulk_export_prohibited"}
					return res, nil
				}

				if ss.ReadOnly && isMutationAction && !contains(ss.AllowedActions, in.ActionType) {
					res.Decision = domain.CanonicalDecisionDeny
					res.Outcome = domain.OutcomeDenied
					res.Basis = "support:read_only_session"
					res.Reason = "Write operation prohibited in read-only support session"
					res.ReasonCodes = []string{"SUPPORT_READ_ONLY_VIOLATION"}
					res.NegativeControls = []string{"support:read_only_session"}
					return res, nil
				}

				if contains(ss.AllowedActions, in.ActionType) || contains(ss.AllowedActions, "*") || (!ss.ReadOnly && len(ss.AllowedActions) == 0) || (ss.ReadOnly && !isMutationAction) {
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
		h.computeAvailableActions(ctx, in, evaluationEntityID, res)
		return res, nil
	}

	// ── Layer 4: Static SoD Conflicts ─────────────────────────────────────────
	others := removeAll(allHeldActions, in.ActionType)
	conflicting, hasConflict, err := h.store.CheckSoDConflict(ctx, others, in.ActionType, in.TenantID)
	if err != nil {
		return nil, fmt.Errorf("sod check: %w", err)
	}
	if hasConflict {
		res.Decision = domain.CanonicalDecisionDeny
		res.Outcome = domain.OutcomeDenied
		res.Basis = "sod:conflict_with=" + conflicting
		res.Reason = fmt.Sprintf("Static Segregation of Duties conflict with held action %s", conflicting)
		res.ReasonCodes = []string{"SOD_STATIC_CONFLICT"}
		res.NegativeControls = []string{"sod:conflict_with=" + conflicting}

		h.computeAvailableActions(ctx, in, evaluationEntityID, res)
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

			h.computeAvailableActions(ctx, in, evaluationEntityID, res)
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

			h.computeAvailableActions(ctx, in, evaluationEntityID, res)
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

			h.computeAvailableActions(ctx, in, evaluationEntityID, res)
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

			h.computeAvailableActions(ctx, in, evaluationEntityID, res)
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

			h.computeAvailableActions(ctx, in, evaluationEntityID, res)
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

			h.computeAvailableActions(ctx, in, evaluationEntityID, res)
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

		h.computeAvailableActions(ctx, in, evaluationEntityID, res)
		return res, nil
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

		h.computeAvailableActions(ctx, in, evaluationEntityID, res)
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

		h.computeAvailableActions(ctx, in, evaluationEntityID, res)
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

		h.computeAvailableActions(ctx, in, evaluationEntityID, res)
		return res, nil
	}

	// ── Layer 7: Stage 7 Assurance / Step-Up Handling (Scenario A26, §7, §8.2) ─
	// Scenario A26: Session authentication too old for period reopen -> STEP_UP
	if isStepUpRequired(in.ActionType, in.Attributes, in.Environment) {
		res.Decision = domain.CanonicalDecisionStepUp
		res.Outcome = domain.OutcomeStepUp
		res.Basis = "assurance:recent_authn_required"
		res.Reason = "Session authentication too old for period reopen (Scenario A26)"
		res.ReasonCodes = []string{"STEP_UP_REQUIRED", "AUTHENTICATION_ASSURANCE_REQUIRED"}
		res.Obligations = []string{"REQUIRE_RECENT_AUTHN"}
		res.StepUp = &domain.StepUpRequirement{
			RequiredAssurance:  "PHISHING_RESISTANT",
			MaxAuthnAgeSeconds: DefaultMaxAuthnAgeSecs,
			Reason:             "PERIOD_REOPEN_REQUIRES_RECENT_AUTHN",
		}

		h.computeAvailableActions(ctx, in, evaluationEntityID, res)
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

			h.computeAvailableActions(ctx, in, evaluationEntityID, res)
			return res, nil
		}
	}

	// ── Final ALLOW Decision ──────────────────────────────────────────────────
	res.Decision = domain.CanonicalDecisionAllow
	res.Outcome = domain.OutcomeGranted
	res.Basis = basis
	res.Reason = "Access permitted per active policy and scope"
	res.ReasonCodes = []string{"PERMISSION_GRANTED", "SOD_CLEAR", "AUTHORITY_WITHIN_LIMIT"}

	// High risk obligation per §8.2
	if in.Environment.Risk == "HIGH" || strings.ToLower(in.Attributes["risk"]) == "high" {
		res.Obligations = append(res.Obligations, "RECORD_HIGH_RISK_EVIDENCE")
	}

	h.computeAvailableActions(ctx, in, evaluationEntityID, res)
	return res, nil
}

// computeAvailableActions populates available_actions, denied_actions, and step_up_actions
// for the subject in this resource/scope context.
func (h *Handler) computeAvailableActions(ctx context.Context, in evalContext, evaluationEntityID string, res *evalResult) {
	for _, act := range res.AllHeldActions {
		if act == in.ActionType {
			if res.Decision == domain.CanonicalDecisionAllow {
				res.AvailableActions = append(res.AvailableActions, act)
			} else if res.Decision == domain.CanonicalDecisionStepUp {
				res.StepUpActions = append(res.StepUpActions, domain.StepUpActionInfo{
					Action:             act,
					RequiredAssurance:  "PHISHING_RESISTANT",
					MaxAuthnAgeSeconds: DefaultMaxAuthnAgeSecs,
					Reason:             res.Reason,
				})
			} else {
				res.DeniedActions = append(res.DeniedActions, domain.DeniedActionInfo{
					Action:     act,
					ReasonCode: firstReasonCode(res.ReasonCodes),
					Basis:      res.Basis,
				})
			}
			continue
		}

		// Candidate check for other held actions
		if isReleaseAction(act) && in.Attributes != nil {
			bankChangedBy := in.Attributes["supplier_bank_changed_by"]
			if bankChangedBy == "" {
				bankChangedBy = in.Attributes["bank_account_changed_by"]
			}
			coolingViolation := in.Attributes["cooling_window_violation"] == "true" ||
				in.Attributes["supplier_bank_cooling_window_active"] == "true" ||
				in.Attributes["cooling_window_conflict"] == "true" ||
				in.Attributes["supplier_bank_cooling_active"] == "true" ||
				in.Attributes["cooling_window_active"] == "true"

			if (bankChangedBy == in.PrincipalID && coolingViolation) || in.Attributes["cooling_window_conflict"] == "true" {
				res.DeniedActions = append(res.DeniedActions, domain.DeniedActionInfo{
					Action:     act,
					ReasonCode: "SOD_DYNAMIC_CONFLICT",
					Basis:      "sod:cooling_window_conflict",
				})
				continue
			}
		}

		// Dynamic SoD — Prior rejected reviewer (§10.2 Pattern 5)
		if isApprovalAction(act) && in.Attributes != nil {
			priorRejectedBy := in.Attributes["prior_rejected_by"]
			if priorRejectedBy == "" {
				priorRejectedBy = in.Attributes["rejected_by"]
			}
			isResubmission := in.Attributes["is_resubmission"] == "true" || in.Attributes["resubmitted"] == "true" || priorRejectedBy != ""
			if priorRejectedBy != "" && priorRejectedBy == in.PrincipalID && isResubmission {
				res.DeniedActions = append(res.DeniedActions, domain.DeniedActionInfo{
					Action:     act,
					ReasonCode: "PRIOR_REJECTED_REVIEWER_CONFLICT",
					Basis:      "sod:prior_rejected_reviewer",
				})
				continue
			}
		}

		// Dynamic SoD — Related-party conflict (§10.2 Pattern 6)
		if in.Attributes != nil {
			relatedPartySubject := in.Attributes["related_party_subject_id"]
			if relatedPartySubject == "" {
				relatedPartySubject = in.Attributes["vendor_related_party_subject_id"]
			}
			isRelatedParty := in.Attributes["is_related_party"] == "true" || in.Attributes["related_party_conflict"] == "true" ||
				(relatedPartySubject != "" && relatedPartySubject == in.PrincipalID)
			if isRelatedParty {
				res.DeniedActions = append(res.DeniedActions, domain.DeniedActionInfo{
					Action:     act,
					ReasonCode: "RELATED_PARTY_CONFLICT",
					Basis:      "sod:related_party_conflict",
				})
				continue
			}
		}

		// Domain Guards — Resource Lifecycle State (ZS-STATE-001)
		if in.Attributes != nil {
			status := getResourceLifecycleStatus(in.Attributes)
			if isTerminalLifecycleState(status) && isMutationOrApprovalAction(act) {
				res.DeniedActions = append(res.DeniedActions, domain.DeniedActionInfo{
					Action:     act,
					ReasonCode: "RESOURCE_STATE_CONFLICT",
					Basis:      "state:terminal_status=" + status,
				})
				continue
			}
		}

		if isStepUpRequired(act, in.Attributes, in.Environment) {
			res.StepUpActions = append(res.StepUpActions, domain.StepUpActionInfo{
				Action:             act,
				RequiredAssurance:  "PHISHING_RESISTANT",
				MaxAuthnAgeSeconds: DefaultMaxAuthnAgeSecs,
				Reason:             "Action requires high assurance or recent authentication",
			})
			continue
		}

		res.AvailableActions = append(res.AvailableActions, act)
	}

	res.AvailableActions = deduplicate(res.AvailableActions)
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

	tenantScope, ok := h.resolveTenantScope(w, r, req.TenantID)
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
		PrivilegedSessionID:  req.SessionID,
		CorrelationID:       correlationID,
		InitiatingSubjectID: initiatingSubjectID,
	}

	evalRes, err := h.evaluateCore(r.Context(), in, evaluationEntityID)
	if err != nil {
		h.log.Error("CanonicalDecision: evaluation failed", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	// Persist the decision artifact in access_decision_log
	decision, err := h.store.RecordAccessDecision(r.Context(), domain.RecordAccessDecisionParams{
		PrincipalID:   subjectID,
		LegalEntityID: evaluationEntityID,
		ActionType:    req.Action,
		Outcome:       evalRes.Outcome,
		Basis:         evalRes.Basis,
		CorrelationID: correlationID,
		TenantID:      tenantScope,
	})
	if err != nil {
		h.log.Error("CanonicalDecision: failed to record access decision", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	// Publish domain events & SIEM telemetry
	h.emitDecisionTelemetry(r.Context(), req.Action, subjectID, evaluationEntityID, tenantScope, correlationID, evalRes, decision)

	expiresAt := time.Now().UTC().Add(5 * time.Minute).Format(time.RFC3339)

	resp := domain.CanonicalDecisionResponse{
		Decision:         evalRes.Decision,
		DecisionID:       decision.AccessDecisionID,
		PolicySetVersion: DefaultPolicySetVersion,
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

	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		principalID = r.URL.Query().Get("principal_id")
	}
	if principalID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing_principal",
			"message": "X-Principal-Id header or principal_id query parameter is required",
		})
		return
	}

	tenantScope := r.Header.Get("X-Tenant-Id")
	if tenantScope == "" {
		tenantScope = r.URL.Query().Get("tenant_id")
	}
	if tenantScope == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing_tenant_scope",
			"message": "X-Tenant-Id header or tenant_id query parameter is required",
		})
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

	authnAge := 0
	if rawAge := r.URL.Query().Get("authn_age_seconds"); rawAge != "" {
		authnAge, _ = strconv.Atoi(rawAge)
	}

	env := domain.EnvironmentContext{
		AuthnAgeSeconds: authnAge,
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
	h.computeAvailableActions(r.Context(), in, evaluationEntityID, res)

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
	if evalRes.Outcome == domain.OutcomeGranted {
		if pubErr := h.publisher.PublishAuthorizationGranted(ctx, *decision); pubErr != nil {
			h.log.Error("emitTelemetry: failed to publish authorization.granted", zap.Error(pubErr))
		}
	} else {
		if pubErr := h.publisher.PublishAuthorizationDenied(ctx, *decision); pubErr != nil {
			h.log.Error("emitTelemetry: failed to publish authorization.denied", zap.Error(pubErr))
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
		if isWorkloadUnbound {
			h.siem.Stream(ctx, tenantScope, "security.workload.unbound_tenant", siem.SeverityHigh,
				fmt.Sprintf("Workload %s attempted access with unbound tenant %s", principalID, tenantScope))
		}

		if isSoD {
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

func isApprovalAction(action string) bool {
	lower := strings.ToLower(action)
	return strings.HasSuffix(lower, ".approve") || lower == "approve" || strings.Contains(lower, "approve")
}

func isReleaseAction(action string) bool {
	lower := strings.ToLower(action)
	return strings.HasSuffix(lower, ".release") || lower == "release" || strings.Contains(lower, "release")
}

func isGrantAction(action string) bool {
	lower := strings.ToLower(action)
	return strings.HasSuffix(lower, ".grant") || strings.HasSuffix(lower, ".assign") || strings.Contains(lower, "access_assign")
}

func isStepUpRequired(action string, attrs map[string]string, env domain.EnvironmentContext) bool {
	lower := strings.ToLower(action)
	isPeriodReopen := strings.Contains(lower, "period.reopen") || strings.Contains(lower, "period_reopen") || strings.Contains(lower, "reopen")
	explicitRequire := attrs != nil && (attrs["requires_recent_authn"] == "true" || attrs["step_up_required"] == "true")

	if isPeriodReopen || explicitRequire {
		maxAge := DefaultMaxAuthnAgeSecs
		if attrs != nil && attrs["max_authn_age_seconds"] != "" {
			if parsed, err := strconv.Atoi(attrs["max_authn_age_seconds"]); err == nil && parsed > 0 {
				maxAge = parsed
			}
		}
		// If authn age exceeds threshold or is not recent (Scenario A26)
		if env.AuthnAgeSeconds > maxAge || env.Assurance == "NONE" {
			return true
		}
	}
	return false
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
	lower := strings.ToLower(action)
	return strings.HasSuffix(lower, ".edit") || strings.HasSuffix(lower, ".modify") ||
		strings.HasSuffix(lower, ".update") || strings.HasSuffix(lower, ".approve") ||
		strings.HasSuffix(lower, ".release") || strings.HasSuffix(lower, ".post") ||
		strings.HasSuffix(lower, ".cancel") || strings.HasSuffix(lower, ".void") ||
		strings.HasSuffix(lower, ".reopen") || strings.Contains(lower, "approve") ||
		strings.Contains(lower, "release") || strings.Contains(lower, "post")
}

// GetMyCapabilities handles GET /v1/me/capabilities (ZS-IAM-001 §21).
func (h *Handler) GetMyCapabilities(w http.ResponseWriter, r *http.Request) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		principalID = r.URL.Query().Get("principal_id")
	}
	if principalID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "missing_principal",
			"message": "X-Principal-Id header or principal_id query parameter is required",
		})
		return
	}

	tenantScope := r.Header.Get("X-Tenant-Id")
	if tenantScope == "" {
		tenantScope = r.URL.Query().Get("tenant_id")
	}
	if tenantScope == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "missing_tenant_scope",
			"message": "X-Tenant-Id header or tenant_id query parameter is required",
		})
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

		lower := strings.ToLower(perm)
		if strings.Contains(lower, "release") {
			features["can_release_payments"] = true
		}
		if strings.Contains(lower, "approve") {
			features["can_approve"] = true
		}
		if strings.Contains(lower, "post") {
			features["can_post_journals"] = true
		}
		if strings.Contains(lower, "export") {
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

