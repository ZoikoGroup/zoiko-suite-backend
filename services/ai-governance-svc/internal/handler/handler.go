package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authzpkg "zoiko.io/ai-governance-svc/internal/authz"
	"zoiko.io/ai-governance-svc/internal/domain"
	"zoiko.io/ai-governance-svc/internal/events"
	"zoiko.io/ai-governance-svc/internal/store"
)

// platformScopeID is the legal_entity_id passed to authorization-svc for
// governance-configuration actions (action-risk taxonomy, automation
// allowlists, provider registry, policy-change approvals) — these are
// platform administration, not scoped to one tenant's data. Same convention
// as capability-registry-svc/policy-svc/configuration-feature-flag-svc.
const platformScopeID = "00000000-0000-0000-0000-00000000f001"

const (
	AIRunCreate                 = "AI_RUN_CREATE"
	ActionRiskClassificationSet = "ACTION_RISK_CLASSIFICATION_SET"
	AutomationPolicyCreate      = "AUTOMATION_POLICY_CREATE"
	AutomationActionPropose     = "AUTOMATION_ACTION_PROPOSE"
	AutomationActionDecide      = "AUTOMATION_ACTION_DECIDE"
	ModelProviderRegister       = "MODEL_PROVIDER_REGISTER"
	PolicyChangePropose         = "POLICY_CHANGE_PROPOSE"
	PolicyChangeDecide          = "POLICY_CHANGE_DECIDE"
)

type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type Handler struct {
	store     store.Store
	publisher events.Publisher
	authz     AuthzChecker
	logger    *zap.Logger
}

func New(st store.Store, pub events.Publisher, az AuthzChecker, logger *zap.Logger) *Handler {
	return &Handler{store: st, publisher: pub, authz: az, logger: logger}
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, actionType string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principalID, platformScopeID, actionType); err != nil {
		if errors.Is(err, authzpkg.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "not authorized to perform this action")
		} else {
			writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
		}
		return false
	}
	return true
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/ai-runs", func(r chi.Router) {
		r.Post("/", h.CreateAIRun)
		r.Get("/{id}", h.GetAIRun)
	})
	r.Route("/v1/action-risk-classifications", func(r chi.Router) {
		r.Post("/", h.SetActionRiskClassification)
		r.Get("/{actionType}", h.GetActionRiskClassification)
	})
	r.Route("/v1/automation-policies", func(r chi.Router) {
		r.Post("/", h.CreateAutomationPolicy)
		r.Get("/resolve", h.ResolveAutomationPolicy)
	})
	r.Route("/v1/automation-actions", func(r chi.Router) {
		r.Post("/", h.ProposeAutomationAction)
		r.Get("/{id}", h.GetAutomationAction)
		r.Post("/{id}/decision", h.DecideAutomationAction)
	})
	r.Route("/v1/model-providers", func(r chi.Router) {
		r.Post("/", h.RegisterModelProvider)
		r.Get("/{provider}/{model}", h.GetModelProvider)
		r.Get("/{provider}/{model}/verify", h.VerifyModelProvider)
	})
	r.Route("/v1/policy-change-approvals", func(r chi.Router) {
		r.Post("/", h.ProposePolicyChange)
		r.Get("/{id}", h.GetPolicyChangeApproval)
		r.Post("/{id}/decision", h.DecidePolicyChange)
	})
}

// CreateAIRun records doc7 §G1's AI run/recommendation object. No authz gate
// — every AI-invoking service needs to write this cheaply and often, and the
// record itself carries no authority (it's evidence, not an action).
func (h *Handler) CreateAIRun(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateAIRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.RunType == "" || req.ModelID == "" || req.PromptVersion == "" || req.AuditID == "" {
		writeError(w, http.StatusBadRequest, "run_type, model_id, prompt_version, and audit_id are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID := r.Header.Get("X-Tenant-Id")

	uncertaintyState := domain.UncertaintyNone
	if req.UncertaintyState != "" {
		uncertaintyState = domain.UncertaintyState(req.UncertaintyState)
	}
	a := &domain.AIRun{
		AIRunID:              uuid.NewString(),
		TenantID:             tenantID,
		RunType:              domain.AIRunType(req.RunType),
		ModelID:              req.ModelID,
		PromptVersion:        req.PromptVersion,
		SourceRefs:           req.SourceRefs,
		EvidenceRefs:         req.EvidenceRefs,
		Confidence:           req.Confidence,
		UncertaintyState:     uncertaintyState,
		AuditID:              req.AuditID,
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: principalID,
	}
	if req.ToolVersion != "" {
		a.ToolVersion = &req.ToolVersion
	}
	if req.Limitation != "" {
		a.Limitation = &req.Limitation
	}
	if req.RecommendedAction != "" {
		a.RecommendedAction = &req.RecommendedAction
	}
	if err := h.store.CreateAIRun(r.Context(), a); err != nil {
		h.logger.Error("create ai run failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create ai run")
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

func (h *Handler) GetAIRun(w http.ResponseWriter, r *http.Request) {
	a, err := h.store.GetAIRun(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		if errors.Is(err, domain.ErrAIRunNotFound) {
			writeError(w, http.StatusNotFound, "ai run not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get ai run")
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (h *Handler) SetActionRiskClassification(w http.ResponseWriter, r *http.Request) {
	var req domain.SetActionRiskClassificationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ActionType == "" {
		writeError(w, http.StatusBadRequest, "action_type is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, ActionRiskClassificationSet) {
		return
	}

	riskCategory := domain.RiskCategoryNone
	if req.RiskCategory != "" {
		riskCategory = domain.RiskCategory(req.RiskCategory)
	}
	c := &domain.ActionRiskClassification{
		ActionType:           req.ActionType,
		RiskCategory:         riskCategory,
		HumanReviewTrigger:   req.HumanReviewTrigger,
		RequiresMakerChecker: req.RequiresMakerChecker,
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: principalID,
	}
	if err := h.store.SetActionRiskClassification(r.Context(), c); err != nil {
		h.logger.Error("set action risk classification failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to set action risk classification")
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (h *Handler) GetActionRiskClassification(w http.ResponseWriter, r *http.Request) {
	c, err := h.store.GetActionRiskClassification(r.Context(), chi.URLParam(r, "actionType"))
	if err != nil {
		if errors.Is(err, domain.ErrActionRiskClassificationNotFound) {
			writeError(w, http.StatusNotFound, "action risk classification not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get action risk classification")
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (h *Handler) CreateAutomationPolicy(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateAutomationPolicyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.TenantID == "" || req.Role == "" || req.Tool == "" || req.ActionType == "" {
		writeError(w, http.StatusBadRequest, "tenant_id, role, tool, and action_type are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, AutomationPolicyCreate) {
		return
	}

	riskCategory := domain.RiskCategoryNone
	if req.RiskCategory != "" {
		riskCategory = domain.RiskCategory(req.RiskCategory)
	}
	p := &domain.AutomationPolicy{
		AutomationPolicyID:   uuid.NewString(),
		TenantID:             req.TenantID,
		Role:                 req.Role,
		RiskCategory:         riskCategory,
		Tool:                 req.Tool,
		ActionType:           req.ActionType,
		MaxScopeAmount:       req.MaxScopeAmount,
		RequiredApprovals:    req.RequiredApprovals,
		DryRunRequired:       req.DryRunRequired,
		RateLimitPerDay:      req.RateLimitPerDay,
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: principalID,
	}
	if err := h.store.CreateAutomationPolicy(r.Context(), p); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			writeError(w, http.StatusConflict, "an automation policy already exists for this tenant/role/risk-category/tool/action")
			return
		}
		h.logger.Error("create automation policy failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create automation policy")
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

// ResolveAutomationPolicy is doc7 §G7's core check: may this
// tenant/role/risk-class/tool autonomously perform this action right now.
func (h *Handler) ResolveAutomationPolicy(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tenantID, role, riskCategory, tool, actionType := q.Get("tenant_id"), q.Get("role"), q.Get("risk_category"), q.Get("tool"), q.Get("action_type")
	if tenantID == "" || role == "" || tool == "" || actionType == "" {
		writeError(w, http.StatusBadRequest, "tenant_id, role, tool, and action_type query params are required")
		return
	}
	if riskCategory == "" {
		riskCategory = string(domain.RiskCategoryNone)
	}
	resolved, err := h.store.ResolveAutomationPolicy(r.Context(), tenantID, role, riskCategory, tool, actionType)
	if err != nil {
		h.logger.Error("resolve automation policy failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to resolve automation policy")
		return
	}
	writeJSON(w, http.StatusOK, resolved)
}

// ProposeAutomationAction is doc7 §G2/§G7's gate: an autonomous action may
// only be proposed if an AutomationPolicy allowlists it. The proposer's own
// idempotency key structurally prevents a retried proposal from creating a
// second row.
func (h *Handler) ProposeAutomationAction(w http.ResponseWriter, r *http.Request) {
	var req domain.ProposeAutomationActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.TenantID == "" || req.ActionType == "" || req.Role == "" || req.Tool == "" || req.IdempotencyKey == "" {
		writeError(w, http.StatusBadRequest, "tenant_id, action_type, role, tool, and idempotency_key are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, AutomationActionPropose) {
		return
	}

	classification, err := h.store.GetActionRiskClassification(r.Context(), req.ActionType)
	if err != nil && !errors.Is(err, domain.ErrActionRiskClassificationNotFound) {
		writeError(w, http.StatusInternalServerError, "failed to fetch action risk classification")
		return
	}
	riskCategory := domain.RiskCategoryNone
	requiresMakerChecker := false
	if classification != nil {
		riskCategory = classification.RiskCategory
		requiresMakerChecker = classification.RequiresMakerChecker
	}

	resolution, err := h.store.ResolveAutomationPolicy(r.Context(), req.TenantID, req.Role, string(riskCategory), req.Tool, req.ActionType)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to resolve automation policy")
		return
	}
	if !resolution.Allowed {
		writeError(w, http.StatusForbidden, "action is not allowlisted: "+resolution.ReasonCode)
		return
	}

	approvalStatus := domain.ApprovalNotRequired
	if requiresMakerChecker {
		approvalStatus = domain.ApprovalPending
	}
	a := &domain.AutomationAction{
		AutomationActionID:    uuid.NewString(),
		TenantID:              req.TenantID,
		ActionType:            req.ActionType,
		RiskCategory:          riskCategory,
		IdempotencyKey:        req.IdempotencyKey,
		PreconditionsMet:      req.PreconditionsMet,
		ApprovalStatus:        approvalStatus,
		Status:                domain.AutomationActionProposed,
		ProposedByPrincipalID: principalID,
		CreatedAt:             time.Now().UTC(),
	}
	if req.RollbackPlan != "" {
		a.RollbackPlan = &req.RollbackPlan
	}
	if err := h.store.ProposeAutomationAction(r.Context(), a); err != nil {
		if errors.Is(err, domain.ErrDuplicateIdempotencyKey) {
			writeError(w, http.StatusConflict, "automation action already proposed with this idempotency_key")
			return
		}
		h.logger.Error("propose automation action failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to propose automation action")
		return
	}
	_ = h.publisher.Publish(r.Context(), "automation_action.proposed", a.AutomationActionID, a.TenantID, a)
	writeJSON(w, http.StatusCreated, a)
}

func (h *Handler) GetAutomationAction(w http.ResponseWriter, r *http.Request) {
	a, err := h.store.GetAutomationAction(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		if errors.Is(err, domain.ErrAutomationActionNotFound) {
			writeError(w, http.StatusNotFound, "automation action not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get automation action")
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// DecideAutomationAction is doc7 §G2/§H3's maker-checker decision step —
// self-approval (decider == proposer) is blocked fail-closed.
func (h *Handler) DecideAutomationAction(w http.ResponseWriter, r *http.Request) {
	actionID := chi.URLParam(r, "id")
	var req domain.ApproveAutomationActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Decision != string(domain.ApprovalApproved) && req.Decision != string(domain.ApprovalRejected) {
		writeError(w, http.StatusBadRequest, "decision must be APPROVED or REJECTED")
		return
	}

	existing, err := h.store.GetAutomationAction(r.Context(), actionID)
	if err != nil {
		if errors.Is(err, domain.ErrAutomationActionNotFound) {
			writeError(w, http.StatusNotFound, "automation action not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch automation action")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, AutomationActionDecide) {
		return
	}
	if principalID == existing.ProposedByPrincipalID {
		writeError(w, http.StatusForbidden, domain.ErrSelfApprovalBlocked.Error())
		return
	}

	updated, err := h.store.DecideAutomationAction(r.Context(), actionID, req.Decision, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidDecision) {
			writeError(w, http.StatusConflict, "automation action is not pending a decision")
			return
		}
		h.logger.Error("decide automation action failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to decide automation action")
		return
	}
	_ = h.publisher.Publish(r.Context(), "automation_action.decided", updated.AutomationActionID, updated.TenantID, updated)
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) RegisterModelProvider(w http.ResponseWriter, r *http.Request) {
	var req domain.RegisterModelProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ProviderName == "" || req.ModelName == "" || req.DataRegion == "" {
		writeError(w, http.StatusBadRequest, "provider_name, model_name, and data_region are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, ModelProviderRegister) {
		return
	}

	// doc7 §G6: "No default training use is authorized." — NO_TRAINING unless
	// the caller explicitly asserts a different, verified posture.
	posture := domain.TrainingUseNone
	if req.TrainingUsePosture != "" {
		posture = domain.TrainingUsePosture(req.TrainingUsePosture)
	}
	m := &domain.ModelProviderRegistration{
		ProviderRegistrationID: uuid.NewString(),
		ProviderName:           req.ProviderName,
		ModelName:              req.ModelName,
		TrainingUsePosture:     posture,
		DataRegion:             req.DataRegion,
		DPAVerified:            req.DPAVerified,
		ApprovedDataClasses:    req.ApprovedDataClasses,
		CreatedAt:              time.Now().UTC(),
	}
	if req.RetentionPolicyRef != "" {
		m.RetentionPolicyRef = &req.RetentionPolicyRef
	}
	if req.DPAVerified {
		now := time.Now().UTC()
		m.ApprovedAt = &now
		m.ApprovedByPrincipalID = &principalID
	}
	if err := h.store.RegisterModelProvider(r.Context(), m); err != nil {
		h.logger.Error("register model provider failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to register model provider")
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (h *Handler) GetModelProvider(w http.ResponseWriter, r *http.Request) {
	m, err := h.store.GetModelProvider(r.Context(), chi.URLParam(r, "provider"), chi.URLParam(r, "model"))
	if err != nil {
		if errors.Is(err, domain.ErrModelProviderNotFound) {
			writeError(w, http.StatusNotFound, "model provider registration not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get model provider registration")
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// VerifyModelProvider is doc7 §G6's mandatory pre-production-call check:
// training-use posture, retention, region, DPA, and approved data classes
// must all clear before a real call is made.
func (h *Handler) VerifyModelProvider(w http.ResponseWriter, r *http.Request) {
	dataClass := r.URL.Query().Get("data_class")
	m, err := h.store.GetModelProvider(r.Context(), chi.URLParam(r, "provider"), chi.URLParam(r, "model"))
	if err != nil {
		if errors.Is(err, domain.ErrModelProviderNotFound) {
			writeJSON(w, http.StatusOK, domain.ModelProviderVerification{Eligible: false, Reasons: []string{"provider/model not registered"}})
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to verify model provider")
		return
	}

	var reasons []string
	if m.TrainingUsePosture == domain.TrainingUseAllowed {
		reasons = append(reasons, "training_use_posture is ALLOWED — verify this is an explicit, verified opt-in before proceeding")
	}
	if !m.DPAVerified {
		reasons = append(reasons, "dpa_verified is false")
	}
	if dataClass != "" {
		approved := false
		for _, c := range m.ApprovedDataClasses {
			if c == dataClass {
				approved = true
				break
			}
		}
		if !approved {
			reasons = append(reasons, "data_class "+dataClass+" is not in approved_data_classes")
		}
	}

	eligible := m.DPAVerified && m.TrainingUsePosture != domain.TrainingUseAllowed
	if dataClass != "" {
		for _, reason := range reasons {
			if reason == "data_class "+dataClass+" is not in approved_data_classes" {
				eligible = false
			}
		}
	}
	writeJSON(w, http.StatusOK, domain.ModelProviderVerification{Eligible: eligible, Reasons: reasons})
}

// ProposePolicyChange is doc7 §G3's mandatory drafting step: an AI agent may
// draft a policy/control change, but publication requires this versioned
// change request plus authorized human approval — never a silent apply.
func (h *Handler) ProposePolicyChange(w http.ResponseWriter, r *http.Request) {
	var req domain.ProposePolicyChangeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.TargetPolicyRef == "" || req.ProposedChange == "" {
		writeError(w, http.StatusBadRequest, "target_policy_ref and proposed_change are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, PolicyChangePropose) {
		return
	}

	p := &domain.PolicyChangeApproval{
		PolicyChangeApprovalID: uuid.NewString(),
		TargetPolicyRef:        req.TargetPolicyRef,
		ProposedChange:         req.ProposedChange,
		ProposedByPrincipalID:  principalID,
		Decision:               domain.PolicyChangePending,
		CreatedAt:              time.Now().UTC(),
	}
	if err := h.store.ProposePolicyChange(r.Context(), p); err != nil {
		h.logger.Error("propose policy change failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to propose policy change")
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (h *Handler) GetPolicyChangeApproval(w http.ResponseWriter, r *http.Request) {
	p, err := h.store.GetPolicyChangeApproval(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		if errors.Is(err, domain.ErrPolicyChangeApprovalNotFound) {
			writeError(w, http.StatusNotFound, "policy change approval not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get policy change approval")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// DecidePolicyChange enforces doc7 §G3/§H3's mandatory self-approval block —
// the principal that proposed the change may never be the one who approves
// or rejects it.
func (h *Handler) DecidePolicyChange(w http.ResponseWriter, r *http.Request) {
	approvalID := chi.URLParam(r, "id")
	var req domain.DecidePolicyChangeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Decision != string(domain.PolicyChangeApproved) && req.Decision != string(domain.PolicyChangeRejected) {
		writeError(w, http.StatusBadRequest, "decision must be APPROVED or REJECTED")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, PolicyChangeDecide) {
		return
	}

	updated, err := h.store.DecidePolicyChange(r.Context(), approvalID, req.Decision, principalID, req.Reason)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrPolicyChangeApprovalNotFound):
			writeError(w, http.StatusNotFound, "policy change approval not found")
		case errors.Is(err, domain.ErrSelfApprovalBlocked):
			writeError(w, http.StatusForbidden, err.Error())
		case errors.Is(err, domain.ErrInvalidDecision):
			writeError(w, http.StatusConflict, "policy change approval is not pending a decision")
		default:
			h.logger.Error("decide policy change failed", zap.Error(err))
			writeError(w, http.StatusInternalServerError, "failed to decide policy change")
		}
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
