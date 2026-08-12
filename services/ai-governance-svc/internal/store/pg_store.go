// Package store provides the PostgreSQL implementation of
// ai-governance-svc's persistence layer.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/ai-governance-svc/internal/domain"
)

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

type Store interface {
	CreateAIRun(ctx context.Context, a *domain.AIRun) error
	GetAIRun(ctx context.Context, aiRunID string) (*domain.AIRun, error)

	SetActionRiskClassification(ctx context.Context, c *domain.ActionRiskClassification) error
	GetActionRiskClassification(ctx context.Context, actionType string) (*domain.ActionRiskClassification, error)

	CreateAutomationPolicy(ctx context.Context, p *domain.AutomationPolicy) error
	ResolveAutomationPolicy(ctx context.Context, tenantID, role, riskCategory, tool, actionType string) (*domain.AutomationPolicyResolution, error)

	ProposeAutomationAction(ctx context.Context, a *domain.AutomationAction) error
	GetAutomationAction(ctx context.Context, automationActionID string) (*domain.AutomationAction, error)
	DecideAutomationAction(ctx context.Context, automationActionID, decision, deciderPrincipalID string) (*domain.AutomationAction, error)

	RegisterModelProvider(ctx context.Context, m *domain.ModelProviderRegistration) error
	GetModelProvider(ctx context.Context, providerName, modelName string) (*domain.ModelProviderRegistration, error)

	ProposePolicyChange(ctx context.Context, p *domain.PolicyChangeApproval) error
	GetPolicyChangeApproval(ctx context.Context, policyChangeApprovalID string) (*domain.PolicyChangeApproval, error)
	DecidePolicyChange(ctx context.Context, policyChangeApprovalID, decision, decidedByPrincipalID, reason string) (*domain.PolicyChangeApproval, error)
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

func marshalStrings(v []string) ([]byte, error) {
	if v == nil {
		v = []string{}
	}
	return json.Marshal(v)
}

func unmarshalStrings(data []byte) []string {
	var out []string
	_ = json.Unmarshal(data, &out)
	return out
}

func (s *PgStore) CreateAIRun(ctx context.Context, a *domain.AIRun) error {
	sourceRefs, err := marshalStrings(a.SourceRefs)
	if err != nil {
		return fmt.Errorf("marshal source_refs: %w", err)
	}
	evidenceRefs, err := marshalStrings(a.EvidenceRefs)
	if err != nil {
		return fmt.Errorf("marshal evidence_refs: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO ai_runs (
			ai_run_id, tenant_id, run_type, model_id, prompt_version, tool_version,
			source_refs, evidence_refs, confidence, limitation, uncertainty_state,
			recommended_action, audit_id, created_at, created_by_principal_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
	`, a.AIRunID, a.TenantID, string(a.RunType), a.ModelID, a.PromptVersion, a.ToolVersion,
		sourceRefs, evidenceRefs, a.Confidence, a.Limitation, string(a.UncertaintyState),
		a.RecommendedAction, a.AuditID, a.CreatedAt, a.CreatedByPrincipalID,
	)
	if err != nil {
		return fmt.Errorf("insert ai run: %w", err)
	}
	return nil
}

func (s *PgStore) GetAIRun(ctx context.Context, aiRunID string) (*domain.AIRun, error) {
	var a domain.AIRun
	var runType, uncertaintyState string
	var sourceRefs, evidenceRefs []byte
	err := s.pool.QueryRow(ctx, `
		SELECT ai_run_id, tenant_id, run_type, model_id, prompt_version, tool_version,
		       source_refs, evidence_refs, confidence, limitation, uncertainty_state,
		       recommended_action, audit_id, created_at, created_by_principal_id
		FROM ai_runs WHERE ai_run_id = $1
	`, aiRunID).Scan(
		&a.AIRunID, &a.TenantID, &runType, &a.ModelID, &a.PromptVersion, &a.ToolVersion,
		&sourceRefs, &evidenceRefs, &a.Confidence, &a.Limitation, &uncertaintyState,
		&a.RecommendedAction, &a.AuditID, &a.CreatedAt, &a.CreatedByPrincipalID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrAIRunNotFound
	}
	if err != nil {
		return nil, err
	}
	a.RunType = domain.AIRunType(runType)
	a.UncertaintyState = domain.UncertaintyState(uncertaintyState)
	a.SourceRefs = unmarshalStrings(sourceRefs)
	a.EvidenceRefs = unmarshalStrings(evidenceRefs)
	return &a, nil
}

func (s *PgStore) SetActionRiskClassification(ctx context.Context, c *domain.ActionRiskClassification) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO action_risk_classifications (
			action_type, risk_category, human_review_trigger, requires_maker_checker,
			created_at, created_by_principal_id
		) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (action_type) DO UPDATE SET
			risk_category = EXCLUDED.risk_category,
			human_review_trigger = EXCLUDED.human_review_trigger,
			requires_maker_checker = EXCLUDED.requires_maker_checker
	`, c.ActionType, string(c.RiskCategory), c.HumanReviewTrigger, c.RequiresMakerChecker,
		c.CreatedAt, c.CreatedByPrincipalID,
	)
	if err != nil {
		return fmt.Errorf("set action risk classification: %w", err)
	}
	return nil
}

func (s *PgStore) GetActionRiskClassification(ctx context.Context, actionType string) (*domain.ActionRiskClassification, error) {
	var c domain.ActionRiskClassification
	var riskCategory string
	err := s.pool.QueryRow(ctx, `
		SELECT action_type, risk_category, human_review_trigger, requires_maker_checker,
		       created_at, created_by_principal_id
		FROM action_risk_classifications WHERE action_type = $1
	`, actionType).Scan(
		&c.ActionType, &riskCategory, &c.HumanReviewTrigger, &c.RequiresMakerChecker,
		&c.CreatedAt, &c.CreatedByPrincipalID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrActionRiskClassificationNotFound
	}
	if err != nil {
		return nil, err
	}
	c.RiskCategory = domain.RiskCategory(riskCategory)
	return &c, nil
}

func (s *PgStore) CreateAutomationPolicy(ctx context.Context, p *domain.AutomationPolicy) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO automation_policies (
			automation_policy_id, tenant_id, role, risk_category, tool, action_type,
			max_scope_amount, required_approvals, dry_run_required, rate_limit_per_day,
			kill_switch_engaged, created_at, created_by_principal_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
	`, p.AutomationPolicyID, p.TenantID, p.Role, string(p.RiskCategory), p.Tool, p.ActionType,
		p.MaxScopeAmount, p.RequiredApprovals, p.DryRunRequired, p.RateLimitPerDay,
		p.KillSwitchEngaged, p.CreatedAt, p.CreatedByPrincipalID,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: an automation policy already exists for this tenant/role/risk-category/tool/action", domain.ErrConflict)
		}
		return fmt.Errorf("insert automation policy: %w", err)
	}
	return nil
}

// ResolveAutomationPolicy is doc7 §G7's core check: may this tenant/role/tool
// autonomously perform this action right now. Fail-closed — absence of a
// matching row means NOT_ALLOWLISTED, never a default allow.
func (s *PgStore) ResolveAutomationPolicy(ctx context.Context, tenantID, role, riskCategory, tool, actionType string) (*domain.AutomationPolicyResolution, error) {
	var killSwitchEngaged bool
	err := s.pool.QueryRow(ctx, `
		SELECT kill_switch_engaged FROM automation_policies
		WHERE tenant_id = $1 AND role = $2 AND risk_category = $3 AND tool = $4 AND action_type = $5
	`, tenantID, role, riskCategory, tool, actionType).Scan(&killSwitchEngaged)
	if errors.Is(err, pgx.ErrNoRows) {
		return &domain.AutomationPolicyResolution{Allowed: false, ReasonCode: "NOT_ALLOWLISTED"}, nil
	}
	if err != nil {
		return nil, err
	}
	if killSwitchEngaged {
		return &domain.AutomationPolicyResolution{Allowed: false, ReasonCode: "KILL_SWITCH_ENGAGED"}, nil
	}
	return &domain.AutomationPolicyResolution{Allowed: true, ReasonCode: "ALLOWED"}, nil
}

func (s *PgStore) ProposeAutomationAction(ctx context.Context, a *domain.AutomationAction) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO automation_actions (
			automation_action_id, tenant_id, action_type, risk_category, idempotency_key,
			preconditions_met, approval_status, postcondition_verified, rollback_plan,
			status, proposed_by_principal_id, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $12)
	`, a.AutomationActionID, a.TenantID, a.ActionType, string(a.RiskCategory), a.IdempotencyKey,
		a.PreconditionsMet, string(a.ApprovalStatus), a.PostconditionVerified, a.RollbackPlan,
		string(a.Status), a.ProposedByPrincipalID, a.CreatedAt,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: idempotency_key %s", domain.ErrDuplicateIdempotencyKey, a.IdempotencyKey)
		}
		return fmt.Errorf("insert automation action: %w", err)
	}
	return nil
}

func (s *PgStore) GetAutomationAction(ctx context.Context, automationActionID string) (*domain.AutomationAction, error) {
	var a domain.AutomationAction
	var riskCategory, approvalStatus, status string
	err := s.pool.QueryRow(ctx, `
		SELECT automation_action_id, tenant_id, action_type, risk_category, idempotency_key,
		       preconditions_met, approval_status, postcondition_verified, rollback_plan,
		       status, proposed_by_principal_id, approved_by_principal_id, created_at, updated_at
		FROM automation_actions WHERE automation_action_id = $1
	`, automationActionID).Scan(
		&a.AutomationActionID, &a.TenantID, &a.ActionType, &riskCategory, &a.IdempotencyKey,
		&a.PreconditionsMet, &approvalStatus, &a.PostconditionVerified, &a.RollbackPlan,
		&status, &a.ProposedByPrincipalID, &a.ApprovedByPrincipalID, &a.CreatedAt, &a.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrAutomationActionNotFound
	}
	if err != nil {
		return nil, err
	}
	a.RiskCategory = domain.RiskCategory(riskCategory)
	a.ApprovalStatus = domain.ApprovalStatus(approvalStatus)
	a.Status = domain.AutomationActionStatus(status)
	return &a, nil
}

// DecideAutomationAction records a maker-checker decision on a PENDING
// automation action. Self-approval blocking (deciderPrincipalID must differ
// from the original proposer) is enforced by the caller (handler) before
// this is invoked, since the store layer alone can't distinguish "no rows
// changed because already decided" from "no rows changed because blocked."
func (s *PgStore) DecideAutomationAction(ctx context.Context, automationActionID, decision, deciderPrincipalID string) (*domain.AutomationAction, error) {
	now := time.Now().UTC()
	newStatus := domain.AutomationActionApproved
	if decision == string(domain.ApprovalRejected) {
		newStatus = domain.AutomationActionRejected
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE automation_actions
		SET approval_status = $1, status = $2, approved_by_principal_id = $3, updated_at = $4
		WHERE automation_action_id = $5 AND approval_status = 'PENDING'
	`, decision, string(newStatus), deciderPrincipalID, now, automationActionID)
	if err != nil {
		return nil, fmt.Errorf("decide automation action: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, domain.ErrInvalidDecision
	}
	return s.GetAutomationAction(ctx, automationActionID)
}

func (s *PgStore) RegisterModelProvider(ctx context.Context, m *domain.ModelProviderRegistration) error {
	dataClasses, err := marshalStrings(m.ApprovedDataClasses)
	if err != nil {
		return fmt.Errorf("marshal approved_data_classes: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO model_provider_registrations (
			provider_registration_id, provider_name, model_name, training_use_posture,
			retention_policy_ref, data_region, dpa_verified, approved_data_classes,
			approved_at, approved_by_principal_id, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (provider_name, model_name) DO UPDATE SET
			training_use_posture = EXCLUDED.training_use_posture,
			retention_policy_ref = EXCLUDED.retention_policy_ref,
			data_region = EXCLUDED.data_region,
			dpa_verified = EXCLUDED.dpa_verified,
			approved_data_classes = EXCLUDED.approved_data_classes,
			approved_at = EXCLUDED.approved_at,
			approved_by_principal_id = EXCLUDED.approved_by_principal_id
	`, m.ProviderRegistrationID, m.ProviderName, m.ModelName, string(m.TrainingUsePosture),
		m.RetentionPolicyRef, m.DataRegion, m.DPAVerified, dataClasses,
		m.ApprovedAt, m.ApprovedByPrincipalID, m.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("register model provider: %w", err)
	}
	return nil
}

func (s *PgStore) GetModelProvider(ctx context.Context, providerName, modelName string) (*domain.ModelProviderRegistration, error) {
	var m domain.ModelProviderRegistration
	var trainingUsePosture string
	var dataClasses []byte
	err := s.pool.QueryRow(ctx, `
		SELECT provider_registration_id, provider_name, model_name, training_use_posture,
		       retention_policy_ref, data_region, dpa_verified, approved_data_classes,
		       approved_at, approved_by_principal_id, created_at
		FROM model_provider_registrations WHERE provider_name = $1 AND model_name = $2
	`, providerName, modelName).Scan(
		&m.ProviderRegistrationID, &m.ProviderName, &m.ModelName, &trainingUsePosture,
		&m.RetentionPolicyRef, &m.DataRegion, &m.DPAVerified, &dataClasses,
		&m.ApprovedAt, &m.ApprovedByPrincipalID, &m.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrModelProviderNotFound
	}
	if err != nil {
		return nil, err
	}
	m.TrainingUsePosture = domain.TrainingUsePosture(trainingUsePosture)
	m.ApprovedDataClasses = unmarshalStrings(dataClasses)
	return &m, nil
}

func (s *PgStore) ProposePolicyChange(ctx context.Context, p *domain.PolicyChangeApproval) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO policy_change_approvals (
			policy_change_approval_id, target_policy_ref, proposed_change,
			proposed_by_principal_id, decision, created_at
		) VALUES ($1, $2, $3, $4, $5, $6)
	`, p.PolicyChangeApprovalID, p.TargetPolicyRef, p.ProposedChange,
		p.ProposedByPrincipalID, string(p.Decision), p.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert policy change approval: %w", err)
	}
	return nil
}

func (s *PgStore) GetPolicyChangeApproval(ctx context.Context, policyChangeApprovalID string) (*domain.PolicyChangeApproval, error) {
	var p domain.PolicyChangeApproval
	var decision string
	err := s.pool.QueryRow(ctx, `
		SELECT policy_change_approval_id, target_policy_ref, proposed_change,
		       proposed_by_principal_id, decision, decided_by_principal_id,
		       decision_reason, decided_at, created_at
		FROM policy_change_approvals WHERE policy_change_approval_id = $1
	`, policyChangeApprovalID).Scan(
		&p.PolicyChangeApprovalID, &p.TargetPolicyRef, &p.ProposedChange,
		&p.ProposedByPrincipalID, &decision, &p.DecidedByPrincipalID,
		&p.DecisionReason, &p.DecidedAt, &p.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrPolicyChangeApprovalNotFound
	}
	if err != nil {
		return nil, err
	}
	p.Decision = domain.PolicyChangeDecision(decision)
	return &p, nil
}

// DecidePolicyChange enforces doc7 §G3/§H3's mandatory self-approval block
// at the one point it can actually be checked — decision time, since the
// decider identity isn't known when the change was proposed.
func (s *PgStore) DecidePolicyChange(ctx context.Context, policyChangeApprovalID, decision, decidedByPrincipalID, reason string) (*domain.PolicyChangeApproval, error) {
	existing, err := s.GetPolicyChangeApproval(ctx, policyChangeApprovalID)
	if err != nil {
		return nil, err
	}
	if existing.ProposedByPrincipalID == decidedByPrincipalID {
		return nil, domain.ErrSelfApprovalBlocked
	}

	now := time.Now().UTC()
	var reasonPtr *string
	if reason != "" {
		reasonPtr = &reason
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE policy_change_approvals
		SET decision = $1, decided_by_principal_id = $2, decision_reason = $3, decided_at = $4
		WHERE policy_change_approval_id = $5 AND decision = 'PENDING'
	`, decision, decidedByPrincipalID, reasonPtr, now, policyChangeApprovalID)
	if err != nil {
		return nil, fmt.Errorf("decide policy change: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, domain.ErrInvalidDecision
	}
	return s.GetPolicyChangeApproval(ctx, policyChangeApprovalID)
}
