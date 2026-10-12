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

	// SubmitQuorumVote casts one eligible approver's vote against the
	// current QUORUM stage. Same return-shape contract as SubmitAction.
	SubmitQuorumVote(ctx context.Context, params domain.SubmitQuorumVoteParams) (*domain.WorkflowInstance, *domain.WorkflowStage, bool, error)

	EscalateWorkflow(ctx context.Context, workflowInstanceID, actorPrincipalID string) (*domain.WorkflowInstance, bool, error)
	CancelWorkflow(ctx context.Context, workflowInstanceID, actorPrincipalID string) (*domain.WorkflowInstance, bool, error)
	InvalidateWorkflow(ctx context.Context, params domain.InvalidateWorkflowParams) (*domain.WorkflowInstance, bool, error)
	VerifyRelease(ctx context.Context, params domain.VerifyReleaseParams) (*domain.ReleaseVerificationResult, error)

	// Workflow Definitions (versioned, immutable) per R-001 WFC-02
	CreateWorkflowDefinition(ctx context.Context, params domain.CreateWorkflowDefinitionParams) (*domain.WorkflowDefinition, error)
	GetWorkflowDefinition(ctx context.Context, params domain.GetWorkflowDefinitionParams) (*domain.WorkflowDefinition, error)
	ListWorkflowDefinitions(ctx context.Context, params domain.ListWorkflowDefinitionsParams) ([]*domain.WorkflowDefinition, error)
	SupersedeWorkflowDefinition(ctx context.Context, oldDefID, newDefID string) error
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

const instanceColumns = `workflow_instance_id, tenant_id, legal_entity_id, workflow_type, workflow_status, current_stage, initiated_by, correlation_id, started_at, completed_at, subject_type, subject_id, subject_version, subject_fingerprint, invalidated_at, invalidation_reason_code, invalidation_narrative, invalidation_evidence_refs, row_version, updated_at, created_by, plane, data_class, residency_region, idempotency_key, due_at, next_action_at, workflow_definition_id, workflow_definition_version`

func scanInstance(row pgx.Row) (*domain.WorkflowInstance, error) {
	w := &domain.WorkflowInstance{}
	// idempotency_key (000013) and the definition reference (000016) are
	// nullable: a workflow created without an Idempotency-Key or a definition
	// has none, and scanning NULL into a plain string or int fails.
	var idempotencyKey, definitionID *string
	var definitionVersion *int
	err := row.Scan(&w.WorkflowInstanceID, &w.TenantID, &w.LegalEntityID, &w.WorkflowType, &w.WorkflowStatus,
		&w.CurrentStage, &w.InitiatedBy, &w.CorrelationID, &w.StartedAt, &w.CompletedAt,
		&w.SubjectType, &w.SubjectID, &w.SubjectVersion, &w.SubjectFingerprint,
		&w.InvalidatedAt, &w.InvalidationReasonCode, &w.InvalidationNarrative, &w.InvalidationEvidenceRefs,
		&w.RowVersion, &w.UpdatedAt, &w.CreatedBy, &w.Plane, &w.DataClass, &w.ResidencyRegion, &idempotencyKey,
		&w.DueAt, &w.NextActionAt, &definitionID, &definitionVersion)
	if idempotencyKey != nil {
		w.IdempotencyKey = *idempotencyKey
	}
	if definitionID != nil {
		w.WorkflowDefinitionID = *definitionID
	}
	if definitionVersion != nil {
		w.WorkflowDefinitionVersion = *definitionVersion
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

const stageColumns = `workflow_stage_id, workflow_instance_id, stage_order, stage_type, approver_principal_id, required_approvals, stage_status, acted_at, rationale, row_version, updated_at, created_by, plane, data_class, residency_region`

func scanStage(row pgx.Row) (*domain.WorkflowStage, error) {
	st := &domain.WorkflowStage{}
	var approverPrincipalID *string
	err := row.Scan(&st.WorkflowStageID, &st.WorkflowInstanceID, &st.StageOrder, &st.StageType, &approverPrincipalID,
		&st.RequiredApprovals, &st.StageStatus, &st.ActedAt, &st.Rationale,
		&st.RowVersion, &st.UpdatedAt, &st.CreatedBy, &st.Plane, &st.DataClass, &st.ResidencyRegion)
	if approverPrincipalID != nil {
		st.ApproverPrincipalID = *approverPrincipalID
	}
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
// Uses ORDER BY stage_order to return the earliest matching stage.
func (s *PgStore) findStageByApprover(ctx context.Context, workflowInstanceID, approverPrincipalID string) (*domain.WorkflowStage, error) {
	const query = `SELECT ` + stageColumns + ` FROM workflow_stages WHERE workflow_instance_id = $1 AND approver_principal_id = $2 ORDER BY stage_order LIMIT 1;`
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

	// Determine which idempotency key to use
	idemKey := params.IdempotencyKey
	if idemKey == "" {
		idemKey = params.CorrelationID // fallback for backward compatibility
	}

	var instance *domain.WorkflowInstance

	if idemKey != "" {
		// Try to insert with idempotency key
		const insertInstance = `
			INSERT INTO workflow_instances (workflow_instance_id, tenant_id, legal_entity_id, workflow_type, initiated_by, correlation_id, subject_type, subject_id, subject_version, subject_fingerprint, idempotency_key)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			ON CONFLICT (tenant_id, idempotency_key) WHERE idempotency_key IS NOT NULL AND idempotency_key != '' DO NOTHING
			RETURNING ` + instanceColumns + `;`
		row := tx.QueryRow(ctx, insertInstance, params.WorkflowInstanceID, params.TenantID, params.LegalEntityID, params.WorkflowType, params.InitiatedBy, params.CorrelationID, params.SubjectType, params.SubjectID, params.SubjectVersion, params.SubjectFingerprint, idemKey)
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
			if existing != nil && s.workflowBodyDiffers(existing, params) {
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
			INSERT INTO workflow_instances (workflow_instance_id, tenant_id, legal_entity_id, workflow_type, initiated_by, correlation_id, subject_type, subject_id, subject_version, subject_fingerprint)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			RETURNING ` + instanceColumns + `;`
		row := tx.QueryRow(ctx, insertInstance, params.WorkflowInstanceID, params.TenantID, params.LegalEntityID, params.WorkflowType, params.InitiatedBy, params.CorrelationID, params.SubjectType, params.SubjectID, params.SubjectVersion, params.SubjectFingerprint)
		instance, err = scanInstance(row)
		if err != nil {
			s.log.Error("pg CreateWorkflow: insert instance failed", zap.Error(err))
			return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
	}

	stages := make([]*domain.WorkflowStage, 0, len(params.Stages))
	const insertSingleStage = `
		INSERT INTO workflow_stages (workflow_instance_id, stage_order, stage_type, approver_principal_id)
		VALUES ($1, $2, 'SINGLE', $3)
		RETURNING ` + stageColumns + `;`
	const insertQuorumStage = `
		INSERT INTO workflow_stages (workflow_instance_id, stage_order, stage_type, required_approvals)
		VALUES ($1, $2, 'QUORUM', $3)
		RETURNING ` + stageColumns + `;`
	const insertQuorumApprover = `
		INSERT INTO workflow_stage_quorum_approvers (workflow_stage_id, approver_principal_id) VALUES ($1, $2);`
	for i, stageInput := range params.Stages {
		stageType := stageInput.StageType
		if stageType == "" {
			stageType = domain.StageTypeSingle
		}
		var row pgx.Row
		if stageType == domain.StageTypeQuorum {
			row = tx.QueryRow(ctx, insertQuorumStage, params.WorkflowInstanceID, i+1, stageInput.RequiredApprovals)
		} else {
			row = tx.QueryRow(ctx, insertSingleStage, params.WorkflowInstanceID, i+1, stageInput.ApproverPrincipalID)
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

// workflowBodyDiffers compares the existing workflow with new params to detect IDEMPOTENCY_MISMATCH
func (s *PgStore) workflowBodyDiffers(existing *domain.WorkflowInstance, params domain.CreateWorkflowParams) bool {
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
	// TODO: Compare stages when needed
	return false
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
	// Allow actions on PENDING or ESCALATED workflows.
	// ESCALATED is not a terminal state — it can be resolved to APPROVED/REJECTED.
	if current.WorkflowStatus != "PENDING" && current.WorkflowStatus != "ESCALATED" {
		// Workflow already reached a terminal state (APPROVED, REJECTED, CANCELLED, INVALIDATED)
		// — no action can be submitted against it.
		return nil, nil, false, domain.ErrInvalidTransition
	}

	// Determine which approver's stage to look up.
	// If AssignedApproverID is provided (delegation), use that.
	// Otherwise, use ActorPrincipalID (direct approval).
	approverID := params.ActorPrincipalID
	if params.AssignedApproverID != "" {
		approverID = params.AssignedApproverID
	}

	// Look up the approver's stage (not just "the current stage") —
	// a duplicate/retried submission must be recognized as idempotent even
	// after the workflow has already advanced past that stage (e.g. a
	// slow network retry of stage 1's approval arriving after stage 2 has
	// already become current). Assumes at most one stage per approver per
	// workflow — a documented v1 simplification.
	actorStage, err := s.findStageByApprover(ctx, params.WorkflowInstanceID, approverID)
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

	// Optimistic concurrency: update stage with row_version check.
	// This prevents lost updates from concurrent approvals on the same stage.
	const updateStage = `
		UPDATE workflow_stages
		SET stage_status = $1, acted_at = NOW(), rationale = $2,
		    row_version = row_version + 1, updated_at = NOW()
		WHERE workflow_stage_id = $3 AND row_version = $4
		RETURNING ` + stageColumns + `;`
	row := tx.QueryRow(ctx, updateStage, wantStatus, params.Rationale, st.WorkflowStageID, st.RowVersion)
	updatedStage, err := scanStage(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, false, domain.ErrConcurrencyConflict
		}
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

	// Optimistic concurrency: update instance with row_version check.
	const updateInstance = `
		UPDATE workflow_instances
		SET workflow_status = $1, current_stage = $2,
		    completed_at = CASE WHEN $1::VARCHAR != 'PENDING' THEN NOW() ELSE completed_at END,
		    row_version = row_version + 1, updated_at = NOW()
		WHERE workflow_instance_id = $3 AND row_version = $4
		RETURNING ` + instanceColumns + `;`
	row = tx.QueryRow(ctx, updateInstance, newInstanceStatus, newCurrentStage, params.WorkflowInstanceID, current.RowVersion)
	updatedInstance, err := scanInstance(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, false, domain.ErrConcurrencyConflict
		}
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

// SubmitQuorumVote casts one eligible approver's vote against the
// workflow's current QUORUM stage (ZS-SVC-R-001 §5.1/§8.3). This is a
// self-contained path, deliberately not sharing SubmitAction's
// internals — SubmitAction's single-approver lookup
// (findStageByApprover) assumes at most one stage per approver per
// workflow, which does not hold for a quorum pool, and duplicating
// the small "stage resolved -> advance instance" tail here keeps
// SubmitAction (and its existing test coverage) completely
// unmodified rather than risking a regression in a core, heavily
// exercised function for a feature it doesn't need to know about.
//
// Idempotency mirrors SubmitAction: a replay of the identical vote is
// a no-op (transitioned=false); a vote that conflicts with the
// approver's own prior vote is domain.ErrInvalidTransition. A new
// vote that does not yet resolve the stage (quorum neither reached
// nor mathematically failed) still returns transitioned=true — the
// vote was recorded — with the stage/instance left PENDING.
func (s *PgStore) SubmitQuorumVote(ctx context.Context, params domain.SubmitQuorumVoteParams) (instance *domain.WorkflowInstance, stage *domain.WorkflowStage, transitioned bool, err error) {
	current, err := s.FindWorkflowByID(ctx, params.WorkflowInstanceID)
	if err != nil {
		return nil, nil, false, err
	}
	if current.WorkflowStatus != "PENDING" {
		return nil, nil, false, domain.ErrInvalidTransition
	}

	const stageQuery = `SELECT ` + stageColumns + ` FROM workflow_stages WHERE workflow_instance_id = $1 AND stage_order = $2;`
	st, err := scanStage(s.pool.QueryRow(ctx, stageQuery, params.WorkflowInstanceID, current.CurrentStage))
	if err != nil {
		s.log.Error("pg SubmitQuorumVote: load current stage failed", zap.Error(err))
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if st.StageType != domain.StageTypeQuorum {
		return nil, nil, false, domain.ErrNotQuorumStage
	}
	if st.StageStatus != "PENDING" {
		return nil, nil, false, domain.ErrInvalidTransition
	}

	var eligible bool
	const eligibleQuery = `SELECT EXISTS(SELECT 1 FROM workflow_stage_quorum_approvers WHERE workflow_stage_id = $1 AND approver_principal_id = $2);`
	if err := s.pool.QueryRow(ctx, eligibleQuery, st.WorkflowStageID, params.ActorPrincipalID).Scan(&eligible); err != nil {
		s.log.Error("pg SubmitQuorumVote: eligibility check failed", zap.Error(err))
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if !eligible {
		return nil, nil, false, domain.ErrNotEligibleQuorumApprover
	}

	wantDecision := domain.QuorumDecisionApprove
	if params.Action == "REJECT" {
		wantDecision = domain.QuorumDecisionReject
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.log.Error("pg SubmitQuorumVote: begin tx failed", zap.Error(err))
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", svcmiddleware.TenantFromContext(ctx)); err != nil {
		return nil, nil, false, fmt.Errorf("set_config app.tenant_id: %w", err)
	}

	const insertDecision = `
		INSERT INTO workflow_stage_quorum_decisions (workflow_stage_id, approver_principal_id, decision, rationale)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (workflow_stage_id, approver_principal_id) DO NOTHING
		RETURNING decision;`
	var insertedDecision string
	insertErr := tx.QueryRow(ctx, insertDecision, st.WorkflowStageID, params.ActorPrincipalID, wantDecision, params.Rationale).Scan(&insertedDecision)
	if insertErr != nil {
		if !errors.Is(insertErr, pgx.ErrNoRows) {
			s.log.Error("pg SubmitQuorumVote: insert decision failed", zap.Error(insertErr))
			return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, insertErr)
		}
		// Conflict: this approver already voted. Idempotent replay of the
		// identical vote is a no-op; a different vote is a real conflict.
		const existingQuery = `SELECT decision FROM workflow_stage_quorum_decisions WHERE workflow_stage_id = $1 AND approver_principal_id = $2;`
		var existing string
		if err := tx.QueryRow(ctx, existingQuery, st.WorkflowStageID, params.ActorPrincipalID).Scan(&existing); err != nil {
			return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		if existing != wantDecision {
			return nil, nil, false, domain.ErrInvalidTransition
		}
		return current, st, false, nil
	}

	tally, err := quorumTally(ctx, tx, st.WorkflowStageID)
	if err != nil {
		return nil, nil, false, err
	}
	requiredApprovals := 0
	if st.RequiredApprovals != nil {
		requiredApprovals = *st.RequiredApprovals
	}
	qs := domain.QuorumStageStatus{
		PoolSize: tally.poolSize, RequiredApprovals: requiredApprovals,
		ApprovalCount: tally.approvals, RejectionCount: tally.rejections, VotesCast: tally.votesCast,
	}
	outcome, resolved := qs.Resolved()
	if !resolved {
		// Vote recorded; stage still awaiting more votes. Nothing else to
		// update — commit just the decision row.
		if err := tx.Commit(ctx); err != nil {
			return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		return current, st, true, nil
	}

	const updateStage = `
		UPDATE workflow_stages SET stage_status = $1, acted_at = NOW(), rationale = $2
		WHERE workflow_stage_id = $3 RETURNING ` + stageColumns + `;`
	rationale := fmt.Sprintf("quorum %s: %d/%d approvals (%d of %d eligible voted)", outcome, tally.approvals, requiredApprovals, tally.votesCast, tally.poolSize)
	updatedStage, err := scanStage(tx.QueryRow(ctx, updateStage, outcome, rationale, st.WorkflowStageID))
	if err != nil {
		s.log.Error("pg SubmitQuorumVote: update stage failed", zap.Error(err))
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	isLastStage, err := isFinalStage(ctx, tx, params.WorkflowInstanceID, st.StageOrder)
	if err != nil {
		return nil, nil, false, err
	}
	newInstanceStatus := "PENDING"
	newCurrentStage := st.StageOrder + 1
	switch {
	case outcome == "REJECTED":
		newInstanceStatus = "REJECTED"
		newCurrentStage = 0
	case outcome == "APPROVED" && isLastStage:
		newInstanceStatus = "APPROVED"
		newCurrentStage = 0
	}

	const updateInstance = `
		UPDATE workflow_instances
		SET workflow_status = $1, current_stage = $2,
		    completed_at = CASE WHEN $1::VARCHAR != 'PENDING' THEN NOW() ELSE completed_at END
		WHERE workflow_instance_id = $3
		RETURNING ` + instanceColumns + `;`
	updatedInstance, err := scanInstance(tx.QueryRow(ctx, updateInstance, newInstanceStatus, newCurrentStage, params.WorkflowInstanceID))
	if err != nil {
		s.log.Error("pg SubmitQuorumVote: update instance failed", zap.Error(err))
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

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
			"workflow_instance_id": updatedInstance.WorkflowInstanceID,
			"stage_order":          updatedStage.StageOrder,
			"quorum_approvals":     tally.approvals,
			"quorum_required":      requiredApprovals,
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
		s.log.Error("pg SubmitQuorumVote: commit failed", zap.Error(err))
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return updatedInstance, updatedStage, true, nil
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
	return s.transitionInstanceStatus(ctx, workflowInstanceID, "PENDING", "ESCALATED", actorPrincipalID, 0, current.RowVersion)
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
	return s.transitionInstanceStatus(ctx, workflowInstanceID, current.WorkflowStatus, "CANCELLED", actorPrincipalID, 0, current.RowVersion)
}

func (s *PgStore) transitionInstanceStatus(ctx context.Context, workflowInstanceID, fromState, toState, actorPrincipalID string, newCurrentStage int, expectedRowVersion int) (*domain.WorkflowInstance, bool, error) {
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
		    completed_at = CASE WHEN $1::VARCHAR IN ('APPROVED','REJECTED','CANCELLED','INVALIDATED') THEN NOW() ELSE completed_at END,
		    row_version = row_version + 1, updated_at = NOW()
		WHERE workflow_instance_id = $3 AND row_version = $4
		RETURNING ` + instanceColumns + `;`
	row := tx.QueryRow(ctx, query, toState, newCurrentStage, workflowInstanceID, expectedRowVersion)
	updated, err := scanInstance(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, domain.ErrConcurrencyConflict
		}
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

// Workflow Definition columns and scanning
const definitionColumns = `workflow_definition_id, tenant_id, workflow_type, version, stages_json, name, description, created_by, created_at, superseded_by, is_active, row_version, updated_at, plane, data_class, residency_region`

func scanDefinition(row pgx.Row) (*domain.WorkflowDefinition, error) {
	d := &domain.WorkflowDefinition{}
	err := row.Scan(&d.WorkflowDefinitionID, &d.TenantID, &d.WorkflowType, &d.Version, &d.StagesJSON,
		&d.Name, &d.Description, &d.CreatedBy, &d.CreatedAt, &d.SupersededBy, &d.IsActive,
		&d.RowVersion, &d.UpdatedAt, &d.Plane, &d.DataClass, &d.ResidencyRegion)
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
		INSERT INTO workflow_definitions (workflow_definition_id, tenant_id, workflow_type, version, stages_json, name, description, created_by, created_at, is_active)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, NOW(), true)
		RETURNING ` + definitionColumns + `;`

	row := tx.QueryRow(ctx, query, params.TenantID, params.WorkflowType, params.Version, params.StagesJSON, params.Name, params.Description, params.CreatedBy)
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
		query = `SELECT ` + definitionColumns + ` FROM workflow_definitions WHERE workflow_definition_id = $1;`
		args = []any{*params.WorkflowDefinitionID}
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

// ListWorkflowDefinitions lists workflow definitions with optional filters.
func (s *PgStore) ListWorkflowDefinitions(ctx context.Context, params domain.ListWorkflowDefinitionsParams) ([]*domain.WorkflowDefinition, error) {
	query := `SELECT ` + definitionColumns + ` FROM workflow_definitions WHERE tenant_id = $1`
	args := []any{params.TenantID}
	argIdx := 2

	if params.WorkflowType != nil {
		query += fmt.Sprintf(" AND workflow_type = $%d", argIdx)
		args = append(args, *params.WorkflowType)
		argIdx++
	}
	if params.IsActive != nil {
		query += fmt.Sprintf(" AND is_active = $%d", argIdx)
		args = append(args, *params.IsActive)
		argIdx++
	}
	query += ` ORDER BY workflow_type, version DESC`

	if params.Limit > 0 {
		query += fmt.Sprintf(" LIMIT $%d", argIdx)
		args = append(args, params.Limit)
		argIdx++
	}
	if params.Offset > 0 {
		query += fmt.Sprintf(" OFFSET $%d", argIdx)
		args = append(args, params.Offset)
	}

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		s.log.Error("pg ListWorkflowDefinitions failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	var definitions []*domain.WorkflowDefinition
	for rows.Next() {
		def, scanErr := scanDefinition(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		definitions = append(definitions, def)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return definitions, nil
}

// SupersedeWorkflowDefinition marks an old definition as inactive and links to new one.
func (s *PgStore) SupersedeWorkflowDefinition(ctx context.Context, oldDefID, newDefID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const query = `
		UPDATE workflow_definitions
		SET is_active = false, superseded_by = $1, updated_at = NOW(), row_version = row_version + 1
		WHERE workflow_definition_id = $2 AND is_active = true;`

	_, err = tx.Exec(ctx, query, newDefID, oldDefID)
	if err != nil {
		s.log.Error("pg SupersedeWorkflowDefinition failed", zap.Error(err))
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	return tx.Commit(ctx)
}
