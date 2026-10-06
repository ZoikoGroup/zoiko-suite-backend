package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/ai-governance-svc/internal/authz"
	"zoiko.io/ai-governance-svc/internal/domain"
	"zoiko.io/ai-governance-svc/internal/events"
	"zoiko.io/ai-governance-svc/internal/killswitch"
	"zoiko.io/ai-governance-svc/internal/middleware"
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

	// AIG-01/AIG-02: in-memory, forward-only state machines mirroring
	// PgStore's precondition checks (internal/store/pg_store.go) closely
	// enough to exercise the handler's event-publishing paths, including
	// rejecting an illegal transition exactly as PgStore does. Full
	// state-machine fidelity (migration triggers, RLS, row locking) is
	// covered separately by internal/store/aig01_aig02_test.go against a
	// real Postgres instance — this stub's only job is to let handler
	// tests observe what gets published, not to re-prove the state
	// machine itself.
	useCases      map[string]*domain.AIUseCase
	useCaseReplay map[string]string // tenant|client_request_id -> use_case_id
	assessments   map[string]*domain.AIImpactAssessment
	modelReleases map[string]*domain.AIModelRelease
	executions    map[string]*domain.AIExecution
	executionKeys map[string]string
	dispositions    map[string]*domain.AIOutputDisposition
	dispositionKeys map[string]string
	seq           int
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
		useCases:        make(map[string]*domain.AIUseCase),
		useCaseReplay:   make(map[string]string),
		assessments:     make(map[string]*domain.AIImpactAssessment),
		modelReleases:   make(map[string]*domain.AIModelRelease),
		executions:      make(map[string]*domain.AIExecution),
		executionKeys:   make(map[string]string),
		dispositions:    make(map[string]*domain.AIOutputDisposition),
		dispositionKeys: make(map[string]string),
	}
}

func (s *stubStore) nextID(prefix string) string {
	s.seq++
	return fmt.Sprintf("%s-%04d", prefix, s.seq)
}

// The tenant-scoped methods below honour the context tenant exactly as
// PgStore does. That is not gold-plating a fake: a stub that ignores the
// tenant cannot fail an isolation assertion, so every handler test made
// against it would pass no matter what the handlers did. The platform-scope
// methods (classifications, providers, policy changes) correctly ignore it,
// because those tables carry no tenant_id.

func (s *stubStore) CreateAIRun(_ context.Context, a *domain.AIRun) error {
	s.aiRuns[a.AIRunID] = a
	return nil
}

func (s *stubStore) GetAIRun(ctx context.Context, id string) (*domain.AIRun, error) {
	if a, ok := s.aiRuns[id]; ok && a.TenantID == middleware.TenantFromContext(ctx) {
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

// ResolveAutomationPolicy resolves against the CONTEXT tenant, ignoring
// the tenantID argument if it disagrees — mirroring RLS, which is what
// stops a foreign tenant id in this parameter from resolving against
// someone else's allowlist.
func (s *stubStore) ResolveAutomationPolicy(ctx context.Context, tenantID, role, riskCategory, tool, actionType string) (*domain.AutomationPolicyResolution, error) {
	if ctxTenant := middleware.TenantFromContext(ctx); ctxTenant != "" {
		tenantID = ctxTenant
	}
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

func (s *stubStore) GetAutomationAction(ctx context.Context, id string) (*domain.AutomationAction, error) {
	if a, ok := s.actions[id]; ok && a.TenantID == middleware.TenantFromContext(ctx) {
		return a, nil
	}
	return nil, domain.ErrAutomationActionNotFound
}

func (s *stubStore) DecideAutomationAction(ctx context.Context, id, decision, deciderPrincipalID string) (*domain.AutomationAction, error) {
	a, ok := s.actions[id]
	if !ok || a.TenantID != middleware.TenantFromContext(ctx) {
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

func (s *stubStore) ListActionRiskClassifications(_ context.Context) ([]domain.ActionRiskClassification, error) {
	var list []domain.ActionRiskClassification
	for _, c := range s.classifications {
		list = append(list, *c)
	}
	return list, nil
}

func (s *stubStore) ListAutomationPolicies(ctx context.Context) ([]domain.AutomationPolicy, error) {
	tenantID := middleware.TenantFromContext(ctx)
	var list []domain.AutomationPolicy
	for _, p := range s.policies {
		if p.TenantID == tenantID {
			list = append(list, *p)
		}
	}
	return list, nil
}

func (s *stubStore) ListAutomationActions(ctx context.Context) ([]domain.AutomationAction, error) {
	tenantID := middleware.TenantFromContext(ctx)
	var list []domain.AutomationAction
	for _, a := range s.actions {
		if a.TenantID == tenantID {
			list = append(list, *a)
		}
	}
	return list, nil
}

func (s *stubStore) ListModelProviders(_ context.Context) ([]domain.ModelProviderRegistration, error) {
	var list []domain.ModelProviderRegistration
	for _, m := range s.providers {
		list = append(list, *m)
	}
	return list, nil
}

func (s *stubStore) ListPolicyChangeApprovals(_ context.Context) ([]domain.PolicyChangeApproval, error) {
	var list []domain.PolicyChangeApproval
	for _, p := range s.policyChanges {
		list = append(list, *p)
	}
	return list, nil
}

func (s *stubStore) CreateUseCase(_ context.Context, req domain.CreateUseCaseRequest, tenantID, actor, clientRequestID string) (*domain.AIUseCase, error) {
	if clientRequestID != "" {
		if existingID, ok := s.useCaseReplay[tenantID+"|"+clientRequestID]; ok {
			return s.useCases[existingID], nil
		}
	}
	now := time.Now().UTC()
	uc := &domain.AIUseCase{
		UseCaseID:              s.nextID("usecase"),
		TenantID:               tenantID,
		Domain:                 req.Domain,
		Purpose:                req.Purpose,
		OutcomeType:            req.OutcomeType,
		OperationalClass:       domain.OperationalClass(req.OperationalClass),
		LegalClassificationRef: req.LegalClassificationRef,
		OwnerPrincipalID:       req.OwnerPrincipalID,
		BusinessOutcome:        req.BusinessOutcome,
		AffectedDecisions:      req.AffectedDecisions,
		DataProfile:            req.DataProfile,
		AutomationLevel:        domain.AutomationLevel(req.AutomationLevel),
		HumanRole:              req.HumanRole,
		Fallback:               req.Fallback,
		SuccessMeasures:        req.SuccessMeasures,
		ProhibitedBoundary:     req.ProhibitedBoundary,
		RetirementCriteria:     req.RetirementCriteria,
		LifecycleState:         domain.UseCaseDraft,
		CreatedAt:              now,
		CreatedByPrincipalID:   actor,
		UpdatedAt:              now,
	}
	s.useCases[uc.UseCaseID] = uc
	if clientRequestID != "" {
		s.useCaseReplay[tenantID+"|"+clientRequestID] = uc.UseCaseID
	}
	return uc, nil
}

func (s *stubStore) GetUseCase(ctx context.Context, id string) (*domain.AIUseCase, error) {
	uc, ok := s.useCases[id]
	if !ok || uc.TenantID != middleware.TenantFromContext(ctx) {
		return nil, domain.ErrUseCaseNotFound
	}
	return uc, nil
}

// StartAssessment mirrors PgStore.StartAssessment's exact precondition:
// valid from DRAFT, APPROVED, ACTIVE, LIMITED or SUSPENDED.
func (s *stubStore) StartAssessment(ctx context.Context, useCaseID string, req domain.StartAssessmentRequest, actor string) (*domain.AIImpactAssessment, error) {
	uc, ok := s.useCases[useCaseID]
	if !ok || uc.TenantID != middleware.TenantFromContext(ctx) {
		return nil, domain.ErrUseCaseNotFound
	}
	switch uc.LifecycleState {
	case domain.UseCaseDraft, domain.UseCaseApproved, domain.UseCaseActive, domain.UseCaseLimited, domain.UseCaseSuspended:
	default:
		return nil, domain.ErrUseCaseNotAssessable
	}
	now := time.Now().UTC()
	a := &domain.AIImpactAssessment{
		AssessmentID:         s.nextID("assess"),
		UseCaseID:            useCaseID,
		TenantID:             uc.TenantID,
		AffectedGroups:       req.AffectedGroups,
		RightsImpact:         req.RightsImpact,
		FinancialImpact:      req.FinancialImpact,
		EmploymentImpact:     req.EmploymentImpact,
		Mitigations:          req.Mitigations,
		Approvers:            req.Approvers,
		Decision:             domain.AssessmentPending,
		ExpiresAt:            req.ExpiresAt,
		CreatedAt:            now,
		CreatedByPrincipalID: actor,
	}
	s.assessments[a.AssessmentID] = a
	uc.LifecycleState = domain.UseCaseAssessing
	uc.UpdatedAt = now
	return a, nil
}

func (s *stubStore) DecideAssessment(_ context.Context, assessmentID, decision, decidedByPrincipalID, reason string) (*domain.AIImpactAssessment, error) {
	if decision != string(domain.AssessmentApproved) && decision != string(domain.AssessmentRejected) {
		return nil, domain.ErrInvalidDecision
	}
	a, ok := s.assessments[assessmentID]
	if !ok {
		return nil, domain.ErrAssessmentNotFound
	}
	if a.Decision != domain.AssessmentPending {
		return nil, domain.ErrAssessmentNotPending
	}
	now := time.Now().UTC()
	a.Decision = domain.AssessmentDecision(decision)
	a.DecidedByPrincipalID = &decidedByPrincipalID
	if reason != "" {
		a.DecisionReason = &reason
	}
	a.DecidedAt = &now
	if uc, ok := s.useCases[a.UseCaseID]; ok {
		if decision == string(domain.AssessmentRejected) {
			uc.LifecycleState = domain.UseCaseRejected
		} else {
			uc.LifecycleState = domain.UseCaseApproved
		}
		uc.UpdatedAt = now
	}
	return a, nil
}

// ActivateUseCase mirrors PgStore.ActivateUseCase's exact gates: only
// from APPROVED/LIMITED, A4 always prohibited, high-impact classes need
// a resolved legal classification, and the latest assessment must be an
// unexpired APPROVED.
func (s *stubStore) ActivateUseCase(ctx context.Context, useCaseID string, req domain.ActivateUseCaseRequest) (*domain.AIUseCase, error) {
	uc, ok := s.useCases[useCaseID]
	if !ok || uc.TenantID != middleware.TenantFromContext(ctx) {
		return nil, domain.ErrUseCaseNotFound
	}
	if uc.LifecycleState != domain.UseCaseApproved && uc.LifecycleState != domain.UseCaseLimited {
		return nil, domain.ErrUseCaseNotActivatable
	}
	if uc.OperationalClass == domain.OperationalClassA4 {
		return nil, domain.ErrOperationalClassProhibited
	}
	if uc.OperationalClass.IsPotentiallyHighImpact() &&
		(uc.LegalClassificationRef == "" || uc.LegalClassificationRef == "INDETERMINATE") {
		return nil, domain.ErrLegalClassificationIndeterminate
	}
	latest := s.latestAssessmentForUseCase(useCaseID)
	if latest == nil || latest.Decision != domain.AssessmentApproved {
		return nil, domain.ErrAssessmentNotApproved
	}
	if latest.ExpiresAt != nil && latest.ExpiresAt.Before(time.Now().UTC()) {
		return nil, domain.ErrAssessmentExpired
	}
	if req.Limited {
		uc.LifecycleState = domain.UseCaseLimited
	} else {
		uc.LifecycleState = domain.UseCaseActive
	}
	uc.UpdatedAt = time.Now().UTC()
	return uc, nil
}

func (s *stubStore) latestAssessmentForUseCase(useCaseID string) *domain.AIImpactAssessment {
	var latest *domain.AIImpactAssessment
	for _, a := range s.assessments {
		if a.UseCaseID != useCaseID {
			continue
		}
		if latest == nil || a.CreatedAt.After(latest.CreatedAt) {
			latest = a
		}
	}
	return latest
}

func (s *stubStore) SuspendUseCase(ctx context.Context, useCaseID string, req domain.SuspendUseCaseRequest) (*domain.AIUseCase, error) {
	uc, ok := s.useCases[useCaseID]
	if !ok || uc.TenantID != middleware.TenantFromContext(ctx) {
		return nil, domain.ErrUseCaseNotFound
	}
	if uc.LifecycleState != domain.UseCaseActive && uc.LifecycleState != domain.UseCaseLimited {
		return nil, domain.ErrUseCaseNotSuspendable
	}
	uc.LifecycleState = domain.UseCaseSuspended
	uc.UpdatedAt = time.Now().UTC()
	return uc, nil
}

func (s *stubStore) RequestReassessment(ctx context.Context, useCaseID string, req domain.RequestReassessmentRequest) (*domain.AIUseCase, error) {
	uc, ok := s.useCases[useCaseID]
	if !ok || uc.TenantID != middleware.TenantFromContext(ctx) {
		return nil, domain.ErrUseCaseNotFound
	}
	switch uc.LifecycleState {
	case domain.UseCaseApproved, domain.UseCaseActive, domain.UseCaseLimited, domain.UseCaseSuspended:
	default:
		return nil, domain.ErrUseCaseNotReassessable
	}
	uc.LifecycleState = domain.UseCaseAssessing
	uc.UpdatedAt = time.Now().UTC()
	return uc, nil
}

func (s *stubStore) RetireUseCase(ctx context.Context, useCaseID string, req domain.RetireUseCaseRequest) (*domain.AIUseCase, error) {
	uc, ok := s.useCases[useCaseID]
	if !ok || uc.TenantID != middleware.TenantFromContext(ctx) {
		return nil, domain.ErrUseCaseNotFound
	}
	switch uc.LifecycleState {
	case domain.UseCaseActive, domain.UseCaseLimited, domain.UseCaseSuspended:
	default:
		return nil, domain.ErrUseCaseNotRetirable
	}
	uc.LifecycleState = domain.UseCaseRetired
	uc.UpdatedAt = time.Now().UTC()
	return uc, nil
}

func (s *stubStore) GetEffectiveUseCaseControl(ctx context.Context, useCaseID string) (*domain.EffectiveUseCaseControl, error) {
	uc, ok := s.useCases[useCaseID]
	if !ok || uc.TenantID != middleware.TenantFromContext(ctx) {
		return nil, domain.ErrUseCaseNotFound
	}
	return &domain.EffectiveUseCaseControl{UseCase: *uc, LatestAssessment: s.latestAssessmentForUseCase(useCaseID)}, nil
}

func (s *stubStore) ListUseCases(ctx context.Context) ([]domain.AIUseCase, error) {
	tenantID := middleware.TenantFromContext(ctx)
	list := []domain.AIUseCase{}
	for _, uc := range s.useCases {
		if uc.TenantID == tenantID {
			list = append(list, *uc)
		}
	}
	return list, nil
}

func (s *stubStore) CreateExecution(ctx context.Context, req domain.CreateExecutionRequest, key, requestHash string) (*domain.AIExecution, bool, error) {
	tenantID := middleware.TenantFromContext(ctx)
	lookup := tenantID + "|" + key
	if existingID, ok := s.executionKeys[lookup]; ok {
		existing := s.executions[existingID]
		if existing.RequestSHA256 != requestHash {
			return nil, false, domain.ErrIdempotencyConflict
		}
		return existing, true, nil
	}
	execution := &domain.AIExecution{
		ExecutionID: s.nextID("execution"), TenantID: tenantID,
		UseCaseID: req.UseCaseID, ModelReleaseID: req.ModelReleaseID,
		PackageID: req.PackageID, PackageVersion: req.PackageVersion,
		RequestSHA256: requestHash, Status: "BLOCKED",
		BlockReason: "GOVERNANCE_DEPENDENCY_UNAVAILABLE",
		BlockedBy:   []string{"IAM", "COM", "PRV", "PDC", "XIC"},
		CreatedAt:   time.Now().UTC(), CreatedByPrincipalID: middleware.PrincipalFromContext(ctx),
	}
	s.executionKeys[lookup] = execution.ExecutionID
	s.executions[execution.ExecutionID] = execution
	return execution, false, nil
}

func (s *stubStore) GetExecution(ctx context.Context, id string) (*domain.AIExecution, error) {
	execution, ok := s.executions[id]
	if !ok || execution.TenantID != middleware.TenantFromContext(ctx) {
		return nil, domain.ErrExecutionNotFound
	}
	return execution, nil
}

func (s *stubStore) CreateAIIncident(ctx context.Context, req domain.CreateAIIncidentRequest, key, requestHash string) (*domain.AIIncident, bool, error) {
	return &domain.AIIncident{
		IncidentID: s.nextID("incident"), TenantID: middleware.TenantFromContext(ctx),
		Severity: req.Severity, ModelReleaseID: req.ModelReleaseID, Description: req.Description,
		EvidenceReferences: req.EvidenceReferences, Status: "OPEN",
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: middleware.PrincipalFromContext(ctx),
	}, false, nil
}

func (s *stubStore) CreateOutputDisposition(ctx context.Context, req domain.CreateOutputDispositionRequest, key, requestHash string) (*domain.AIOutputDisposition, bool, error) {
	if req.OversightClass == string(domain.OversightProhibited) {
		return nil, false, domain.ErrOversightClassProhibited
	}
	if _, ok := s.aiRuns[req.AIRunID]; !ok {
		return nil, false, domain.ErrAIRunNotFound
	}
	tenantID := middleware.TenantFromContext(ctx)
	lookup := tenantID + "|" + key
	if existingID, ok := s.dispositionKeys[lookup]; ok {
		existing := s.dispositions[existingID]
		if existing.RequestSHA256 != requestHash {
			return nil, false, domain.ErrIdempotencyConflict
		}
		return existing, true, nil
	}
	for _, d := range s.dispositions {
		if d.AIRunID == req.AIRunID {
			return nil, false, domain.ErrAIRunAlreadyHasDisposition
		}
	}
	status := string(domain.DispositionReviewRequired)
	if req.OversightClass == string(domain.OversightNone) {
		status = string(domain.DispositionDraftAssistive)
	}
	disposition := &domain.AIOutputDisposition{
		DispositionID: s.nextID("disposition"), TenantID: tenantID,
		AIRunID: req.AIRunID, OversightClass: req.OversightClass, Status: status,
		RequestSHA256: requestHash, CreatedAt: time.Now().UTC(),
		CreatedByPrincipalID: middleware.PrincipalFromContext(ctx),
	}
	s.dispositionKeys[lookup] = disposition.DispositionID
	s.dispositions[disposition.DispositionID] = disposition
	return disposition, false, nil
}

func (s *stubStore) GetOutputDisposition(ctx context.Context, id string) (*domain.AIOutputDisposition, error) {
	disposition, ok := s.dispositions[id]
	if !ok || disposition.TenantID != middleware.TenantFromContext(ctx) {
		return nil, domain.ErrDispositionNotFound
	}
	return disposition, nil
}

func (s *stubStore) DecideOutputDisposition(ctx context.Context, id string, req domain.DecideOutputDispositionRequest) (*domain.AIOutputDisposition, error) {
	disposition, err := s.GetOutputDisposition(ctx, id)
	if err != nil {
		return nil, err
	}
	if disposition.Status != string(domain.DispositionReviewRequired) {
		return nil, domain.ErrDispositionNotReviewable
	}
	decider := middleware.PrincipalFromContext(ctx)
	if disposition.CreatedByPrincipalID == decider {
		return nil, domain.ErrSelfApprovalBlocked
	}
	now := time.Now().UTC()
	disposition.Status = req.Decision
	disposition.DecidedByPrincipalID = &decider
	disposition.DecidedAt = &now
	if req.Reason != "" {
		disposition.Reason = &req.Reason
	}
	return disposition, nil
}

func (s *stubStore) RegisterModelRelease(_ context.Context, req domain.RegisterModelReleaseRequest, actor string) (*domain.AIModelRelease, error) {
	now := time.Now().UTC()
	trainingUse := domain.TrainingUse(req.TrainingUse)
	if trainingUse == "" {
		trainingUse = domain.TrainingUseNoTraining
	}
	m := &domain.AIModelRelease{
		ModelReleaseID:       s.nextID("release"),
		Provider:             req.Provider,
		ProviderModelID:      req.ProviderModelID,
		DeploymentRegion:     req.DeploymentRegion,
		CapabilitySet:        req.CapabilitySet,
		ContextLimit:         req.ContextLimit,
		TrainingUse:          trainingUse,
		Retention:            req.Retention,
		ApprovedScopes:       req.ApprovedScopes,
		ControlEvidence:      req.ControlEvidence,
		ReleaseState:         domain.ReleaseDiscovered,
		CreatedAt:            now,
		CreatedByPrincipalID: actor,
		UpdatedAt:            now,
	}
	s.modelReleases[m.ModelReleaseID] = m
	return m, nil
}

func (s *stubStore) GetModelRelease(_ context.Context, id string) (*domain.AIModelRelease, error) {
	m, ok := s.modelReleases[id]
	if !ok {
		return nil, domain.ErrModelReleaseNotFound
	}
	return m, nil
}

// transitionRelease mirrors PgStore.transitionRelease: load, verify the
// caller's expected current state(s), merge evidence, move to target.
func (s *stubStore) transitionRelease(modelReleaseID string, expectedStates []domain.ReleaseState, targetState domain.ReleaseState,
	evidence map[string]interface{}, reason string) (*domain.AIModelRelease, error) {
	m, ok := s.modelReleases[modelReleaseID]
	if !ok {
		return nil, domain.ErrModelReleaseNotFound
	}
	allowed := false
	for _, st := range expectedStates {
		if m.ReleaseState == st {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, domain.ErrInvalidReleaseTransition
	}
	if m.ControlEvidence == nil {
		m.ControlEvidence = map[string]interface{}{}
	}
	for k, v := range evidence {
		m.ControlEvidence[k] = v
	}
	if reason != "" {
		m.StatusReason = &reason
	}
	m.ReleaseState = targetState
	m.UpdatedAt = time.Now().UTC()
	return m, nil
}

func (s *stubStore) RecordDueDiligence(_ context.Context, modelReleaseID string, req domain.AdvanceReleaseRequest, actor string) (*domain.AIModelRelease, error) {
	return s.transitionRelease(modelReleaseID, []domain.ReleaseState{domain.ReleaseDiscovered}, domain.ReleaseDueDiligence, req.ControlEvidence, "")
}

func (s *stubStore) RecordEvaluation(_ context.Context, modelReleaseID string, req domain.AdvanceReleaseRequest, actor string) (*domain.AIModelRelease, error) {
	return s.transitionRelease(modelReleaseID, []domain.ReleaseState{domain.ReleaseDueDiligence}, domain.ReleaseEvaluating, req.ControlEvidence, "")
}

func (s *stubStore) ApproveRelease(_ context.Context, modelReleaseID string, req domain.ApproveReleaseRequest, actor string) (*domain.AIModelRelease, error) {
	if !req.AllGatesCleared() {
		return nil, domain.ErrReleaseGatesNotCleared
	}
	return s.transitionRelease(modelReleaseID, []domain.ReleaseState{domain.ReleaseEvaluating}, domain.ReleaseApproved, req.ControlEvidence, "")
}

func (s *stubStore) RejectRelease(_ context.Context, modelReleaseID string, req domain.AdvanceReleaseRequest, actor string) (*domain.AIModelRelease, error) {
	return s.transitionRelease(modelReleaseID, []domain.ReleaseState{domain.ReleaseDiscovered, domain.ReleaseDueDiligence}, domain.ReleaseRejected, req.ControlEvidence, req.Reason)
}

func (s *stubStore) BlockRelease(_ context.Context, modelReleaseID string, req domain.AdvanceReleaseRequest, actor string) (*domain.AIModelRelease, error) {
	return s.transitionRelease(modelReleaseID, []domain.ReleaseState{domain.ReleaseEvaluating}, domain.ReleaseBlocked, req.ControlEvidence, req.Reason)
}

func (s *stubStore) ActivateRelease(_ context.Context, modelReleaseID string, actor string) (*domain.AIModelRelease, error) {
	return s.transitionRelease(modelReleaseID, []domain.ReleaseState{domain.ReleaseApproved, domain.ReleaseRestricted}, domain.ReleaseActive, nil, "")
}

func (s *stubStore) RestrictRelease(_ context.Context, modelReleaseID string, req domain.AdvanceReleaseRequest, actor string) (*domain.AIModelRelease, error) {
	return s.transitionRelease(modelReleaseID, []domain.ReleaseState{domain.ReleaseActive}, domain.ReleaseRestricted, req.ControlEvidence, req.Reason)
}

func (s *stubStore) UnrestrictRelease(_ context.Context, modelReleaseID string, actor string) (*domain.AIModelRelease, error) {
	return s.transitionRelease(modelReleaseID, []domain.ReleaseState{domain.ReleaseRestricted}, domain.ReleaseActive, nil, "")
}

func (s *stubStore) QuarantineRelease(_ context.Context, modelReleaseID string, req domain.AdvanceReleaseRequest, actor string) (*domain.AIModelRelease, error) {
	return s.transitionRelease(modelReleaseID, []domain.ReleaseState{domain.ReleaseActive}, domain.ReleaseQuarantined, req.ControlEvidence, req.Reason)
}

func (s *stubStore) RetireRelease(_ context.Context, modelReleaseID string, req domain.AdvanceReleaseRequest, actor string) (*domain.AIModelRelease, error) {
	return s.transitionRelease(modelReleaseID,
		[]domain.ReleaseState{domain.ReleaseActive, domain.ReleaseRestricted, domain.ReleaseQuarantined},
		domain.ReleaseRetired, req.ControlEvidence, req.Reason)
}

func (s *stubStore) ListModelReleases(context.Context) ([]domain.AIModelRelease, error) {
	list := []domain.AIModelRelease{}
	for _, m := range s.modelReleases {
		list = append(list, *m)
	}
	return list, nil
}

var _ store.Store = (*stubStore)(nil)

type stubPublisher struct {
	events []events.PublishParams
}

func (p *stubPublisher) Publish(_ context.Context, params events.PublishParams) error {
	p.events = append(p.events, params)
	return nil
}

var _ events.Publisher = (*stubPublisher)(nil)

type stubAuthz struct{ err error }

func (s *stubAuthz) CheckAllowed(_ context.Context, _, _, _ string) error { return s.err }

var _ AuthzChecker = (*stubAuthz)(nil)

// stubKillSwitch defaults to "not blocked" so every pre-existing test
// keeps exercising only the store's own static-bool resolution, exactly
// as before this integration existed. Tests that care about the live
// kill-switch path construct their own instance directly.
type stubKillSwitch struct {
	blocked bool
	err     error
	calls   int
}

func (s *stubKillSwitch) Resolve(_ context.Context, _, _, _ string) (bool, error) {
	s.calls++
	return s.blocked, s.err
}

func newTestHandler() *Handler {
	logger, _ := zap.NewDevelopment()
	return New(newStubStore(), &stubPublisher{}, &stubAuthz{}, &stubKillSwitch{}, logger)
}

func newTestRouter(h *Handler) *chi.Mux {
	r := chi.NewRouter()
	// The real server wires this in cmd/server. Without it the handlers see
	// no verified tenant and every tenant-scoped route 401s — which is also
	// what the whole suite used to prove nothing about, since it sent
	// tenant_id in the body instead of the header.
	r.Use(middleware.TenantContext())
	RegisterRoutes(r, h)
	return r
}

const (
	testTenantA = "11111111-1111-1111-1111-111111111111"
	testTenantB = "22222222-2222-2222-2222-222222222222"
)

func buildRequest(method, path string, body interface{}) *http.Request {
	return buildRequestAs(method, path, body, testTenantA)
}

// buildRequestAs builds a request as a named tenant. Passing "" omits
// X-Tenant-Id entirely, which is what the platform-scope routes
// legitimately do and what the tenant-scoped routes must refuse.
func buildRequestAs(method, path string, body interface{}, tenantID string) *http.Request {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	r := httptest.NewRequest(method, path, &buf)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Principal-Id", "principal-test-01")
	if tenantID != "" {
		r.Header.Set("X-Tenant-Id", tenantID)
	}
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
		TenantID:       testTenantA,
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
		TenantID:          testTenantA,
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
		TenantID:       testTenantA,
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

func TestDecideAutomationAction_IgnoresClientSuppliedCheckerIdentity(t *testing.T) {
	h := newTestHandler()
	router := newTestRouter(h)
	actionID := "action-forged-checker"
	h.store.(*stubStore).actions[actionID] = &domain.AutomationAction{
		AutomationActionID:    actionID,
		TenantID:              testTenantA,
		ProposedByPrincipalID: "principal-test-01",
		ApprovalStatus:        domain.ApprovalPending,
		Status:                domain.AutomationActionProposed,
	}

	req := httptest.NewRequest(http.MethodPost,
		"/v1/automation-actions/"+actionID+"/decision",
		bytes.NewBufferString(`{"decision":"APPROVED","checkerPrincipalId":"principal-checker-02"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	req.Header.Set("X-Tenant-Id", testTenantA)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("client-supplied checker identity bypassed maker-checker: got %d — %s", w.Code, w.Body.String())
	}
	if h.store.(*stubStore).actions[actionID].ApprovalStatus != domain.ApprovalPending {
		t.Fatal("maker-checker rejection changed the action")
	}
}

func TestCreateExecution_RequiresAndReplaysIdempotencyKey(t *testing.T) {
	h := newTestHandler()
	router := newTestRouter(h)
	body := domain.CreateExecutionRequest{
		UseCaseID: "use-case-1", ModelReleaseID: "release-1",
		PackageID: "package-1", PackageVersion: "1.0",
		Input: []byte(`{"value":1}`),
	}
	withoutKey := httptest.NewRecorder()
	router.ServeHTTP(withoutKey, buildRequest(http.MethodPost, "/v1/ai/executions", body))
	if withoutKey.Code != http.StatusBadRequest {
		t.Fatalf("expected missing idempotency key to be rejected, got %d — %s", withoutKey.Code, withoutKey.Body.String())
	}

	create := func(request domain.CreateExecutionRequest) *httptest.ResponseRecorder {
		req := buildRequest(http.MethodPost, "/v1/ai/executions", request)
		req.Header.Set("Idempotency-Key", "execution-key-1")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	first := create(body)
	if first.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — %s", first.Code, first.Body.String())
	}
	var firstExecution domain.AIExecution
	if err := json.NewDecoder(first.Body).Decode(&firstExecution); err != nil {
		t.Fatal(err)
	}
	replay := create(body)
	if replay.Code != http.StatusCreated {
		t.Fatalf("expected replay status 201, got %d — %s", replay.Code, replay.Body.String())
	}
	var replayedExecution domain.AIExecution
	if err := json.NewDecoder(replay.Body).Decode(&replayedExecution); err != nil {
		t.Fatal(err)
	}
	if replayedExecution.ExecutionID != firstExecution.ExecutionID {
		t.Fatalf("replay created a second execution: first=%s replay=%s", firstExecution.ExecutionID, replayedExecution.ExecutionID)
	}

	body.PackageVersion = "2.0"
	changed := create(body)
	if changed.Code != http.StatusConflict {
		t.Fatalf("expected changed request conflict, got %d — %s", changed.Code, changed.Body.String())
	}
	if len(h.store.(*stubStore).executions) != 1 {
		t.Fatalf("expected one execution after replay and conflict, got %d", len(h.store.(*stubStore).executions))
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

// TestGetPolicyChangeApproval_RequiresAuth and
// TestListPolicyChangeApprovals_RequiresAuth prove these two reads are now
// gated the same way §4.3 always documented them ("platformScopeID
// required") and the same way every other resource's reads in this file
// already are — previously neither called requirePrincipal or authorize at
// all, so proposer/decider identities and proposed policy changes were
// readable by anyone with no principal and no grant.
func TestGetPolicyChangeApproval_RequiresAuth(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)

	wPropose := httptest.NewRecorder()
	r.ServeHTTP(wPropose, buildRequest(http.MethodPost, "/v1/policy-change-approvals", domain.ProposePolicyChangeRequest{
		TargetPolicyRef: "policy-svc:obligation-rule-43",
		ProposedChange:  "auth-gap regression fixture",
	}))
	if wPropose.Code != http.StatusCreated {
		t.Fatalf("expected 201 proposing policy change, got %d — %s", wPropose.Code, wPropose.Body.String())
	}
	var change domain.PolicyChangeApproval
	_ = json.NewDecoder(wPropose.Body).Decode(&change)

	noPrincipal := httptest.NewRequest(http.MethodGet, "/v1/policy-change-approvals/"+change.PolicyChangeApprovalID, nil)
	wNoPrincipal := httptest.NewRecorder()
	r.ServeHTTP(wNoPrincipal, noPrincipal)
	if wNoPrincipal.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no principal, got %d — %s", wNoPrincipal.Code, wNoPrincipal.Body.String())
	}

	logger, _ := zap.NewDevelopment()
	denied := New(newStubStore(), &stubPublisher{}, &stubAuthz{err: authzpkg.ErrAuthorizationDenied}, &stubKillSwitch{}, logger)
	rDenied := newTestRouter(denied)
	wDenied := httptest.NewRecorder()
	rDenied.ServeHTTP(wDenied, buildRequest(http.MethodGet, "/v1/policy-change-approvals/"+change.PolicyChangeApprovalID, nil))
	if wDenied.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for an unauthorized principal, got %d — %s", wDenied.Code, wDenied.Body.String())
	}

	wOK := httptest.NewRecorder()
	r.ServeHTTP(wOK, buildRequest(http.MethodGet, "/v1/policy-change-approvals/"+change.PolicyChangeApprovalID, nil))
	if wOK.Code != http.StatusOK {
		t.Fatalf("expected 200 for an authorized principal, got %d — %s", wOK.Code, wOK.Body.String())
	}
}

func TestListPolicyChangeApprovals_RequiresAuth(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)

	noPrincipal := httptest.NewRequest(http.MethodGet, "/v1/policy-change-approvals", nil)
	wNoPrincipal := httptest.NewRecorder()
	r.ServeHTTP(wNoPrincipal, noPrincipal)
	if wNoPrincipal.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no principal, got %d — %s", wNoPrincipal.Code, wNoPrincipal.Body.String())
	}

	logger, _ := zap.NewDevelopment()
	denied := New(newStubStore(), &stubPublisher{}, &stubAuthz{err: authzpkg.ErrAuthorizationDenied}, &stubKillSwitch{}, logger)
	rDenied := newTestRouter(denied)
	wDenied := httptest.NewRecorder()
	rDenied.ServeHTTP(wDenied, buildRequest(http.MethodGet, "/v1/policy-change-approvals", nil))
	if wDenied.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for an unauthorized principal, got %d — %s", wDenied.Code, wDenied.Body.String())
	}

	wOK := httptest.NewRecorder()
	r.ServeHTTP(wOK, buildRequest(http.MethodGet, "/v1/policy-change-approvals", nil))
	if wOK.Code != http.StatusOK {
		t.Fatalf("expected 200 for an authorized principal, got %d — %s", wOK.Code, wOK.Body.String())
	}
}

// allowlistTool creates an automation policy that would otherwise allow
// role/tool/action for testTenantA — shared setup for the kill-switch
// tests below, which all care about what happens to an ALREADY-allowed
// policy once the live registry is consulted.
func allowlistTool(t *testing.T, r *chi.Mux, role, tool, actionType string) {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/automation-policies", domain.CreateAutomationPolicyRequest{
		TenantID:   testTenantA,
		Role:       role,
		Tool:       tool,
		ActionType: actionType,
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 creating automation policy, got %d — %s", w.Code, w.Body.String())
	}
}

func TestResolveAutomationPolicy_LiveKillSwitchOverridesAllowedPolicy(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	ks := &stubKillSwitch{blocked: true}
	h := New(newStubStore(), &stubPublisher{}, &stubAuthz{}, ks, logger)
	r := newTestRouter(h)

	allowlistTool(t, r, "billing-agent", "stripe-refund-tool", "SEND_REFUND")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodGet,
		"/v1/automation-policies/resolve?role=billing-agent&tool=stripe-refund-tool&action_type=SEND_REFUND", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — %s", w.Code, w.Body.String())
	}
	var res domain.AutomationPolicyResolution
	_ = json.NewDecoder(w.Body).Decode(&res)
	if res.Allowed {
		t.Fatalf("expected an engaged live kill switch to block an otherwise-allowed policy, got %+v", res)
	}
	if res.ReasonCode != "KILL_SWITCH_ENGAGED" {
		t.Fatalf("expected reason KILL_SWITCH_ENGAGED, got %s", res.ReasonCode)
	}
	if ks.calls != 1 {
		t.Fatalf("expected exactly one live kill-switch check, got %d", ks.calls)
	}
}

func TestResolveAutomationPolicy_KillSwitchServiceUnavailable_FailsClosed(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	ks := &stubKillSwitch{err: killswitch.ErrServiceUnavailable}
	h := New(newStubStore(), &stubPublisher{}, &stubAuthz{}, ks, logger)
	r := newTestRouter(h)

	allowlistTool(t, r, "billing-agent", "stripe-refund-tool", "SEND_REFUND")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodGet,
		"/v1/automation-policies/resolve?role=billing-agent&tool=stripe-refund-tool&action_type=SEND_REFUND", nil))
	var res domain.AutomationPolicyResolution
	_ = json.NewDecoder(w.Body).Decode(&res)
	if res.Allowed {
		t.Fatalf("expected an unreachable kill-switch-registry-svc to fail closed (not allowed), got %+v", res)
	}
	if res.ReasonCode != "KILL_SWITCH_CHECK_UNAVAILABLE" {
		t.Fatalf("expected reason KILL_SWITCH_CHECK_UNAVAILABLE, got %s", res.ReasonCode)
	}
}

// TestResolveAutomationPolicy_NotAllowlisted_SkipsLiveKillSwitchCheck is the
// negative control's mirror: a policy that is already NOT_ALLOWLISTED must
// short-circuit before ever calling the live registry — there is nothing
// for the kill switch to add to an already-denied answer, and calling it
// anyway would mean every unallowlisted resolve pays a network round trip
// for no reason.
func TestResolveAutomationPolicy_NotAllowlisted_SkipsLiveKillSwitchCheck(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	ks := &stubKillSwitch{blocked: true}
	h := New(newStubStore(), &stubPublisher{}, &stubAuthz{}, ks, logger)
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodGet,
		"/v1/automation-policies/resolve?role=nobody&tool=nothing&action_type=NOTHING", nil))
	var res domain.AutomationPolicyResolution
	_ = json.NewDecoder(w.Body).Decode(&res)
	if res.ReasonCode != "NOT_ALLOWLISTED" {
		t.Fatalf("expected NOT_ALLOWLISTED, got %s", res.ReasonCode)
	}
	if ks.calls != 0 {
		t.Fatalf("expected the live kill-switch check to be skipped for an already-denied policy, got %d calls", ks.calls)
	}
}

func TestProposeAutomationAction_BlockedByLiveKillSwitch(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	ks := &stubKillSwitch{blocked: true}
	h := New(newStubStore(), &stubPublisher{}, &stubAuthz{}, ks, logger)
	r := newTestRouter(h)

	allowlistTool(t, r, "billing-agent", "stripe-refund-tool", "SEND_REFUND")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/automation-actions", domain.ProposeAutomationActionRequest{
		TenantID:       testTenantA,
		ActionType:     "SEND_REFUND",
		Role:           "billing-agent",
		Tool:           "stripe-refund-tool",
		IdempotencyKey: "idem-killswitch-1",
	}))
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 — an engaged live kill switch must block proposing the action, not just reading its resolution, got %d — %s", w.Code, w.Body.String())
	}
}

func TestListEndpoints(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	h := New(newStubStore(), &stubPublisher{}, &stubAuthz{}, &stubKillSwitch{}, logger)
	r := newTestRouter(h)

	// Seed Model Provider
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, buildRequest(http.MethodPost, "/v1/model-providers", domain.RegisterModelProviderRequest{
		ProviderName: "anthropic",
		ModelName:    "claude-3-7-sonnet",
		DataRegion:   "eu-west-1",
		DPAVerified:  true,
	}))
	if w1.Code != http.StatusOK && w1.Code != http.StatusCreated {
		t.Fatalf("seed model provider failed: %d - %s", w1.Code, w1.Body.String())
	}

	// Query GET /v1/model-providers
	wListModels := httptest.NewRecorder()
	r.ServeHTTP(wListModels, buildRequest(http.MethodGet, "/v1/model-providers", nil))
	if wListModels.Code != http.StatusOK {
		t.Fatalf("expected 200 from GET /v1/model-providers, got %d", wListModels.Code)
	}
	var modelsResp struct {
		ModelProviders []domain.ModelProviderRegistration `json:"model_providers"`
	}
	if err := json.NewDecoder(wListModels.Body).Decode(&modelsResp); err != nil {
		t.Fatalf("decode models failed: %v", err)
	}
	if len(modelsResp.ModelProviders) != 1 || modelsResp.ModelProviders[0].ModelName != "claude-3-7-sonnet" {
		t.Fatalf("unexpected model providers list: %+v", modelsResp.ModelProviders)
	}

	// Seed Action Risk Classification
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, buildRequest(http.MethodPost, "/v1/action-risk-classifications", domain.SetActionRiskClassificationRequest{
		ActionType:           "SEND_REFUND",
		RiskCategory:         "MONEY",
		HumanReviewTrigger:   true,
		RequiresMakerChecker: true,
	}))
	if w2.Code != http.StatusOK {
		t.Fatalf("seed classification failed: %d - %s", w2.Code, w2.Body.String())
	}

	// Query GET /v1/action-risk-classifications
	wListRisk := httptest.NewRecorder()
	r.ServeHTTP(wListRisk, buildRequest(http.MethodGet, "/v1/action-risk-classifications", nil))
	if wListRisk.Code != http.StatusOK {
		t.Fatalf("expected 200 from GET /v1/action-risk-classifications, got %d", wListRisk.Code)
	}
	var riskResp struct {
		Classifications []domain.ActionRiskClassification `json:"action_risk_classifications"`
	}
	if err := json.NewDecoder(wListRisk.Body).Decode(&riskResp); err != nil {
		t.Fatalf("decode risk classifications failed: %v", err)
	}
	if len(riskResp.Classifications) != 1 || riskResp.Classifications[0].ActionType != "SEND_REFUND" {
		t.Fatalf("unexpected risk classifications list: %+v", riskResp.Classifications)
	}

	// Seed Automation Policy
	wPolicy := httptest.NewRecorder()
	r.ServeHTTP(wPolicy, buildRequest(http.MethodPost, "/v1/automation-policies", domain.CreateAutomationPolicyRequest{
		TenantID:     testTenantA,
		Role:         "billing-agent",
		RiskCategory: "MONEY",
		Tool:         "stripe-refund-tool",
		ActionType:   "SEND_REFUND",
	}))
	if wPolicy.Code != http.StatusCreated {
		t.Fatalf("create automation policy failed: %d - %s", wPolicy.Code, wPolicy.Body.String())
	}

	// Query GET /v1/automation-policies
	wListPolicies := httptest.NewRecorder()
	r.ServeHTTP(wListPolicies, buildRequest(http.MethodGet, "/v1/automation-policies", nil))
	if wListPolicies.Code != http.StatusOK {
		t.Fatalf("expected 200 from GET /v1/automation-policies, got %d", wListPolicies.Code)
	}
	var policiesResp struct {
		Policies []domain.AutomationPolicy `json:"automation_policies"`
	}
	if err := json.NewDecoder(wListPolicies.Body).Decode(&policiesResp); err != nil {
		t.Fatalf("decode policies failed: %v", err)
	}
	if len(policiesResp.Policies) != 1 || policiesResp.Policies[0].Tool != "stripe-refund-tool" {
		t.Fatalf("unexpected automation policies list: %+v", policiesResp.Policies)
	}

	// Seed Automation Action
	w3 := httptest.NewRecorder()
	r.ServeHTTP(w3, buildRequest(http.MethodPost, "/v1/automation-actions", domain.ProposeAutomationActionRequest{
		ActionType:     "SEND_REFUND",
		Role:           "billing-agent",
		Tool:           "stripe-refund-tool",
		IdempotencyKey: "test-idem-list-1",
	}))
	if w3.Code != http.StatusCreated {
		t.Fatalf("seed automation action failed: %d - %s", w3.Code, w3.Body.String())
	}

	// Query GET /v1/automation-actions
	wListActions := httptest.NewRecorder()
	r.ServeHTTP(wListActions, buildRequest(http.MethodGet, "/v1/automation-actions", nil))
	if wListActions.Code != http.StatusOK {
		t.Fatalf("expected 200 from GET /v1/automation-actions, got %d", wListActions.Code)
	}
	var actionsResp struct {
		Actions []domain.AutomationAction `json:"automation_actions"`
	}
	if err := json.NewDecoder(wListActions.Body).Decode(&actionsResp); err != nil {
		t.Fatalf("decode actions failed: %v", err)
	}
	if len(actionsResp.Actions) != 1 || actionsResp.Actions[0].ActionType != "SEND_REFUND" {
		t.Fatalf("unexpected automation actions list: %+v", actionsResp.Actions)
	}

	// Seed Policy Change Approval
	w4 := httptest.NewRecorder()
	r.ServeHTTP(w4, buildRequest(http.MethodPost, "/v1/policy-change-approvals", domain.ProposePolicyChangeRequest{
		TargetPolicyRef: "policy-refund-001",
		ProposedChange:  "Raise refund cap to $500",
	}))
	if w4.Code != http.StatusCreated {
		t.Fatalf("seed policy change failed: %d - %s", w4.Code, w4.Body.String())
	}

	// Query GET /v1/policy-change-approvals
	wListApprovals := httptest.NewRecorder()
	r.ServeHTTP(wListApprovals, buildRequest(http.MethodGet, "/v1/policy-change-approvals", nil))
	if wListApprovals.Code != http.StatusOK {
		t.Fatalf("expected 200 from GET /v1/policy-change-approvals, got %d", wListApprovals.Code)
	}
	var approvalsResp struct {
		Approvals []domain.PolicyChangeApproval `json:"policy_change_approvals"`
	}
	if err := json.NewDecoder(wListApprovals.Body).Decode(&approvalsResp); err != nil {
		t.Fatalf("decode approvals failed: %v", err)
	}
	if len(approvalsResp.Approvals) != 1 || approvalsResp.Approvals[0].TargetPolicyRef != "policy-refund-001" {
		t.Fatalf("unexpected policy change approvals list: %+v", approvalsResp.Approvals)
	}
}

func TestKafkaEvents_EmittedOnWrites(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	pub := &stubPublisher{}
	st := newStubStore()
	h := New(st, pub, &stubAuthz{}, &stubKillSwitch{}, logger)
	r := newTestRouter(h)

	// 1. CreateAIRun
	w := httptest.NewRecorder()
	reqAIRun := domain.CreateAIRunRequest{
		RunType:       "categorization",
		ModelID:       "claude-3-7-sonnet",
		PromptVersion: "v1.0",
		AuditID:       "audit-001",
	}
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/ai-runs", reqAIRun))
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateAIRun failed: %d - %s", w.Code, w.Body.String())
	}
	assertLastEvent(t, pub, "ai_run.created", testTenantA)

	// 2. SetActionRiskClassification
	w = httptest.NewRecorder()
	reqARC := domain.SetActionRiskClassificationRequest{
		ActionType:   "EXECUTE_PAYMENT",
		RiskCategory: "MONEY",
	}
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/action-risk-classifications", reqARC))
	if w.Code != http.StatusOK {
		t.Fatalf("SetActionRiskClassification failed: %d - %s", w.Code, w.Body.String())
	}
	assertLastEvent(t, pub, "action_risk_classification.set", "")

	// 3. CreateAutomationPolicy
	w = httptest.NewRecorder()
	reqAP := domain.CreateAutomationPolicyRequest{
		TenantID:     testTenantA,
		Role:         "finance-operator",
		RiskCategory: "MONEY",
		Tool:         "payment-tool",
		ActionType:   "EXECUTE_PAYMENT",
	}
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/automation-policies", reqAP))
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateAutomationPolicy failed: %d - %s", w.Code, w.Body.String())
	}
	assertLastEvent(t, pub, "automation_policy.created", testTenantA)

	// 4. RegisterModelProvider
	w = httptest.NewRecorder()
	reqMP := domain.RegisterModelProviderRequest{
		ProviderName: "openai",
		ModelName:    "gpt-4o",
		DataRegion:   "us-east-1",
	}
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/model-providers", reqMP))
	if w.Code != http.StatusOK {
		t.Fatalf("RegisterModelProvider failed: %d - %s", w.Code, w.Body.String())
	}
	assertLastEvent(t, pub, "model_provider.registered", "")

	// 5. ProposePolicyChange
	w = httptest.NewRecorder()
	reqPPC := domain.ProposePolicyChangeRequest{
		TargetPolicyRef: "policy-001",
		ProposedChange:  "Update limits",
	}
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/policy-change-approvals", reqPPC))
	if w.Code != http.StatusCreated {
		t.Fatalf("ProposePolicyChange failed: %d - %s", w.Code, w.Body.String())
	}
	assertLastEvent(t, pub, "policy_change.proposed", "")
	var createdPPC domain.PolicyChangeApproval
	_ = json.NewDecoder(w.Body).Decode(&createdPPC)

	// 6. DecidePolicyChange (as different principal to respect maker-checker)
	w = httptest.NewRecorder()
	reqDPC := domain.DecidePolicyChangeRequest{
		Decision: "APPROVED",
		Reason:   "Looks good",
	}
	reqDecide := buildRequest(http.MethodPost, "/v1/policy-change-approvals/"+createdPPC.PolicyChangeApprovalID+"/decision", reqDPC)
	reqDecide.Header.Set("X-Principal-Id", "principal-checker-02")
	r.ServeHTTP(w, reqDecide)
	if w.Code != http.StatusOK {
		t.Fatalf("DecidePolicyChange failed: %d - %s", w.Code, w.Body.String())
	}
	assertLastEvent(t, pub, "policy_change.decided", "")
}

func assertLastEvent(t *testing.T, pub *stubPublisher, expectedType, expectedTenant string) {
	t.Helper()
	if len(pub.events) == 0 {
		t.Fatalf("expected event %q, but no events published", expectedType)
	}
	last := pub.events[len(pub.events)-1]
	if last.EventType != expectedType {
		t.Fatalf("expected event type %q, got %q", expectedType, last.EventType)
	}
	if expectedTenant != "" && last.TenantID != expectedTenant {
		t.Fatalf("expected event tenant %q, got %q", expectedTenant, last.TenantID)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Kafka events for the 15 AIG-01/AIG-02 write/state-transition endpoints that
// were added without event emission alongside CreateUseCase. Each endpoint's
// event is covered on a path that actually reaches it through the real
// forward-only state machine (mirrored by stubStore), not by calling the
// handler out of sequence.
// ─────────────────────────────────────────────────────────────────────────────

func createDraftUseCase(t *testing.T, r *chi.Mux) string {
	t.Helper()
	w := httptest.NewRecorder()
	req := domain.CreateUseCaseRequest{
		Domain:           "customer-support",
		Purpose:          "triage inbound tickets",
		OutcomeType:      "RECOMMENDATION",
		OperationalClass: "A1",
		OwnerPrincipalID: "principal-test-01",
		BusinessOutcome:  "faster first response",
		AutomationLevel:  "RECOMMENDATION",
	}
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/ai/use-cases", req))
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateUseCase failed: %d - %s", w.Code, w.Body.String())
	}
	var uc domain.AIUseCase
	_ = json.NewDecoder(w.Body).Decode(&uc)
	return uc.UseCaseID
}

// approveUseCase drives a DRAFT use case to APPROVED via StartAssessment +
// DecideAssessment(APPROVED) — neither is one of the 15 endpoints this fix
// targets, so no event assertion is made here.
func approveUseCase(t *testing.T, r *chi.Mux, useCaseID string) {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/ai/use-cases/"+useCaseID+"/assess", domain.StartAssessmentRequest{}))
	if w.Code != http.StatusCreated {
		t.Fatalf("StartAssessment failed: %d - %s", w.Code, w.Body.String())
	}
	var a domain.AIImpactAssessment
	_ = json.NewDecoder(w.Body).Decode(&a)

	w = httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/ai/use-cases/assessments/"+a.AssessmentID+"/decision", domain.DecideAssessmentRequest{Decision: "APPROVED"}))
	if w.Code != http.StatusOK {
		t.Fatalf("DecideAssessment failed: %d - %s", w.Code, w.Body.String())
	}
}

func activateApprovedUseCase(t *testing.T, r *chi.Mux, useCaseID string) {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/ai/use-cases/"+useCaseID+"/activate", domain.ActivateUseCaseRequest{}))
	if w.Code != http.StatusOK {
		t.Fatalf("ActivateUseCase failed: %d - %s", w.Code, w.Body.String())
	}
}

func TestKafkaEvents_EmittedOnAIG01UseCaseTransitions(t *testing.T) {
	logger, _ := zap.NewDevelopment()

	t.Run("ActivateUseCase", func(t *testing.T) {
		pub := &stubPublisher{}
		h := New(newStubStore(), pub, &stubAuthz{}, &stubKillSwitch{}, logger)
		r := newTestRouter(h)
		id := createDraftUseCase(t, r)
		approveUseCase(t, r, id)
		activateApprovedUseCase(t, r, id)
		assertLastEvent(t, pub, "ai.use_case.state_changed", testTenantA)
	})

	t.Run("SuspendUseCase", func(t *testing.T) {
		pub := &stubPublisher{}
		h := New(newStubStore(), pub, &stubAuthz{}, &stubKillSwitch{}, logger)
		r := newTestRouter(h)
		id := createDraftUseCase(t, r)
		approveUseCase(t, r, id)
		activateApprovedUseCase(t, r, id)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/ai/use-cases/"+id+"/suspend", domain.SuspendUseCaseRequest{Reason: "incident"}))
		if w.Code != http.StatusOK {
			t.Fatalf("SuspendUseCase failed: %d - %s", w.Code, w.Body.String())
		}
		assertLastEvent(t, pub, "ai.use_case.state_changed", testTenantA)
	})

	t.Run("RequestReassessment", func(t *testing.T) {
		pub := &stubPublisher{}
		h := New(newStubStore(), pub, &stubAuthz{}, &stubKillSwitch{}, logger)
		r := newTestRouter(h)
		id := createDraftUseCase(t, r)
		approveUseCase(t, r, id)
		activateApprovedUseCase(t, r, id)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/ai/use-cases/"+id+"/reassess", domain.RequestReassessmentRequest{Reason: "model changed"}))
		if w.Code != http.StatusOK {
			t.Fatalf("RequestReassessment failed: %d - %s", w.Code, w.Body.String())
		}
		assertLastEvent(t, pub, "ai.use_case.state_changed", testTenantA)
	})

	t.Run("RetireUseCase", func(t *testing.T) {
		pub := &stubPublisher{}
		h := New(newStubStore(), pub, &stubAuthz{}, &stubKillSwitch{}, logger)
		r := newTestRouter(h)
		id := createDraftUseCase(t, r)
		approveUseCase(t, r, id)
		activateApprovedUseCase(t, r, id)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/ai/use-cases/"+id+"/retire", domain.RetireUseCaseRequest{Reason: "sunset"}))
		if w.Code != http.StatusOK {
			t.Fatalf("RetireUseCase failed: %d - %s", w.Code, w.Body.String())
		}
		assertLastEvent(t, pub, "ai.use_case.state_changed", testTenantA)
	})
}

// TestKafkaEvents_EmittedOnAIG02ReleaseLifecycle walks one release through
// its full forward chain and checks every hop's event, including an
// explicit (not just assertLastEvent-generic) check that QuarantineRelease
// emits exactly ai.model.release_quarantined — ZS-SVC-X-001 §9.3 names this
// event specifically.
func TestKafkaEvents_EmittedOnAIG02ReleaseLifecycle(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	pub := &stubPublisher{}
	h := New(newStubStore(), pub, &stubAuthz{}, &stubKillSwitch{}, logger)
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/ai/model-releases", domain.RegisterModelReleaseRequest{
		Provider:         "openai",
		ProviderModelID:  "gpt-5",
		DeploymentRegion: "us-east-1",
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("RegisterModelRelease failed: %d - %s", w.Code, w.Body.String())
	}
	assertLastEvent(t, pub, "ai.model.release_registered", "")
	var release domain.AIModelRelease
	_ = json.NewDecoder(w.Body).Decode(&release)
	id := release.ModelReleaseID

	w = httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/ai/model-releases/"+id+"/due-diligence", domain.AdvanceReleaseRequest{}))
	if w.Code != http.StatusOK {
		t.Fatalf("RecordDueDiligence failed: %d - %s", w.Code, w.Body.String())
	}
	assertLastEvent(t, pub, "ai.model.release_due_diligence_recorded", "")

	w = httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/ai/model-releases/"+id+"/evaluation", domain.AdvanceReleaseRequest{}))
	if w.Code != http.StatusOK {
		t.Fatalf("RecordEvaluation failed: %d - %s", w.Code, w.Body.String())
	}
	assertLastEvent(t, pub, "ai.model.release_evaluated", "")

	w = httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/ai/model-releases/"+id+"/approve", domain.ApproveReleaseRequest{
		PrivacyContractCleared: true, ResidencyCleared: true, SecurityCleared: true,
		EvaluationCleared: true, ExplainabilityCleared: true, ContinuityCleared: true, LegalCleared: true,
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("ApproveRelease failed: %d - %s", w.Code, w.Body.String())
	}
	assertLastEvent(t, pub, "ai.model.release_approved", "")

	w = httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/ai/model-releases/"+id+"/activate", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("ActivateRelease failed: %d - %s", w.Code, w.Body.String())
	}
	assertLastEvent(t, pub, "ai.model.release_activated", "")

	w = httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/ai/model-releases/"+id+"/restrict", domain.AdvanceReleaseRequest{Reason: "narrow scope"}))
	if w.Code != http.StatusOK {
		t.Fatalf("RestrictRelease failed: %d - %s", w.Code, w.Body.String())
	}
	assertLastEvent(t, pub, "ai.model.release_restricted", "")

	w = httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/ai/model-releases/"+id+"/unrestrict", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("UnrestrictRelease failed: %d - %s", w.Code, w.Body.String())
	}
	assertLastEvent(t, pub, "ai.model.release_unrestricted", "")

	w = httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/ai/model-releases/"+id+"/quarantine", domain.AdvanceReleaseRequest{Reason: "safety incident"}))
	if w.Code != http.StatusOK {
		t.Fatalf("QuarantineRelease failed: %d - %s", w.Code, w.Body.String())
	}
	if len(pub.events) == 0 {
		t.Fatalf("expected ai.model.release_quarantined event, got none")
	}
	quarantineEvt := pub.events[len(pub.events)-1]
	if quarantineEvt.EventType != "ai.model.release_quarantined" {
		t.Fatalf("ZS-SVC-X-001 §9.3 requires exactly %q, got %q", "ai.model.release_quarantined", quarantineEvt.EventType)
	}
	if quarantineEvt.EntityID != id {
		t.Fatalf("expected quarantine event entity_id %q, got %q", id, quarantineEvt.EntityID)
	}

	w = httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/ai/model-releases/"+id+"/retire", domain.AdvanceReleaseRequest{Reason: "decommission"}))
	if w.Code != http.StatusOK {
		t.Fatalf("RetireRelease failed: %d - %s", w.Code, w.Body.String())
	}
	assertLastEvent(t, pub, "ai.model.release_retired", "")
}

// TestKafkaEvents_EmittedOnAIG02ReleaseRejectAndBlock covers the two
// alternate terminal hops that RegisterModelRelease's main forward chain
// above never reaches (REJECTED only from DISCOVERED/DUE_DILIGENCE; BLOCKED
// only from EVALUATING).
func TestKafkaEvents_EmittedOnAIG02ReleaseRejectAndBlock(t *testing.T) {
	logger, _ := zap.NewDevelopment()

	t.Run("RejectRelease", func(t *testing.T) {
		pub := &stubPublisher{}
		h := New(newStubStore(), pub, &stubAuthz{}, &stubKillSwitch{}, logger)
		r := newTestRouter(h)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/ai/model-releases", domain.RegisterModelReleaseRequest{
			Provider: "anthropic", ProviderModelID: "claude-5-reject", DeploymentRegion: "us-east-1",
		}))
		if w.Code != http.StatusCreated {
			t.Fatalf("RegisterModelRelease failed: %d - %s", w.Code, w.Body.String())
		}
		var release domain.AIModelRelease
		_ = json.NewDecoder(w.Body).Decode(&release)

		w = httptest.NewRecorder()
		r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/ai/model-releases/"+release.ModelReleaseID+"/reject", domain.AdvanceReleaseRequest{Reason: "fails due diligence"}))
		if w.Code != http.StatusOK {
			t.Fatalf("RejectRelease failed: %d - %s", w.Code, w.Body.String())
		}
		assertLastEvent(t, pub, "ai.model.release_rejected", "")
	})

	t.Run("BlockRelease", func(t *testing.T) {
		pub := &stubPublisher{}
		h := New(newStubStore(), pub, &stubAuthz{}, &stubKillSwitch{}, logger)
		r := newTestRouter(h)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/ai/model-releases", domain.RegisterModelReleaseRequest{
			Provider: "anthropic", ProviderModelID: "claude-5-block", DeploymentRegion: "us-east-1",
		}))
		if w.Code != http.StatusCreated {
			t.Fatalf("RegisterModelRelease failed: %d - %s", w.Code, w.Body.String())
		}
		var release domain.AIModelRelease
		_ = json.NewDecoder(w.Body).Decode(&release)
		id := release.ModelReleaseID

		w = httptest.NewRecorder()
		r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/ai/model-releases/"+id+"/due-diligence", domain.AdvanceReleaseRequest{}))
		if w.Code != http.StatusOK {
			t.Fatalf("RecordDueDiligence failed: %d - %s", w.Code, w.Body.String())
		}
		w = httptest.NewRecorder()
		r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/ai/model-releases/"+id+"/evaluation", domain.AdvanceReleaseRequest{}))
		if w.Code != http.StatusOK {
			t.Fatalf("RecordEvaluation failed: %d - %s", w.Code, w.Body.String())
		}

		w = httptest.NewRecorder()
		r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/ai/model-releases/"+id+"/block", domain.AdvanceReleaseRequest{Reason: "security posture failed"}))
		if w.Code != http.StatusOK {
			t.Fatalf("BlockRelease failed: %d - %s", w.Code, w.Body.String())
		}
		assertLastEvent(t, pub, "ai.model.release_blocked", "")
	})
}

// TestKafkaEvents_NotEmittedOnFailedOrDuplicateTransitions covers task
// requirements #6 ("do not emit events for failed, rejected, or
// unauthorized operations" — here, an illegal state transition) and #8
// ("prevent duplicate events during idempotent/repeated requests").
func TestKafkaEvents_NotEmittedOnFailedOrDuplicateTransitions(t *testing.T) {
	logger, _ := zap.NewDevelopment()

	t.Run("InvalidReleaseTransitionPublishesNothing", func(t *testing.T) {
		pub := &stubPublisher{}
		h := New(newStubStore(), pub, &stubAuthz{}, &stubKillSwitch{}, logger)
		r := newTestRouter(h)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/ai/model-releases", domain.RegisterModelReleaseRequest{
			Provider: "openai", ProviderModelID: "gpt-5-invalid", DeploymentRegion: "us-east-1",
		}))
		if w.Code != http.StatusCreated {
			t.Fatalf("RegisterModelRelease failed: %d - %s", w.Code, w.Body.String())
		}
		var release domain.AIModelRelease
		_ = json.NewDecoder(w.Body).Decode(&release)

		// DISCOVERED cannot activate directly — must pass through
		// due-diligence/evaluation/approve first.
		w = httptest.NewRecorder()
		r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/ai/model-releases/"+release.ModelReleaseID+"/activate", nil))
		if w.Code != http.StatusConflict {
			t.Fatalf("expected 409 for illegal transition, got %d - %s", w.Code, w.Body.String())
		}
		if len(pub.events) != 1 {
			t.Fatalf("expected only the register event (1), got %d events published after a failed transition", len(pub.events))
		}
		if pub.events[0].EventType != "ai.model.release_registered" {
			t.Fatalf("expected the only event to remain ai.model.release_registered, got %q", pub.events[0].EventType)
		}
	})

	t.Run("RepeatedActivateUseCaseAfterSuccessPublishesNoDuplicate", func(t *testing.T) {
		pub := &stubPublisher{}
		h := New(newStubStore(), pub, &stubAuthz{}, &stubKillSwitch{}, logger)
		r := newTestRouter(h)
		id := createDraftUseCase(t, r)
		approveUseCase(t, r, id)
		activateApprovedUseCase(t, r, id)
		// CreateUseCase itself also publishes ai.use_case.state_changed, so
		// the baseline after one successful activate is 2 events (create +
		// activate), not 1 — what matters for this test is that the count
		// doesn't grow further on a retry.
		countAfterActivate := len(pub.events)
		if countAfterActivate == 0 {
			t.Fatalf("expected at least 1 event after first activate, got 0")
		}
		lastEventAfterActivate := pub.events[countAfterActivate-1]

		// Retrying the exact same activate call after it already succeeded
		// must not publish a second event — ActivateUseCase is only legal
		// from APPROVED/LIMITED, and the use case is now ACTIVE, so the
		// state-machine precondition blocks the retry before any publish
		// is reached.
		w := httptest.NewRecorder()
		r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/ai/use-cases/"+id+"/activate", domain.ActivateUseCaseRequest{}))
		if w.Code != http.StatusConflict {
			t.Fatalf("expected 409 on retry of already-ACTIVE use case, got %d - %s", w.Code, w.Body.String())
		}
		if len(pub.events) != countAfterActivate {
			t.Fatalf("expected event count to stay at %d after retried activate, got %d", countAfterActivate, len(pub.events))
		}
		if pub.events[len(pub.events)-1] != lastEventAfterActivate {
			t.Fatalf("expected last published event to remain the one from the successful activate, got a different event")
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// ListUseCases / ListModelReleases: the two collection-GET endpoints added to
// close the "console cannot browse existing registry records" gap. The
// frontend previously called these exact URLs and got a live 405, since
// no such route existed; these tests cover the route and store layer that
// now backs it.
// ─────────────────────────────────────────────────────────────────────────────

func TestListUseCases_EmptyWhenNoneExist(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodGet, "/v1/ai/use-cases", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d - %s", w.Code, w.Body.String())
	}
	var body struct {
		UseCases []domain.AIUseCase `json:"use_cases"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.UseCases == nil {
		t.Fatalf("expected an empty array, got a null use_cases field — frontend code checks Array.isArray")
	}
	if len(body.UseCases) != 0 {
		t.Fatalf("expected 0 use cases, got %d", len(body.UseCases))
	}
}

func TestListUseCases_ReturnsExistingRecords(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)

	id1 := createDraftUseCase(t, r)
	id2 := createDraftUseCase(t, r)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodGet, "/v1/ai/use-cases", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d - %s", w.Code, w.Body.String())
	}
	var body struct {
		UseCases []domain.AIUseCase `json:"use_cases"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.UseCases) != 2 {
		t.Fatalf("expected 2 use cases, got %d", len(body.UseCases))
	}
	seen := map[string]bool{}
	for _, uc := range body.UseCases {
		seen[uc.UseCaseID] = true
	}
	if !seen[id1] || !seen[id2] {
		t.Fatalf("expected both created use cases in the list, got %+v", body.UseCases)
	}
}

// TestListUseCases_TenantIsolation is the core RLS/isolation claim: a use
// case created under tenant A must never appear when tenant B lists its own
// use cases, and must never leak tenant A's data into tenant B's response.
func TestListUseCases_TenantIsolation(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequestAs(http.MethodPost, "/v1/ai/use-cases", domain.CreateUseCaseRequest{
		Domain: "finance", Purpose: "tenant-a-confidential-purpose", OutcomeType: "RECOMMENDATION",
		OperationalClass: "A1", OwnerPrincipalID: "principal-test-01", BusinessOutcome: "x", AutomationLevel: "RECOMMENDATION",
	}, testTenantA))
	if w.Code != http.StatusCreated {
		t.Fatalf("create as tenant A: got %d - %s", w.Code, w.Body.String())
	}

	wB := httptest.NewRecorder()
	r.ServeHTTP(wB, buildRequestAs(http.MethodGet, "/v1/ai/use-cases", nil, testTenantB))
	if wB.Code != http.StatusOK {
		t.Fatalf("expected 200 for tenant B's own (empty) list, got %d - %s", wB.Code, wB.Body.String())
	}
	var bodyB struct {
		UseCases []domain.AIUseCase `json:"use_cases"`
	}
	if err := json.NewDecoder(wB.Body).Decode(&bodyB); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(bodyB.UseCases) != 0 {
		t.Fatalf("ISOLATION FAILURE: tenant B's list returned %d use cases that belong to tenant A", len(bodyB.UseCases))
	}
	if bytes.Contains(wB.Body.Bytes(), []byte("tenant-a-confidential-purpose")) {
		t.Fatalf("ISOLATION FAILURE: tenant B's response body contains tenant A's data: %s", wB.Body.String())
	}
}

func TestListUseCases_RequiresPrincipalTenantAndAuthz(t *testing.T) {
	t.Run("no principal", func(t *testing.T) {
		r := newTestRouter(newTestHandler())
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/ai/use-cases", nil)
		req.Header.Set("X-Tenant-Id", testTenantA)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 with no X-Principal-Id, got %d - %s", w.Code, w.Body.String())
		}
	})

	t.Run("no tenant", func(t *testing.T) {
		r := newTestRouter(newTestHandler())
		w := httptest.NewRecorder()
		r.ServeHTTP(w, buildRequestAs(http.MethodGet, "/v1/ai/use-cases", nil, ""))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 with no X-Tenant-Id, got %d - %s", w.Code, w.Body.String())
		}
	})

	t.Run("authz denied", func(t *testing.T) {
		logger, _ := zap.NewDevelopment()
		h := New(newStubStore(), &stubPublisher{}, &stubAuthz{err: authzpkg.ErrAuthorizationDenied}, &stubKillSwitch{}, logger)
		r := newTestRouter(h)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, buildRequest(http.MethodGet, "/v1/ai/use-cases", nil))
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403 when UseCaseRead is denied, got %d - %s", w.Code, w.Body.String())
		}
	})
}

func TestListModelReleases_EmptyWhenNoneExist(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodGet, "/v1/ai/model-releases", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d - %s", w.Code, w.Body.String())
	}
	var body struct {
		ModelReleases []domain.AIModelRelease `json:"model_releases"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.ModelReleases == nil {
		t.Fatalf("expected an empty array, got a null model_releases field")
	}
	if len(body.ModelReleases) != 0 {
		t.Fatalf("expected 0 model releases, got %d", len(body.ModelReleases))
	}
}

func TestListModelReleases_ReturnsExistingRecords(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)

	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, buildRequest(http.MethodPost, "/v1/ai/model-releases", domain.RegisterModelReleaseRequest{
		Provider: "openai", ProviderModelID: "gpt-5-list-test", DeploymentRegion: "us-east-1",
	}))
	if w1.Code != http.StatusCreated {
		t.Fatalf("register release: got %d - %s", w1.Code, w1.Body.String())
	}
	var created domain.AIModelRelease
	_ = json.NewDecoder(w1.Body).Decode(&created)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodGet, "/v1/ai/model-releases", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d - %s", w.Code, w.Body.String())
	}
	var body struct {
		ModelReleases []domain.AIModelRelease `json:"model_releases"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.ModelReleases) != 1 {
		t.Fatalf("expected 1 model release, got %d", len(body.ModelReleases))
	}
	if body.ModelReleases[0].ModelReleaseID != created.ModelReleaseID {
		t.Fatalf("expected the created release back, got a different one")
	}
}

// TestListModelReleases_NoAuthRequired matches GetModelRelease's existing,
// deliberate unauthenticated posture for this platform-wide registry — the
// same convention as ListModelProviders.
func TestListModelReleases_NoAuthRequired(t *testing.T) {
	r := newTestRouter(newTestHandler())
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/ai/model-releases", nil)
	// Deliberately no X-Principal-Id, no X-Tenant-Id.
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with no auth headers (platform-wide read), got %d - %s", w.Code, w.Body.String())
	}
}

// seedAIRun creates a minimal AI run directly in the stub so disposition
// tests have a real ai_run_id to reference, mirroring how the store-level
// tests seed with seedActiveUseCase/seedActiveModelRelease.
func seedAIRun(h *Handler, tenantID string) string {
	store := h.store.(*stubStore)
	id := store.nextID("ai-run")
	store.aiRuns[id] = &domain.AIRun{AIRunID: id, TenantID: tenantID}
	return id
}

func TestCreateOutputDisposition_RequiresIdempotencyKeyAndRejectsO4(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)
	runID := seedAIRun(h, testTenantA)

	withoutKey := httptest.NewRecorder()
	r.ServeHTTP(withoutKey, buildRequest(http.MethodPost, "/v1/ai/output-dispositions",
		domain.CreateOutputDispositionRequest{AIRunID: runID, OversightClass: "O2"}))
	if withoutKey.Code != http.StatusBadRequest {
		t.Fatalf("expected missing idempotency key to be rejected, got %d — %s", withoutKey.Code, withoutKey.Body.String())
	}

	o4 := buildRequest(http.MethodPost, "/v1/ai/output-dispositions",
		domain.CreateOutputDispositionRequest{AIRunID: runID, OversightClass: "O4"})
	o4.Header.Set("Idempotency-Key", "disposition-o4")
	wO4 := httptest.NewRecorder()
	r.ServeHTTP(wO4, o4)
	if wO4.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected O4 to be refused fail-closed with 422, got %d — %s", wO4.Code, wO4.Body.String())
	}

	notFound := buildRequest(http.MethodPost, "/v1/ai/output-dispositions",
		domain.CreateOutputDispositionRequest{AIRunID: "no-such-run", OversightClass: "O2"})
	notFound.Header.Set("Idempotency-Key", "disposition-missing-run")
	wNF := httptest.NewRecorder()
	r.ServeHTTP(wNF, notFound)
	if wNF.Code != http.StatusNotFound {
		t.Fatalf("expected unknown ai_run_id to 404, got %d — %s", wNF.Code, wNF.Body.String())
	}
}

func TestCreateOutputDisposition_O0IsTerminalDraftAssistive(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)
	runID := seedAIRun(h, testTenantA)

	req := buildRequest(http.MethodPost, "/v1/ai/output-dispositions",
		domain.CreateOutputDispositionRequest{AIRunID: runID, OversightClass: "O0"})
	req.Header.Set("Idempotency-Key", "disposition-o0")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — %s", w.Code, w.Body.String())
	}
	var created domain.AIOutputDisposition
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.Status != string(domain.DispositionDraftAssistive) {
		t.Fatalf("expected O0 to create a terminal DRAFT_ASSISTIVE record, got status %q", created.Status)
	}
}

func TestCreateOutputDisposition_IdempotentReplayAndConflict(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)
	runID := seedAIRun(h, testTenantA)

	create := func(class string) *httptest.ResponseRecorder {
		req := buildRequest(http.MethodPost, "/v1/ai/output-dispositions",
			domain.CreateOutputDispositionRequest{AIRunID: runID, OversightClass: class})
		req.Header.Set("Idempotency-Key", "disposition-replay-key")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	first := create("O2")
	if first.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — %s", first.Code, first.Body.String())
	}
	var firstDisposition domain.AIOutputDisposition
	if err := json.NewDecoder(first.Body).Decode(&firstDisposition); err != nil {
		t.Fatal(err)
	}

	replay := create("O2")
	if replay.Code != http.StatusCreated {
		t.Fatalf("expected replay 201, got %d — %s", replay.Code, replay.Body.String())
	}
	var replayed domain.AIOutputDisposition
	if err := json.NewDecoder(replay.Body).Decode(&replayed); err != nil {
		t.Fatal(err)
	}
	if replayed.DispositionID != firstDisposition.DispositionID {
		t.Fatalf("replay created a second disposition: first=%s replay=%s", firstDisposition.DispositionID, replayed.DispositionID)
	}

	changed := create("O3")
	if changed.Code != http.StatusConflict {
		t.Fatalf("expected changed-body conflict, got %d — %s", changed.Code, changed.Body.String())
	}
	if len(h.store.(*stubStore).dispositions) != 1 {
		t.Fatalf("expected exactly one disposition after replay+conflict, got %d", len(h.store.(*stubStore).dispositions))
	}
}

func TestCreateOutputDisposition_SecondDispositionForSameRunConflicts(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)
	runID := seedAIRun(h, testTenantA)

	first := buildRequest(http.MethodPost, "/v1/ai/output-dispositions",
		domain.CreateOutputDispositionRequest{AIRunID: runID, OversightClass: "O2"})
	first.Header.Set("Idempotency-Key", "disposition-dup-1")
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, first)
	if w1.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — %s", w1.Code, w1.Body.String())
	}

	second := buildRequest(http.MethodPost, "/v1/ai/output-dispositions",
		domain.CreateOutputDispositionRequest{AIRunID: runID, OversightClass: "O2"})
	second.Header.Set("Idempotency-Key", "disposition-dup-2")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, second)
	if w2.Code != http.StatusConflict {
		t.Fatalf("expected a second disposition for the same ai_run_id to conflict, got %d — %s", w2.Code, w2.Body.String())
	}
}

func TestDecideOutputDisposition_SelfApprovalBlockedAndDistinctReviewerSucceeds(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)
	runID := seedAIRun(h, testTenantA)

	create := buildRequest(http.MethodPost, "/v1/ai/output-dispositions",
		domain.CreateOutputDispositionRequest{AIRunID: runID, OversightClass: "O2"})
	create.Header.Set("Idempotency-Key", "disposition-sod-key")
	wCreate := httptest.NewRecorder()
	r.ServeHTTP(wCreate, create)
	var created domain.AIOutputDisposition
	if err := json.NewDecoder(wCreate.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}

	selfDecision := buildRequest(http.MethodPost,
		"/v1/ai/output-dispositions/"+created.DispositionID+"/decision",
		domain.DecideOutputDispositionRequest{Decision: "ACCEPTED"})
	wSelf := httptest.NewRecorder()
	r.ServeHTTP(wSelf, selfDecision)
	if wSelf.Code != http.StatusForbidden {
		t.Fatalf("expected self-approval to be blocked with 403, got %d — %s", wSelf.Code, wSelf.Body.String())
	}

	distinctDecision := buildRequest(http.MethodPost,
		"/v1/ai/output-dispositions/"+created.DispositionID+"/decision",
		domain.DecideOutputDispositionRequest{Decision: "ACCEPTED", Reason: "evidence checked"})
	distinctDecision.Header.Set("X-Principal-Id", "principal-reviewer-02")
	wDistinct := httptest.NewRecorder()
	r.ServeHTTP(wDistinct, distinctDecision)
	if wDistinct.Code != http.StatusOK {
		t.Fatalf("expected distinct reviewer to succeed with 200, got %d — %s", wDistinct.Code, wDistinct.Body.String())
	}
	var decided domain.AIOutputDisposition
	if err := json.NewDecoder(wDistinct.Body).Decode(&decided); err != nil {
		t.Fatal(err)
	}
	if decided.Status != string(domain.DispositionAccepted) {
		t.Fatalf("expected status ACCEPTED, got %q", decided.Status)
	}
	if decided.DecidedByPrincipalID == nil || *decided.DecidedByPrincipalID != "principal-reviewer-02" {
		t.Fatalf("expected decided_by_principal_id to be the distinct reviewer, got %v", decided.DecidedByPrincipalID)
	}

	repeat := buildRequest(http.MethodPost,
		"/v1/ai/output-dispositions/"+created.DispositionID+"/decision",
		domain.DecideOutputDispositionRequest{Decision: "REJECTED"})
	repeat.Header.Set("X-Principal-Id", "principal-reviewer-03")
	wRepeat := httptest.NewRecorder()
	r.ServeHTTP(wRepeat, repeat)
	if wRepeat.Code != http.StatusConflict {
		t.Fatalf("expected deciding an already-decided disposition to 409, got %d — %s", wRepeat.Code, wRepeat.Body.String())
	}
}

func TestDecideOutputDisposition_NotFoundAndUnauthorized(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)

	notFound := buildRequest(http.MethodPost, "/v1/ai/output-dispositions/no-such-id/decision",
		domain.DecideOutputDispositionRequest{Decision: "ACCEPTED"})
	wNF := httptest.NewRecorder()
	r.ServeHTTP(wNF, notFound)
	if wNF.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d — %s", wNF.Code, wNF.Body.String())
	}

	logger, _ := zap.NewDevelopment()
	denied := New(newStubStore(), &stubPublisher{}, &stubAuthz{err: authzpkg.ErrAuthorizationDenied}, &stubKillSwitch{}, logger)
	rDenied := newTestRouter(denied)
	wDenied := httptest.NewRecorder()
	rDenied.ServeHTTP(wDenied, buildRequest(http.MethodPost, "/v1/ai/output-dispositions/anything/decision",
		domain.DecideOutputDispositionRequest{Decision: "ACCEPTED"}))
	if wDenied.Code != http.StatusForbidden {
		t.Fatalf("expected unauthorized principal to be refused with 403, got %d — %s", wDenied.Code, wDenied.Body.String())
	}
}
