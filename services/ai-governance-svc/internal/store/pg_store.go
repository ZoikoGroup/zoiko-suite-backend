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
	"zoiko.io/ai-governance-svc/internal/middleware"
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

	// AIG-01: AI Use-Case, Risk & Impact Registry (additive; see domain/types.go).
	CreateUseCase(ctx context.Context, req domain.CreateUseCaseRequest, tenantID, actor, clientRequestID string) (*domain.AIUseCase, error)
	GetUseCase(ctx context.Context, useCaseID string) (*domain.AIUseCase, error)
	StartAssessment(ctx context.Context, useCaseID string, req domain.StartAssessmentRequest, actor string) (*domain.AIImpactAssessment, error)
	DecideAssessment(ctx context.Context, assessmentID, decision, decidedByPrincipalID, reason string) (*domain.AIImpactAssessment, error)
	ActivateUseCase(ctx context.Context, useCaseID string, req domain.ActivateUseCaseRequest) (*domain.AIUseCase, error)
	SuspendUseCase(ctx context.Context, useCaseID string, req domain.SuspendUseCaseRequest) (*domain.AIUseCase, error)
	RequestReassessment(ctx context.Context, useCaseID string, req domain.RequestReassessmentRequest) (*domain.AIUseCase, error)
	RetireUseCase(ctx context.Context, useCaseID string, req domain.RetireUseCaseRequest) (*domain.AIUseCase, error)
	GetEffectiveUseCaseControl(ctx context.Context, useCaseID string) (*domain.EffectiveUseCaseControl, error)

	// AIG-02: Model, Provider & Capability Registry (additive; platform-wide, no tenant_id).
	RegisterModelRelease(ctx context.Context, req domain.RegisterModelReleaseRequest, actor string) (*domain.AIModelRelease, error)
	GetModelRelease(ctx context.Context, modelReleaseID string) (*domain.AIModelRelease, error)
	RecordDueDiligence(ctx context.Context, modelReleaseID string, req domain.AdvanceReleaseRequest, actor string) (*domain.AIModelRelease, error)
	RecordEvaluation(ctx context.Context, modelReleaseID string, req domain.AdvanceReleaseRequest, actor string) (*domain.AIModelRelease, error)
	ApproveRelease(ctx context.Context, modelReleaseID string, req domain.ApproveReleaseRequest, actor string) (*domain.AIModelRelease, error)
	RejectRelease(ctx context.Context, modelReleaseID string, req domain.AdvanceReleaseRequest, actor string) (*domain.AIModelRelease, error)
	BlockRelease(ctx context.Context, modelReleaseID string, req domain.AdvanceReleaseRequest, actor string) (*domain.AIModelRelease, error)
	ActivateRelease(ctx context.Context, modelReleaseID string, actor string) (*domain.AIModelRelease, error)
	RestrictRelease(ctx context.Context, modelReleaseID string, req domain.AdvanceReleaseRequest, actor string) (*domain.AIModelRelease, error)
	UnrestrictRelease(ctx context.Context, modelReleaseID string, actor string) (*domain.AIModelRelease, error)
	QuarantineRelease(ctx context.Context, modelReleaseID string, req domain.AdvanceReleaseRequest, actor string) (*domain.AIModelRelease, error)
	RetireRelease(ctx context.Context, modelReleaseID string, req domain.AdvanceReleaseRequest, actor string) (*domain.AIModelRelease, error)
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

// withTenant runs fn inside a transaction with app.tenant_id set from the
// request context, so the tenant_isolation_policy added in migration
// 000002_add_rls.sql has a value to enforce against.
//
// Only the three tenant-scoped tables go through this: ai_runs,
// automation_policies and automation_actions. The other three carry no
// tenant_id and are correct that way — action_risk_classifications is the
// platform risk taxonomy, model_provider_registrations the provider
// registry, and policy_change_approvals is platform governance because
// doc7 §G3 says policy changes "alter governance truth across tenants".
// Those methods deliberately stay on s.pool; wrapping them would imply a
// tenant boundary the doc does not want.
//
// The tenant comes from context, set by middleware.TenantContext from a
// gateway-verified X-Tenant-Id and enforced by Handler.requireTenant. It
// is never read from a request body or query string — which is exactly
// what this service used to do, letting a caller name the tenant whose
// autonomy allowlist it was creating or resolving against.
func (s *PgStore) withTenant(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback error discarded intentionally on commit path

	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.tenant_id', $1, true)", middleware.TenantFromContext(ctx),
	); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
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
	err = s.withTenant(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO ai_runs (
				ai_run_id, tenant_id, run_type, model_id, prompt_version, tool_version,
				source_refs, evidence_refs, confidence, limitation, uncertainty_state,
				recommended_action, audit_id, created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		`, a.AIRunID, a.TenantID, string(a.RunType), a.ModelID, a.PromptVersion, a.ToolVersion,
			sourceRefs, evidenceRefs, a.Confidence, a.Limitation, string(a.UncertaintyState),
			a.RecommendedAction, a.AuditID, a.CreatedAt, a.CreatedByPrincipalID,
		)
		return err
	})
	if err != nil {
		return fmt.Errorf("insert ai run: %w", err)
	}
	return nil
}

// GetAIRun reads one AI run scoped to the caller's tenant.
//
// The tenant predicate is new. This query was `WHERE ai_run_id = $1`
// alone, so any caller holding or guessing an ai_run_id could read another
// tenant's model_id, prompt_version, source_refs, evidence_refs and
// recommended_action — how a governed decision was reached and what
// evidence it rested on. Returns ErrAIRunNotFound rather than a distinct
// forbidden error, so a probe cannot confirm the id exists.
func (s *PgStore) GetAIRun(ctx context.Context, aiRunID string) (*domain.AIRun, error) {
	var a domain.AIRun
	var runType, uncertaintyState string
	var sourceRefs, evidenceRefs []byte
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT ai_run_id, tenant_id, run_type, model_id, prompt_version, tool_version,
			       source_refs, evidence_refs, confidence, limitation, uncertainty_state,
			       recommended_action, audit_id, created_at, created_by_principal_id
			FROM ai_runs
			WHERE ai_run_id = $1
			  AND tenant_id = $2
		`, aiRunID, middleware.TenantFromContext(ctx)).Scan(
			&a.AIRunID, &a.TenantID, &runType, &a.ModelID, &a.PromptVersion, &a.ToolVersion,
			&sourceRefs, &evidenceRefs, &a.Confidence, &a.Limitation, &uncertaintyState,
			&a.RecommendedAction, &a.AuditID, &a.CreatedAt, &a.CreatedByPrincipalID,
		)
	})
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
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO automation_policies (
				automation_policy_id, tenant_id, role, risk_category, tool, action_type,
				max_scope_amount, required_approvals, dry_run_required, rate_limit_per_day,
				kill_switch_engaged, created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		`, p.AutomationPolicyID, p.TenantID, p.Role, string(p.RiskCategory), p.Tool, p.ActionType,
			p.MaxScopeAmount, p.RequiredApprovals, p.DryRunRequired, p.RateLimitPerDay,
			p.KillSwitchEngaged, p.CreatedAt, p.CreatedByPrincipalID,
		)
		return err
	})
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
//
// The tenantID parameter is retained, but the handler now sources it from
// the verified X-Tenant-Id rather than a ?tenant_id= query param, and RLS
// is a second gate beneath it: if a future caller passed a foreign tenant
// here, the policy would match no row and this fails closed to
// NOT_ALLOWLISTED rather than leaking that tenant's kill-switch state.
func (s *PgStore) ResolveAutomationPolicy(ctx context.Context, tenantID, role, riskCategory, tool, actionType string) (*domain.AutomationPolicyResolution, error) {
	var killSwitchEngaged bool
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT kill_switch_engaged FROM automation_policies
			WHERE tenant_id = $1 AND role = $2 AND risk_category = $3 AND tool = $4 AND action_type = $5
		`, tenantID, role, riskCategory, tool, actionType).Scan(&killSwitchEngaged)
	})
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
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO automation_actions (
				automation_action_id, tenant_id, action_type, risk_category, idempotency_key,
				preconditions_met, approval_status, postcondition_verified, rollback_plan,
				status, proposed_by_principal_id, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $12)
		`, a.AutomationActionID, a.TenantID, a.ActionType, string(a.RiskCategory), a.IdempotencyKey,
			a.PreconditionsMet, string(a.ApprovalStatus), a.PostconditionVerified, a.RollbackPlan,
			string(a.Status), a.ProposedByPrincipalID, a.CreatedAt,
		)
		return err
	})
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: idempotency_key %s", domain.ErrDuplicateIdempotencyKey, a.IdempotencyKey)
		}
		return fmt.Errorf("insert automation action: %w", err)
	}
	return nil
}

// GetAutomationAction reads one proposed or executed autonomous action
// scoped to the caller's tenant.
//
// The tenant predicate is new, and it also guards the decision path:
// DecideAutomationAction fetches through here first, so a foreign action
// id now 404s before any approval is possible.
func (s *PgStore) GetAutomationAction(ctx context.Context, automationActionID string) (*domain.AutomationAction, error) {
	var a domain.AutomationAction
	var riskCategory, approvalStatus, status string
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT automation_action_id, tenant_id, action_type, risk_category, idempotency_key,
			       preconditions_met, approval_status, postcondition_verified, rollback_plan,
			       status, proposed_by_principal_id, approved_by_principal_id, created_at, updated_at
			FROM automation_actions
			WHERE automation_action_id = $1
			  AND tenant_id = $2
		`, automationActionID, middleware.TenantFromContext(ctx)).Scan(
			&a.AutomationActionID, &a.TenantID, &a.ActionType, &riskCategory, &a.IdempotencyKey,
			&a.PreconditionsMet, &approvalStatus, &a.PostconditionVerified, &a.RollbackPlan,
			&status, &a.ProposedByPrincipalID, &a.ApprovedByPrincipalID, &a.CreatedAt, &a.UpdatedAt,
		)
	})
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
//
// The tenant predicate is new and this is the service's most consequential
// write: approving here is what authorizes an autonomous action to
// execute. The UPDATE was `WHERE automation_action_id = $5 AND
// approval_status = 'PENDING'`, so a caller holding another tenant's
// action id could approve that tenant's pending autonomous action — grant
// agentic authority inside someone else's tenant. The handler's fetch is
// now tenant-scoped too, but the predicate is here as well so this does
// not depend on the handler's call ordering staying as it is.
func (s *PgStore) DecideAutomationAction(ctx context.Context, automationActionID, decision, deciderPrincipalID string) (*domain.AutomationAction, error) {
	now := time.Now().UTC()
	newStatus := domain.AutomationActionApproved
	if decision == string(domain.ApprovalRejected) {
		newStatus = domain.AutomationActionRejected
	}
	var rowsAffected int64
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE automation_actions
			SET approval_status = $1, status = $2, approved_by_principal_id = $3, updated_at = $4
			WHERE automation_action_id = $5
			  AND tenant_id = $6
			  AND approval_status = 'PENDING'
		`, decision, string(newStatus), deciderPrincipalID, now, automationActionID,
			middleware.TenantFromContext(ctx))
		if err != nil {
			return err
		}
		rowsAffected = tag.RowsAffected()
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("decide automation action: %w", err)
	}
	if rowsAffected == 0 {
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

// ─────────────────────────────────────────────────────────────────────────────
// AIG-01: AI Use-Case, Risk & Impact Registry — additive, does not touch
// the doc7-based methods above.
// ─────────────────────────────────────────────────────────────────────────────

func marshalJSON(v interface{}) ([]byte, error) {
	if v == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(v)
}

func unmarshalJSONMap(data []byte) map[string]interface{} {
	out := map[string]interface{}{}
	_ = json.Unmarshal(data, &out)
	return out
}

func marshalHumanRole(h domain.HumanRole) ([]byte, error) {
	return json.Marshal(h)
}

func unmarshalHumanRole(data []byte) domain.HumanRole {
	var h domain.HumanRole
	_ = json.Unmarshal(data, &h)
	return h
}

const useCaseColumns = `use_case_id, tenant_id, domain, purpose, outcome_type, operational_class,
	legal_classification_ref, owner_principal_id, business_outcome, affected_decisions, data_profile,
	automation_level, human_role, fallback, success_measures, prohibited_boundary, retirement_criteria,
	lifecycle_state, created_at, created_by_principal_id, updated_at`

func scanUseCase(row pgx.Row) (*domain.AIUseCase, error) {
	var u domain.AIUseCase
	var operationalClass, automationLevel, lifecycleState string
	var legalRef *string
	var affectedDecisions, dataProfile, humanRole []byte
	if err := row.Scan(&u.UseCaseID, &u.TenantID, &u.Domain, &u.Purpose, &u.OutcomeType, &operationalClass,
		&legalRef, &u.OwnerPrincipalID, &u.BusinessOutcome, &affectedDecisions, &dataProfile,
		&automationLevel, &humanRole, &u.Fallback, &u.SuccessMeasures, &u.ProhibitedBoundary, &u.RetirementCriteria,
		&lifecycleState, &u.CreatedAt, &u.CreatedByPrincipalID, &u.UpdatedAt); err != nil {
		return nil, err
	}
	u.OperationalClass = domain.OperationalClass(operationalClass)
	u.AutomationLevel = domain.AutomationLevel(automationLevel)
	u.LifecycleState = domain.UseCaseLifecycleState(lifecycleState)
	if legalRef != nil {
		u.LegalClassificationRef = *legalRef
	}
	u.AffectedDecisions = unmarshalStrings(affectedDecisions)
	u.DataProfile = unmarshalJSONMap(dataProfile)
	u.HumanRole = unmarshalHumanRole(humanRole)
	return &u, nil
}

// CreateUseCase registers a new AI use case in DRAFT. client_request_id
// gives idempotent create semantics via a unique partial index (see
// migration) scoped per tenant.
func (s *PgStore) CreateUseCase(ctx context.Context, req domain.CreateUseCaseRequest, tenantID, actor, clientRequestID string) (*domain.AIUseCase, error) {
	affectedDecisions, err := marshalStrings(req.AffectedDecisions)
	if err != nil {
		return nil, fmt.Errorf("marshal affected_decisions: %w", err)
	}
	dataProfile, err := marshalJSON(req.DataProfile)
	if err != nil {
		return nil, fmt.Errorf("marshal data_profile: %w", err)
	}
	humanRole, err := marshalHumanRole(req.HumanRole)
	if err != nil {
		return nil, fmt.Errorf("marshal human_role: %w", err)
	}
	var legalRef *string
	if req.LegalClassificationRef != "" {
		legalRef = &req.LegalClassificationRef
	}
	var clientRequestIDPtr *string
	if clientRequestID != "" {
		clientRequestIDPtr = &clientRequestID
	}
	var out *domain.AIUseCase
	err = s.withTenant(ctx, func(tx pgx.Tx) error {
		var newID string
		insertErr := tx.QueryRow(ctx, `
			INSERT INTO ai_use_cases (tenant_id, domain, purpose, outcome_type, operational_class,
				legal_classification_ref, owner_principal_id, business_outcome, affected_decisions, data_profile,
				automation_level, human_role, fallback, success_measures, prohibited_boundary, retirement_criteria,
				client_request_id, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
			ON CONFLICT (tenant_id, client_request_id) WHERE client_request_id IS NOT NULL DO NOTHING
			RETURNING use_case_id`,
			tenantID, req.Domain, req.Purpose, req.OutcomeType, req.OperationalClass,
			legalRef, req.OwnerPrincipalID, req.BusinessOutcome, affectedDecisions, dataProfile,
			req.AutomationLevel, humanRole, req.Fallback, req.SuccessMeasures, req.ProhibitedBoundary, req.RetirementCriteria,
			clientRequestIDPtr, actor).Scan(&newID)

		var useCaseID string
		if errors.Is(insertErr, pgx.ErrNoRows) {
			if clientRequestID == "" {
				return fmt.Errorf("insert ai use case: no row returned and no client_request_id to replay")
			}
			if err := tx.QueryRow(ctx, `SELECT use_case_id FROM ai_use_cases WHERE tenant_id = $1 AND client_request_id = $2`,
				tenantID, clientRequestID).Scan(&useCaseID); err != nil {
				return fmt.Errorf("replay lookup for client_request_id %s: %w", clientRequestID, err)
			}
		} else if insertErr != nil {
			return fmt.Errorf("insert ai use case: %w", insertErr)
		} else {
			useCaseID = newID
		}

		u, err := scanUseCase(tx.QueryRow(ctx, `SELECT `+useCaseColumns+` FROM ai_use_cases WHERE use_case_id = $1`, useCaseID))
		if err != nil {
			return fmt.Errorf("load created use case: %w", err)
		}
		out = u
		return nil
	})
	return out, err
}

func (s *PgStore) GetUseCase(ctx context.Context, useCaseID string) (*domain.AIUseCase, error) {
	var out *domain.AIUseCase
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		u, err := scanUseCase(tx.QueryRow(ctx, `SELECT `+useCaseColumns+` FROM ai_use_cases WHERE use_case_id = $1 AND tenant_id = $2`,
			useCaseID, middleware.TenantFromContext(ctx)))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrUseCaseNotFound
		}
		out = u
		return err
	})
	return out, err
}

func loadUseCaseForUpdate(ctx context.Context, tx pgx.Tx, tenantID, useCaseID string) (*domain.AIUseCase, error) {
	u, err := scanUseCase(tx.QueryRow(ctx, `SELECT `+useCaseColumns+` FROM ai_use_cases WHERE use_case_id = $1 AND tenant_id = $2 FOR UPDATE`,
		useCaseID, tenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrUseCaseNotFound
	}
	return u, err
}

const assessmentColumns = `assessment_id, use_case_id, tenant_id, version, affected_groups, rights_impact,
	financial_impact, employment_impact, mitigations, approvers, decision, decided_by_principal_id,
	decision_reason, decided_at, expires_at, created_at, created_by_principal_id`

func scanAssessment(row pgx.Row) (*domain.AIImpactAssessment, error) {
	var a domain.AIImpactAssessment
	var decision string
	var affectedGroups, approvers []byte
	if err := row.Scan(&a.AssessmentID, &a.UseCaseID, &a.TenantID, &a.Version, &affectedGroups, &a.RightsImpact,
		&a.FinancialImpact, &a.EmploymentImpact, &a.Mitigations, &approvers, &decision, &a.DecidedByPrincipalID,
		&a.DecisionReason, &a.DecidedAt, &a.ExpiresAt, &a.CreatedAt, &a.CreatedByPrincipalID); err != nil {
		return nil, err
	}
	a.Decision = domain.AssessmentDecision(decision)
	a.AffectedGroups = unmarshalStrings(affectedGroups)
	a.Approvers = unmarshalStrings(approvers)
	return &a, nil
}

// StartAssessment creates a new, versioned AIImpactAssessment and moves
// the use case into ASSESSING — valid from DRAFT, APPROVED, ACTIVE,
// LIMITED or SUSPENDED (material-change reassessment), enforced by the
// migration's forward-only trigger.
func (s *PgStore) StartAssessment(ctx context.Context, useCaseID string, req domain.StartAssessmentRequest, actor string) (*domain.AIImpactAssessment, error) {
	affectedGroups, err := marshalStrings(req.AffectedGroups)
	if err != nil {
		return nil, fmt.Errorf("marshal affected_groups: %w", err)
	}
	approvers, err := marshalStrings(req.Approvers)
	if err != nil {
		return nil, fmt.Errorf("marshal approvers: %w", err)
	}
	var out *domain.AIImpactAssessment
	err = s.withTenant(ctx, func(tx pgx.Tx) error {
		tenantID := middleware.TenantFromContext(ctx)
		uc, err := loadUseCaseForUpdate(ctx, tx, tenantID, useCaseID)
		if err != nil {
			return err
		}
		if uc.LifecycleState != domain.UseCaseDraft && uc.LifecycleState != domain.UseCaseApproved &&
			uc.LifecycleState != domain.UseCaseActive && uc.LifecycleState != domain.UseCaseLimited &&
			uc.LifecycleState != domain.UseCaseSuspended {
			return domain.ErrUseCaseNotAssessable
		}

		var nextVersion int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) + 1 FROM ai_impact_assessments WHERE use_case_id = $1`,
			useCaseID).Scan(&nextVersion); err != nil {
			return fmt.Errorf("compute next assessment version: %w", err)
		}

		a, err := scanAssessment(tx.QueryRow(ctx, `
			INSERT INTO ai_impact_assessments (use_case_id, tenant_id, version, affected_groups, rights_impact,
				financial_impact, employment_impact, mitigations, approvers, expires_at, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			RETURNING `+assessmentColumns,
			useCaseID, tenantID, nextVersion, affectedGroups, req.RightsImpact,
			req.FinancialImpact, req.EmploymentImpact, req.Mitigations, approvers, req.ExpiresAt, actor))
		if err != nil {
			return fmt.Errorf("insert ai impact assessment: %w", err)
		}

		if _, err := tx.Exec(ctx, `UPDATE ai_use_cases SET lifecycle_state = 'ASSESSING' WHERE use_case_id = $1`, useCaseID); err != nil {
			return fmt.Errorf("move use case to ASSESSING: %w", err)
		}
		out = a
		return nil
	})
	return out, err
}

func loadAssessmentForUpdate(ctx context.Context, tx pgx.Tx, assessmentID string) (*domain.AIImpactAssessment, error) {
	a, err := scanAssessment(tx.QueryRow(ctx, `SELECT `+assessmentColumns+` FROM ai_impact_assessments WHERE assessment_id = $1 FOR UPDATE`,
		assessmentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrAssessmentNotFound
	}
	return a, err
}

// DecideAssessment finalizes a PENDING assessment as APPROVED or
// REJECTED and moves the owning use case accordingly — not a named
// AIG-01 API endpoint in §4.4, but required to complete the documented
// ASSESSING -> APPROVED/REJECTED lifecycle hop.
func (s *PgStore) DecideAssessment(ctx context.Context, assessmentID, decision, decidedByPrincipalID, reason string) (*domain.AIImpactAssessment, error) {
	if decision != string(domain.AssessmentApproved) && decision != string(domain.AssessmentRejected) {
		return nil, domain.ErrInvalidDecision
	}
	var out *domain.AIImpactAssessment
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		a, err := loadAssessmentForUpdate(ctx, tx, assessmentID)
		if err != nil {
			return err
		}
		if a.Decision != domain.AssessmentPending {
			return domain.ErrAssessmentNotPending
		}
		now := time.Now().UTC()
		var reasonPtr *string
		if reason != "" {
			reasonPtr = &reason
		}
		updated, err := scanAssessment(tx.QueryRow(ctx, `
			UPDATE ai_impact_assessments SET decision = $2, decided_by_principal_id = $3, decision_reason = $4, decided_at = $5
			WHERE assessment_id = $1 RETURNING `+assessmentColumns,
			assessmentID, decision, decidedByPrincipalID, reasonPtr, now))
		if err != nil {
			return fmt.Errorf("decide assessment: %w", err)
		}

		useCaseLifecycle := "APPROVED"
		if decision == string(domain.AssessmentRejected) {
			useCaseLifecycle = "REJECTED"
		}
		if _, err := tx.Exec(ctx, `UPDATE ai_use_cases SET lifecycle_state = $2 WHERE use_case_id = $1`,
			a.UseCaseID, useCaseLifecycle); err != nil {
			return fmt.Errorf("move use case to %s: %w", useCaseLifecycle, err)
		}
		out = updated
		return nil
	})
	return out, err
}

// ActivateUseCase enforces §4.5's failure rules: legal classification
// must be resolved for potentially high-impact classes, A4 is
// prohibited-disabled and can never activate, and the latest
// assessment must be APPROVED and not expired.
func (s *PgStore) ActivateUseCase(ctx context.Context, useCaseID string, req domain.ActivateUseCaseRequest) (*domain.AIUseCase, error) {
	var out *domain.AIUseCase
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		tenantID := middleware.TenantFromContext(ctx)
		uc, err := loadUseCaseForUpdate(ctx, tx, tenantID, useCaseID)
		if err != nil {
			return err
		}
		if uc.LifecycleState != domain.UseCaseApproved && uc.LifecycleState != domain.UseCaseLimited {
			return domain.ErrUseCaseNotActivatable
		}
		if uc.OperationalClass == domain.OperationalClassA4 {
			return domain.ErrOperationalClassProhibited
		}
		if uc.OperationalClass.IsPotentiallyHighImpact() &&
			(uc.LegalClassificationRef == "" || uc.LegalClassificationRef == "INDETERMINATE") {
			return domain.ErrLegalClassificationIndeterminate
		}

		var decision string
		var expiresAt *time.Time
		err = tx.QueryRow(ctx, `
			SELECT decision, expires_at FROM ai_impact_assessments
			WHERE use_case_id = $1 ORDER BY version DESC LIMIT 1`, useCaseID).Scan(&decision, &expiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrAssessmentNotApproved
		}
		if err != nil {
			return fmt.Errorf("load latest assessment: %w", err)
		}
		if decision != string(domain.AssessmentApproved) {
			return domain.ErrAssessmentNotApproved
		}
		if expiresAt != nil && expiresAt.Before(time.Now().UTC()) {
			return domain.ErrAssessmentExpired
		}

		targetState := "ACTIVE"
		if req.Limited {
			targetState = "LIMITED"
		}
		updated, err := scanUseCase(tx.QueryRow(ctx, `UPDATE ai_use_cases SET lifecycle_state = $2 WHERE use_case_id = $1 RETURNING `+useCaseColumns,
			useCaseID, targetState))
		if err != nil {
			return fmt.Errorf("activate use case: %w", err)
		}
		out = updated
		return nil
	})
	return out, err
}

// SuspendUseCase provides immediate containment — valid from ACTIVE or
// LIMITED only, reason mandatory (enforced by the handler/domain
// validation before this is called).
func (s *PgStore) SuspendUseCase(ctx context.Context, useCaseID string, req domain.SuspendUseCaseRequest) (*domain.AIUseCase, error) {
	var out *domain.AIUseCase
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		tenantID := middleware.TenantFromContext(ctx)
		uc, err := loadUseCaseForUpdate(ctx, tx, tenantID, useCaseID)
		if err != nil {
			return err
		}
		if uc.LifecycleState != domain.UseCaseActive && uc.LifecycleState != domain.UseCaseLimited {
			return domain.ErrUseCaseNotSuspendable
		}
		updated, err := scanUseCase(tx.QueryRow(ctx, `UPDATE ai_use_cases SET lifecycle_state = 'SUSPENDED' WHERE use_case_id = $1 RETURNING `+useCaseColumns,
			useCaseID))
		if err != nil {
			return fmt.Errorf("suspend use case: %w", err)
		}
		out = updated
		return nil
	})
	return out, err
}

// RequestReassessment is §4.3's material-change trigger: model
// capability, purpose, data, affected outcome, tool authority,
// jurisdiction, provider terms, oversight design or materially
// different prompt/agent behavior forces a return to ASSESSING from
// any non-terminal, non-draft state.
func (s *PgStore) RequestReassessment(ctx context.Context, useCaseID string, req domain.RequestReassessmentRequest) (*domain.AIUseCase, error) {
	var out *domain.AIUseCase
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		tenantID := middleware.TenantFromContext(ctx)
		uc, err := loadUseCaseForUpdate(ctx, tx, tenantID, useCaseID)
		if err != nil {
			return err
		}
		if uc.LifecycleState != domain.UseCaseApproved && uc.LifecycleState != domain.UseCaseActive &&
			uc.LifecycleState != domain.UseCaseLimited && uc.LifecycleState != domain.UseCaseSuspended {
			return domain.ErrUseCaseNotReassessable
		}
		updated, err := scanUseCase(tx.QueryRow(ctx, `UPDATE ai_use_cases SET lifecycle_state = 'ASSESSING' WHERE use_case_id = $1 RETURNING `+useCaseColumns,
			useCaseID))
		if err != nil {
			return fmt.Errorf("request reassessment: %w", err)
		}
		out = updated
		return nil
	})
	return out, err
}

func (s *PgStore) RetireUseCase(ctx context.Context, useCaseID string, req domain.RetireUseCaseRequest) (*domain.AIUseCase, error) {
	var out *domain.AIUseCase
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		tenantID := middleware.TenantFromContext(ctx)
		uc, err := loadUseCaseForUpdate(ctx, tx, tenantID, useCaseID)
		if err != nil {
			return err
		}
		if uc.LifecycleState != domain.UseCaseActive && uc.LifecycleState != domain.UseCaseLimited &&
			uc.LifecycleState != domain.UseCaseSuspended {
			return domain.ErrUseCaseNotRetirable
		}
		updated, err := scanUseCase(tx.QueryRow(ctx, `UPDATE ai_use_cases SET lifecycle_state = 'RETIRED' WHERE use_case_id = $1 RETURNING `+useCaseColumns,
			useCaseID))
		if err != nil {
			return fmt.Errorf("retire use case: %w", err)
		}
		out = updated
		return nil
	})
	return out, err
}

// GetEffectiveUseCaseControl returns the exact effective control
// snapshot §4.4's GET .../effective names — the use case plus its
// latest assessment, whatever its decision.
func (s *PgStore) GetEffectiveUseCaseControl(ctx context.Context, useCaseID string) (*domain.EffectiveUseCaseControl, error) {
	var out *domain.EffectiveUseCaseControl
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		tenantID := middleware.TenantFromContext(ctx)
		uc, err := scanUseCase(tx.QueryRow(ctx, `SELECT `+useCaseColumns+` FROM ai_use_cases WHERE use_case_id = $1 AND tenant_id = $2`,
			useCaseID, tenantID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrUseCaseNotFound
		}
		if err != nil {
			return err
		}
		result := &domain.EffectiveUseCaseControl{UseCase: *uc}
		latest, err := scanAssessment(tx.QueryRow(ctx, `SELECT `+assessmentColumns+` FROM ai_impact_assessments
			WHERE use_case_id = $1 ORDER BY version DESC LIMIT 1`, useCaseID))
		if err == nil {
			result.LatestAssessment = latest
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		out = result
		return nil
	})
	return out, err
}

// ─────────────────────────────────────────────────────────────────────────────
// AIG-02: Model, Provider & Capability Registry — platform-wide
// (no tenant_id), same convention as model_provider_registrations.
// ─────────────────────────────────────────────────────────────────────────────

const modelReleaseColumns = `model_release_id, provider, provider_model_id, deployment_region, capability_set,
	context_limit, training_use, retention, approved_scopes, control_evidence, release_state, status_reason,
	created_at, created_by_principal_id, updated_at`

func scanModelRelease(row pgx.Row) (*domain.AIModelRelease, error) {
	var m domain.AIModelRelease
	var trainingUse, releaseState string
	var capabilitySet, approvedScopes, controlEvidence []byte
	if err := row.Scan(&m.ModelReleaseID, &m.Provider, &m.ProviderModelID, &m.DeploymentRegion, &capabilitySet,
		&m.ContextLimit, &trainingUse, &m.Retention, &approvedScopes, &controlEvidence, &releaseState, &m.StatusReason,
		&m.CreatedAt, &m.CreatedByPrincipalID, &m.UpdatedAt); err != nil {
		return nil, err
	}
	m.TrainingUse = domain.TrainingUse(trainingUse)
	m.ReleaseState = domain.ReleaseState(releaseState)
	m.CapabilitySet = unmarshalStrings(capabilitySet)
	m.ApprovedScopes = unmarshalStrings(approvedScopes)
	m.ControlEvidence = unmarshalJSONMap(controlEvidence)
	return &m, nil
}

// RegisterModelRelease always creates a NEW, immutable release row —
// never an upsert. §5.3: "provider alias movement or silent behavior
// change -> new internal release or quarantine, never mutation of
// historical model_release_id."
func (s *PgStore) RegisterModelRelease(ctx context.Context, req domain.RegisterModelReleaseRequest, actor string) (*domain.AIModelRelease, error) {
	capabilitySet, err := marshalStrings(req.CapabilitySet)
	if err != nil {
		return nil, fmt.Errorf("marshal capability_set: %w", err)
	}
	approvedScopes, err := marshalStrings(req.ApprovedScopes)
	if err != nil {
		return nil, fmt.Errorf("marshal approved_scopes: %w", err)
	}
	controlEvidence, err := marshalJSON(req.ControlEvidence)
	if err != nil {
		return nil, fmt.Errorf("marshal control_evidence: %w", err)
	}
	trainingUse := req.TrainingUse
	if trainingUse == "" {
		trainingUse = string(domain.TrainingUseNoTraining)
	}
	m, err := scanModelRelease(s.pool.QueryRow(ctx, `
		INSERT INTO ai_model_releases (provider, provider_model_id, deployment_region, capability_set,
			context_limit, training_use, retention, approved_scopes, control_evidence, created_by_principal_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING `+modelReleaseColumns,
		req.Provider, req.ProviderModelID, req.DeploymentRegion, capabilitySet,
		req.ContextLimit, trainingUse, req.Retention, approvedScopes, controlEvidence, actor))
	if err != nil {
		return nil, fmt.Errorf("register model release: %w", err)
	}
	return m, nil
}

func (s *PgStore) GetModelRelease(ctx context.Context, modelReleaseID string) (*domain.AIModelRelease, error) {
	m, err := scanModelRelease(s.pool.QueryRow(ctx, `SELECT `+modelReleaseColumns+` FROM ai_model_releases WHERE model_release_id = $1`, modelReleaseID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrModelReleaseNotFound
	}
	return m, err
}

func loadModelReleaseForUpdate(ctx context.Context, tx pgx.Tx, modelReleaseID string) (*domain.AIModelRelease, error) {
	m, err := scanModelRelease(tx.QueryRow(ctx, `SELECT `+modelReleaseColumns+` FROM ai_model_releases WHERE model_release_id = $1 FOR UPDATE`, modelReleaseID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrModelReleaseNotFound
	}
	return m, err
}

// transitionRelease is the shared low-level helper every named release
// transition uses: load-for-update, verify the caller's expected
// current state, merge any new control_evidence, move to the target
// state (the migration's trigger is the actual authority on whether
// the hop is legal), and persist an optional status_reason.
func (s *PgStore) transitionRelease(ctx context.Context, modelReleaseID string, expectedStates []domain.ReleaseState, targetState domain.ReleaseState,
	evidence map[string]interface{}, reason string) (*domain.AIModelRelease, error) {
	var out *domain.AIModelRelease
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	m, err := loadModelReleaseForUpdate(ctx, tx, modelReleaseID)
	if err != nil {
		return nil, err
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

	mergedEvidence := m.ControlEvidence
	if mergedEvidence == nil {
		mergedEvidence = map[string]interface{}{}
	}
	for k, v := range evidence {
		mergedEvidence[k] = v
	}
	evidenceJSON, err := marshalJSON(mergedEvidence)
	if err != nil {
		return nil, fmt.Errorf("marshal control_evidence: %w", err)
	}
	var reasonPtr *string
	if reason != "" {
		reasonPtr = &reason
	}

	updated, err := scanModelRelease(tx.QueryRow(ctx, `
		UPDATE ai_model_releases SET release_state = $2, control_evidence = $3, status_reason = $4
		WHERE model_release_id = $1 RETURNING `+modelReleaseColumns,
		modelReleaseID, string(targetState), evidenceJSON, reasonPtr))
	if err != nil {
		return nil, fmt.Errorf("transition model release to %s: %w", targetState, err)
	}
	out = updated
	return out, tx.Commit(ctx)
}

func (s *PgStore) RecordDueDiligence(ctx context.Context, modelReleaseID string, req domain.AdvanceReleaseRequest, actor string) (*domain.AIModelRelease, error) {
	return s.transitionRelease(ctx, modelReleaseID, []domain.ReleaseState{domain.ReleaseDiscovered}, domain.ReleaseDueDiligence, req.ControlEvidence, "")
}

func (s *PgStore) RecordEvaluation(ctx context.Context, modelReleaseID string, req domain.AdvanceReleaseRequest, actor string) (*domain.AIModelRelease, error) {
	return s.transitionRelease(ctx, modelReleaseID, []domain.ReleaseState{domain.ReleaseDueDiligence}, domain.ReleaseEvaluating, req.ControlEvidence, "")
}

// ApproveRelease is §5.4's procurement/enablement gate: every one of
// privacy/residency/security/evaluation/explainability/continuity/legal
// must be attested cleared, or the release is refused outright — no
// partial approval.
func (s *PgStore) ApproveRelease(ctx context.Context, modelReleaseID string, req domain.ApproveReleaseRequest, actor string) (*domain.AIModelRelease, error) {
	if !req.AllGatesCleared() {
		return nil, domain.ErrReleaseGatesNotCleared
	}
	return s.transitionRelease(ctx, modelReleaseID, []domain.ReleaseState{domain.ReleaseEvaluating}, domain.ReleaseApproved, req.ControlEvidence, "")
}

func (s *PgStore) RejectRelease(ctx context.Context, modelReleaseID string, req domain.AdvanceReleaseRequest, actor string) (*domain.AIModelRelease, error) {
	return s.transitionRelease(ctx, modelReleaseID, []domain.ReleaseState{domain.ReleaseDiscovered, domain.ReleaseDueDiligence}, domain.ReleaseRejected, req.ControlEvidence, req.Reason)
}

func (s *PgStore) BlockRelease(ctx context.Context, modelReleaseID string, req domain.AdvanceReleaseRequest, actor string) (*domain.AIModelRelease, error) {
	return s.transitionRelease(ctx, modelReleaseID, []domain.ReleaseState{domain.ReleaseEvaluating}, domain.ReleaseBlocked, req.ControlEvidence, req.Reason)
}

func (s *PgStore) ActivateRelease(ctx context.Context, modelReleaseID string, actor string) (*domain.AIModelRelease, error) {
	return s.transitionRelease(ctx, modelReleaseID, []domain.ReleaseState{domain.ReleaseApproved, domain.ReleaseRestricted}, domain.ReleaseActive, nil, "")
}

func (s *PgStore) RestrictRelease(ctx context.Context, modelReleaseID string, req domain.AdvanceReleaseRequest, actor string) (*domain.AIModelRelease, error) {
	return s.transitionRelease(ctx, modelReleaseID, []domain.ReleaseState{domain.ReleaseActive}, domain.ReleaseRestricted, req.ControlEvidence, req.Reason)
}

func (s *PgStore) UnrestrictRelease(ctx context.Context, modelReleaseID string, actor string) (*domain.AIModelRelease, error) {
	return s.transitionRelease(ctx, modelReleaseID, []domain.ReleaseState{domain.ReleaseRestricted}, domain.ReleaseActive, nil, "")
}

func (s *PgStore) QuarantineRelease(ctx context.Context, modelReleaseID string, req domain.AdvanceReleaseRequest, actor string) (*domain.AIModelRelease, error) {
	return s.transitionRelease(ctx, modelReleaseID, []domain.ReleaseState{domain.ReleaseActive}, domain.ReleaseQuarantined, req.ControlEvidence, req.Reason)
}

func (s *PgStore) RetireRelease(ctx context.Context, modelReleaseID string, req domain.AdvanceReleaseRequest, actor string) (*domain.AIModelRelease, error) {
	return s.transitionRelease(ctx, modelReleaseID,
		[]domain.ReleaseState{domain.ReleaseActive, domain.ReleaseRestricted, domain.ReleaseQuarantined},
		domain.ReleaseRetired, req.ControlEvidence, req.Reason)
}
