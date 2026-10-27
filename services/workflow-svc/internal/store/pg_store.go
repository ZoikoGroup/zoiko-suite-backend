// Package store provides the PostgreSQL implementation of the workflow
// read and write model, including the approval state machine.
//
// This package is the ONLY layer that touches the database directly.
// No SQL appears in handlers or domain packages.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/domain"
	svcmiddleware "zoiko.io/workflow-svc/internal/middleware"
	"zoiko.io/workflow-svc/internal/outbox"
)

// Store is the interface consumed by the handler.
type Store interface {
	// CreateWorkflow returns created=false when params.CorrelationID matches
	// an existing workflow instance for this tenant — the existing
	// instance/stages are returned as-is, nothing new is inserted.
	CreateWorkflow(ctx context.Context, params domain.CreateWorkflowParams) (instance *domain.WorkflowInstance, stages []*domain.WorkflowStage, created bool, err error)
	FindWorkflowByID(ctx context.Context, workflowInstanceID string) (*domain.WorkflowInstance, error)
	FindStagesByWorkflowID(ctx context.Context, workflowInstanceID string) ([]*domain.WorkflowStage, error)
	FindCurrentStage(ctx context.Context, workflowInstanceID string) (*domain.WorkflowStage, error)

	// SubmitAction applies an APPROVE/REJECT action to the current stage.
	// Returns the updated instance, the acted-on stage, and whether this
	// call actually performed a transition (false = idempotent no-op, the
	// stage was already in the requested outcome).
	SubmitAction(ctx context.Context, params domain.SubmitActionParams) (*domain.WorkflowInstance, *domain.WorkflowStage, bool, error)

	// SubmitQuorumVote casts one eligible approver's vote against the
	// current QUORUM stage. Same return-shape contract as SubmitAction.
	SubmitQuorumVote(ctx context.Context, params domain.SubmitQuorumVoteParams) (*domain.WorkflowInstance, *domain.WorkflowStage, bool, error)

	EscalateWorkflow(ctx context.Context, workflowInstanceID, actorPrincipalID string, cmd ...domain.LifecycleCommand) (*domain.WorkflowInstance, bool, error)
	CancelWorkflow(ctx context.Context, workflowInstanceID, actorPrincipalID string, cmd ...domain.LifecycleCommand) (*domain.WorkflowInstance, bool, error)
	ExpireWorkflow(ctx context.Context, workflowInstanceID string, cmd ...domain.LifecycleCommand) (*domain.WorkflowInstance, bool, error)
	InvalidateWorkflow(ctx context.Context, params domain.InvalidateWorkflowParams) (*domain.WorkflowInstance, bool, error)
	VerifyRelease(ctx context.Context, params domain.VerifyReleaseParams) (*domain.ReleaseVerificationResult, error)

	// EscalateToWorkflow escalates a workflow to a different target per R-001 §4.2
	EscalateToWorkflow(ctx context.Context, params domain.EscalateToWorkflowParams) (*domain.WorkflowInstance, bool, error)

	// Workflow Definitions (versioned, immutable) per R-001 WFC-02
	CreateWorkflowDefinition(ctx context.Context, params domain.CreateWorkflowDefinitionParams) (*domain.WorkflowDefinition, error)
	GetWorkflowDefinition(ctx context.Context, params domain.GetWorkflowDefinitionParams) (*domain.WorkflowDefinition, error)
	ListWorkflowDefinitions(ctx context.Context, params domain.ListWorkflowDefinitionsParams) ([]*domain.WorkflowDefinition, error)
	SupersedeWorkflowDefinition(ctx context.Context, oldDefID, newDefID string) error

	// Additional workflow definition lifecycle methods
	ActivateWorkflowDefinition(ctx context.Context, workflowDefinitionID string) error
	SuspendWorkflowDefinition(ctx context.Context, workflowDefinitionID string) error
	RetireWorkflowDefinition(ctx context.Context, workflowDefinitionID string) error

	// Additional workflow instance methods
	ListPendingApprovals(ctx context.Context, tenantID string, limit, offset int) ([]*domain.WorkflowInstance, error)
	GetApprovalEvidence(ctx context.Context, workflowInstanceID string) ([]*domain.WorkflowTransition, error)
	RequestChanges(ctx context.Context, workflowInstanceID, actorPrincipalID, rationale string, causationID *string, cmd ...domain.LifecycleCommand) error
}

// PgStore implements Store against a PostgreSQL cluster via pgxpool.
type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

// New returns an open PgStore. Caller must call pool.Close() when done.
func New(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

// withRLS runs fn inside a transaction with app.tenant_id set for the
// session, so workflow_instances' tenant_isolation_policy has a value to
// enforce against. Mirrors tenant-entity-registry-svc's PgStore.withRLS.
// An empty tenantID is valid — it means "no tenant scope known" and
// matches nothing, same posture as FindWorkflowByID's existing fallback.
func (s *PgStore) withRLS(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback error discarded intentionally on commit path

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set_config app.tenant_id: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ── workflow_instances ───────────────────────────────────────────────────────

const instanceColumns = `workflow_instance_id, tenant_id, legal_entity_id, workflow_type, workflow_status, current_stage, initiated_by, correlation_id, started_at, completed_at, subject_type, subject_id, subject_version, subject_fingerprint, invalidated_at, invalidation_reason_code, invalidation_narrative, invalidation_evidence_refs, row_version, updated_at, created_by, plane, data_class, residency_region, idempotency_key, due_at, next_action_at, escalation_target, workflow_definition_id, workflow_definition_version, review_cycle, escalated_at`

func scanInstance(row pgx.Row) (*domain.WorkflowInstance, error) {
	w := &domain.WorkflowInstance{}
	// idempotency_key (000013) and the definition reference (000016) are
	// nullable: a workflow created without an Idempotency-Key or a definition
	// has none, and scanning NULL into a plain string or int fails.
	var idempotencyKey, definitionID, escalationTarget *string
	var definitionVersion *int
	err := row.Scan(&w.WorkflowInstanceID, &w.TenantID, &w.LegalEntityID, &w.WorkflowType, &w.WorkflowStatus,
		&w.CurrentStage, &w.InitiatedBy, &w.CorrelationID, &w.StartedAt, &w.CompletedAt,
		&w.SubjectType, &w.SubjectID, &w.SubjectVersion, &w.SubjectFingerprint,
		&w.InvalidatedAt, &w.InvalidationReasonCode, &w.InvalidationNarrative, &w.InvalidationEvidenceRefs,
		&w.RowVersion, &w.UpdatedAt, &w.CreatedBy, &w.Plane, &w.DataClass, &w.ResidencyRegion, &idempotencyKey,
		&w.DueAt, &w.NextActionAt, &escalationTarget, &definitionID, &definitionVersion, &w.ReviewCycle, &w.EscalatedAt)
	if idempotencyKey != nil {
		w.IdempotencyKey = *idempotencyKey
	}
	if definitionID != nil {
		w.WorkflowDefinitionID = *definitionID
	}
	if definitionVersion != nil {
		w.WorkflowDefinitionVersion = *definitionVersion
	}
	if escalationTarget != nil {
		w.EscalationTarget = escalationTarget
	}
	return w, err
}

// FindWorkflowByID is the choke point every other Store method routes
// through before mutating a workflow, so scoping it to the caller's tenant
// (via svcmiddleware.TenantFromContext) closes the cross-tenant read/write
// gap for the whole service in one place. An empty tenant in context (a
// caller that predates the X-Tenant-Id header) falls back to unscoped lookup.
func (s *PgStore) FindWorkflowByID(ctx context.Context, workflowInstanceID string) (*domain.WorkflowInstance, error) {
	// tenant_id is a UUID column — passing "" and comparing with a plain
	// "$2 = '' OR tenant_id = $2" makes Postgres try to resolve $2 as both
	// text and uuid in one prepared statement ("operator does not exist:
	// uuid = text"). A nil *string sends an actual SQL NULL instead, so both
	// usages below agree $2 is uuid.
	tenantCtx := svcmiddleware.TenantFromContext(ctx)
	var tenantID *string
	if tenantCtx != "" {
		tenantID = &tenantCtx
	}
	const query = `SELECT ` + instanceColumns + ` FROM workflow_instances WHERE workflow_instance_id = $1 AND ($2::uuid IS NULL OR tenant_id = $2::uuid);`
	var w *domain.WorkflowInstance
	err := s.withRLS(ctx, tenantCtx, func(tx pgx.Tx) error {
		var scanErr error
		w, scanErr = scanInstance(tx.QueryRow(ctx, query, workflowInstanceID, tenantID))
		return scanErr
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrWorkflowNotFound
		}
		s.log.Error("pg FindWorkflowByID failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return w, nil
}

const stageColumns = `workflow_stage_id, workflow_instance_id, stage_order, stage_type, approver_principal_id, required_approvals, stage_status, acted_at, rationale, row_version, updated_at, created_by, plane, data_class, residency_region, approver_role, due_at, acted_by`

func scanStage(row pgx.Row) (*domain.WorkflowStage, error) {
	st := &domain.WorkflowStage{}
	var approverPrincipalID *string
	err := row.Scan(&st.WorkflowStageID, &st.WorkflowInstanceID, &st.StageOrder, &st.StageType, &approverPrincipalID,
		&st.RequiredApprovals, &st.StageStatus, &st.ActedAt, &st.Rationale,
		&st.RowVersion, &st.UpdatedAt, &st.CreatedBy, &st.Plane, &st.DataClass, &st.ResidencyRegion,
		&st.ApproverRole, &st.DueAt, &st.ActedBy)
	if approverPrincipalID != nil {
		st.ApproverPrincipalID = *approverPrincipalID
	}
	return st, err
}

// Every read of workflow_stages, its quorum tables and workflow_transitions
// runs inside withRLS with the caller's tenant. 000017 put FORCE RLS on those
// tables, and the runtime role (zoiko_app) does not bypass it: reads made on
// the bare pool saw no rows at all, so under the real role a workflow had no
// stages, its approver could not approve ("not the approver for the current
// stage") and every retry was an idempotency mismatch. The suite proves this
// as a NOBYPASSRLS role (TestPgStore_RuntimeRole_*).

func (s *PgStore) FindStagesByWorkflowID(ctx context.Context, workflowInstanceID string) ([]*domain.WorkflowStage, error) {
	if _, err := s.FindWorkflowByID(ctx, workflowInstanceID); err != nil {
		return nil, err
	}
	const query = `SELECT ` + stageColumns + ` FROM workflow_stages WHERE workflow_instance_id = $1 ORDER BY stage_order;`
	var stages []*domain.WorkflowStage
	err := s.withRLS(ctx, svcmiddleware.TenantFromContext(ctx), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, workflowInstanceID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			st, scanErr := scanStage(rows)
			if scanErr != nil {
				return scanErr
			}
			stages = append(stages, st)
		}
		return rows.Err()
	})
	if err != nil {
		s.log.Error("pg FindStagesByWorkflowID failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return stages, nil
}

// findStageByApprover looks up the stage in workflowInstanceID assigned to
// approverPrincipalID. Returns domain.ErrWrongApprover if none exists —
// this principal is not an approver anywhere in this workflow's chain.
// Assumes at most one stage per approver per workflow (v1 simplification).
// Uses ORDER BY stage_order to return the earliest matching stage.
func (s *PgStore) findStageByApprover(ctx context.Context, workflowInstanceID, approverPrincipalID string) (*domain.WorkflowStage, error) {
	const query = `SELECT ` + stageColumns + ` FROM workflow_stages WHERE workflow_instance_id = $1 AND approver_principal_id = $2 ORDER BY stage_order LIMIT 1;`
	var st *domain.WorkflowStage
	err := s.withRLS(ctx, svcmiddleware.TenantFromContext(ctx), func(tx pgx.Tx) error {
		var scanErr error
		st, scanErr = scanStage(tx.QueryRow(ctx, query, workflowInstanceID, approverPrincipalID))
		return scanErr
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrWrongApprover
		}
		s.log.Error("pg findStageByApprover failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return st, nil
}

// findStageByOrder reads one stage of an instance under the caller's tenant.
func (s *PgStore) findStageByOrder(ctx context.Context, workflowInstanceID string, order int) (*domain.WorkflowStage, error) {
	const query = `SELECT ` + stageColumns + ` FROM workflow_stages WHERE workflow_instance_id = $1 AND stage_order = $2;`
	var st *domain.WorkflowStage
	err := s.withRLS(ctx, svcmiddleware.TenantFromContext(ctx), func(tx pgx.Tx) error {
		var scanErr error
		st, scanErr = scanStage(tx.QueryRow(ctx, query, workflowInstanceID, order))
		return scanErr
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrWorkflowNotFound
		}
		s.log.Error("pg findStageByOrder failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return st, nil
}

// FindCurrentStage returns the stage awaiting a decision. An ESCALATED
// instance keeps its current stage (it is still decidable); a terminal one
// has none.
func (s *PgStore) FindCurrentStage(ctx context.Context, workflowInstanceID string) (*domain.WorkflowStage, error) {
	instance, err := s.FindWorkflowByID(ctx, workflowInstanceID)
	if err != nil {
		return nil, err
	}
	if instance.CurrentStage == 0 {
		return nil, domain.ErrNoCurrentStage
	}
	return s.findStageByOrder(ctx, workflowInstanceID, instance.CurrentStage)
}

// IsCurrentStageApprover reports whether principalID is an approver on the
// workflow's current stage: the one approver of a SINGLE stage, or anyone in
// a QUORUM stage's pool. The join starts from workflow_instances, scoped both
// by the tenant GUC (RLS) and by an explicit tenant predicate, so a workflow
// in another tenant, or a call with no tenant, answers false, never true.
func (s *PgStore) IsCurrentStageApprover(ctx context.Context, workflowInstanceID, principalID string) (bool, error) {
	const query = `
		SELECT EXISTS (
			SELECT 1
			  FROM workflow_instances i
			  JOIN workflow_stages st
			    ON st.workflow_instance_id = i.workflow_instance_id
			   AND st.stage_order = i.current_stage
			  LEFT JOIN workflow_stage_quorum_approvers q
			    ON q.workflow_stage_id = st.workflow_stage_id
			 WHERE i.workflow_instance_id = $1
			   AND i.tenant_id = NULLIF($3, '')::uuid
			   AND (st.approver_principal_id = $2 OR q.approver_principal_id = $2)
		);`
	tenantID := svcmiddleware.TenantFromContext(ctx)
	var onStage bool
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, query, workflowInstanceID, principalID, tenantID).Scan(&onStage)
	})
	if err != nil {
		s.log.Error("pg IsCurrentStageApprover failed", zap.Error(err))
		return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return onStage, nil
}

// CreateWorkflow inserts a new workflow instance plus its ordered stage
// chain, and records the initial "" -> PENDING transition, all in one
// transaction.
//
// Idempotency: uses (tenant_id, idempotency_key) when idempotency_key is
// provided; falls back to (tenant_id, correlation_id) for backward
// compatibility. Returns ErrIdempotencyMismatch if same key exists with
// different request body.
func (s *PgStore) CreateWorkflow(ctx context.Context, params domain.CreateWorkflowParams) (*domain.WorkflowInstance, []*domain.WorkflowStage, bool, error) {
	if len(params.Stages) == 0 {
		return nil, nil, false, domain.ErrNoStages
	}
	if params.WorkflowInstanceID == "" {
		params.WorkflowInstanceID = uuid.New().String()
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.log.Error("pg CreateWorkflow: begin tx failed", zap.Error(err))
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", params.TenantID); err != nil {
		return nil, nil, false, fmt.Errorf("set_config app.tenant_id: %w", err)
	}

	// Idempotency is the Idempotency-Key alone. It used to fall back to the
	// correlation id, which is the business-flow trace: a second, different
	// workflow started in the same flow was then answered with the first
	// one (or, with 000003's unique index, a 503). 000020 drops that index.
	idemKey := params.IdempotencyKey

	// Stage 1 is current from the start, so its deadline starts now; the
	// instance's next_action_at is that deadline (the timer sweep escalates
	// past it). due_at is the expiry of the whole approval.
	now := time.Now().UTC()
	var firstStageDue *time.Time
	if len(params.Stages) > 0 && params.Stages[0].DueAtHours > 0 {
		t := now.Add(time.Duration(params.Stages[0].DueAtHours) * time.Hour)
		firstStageDue = &t
	}

	var instance *domain.WorkflowInstance
	var definitionID *string
	if params.WorkflowDefinitionID != "" {
		definitionID = &params.WorkflowDefinitionID
	}
	var definitionVersion *int
	if params.WorkflowDefinitionVersion > 0 {
		definitionVersion = &params.WorkflowDefinitionVersion
	}

	if idemKey != "" {
		// Try to insert with idempotency key
		const insertInstance = `
			INSERT INTO workflow_instances (workflow_instance_id, tenant_id, legal_entity_id, workflow_type, initiated_by, correlation_id, subject_type, subject_id, subject_version, subject_fingerprint, idempotency_key, escalation_target, workflow_definition_id, workflow_definition_version, created_by, due_at, next_action_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $5, $15, $16)
			ON CONFLICT (tenant_id, idempotency_key) WHERE idempotency_key IS NOT NULL AND idempotency_key != '' DO NOTHING
			RETURNING ` + instanceColumns + `;`
		row := tx.QueryRow(ctx, insertInstance, params.WorkflowInstanceID, params.TenantID, params.LegalEntityID, params.WorkflowType, params.InitiatedBy, params.CorrelationID, params.SubjectType, params.SubjectID, params.SubjectVersion, params.SubjectFingerprint, idemKey, params.EscalationTarget, definitionID, definitionVersion, params.DueAt, firstStageDue)
		instance, err = scanInstance(row)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				s.log.Error("pg CreateWorkflow: insert instance failed", zap.Error(err))
				return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
			}
			// Conflict on idempotency key — check if it's a mismatch (different body)
			// Fetch existing and compare relevant fields
			existing, fetchErr := s.findByIdempotencyKey(ctx, params.TenantID, idemKey)
			if fetchErr != nil {
				s.log.Error("pg CreateWorkflow: lookup by idempotency_key failed", zap.Error(fetchErr))
				return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, fetchErr)
			}
			if existing != nil && s.workflowBodyDiffers(ctx, existing, params) {
				return nil, nil, false, domain.ErrIdempotencyMismatch
			}
			// Same body — return existing (idempotent replay)
			stages, err := s.FindStagesByWorkflowID(ctx, existing.WorkflowInstanceID)
			if err != nil {
				return nil, nil, false, err
			}
			return existing, stages, false, nil
		}
	} else {
		// No idempotency key provided — insert without idempotency check
		const insertInstance = `
			INSERT INTO workflow_instances (workflow_instance_id, tenant_id, legal_entity_id, workflow_type, initiated_by, correlation_id, subject_type, subject_id, subject_version, subject_fingerprint, escalation_target, workflow_definition_id, workflow_definition_version, created_by, due_at, next_action_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $5, $14, $15)
			RETURNING ` + instanceColumns + `;`
		row := tx.QueryRow(ctx, insertInstance, params.WorkflowInstanceID, params.TenantID, params.LegalEntityID, params.WorkflowType, params.InitiatedBy, params.CorrelationID, params.SubjectType, params.SubjectID, params.SubjectVersion, params.SubjectFingerprint, params.EscalationTarget, definitionID, definitionVersion, params.DueAt, firstStageDue)
		instance, err = scanInstance(row)
		if err != nil {
			s.log.Error("pg CreateWorkflow: insert instance failed", zap.Error(err))
			return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
	}

	stages := make([]*domain.WorkflowStage, 0, len(params.Stages))
	// created_by is the initiator (it was never written, so every row read
	// ''). Only stage 1 gets a due_at now; later stages get theirs when they
	// become current.
	const insertSingleStage = `
		INSERT INTO workflow_stages (workflow_instance_id, stage_order, stage_type, approver_principal_id, created_by, due_at, due_hours)
		VALUES ($1, $2, 'SINGLE', $3, $4, $5, $6)
		RETURNING ` + stageColumns + `;`
	const insertQuorumStage = `
		INSERT INTO workflow_stages (workflow_instance_id, stage_order, stage_type, required_approvals, created_by, due_at, due_hours)
		VALUES ($1, $2, 'QUORUM', $3, $4, $5, $6)
		RETURNING ` + stageColumns + `;`
	const insertRoleStage = `
		INSERT INTO workflow_stages (workflow_instance_id, stage_order, stage_type, approver_role, created_by, due_at, due_hours)
		VALUES ($1, $2, 'ROLE', $3, $4, $5, $6)
		RETURNING ` + stageColumns + `;`
	const insertQuorumApprover = `
		INSERT INTO workflow_stage_quorum_approvers (workflow_stage_id, approver_principal_id) VALUES ($1, $2);`
	for i, stageInput := range params.Stages {
		stageType := stageInput.StageType
		if stageType == "" {
			stageType = domain.StageTypeSingle
		}
		var due *time.Time
		if i == 0 {
			due = firstStageDue
		}
		var dueHours *int
		if stageInput.DueAtHours > 0 {
			h := stageInput.DueAtHours
			dueHours = &h
		}
		var row pgx.Row
		switch stageType {
		case domain.StageTypeQuorum:
			row = tx.QueryRow(ctx, insertQuorumStage, params.WorkflowInstanceID, i+1, stageInput.RequiredApprovals, params.InitiatedBy, due, dueHours)
		case domain.StageTypeRole:
			row = tx.QueryRow(ctx, insertRoleStage, params.WorkflowInstanceID, i+1, stageInput.ApproverRole, params.InitiatedBy, due, dueHours)
		default:
			row = tx.QueryRow(ctx, insertSingleStage, params.WorkflowInstanceID, i+1, stageInput.ApproverPrincipalID, params.InitiatedBy, due, dueHours)
		}
		st, err := scanStage(row)
		if err != nil {
			s.log.Error("pg CreateWorkflow: insert stage failed", zap.Error(err))
			return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		if stageType == domain.StageTypeQuorum {
			for _, approver := range stageInput.QuorumApprovers {
				if _, err := tx.Exec(ctx, insertQuorumApprover, st.WorkflowStageID, approver); err != nil {
					s.log.Error("pg CreateWorkflow: insert quorum approver failed", zap.Error(err))
					return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
				}
			}
		}
		stages = append(stages, st)
	}

	var correlationID *string
	if params.CorrelationID != "" {
		correlationID = &params.CorrelationID
	}
	if err := insertTransition(ctx, tx, params.WorkflowInstanceID, "", "PENDING", params.InitiatedBy, nil, correlationID, nil); err != nil {
		return nil, nil, false, err
	}

	startedPayload := map[string]any{
		"workflow_instance_id": instance.WorkflowInstanceID,
		"tenant_id":            instance.TenantID,
		"legal_entity_id":      instance.LegalEntityID,
		"workflow_type":        instance.WorkflowType,
		"initiated_by":         instance.InitiatedBy,
		"started_at":           instance.StartedAt,
	}
	if instance.SubjectType != nil {
		startedPayload["subject_type"] = *instance.SubjectType
	}
	if instance.SubjectID != nil {
		startedPayload["subject_id"] = *instance.SubjectID
	}
	if instance.SubjectVersion != nil {
		startedPayload["subject_version"] = *instance.SubjectVersion
	}
	if instance.SubjectFingerprint != nil {
		startedPayload["subject_fingerprint"] = *instance.SubjectFingerprint
	}
	if err := outbox.Insert(ctx, tx, outbox.Event{
		AggregateType: "workflow_instance",
		AggregateID:   instance.WorkflowInstanceID,
		EventType:     "workflow.started",
		TenantID:      instance.TenantID,
		LegalEntityID: instance.LegalEntityID,
		ActorID:       &instance.InitiatedBy,
		CorrelationID: correlationID,
		Payload:       startedPayload,
	}); err != nil {
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	if err := tx.Commit(ctx); err != nil {
		s.log.Error("pg CreateWorkflow: commit failed", zap.Error(err))
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return instance, stages, true, nil
}

// findByIdempotencyKey fetches a workflow by tenant_id and idempotency_key
func (s *PgStore) findByIdempotencyKey(ctx context.Context, tenantID, idempotencyKey string) (*domain.WorkflowInstance, error) {
	const query = `SELECT ` + instanceColumns + ` FROM workflow_instances WHERE tenant_id = $1 AND idempotency_key = $2;`
	var instance *domain.WorkflowInstance
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var scanErr error
		instance, scanErr = scanInstance(tx.QueryRow(ctx, query, tenantID, idempotencyKey))
		if errors.Is(scanErr, pgx.ErrNoRows) {
			return nil
		}
		return scanErr
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		s.log.Error("pg findByIdempotencyKey failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return instance, nil
}

// workflowBodyDiffers compares the existing workflow with new params to detect
// IDEMPOTENCY_MISMATCH. It reads the stored stages with the caller's context:
// it used context.Background(), which carries no tenant, so under the runtime
// role RLS hid the stages, "cannot read" counted as "differs", and every
// honest same-key retry was refused 409.
func (s *PgStore) workflowBodyDiffers(ctx context.Context, existing *domain.WorkflowInstance, params domain.CreateWorkflowParams) bool {
	// Compare the fields that define the workflow body
	if existing.WorkflowType != params.WorkflowType {
		return true
	}
	if existing.LegalEntityID != params.LegalEntityID {
		return true
	}
	if (existing.SubjectType == nil) != (params.SubjectType == nil) {
		return true
	}
	if existing.SubjectType != nil && params.SubjectType != nil && *existing.SubjectType != *params.SubjectType {
		return true
	}
	if (existing.SubjectID == nil) != (params.SubjectID == nil) {
		return true
	}
	if existing.SubjectID != nil && params.SubjectID != nil && *existing.SubjectID != *params.SubjectID {
		return true
	}
	if (existing.SubjectVersion == nil) != (params.SubjectVersion == nil) {
		return true
	}
	if existing.SubjectVersion != nil && params.SubjectVersion != nil && *existing.SubjectVersion != *params.SubjectVersion {
		return true
	}
	if (existing.SubjectFingerprint == nil) != (params.SubjectFingerprint == nil) {
		return true
	}
	if existing.SubjectFingerprint != nil && params.SubjectFingerprint != nil && *existing.SubjectFingerprint != *params.SubjectFingerprint {
		return true
	}
	if existing.WorkflowDefinitionID != params.WorkflowDefinitionID ||
		existing.WorkflowDefinitionVersion != params.WorkflowDefinitionVersion {
		return true
	}

	// Compare stages: stage order, type, approver, required approvals, quorum pool
	existingStages, err := s.FindStagesByWorkflowID(ctx, existing.WorkflowInstanceID)
	if err != nil {
		// If we can't fetch stages, assume they differ to be safe
		return true
	}
	if len(existingStages) != len(params.Stages) {
		return true
	}
	for i := range existingStages {
		existingStage := existingStages[i]
		newStage := params.Stages[i]
		if existingStage.StageOrder != i+1 {
			return true
		}
		existingStageType := existingStage.StageType
		if existingStageType == "" {
			existingStageType = domain.StageTypeSingle
		}
		newStageType := newStage.StageType
		if newStageType == "" {
			newStageType = domain.StageTypeSingle
		}
		if existingStageType != newStageType {
			return true
		}
		switch existingStageType {
		case domain.StageTypeSingle:
			if existingStage.ApproverPrincipalID != newStage.ApproverPrincipalID {
				return true
			}
		case domain.StageTypeQuorum:
			requiredApprovals := 0
			if existingStage.RequiredApprovals != nil {
				requiredApprovals = *existingStage.RequiredApprovals
			}
			if requiredApprovals != newStage.RequiredApprovals {
				return true
			}
			// Fetch quorum approvers from DB for comparison
			existingApprovers, err := s.fetchQuorumApprovers(ctx, existingStage.WorkflowStageID)
			if err != nil {
				return true
			}
			if !sameSet(existingApprovers, newStage.QuorumApprovers) {
				return true
			}
		case domain.StageTypeRole:
			if existingStage.ApproverRole == nil || *existingStage.ApproverRole != newStage.ApproverRole {
				return true
			}
		}
	}
	return false
}

// sameSet compares two approver pools as sets: the store returns a pool
// sorted, the caller sends it in any order, and order means nothing.
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, x := range a {
		seen[x]++
	}
	for _, x := range b {
		if seen[x] == 0 {
			return false
		}
		seen[x]--
	}
	return true
}

func (s *PgStore) fetchQuorumApprovers(ctx context.Context, workflowStageID string) ([]string, error) {
	const query = `SELECT approver_principal_id FROM workflow_stage_quorum_approvers WHERE workflow_stage_id = $1 ORDER BY approver_principal_id;`
	var approvers []string
	err := s.withRLS(ctx, svcmiddleware.TenantFromContext(ctx), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, workflowStageID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a string
			if err := rows.Scan(&a); err != nil {
				return err
			}
			approvers = append(approvers, a)
		}
		return rows.Err()
	})
	return approvers, err
}

// findExistingByCorrelation resolves the instance+stages an idempotent
// CreateWorkflow retry should return, in its own read against the
// already-committed data from the original call.
func (s *PgStore) findExistingByCorrelation(ctx context.Context, tenantID, correlationID string) (*domain.WorkflowInstance, []*domain.WorkflowStage, bool, error) {
	const query = `SELECT ` + instanceColumns + ` FROM workflow_instances WHERE tenant_id = $1 AND correlation_id = $2;`
	var instance *domain.WorkflowInstance
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var scanErr error
		instance, scanErr = scanInstance(tx.QueryRow(ctx, query, tenantID, correlationID))
		return scanErr
	})
	if err != nil {
		s.log.Error("pg CreateWorkflow: lookup existing by correlation_id failed", zap.Error(err))
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	stages, err := s.FindStagesByWorkflowID(ctx, instance.WorkflowInstanceID)
	if err != nil {
		return nil, nil, false, err
	}
	return instance, stages, false, nil
}

func insertTransition(ctx context.Context, tx pgx.Tx, workflowInstanceID, fromState, toState, actedBy string, rationale, correlationID, causationID *string) error {
	return insertTransitionRecord(ctx, tx, transitionRecord{InstanceID: workflowInstanceID, From: fromState, To: toState,
		ActedBy: actedBy, Rationale: rationale, CorrelationID: correlationID, CausationID: causationID})
}

// transitionRecord is one row of the append-only transition log with its
// lineage: the stage and review cycle, the authority snapshot, the route
// version, the command's Idempotency-Key and request hash, and the
// governance-ledger decision id.
type transitionRecord struct {
	InstanceID, From, To, ActedBy string
	Rationale, CorrelationID      *string
	CausationID                   *string
	StageOrder, ReviewCycle       *int
	DefinitionVersion             *int
	IdempotencyKey, RequestHash   string
	Authority                     domain.ActionAuthority
	DecisionID                    string
}

func insertTransitionRecord(ctx context.Context, tx pgx.Tx, r transitionRecord) error {
	const query = `
		INSERT INTO workflow_transitions (workflow_instance_id, from_state, to_state, acted_by, rationale, correlation_id, causation_id,
			stage_order, review_cycle, workflow_definition_version, idempotency_key, request_hash,
			access_decision_id, authority_basis, delegation_id, decision_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16);`
	var decisionID *string
	if r.DecisionID != "" {
		decisionID = &r.DecisionID
	}
	if _, err := tx.Exec(ctx, query, r.InstanceID, r.From, r.To, r.ActedBy, r.Rationale, r.CorrelationID, r.CausationID,
		r.StageOrder, r.ReviewCycle, r.DefinitionVersion, nullIfEmpty(r.IdempotencyKey), nullIfEmpty(r.RequestHash),
		nullIfEmpty(r.Authority.AccessDecisionID), nullIfEmpty(r.Authority.Basis), nullIfEmpty(r.Authority.DelegationID), decisionID); err != nil {
		if isUniqueViolation(err, "uq_workflow_transitions_idempotency") {
			return errIdempotentRetry
		}
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

// errIdempotentRetry: a transition with this command's Idempotency-Key was
// committed concurrently; the caller answers as a replay.
var errIdempotentRetry = errors.New("idempotent retry")

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && (constraint == "" || pgErr.ConstraintName == constraint)
}

// replayedCommand looks for a transition already recorded under this
// command's Idempotency-Key. found=false when there is none; a stored request
// hash that differs is domain.ErrIdempotencyMismatch.
func replayedCommand(ctx context.Context, tx pgx.Tx, instanceID string, meta domain.CommandMeta) (bool, error) {
	if meta.IdempotencyKey == "" {
		return false, nil
	}
	var hash *string
	err := tx.QueryRow(ctx, `SELECT request_hash FROM workflow_transitions WHERE workflow_instance_id = $1 AND idempotency_key = $2`,
		instanceID, meta.IdempotencyKey).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if hash != nil && meta.RequestHash != "" && *hash != meta.RequestHash {
		return true, domain.ErrIdempotencyMismatch
	}
	return true, nil
}

func isFinalStage(ctx context.Context, tx pgx.Tx, workflowInstanceID string, stageOrder int) (bool, error) {
	const query = `SELECT MAX(stage_order) FROM workflow_stages WHERE workflow_instance_id = $1;`
	var maxOrder int
	if err := tx.QueryRow(ctx, query, workflowInstanceID).Scan(&maxOrder); err != nil {
		return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return stageOrder == maxOrder, nil
}

type quorumTallyResult struct {
	poolSize   int
	approvals  int
	rejections int
	votesCast  int
}

func quorumTally(ctx context.Context, tx pgx.Tx, workflowStageID string) (quorumTallyResult, error) {
	var tr quorumTallyResult
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM workflow_stage_quorum_approvers WHERE workflow_stage_id = $1`, workflowStageID).Scan(&tr.poolSize); err != nil {
		return tr, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	rows, err := tx.Query(ctx, `SELECT decision FROM workflow_stage_quorum_decisions WHERE workflow_stage_id = $1`, workflowStageID)
	if err != nil {
		return tr, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return tr, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		tr.votesCast++
		if d == domain.QuorumDecisionApprove {
			tr.approvals++
		} else {
			tr.rejections++
		}
	}
	if err := rows.Err(); err != nil {
		return tr, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return tr, nil
}

// InvalidateWorkflow transitions PENDING or APPROVED -> INVALIDATED per ZS-STATE-001 §6.1 / §7.
//
// Idempotency: if already INVALIDATED, returns current instance and transitioned=false.
// Terminal rejection/cancellation: cannot be invalidated (ErrInvalidTransition).
func (s *PgStore) InvalidateWorkflow(ctx context.Context, params domain.InvalidateWorkflowParams) (*domain.WorkflowInstance, bool, error) {
	current, err := s.FindWorkflowByID(ctx, params.WorkflowInstanceID)
	if err != nil {
		return nil, false, err
	}
	if current.WorkflowStatus == domain.WorkflowStatusInvalidated {
		return current, false, nil
	}
	if current.WorkflowStatus == domain.WorkflowStatusRejected || current.WorkflowStatus == domain.WorkflowStatusCancelled {
		return nil, false, domain.ErrInvalidTransition
	}
	if current.WorkflowStatus != domain.WorkflowStatusPending && current.WorkflowStatus != domain.WorkflowStatusApproved && current.WorkflowStatus != domain.WorkflowStatusEscalated {
		return nil, false, domain.ErrInvalidTransition
	}
	if params.ExpectedVersion != nil && current.RowVersion != *params.ExpectedVersion {
		return nil, false, domain.ErrConcurrencyConflict
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.log.Error("pg InvalidateWorkflow: begin tx failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", svcmiddleware.TenantFromContext(ctx)); err != nil {
		return nil, false, fmt.Errorf("set_config app.tenant_id: %w", err)
	}

	const query = `
		UPDATE workflow_instances
		SET workflow_status = 'INVALIDATED',
		    current_stage = 0,
		    completed_at = COALESCE(completed_at, NOW()),
		    invalidated_at = NOW(),
		    invalidation_reason_code = $1,
		    invalidation_narrative = $2,
		    invalidation_evidence_refs = $3,
		    row_version = row_version + 1, updated_at = NOW()
		WHERE workflow_instance_id = $4 AND row_version = $5
		RETURNING ` + instanceColumns + `;`

	evidenceRefs := params.EvidenceRefs
	if evidenceRefs == nil {
		evidenceRefs = []string{}
	}

	row := tx.QueryRow(ctx, query, params.ReasonCode, params.Narrative, evidenceRefs, params.WorkflowInstanceID, current.RowVersion)
	updated, err := scanInstance(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, domain.ErrConcurrencyConflict
		}
		s.log.Error("pg InvalidateWorkflow: update failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	var instanceCorrelationID *string
	if updated.CorrelationID != "" {
		instanceCorrelationID = &updated.CorrelationID
	}
	if params.CorrelationID != "" {
		instanceCorrelationID = &params.CorrelationID
	}

	rationale := fmt.Sprintf("invalidated by %s: code=%s", params.ActorPrincipalID, params.ReasonCode)
	if params.Narrative != nil && *params.Narrative != "" {
		rationale += " narrative=" + *params.Narrative
	}

	cycle := current.ReviewCycle
	if err := insertTransitionRecord(ctx, tx, transitionRecord{
		InstanceID: params.WorkflowInstanceID, From: current.WorkflowStatus, To: domain.WorkflowStatusInvalidated,
		ActedBy: params.ActorPrincipalID, Rationale: &rationale, CorrelationID: instanceCorrelationID, CausationID: params.CausationID,
		ReviewCycle: &cycle, IdempotencyKey: params.Meta.IdempotencyKey, RequestHash: params.Meta.RequestHash,
		Authority: params.Authority, DecisionID: params.Meta.DecisionID,
	}); err != nil {
		if errors.Is(err, errIdempotentRetry) {
			return current, false, nil
		}
		return nil, false, err
	}
	if err := writeLedgerTx(ctx, tx, updated, params.ActorPrincipalID, "WORKFLOW_INVALIDATE", domain.WorkflowStatusInvalidated,
		params.CausationID, params.Authority, params.Meta, map[string]any{"from_status": current.WorkflowStatus,
			"reason_code": params.ReasonCode, "rationale": rationale, "evidence_refs": params.EvidenceRefs}); err != nil {
		if errors.Is(err, errIdempotentRetry) {
			return current, false, nil
		}
		return nil, false, err
	}

	invalidationPayload := map[string]any{
		"workflow_instance_id":     updated.WorkflowInstanceID,
		"workflow_status":          updated.WorkflowStatus,
		"invalidated_at":           updated.InvalidatedAt,
		"invalidation_reason_code": updated.InvalidationReasonCode,
	}
	if updated.SubjectType != nil {
		invalidationPayload["subject_type"] = *updated.SubjectType
	}
	if updated.SubjectID != nil {
		invalidationPayload["subject_id"] = *updated.SubjectID
	}
	if updated.SubjectVersion != nil {
		invalidationPayload["subject_version"] = *updated.SubjectVersion
	}
	if updated.SubjectFingerprint != nil {
		invalidationPayload["subject_fingerprint"] = *updated.SubjectFingerprint
	}
	if updated.InvalidationNarrative != nil {
		invalidationPayload["invalidation_narrative"] = *updated.InvalidationNarrative
	}
	if len(updated.InvalidationEvidenceRefs) > 0 {
		invalidationPayload["invalidation_evidence_refs"] = updated.InvalidationEvidenceRefs
	}
	if err := outbox.Insert(ctx, tx, outbox.Event{
		AggregateType: "workflow_instance",
		AggregateID:   params.WorkflowInstanceID,
		EventType:     "workflow.approval.invalidated",
		TenantID:      updated.TenantID,
		LegalEntityID: updated.LegalEntityID,
		ActorID:       &params.ActorPrincipalID,
		CorrelationID: instanceCorrelationID,
		Payload:       invalidationPayload,
	}); err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	if err := tx.Commit(ctx); err != nil {
		s.log.Error("pg InvalidateWorkflow: commit failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return updated, true, nil
}

// VerifyRelease evaluates whether an approved workflow remains valid to release against the current subject version and fingerprint.
//
// Enforces ZS-STATE-001 Critical Release Rule, I-06, I-07, T-02, T-04:
// - Fails safely if the workflow has no bound subject fingerprint.
// - Fails if the workflow is not in APPROVED status (or has been INVALIDATED).
// - Fails if expected_subject_version is specified and does not match bound subject_version.
// - Fails if current_subject_fingerprint does not match bound subject_fingerprint.
func (s *PgStore) VerifyRelease(ctx context.Context, params domain.VerifyReleaseParams) (*domain.ReleaseVerificationResult, error) {
	current, err := s.FindWorkflowByID(ctx, params.WorkflowInstanceID)
	if err != nil {
		return nil, err
	}

	res := &domain.ReleaseVerificationResult{
		WorkflowInstanceID: current.WorkflowInstanceID,
		WorkflowStatus:     current.WorkflowStatus,
		SubjectFingerprint: current.SubjectFingerprint,
	}

	// 1. Check for unbound subject - fail safely
	if current.SubjectFingerprint == nil || *current.SubjectFingerprint == "" {
		reason := domain.ErrWorkflowUnboundSubject.Error()
		res.CanRelease = false
		res.Status = "INVALID"
		res.Reason = &reason
		return res, nil
	}

	// 2. Check workflow status
	if current.WorkflowStatus == domain.WorkflowStatusInvalidated {
		reason := domain.ErrWorkflowInvalidated.Error()
		res.CanRelease = false
		res.Status = "INVALID"
		res.Reason = &reason
		return res, nil
	}
	if current.WorkflowStatus != domain.WorkflowStatusApproved {
		reason := domain.ErrWorkflowNotApproved.Error()
		res.CanRelease = false
		res.Status = "INVALID"
		res.Reason = &reason
		return res, nil
	}

	// 3. Check version match if expected version provided
	if params.ExpectedSubjectVersion != nil {
		if current.SubjectVersion == nil || *current.SubjectVersion != *params.ExpectedSubjectVersion {
			reason := domain.ErrSubjectVersionMismatch.Error()
			res.CanRelease = false
			res.Status = "INVALID"
			res.Reason = &reason
			return res, nil
		}
	}

	// 4. Check fingerprint match
	if *current.SubjectFingerprint != params.CurrentSubjectFingerprint {
		reason := domain.ErrSubjectFingerprintMismatch.Error()
		res.CanRelease = false
		res.Status = "INVALID"
		res.Reason = &reason
		return res, nil
	}

	res.CanRelease = true
	res.Status = "VALID"
	return res, nil
}

// Workflow Definition columns and scanning
const definitionColumns = `workflow_definition_id, tenant_id, workflow_type, version, stages_json, name, description, created_by, created_at, superseded_by, is_active, row_version, updated_at, plane, data_class, residency_region, expiry_hours, escalation_target`

func scanDefinition(row pgx.Row) (*domain.WorkflowDefinition, error) {
	d := &domain.WorkflowDefinition{}
	err := row.Scan(&d.WorkflowDefinitionID, &d.TenantID, &d.WorkflowType, &d.Version, &d.StagesJSON,
		&d.Name, &d.Description, &d.CreatedBy, &d.CreatedAt, &d.SupersededBy, &d.IsActive,
		&d.RowVersion, &d.UpdatedAt, &d.Plane, &d.DataClass, &d.ResidencyRegion, &d.ExpiryHours, &d.EscalationTarget)
	return d, err
}

// CreateWorkflowDefinition creates a new versioned workflow definition.
func (s *PgStore) CreateWorkflowDefinition(ctx context.Context, params domain.CreateWorkflowDefinitionParams) (*domain.WorkflowDefinition, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.log.Error("pg CreateWorkflowDefinition: begin tx failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", params.TenantID); err != nil {
		return nil, fmt.Errorf("set_config app.tenant_id: %w", err)
	}

	const query = `
		INSERT INTO workflow_definitions (workflow_definition_id, tenant_id, workflow_type, version, stages_json, name, description, created_by, created_at, is_active, expiry_hours, escalation_target)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, NOW(), true, $8, $9)
		RETURNING ` + definitionColumns + `;`

	row := tx.QueryRow(ctx, query, params.TenantID, params.WorkflowType, params.Version, params.StagesJSON, params.Name, params.Description, params.CreatedBy, params.ExpiryHours, params.EscalationTarget)
	definition, err := scanDefinition(row)
	if err != nil {
		s.log.Error("pg CreateWorkflowDefinition: insert failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	if err := tx.Commit(ctx); err != nil {
		s.log.Error("pg CreateWorkflowDefinition: commit failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return definition, nil
}

// GetWorkflowDefinition retrieves a workflow definition by ID or latest active version.
func (s *PgStore) GetWorkflowDefinition(ctx context.Context, params domain.GetWorkflowDefinitionParams) (*domain.WorkflowDefinition, error) {
	var query string
	var args []any

	if params.WorkflowDefinitionID != nil {
		query = `SELECT ` + definitionColumns + ` FROM workflow_definitions WHERE workflow_definition_id = $1 AND tenant_id = $2;`
		args = []any{*params.WorkflowDefinitionID, params.TenantID}
	} else if params.Version != nil {
		query = `SELECT ` + definitionColumns + ` FROM workflow_definitions WHERE tenant_id = $1 AND workflow_type = $2 AND version = $3;`
		args = []any{params.TenantID, params.WorkflowType, *params.Version}
	} else {
		// Latest active version
		query = `SELECT ` + definitionColumns + ` FROM workflow_definitions WHERE tenant_id = $1 AND workflow_type = $2 AND is_active = true ORDER BY version DESC LIMIT 1;`
		args = []any{params.TenantID, params.WorkflowType}
	}

	var definition *domain.WorkflowDefinition
	err := s.withRLS(ctx, params.TenantID, func(tx pgx.Tx) error {
		var scanErr error
		definition, scanErr = scanDefinition(tx.QueryRow(ctx, query, args...))
		return scanErr
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrWorkflowNotFound
		}
		s.log.Error("pg GetWorkflowDefinition failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return definition, nil
}

// ListPendingApprovals lists workflows pending approval for a tenant.
func (s *PgStore) ListPendingApprovals(ctx context.Context, tenantID string, limit, offset int) ([]*domain.WorkflowInstance, error) {
	const query = `SELECT ` + instanceColumns + ` FROM workflow_instances WHERE tenant_id = $1 AND workflow_status IN ('PENDING','ESCALATED') ORDER BY started_at DESC, workflow_instance_id DESC LIMIT $2 OFFSET $3;`

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.log.Error("pg ListPendingApprovals failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return nil, fmt.Errorf("%w: set_config app.tenant_id: %v", domain.ErrStoreUnavailable, err)
	}

	rows, err := tx.Query(ctx, query, tenantID, limit, offset)
	if err != nil {
		s.log.Error("pg ListPendingApprovals failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	var instances []*domain.WorkflowInstance
	for rows.Next() {
		inst, err := scanInstance(rows)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		instances = append(instances, inst)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return instances, nil
}
