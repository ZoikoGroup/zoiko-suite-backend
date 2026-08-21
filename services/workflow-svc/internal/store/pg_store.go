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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/domain"
	svcmiddleware "zoiko.io/workflow-svc/internal/middleware"
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

	EscalateWorkflow(ctx context.Context, workflowInstanceID, actorPrincipalID string) (*domain.WorkflowInstance, bool, error)
	CancelWorkflow(ctx context.Context, workflowInstanceID, actorPrincipalID string) (*domain.WorkflowInstance, bool, error)
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

// ── workflow_instances ───────────────────────────────────────────────────────

const instanceColumns = `workflow_instance_id, tenant_id, legal_entity_id, workflow_type, workflow_status, current_stage, initiated_by, correlation_id, started_at, completed_at`

func scanInstance(row pgx.Row) (*domain.WorkflowInstance, error) {
	w := &domain.WorkflowInstance{}
	err := row.Scan(&w.WorkflowInstanceID, &w.TenantID, &w.LegalEntityID, &w.WorkflowType, &w.WorkflowStatus,
		&w.CurrentStage, &w.InitiatedBy, &w.CorrelationID, &w.StartedAt, &w.CompletedAt)
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
	var tenantID *string
	if t := svcmiddleware.TenantFromContext(ctx); t != "" {
		tenantID = &t
	}
	const query = `SELECT ` + instanceColumns + ` FROM workflow_instances WHERE workflow_instance_id = $1 AND ($2::uuid IS NULL OR tenant_id = $2::uuid);`
	row := s.pool.QueryRow(ctx, query, workflowInstanceID, tenantID)
	w, err := scanInstance(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrWorkflowNotFound
		}
		s.log.Error("pg FindWorkflowByID failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return w, nil
}

const stageColumns = `workflow_stage_id, workflow_instance_id, stage_order, approver_principal_id, stage_status, acted_at, rationale`

func scanStage(row pgx.Row) (*domain.WorkflowStage, error) {
	st := &domain.WorkflowStage{}
	err := row.Scan(&st.WorkflowStageID, &st.WorkflowInstanceID, &st.StageOrder, &st.ApproverPrincipalID, &st.StageStatus, &st.ActedAt, &st.Rationale)
	return st, err
}

func (s *PgStore) FindStagesByWorkflowID(ctx context.Context, workflowInstanceID string) ([]*domain.WorkflowStage, error) {
	if _, err := s.FindWorkflowByID(ctx, workflowInstanceID); err != nil {
		return nil, err
	}
	const query = `SELECT ` + stageColumns + ` FROM workflow_stages WHERE workflow_instance_id = $1 ORDER BY stage_order;`
	rows, err := s.pool.Query(ctx, query, workflowInstanceID)
	if err != nil {
		s.log.Error("pg FindStagesByWorkflowID failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	var stages []*domain.WorkflowStage
	for rows.Next() {
		st, scanErr := scanStage(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		stages = append(stages, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return stages, nil
}

// findStageByApprover looks up the stage in workflowInstanceID assigned to
// approverPrincipalID. Returns domain.ErrWrongApprover if none exists —
// this principal is not an approver anywhere in this workflow's chain.
// Assumes at most one stage per approver per workflow (v1 simplification).
func (s *PgStore) findStageByApprover(ctx context.Context, workflowInstanceID, approverPrincipalID string) (*domain.WorkflowStage, error) {
	const query = `SELECT ` + stageColumns + ` FROM workflow_stages WHERE workflow_instance_id = $1 AND approver_principal_id = $2 LIMIT 1;`
	row := s.pool.QueryRow(ctx, query, workflowInstanceID, approverPrincipalID)
	st, err := scanStage(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrWrongApprover
		}
		s.log.Error("pg findStageByApprover failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return st, nil
}

func (s *PgStore) FindCurrentStage(ctx context.Context, workflowInstanceID string) (*domain.WorkflowStage, error) {
	instance, err := s.FindWorkflowByID(ctx, workflowInstanceID)
	if err != nil {
		return nil, err
	}
	if instance.CurrentStage == 0 {
		return nil, domain.ErrWorkflowNotFound // terminal — no current stage
	}
	const query = `SELECT ` + stageColumns + ` FROM workflow_stages WHERE workflow_instance_id = $1 AND stage_order = $2;`
	row := s.pool.QueryRow(ctx, query, workflowInstanceID, instance.CurrentStage)
	st, err := scanStage(row)
	if err != nil {
		s.log.Error("pg FindCurrentStage failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return st, nil
}

// CreateWorkflow inserts a new workflow instance plus its ordered stage
// chain, and records the initial "" -> PENDING transition, all in one
// transaction.
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

	// Idempotent on (tenant_id, correlation_id): a retried call with the
	// same correlation_id must resolve to the original instance, never
	// create a second workflow (with its own duplicate stage chain) —
	// see migration 000003. ON CONFLICT DO NOTHING here returns zero rows
	// rather than erroring, which is how the conflict is detected below.
	const insertInstance = `
		INSERT INTO workflow_instances (workflow_instance_id, tenant_id, legal_entity_id, workflow_type, initiated_by, correlation_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, correlation_id) WHERE correlation_id != '' DO NOTHING
		RETURNING ` + instanceColumns + `;`
	row := tx.QueryRow(ctx, insertInstance, params.WorkflowInstanceID, params.TenantID, params.LegalEntityID, params.WorkflowType, params.InitiatedBy, params.CorrelationID)
	instance, err := scanInstance(row)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			s.log.Error("pg CreateWorkflow: insert instance failed", zap.Error(err))
			return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		// Conflict: an earlier call with this correlation_id already
		// created the instance. Nothing to insert — fetch and return what
		// already exists, in a fresh read (the failed INSERT already ended
		// this transaction's usefulness; commit below is a no-op).
		return s.findExistingByCorrelation(ctx, params.TenantID, params.CorrelationID)
	}

	stages := make([]*domain.WorkflowStage, 0, len(params.Stages))
	const insertStage = `
		INSERT INTO workflow_stages (workflow_instance_id, stage_order, approver_principal_id)
		VALUES ($1, $2, $3)
		RETURNING ` + stageColumns + `;`
	for i, stageInput := range params.Stages {
		row := tx.QueryRow(ctx, insertStage, params.WorkflowInstanceID, i+1, stageInput.ApproverPrincipalID)
		st, err := scanStage(row)
		if err != nil {
			s.log.Error("pg CreateWorkflow: insert stage failed", zap.Error(err))
			return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
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

	if err := tx.Commit(ctx); err != nil {
		s.log.Error("pg CreateWorkflow: commit failed", zap.Error(err))
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return instance, stages, true, nil
}

// findExistingByCorrelation resolves the instance+stages an idempotent
// CreateWorkflow retry should return, in its own read against the
// already-committed data from the original call.
func (s *PgStore) findExistingByCorrelation(ctx context.Context, tenantID, correlationID string) (*domain.WorkflowInstance, []*domain.WorkflowStage, bool, error) {
	const query = `SELECT ` + instanceColumns + ` FROM workflow_instances WHERE tenant_id = $1 AND correlation_id = $2;`
	row := s.pool.QueryRow(ctx, query, tenantID, correlationID)
	instance, err := scanInstance(row)
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
	const query = `
		INSERT INTO workflow_transitions (workflow_instance_id, from_state, to_state, acted_by, rationale, correlation_id, causation_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7);`
	if _, err := tx.Exec(ctx, query, workflowInstanceID, fromState, toState, actedBy, rationale, correlationID, causationID); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

// SubmitAction applies an APPROVE/REJECT action to the current stage.
//
// Idempotency (doctrine requirement): if the stage is already in the exact
// outcome this action would produce, this is a no-op — transitioned=false,
// no new transition row, no re-publish. If the stage is in a *different*
// terminal outcome (e.g. already REJECTED but caller submits APPROVE), that
// is a real conflict, not idempotency — domain.ErrInvalidTransition.
func (s *PgStore) SubmitAction(ctx context.Context, params domain.SubmitActionParams) (instance *domain.WorkflowInstance, stage *domain.WorkflowStage, transitioned bool, err error) {
	current, err := s.FindWorkflowByID(ctx, params.WorkflowInstanceID)
	if err != nil {
		return nil, nil, false, err
	}
	if current.WorkflowStatus != "PENDING" {
		// Workflow already reached a terminal or escalated state — no
		// action can be submitted against it.
		return nil, nil, false, domain.ErrInvalidTransition
	}

	// Look up the actor's OWN stage first, not just "the current stage" —
	// a duplicate/retried submission must be recognized as idempotent even
	// after the workflow has already advanced past that stage (e.g. a
	// slow network retry of stage 1's approval arriving after stage 2 has
	// already become current). Assumes at most one stage per approver per
	// workflow — a documented v1 simplification.
	actorStage, err := s.findStageByApprover(ctx, params.WorkflowInstanceID, params.ActorPrincipalID)
	if err != nil {
		return nil, nil, false, err
	}

	wantStatus := "APPROVED"
	if params.Action == "REJECT" {
		wantStatus = "REJECTED"
	}
	if actorStage.StageStatus == wantStatus {
		// Idempotent replay of the identical action, even if the workflow
		// has since moved on — doctrine requirement.
		return current, actorStage, false, nil
	}
	if actorStage.StageStatus != "PENDING" {
		// Actor's stage already resolved to the OTHER outcome — a real conflict.
		return nil, nil, false, domain.ErrInvalidTransition
	}
	if actorStage.StageOrder != current.CurrentStage {
		// Actor's stage is still PENDING but it is not yet their turn.
		return nil, nil, false, domain.ErrWrongApprover
	}
	st := actorStage

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.log.Error("pg SubmitAction: begin tx failed", zap.Error(err))
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const updateStage = `
		UPDATE workflow_stages
		SET stage_status = $1, acted_at = NOW(), rationale = $2
		WHERE workflow_stage_id = $3
		RETURNING ` + stageColumns + `;`
	row := tx.QueryRow(ctx, updateStage, wantStatus, params.Rationale, st.WorkflowStageID)
	updatedStage, err := scanStage(row)
	if err != nil {
		s.log.Error("pg SubmitAction: update stage failed", zap.Error(err))
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	isLastStage, err := isFinalStage(ctx, tx, params.WorkflowInstanceID, st.StageOrder)
	if err != nil {
		return nil, nil, false, err
	}

	newInstanceStatus := "PENDING"
	newCurrentStage := st.StageOrder + 1
	switch {
	case params.Action == "REJECT":
		newInstanceStatus = "REJECTED"
		newCurrentStage = 0
	case params.Action == "APPROVE" && isLastStage:
		newInstanceStatus = "APPROVED"
		newCurrentStage = 0
	}

	const updateInstance = `
		UPDATE workflow_instances
		SET workflow_status = $1, current_stage = $2,
		    completed_at = CASE WHEN $1::VARCHAR != 'PENDING' THEN NOW() ELSE completed_at END
		WHERE workflow_instance_id = $3
		RETURNING ` + instanceColumns + `;`
	row = tx.QueryRow(ctx, updateInstance, newInstanceStatus, newCurrentStage, params.WorkflowInstanceID)
	updatedInstance, err := scanInstance(row)
	if err != nil {
		s.log.Error("pg SubmitAction: update instance failed", zap.Error(err))
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	rationale := fmt.Sprintf("stage %d %s by %s", st.StageOrder, wantStatus, params.ActorPrincipalID)
	var instanceCorrelationID *string
	if current.CorrelationID != "" {
		instanceCorrelationID = &current.CorrelationID
	}
	if err := insertTransition(ctx, tx, params.WorkflowInstanceID, "PENDING", newInstanceStatus, params.ActorPrincipalID, &rationale, instanceCorrelationID, params.CausationID); err != nil {
		return nil, nil, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		s.log.Error("pg SubmitAction: commit failed", zap.Error(err))
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return updatedInstance, updatedStage, true, nil
}

func isFinalStage(ctx context.Context, tx pgx.Tx, workflowInstanceID string, stageOrder int) (bool, error) {
	const query = `SELECT MAX(stage_order) FROM workflow_stages WHERE workflow_instance_id = $1;`
	var maxOrder int
	if err := tx.QueryRow(ctx, query, workflowInstanceID).Scan(&maxOrder); err != nil {
		return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return stageOrder == maxOrder, nil
}

// EscalateWorkflow transitions PENDING -> ESCALATED. Idempotent if already ESCALATED.
func (s *PgStore) EscalateWorkflow(ctx context.Context, workflowInstanceID, actorPrincipalID string) (*domain.WorkflowInstance, bool, error) {
	current, err := s.FindWorkflowByID(ctx, workflowInstanceID)
	if err != nil {
		return nil, false, err
	}
	if current.WorkflowStatus == "ESCALATED" {
		return current, false, nil
	}
	if current.WorkflowStatus != "PENDING" {
		return nil, false, domain.ErrInvalidTransition
	}
	return s.transitionInstanceStatus(ctx, workflowInstanceID, "PENDING", "ESCALATED", actorPrincipalID, 0)
}

// CancelWorkflow transitions PENDING or ESCALATED -> CANCELLED. Idempotent if
// already CANCELLED. Illegal from APPROVED/REJECTED (terminal).
func (s *PgStore) CancelWorkflow(ctx context.Context, workflowInstanceID, actorPrincipalID string) (*domain.WorkflowInstance, bool, error) {
	current, err := s.FindWorkflowByID(ctx, workflowInstanceID)
	if err != nil {
		return nil, false, err
	}
	if current.WorkflowStatus == "CANCELLED" {
		return current, false, nil
	}
	if current.WorkflowStatus != "PENDING" && current.WorkflowStatus != "ESCALATED" {
		return nil, false, domain.ErrInvalidTransition
	}
	return s.transitionInstanceStatus(ctx, workflowInstanceID, current.WorkflowStatus, "CANCELLED", actorPrincipalID, 0)
}

func (s *PgStore) transitionInstanceStatus(ctx context.Context, workflowInstanceID, fromState, toState, actorPrincipalID string, newCurrentStage int) (*domain.WorkflowInstance, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.log.Error("pg transitionInstanceStatus: begin tx failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const query = `
		UPDATE workflow_instances
		SET workflow_status = $1, current_stage = $2,
		    completed_at = CASE WHEN $1::VARCHAR IN ('APPROVED','REJECTED','CANCELLED') THEN NOW() ELSE completed_at END
		WHERE workflow_instance_id = $3
		RETURNING ` + instanceColumns + `;`
	row := tx.QueryRow(ctx, query, toState, newCurrentStage, workflowInstanceID)
	updated, err := scanInstance(row)
	if err != nil {
		s.log.Error("pg transitionInstanceStatus: update failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	var instanceCorrelationID *string
	if updated.CorrelationID != "" {
		instanceCorrelationID = &updated.CorrelationID
	}
	if err := insertTransition(ctx, tx, workflowInstanceID, fromState, toState, actorPrincipalID, nil, instanceCorrelationID, nil); err != nil {
		return nil, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		s.log.Error("pg transitionInstanceStatus: commit failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return updated, true, nil
}
