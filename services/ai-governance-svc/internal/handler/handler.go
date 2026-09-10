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
	"zoiko.io/ai-governance-svc/internal/killswitch"
	"zoiko.io/ai-governance-svc/internal/middleware"
	"zoiko.io/ai-governance-svc/internal/store"
)

// killSwitchDomain is the fixed domain scope this service resolves against
// in kill-switch-registry-svc — AI automation is its own incident-response
// category, distinct from any other plane/domain a future service might
// register under.
const killSwitchDomain = "AI_AUTOMATION"

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
	store      store.Store
	publisher  events.Publisher
	authz      AuthzChecker
	killswitch killswitch.Checker
	logger     *zap.Logger
}

func New(st store.Store, pub events.Publisher, az AuthzChecker, ks killswitch.Checker, logger *zap.Logger) *Handler {
	return &Handler{store: st, publisher: pub, authz: az, killswitch: ks, logger: logger}
}

// resolveBlocked layers kill-switch-registry-svc's live, governed halt on
// top of the store's own resolution, which today can only ever reflect a
// static automation_policies.kill_switch_engaged bool set at policy-create
// time. This is the real incident-response capability that static column
// was standing in for: an operator can halt this tool/tenant's automation
// through a real ENGAGE event, logged and reason-coded, without touching
// policy data at all — and the halt takes effect on the very next resolve,
// not on the next policy edit.
//
// Fails closed: if kill-switch-registry-svc cannot be reached, the
// resolution the store already computed is downgraded to NOT_ALLOWED with
// a distinct reason code, never silently left as the store's own answer.
// An unreachable kill-switch check must never look identical to "checked
// and confirmed not blocked" — the same doctrine already applied to
// privacy-decision-svc's dependency-unavailable handling.
func (h *Handler) resolveBlocked(ctx context.Context, resolved *domain.AutomationPolicyResolution, tool, tenantID string) *domain.AutomationPolicyResolution {
	if !resolved.Allowed {
		return resolved
	}
	if h.killswitch == nil {
		return resolved
	}
	blocked, err := h.killswitch.Resolve(ctx, killSwitchDomain, tool, tenantID)
	if err != nil {
		h.logger.Error("kill-switch-registry-svc unavailable during automation policy resolve", zap.Error(err))
		return &domain.AutomationPolicyResolution{Allowed: false, ReasonCode: "KILL_SWITCH_CHECK_UNAVAILABLE"}
	}
	if blocked {
		return &domain.AutomationPolicyResolution{Allowed: false, ReasonCode: "KILL_SWITCH_ENGAGED"}
	}
	return resolved
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

// requireTenant returns the gateway-verified tenant for the routes that
// touch tenant-scoped tables (ai_runs, automation_policies,
// automation_actions), and refuses the request when there is none.
//
// This is the enforcement point for tenant identity in this service —
// middleware.TenantContext cannot refuse, because the platform-scope
// routes legitimately carry no tenant (see its doc comment).
//
// declared is the tenant_id the caller put in its own request body or
// query string, if any. Before this change the handlers used that value
// directly, so a caller chose which tenant's autonomy allowlist to create,
// resolve against, or attribute an action to — doc7 §G7 requires
// allowlists "per tenant, role, risk class and tool" precisely so that
// agentic execution is "a controlled execution model, not broad delegated
// authority", and a caller-declared tenant is that delegation. The field
// is kept in the API for compatibility but is now only allowed to agree
// with the verified header; disagreement is refused rather than silently
// resolved in either direction.
func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request, declared string) (string, bool) {
	tenantID := middleware.TenantFromContext(r.Context())
	if tenantID == "" {
		writeError(w, http.StatusUnauthorized,
			"X-Tenant-Id is required — the gateway sets it from a verified identity envelope")
		return "", false
	}
	if declared != "" && declared != tenantID {
		writeError(w, http.StatusForbidden,
			"tenant_id in the request does not match the verified X-Tenant-Id")
		return "", false
	}
	return tenantID, true
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
	tenantID, ok := h.requireTenant(w, r, "")
	if !ok {
		return
	}

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

// GetAIRun reads back one AI run, scoped to the caller's tenant. An AI run
// carries model_id, prompt_version, source_refs, evidence_refs and
// recommended_action — the reasoning behind a governed decision and the
// evidence it rested on — so a cross-tenant read here exposes how another
// tenant's decisions were made, not just that they were.
func (h *Handler) GetAIRun(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireTenant(w, r, ""); !ok {
		return
	}
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
	if req.Role == "" || req.Tool == "" || req.ActionType == "" {
		writeError(w, http.StatusBadRequest, "role, tool, and action_type are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	// tenant_id is no longer required in the body, and no longer trusted
	// from it: the allowlist entry is created in the verified tenant.
	tenantID, ok := h.requireTenant(w, r, req.TenantID)
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
		TenantID:             tenantID,
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
//
// The tenant now comes from the verified header, never the query string.
// This is the single most security-relevant read in the service — it
// answers "is this autonomous action allowed" and returns
// kill_switch_engaged — and it used to accept ?tenant_id=, so any caller
// could resolve against another tenant's allowlist and read whether their
// kill switch was engaged. A ?tenant_id= that disagrees with the header is
// refused rather than ignored, so a caller that means to ask about
// another tenant gets an error instead of a quietly reinterpreted answer.
func (h *Handler) ResolveAutomationPolicy(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	role, riskCategory, tool, actionType := q.Get("role"), q.Get("risk_category"), q.Get("tool"), q.Get("action_type")
	if role == "" || tool == "" || actionType == "" {
		writeError(w, http.StatusBadRequest, "role, tool, and action_type query params are required")
		return
	}
	tenantID, ok := h.requireTenant(w, r, q.Get("tenant_id"))
	if !ok {
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
	resolved = h.resolveBlocked(r.Context(), resolved, tool, tenantID)
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
	if req.ActionType == "" || req.Role == "" || req.Tool == "" || req.IdempotencyKey == "" {
		writeError(w, http.StatusBadRequest, "action_type, role, tool, and idempotency_key are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r, req.TenantID)
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

	resolution, err := h.store.ResolveAutomationPolicy(r.Context(), tenantID, req.Role, string(riskCategory), req.Tool, req.ActionType)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to resolve automation policy")
		return
	}
	resolution = h.resolveBlocked(r.Context(), resolution, req.Tool, tenantID)
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
		TenantID:              tenantID,
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
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "automation_action.proposed", EntityID: a.AutomationActionID, TenantID: a.TenantID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: a,
	})
	writeJSON(w, http.StatusCreated, a)
}

func (h *Handler) GetAutomationAction(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireTenant(w, r, ""); !ok {
		return
	}
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
//
// This is the most consequential write in the service: approving here is
// what authorizes an autonomous action to execute. It is now tenant-scoped
// at three layers — this handler requires a verified tenant, the
// GetAutomationAction fetch below is tenant-scoped so a foreign action id
// 404s before any decision is reached, and the UPDATE itself carries the
// tenant predicate so it does not depend on that ordering holding.
func (h *Handler) DecideAutomationAction(w http.ResponseWriter, r *http.Request) {
	actionID := chi.URLParam(r, "id")
	if _, ok := h.requireTenant(w, r, ""); !ok {
		return
	}
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
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "automation_action.decided", EntityID: updated.AutomationActionID, TenantID: updated.TenantID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: updated,
	})
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
