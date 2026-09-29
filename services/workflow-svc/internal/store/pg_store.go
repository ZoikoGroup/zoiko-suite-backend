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

	EscalateWorkflow(ctx context.Context, workflowInstanceID, actorPrincipalID string) (*domain.WorkflowInstance, bool, error)
	CancelWorkflow(ctx context.Context, workflowInstanceID, actorPrincipalID string) (*domain.WorkflowInstance, bool, error)
	InvalidateWorkflow(ctx context.Context, params domain.InvalidateWorkflowParams) (*domain.WorkflowInstance, bool, error)
	VerifyRelease(ctx context.Context, params domain.VerifyReleaseParams) (*domain.ReleaseVerificationResult, error)
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

const instanceColumns = `workflow_instance_id, tenant_id, legal_entity_id, workflow_type, workflow_status, current_stage, initiated_by, correlation_id, started_at, completed_at, subject_type, subject_id, subject_version, subject_fingerprint, invalidated_at, invalidation_reason_code, invalidation_narrative, invalidation_evidence_refs`

func scanInstance(row pgx.Row) (*domain.WorkflowInstance, error) {
	w := &domain.WorkflowInstance{}
	err := row.Scan(&w.WorkflowInstanceID, &w.TenantID, &w.LegalEntityID, &w.WorkflowType, &w.WorkflowStatus,
		&w.CurrentStage, &w.InitiatedBy, &w.CorrelationID, &w.StartedAt, &w.CompletedAt,
		&w.SubjectType, &w.SubjectID, &w.SubjectVersion, &w.SubjectFingerprint,
		&w.InvalidatedAt, &w.InvalidationReasonCode, &w.InvalidationNarrative, &w.InvalidationEvidenceRefs)
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

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", params.TenantID); err != nil {
		return nil, nil, false, fmt.Errorf("set_config app.tenant_id: %w", err)
	}

	// Idempotent on (tenant_id, correlation_id): a retried call with the
	// same correlation_id must resolve to the original instance, never
	// create a second workflow (with its own duplicate stage chain) —
	// see migration 000003. ON CONFLICT DO NOTHING here returns zero rows
	// rather than erroring, which is how the conflict is detected below.
	const insertInstance = `
		INSERT INTO workflow_instances (workflow_instance_id, tenant_id, legal_entity_id, workflow_type, initiated_by, correlation_id, subject_type, subject_id, subject_version, subject_fingerprint)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (tenant_id, correlation_id) WHERE correlation_id != '' DO NOTHING
		RETURNING ` + instanceColumns + `;`
	row := tx.QueryRow(ctx, insertInstance, params.WorkflowInstanceID, params.TenantID, params.LegalEntityID, params.WorkflowType, params.InitiatedBy, params.CorrelationID, params.SubjectType, params.SubjectID, params.SubjectVersion, params.SubjectFingerprint)
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

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", svcmiddleware.TenantFromContext(ctx)); err != nil {
		return nil, nil, false, fmt.Errorf("set_config app.tenant_id: %w", err)
	}

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

	stageEventType := "approval.granted"
	if updatedStage.StageStatus == "REJECTED" {
		stageEventType = "approval.rejected"
	}
	if err := outbox.Insert(ctx, tx, outbox.Event{
		AggregateType: "workflow_instance",
		AggregateID:   params.WorkflowInstanceID,
		EventType:     stageEventType,
		TenantID:      updatedInstance.TenantID,
		LegalEntityID: updatedInstance.LegalEntityID,
		ActorID:       &params.ActorPrincipalID,
		CorrelationID: instanceCorrelationID,
		Payload: map[string]any{
			"workflow_instance_id":  updatedInstance.WorkflowInstanceID,
			"stage_order":           updatedStage.StageOrder,
			"approver_principal_id": updatedStage.ApproverPrincipalID,
		},
	}); err != nil {
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	if updatedInstance.WorkflowStatus == "APPROVED" || updatedInstance.WorkflowStatus == "REJECTED" {
		if err := outbox.Insert(ctx, tx, outbox.Event{
			AggregateType: "workflow_instance",
			AggregateID:   params.WorkflowInstanceID,
			EventType:     "workflow.completed",
			TenantID:      updatedInstance.TenantID,
			LegalEntityID: updatedInstance.LegalEntityID,
			ActorID:       &params.ActorPrincipalID,
			CorrelationID: instanceCorrelationID,
			Payload: map[string]any{
				"workflow_instance_id": updatedInstance.WorkflowInstanceID,
				"workflow_status":      updatedInstance.WorkflowStatus,
				"completed_at":         updatedInstance.CompletedAt,
			},
		}); err != nil {
			return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
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

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", svcmiddleware.TenantFromContext(ctx)); err != nil {
		return nil, false, fmt.Errorf("set_config app.tenant_id: %w", err)
	}

	const query = `
		UPDATE workflow_instances
		SET workflow_status = $1, current_stage = $2,
		    completed_at = CASE WHEN $1::VARCHAR IN ('APPROVED','REJECTED','CANCELLED','INVALIDATED') THEN NOW() ELSE completed_at END
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

	if toState == "ESCALATED" {
		if err := outbox.Insert(ctx, tx, outbox.Event{
			AggregateType: "workflow_instance",
			AggregateID:   workflowInstanceID,
			EventType:     "workflow.escalated",
			TenantID:      updated.TenantID,
			LegalEntityID: updated.LegalEntityID,
			ActorID:       &actorPrincipalID,
			CorrelationID: instanceCorrelationID,
			Payload: map[string]any{
				"workflow_instance_id": updated.WorkflowInstanceID,
				"current_stage":        updated.CurrentStage,
			},
		}); err != nil {
			return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
	} else if toState == "CANCELLED" {
		if err := outbox.Insert(ctx, tx, outbox.Event{
			AggregateType: "workflow_instance",
			AggregateID:   workflowInstanceID,
			EventType:     "workflow.completed",
			TenantID:      updated.TenantID,
			LegalEntityID: updated.LegalEntityID,
			ActorID:       &actorPrincipalID,
			CorrelationID: instanceCorrelationID,
			Payload: map[string]any{
				"workflow_instance_id": updated.WorkflowInstanceID,
				"workflow_status":      updated.WorkflowStatus,
				"completed_at":         updated.CompletedAt,
			},
		}); err != nil {
			return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		s.log.Error("pg transitionInstanceStatus: commit failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return updated, true, nil
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
		    invalidation_evidence_refs = $3
		WHERE workflow_instance_id = $4
		RETURNING ` + instanceColumns + `;`

	evidenceRefs := params.EvidenceRefs
	if evidenceRefs == nil {
		evidenceRefs = []string{}
	}

	row := tx.QueryRow(ctx, query, params.ReasonCode, params.Narrative, evidenceRefs, params.WorkflowInstanceID)
	updated, err := scanInstance(row)
	if err != nil {
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

	if err := insertTransition(ctx, tx, params.WorkflowInstanceID, current.WorkflowStatus, domain.WorkflowStatusInvalidated, params.ActorPrincipalID, &rationale, instanceCorrelationID, params.CausationID); err != nil {
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
