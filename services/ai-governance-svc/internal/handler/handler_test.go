package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/ai-governance-svc/internal/domain"
	"zoiko.io/ai-governance-svc/internal/events"
	"zoiko.io/ai-governance-svc/internal/store"
)

type stubStore struct {
	aiRuns          map[string]*domain.AIRun
	classifications map[string]*domain.ActionRiskClassification
	policies        map[string]*domain.AutomationPolicy // keyed by tenant|role|risk|tool|action
	actions         map[string]*domain.AutomationAction
	idempotency     map[string]string                            // tenant|key -> automation_action_id
	providers       map[string]*domain.ModelProviderRegistration // keyed by provider|model
	policyChanges   map[string]*domain.PolicyChangeApproval
}

func newStubStore() *stubStore {
	return &stubStore{
		aiRuns:          make(map[string]*domain.AIRun),
		classifications: make(map[string]*domain.ActionRiskClassification),
		policies:        make(map[string]*domain.AutomationPolicy),
		actions:         make(map[string]*domain.AutomationAction),
		idempotency:     make(map[string]string),
		providers:       make(map[string]*domain.ModelProviderRegistration),
		policyChanges:   make(map[string]*domain.PolicyChangeApproval),
	}
}

func (s *stubStore) CreateAIRun(_ context.Context, a *domain.AIRun) error {
	s.aiRuns[a.AIRunID] = a
	return nil
}

func (s *stubStore) GetAIRun(_ context.Context, id string) (*domain.AIRun, error) {
	if a, ok := s.aiRuns[id]; ok {
		return a, nil
	}
	return nil, domain.ErrAIRunNotFound
}

func (s *stubStore) SetActionRiskClassification(_ context.Context, c *domain.ActionRiskClassification) error {
	s.classifications[c.ActionType] = c
	return nil
}

func (s *stubStore) GetActionRiskClassification(_ context.Context, actionType string) (*domain.ActionRiskClassification, error) {
	if c, ok := s.classifications[actionType]; ok {
		return c, nil
	}
	return nil, domain.ErrActionRiskClassificationNotFound
}

func policyKey(tenantID, role, riskCategory, tool, actionType string) string {
	return tenantID + "|" + role + "|" + riskCategory + "|" + tool + "|" + actionType
}

func (s *stubStore) CreateAutomationPolicy(_ context.Context, p *domain.AutomationPolicy) error {
	key := policyKey(p.TenantID, p.Role, string(p.RiskCategory), p.Tool, p.ActionType)
	if _, exists := s.policies[key]; exists {
		return domain.ErrConflict
	}
	s.policies[key] = p
	return nil
}

func (s *stubStore) ResolveAutomationPolicy(_ context.Context, tenantID, role, riskCategory, tool, actionType string) (*domain.AutomationPolicyResolution, error) {
	p, ok := s.policies[policyKey(tenantID, role, riskCategory, tool, actionType)]
	if !ok {
		return &domain.AutomationPolicyResolution{Allowed: false, ReasonCode: "NOT_ALLOWLISTED"}, nil
	}
	if p.KillSwitchEngaged {
		return &domain.AutomationPolicyResolution{Allowed: false, ReasonCode: "KILL_SWITCH_ENGAGED"}, nil
	}
	return &domain.AutomationPolicyResolution{Allowed: true, ReasonCode: "ALLOWED"}, nil
}

func (s *stubStore) ProposeAutomationAction(_ context.Context, a *domain.AutomationAction) error {
	idemKey := a.TenantID + "|" + a.IdempotencyKey
	if _, exists := s.idempotency[idemKey]; exists {
		return domain.ErrDuplicateIdempotencyKey
	}
	s.idempotency[idemKey] = a.AutomationActionID
	s.actions[a.AutomationActionID] = a
	return nil
}

func (s *stubStore) GetAutomationAction(_ context.Context, id string) (*domain.AutomationAction, error) {
	if a, ok := s.actions[id]; ok {
		return a, nil
	}
	return nil, domain.ErrAutomationActionNotFound
}

func (s *stubStore) DecideAutomationAction(_ context.Context, id, decision, deciderPrincipalID string) (*domain.AutomationAction, error) {
	a, ok := s.actions[id]
	if !ok {
		return nil, domain.ErrAutomationActionNotFound
	}
	if a.ApprovalStatus != domain.ApprovalPending {
		return nil, domain.ErrInvalidDecision
	}
	a.ApprovalStatus = domain.ApprovalStatus(decision)
	if decision == string(domain.ApprovalRejected) {
		a.Status = domain.AutomationActionRejected
	} else {
		a.Status = domain.AutomationActionApproved
	}
	a.ApprovedByPrincipalID = &deciderPrincipalID
	return a, nil
}

func (s *stubStore) RegisterModelProvider(_ context.Context, m *domain.ModelProviderRegistration) error {
	s.providers[m.ProviderName+"|"+m.ModelName] = m
	return nil
}

func (s *stubStore) GetModelProvider(_ context.Context, provider, model string) (*domain.ModelProviderRegistration, error) {
	if m, ok := s.providers[provider+"|"+model]; ok {
		return m, nil
	}
	return nil, domain.ErrModelProviderNotFound
}

func (s *stubStore) ProposePolicyChange(_ context.Context, p *domain.PolicyChangeApproval) error {
	s.policyChanges[p.PolicyChangeApprovalID] = p
	return nil
}

func (s *stubStore) GetPolicyChangeApproval(_ context.Context, id string) (*domain.PolicyChangeApproval, error) {
	if p, ok := s.policyChanges[id]; ok {
		return p, nil
	}
	return nil, domain.ErrPolicyChangeApprovalNotFound
}

func (s *stubStore) DecidePolicyChange(_ context.Context, id, decision, decidedByPrincipalID, reason string) (*domain.PolicyChangeApproval, error) {
	p, ok := s.policyChanges[id]
	if !ok {
		return nil, domain.ErrPolicyChangeApprovalNotFound
	}
	if p.ProposedByPrincipalID == decidedByPrincipalID {
		return nil, domain.ErrSelfApprovalBlocked
	}
	if p.Decision != domain.PolicyChangePending {
		return nil, domain.ErrInvalidDecision
	}
	p.Decision = domain.PolicyChangeDecision(decision)
	p.DecidedByPrincipalID = &decidedByPrincipalID
	if reason != "" {
		p.DecisionReason = &reason
	}
	return p, nil
}

var _ store.Store = (*stubStore)(nil)

type stubPublisher struct{}

func (p *stubPublisher) Publish(_ context.Context, _ events.PublishParams) error { return nil }

var _ events.Publisher = (*stubPublisher)(nil)

type stubAuthz struct{ err error }

func (s *stubAuthz) CheckAllowed(_ context.Context, _, _, _ string) error { return s.err }

var _ AuthzChecker = (*stubAuthz)(nil)

func newTestHandler() *Handler {
	logger, _ := zap.NewDevelopment()
	return New(newStubStore(), &stubPublisher{}, &stubAuthz{}, logger)
}

func newTestRouter(h *Handler) *chi.Mux {
	r := chi.NewRouter()
	RegisterRoutes(r, h)
	return r
}

func buildRequest(method, path string, body interface{}) *http.Request {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	r := httptest.NewRequest(method, path, &buf)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Principal-Id", "principal-test-01")
	return r
}

func TestCreateAndGetAIRun(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/ai-runs", domain.CreateAIRunRequest{
		RunType:       "RECOMMEND",
		ModelID:       "claude-5",
		PromptVersion: "v3",
		AuditID:       "audit-001",
		Confidence:    float64Ptr(0.82),
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — %s", w.Code, w.Body.String())
	}
	var run domain.AIRun
	_ = json.NewDecoder(w.Body).Decode(&run)
	if run.UncertaintyState != domain.UncertaintyNone {
		t.Fatalf("expected default uncertainty_state NONE, got %s", run.UncertaintyState)
	}

	wGet := httptest.NewRecorder()
	r.ServeHTTP(wGet, buildRequest(http.MethodGet, "/v1/ai-runs/"+run.AIRunID, nil))
	if wGet.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", wGet.Code)
	}
}

func float64Ptr(v float64) *float64 { return &v }

func TestProposeAutomationAction_BlockedWhenNotAllowlisted(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/automation-actions", domain.ProposeAutomationActionRequest{
		TenantID:       "tenant-1",
		ActionType:     "SEND_REFUND",
		Role:           "billing-agent",
		Tool:           "stripe-refund-tool",
		IdempotencyKey: "idem-1",
	}))
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when no automation policy allowlists this action, got %d — %s", w.Code, w.Body.String())
	}
}

func TestProposeAutomationAction_AllowedThenMakerCheckerBlocksSelfApproval(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)

	// Classify SEND_REFUND as MONEY risk requiring maker-checker (doc7 §G2).
	wClass := httptest.NewRecorder()
	r.ServeHTTP(wClass, buildRequest(http.MethodPost, "/v1/action-risk-classifications", domain.SetActionRiskClassificationRequest{
		ActionType:           "SEND_REFUND",
		RiskCategory:         "MONEY",
		HumanReviewTrigger:   true,
		RequiresMakerChecker: true,
	}))
	if wClass.Code != http.StatusOK {
		t.Fatalf("expected 200 classifying action, got %d — %s", wClass.Code, wClass.Body.String())
	}

	// Allowlist it for tenant-1/billing-agent/stripe-refund-tool.
	wPolicy := httptest.NewRecorder()
	r.ServeHTTP(wPolicy, buildRequest(http.MethodPost, "/v1/automation-policies", domain.CreateAutomationPolicyRequest{
		TenantID:          "tenant-1",
		Role:              "billing-agent",
		RiskCategory:      "MONEY",
		Tool:              "stripe-refund-tool",
		ActionType:        "SEND_REFUND",
		RequiredApprovals: 1,
	}))
	if wPolicy.Code != http.StatusCreated {
		t.Fatalf("expected 201 creating automation policy, got %d — %s", wPolicy.Code, wPolicy.Body.String())
	}

	wPropose := httptest.NewRecorder()
	r.ServeHTTP(wPropose, buildRequest(http.MethodPost, "/v1/automation-actions", domain.ProposeAutomationActionRequest{
		TenantID:       "tenant-1",
		ActionType:     "SEND_REFUND",
		Role:           "billing-agent",
		Tool:           "stripe-refund-tool",
		IdempotencyKey: "idem-2",
	}))
	if wPropose.Code != http.StatusCreated {
		t.Fatalf("expected 201 proposing automation action, got %d — %s", wPropose.Code, wPropose.Body.String())
	}
	var action domain.AutomationAction
	_ = json.NewDecoder(wPropose.Body).Decode(&action)
	if action.ApprovalStatus != domain.ApprovalPending {
		t.Fatalf("expected PENDING approval since RequiresMakerChecker=true, got %s", action.ApprovalStatus)
	}

	// The default test principal ("principal-test-01") proposed it — the
	// same principal attempting to decide it must be blocked.
	wSelfApprove := httptest.NewRecorder()
	r.ServeHTTP(wSelfApprove, buildRequest(http.MethodPost, "/v1/automation-actions/"+action.AutomationActionID+"/decision", domain.ApproveAutomationActionRequest{
		Decision: "APPROVED",
	}))
	if wSelfApprove.Code != http.StatusForbidden {
		t.Fatalf("expected 403 blocking self-approval, got %d — %s", wSelfApprove.Code, wSelfApprove.Body.String())
	}

	// A different principal deciding must succeed.
	req := buildRequest(http.MethodPost, "/v1/automation-actions/"+action.AutomationActionID+"/decision", domain.ApproveAutomationActionRequest{
		Decision: "APPROVED",
	})
	req.Header.Set("X-Principal-Id", "principal-checker-02")
	wApprove := httptest.NewRecorder()
	r.ServeHTTP(wApprove, req)
	if wApprove.Code != http.StatusOK {
		t.Fatalf("expected 200 from a different approver, got %d — %s", wApprove.Code, wApprove.Body.String())
	}
	var updated domain.AutomationAction
	_ = json.NewDecoder(wApprove.Body).Decode(&updated)
	if updated.ApprovalStatus != domain.ApprovalApproved {
		t.Fatalf("expected APPROVED, got %s", updated.ApprovalStatus)
	}
}

func TestModelProviderVerify_BlocksUnapprovedDataClassAndUnverifiedDPA(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)

	wReg := httptest.NewRecorder()
	r.ServeHTTP(wReg, buildRequest(http.MethodPost, "/v1/model-providers", domain.RegisterModelProviderRequest{
		ProviderName:        "anthropic",
		ModelName:           "claude-5",
		DataRegion:          "us",
		DPAVerified:         true,
		ApprovedDataClasses: []string{"support_tickets"},
	}))
	if wReg.Code != http.StatusOK {
		t.Fatalf("expected 200 registering provider, got %d — %s", wReg.Code, wReg.Body.String())
	}

	wOK := httptest.NewRecorder()
	r.ServeHTTP(wOK, buildRequest(http.MethodGet, "/v1/model-providers/anthropic/claude-5/verify?data_class=support_tickets", nil))
	var okResult domain.ModelProviderVerification
	_ = json.NewDecoder(wOK.Body).Decode(&okResult)
	if !okResult.Eligible {
		t.Fatalf("expected eligible=true for an approved data class, got %+v", okResult)
	}

	wBlocked := httptest.NewRecorder()
	r.ServeHTTP(wBlocked, buildRequest(http.MethodGet, "/v1/model-providers/anthropic/claude-5/verify?data_class=payroll_records", nil))
	var blockedResult domain.ModelProviderVerification
	_ = json.NewDecoder(wBlocked.Body).Decode(&blockedResult)
	if blockedResult.Eligible {
		t.Fatalf("expected eligible=false for an unapproved data class, got %+v", blockedResult)
	}
}

func TestPolicyChangeApproval_BlocksSelfApproval(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)

	wPropose := httptest.NewRecorder()
	r.ServeHTTP(wPropose, buildRequest(http.MethodPost, "/v1/policy-change-approvals", domain.ProposePolicyChangeRequest{
		TargetPolicyRef: "policy-svc:obligation-rule-42",
		ProposedChange:  "widen the auto-approval threshold to $5000",
	}))
	if wPropose.Code != http.StatusCreated {
		t.Fatalf("expected 201 proposing policy change, got %d — %s", wPropose.Code, wPropose.Body.String())
	}
	var change domain.PolicyChangeApproval
	_ = json.NewDecoder(wPropose.Body).Decode(&change)

	wSelf := httptest.NewRecorder()
	r.ServeHTTP(wSelf, buildRequest(http.MethodPost, "/v1/policy-change-approvals/"+change.PolicyChangeApprovalID+"/decision", domain.DecidePolicyChangeRequest{
		Decision: "APPROVED",
	}))
	if wSelf.Code != http.StatusForbidden {
		t.Fatalf("expected 403 blocking self-approval, got %d — %s", wSelf.Code, wSelf.Body.String())
	}

	req := buildRequest(http.MethodPost, "/v1/policy-change-approvals/"+change.PolicyChangeApprovalID+"/decision", domain.DecidePolicyChangeRequest{
		Decision: "APPROVED",
	})
	req.Header.Set("X-Principal-Id", "principal-checker-02")
	wOther := httptest.NewRecorder()
	r.ServeHTTP(wOther, req)
	if wOther.Code != http.StatusOK {
		t.Fatalf("expected 200 from a different approver, got %d — %s", wOther.Code, wOther.Body.String())
	}
}
