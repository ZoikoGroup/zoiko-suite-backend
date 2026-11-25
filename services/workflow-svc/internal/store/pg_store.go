package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/domain"
	svcenvelope "zoiko.io/workflow-svc/internal/envelope"
	svcmiddleware "zoiko.io/workflow-svc/internal/middleware"
)

// Store defines the persistence interface for the workflow service.
// All mutating methods run under the caller's tenant (RLS).
type Store interface {
	// Workflow Instances
	CreateWorkflow(ctx context.Context, params domain.CreateWorkflowParams) (*domain.WorkflowInstance, []*domain.WorkflowStage, bool, error)
	FindWorkflowByID(ctx context.Context, workflowInstanceID string) (*domain.WorkflowInstance, error)
	FindStagesByWorkflowID(ctx context.Context, workflowInstanceID string) ([]*domain.WorkflowStage, error)
	FindCurrentStage(ctx context.Context, workflowInstanceID string) (*domain.WorkflowStage, error)
	IsCurrentStageApprover(ctx context.Context, workflowInstanceID, principalID string) (bool, error)
	SubmitAction(ctx context.Context, params domain.SubmitActionParams) (*domain.WorkflowInstance, *domain.WorkflowStage, bool, error)
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
	ListWorkflowDefinitions(ctx context.Context, params domain.ListWorkflowDefinitionsParams) ([]*domain.WorkflowDefinition, *domain.PaginationCursor, error)
	SupersedeWorkflowDefinition(ctx context.Context, oldDefID, newDefID string) error

	// WFC-01 definition publication lifecycle (WF-S22)
	ValidateDefinition(ctx context.Context, params domain.ValidateDefinitionParams) (*domain.WorkflowDefinition, error)
	SubmitDefinitionApproval(ctx context.Context, params domain.SubmitDefinitionApprovalParams) (*domain.WorkflowDefinition, error)
	ApproveDefinition(ctx context.Context, params domain.ApproveDefinitionParams) (*domain.WorkflowDefinition, error)
	RejectDefinition(ctx context.Context, params domain.RejectDefinitionParams) (*domain.WorkflowDefinition, error)
	PublishDefinition(ctx context.Context, params domain.PublishDefinitionParams) (*domain.WorkflowDefinition, error)
	SuspendDefinition(ctx context.Context, params domain.SuspendDefinitionParams) (*domain.WorkflowDefinition, error)
	RetireDefinition(ctx context.Context, params domain.RetireDefinitionParams) (*domain.WorkflowDefinition, error)

	// Additional workflow definition lifecycle methods (deprecated aliases)
	ActivateWorkflowDefinition(ctx context.Context, workflowDefinitionID string) error
	SuspendWorkflowDefinition(ctx context.Context, workflowDefinitionID string) error
	RetireWorkflowDefinition(ctx context.Context, workflowDefinitionID string) error

	// Additional workflow instance methods
	ListPendingApprovals(ctx context.Context, tenantID string, params domain.ListPendingApprovalsParams) ([]*domain.WorkflowInstance, *domain.PaginationCursor, error)
	ListPendingSubmissions(ctx context.Context, tenantID, formID string, params domain.ListPendingSubmissionsParams) ([]*domain.FormSubmission, *domain.PaginationCursor, error)
	GetApprovalEvidence(ctx context.Context, workflowInstanceID string) ([]*domain.WorkflowTransition, error)
	RequestChanges(ctx context.Context, workflowInstanceID, actorPrincipalID, rationale string, causationID *string, cmd ...domain.LifecycleCommand) error
	ListWorkflows(ctx context.Context, tenantID string, params domain.ListWorkflowsParams) ([]*domain.WorkflowInstance, *domain.PaginationCursor, error)
	GetInstanceSummary(ctx context.Context, workflowInstanceID string) (*domain.InstanceSummary, error)

	// WFC-04: Cases (WF-2)
	CreateCase(ctx context.Context, params domain.CreateCaseParams) (*domain.Case, error)
	FindCaseByID(ctx context.Context, caseID string) (*domain.Case, error)
	ListCases(ctx context.Context, filter domain.ListCasesFilter) ([]*domain.Case, error)
	TransitionCase(ctx context.Context, params domain.TransitionCaseParams) (*domain.Case, bool, error)
	FindCasesByWorkflowInstance(ctx context.Context, workflowInstanceID string) ([]*domain.Case, error)

	// Named case commands (WF-S2)
	TriageCase(ctx context.Context, params domain.TriageCaseParams) (*domain.Case, bool, error)
	StartCase(ctx context.Context, params domain.StartCaseParams) (*domain.Case, bool, error)
	WaitCase(ctx context.Context, params domain.WaitCaseParams) (*domain.Case, bool, error)
	ResumeCase(ctx context.Context, params domain.ResumeCaseParams) (*domain.Case, bool, error)
	ResolveCase(ctx context.Context, params domain.ResolveCaseParams) (*domain.Case, bool, error)
	CloseCase(ctx context.Context, params domain.CloseCaseParams) (*domain.Case, bool, error)
	ReopenCase(ctx context.Context, params domain.ReopenCaseParams) (*domain.Case, bool, error)
	MergeCase(ctx context.Context, params domain.MergeCaseParams) (*domain.Case, error)
	SplitCase(ctx context.Context, params domain.SplitCaseParams) (*domain.Case, error)

	// Case members
	AddCaseMember(ctx context.Context, params domain.AddCaseMemberParams) error
	RemoveCaseMember(ctx context.Context, params domain.RemoveCaseMemberParams) error
	ListCaseMembers(ctx context.Context, caseID string) ([]*domain.CaseMember, error)

	// Case notes
	CreateCaseNote(ctx context.Context, params domain.CreateCaseNoteParams) (*domain.CaseNote, error)
	UpdateCaseNote(ctx context.Context, params domain.UpdateCaseNoteParams) (*domain.CaseNote, error)
	GetCaseNote(ctx context.Context, caseNoteID string) (*domain.CaseNote, error)
	ListCaseNotes(ctx context.Context, caseID string, visibility string) ([]*domain.CaseNote, error)

	// Case links
	CreateCaseLink(ctx context.Context, params domain.CreateCaseLinkParams) (*domain.CaseLink, error)
	ListCaseLinks(ctx context.Context, caseID string) ([]*domain.CaseLink, error)

	// Work items (WF-S2 / WF-S23)
	CreateWorkItem(ctx context.Context, params domain.CreateWorkItemParams) (*domain.WorkItem, error)
	GetWorkItem(ctx context.Context, workItemID string) (*domain.WorkItem, error)
	ListWorkItems(ctx context.Context, tenantID string, filter domain.ListWorkItemsFilter) ([]*domain.WorkItem, *domain.PaginationCursor, error)
	SatisfyWorkItem(ctx context.Context, params domain.SatisfyWorkItemParams) (*domain.WorkItem, bool, error)
	WaiveWorkItem(ctx context.Context, params domain.WaiveWorkItemParams) (*domain.WorkItem, bool, error)
	TransferWorkItem(ctx context.Context, params domain.TransferWorkItemParams) (*domain.WorkItem, bool, error)
	ClaimWorkItem(ctx context.Context, workItemID string, expectedVersion int, assigneePrincipalID string, actorPrincipalID string, correlationID string) (*domain.WorkItem, bool, error)
	ReleaseWorkItem(ctx context.Context, workItemID string, expectedVersion int, actorPrincipalID string, correlationID string) (*domain.WorkItem, bool, error)
	CancelWorkItem(ctx context.Context, workItemID string, expectedVersion int, actorPrincipalID string, correlationID string) (*domain.WorkItem, bool, error)
	EscalateWorkItem(ctx context.Context, workItemID string, expectedVersion int, actorPrincipalID string, correlationID string) (*domain.WorkItem, bool, error)
	ReassignWorkItem(ctx context.Context, workItemID string, expectedVersion int, newAssigneePrincipalID string, actorPrincipalID string, correlationID string) (*domain.WorkItem, bool, error)
	BulkCompleteWorkItems(ctx context.Context, params domain.BulkCompleteWorkItemsParams) ([]*domain.WorkItem, []*domain.WorkItemResult, error)

	// Idempotent case open from obligation (WF-S7)
	FindCaseBySource(ctx context.Context, sourceType, sourceID, milestone string) (*domain.Case, error)
}

// PgStore implements Store against a PostgreSQL cluster via pgxpool.
type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

// Pool returns the underlying connection pool for direct queries.
// Use sparingly — prefer withRLS or withTenantTx.
func (s *PgStore) Pool() *pgxpool.Pool {
	return s.pool
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

// mapPgError turns the Postgres failures that are really caller mistakes into
// domain errors, so they stop arriving at the handler as "the store is
// unavailable".
func mapPgError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22P02" {
		return domain.ErrInvalidIdentifier
	}
	return err
}

// tenantOf reads the caller's tenant from the context.
func tenantOf(ctx context.Context) string {
	if env, ok := svcenvelope.FromContext(ctx); ok && env.TenantID != "" {
		return env.TenantID
	}
	return svcmiddleware.TenantFromContext(ctx)
}

// withTenantTx runs fn inside a transaction with app.tenant_id installed, so
// the row-level security policy has something to compare against.
func (s *PgStore) withTenantTx(ctx context.Context, fn func(tx pgx.Tx, tenantID string) error) error {
	tenantID := tenantOf(ctx)
	if tenantID == "" {
		return domain.ErrTenantMissing
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if err := fn(tx, tenantID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// insertOutboxEventTx inserts an event into the outbox_events table within an existing transaction.
// This is a simplified version for case events; the main workflow events use outbox.Insert.
func insertOutboxEventTx(ctx context.Context, tx pgx.Tx, tenantID, eventType, aggregateType, aggregateID, correlationID, legalEntityID, jurisdictionID, actorID string, payload map[string]any) error {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal outbox payload: %w", err)
	}

	const query = `
		INSERT INTO outbox_events (outbox_event_id, aggregate_type, aggregate_id, event_type,
			tenant_id, legal_entity_id, actor_id, correlation_id, headers, payload, destination, created_at)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, '{}'::jsonb, $8, 'kafka', NOW());`

	_, err = tx.Exec(ctx, query, aggregateType, aggregateID, eventType, tenantID, legalEntityID, actorID, correlationID, payloadBytes)
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

// ── workflow_instances ───────────────────────────────────────────────────────

const instanceColumns = `workflow_instance_id, tenant_id, legal_entity_id, workflow_type, workflow_status, current_stage, initiated_by, correlation_id, started_at, completed_at, subject_type, subject_id, subject_version, subject_fingerprint, invalidated_at, invalidation_reason_code, invalidation_narrative, invalidation_evidence_refs, row_version, updated_at, created_by, plane, data_class, residency_region, idempotency_key, due_at, next_action_at, escalation_target, workflow_definition_id, workflow_definition_version, review_cycle, escalated_at, causation_decision_id`

func scanInstance(row pgx.Row) (*domain.WorkflowInstance, error) {
	w := &domain.WorkflowInstance{}
	// idempotency_key (000013) and the definition reference (000016) are
	// nullable: a workflow created without an Idempotency-Key or a definition
	// has none, and scanning NULL into a plain string or int fails.
	//
	// The targets follow instanceColumns one for one. 7a59de35 (WF-S1..S7)
	// listed definitionID right after idempotency_key and dropped the
	// definition-version holder, so every scan had 33 targets for 32 columns
	// and failed (found 25 Nov 2026, DL-2).
	var idempotencyKey, definitionID, escalationTarget, causationDecisionID *string
	var definitionVersion *int
	err := row.Scan(
		&w.WorkflowInstanceID,
		&w.TenantID,
		&w.LegalEntityID,
		&w.WorkflowType,
		&w.WorkflowStatus,
		&w.CurrentStage,
		&w.InitiatedBy,
		&w.CorrelationID,
		&w.StartedAt,
		&w.CompletedAt,
		&w.SubjectType,
		&w.SubjectID,
		&w.SubjectVersion,
		&w.SubjectFingerprint,
		&w.InvalidatedAt,
		&w.InvalidationReasonCode,
		&w.InvalidationNarrative,
		&w.InvalidationEvidenceRefs,
		&w.RowVersion,
		&w.UpdatedAt,
		&w.CreatedBy,
		&w.Plane,
		&w.DataClass,
		&w.ResidencyRegion,
		&idempotencyKey,
		&w.DueAt,
		&w.NextActionAt,
		&escalationTarget,
		&definitionID,
		&definitionVersion,
		&w.ReviewCycle,
		&w.EscalatedAt,
		&causationDecisionID,
	)
	if definitionVersion != nil {
		w.WorkflowDefinitionVersion = *definitionVersion
	}
	w.CausationDecisionID = causationDecisionID
	if idempotencyKey != nil {
		w.IdempotencyKey = *idempotencyKey
	}
	if definitionID != nil {
		w.WorkflowDefinitionID = *definitionID
	}
	if escalationTarget != nil {
		w.EscalationTarget = escalationTarget
	}
	return w, err
}

// ── workflow_definitions ───────────────────────────────────────────────────────

const definitionColumns = `workflow_definition_id, tenant_id, workflow_type, version, stages_json, name,
	description, created_by, created_at, superseded_by, is_active, row_version, updated_at, plane, data_class,
	residency_region, expiry_hours, escalation_target, status, effective_from, effective_to,
	validated_by, validated_at, approved_by, approved_at, published_by, published_at, checksum`

func scanDefinition(row pgx.Row) (*domain.WorkflowDefinition, error) {
	d := &domain.WorkflowDefinition{}
	var stagesJSON []byte
	var expiryHours, escalationTarget *string
	var status *string
	var effectiveFrom, effectiveTo *time.Time
	var validatedBy, approvedBy, publishedBy *string
	var validatedAt, approvedAt, publishedAt *time.Time
	var checksum *string
	err := row.Scan(
		&d.WorkflowDefinitionID, &d.TenantID, &d.WorkflowType, &d.Version, &stagesJSON,
		&d.Name, &d.Description, &d.CreatedBy, &d.CreatedAt, &d.SupersededBy, &d.IsActive,
		&d.RowVersion, &d.UpdatedAt, &d.Plane, &d.DataClass, &d.ResidencyRegion,
		&expiryHours, &escalationTarget, &status, &effectiveFrom, &effectiveTo,
		&validatedBy, &validatedAt, &approvedBy, &approvedAt, &publishedBy, &publishedAt,
		&checksum,
	)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(stagesJSON, &d.Stages)
	if expiryHours != nil {
		if h, err := strconv.Atoi(*expiryHours); err == nil {
			d.ExpiryHours = &h
		}
	}
	if escalationTarget != nil {
		d.EscalationTarget = escalationTarget
	}
	if status != nil {
		d.Status = status
	}
	if effectiveFrom != nil {
		d.EffectiveFrom = effectiveFrom
	}
	if effectiveTo != nil {
		d.EffectiveTo = effectiveTo
	}
	if validatedBy != nil {
		d.ValidatedBy = validatedBy
	}
	if validatedAt != nil {
		d.ValidatedAt = validatedAt
	}
	if approvedBy != nil {
		d.ApprovedBy = approvedBy
	}
	if approvedAt != nil {
		d.ApprovedAt = approvedAt
	}
	if publishedBy != nil {
		d.PublishedBy = publishedBy
	}
	if publishedAt != nil {
		d.PublishedAt = publishedAt
	}
	if checksum != nil {
		d.Checksum = checksum
	}
	return d, nil
}

// ── workflow_stages ───────────────────────────────────────────────────────────

const stageColumns = `workflow_stage_id, workflow_instance_id, stage_order, stage_type, approver_principal_id,
	required_approvals, stage_status, acted_at, rationale, row_version, updated_at, created_by, plane, data_class,
	residency_region, approver_role, due_at, acted_by, quorum_approvers`

func scanStage(row pgx.Row) (*domain.WorkflowStage, error) {
	st := &domain.WorkflowStage{}
	var quorumApprovers []byte
	err := row.Scan(
		&st.WorkflowStageID,
		&st.WorkflowInstanceID,
		&st.StageOrder,
		&st.StageType,
		&st.ApproverPrincipalID,
		&st.ApproverRole,
		&st.RequiredApprovals,
		&st.StageStatus,
		&st.ActedAt,
		&st.Rationale,
		&st.RowVersion,
		&st.UpdatedAt,
		&st.CreatedBy,
		&st.Plane,
		&st.DataClass,
		&st.ResidencyRegion,
		&st.DueAt,
		&st.ActedBy,
		&quorumApprovers,
	)
	if quorumApprovers != nil {
		_ = json.Unmarshal(quorumApprovers, &st.QuorumApprovers)
	}
	return st, err
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
	const query = `
		SELECT stage_order FROM workflow_transitions
		WHERE workflow_instance_id = $1 AND idempotency_key = $2;`
	var stageOrder int
	err := tx.QueryRow(ctx, query, instanceID, meta.IdempotencyKey).Scan(&stageOrder)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	// If the request hash matches, it's a replay; otherwise it's a conflict.
	const hashQuery = `SELECT request_hash FROM workflow_transitions WHERE workflow_instance_id = $1 AND idempotency_key = $2;`
	var storedHash string
	err = tx.QueryRow(ctx, hashQuery, instanceID, meta.IdempotencyKey).Scan(&storedHash)
	if err != nil {
		return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if storedHash != meta.RequestHash {
		return false, domain.ErrIdempotencyMismatch
	}
	return true, nil
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
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM workflow_stage_quorum_decisions WHERE workflow_stage_id = $1 AND decision = 'APPROVE'`, workflowStageID).Scan(&tr.approvals); err != nil {
		return tr, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM workflow_stage_quorum_decisions WHERE workflow_stage_id = $1 AND decision = 'REJECT'`, workflowStageID).Scan(&tr.rejections); err != nil {
		return tr, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	tr.votesCast = tr.approvals + tr.rejections
	return tr, nil
}

// findStageByApprover reads the first stage of an instance where the principal is the approver.
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
			return nil, domain.ErrWorkflowNotFound
		}
		if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
			return nil, domain.ErrInvalidIdentifier
		}
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
		if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
			return nil, domain.ErrInvalidIdentifier
		}
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return st, nil
}

// CreateWorkflow creates a new workflow instance with its stages.
func (s *PgStore) CreateWorkflow(ctx context.Context, params domain.CreateWorkflowParams) (*domain.WorkflowInstance, []*domain.WorkflowStage, bool, error) {
	var w *domain.WorkflowInstance
	var stages []*domain.WorkflowStage

	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		if params.TenantID != tenantID {
			return domain.ErrTenantMissing
		}

		// Resolve stages from definition or use inline stages
		if params.WorkflowDefinitionID != "" {
			def, err := s.getWorkflowDefinitionTx(ctx, tx, params.WorkflowDefinitionID, tenantID)
			if err != nil {
				return err
			}
			if def == nil {
				return domain.ErrWorkflowNotFound
			}
			if def.Status == nil || (*def.Status != "PUBLISHED" && *def.Status != "APPROVAL_PENDING" && *def.Status != "VALIDATED") {
				return fmt.Errorf("%w: definition not in startable state", domain.ErrInvalidTransition)
			}
			if def.EffectiveFrom != nil && time.Now().UTC().Before(*def.EffectiveFrom) {
				return fmt.Errorf("%w: definition not yet effective", domain.ErrInvalidTransition)
			}
			if def.EffectiveTo != nil && time.Now().UTC().After(*def.EffectiveTo) {
				return fmt.Errorf("%w: definition expired", domain.ErrInvalidTransition)
			}
			params.WorkflowDefinitionVersion = def.Version
			params.Stages = def.Stages
			if params.ExpiryHours == nil && def.ExpiryHours != nil {
				params.ExpiryHours = def.ExpiryHours
			}
			if params.EscalationTarget == nil && def.EscalationTarget != nil {
				params.EscalationTarget = def.EscalationTarget
			}
		}

		if len(params.Stages) == 0 {
			return domain.ErrNoStages
		}

		// Check initiator is not an approver
		for _, st := range params.Stages {
			if st.ApproverPrincipalID != "" && st.ApproverPrincipalID == params.InitiatedBy {
				return domain.ErrInitiatorCannotBeApprover
			}
			if len(st.QuorumApprovers) > 0 {
				for _, a := range st.QuorumApprovers {
					if a == params.InitiatedBy {
						return domain.ErrInitiatorCannotBeApprover
					}
				}
			}
		}

		var dueAt *time.Time
		if params.ExpiryHours != nil && *params.ExpiryHours > 0 {
			d := time.Now().UTC().Add(time.Duration(*params.ExpiryHours) * time.Hour)
			dueAt = &d
		}

		const insertInstance = `
			INSERT INTO workflow_instances (
				workflow_instance_id, tenant_id, legal_entity_id, workflow_type, workflow_status,
				current_stage, initiated_by, correlation_id, started_at, subject_type,
				subject_id, subject_version, subject_fingerprint, row_version,
				created_by, plane, data_class, residency_region, idempotency_key,
				due_at, next_action_at, escalation_target,
				workflow_definition_id, workflow_definition_version, review_cycle, causation_decision_id
			) VALUES (gen_random_uuid(), $1, $2, $3, 'PENDING', 1, $4, $5, NOW(), $6, $7, $8, $9, 1, $10, $11, $12, $13, $14, $15, NOW(), $16, $17, $18, 1, NULLIF($19, ''))
			RETURNING ` + instanceColumns + `;`

		var scanErr error
		w, scanErr = scanInstance(tx.QueryRow(ctx, insertInstance,
			tenantID, params.LegalEntityID, params.WorkflowType, params.InitiatedBy,
			params.CorrelationID, params.SubjectType, params.SubjectID, params.SubjectVersion,
			params.SubjectFingerprint, params.CreatedBy, params.Plane, params.DataClass,
			params.ResidencyRegion, params.IdempotencyKey, dueAt,
			params.EscalationTarget, params.WorkflowDefinitionID, params.WorkflowDefinitionVersion,
			params.CausationDecisionID))
		if scanErr != nil {
			if errors.Is(mapPgError(scanErr), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}

		// Insert stages
		for i, st := range params.Stages {
			stageOrder := i + 1
			var approver *string
			if st.ApproverPrincipalID != "" {
				approver = &st.ApproverPrincipalID
			}
			var role *string
			if st.ApproverRole != "" {
				role = &st.ApproverRole
			}
			var dueAtStage *time.Time
			if st.DueAtHours > 0 {
				d := time.Now().UTC().Add(time.Duration(st.DueAtHours) * time.Hour)
				dueAtStage = &d
			}

			const insertStage = `
				INSERT INTO workflow_stages (
					workflow_stage_id, workflow_instance_id, stage_order, stage_type,
					approver_principal_id, approver_role, required_approvals,
					stage_status, due_at, quorum_approvers
				) VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, $8, $9)
				RETURNING workflow_stage_id, workflow_instance_id, stage_order, stage_type,
					approver_principal_id, approver_role, required_approvals, stage_status, due_at, acted_at, acted_by, rationale;`
			stage, sErr := scanStage(tx.QueryRow(ctx, insertStage,
				w.WorkflowInstanceID, stageOrder, st.StageType, approver, role,
				st.RequiredApprovals, "PENDING", dueAtStage, st.QuorumApprovers))
			if sErr != nil {
				if errors.Is(mapPgError(sErr), domain.ErrInvalidIdentifier) {
					return domain.ErrInvalidIdentifier
				}
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, sErr)
			}
			stages = append(stages, stage)

			// Insert quorum approvers
			if len(st.QuorumApprovers) > 0 {
				for _, a := range st.QuorumApprovers {
					const insertQA = `
						INSERT INTO workflow_stage_quorum_approvers (workflow_stage_quorum_approver_id, workflow_stage_id, approver_principal_id)
						VALUES (gen_random_uuid(), $1, $2);`
					_, err := tx.Exec(ctx, insertQA, stage.WorkflowStageID, a)
					if err != nil {
						if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
							return domain.ErrInvalidIdentifier
						}
						return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
					}
				}
			}
		}

		// Set next_action_at to first stage's due_at if available
		if len(stages) > 0 && stages[0].DueAt != nil {
			const updateNext = `UPDATE workflow_instances SET next_action_at = $1 WHERE workflow_instance_id = $2;`
			_, err := tx.Exec(ctx, updateNext, stages[0].DueAt, w.WorkflowInstanceID)
			if err != nil {
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
			}
			w.NextActionAt = stages[0].DueAt
		}

		// Write workflow.started event
		payload := map[string]any{
			"workflow_instance_id": w.WorkflowInstanceID,
			"workflow_type":        w.WorkflowType,
			"initiated_by":         w.InitiatedBy,
			"subject_type":         w.SubjectType,
			"subject_id":           w.SubjectID,
			"subject_version":      w.SubjectVersion,
			"subject_fingerprint":  w.SubjectFingerprint,
		}
		return insertOutboxEventTx(ctx, tx, tenantID, "workflow.started", "workflow_instance", w.WorkflowInstanceID, params.CorrelationID, params.LegalEntityID, "", params.InitiatedBy, payload)
	})
	if err != nil {
		return nil, nil, false, err
	}
	return w, stages, true, nil
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
	_, err := tx.Exec(ctx, query, r.InstanceID, r.From, r.To, r.ActedBy, r.Rationale, r.CorrelationID, r.CausationID,
		r.StageOrder, r.ReviewCycle, r.DefinitionVersion, r.IdempotencyKey, r.RequestHash,
		r.Authority.AccessDecisionID, r.Authority.Basis, r.Authority.DelegationID, r.DecisionID)
	if err != nil {
		if isUniqueViolation(err, "uq_workflow_transitions_idempotency") {
			return errIdempotentRetry
		}
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

func insertTransition(ctx context.Context, tx pgx.Tx, workflowInstanceID, fromState, toState, actedBy string, rationale, correlationID, causationID *string) error {
	return insertTransitionRecord(ctx, tx, transitionRecord{InstanceID: workflowInstanceID, From: fromState, To: toState, ActedBy: actedBy, Rationale: rationale, CorrelationID: correlationID, CausationID: causationID})
}

func isFinalStage(ctx context.Context, tx pgx.Tx, workflowInstanceID string, stageOrder int) (bool, error) {
	const query = `SELECT MAX(stage_order) FROM workflow_stages WHERE workflow_instance_id = $1;`
	var maxOrder int
	if err := tx.QueryRow(ctx, query, workflowInstanceID).Scan(&maxOrder); err != nil {
		return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return stageOrder == maxOrder, nil
}

func (s *PgStore) getWorkflowDefinitionTx(ctx context.Context, tx pgx.Tx, definitionID string, tenantID string) (*domain.WorkflowDefinition, error) {
	const query = `
		SELECT workflow_definition_id, tenant_id, workflow_type, version, stages_json,
			name, description, created_by, created_at, superseded_by, is_active,
			row_version, updated_at, plane, data_class, residency_region,
			expiry_hours, escalation_target, status, effective_from, effective_to,
			validated_by, validated_at, approved_by, approved_at, published_by, published_at,
			checksum
		FROM workflow_definitions
		WHERE workflow_definition_id = $1 AND tenant_id = $2;`
	var def domain.WorkflowDefinition
	var stagesJSON []byte
	var expiryHours, escalationTarget *string
	var status *string
	var effectiveFrom, effectiveTo *time.Time
	var validatedBy, approvedBy, publishedBy *string
	var validatedAt, approvedAt, publishedAt *time.Time
	var checksum *string
	err := tx.QueryRow(ctx, query, definitionID, tenantID).Scan(
		&def.WorkflowDefinitionID, &def.TenantID, &def.WorkflowType, &def.Version, &stagesJSON,
		&def.Name, &def.Description, &def.CreatedBy, &def.CreatedAt, &def.SupersededBy, &def.IsActive,
		&def.RowVersion, &def.UpdatedAt, &def.Plane, &def.DataClass, &def.ResidencyRegion,
		&expiryHours, &escalationTarget, &status, &effectiveFrom, &effectiveTo,
		&validatedBy, &validatedAt, &approvedBy, &approvedAt, &publishedBy, &publishedAt,
		&checksum,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	_ = json.Unmarshal(stagesJSON, &def.Stages)
	if expiryHours != nil {
		if h, err := strconv.Atoi(*expiryHours); err == nil {
			def.ExpiryHours = &h
		}
	}
	if escalationTarget != nil {
		def.EscalationTarget = escalationTarget
	}
	if status != nil {
		def.Status = status
	}
	if effectiveFrom != nil {
		def.EffectiveFrom = effectiveFrom
	}
	if effectiveTo != nil {
		def.EffectiveTo = effectiveTo
	}
	if validatedBy != nil {
		def.ValidatedBy = validatedBy
	}
	if validatedAt != nil {
		def.ValidatedAt = validatedAt
	}
	if approvedBy != nil {
		def.ApprovedBy = approvedBy
	}
	if approvedAt != nil {
		def.ApprovedAt = approvedAt
	}
	if publishedBy != nil {
		def.PublishedBy = publishedBy
	}
	if publishedAt != nil {
		def.PublishedAt = publishedAt
	}
	if checksum != nil {
		def.Checksum = checksum
	}
	return &def, nil
}

// FindWorkflowByID returns the workflow instance with the given ID.
func (s *PgStore) FindWorkflowByID(ctx context.Context, workflowInstanceID string) (*domain.WorkflowInstance, error) {
	const query = `SELECT ` + instanceColumns + ` FROM workflow_instances WHERE workflow_instance_id = $1 AND tenant_id = $2;`

	var w *domain.WorkflowInstance
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		var scanErr error
		w, scanErr = scanInstance(tx.QueryRow(ctx, query, workflowInstanceID, tenantID))
		return scanErr
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrWorkflowNotFound
		}
		if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
			return nil, domain.ErrWorkflowNotFound
		}
		s.log.Error("pg FindWorkflowByID failed", zap.String("workflow_instance_id", workflowInstanceID), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return w, nil
}

// FindStagesByWorkflowID returns all stages for a workflow instance.
func (s *PgStore) FindStagesByWorkflowID(ctx context.Context, workflowInstanceID string) ([]*domain.WorkflowStage, error) {
	const query = `
		SELECT workflow_stage_id, workflow_instance_id, stage_order, stage_type,
			approver_principal_id, approver_role, required_approvals, stage_status, due_at, acted_at, acted_by, rationale
		FROM workflow_stages
		WHERE workflow_instance_id = $1 AND tenant_id = $2
		ORDER BY stage_order;`

	var results []*domain.WorkflowStage
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		rows, qErr := tx.Query(ctx, query, workflowInstanceID, tenantID)
		if qErr != nil {
			if errors.Is(mapPgError(qErr), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			s.log.Error("pg FindStagesByWorkflowID failed", zap.Error(qErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, qErr)
		}
		defer rows.Close()

		for rows.Next() {
			stage, scanErr := scanStage(rows)
			if scanErr != nil {
				s.log.Error("pg FindStagesByWorkflowID scan failed", zap.Error(scanErr))
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
			}
			results = append(results, stage)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// FindCurrentStage returns the current stage for a workflow instance.
func (s *PgStore) FindCurrentStage(ctx context.Context, workflowInstanceID string) (*domain.WorkflowStage, error) {
	const query = `
		SELECT workflow_stage_id, workflow_instance_id, stage_order, stage_type,
			approver_principal_id, approver_role, required_approvals, stage_status, due_at, acted_at, acted_by, rationale
		FROM workflow_stages
		WHERE workflow_instance_id = $1 AND tenant_id = $2 AND stage_order = (
			SELECT current_stage FROM workflow_instances WHERE workflow_instance_id = $1 AND tenant_id = $2
		);`

	var stage *domain.WorkflowStage
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		var scanErr error
		stage, scanErr = scanStage(tx.QueryRow(ctx, query, workflowInstanceID, tenantID))
		return scanErr
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrWorkflowNotFound
		}
		if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
			return nil, domain.ErrInvalidIdentifier
		}
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return stage, nil
}

// IsCurrentStageApprover checks if a principal is the approver for the current stage.
func (s *PgStore) IsCurrentStageApprover(ctx context.Context, workflowInstanceID, principalID string) (bool, error) {
	const query = `
		SELECT 1 FROM workflow_stages ws
		JOIN workflow_instances wi ON wi.workflow_instance_id = ws.workflow_instance_id AND wi.tenant_id = ws.tenant_id
		WHERE wi.workflow_instance_id = $1 AND wi.tenant_id = $2
		AND ws.stage_order = wi.current_stage
		AND (
			(ws.stage_type = 'SINGLE' AND ws.approver_principal_id = $3)
			OR (ws.stage_type = 'QUORUM' AND $3 = ANY(ws.quorum_approvers))
			OR (ws.stage_type = 'ROLE' AND ws.approver_role IS NOT NULL AND EXISTS (
				SELECT 1 FROM authorization_svc_role_holders rh
				WHERE rh.principal_id = $3 AND rh.role = ws.approver_role
				AND rh.tenant_id = wi.tenant_id
			))
		);`

	var exists bool
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		err := tx.QueryRow(ctx, query, workflowInstanceID, tenantID, principalID).Scan(&exists)
		if errors.Is(err, pgx.ErrNoRows) {
			exists = false
			return nil
		}
		return err
	})
	if err != nil {
		if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
			return false, domain.ErrInvalidIdentifier
		}
		s.log.Error("pg IsCurrentStageApprover failed", zap.Error(err))
		return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return exists, nil
}

// IsQuorumApprover checks if a principal is in the quorum pool for a specific stage.
func (s *PgStore) IsQuorumApprover(ctx context.Context, workflowStageID, principalID string) (bool, error) {
	const query = `
		SELECT EXISTS(SELECT 1 FROM workflow_stage_quorum_approvers WHERE workflow_stage_id = $1 AND approver_principal_id = $2);`
	var eligible bool
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		return tx.QueryRow(ctx, query, workflowStageID, principalID).Scan(&eligible)
	})
	if err != nil {
		if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
			return false, domain.ErrInvalidIdentifier
		}
		s.log.Error("pg IsQuorumApprover failed", zap.Error(err))
		return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return eligible, nil
}

// =============================================================================
// Workflow Definitions (WFC-02) — Versioned, immutable definitions
// =============================================================================

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

	var stagesJSON []byte
	if params.StagesJSON != nil {
		stagesJSON = params.StagesJSON
	} else {
		stagesJSON = []byte("[]")
	}

	const query = `
		INSERT INTO workflow_definitions (workflow_definition_id, tenant_id, workflow_type, version, stages_json, name,
			description, created_by, created_at, is_active, expiry_hours, escalation_target, status, checksum)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, NOW(), true, $8, $9, 'DRAFT', $10)
		RETURNING ` + definitionColumns + `;`

	checksum := computeChecksum(params.WorkflowType, params.Version, stagesJSON)

	row := tx.QueryRow(ctx, query, params.TenantID, params.WorkflowType, params.Version, stagesJSON,
		params.Name, params.Description, params.CreatedBy, params.ExpiryHours, params.EscalationTarget, checksum)
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
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
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

// InvalidateWorkflow transitions PENDING or APPROVED -> INVALIDATED per ZS-STATE-001 §6.1 / §7.
func (s *PgStore) InvalidateWorkflow(ctx context.Context, params domain.InvalidateWorkflowParams) (*domain.WorkflowInstance, bool, error) {
	current, err := s.FindWorkflowByID(ctx, params.WorkflowInstanceID)
	if err != nil {
		return nil, false, err
	}
	if current.WorkflowStatus == domain.WorkflowStatusInvalidated {
		return current, false, nil
	}
	if current.WorkflowStatus != domain.WorkflowStatusPending && current.WorkflowStatus != domain.WorkflowStatusApproved {
		return nil, false, domain.ErrInvalidTransition
	}

	rationale := params.ReasonCode
	if params.Narrative != nil && *params.Narrative != "" {
		rationale += " narrative=" + *params.Narrative
	}

	return s.transitionInstanceStatus(ctx, current, domain.WorkflowStatusInvalidated, params.ActorPrincipalID, current.CurrentStage, domain.LifecycleCommand{
		Rationale:  &rationale,
		ReasonCode: params.ReasonCode,
		Authority:  params.Authority,
		Meta:       params.Meta,
	})
}

// VerifyRelease evaluates whether an approved workflow remains valid to release against the current subject version and fingerprint.
func (s *PgStore) VerifyRelease(ctx context.Context, params domain.VerifyReleaseParams) (*domain.ReleaseVerificationResult, error) {
	current, err := s.FindWorkflowByID(ctx, params.WorkflowInstanceID)
	if err != nil {
		return nil, err
	}

	if current.WorkflowStatus != domain.WorkflowStatusApproved && current.WorkflowStatus != domain.WorkflowStatusInvalidated {
		reason := "workflow not in approved or invalidated state"
		return &domain.ReleaseVerificationResult{
			WorkflowInstanceID: current.WorkflowInstanceID,
			CanRelease:         false,
			Status:             "INVALID",
			Reason:             &reason,
			WorkflowStatus:     current.WorkflowStatus,
			SubjectFingerprint: current.SubjectFingerprint,
		}, nil
	}

	if current.SubjectFingerprint == nil || *current.SubjectFingerprint == "" {
		reason := "workflow has no bound subject fingerprint"
		return &domain.ReleaseVerificationResult{
			WorkflowInstanceID: current.WorkflowInstanceID,
			CanRelease:         false,
			Status:             "INVALID",
			Reason:             &reason,
			WorkflowStatus:     current.WorkflowStatus,
			SubjectFingerprint: current.SubjectFingerprint,
		}, nil
	}

	if params.ExpectedSubjectVersion != nil && current.SubjectVersion != nil && *params.ExpectedSubjectVersion != *current.SubjectVersion {
		reason := "subject version mismatch"
		return &domain.ReleaseVerificationResult{
			WorkflowInstanceID: current.WorkflowInstanceID,
			CanRelease:         false,
			Status:             "INVALID",
			Reason:             &reason,
			WorkflowStatus:     current.WorkflowStatus,
			SubjectFingerprint: current.SubjectFingerprint,
		}, nil
	}

	if params.CurrentSubjectFingerprint != "" && current.SubjectFingerprint != nil && params.CurrentSubjectFingerprint != *current.SubjectFingerprint {
		reason := "subject fingerprint mismatch"
		return &domain.ReleaseVerificationResult{
			WorkflowInstanceID: current.WorkflowInstanceID,
			CanRelease:         false,
			Status:             "INVALID",
			Reason:             &reason,
			WorkflowStatus:     current.WorkflowStatus,
			SubjectFingerprint: current.SubjectFingerprint,
		}, nil
	}

	return &domain.ReleaseVerificationResult{
		WorkflowInstanceID: current.WorkflowInstanceID,
		CanRelease:         true,
		Status:             "VALID",
		Reason:             nil,
		WorkflowStatus:     current.WorkflowStatus,
		SubjectFingerprint: current.SubjectFingerprint,
	}, nil
}

// computeChecksum computes a SHA256 checksum of the workflow definition for integrity verification.
func computeChecksum(workflowType string, version int, stagesJSON []byte) string {
	h := sha256.New()
	h.Write([]byte(workflowType))
	h.Write([]byte(strconv.Itoa(version)))
	h.Write(stagesJSON)
	return hex.EncodeToString(h.Sum(nil))
}

// ListPendingApprovals lists workflows pending approval for a tenant using cursor paging (API-4).
func (s *PgStore) ListPendingApprovals(ctx context.Context, tenantID string, params domain.ListPendingApprovalsParams) ([]*domain.WorkflowInstance, *domain.PaginationCursor, error) {
	limit := params.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	query := `
		SELECT ` + instanceColumns + `
		FROM workflow_instances
		WHERE tenant_id = $1 AND workflow_status IN ('PENDING', 'ESCALATED')
		ORDER BY started_at DESC, workflow_instance_id DESC`
	args := []any{tenantID}

	if params.Cursor != nil && params.Cursor.StartedAt != "" && params.Cursor.InstanceID != "" {
		args = append(args, params.Cursor.StartedAt, params.Cursor.InstanceID)
		query += " AND (started_at, workflow_instance_id) < ($2, $3)"
	}
	args = append(args, limit+1)
	query += fmt.Sprintf(" LIMIT $%d;", len(args))

	var results []*domain.WorkflowInstance
	var err error
	err = s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
		rows, qErr := tx.Query(ctx, query, args...)
		if qErr != nil {
			if errors.Is(mapPgError(qErr), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			s.log.Error("pg ListPendingApprovals failed", zap.Error(qErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, qErr)
		}
		defer rows.Close()

		for rows.Next() {
			w, scanErr := scanInstance(rows)
			if scanErr != nil {
				s.log.Error("pg ListPendingApprovals scan failed", zap.Error(scanErr))
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
			}
			results = append(results, w)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, nil, err
	}

	var nextCursor *domain.PaginationCursor
	if len(results) > limit {
		last := results[limit-1]
		results = results[:limit]
		nextCursor = &domain.PaginationCursor{
			StartedAt:  last.StartedAt.UTC().Format(time.RFC3339),
			InstanceID: last.WorkflowInstanceID,
		}
	}

	return results, nextCursor, nil
}

// ListWorkflows lists workflow instances with filters and cursor paging (API-4).
func (s *PgStore) ListWorkflows(ctx context.Context, tenantID string, params domain.ListWorkflowsParams) ([]*domain.WorkflowInstance, *domain.PaginationCursor, error) {
	limit := params.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	query := `SELECT ` + instanceColumns + ` FROM workflow_instances WHERE tenant_id = $1`
	args := []any{tenantID}

	addFilter := func(clause string, value any) {
		args = append(args, value)
		query += fmt.Sprintf(" AND %s $%d", clause, len(args))
	}

	if params.LegalEntityID != "" {
		addFilter("legal_entity_id =", params.LegalEntityID)
	}
	if params.WorkflowType != "" {
		addFilter("workflow_type =", params.WorkflowType)
	}
	if params.WorkflowStatus != "" {
		addFilter("workflow_status =", params.WorkflowStatus)
	}
	if params.SubjectType != "" {
		addFilter("subject_type =", params.SubjectType)
	}
	if params.SubjectID != "" {
		addFilter("subject_id =", params.SubjectID)
	}
	if params.WorkflowDefinitionID != "" {
		addFilter("workflow_definition_id =", params.WorkflowDefinitionID)
	}

	query += " ORDER BY started_at DESC, workflow_instance_id DESC"

	if params.Cursor != nil && params.Cursor.StartedAt != "" && params.Cursor.InstanceID != "" {
		args = append(args, params.Cursor.StartedAt, params.Cursor.InstanceID)
		query += " AND (started_at, workflow_instance_id) < ($" + fmt.Sprint(len(args)-1) + ", $" + fmt.Sprint(len(args)) + ")"
	}
	args = append(args, limit+1)
	query += fmt.Sprintf(" LIMIT $%d;", len(args))

	var results []*domain.WorkflowInstance
	var err error
	err = s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
		rows, qErr := tx.Query(ctx, query, args...)
		if qErr != nil {
			if errors.Is(mapPgError(qErr), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			s.log.Error("pg ListWorkflows failed", zap.Error(qErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, qErr)
		}
		defer rows.Close()

		for rows.Next() {
			w, scanErr := scanInstance(rows)
			if scanErr != nil {
				s.log.Error("pg ListWorkflows scan failed", zap.Error(scanErr))
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
			}
			results = append(results, w)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, nil, err
	}

	var nextCursor *domain.PaginationCursor
	if len(results) > limit {
		last := results[limit-1]
		results = results[:limit]
		nextCursor = &domain.PaginationCursor{
			StartedAt:  last.StartedAt.UTC().Format(time.RFC3339),
			InstanceID: last.WorkflowInstanceID,
		}
	}

	return results, nextCursor, nil
}

// GetInstanceSummary returns a summary view of a workflow instance including its stages.
func (s *PgStore) GetInstanceSummary(ctx context.Context, workflowInstanceID string) (*domain.InstanceSummary, error) {
	instance, err := s.FindWorkflowByID(ctx, workflowInstanceID)
	if err != nil {
		return nil, err
	}

	stages, err := s.FindStagesByWorkflowID(ctx, workflowInstanceID)
	if err != nil {
		return nil, err
	}

	stageSummaries := make([]domain.StageSummary, len(stages))
	for i, st := range stages {
		stageSummaries[i] = domain.StageSummary{
			StageOrder:          st.StageOrder,
			StageType:           st.StageType,
			StageStatus:         st.StageStatus,
			ApproverPrincipalID: st.ApproverPrincipalID,
			RequiredApprovals:   st.RequiredApprovals,
			QuorumApprovers:     st.QuorumApprovers,
			ActedAt:             st.ActedAt,
			ActedBy:             st.ActedBy,
			Rationale:           st.Rationale,
		}
	}

	summary := &domain.InstanceSummary{
		WorkflowInstanceID: instance.WorkflowInstanceID,
		WorkflowType:       instance.WorkflowType,
		WorkflowStatus:     instance.WorkflowStatus,
		CurrentStage:       instance.CurrentStage,
		InitiatedBy:        instance.InitiatedBy,
		SubjectType:        instance.SubjectType,
		SubjectID:          instance.SubjectID,
		SubjectVersion:     instance.SubjectVersion,
		StartedAt:          instance.StartedAt,
		CompletedAt:        instance.CompletedAt,
		DueAt:              instance.DueAt,
		NextActionAt:       instance.NextActionAt,
		EscalationTarget:   instance.EscalationTarget,
		ReviewCycle:        instance.ReviewCycle,
		StagesSummary:      stageSummaries,
	}

	return summary, nil
}

// =============================================================================
// WFC-01: Definition Publication Lifecycle (WF-S22)
// =============================================================================

func (s *PgStore) ValidateDefinition(ctx context.Context, params domain.ValidateDefinitionParams) (*domain.WorkflowDefinition, error) {
	return s.transitionDefinitionStatus(ctx, params.WorkflowDefinitionID, "VALIDATION_PENDING", params.ActorPrincipalID, params.CorrelationID, nil, nil, "", nil, nil, params.Meta)
}

func (s *PgStore) SubmitDefinitionApproval(ctx context.Context, params domain.SubmitDefinitionApprovalParams) (*domain.WorkflowDefinition, error) {
	return s.transitionDefinitionStatus(ctx, params.WorkflowDefinitionID, "APPROVAL_PENDING", params.ActorPrincipalID, params.CorrelationID, nil, nil, "", nil, nil, params.Meta)
}

func (s *PgStore) ApproveDefinition(ctx context.Context, params domain.ApproveDefinitionParams) (*domain.WorkflowDefinition, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	return s.transitionDefinitionStatus(ctx, params.WorkflowDefinitionID, "VALIDATED", params.ActorPrincipalID, params.CorrelationID, &params.ActorPrincipalID, &now, "", nil, nil, params.Meta)
}

func (s *PgStore) RejectDefinition(ctx context.Context, params domain.RejectDefinitionParams) (*domain.WorkflowDefinition, error) {
	return s.transitionDefinitionStatus(ctx, params.WorkflowDefinitionID, "REJECTED", params.ActorPrincipalID, params.CorrelationID, nil, nil, "", nil, nil, params.Meta)
}

func (s *PgStore) PublishDefinition(ctx context.Context, params domain.PublishDefinitionParams) (*domain.WorkflowDefinition, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	return s.transitionDefinitionStatus(ctx, params.WorkflowDefinitionID, "PUBLISHED", params.ActorPrincipalID, params.CorrelationID, nil, &now, params.ActorPrincipalID, params.EffectiveFrom, params.EffectiveTo, params.Meta)
}

func (s *PgStore) SuspendDefinition(ctx context.Context, params domain.SuspendDefinitionParams) (*domain.WorkflowDefinition, error) {
	return s.transitionDefinitionStatus(ctx, params.WorkflowDefinitionID, "SUSPENDED", params.ActorPrincipalID, params.CorrelationID, nil, nil, "", nil, nil, params.Meta)
}

func (s *PgStore) RetireDefinition(ctx context.Context, params domain.RetireDefinitionParams) (*domain.WorkflowDefinition, error) {
	return s.transitionDefinitionStatus(ctx, params.WorkflowDefinitionID, "RETIRED", params.ActorPrincipalID, params.CorrelationID, nil, nil, "", nil, nil, params.Meta)
}

// transitionDefinitionStatus is the internal helper for definition status transitions.
func (s *PgStore) transitionDefinitionStatus(ctx context.Context, definitionID, newStatus, actorPrincipalID, correlationID string, validatedBy, validatedAt *string, publishedBy string, effectiveFrom, effectiveTo *time.Time, meta domain.CommandMeta) (*domain.WorkflowDefinition, error) {
	var updated *domain.WorkflowDefinition

	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		// Lock the definition row
		const lockQuery = `SELECT ` + definitionColumns + ` FROM workflow_definitions WHERE workflow_definition_id = $1 AND tenant_id = $2 FOR UPDATE;`
		current, err := scanDefinition(tx.QueryRow(ctx, lockQuery, definitionID, tenantID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrWorkflowNotFound
			}
			if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
				return domain.ErrWorkflowNotFound
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		currentStatus := ""
		if current.Status != nil {
			currentStatus = *current.Status
		}

		// Check if transition is allowed
		allowedTransitions := map[string][]string{
			"DRAFT":              {"VALIDATION_PENDING", "RETIRED"},
			"VALIDATION_PENDING": {"VALIDATED", "REJECTED", "DRAFT"},
			"VALIDATED":          {"APPROVAL_PENDING", "DRAFT"},
			"APPROVAL_PENDING":   {"PUBLISHED", "REJECTED", "VALIDATED"},
			"PUBLISHED":          {"SUSPENDED", "SUPERSEDED", "RETIRED"},
			"SUSPENDED":          {"PUBLISHED", "RETIRED"},
			"REJECTED":           {"DRAFT"},
			"SUPERSEDED":         {"RETIRED"},
			"RETIRED":            {},
		}

		allowed, ok := allowedTransitions[currentStatus]
		if !ok {
			return fmt.Errorf("%w: unknown current status %s", domain.ErrInvalidTransition, currentStatus)
		}
		transitionAllowed := false
		for _, s := range allowed {
			if s == newStatus {
				transitionAllowed = true
				break
			}
		}
		if !transitionAllowed {
			return fmt.Errorf("%w: cannot transition from %s to %s", domain.ErrInvalidTransition, currentStatus, newStatus)
		}

		// Idempotent: already in target status
		if currentStatus == newStatus {
			updated = current
			return nil
		}

		// Maker-checker: approver cannot be the same as creator/validator
		if newStatus == "VALIDATED" && validatedBy != nil && *validatedBy == current.CreatedBy {
			return fmt.Errorf("%w: approver cannot be the creator", domain.ErrAuthorizationDenied)
		}

		// Compute checksum for PUBLISHED
		var checksum string
		if newStatus == "PUBLISHED" {
			h := sha256.New()
			h.Write([]byte(current.WorkflowType))
			h.Write([]byte(strconv.Itoa(current.Version)))
			h.Write(current.StagesJSON)
			checksum = hex.EncodeToString(h.Sum(nil))
		}

		// Build update query
		setClauses := []string{"definition_status = $1", "updated_at = NOW()", "row_version = row_version + 1"}
		args := []any{newStatus}
		argIdx := 2

		switch newStatus {
		case "VALIDATED":
			setClauses = append(setClauses, "validated_by = $"+strconv.Itoa(argIdx), "validated_at = $"+strconv.Itoa(argIdx+1))
			args = append(args, *validatedBy, *validatedAt)
			argIdx += 2
		case "PUBLISHED":
			setClauses = append(setClauses,
				"published_by = $"+strconv.Itoa(argIdx),
				"published_at = $"+strconv.Itoa(argIdx+1),
				"effective_from = $"+strconv.Itoa(argIdx+2),
				"effective_to = $"+strconv.Itoa(argIdx+3),
				"checksum = $"+strconv.Itoa(argIdx+4))
			args = append(args, publishedBy, time.Now().UTC())
			if effectiveFrom != nil {
				args = append(args, *effectiveFrom)
			} else {
				args = append(args, time.Now().UTC())
			}
			if effectiveTo != nil {
				args = append(args, *effectiveTo)
			} else {
				args = append(args, nil)
			}
			args = append(args, checksum)
			argIdx += 5
		}

		args = append(args, definitionID, tenantID)
		updateQuery := `
			UPDATE workflow_definitions
			SET ` + strings.Join(setClauses, ", ") + `
			WHERE workflow_definition_id = $` + strconv.Itoa(len(args)-1) + ` AND tenant_id = $` + strconv.Itoa(len(args)) + `
			RETURNING ` + definitionColumns + `;`

		updated, err = scanDefinition(tx.QueryRow(ctx, updateQuery, args...))
		if err != nil {
			if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
				return domain.ErrWorkflowNotFound
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		// Write definition event
		eventType := "workflow.definition." + strings.ToLower(newStatus)
		payload := map[string]any{
			"workflow_definition_id": updated.WorkflowDefinitionID,
			"workflow_type":          updated.WorkflowType,
			"version":                updated.Version,
			"previous_status":        current.Status,
			"new_status":             updated.Status,
			"actor_principal_id":     actorPrincipalID,
		}
		if validatedBy != nil {
			payload["validated_by"] = *validatedBy
		}
		if publishedBy != "" {
			payload["published_by"] = publishedBy
		}

		return s.outboxInsertTx(ctx, tx, eventType, "workflow_definition", updated.WorkflowDefinitionID, correlationID, payload, tenantID, "", actorPrincipalID)
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// =============================================================================
// WFC-04: Cases (WF-2)
// =============================================================================

const caseColumns = `
	case_id,
	tenant_id,
	legal_entity_id,
	workflow_instance_id,
	case_family,
	case_type,
	case_state,
	severity,
	confidentiality,
	title,
	description,
	assigned_to_principal_id,
	created_by_principal_id,
	canonical_case_id,
	split_from_case_id,
	opened_at,
	triaged_at,
	resolved_at,
	closed_at,
	created_at,
	updated_at,
	row_version`

func scanCase(row pgx.Row) (*domain.Case, error) {
	c := &domain.Case{}
	err := row.Scan(
		&c.CaseID,
		&c.TenantID,
		&c.LegalEntityID,
		&c.WorkflowInstanceID,
		&c.CaseFamily,
		&c.CaseType,
		&c.CaseState,
		&c.Severity,
		&c.Confidentiality,
		&c.Title,
		&c.Description,
		&c.AssignedToPrincipal,
		&c.CreatedByPrincipalID,
		&c.CanonicalCaseID,
		&c.SplitFromCaseID,
		&c.OpenedAt,
		&c.TriagedAt,
		&c.ResolvedAt,
		&c.ClosedAt,
		&c.CreatedAt,
		&c.UpdatedAt,
		&c.RowVersion,
	)
	return c, err
}

// CreateCase creates a new case.
func (s *PgStore) CreateCase(ctx context.Context, params domain.CreateCaseParams) (*domain.Case, error) {
	if params.WorkflowInstanceID != nil {
		_, err := s.FindWorkflowByID(ctx, *params.WorkflowInstanceID)
		if err != nil {
			return nil, err
		}
	}

	const query = `
		INSERT INTO cases (
			case_id, tenant_id, legal_entity_id, workflow_instance_id,
			case_family, case_type, case_state, severity, confidentiality,
			title, description, assigned_to_principal_id, created_by_principal_id
		) VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, 'OPEN', 'MEDIUM', 'INTERNAL', $6, $7, $8, $9)
		RETURNING ` + caseColumns + `;`

	var c *domain.Case
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		if params.TenantID != tenantID {
			return domain.ErrTenantMissing
		}

		var scanErr error
		c, scanErr = scanCase(tx.QueryRow(ctx, query,
			tenantID, params.LegalEntityID, params.WorkflowInstanceID,
			params.CaseFamily, params.CaseType, params.Title, params.Description,
			params.AssignedToPrincipal, params.CreatedByPrincipalID))
		if scanErr != nil {
			if errors.Is(mapPgError(scanErr), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}

		return insertOutboxEventTx(ctx, tx, tenantID, "case.created", "case", c.CaseID, params.CorrelationID,
			params.LegalEntityID, "", params.CreatedByPrincipalID, map[string]any{
				"case_id":              c.CaseID,
				"case_family":          c.CaseFamily,
				"case_type":            c.CaseType,
				"case_state":           string(c.CaseState),
				"severity":             c.Severity,
				"confidentiality":      c.Confidentiality,
				"workflow_instance_id": c.WorkflowInstanceID,
				"row_version":          c.RowVersion,
			})
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}

// FindCaseByID returns the Case with the given case_id.
func (s *PgStore) FindCaseByID(ctx context.Context, caseID string) (*domain.Case, error) {
	const query = `SELECT ` + caseColumns + ` FROM cases WHERE case_id = $1 AND tenant_id = $2;`

	var c *domain.Case
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		var scanErr error
		c, scanErr = scanCase(tx.QueryRow(ctx, query, caseID, tenantID))
		return scanErr
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrCaseNotFound
		}
		if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
			return nil, domain.ErrCaseNotFound
		}
		s.log.Error("pg FindCaseByID failed", zap.String("case_id", caseID), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return c, nil
}

// ListCases returns cases matching the given filter.
func (s *PgStore) ListCases(ctx context.Context, filter domain.ListCasesFilter) ([]*domain.Case, error) {
	tenantID := tenantOf(ctx)
	if tenantID == "" {
		return nil, domain.ErrTenantMissing
	}
	if filter.TenantID != "" && filter.TenantID != tenantID {
		return nil, domain.ErrTenantMissing
	}

	query := `SELECT ` + caseColumns + ` FROM cases WHERE tenant_id = $1`
	args := []any{tenantID}

	addFilter := func(clause string, value any) {
		args = append(args, value)
		query += fmt.Sprintf(" AND %s $%d", clause, len(args))
	}

	if filter.LegalEntityID != "" {
		addFilter("legal_entity_id =", filter.LegalEntityID)
	}
	if filter.WorkflowInstanceID != nil {
		addFilter("workflow_instance_id =", *filter.WorkflowInstanceID)
	}
	if filter.CaseType != "" {
		addFilter("case_type =", filter.CaseType)
	}
	if filter.CaseState != "" {
		addFilter("case_state =", filter.CaseState)
	}
	if filter.Confidentiality != "" {
		addFilter("confidentiality =", filter.Confidentiality)
	}
	if filter.AssignedToPrincipal != nil {
		addFilter("assigned_to_principal_id =", *filter.AssignedToPrincipal)
	}

	query += " ORDER BY created_at DESC, case_id DESC"

	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	args = append(args, limit)
	query += fmt.Sprintf(" LIMIT $%d", len(args))
	args = append(args, filter.Offset)
	query += fmt.Sprintf(" OFFSET $%d;", len(args))

	var results []*domain.Case
	var err error
	err = s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
		rows, qErr := tx.Query(ctx, query, args...)
		if qErr != nil {
			if errors.Is(mapPgError(qErr), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			s.log.Error("pg ListCases failed", zap.Error(qErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, qErr)
		}
		defer rows.Close()

		for rows.Next() {
			c, scanErr := scanCase(rows)
			if scanErr != nil {
				s.log.Error("pg ListCases scan failed", zap.Error(scanErr))
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
			}
			results = append(results, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// TransitionCase transitions a case to a new state, enforcing the case state machine.
func (s *PgStore) TransitionCase(ctx context.Context, params domain.TransitionCaseParams) (*domain.Case, bool, error) {
	return s.transitionCaseInternal(ctx, params)
}

// FindCasesByWorkflowInstance returns all cases linked to a workflow instance.
func (s *PgStore) FindCasesByWorkflowInstance(ctx context.Context, workflowInstanceID string) ([]*domain.Case, error) {
	const query = `SELECT ` + caseColumns + ` FROM cases WHERE workflow_instance_id = $1 AND tenant_id = $2 ORDER BY created_at DESC;`

	tenantID := tenantOf(ctx)
	if tenantID == "" {
		return nil, domain.ErrTenantMissing
	}

	var results []*domain.Case
	err := s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
		rows, qErr := tx.Query(ctx, query, workflowInstanceID, tenantID)
		if qErr != nil {
			if errors.Is(mapPgError(qErr), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			s.log.Error("pg FindCasesByWorkflowInstance failed", zap.Error(qErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, qErr)
		}
		defer rows.Close()

		for rows.Next() {
			c, scanErr := scanCase(rows)
			if scanErr != nil {
				s.log.Error("pg FindCasesByWorkflowInstance scan failed", zap.Error(scanErr))
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
			}
			results = append(results, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// =============================================================================
// WFC-04: Case Commands (WF-S2) — Named transitions
// =============================================================================

func (s *PgStore) TriageCase(ctx context.Context, params domain.TriageCaseParams) (*domain.Case, bool, error) {
	return s.transitionCaseInternal(ctx, domain.TransitionCaseParams{
		CaseID:           params.CaseID,
		NewState:         domain.CaseStateTriaged,
		ExpectedVersion:  params.ExpectedVersion,
		ActorPrincipalID: params.ActorPrincipalID,
		CorrelationID:    params.CorrelationID,
		Reason:           new(string),
	})
}

func (s *PgStore) StartCase(ctx context.Context, params domain.StartCaseParams) (*domain.Case, bool, error) {
	return s.transitionCaseInternal(ctx, domain.TransitionCaseParams{
		CaseID:           params.CaseID,
		NewState:         domain.CaseStateInProgress,
		ExpectedVersion:  params.ExpectedVersion,
		ActorPrincipalID: params.ActorPrincipalID,
		CorrelationID:    params.CorrelationID,
		Reason:           new(string),
	})
}

func (s *PgStore) WaitCase(ctx context.Context, params domain.WaitCaseParams) (*domain.Case, bool, error) {
	newState := domain.CaseStateWaitingInternal
	if params.WaitType == "external" {
		newState = domain.CaseStateWaitingExternal
	}
	return s.transitionCaseInternal(ctx, domain.TransitionCaseParams{
		CaseID:           params.CaseID,
		NewState:         newState,
		ExpectedVersion:  params.ExpectedVersion,
		ActorPrincipalID: params.ActorPrincipalID,
		CorrelationID:    params.CorrelationID,
		Reason:           &params.Reason,
	})
}

func (s *PgStore) ResumeCase(ctx context.Context, params domain.ResumeCaseParams) (*domain.Case, bool, error) {
	return s.transitionCaseInternal(ctx, domain.TransitionCaseParams{
		CaseID:           params.CaseID,
		NewState:         domain.CaseStateInProgress,
		ExpectedVersion:  params.ExpectedVersion,
		ActorPrincipalID: params.ActorPrincipalID,
		CorrelationID:    params.CorrelationID,
		Reason:           new(string),
	})
}

func (s *PgStore) ResolveCase(ctx context.Context, params domain.ResolveCaseParams) (*domain.Case, bool, error) {
	return s.transitionCaseInternal(ctx, domain.TransitionCaseParams{
		CaseID:           params.CaseID,
		NewState:         domain.CaseStateResolved,
		ExpectedVersion:  params.ExpectedVersion,
		ActorPrincipalID: params.ActorPrincipalID,
		CorrelationID:    params.CorrelationID,
		Reason:           &params.OutcomeCode,
	})
}

func (s *PgStore) CloseCase(ctx context.Context, params domain.CloseCaseParams) (*domain.Case, bool, error) {
	return s.transitionCaseInternal(ctx, domain.TransitionCaseParams{
		CaseID:           params.CaseID,
		NewState:         domain.CaseStateClosed,
		ExpectedVersion:  params.ExpectedVersion,
		ActorPrincipalID: params.ActorPrincipalID,
		CorrelationID:    params.CorrelationID,
		Reason:           new(string),
	})
}

func (s *PgStore) ReopenCase(ctx context.Context, params domain.ReopenCaseParams) (*domain.Case, bool, error) {
	return s.transitionCaseInternal(ctx, domain.TransitionCaseParams{
		CaseID:           params.CaseID,
		NewState:         domain.CaseStateReopened,
		ExpectedVersion:  params.ExpectedVersion,
		ActorPrincipalID: params.ActorPrincipalID,
		CorrelationID:    params.CorrelationID,
		Reason:           &params.Reason,
	})
}

func (s *PgStore) MergeCase(ctx context.Context, params domain.MergeCaseParams) (*domain.Case, error) {
	var canonical *domain.Case
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		canonicalCase, err := s.findCaseByIDTx(ctx, tx, params.CanonicalCaseID, tenantID)
		if err != nil {
			return err
		}
		aliasCase, err := s.findCaseByIDTx(ctx, tx, params.AliasCaseID, tenantID)
		if err != nil {
			return err
		}

		if canonicalCase.TenantID != aliasCase.TenantID {
			return fmt.Errorf("%w: cross-tenant merge", domain.ErrInvalidCaseTransition)
		}

		const updateAlias = `
			UPDATE cases
			SET canonical_case_id = $1, updated_at = NOW(), row_version = row_version + 1
			WHERE case_id = $2 AND tenant_id = $3;`
		_, err = tx.Exec(ctx, updateAlias, params.CanonicalCaseID, params.AliasCaseID, tenantID)
		if err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		const insertAlias = `
			INSERT INTO case_aliases (alias_id, canonical_case_id, alias_case_id, merged_at, merged_by_principal_id, tenant_id)
			VALUES (gen_random_uuid(), $1, $2, NOW(), $3, $4);`
		_, err = tx.Exec(ctx, insertAlias, params.CanonicalCaseID, params.AliasCaseID, params.ActorPrincipalID, tenantID)
		if err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		payload := map[string]any{
			"canonical_case_id": params.CanonicalCaseID,
			"merged_case_id":    params.AliasCaseID,
			"merged_by":         params.ActorPrincipalID,
		}
		err = s.outboxInsertTx(ctx, tx, "case.merged", "case", params.CanonicalCaseID, params.CorrelationID, payload, tenantID, canonicalCase.LegalEntityID, params.ActorPrincipalID)
		if err != nil {
			return err
		}

		payload["canonical_case_id"] = params.CanonicalCaseID
		payload["merged_case_id"] = params.AliasCaseID
		err = s.outboxInsertTx(ctx, tx, "case.merged", "case", params.AliasCaseID, params.CorrelationID, payload, tenantID, aliasCase.LegalEntityID, params.ActorPrincipalID)
		if err != nil {
			return err
		}

		canonical = canonicalCase
		return nil
	})
	if err != nil {
		return nil, err
	}
	return canonical, nil
}

func (s *PgStore) SplitCase(ctx context.Context, params domain.SplitCaseParams) (*domain.Case, error) {
	var newCase *domain.Case
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		sourceCase, err := s.findCaseByIDTx(ctx, tx, params.SourceCaseID, tenantID)
		if err != nil {
			return err
		}

		const insert = `
			INSERT INTO cases (
				case_id, tenant_id, legal_entity_id, workflow_instance_id,
				case_family, case_type, case_state, severity, confidentiality,
				title, description, created_by_principal_id,
				split_from_case_id
			) VALUES (gen_random_uuid(), $1, $2, NULL, $3, $4, 'OPEN', 'MEDIUM', 'INTERNAL', $5, $6, $7, $8)
			RETURNING ` + caseColumns + `;`
		var scanErr error
		newCase, scanErr = scanCase(tx.QueryRow(ctx, insert,
			tenantID, sourceCase.LegalEntityID, params.NewCaseFamily, params.NewCaseType,
			params.Title, params.Description, params.ActorPrincipalID, params.SourceCaseID))
		if scanErr != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}

		payload := map[string]any{
			"source_case_id": params.SourceCaseID,
			"new_case_id":    newCase.CaseID,
			"evidence_scope": params.EvidenceScope,
			"split_by":       params.ActorPrincipalID,
		}
		err = s.outboxInsertTx(ctx, tx, "case.split", "case", params.SourceCaseID, params.CorrelationID, payload, tenantID, sourceCase.LegalEntityID, params.ActorPrincipalID)
		if err != nil {
			return err
		}

		payload = map[string]any{
			"case_id":            newCase.CaseID,
			"case_family":        newCase.CaseFamily,
			"case_type":          newCase.CaseType,
			"case_state":         string(newCase.CaseState),
			"severity":           newCase.Severity,
			"confidentiality":    newCase.Confidentiality,
			"split_from_case_id": newCase.SplitFromCaseID,
			"row_version":        newCase.RowVersion,
		}
		err = s.outboxInsertTx(ctx, tx, "case.opened", "case", newCase.CaseID, params.CorrelationID, payload, tenantID, newCase.LegalEntityID, params.ActorPrincipalID)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, err
	}
	return newCase, nil
}

// =============================================================================
// WFC-04: Case Members (WF-S2)
// =============================================================================

func (s *PgStore) AddCaseMember(ctx context.Context, params domain.AddCaseMemberParams) error {
	return s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		_, err := s.findCaseByIDTx(ctx, tx, params.CaseID, tenantID)
		if err != nil {
			return err
		}

		const insert = `
			INSERT INTO case_members (case_member_id, tenant_id, case_id, principal_id, role, added_at, added_by_principal_id)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, NOW(), $5)
			ON CONFLICT (case_id, principal_id, role) WHERE removed_at IS NULL
			DO NOTHING;`
		_, err = tx.Exec(ctx, insert, tenantID, params.CaseID, params.PrincipalID, params.Role, params.ActorPrincipalID)
		if err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		payload := map[string]any{
			"case_id":   params.CaseID,
			"member_id": params.PrincipalID,
			"role":      params.Role,
			"added_by":  params.ActorPrincipalID,
		}
		return s.outboxInsertTx(ctx, tx, "case.member_added", "case", params.CaseID, params.CorrelationID, payload, tenantID, "", params.ActorPrincipalID)
	})
}

func (s *PgStore) RemoveCaseMember(ctx context.Context, params domain.RemoveCaseMemberParams) error {
	return s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		const update = `
			UPDATE case_members
			SET removed_at = NOW(), removed_by_principal_id = $1
			WHERE case_id = $2 AND principal_id = $3 AND role = $4 AND tenant_id = $5 AND removed_at IS NULL;`
		result, err := tx.Exec(ctx, update, params.ActorPrincipalID, params.CaseID, params.PrincipalID, params.Role, tenantID)
		if err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		if result.RowsAffected() == 0 {
			return domain.ErrCaseNotFound
		}

		payload := map[string]any{
			"case_id":    params.CaseID,
			"member_id":  params.PrincipalID,
			"role":       params.Role,
			"removed_by": params.ActorPrincipalID,
		}
		return s.outboxInsertTx(ctx, tx, "case.member_removed", "case", params.CaseID, params.CorrelationID, payload, tenantID, "", params.ActorPrincipalID)
	})
}

func (s *PgStore) ListCaseMembers(ctx context.Context, caseID string) ([]*domain.CaseMember, error) {
	tenantID := tenantOf(ctx)
	if tenantID == "" {
		return nil, domain.ErrTenantMissing
	}

	const query = `
		SELECT case_member_id, case_id, principal_id, role, added_at, added_by_principal_id, removed_at, removed_by_principal_id
		FROM case_members
		WHERE case_id = $1 AND tenant_id = $2 AND removed_at IS NULL
		ORDER BY added_at;`

	var results []*domain.CaseMember
	err := s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
		rows, qErr := tx.Query(ctx, query, caseID, tenantID)
		if qErr != nil {
			if errors.Is(mapPgError(qErr), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			s.log.Error("pg ListCaseMembers failed", zap.Error(qErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, qErr)
		}
		defer rows.Close()

		for rows.Next() {
			var m domain.CaseMember
			scanErr := rows.Scan(&m.CaseMemberID, &m.CaseID, &m.PrincipalID, &m.Role, &m.AddedAt, &m.AddedByPrincipalID, &m.RemovedAt, &m.RemovedByPrincipalID)
			if scanErr != nil {
				s.log.Error("pg ListCaseMembers scan failed", zap.Error(scanErr))
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
			}
			results = append(results, &m)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// =============================================================================
// WFC-04: Case Notes (WF-S2)
// =============================================================================

func (s *PgStore) CreateCaseNote(ctx context.Context, params domain.CreateCaseNoteParams) (*domain.CaseNote, error) {
	var note *domain.CaseNote
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		const insert = `
			INSERT INTO case_notes (case_note_id, tenant_id, case_id, principal_id, content, visibility, evidence_refs, created_at, updated_at, version)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, NOW(), NOW(), 1)
			RETURNING case_note_id, tenant_id, case_id, principal_id, content, visibility, evidence_refs, created_at, updated_at, version, previous_note_id;`
		var scanErr error
		note, scanErr = s.scanCaseNote(tx.QueryRow(ctx, insert, tenantID, params.CaseID, params.PrincipalID, params.Content, params.Visibility, params.EvidenceRefs))
		if scanErr != nil {
			if errors.Is(mapPgError(scanErr), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}

		payload := map[string]any{
			"case_id":       params.CaseID,
			"note_id":       note.CaseNoteID,
			"principal_id":  params.PrincipalID,
			"visibility":    params.Visibility,
			"evidence_refs": params.EvidenceRefs,
		}
		return s.outboxInsertTx(ctx, tx, "case.note_created", "case", params.CaseID, params.CorrelationID, payload, tenantID, "", params.PrincipalID)
	})
	if err != nil {
		return nil, err
	}
	return note, nil
}

func (s *PgStore) UpdateCaseNote(ctx context.Context, params domain.UpdateCaseNoteParams) (*domain.CaseNote, error) {
	var note *domain.CaseNote
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		const lockQuery = `
			SELECT case_note_id, tenant_id, case_id, principal_id, content, visibility, evidence_refs, created_at, updated_at, version, previous_note_id
			FROM case_notes
			WHERE case_note_id = $1 AND tenant_id = $2
			FOR UPDATE;`
		current, err := s.scanCaseNote(tx.QueryRow(ctx, lockQuery, params.CaseNoteID, tenantID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrCaseNotFound
			}
			if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		if params.ExpectedVersion > 0 && current.Version != params.ExpectedVersion {
			return domain.ErrConcurrencyConflict
		}

		const update = `
			UPDATE case_notes
			SET content = $1, visibility = $2, evidence_refs = $3, updated_at = NOW(), version = version + 1, previous_note_id = $4
			WHERE case_note_id = $5 AND tenant_id = $6
			RETURNING case_note_id, tenant_id, case_id, principal_id, content, visibility, evidence_refs, created_at, updated_at, version, previous_note_id;`
		var scanErr error
		note, scanErr = s.scanCaseNote(tx.QueryRow(ctx, update, params.Content, params.Visibility, params.EvidenceRefs, params.CaseNoteID, params.CaseNoteID, tenantID))
		if scanErr != nil {
			if errors.Is(mapPgError(scanErr), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}

		payload := map[string]any{
			"case_id":      current.CaseID,
			"note_id":      params.CaseNoteID,
			"principal_id": params.PrincipalID,
			"previous_ver": current.Version,
			"new_ver":      note.Version,
			"visibility":   params.Visibility,
		}
		return s.outboxInsertTx(ctx, tx, "case.note_updated", "case", current.CaseID, params.CorrelationID, payload, tenantID, "", params.PrincipalID)
	})
	if err != nil {
		return nil, err
	}
	return note, nil
}

func (s *PgStore) GetCaseNote(ctx context.Context, caseNoteID string) (*domain.CaseNote, error) {
	tenantID := tenantOf(ctx)
	if tenantID == "" {
		return nil, domain.ErrTenantMissing
	}

	const query = `
		SELECT case_note_id, tenant_id, case_id, principal_id, content, visibility, evidence_refs, created_at, updated_at, version, previous_note_id
		FROM case_notes
		WHERE case_note_id = $1 AND tenant_id = $2;`

	var note *domain.CaseNote
	err := s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
		var scanErr error
		note, scanErr = s.scanCaseNote(tx.QueryRow(ctx, query, caseNoteID, tenantID))
		return scanErr
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrCaseNotFound
		}
		if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
			return nil, domain.ErrInvalidIdentifier
		}
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return note, nil
}

func (s *PgStore) ListCaseNotes(ctx context.Context, caseID string, visibility string) ([]*domain.CaseNote, error) {
	tenantID := tenantOf(ctx)
	if tenantID == "" {
		return nil, domain.ErrTenantMissing
	}

	query := `
		SELECT case_note_id, tenant_id, case_id, principal_id, content, visibility, evidence_refs, created_at, updated_at, version, previous_note_id
		FROM case_notes
		WHERE case_id = $1 AND tenant_id = $2`
	args := []any{caseID, tenantID}

	if visibility != "" {
		args = append(args, visibility)
		query += " AND visibility = $" + strconv.Itoa(len(args))
	}

	query += " ORDER BY created_at DESC;"

	var results []*domain.CaseNote
	err := s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
		rows, qErr := tx.Query(ctx, query, args...)
		if qErr != nil {
			if errors.Is(mapPgError(qErr), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			s.log.Error("pg ListCaseNotes failed", zap.Error(qErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, qErr)
		}
		defer rows.Close()

		for rows.Next() {
			n, scanErr := s.scanCaseNote(rows)
			if scanErr != nil {
				s.log.Error("pg ListCaseNotes scan failed", zap.Error(scanErr))
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
			}
			results = append(results, n)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

func (s *PgStore) scanCaseNote(row pgx.Row) (*domain.CaseNote, error) {
	n := &domain.CaseNote{}
	err := row.Scan(
		&n.CaseNoteID,
		&n.TenantID,
		&n.CaseID,
		&n.PrincipalID,
		&n.Content,
		&n.Visibility,
		&n.EvidenceRefs,
		&n.CreatedAt,
		&n.UpdatedAt,
		&n.Version,
		&n.PreviousNoteID,
	)
	return n, err
}

// =============================================================================
// WFC-04: Case Links (WF-S2)
// =============================================================================

func (s *PgStore) CreateCaseLink(ctx context.Context, params domain.CreateCaseLinkParams) (*domain.CaseLink, error) {
	var link *domain.CaseLink
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		const insert = `
			INSERT INTO case_links (case_link_id, tenant_id, case_id, domain_object_type, domain_object_id, linked_at, linked_by_principal_id)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, NOW(), $5)
			RETURNING case_link_id, tenant_id, case_id, domain_object_type, domain_object_id, linked_at, linked_by_principal_id;`
		var scanErr error
		link, scanErr = s.scanCaseLink(tx.QueryRow(ctx, insert, tenantID, params.CaseID, params.DomainObjectType, params.DomainObjectID, params.ActorPrincipalID))
		if scanErr != nil {
			if errors.Is(mapPgError(scanErr), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}

		payload := map[string]any{
			"case_id":            params.CaseID,
			"link_id":            link.CaseLinkID,
			"domain_object_type": params.DomainObjectType,
			"domain_object_id":   params.DomainObjectID,
			"linked_by":          params.ActorPrincipalID,
		}
		return s.outboxInsertTx(ctx, tx, "case.link_created", "case", params.CaseID, params.CorrelationID, payload, tenantID, "", params.ActorPrincipalID)
	})
	if err != nil {
		return nil, err
	}
	return link, nil
}

func (s *PgStore) ListCaseLinks(ctx context.Context, caseID string) ([]*domain.CaseLink, error) {
	tenantID := tenantOf(ctx)
	if tenantID == "" {
		return nil, domain.ErrTenantMissing
	}

	const query = `
		SELECT case_link_id, tenant_id, case_id, domain_object_type, domain_object_id, linked_at, linked_by_principal_id
		FROM case_links
		WHERE case_id = $1 AND tenant_id = $2
		ORDER BY linked_at;`

	var results []*domain.CaseLink
	err := s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
		rows, qErr := tx.Query(ctx, query, caseID, tenantID)
		if qErr != nil {
			if errors.Is(mapPgError(qErr), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			s.log.Error("pg ListCaseLinks failed", zap.Error(qErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, qErr)
		}
		defer rows.Close()

		for rows.Next() {
			l, scanErr := s.scanCaseLink(rows)
			if scanErr != nil {
				s.log.Error("pg ListCaseLinks scan failed", zap.Error(scanErr))
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
			}
			results = append(results, l)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

func (s *PgStore) scanCaseLink(row pgx.Row) (*domain.CaseLink, error) {
	l := &domain.CaseLink{}
	err := row.Scan(
		&l.CaseLinkID,
		&l.TenantID,
		&l.CaseID,
		&l.DomainObjectType,
		&l.DomainObjectID,
		&l.LinkedAt,
		&l.LinkedByPrincipalID,
	)
	return l, err
}

// =============================================================================
// WFC-02: Work Items (WF-S2 / WF-S23)
// =============================================================================

const workItemColumns = `
	work_item_id, tenant_id, legal_entity_id, case_id, workflow_instance_id,
	item_type, queue, role, assignee_principal_id, state, mandatory,
	completion_schema, due_at, created_at, updated_at, row_version`

func scanWorkItem(row pgx.Row) (*domain.WorkItem, error) {
	w := &domain.WorkItem{}
	err := row.Scan(
		&w.WorkItemID,
		&w.TenantID,
		&w.LegalEntityID,
		&w.CaseID,
		&w.WorkflowInstanceID,
		&w.ItemType,
		&w.Queue,
		&w.Role,
		&w.AssigneePrincipalID,
		&w.State,
		&w.Mandatory,
		&w.CompletionSchema,
		&w.DueAt,
		&w.CreatedAt,
		&w.UpdatedAt,
		&w.RowVersion,
	)
	return w, err
}

func (s *PgStore) CreateWorkItem(ctx context.Context, params domain.CreateWorkItemParams) (*domain.WorkItem, error) {
	var item *domain.WorkItem
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		if params.TenantID != tenantID {
			return domain.ErrTenantMissing
		}

		var completionSchema []byte
		if params.CompletionSchema != nil {
			var err error
			completionSchema, err = json.Marshal(params.CompletionSchema)
			if err != nil {
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
			}
		}

		const insert = `
			INSERT INTO work_items (
				work_item_id, tenant_id, legal_entity_id, case_id, workflow_instance_id,
				item_type, queue, role, assignee_principal_id, state, mandatory,
				completion_schema, due_at, created_at, updated_at, row_version
			) VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, $8, 'UNASSIGNED', $9, $10, $11, NOW(), NOW(), 1)
			RETURNING ` + workItemColumns + `;`
		var scanErr error
		item, scanErr = scanWorkItem(tx.QueryRow(ctx, insert,
			tenantID, params.LegalEntityID, params.CaseID, params.WorkflowInstanceID,
			params.ItemType, params.Queue, params.Role, params.AssigneePrincipalID,
			params.Mandatory, completionSchema, params.DueAt))
		if scanErr != nil {
			if errors.Is(mapPgError(scanErr), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}

		payload := map[string]any{
			"work_item_id":       item.WorkItemID,
			"item_type":          item.ItemType,
			"mandatory":          item.Mandatory,
			"assignee_principal": item.AssigneePrincipalID,
		}
		return s.outboxInsertTx(ctx, tx, "work_item.created", "work_item", item.WorkItemID, params.CorrelationID, payload, tenantID, params.LegalEntityID, params.ActorPrincipalID)
	})
	if err != nil {
		return nil, err
	}
	return item, nil
}

func (s *PgStore) GetWorkItem(ctx context.Context, workItemID string) (*domain.WorkItem, error) {
	tenantID := tenantOf(ctx)
	if tenantID == "" {
		return nil, domain.ErrTenantMissing
	}

	const query = `SELECT ` + workItemColumns + ` FROM work_items WHERE work_item_id = $1 AND tenant_id = $2;`

	var item *domain.WorkItem
	err := s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
		var scanErr error
		item, scanErr = scanWorkItem(tx.QueryRow(ctx, query, workItemID, tenantID))
		return scanErr
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrCaseNotFound
		}
		if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
			return nil, domain.ErrInvalidIdentifier
		}
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return item, nil
}

func (s *PgStore) ListWorkItems(ctx context.Context, tenantID string, filter domain.ListWorkItemsFilter) ([]*domain.WorkItem, *domain.PaginationCursor, error) {
	if filter.TenantID != "" && filter.TenantID != tenantID {
		return nil, nil, domain.ErrTenantMissing
	}

	query := `SELECT ` + workItemColumns + ` FROM work_items WHERE tenant_id = $1`
	args := []any{tenantID}

	addFilter := func(clause string, value any) {
		args = append(args, value)
		query += fmt.Sprintf(" AND %s $%d", clause, len(args))
	}

	if filter.CaseID != nil {
		addFilter("case_id =", *filter.CaseID)
	}
	if filter.WorkflowInstanceID != nil {
		addFilter("workflow_instance_id =", *filter.WorkflowInstanceID)
	}
	if filter.ItemType != "" {
		addFilter("item_type =", filter.ItemType)
	}
	if filter.Queue != nil {
		addFilter("queue =", *filter.Queue)
	}
	if filter.AssigneePrincipalID != nil {
		addFilter("assignee_principal_id =", *filter.AssigneePrincipalID)
	}
	if filter.State != "" {
		addFilter("state =", filter.State)
	}
	if filter.Mandatory != nil {
		addFilter("mandatory =", *filter.Mandatory)
	}

	query += " ORDER BY created_at DESC, work_item_id DESC"

	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if filter.Cursor != nil {
		args = append(args, filter.Cursor.StartedAt)
		query += fmt.Sprintf(" AND (created_at, work_item_id) < ($%d, $%d)", len(args), len(args)+1)
		args = append(args, filter.Cursor.InstanceID)
	}
	args = append(args, limit+1)
	query += fmt.Sprintf(" LIMIT $%d;", len(args))

	var results []*domain.WorkItem
	var err error
	err = s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
		rows, qErr := tx.Query(ctx, query, args...)
		if qErr != nil {
			if errors.Is(mapPgError(qErr), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			s.log.Error("pg ListWorkItems failed", zap.Error(qErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, qErr)
		}
		defer rows.Close()

		for rows.Next() {
			w, scanErr := scanWorkItem(rows)
			if scanErr != nil {
				s.log.Error("pg ListWorkItems scan failed", zap.Error(scanErr))
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
			}
			results = append(results, w)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, nil, err
	}

	var nextCursor *domain.PaginationCursor
	if len(results) > limit {
		last := results[limit-1]
		results = results[:limit]
		nextCursor = &domain.PaginationCursor{
			StartedAt:  last.CreatedAt.UTC().Format(time.RFC3339),
			InstanceID: last.WorkItemID,
		}
	}

	return results, nextCursor, nil
}

func (s *PgStore) SatisfyWorkItem(ctx context.Context, params domain.SatisfyWorkItemParams) (*domain.WorkItem, bool, error) {
	var updated *domain.WorkItem
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		const lockQuery = `SELECT ` + workItemColumns + ` FROM work_items WHERE work_item_id = $1 AND tenant_id = $2 FOR UPDATE;`
		current, err := scanWorkItem(tx.QueryRow(ctx, lockQuery, params.WorkItemID, tenantID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrCaseNotFound
			}
			if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		if params.ExpectedVersion > 0 && current.RowVersion != params.ExpectedVersion {
			return domain.ErrConcurrencyConflict
		}

		if current.State != "CLAIMED" && current.State != "UNASSIGNED" {
			return fmt.Errorf("%w: work item not in claimable state", domain.ErrInvalidCaseTransition)
		}

		var completionData []byte
		if params.CompletionData != nil {
			var err error
			completionData, err = json.Marshal(params.CompletionData)
			if err != nil {
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
			}
		}

		const update = `
			UPDATE work_items
			SET state = 'COMPLETED', completion_schema = $1, updated_at = NOW(), row_version = row_version + 1
			WHERE work_item_id = $2 AND tenant_id = $3
			RETURNING ` + workItemColumns + `;`
		updated, err = scanWorkItem(tx.QueryRow(ctx, update, completionData, params.WorkItemID, tenantID))
		if err != nil {
			if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		payload := map[string]any{
			"work_item_id":    updated.WorkItemID,
			"item_type":       updated.ItemType,
			"completion_data": params.CompletionData,
			"completed_by":    params.ActorPrincipalID,
		}
		return s.outboxInsertTx(ctx, tx, "work_item.completed", "work_item", updated.WorkItemID, params.CorrelationID, payload, tenantID, updated.LegalEntityID, params.ActorPrincipalID)
	})
	if err != nil {
		return nil, false, err
	}
	return updated, updated != nil, nil
}

func (s *PgStore) WaiveWorkItem(ctx context.Context, params domain.WaiveWorkItemParams) (*domain.WorkItem, bool, error) {
	var updated *domain.WorkItem
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		const lockQuery = `SELECT ` + workItemColumns + ` FROM work_items WHERE work_item_id = $1 AND tenant_id = $2 FOR UPDATE;`
		current, err := scanWorkItem(tx.QueryRow(ctx, lockQuery, params.WorkItemID, tenantID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrCaseNotFound
			}
			if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		if params.ExpectedVersion > 0 && current.RowVersion != params.ExpectedVersion {
			return domain.ErrConcurrencyConflict
		}

		if !current.Mandatory {
			return fmt.Errorf("%w: work item is not mandatory", domain.ErrInvalidCaseTransition)
		}

		const update = `
			UPDATE work_items
			SET state = 'COMPLETED', updated_at = NOW(), row_version = row_version + 1
			WHERE work_item_id = $1 AND tenant_id = $2
			RETURNING ` + workItemColumns + `;`
		updated, err = scanWorkItem(tx.QueryRow(ctx, update, params.WorkItemID, tenantID))
		if err != nil {
			if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		payload := map[string]any{
			"work_item_id": updated.WorkItemID,
			"item_type":    updated.ItemType,
			"waived_by":    params.ActorPrincipalID,
			"reason":       params.Reason,
		}
		return s.outboxInsertTx(ctx, tx, "work_item.waived", "work_item", updated.WorkItemID, params.CorrelationID, payload, tenantID, updated.LegalEntityID, params.ActorPrincipalID)
	})
	if err != nil {
		return nil, false, err
	}
	return updated, updated != nil, nil
}

func (s *PgStore) TransferWorkItem(ctx context.Context, params domain.TransferWorkItemParams) (*domain.WorkItem, bool, error) {
	var updated *domain.WorkItem
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		const lockQuery = `SELECT ` + workItemColumns + ` FROM work_items WHERE work_item_id = $1 AND tenant_id = $2 FOR UPDATE;`
		current, err := scanWorkItem(tx.QueryRow(ctx, lockQuery, params.WorkItemID, tenantID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrCaseNotFound
			}
			if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		if params.ExpectedVersion > 0 && current.RowVersion != params.ExpectedVersion {
			return domain.ErrConcurrencyConflict
		}

		setClauses := []string{"updated_at = NOW()", "row_version = row_version + 1"}
		args := []any{}

		if params.NewCaseID != nil {
			setClauses = append(setClauses, "case_id = $"+strconv.Itoa(len(args)+1))
			args = append(args, *params.NewCaseID)
		}
		if params.NewWorkflowID != nil {
			setClauses = append(setClauses, "workflow_instance_id = $"+strconv.Itoa(len(args)+1))
			args = append(args, *params.NewWorkflowID)
		}

		args = append(args, params.WorkItemID, tenantID)
		updateQuery := `
			UPDATE work_items
			SET ` + strings.Join(setClauses, ", ") + `
			WHERE work_item_id = $` + strconv.Itoa(len(args)-1) + ` AND tenant_id = $` + strconv.Itoa(len(args)) + `
			RETURNING ` + workItemColumns + `;`

		updated, err = scanWorkItem(tx.QueryRow(ctx, updateQuery, args...))
		if err != nil {
			if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		payload := map[string]any{
			"work_item_id":    updated.WorkItemID,
			"item_type":       updated.ItemType,
			"transferred_by":  params.ActorPrincipalID,
			"new_case_id":     params.NewCaseID,
			"new_workflow_id": params.NewWorkflowID,
		}
		return s.outboxInsertTx(ctx, tx, "work_item.transferred", "work_item", updated.WorkItemID, params.CorrelationID, payload, tenantID, updated.LegalEntityID, params.ActorPrincipalID)
	})
	if err != nil {
		return nil, false, err
	}
	return updated, updated != nil, nil
}

func (s *PgStore) ClaimWorkItem(ctx context.Context, workItemID string, expectedVersion int, assigneePrincipalID string, actorPrincipalID string, correlationID string) (*domain.WorkItem, bool, error) {
	var updated *domain.WorkItem
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		const lockQuery = `SELECT ` + workItemColumns + ` FROM work_items WHERE work_item_id = $1 AND tenant_id = $2 FOR UPDATE;`
		current, err := scanWorkItem(tx.QueryRow(ctx, lockQuery, workItemID, tenantID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrCaseNotFound
			}
			if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		if expectedVersion > 0 && current.RowVersion != expectedVersion {
			return domain.ErrConcurrencyConflict
		}

		if current.State != "UNASSIGNED" {
			return fmt.Errorf("%w: work item not unassigned", domain.ErrInvalidCaseTransition)
		}

		const update = `
			UPDATE work_items
			SET assignee_principal_id = $1, state = 'CLAIMED', updated_at = NOW(), row_version = row_version + 1
			WHERE work_item_id = $2 AND tenant_id = $3
			RETURNING ` + workItemColumns + `;`
		updated, err = scanWorkItem(tx.QueryRow(ctx, update, assigneePrincipalID, workItemID, tenantID))
		if err != nil {
			if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		const insertAssign = `
			INSERT INTO work_item_assignments (work_item_assignment_id, tenant_id, work_item_id, previous_assignee_id, new_assignee_id, actor_principal_id, assigned_at)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, NOW());`
		_, err = tx.Exec(ctx, insertAssign, tenantID, workItemID, current.AssigneePrincipalID, assigneePrincipalID, actorPrincipalID)
		if err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		payload := map[string]any{
			"work_item_id":       updated.WorkItemID,
			"item_type":          updated.ItemType,
			"claimed_by":         actorPrincipalID,
			"assignee_principal": assigneePrincipalID,
		}
		return s.outboxInsertTx(ctx, tx, "work_item.claimed", "work_item", updated.WorkItemID, correlationID, payload, tenantID, updated.LegalEntityID, actorPrincipalID)
	})
	if err != nil {
		return nil, false, err
	}
	return updated, updated != nil, nil
}

func (s *PgStore) ReleaseWorkItem(ctx context.Context, workItemID string, expectedVersion int, actorPrincipalID string, correlationID string) (*domain.WorkItem, bool, error) {
	var updated *domain.WorkItem
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		const lockQuery = `SELECT ` + workItemColumns + ` FROM work_items WHERE work_item_id = $1 AND tenant_id = $2 FOR UPDATE;`
		current, err := scanWorkItem(tx.QueryRow(ctx, lockQuery, workItemID, tenantID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrCaseNotFound
			}
			if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		if expectedVersion > 0 && current.RowVersion != expectedVersion {
			return domain.ErrConcurrencyConflict
		}

		if current.State != "CLAIMED" {
			return fmt.Errorf("%w: work item not claimed", domain.ErrInvalidCaseTransition)
		}

		const update = `
			UPDATE work_items
			SET assignee_principal_id = NULL, state = 'UNASSIGNED', updated_at = NOW(), row_version = row_version + 1
			WHERE work_item_id = $1 AND tenant_id = $2
			RETURNING ` + workItemColumns + `;`
		updated, err = scanWorkItem(tx.QueryRow(ctx, update, workItemID, tenantID))
		if err != nil {
			if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		const insertAssign = `
			INSERT INTO work_item_assignments (work_item_assignment_id, tenant_id, work_item_id, previous_assignee_id, new_assignee_id, actor_principal_id, assigned_at)
			VALUES (gen_random_uuid(), $1, $2, $3, NULL, $4, NOW());`
		_, err = tx.Exec(ctx, insertAssign, tenantID, workItemID, current.AssigneePrincipalID, actorPrincipalID)
		if err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		payload := map[string]any{
			"work_item_id":      updated.WorkItemID,
			"item_type":         updated.ItemType,
			"released_by":       actorPrincipalID,
			"previous_assignee": current.AssigneePrincipalID,
		}
		return s.outboxInsertTx(ctx, tx, "work_item.released", "work_item", updated.WorkItemID, correlationID, payload, tenantID, updated.LegalEntityID, actorPrincipalID)
	})
	if err != nil {
		return nil, false, err
	}
	return updated, updated != nil, nil
}

func (s *PgStore) CancelWorkItem(ctx context.Context, workItemID string, expectedVersion int, actorPrincipalID string, correlationID string) (*domain.WorkItem, bool, error) {
	var updated *domain.WorkItem
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		const lockQuery = `SELECT ` + workItemColumns + ` FROM work_items WHERE work_item_id = $1 AND tenant_id = $2 FOR UPDATE;`
		current, err := scanWorkItem(tx.QueryRow(ctx, lockQuery, workItemID, tenantID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrCaseNotFound
			}
			if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		if expectedVersion > 0 && current.RowVersion != expectedVersion {
			return domain.ErrConcurrencyConflict
		}

		if current.State == "COMPLETED" || current.State == "CANCELLED" {
			return fmt.Errorf("%w: work item already terminal", domain.ErrInvalidCaseTransition)
		}

		const update = `
			UPDATE work_items
			SET state = 'CANCELLED', updated_at = NOW(), row_version = row_version + 1
			WHERE work_item_id = $1 AND tenant_id = $2
			RETURNING ` + workItemColumns + `;`
		updated, err = scanWorkItem(tx.QueryRow(ctx, update, workItemID, tenantID))
		if err != nil {
			if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		payload := map[string]any{
			"work_item_id":   updated.WorkItemID,
			"item_type":      updated.ItemType,
			"cancelled_by":   actorPrincipalID,
			"previous_state": current.State,
		}
		return s.outboxInsertTx(ctx, tx, "work_item.cancelled", "work_item", updated.WorkItemID, correlationID, payload, tenantID, updated.LegalEntityID, actorPrincipalID)
	})
	if err != nil {
		return nil, false, err
	}
	return updated, updated != nil, nil
}

func (s *PgStore) EscalateWorkItem(ctx context.Context, workItemID string, expectedVersion int, actorPrincipalID string, correlationID string) (*domain.WorkItem, bool, error) {
	var updated *domain.WorkItem
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		const lockQuery = `SELECT ` + workItemColumns + ` FROM work_items WHERE work_item_id = $1 AND tenant_id = $2 FOR UPDATE;`
		current, err := scanWorkItem(tx.QueryRow(ctx, lockQuery, workItemID, tenantID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrCaseNotFound
			}
			if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		if expectedVersion > 0 && current.RowVersion != expectedVersion {
			return domain.ErrConcurrencyConflict
		}

		const update = `
			UPDATE work_items
			SET updated_at = NOW(), row_version = row_version + 1
			WHERE work_item_id = $1 AND tenant_id = $2
			RETURNING ` + workItemColumns + `;`
		updated, err = scanWorkItem(tx.QueryRow(ctx, update, workItemID, tenantID))
		if err != nil {
			if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		payload := map[string]any{
			"work_item_id": updated.WorkItemID,
			"item_type":    updated.ItemType,
			"escalated_by": actorPrincipalID,
		}
		return s.outboxInsertTx(ctx, tx, "work_item.escalated", "work_item", updated.WorkItemID, correlationID, payload, tenantID, updated.LegalEntityID, actorPrincipalID)
	})
	if err != nil {
		return nil, false, err
	}
	return updated, updated != nil, nil
}

func (s *PgStore) ReassignWorkItem(ctx context.Context, workItemID string, expectedVersion int, newAssigneePrincipalID string, actorPrincipalID string, correlationID string) (*domain.WorkItem, bool, error) {
	var updated *domain.WorkItem
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		const lockQuery = `SELECT ` + workItemColumns + ` FROM work_items WHERE work_item_id = $1 AND tenant_id = $2 FOR UPDATE;`
		current, err := scanWorkItem(tx.QueryRow(ctx, lockQuery, workItemID, tenantID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrCaseNotFound
			}
			if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		if expectedVersion > 0 && current.RowVersion != expectedVersion {
			return domain.ErrConcurrencyConflict
		}

		if current.State != "CLAIMED" {
			return fmt.Errorf("%w: work item not claimed", domain.ErrInvalidCaseTransition)
		}

		const update = `
			UPDATE work_items
			SET assignee_principal_id = $1, updated_at = NOW(), row_version = row_version + 1
			WHERE work_item_id = $2 AND tenant_id = $3
			RETURNING ` + workItemColumns + `;`
		updated, err = scanWorkItem(tx.QueryRow(ctx, update, newAssigneePrincipalID, workItemID, tenantID))
		if err != nil {
			if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		const insertAssign = `
			INSERT INTO work_item_assignments (work_item_assignment_id, tenant_id, work_item_id, previous_assignee_id, new_assignee_id, actor_principal_id, assigned_at)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, NOW());`
		_, err = tx.Exec(ctx, insertAssign, tenantID, workItemID, current.AssigneePrincipalID, newAssigneePrincipalID, actorPrincipalID)
		if err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		payload := map[string]any{
			"work_item_id":      updated.WorkItemID,
			"item_type":         updated.ItemType,
			"reassigned_by":     actorPrincipalID,
			"new_assignee":      newAssigneePrincipalID,
			"previous_assignee": current.AssigneePrincipalID,
		}
		return s.outboxInsertTx(ctx, tx, "work_item.reassigned", "work_item", updated.WorkItemID, correlationID, payload, tenantID, updated.LegalEntityID, actorPrincipalID)
	})
	if err != nil {
		return nil, false, err
	}
	return updated, updated != nil, nil
}

func (s *PgStore) BulkCompleteWorkItems(ctx context.Context, params domain.BulkCompleteWorkItemsParams) ([]*domain.WorkItem, []*domain.WorkItemResult, error) {
	var results []*domain.WorkItem
	var itemResults []*domain.WorkItemResult

	for _, workItemID := range params.WorkItemIDs {
		item, transitioned, err := s.SatisfyWorkItem(ctx, domain.SatisfyWorkItemParams{
			WorkItemID:       workItemID,
			ExpectedVersion:  0,
			ActorPrincipalID: params.ActorPrincipalID,
			CompletionData:   map[string]any{"bulk": true},
			CorrelationID:    params.CorrelationID,
			Meta:             params.Meta,
		})
		if err != nil {
			itemResults = append(itemResults, &domain.WorkItemResult{
				WorkItemID: workItemID,
				Success:    false,
				Error:      err.Error(),
			})
		} else {
			itemResults = append(itemResults, &domain.WorkItemResult{
				WorkItemID: workItemID,
				Success:    transitioned,
			})
			if item != nil {
				results = append(results, item)
			}
		}
	}

	return results, itemResults, nil
}

// =============================================================================
// WFC-04: Idempotent Case Open (WF-S7)
// =============================================================================

func (s *PgStore) FindCaseBySource(ctx context.Context, sourceType, sourceID, milestone string) (*domain.Case, error) {
	tenantID := tenantOf(ctx)
	if tenantID == "" {
		return nil, domain.ErrTenantMissing
	}

	const query = `
		SELECT ` + caseColumns + `
		FROM cases
		WHERE tenant_id = $1
		AND workflow_instance_id IN (
			SELECT workflow_instance_id FROM workflow_instances
			WHERE tenant_id = $1
			AND idempotency_key = $2
		);`

	idemKey := fmt.Sprintf("%s:%s:%s", sourceType, sourceID, milestone)

	var c *domain.Case
	err := s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
		var scanErr error
		c, scanErr = scanCase(tx.QueryRow(ctx, query, tenantID, idemKey))
		return scanErr
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrCaseNotFound
		}
		if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
			return nil, domain.ErrInvalidIdentifier
		}
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return c, nil
}

// =============================================================================
// Internal helpers
// =============================================================================

func (s *PgStore) findCaseByIDTx(ctx context.Context, tx pgx.Tx, caseID string, tenantID string) (*domain.Case, error) {
	const query = `SELECT ` + caseColumns + ` FROM cases WHERE case_id = $1 AND tenant_id = $2;`
	return scanCase(tx.QueryRow(ctx, query, caseID, tenantID))
}

func (s *PgStore) outboxInsertTx(ctx context.Context, tx pgx.Tx, eventType, aggregateType, aggregateID, correlationID string, payload map[string]any, tenantID, legalEntityID, actorID string) error {
	return insertOutboxEventTx(ctx, tx, tenantID, eventType, aggregateType, aggregateID, correlationID, legalEntityID, "", actorID, payload)
}

func (s *PgStore) transitionCaseInternal(ctx context.Context, params domain.TransitionCaseParams) (*domain.Case, bool, error) {
	var updated *domain.Case

	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		const lockQuery = `
			SELECT ` + caseColumns + `
			FROM cases
			WHERE case_id = $1 AND tenant_id = $2
			FOR UPDATE;`

		current, err := scanCase(tx.QueryRow(ctx, lockQuery, params.CaseID, tenantID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrCaseNotFound
			}
			if mapped := mapPgError(err); errors.Is(mapped, domain.ErrInvalidIdentifier) {
				return domain.ErrCaseNotFound
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		if params.ExpectedVersion > 0 && current.RowVersion != params.ExpectedVersion {
			return domain.ErrConcurrencyConflict
		}

		allowed, ok := domain.CaseStateTransitions[current.CaseState]
		if !ok {
			return domain.ErrInvalidCaseTransition
		}
		transitionAllowed := false
		for _, s := range allowed {
			if s == params.NewState {
				transitionAllowed = true
				break
			}
		}
		if !transitionAllowed {
			return domain.ErrInvalidCaseTransition
		}

		if current.CaseState == params.NewState {
			updated = current
			return nil
		}

		setClauses := []string{"case_state = $1", "updated_at = NOW()", "row_version = row_version + 1"}
		args := []any{params.NewState}
		argIdx := 2

		var eventType string
		now := time.Now().UTC()

		switch params.NewState {
		case domain.CaseStateTriaged:
			setClauses = append(setClauses, "triaged_at = $"+strconv.Itoa(argIdx))
			args = append(args, now)
			argIdx++
			eventType = "case.triaged"
		case domain.CaseStateInProgress:
			eventType = "case.started"
		case domain.CaseStateWaitingInternal:
			eventType = "case.waiting_internal"
		case domain.CaseStateWaitingExternal:
			eventType = "case.waiting_external"
		case domain.CaseStateResolved:
			setClauses = append(setClauses, "resolved_at = $"+strconv.Itoa(argIdx))
			args = append(args, now)
			argIdx++
			eventType = "case.resolved"
		case domain.CaseStateClosed:
			setClauses = append(setClauses, "closed_at = $"+strconv.Itoa(argIdx))
			args = append(args, now)
			argIdx++
			eventType = "case.closed"
		case domain.CaseStateReopened:
			eventType = "case.reopened"
		}

		args = append(args, params.CaseID, tenantID)
		updateQuery := `
			UPDATE cases
			SET ` + strings.Join(setClauses, ", ") + `
			WHERE case_id = $` + strconv.Itoa(len(args)-1) + ` AND tenant_id = $` + strconv.Itoa(len(args)) + `
			RETURNING ` + caseColumns + `;`

		updated, err = scanCase(tx.QueryRow(ctx, updateQuery, args...))
		if err != nil {
			if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		payload := map[string]any{
			"case_id":        updated.CaseID,
			"case_family":    updated.CaseFamily,
			"case_type":      updated.CaseType,
			"previous_state": current.CaseState,
			"new_state":      updated.CaseState,
			"row_version":    updated.RowVersion,
		}

		if params.Reason != nil && *params.Reason != "" {
			payload["reason"] = *params.Reason
		}

		switch params.NewState {
		case domain.CaseStateResolved:
			payload["resolved_by"] = params.ActorPrincipalID
		case domain.CaseStateClosed:
			payload["closed_by"] = params.ActorPrincipalID
		case domain.CaseStateReopened:
			payload["reopened_by"] = params.ActorPrincipalID
		}

		return insertOutboxEventTx(ctx, tx, tenantID, eventType, "case", updated.CaseID, params.CorrelationID,
			updated.LegalEntityID, "", params.ActorPrincipalID, payload)
	})
	if err != nil {
		return nil, false, err
	}
	return updated, updated != nil, nil
}

// =============================================================================
// WF-S24: Operator Repair (F30)
// =============================================================================

const operatorActionColumns = `
	operator_action_id, tenant_id, workflow_instance_id, kind, status,
	reason, scope, evidence_refs, requested_by, requested_at,
	approved_by, approved_at, executed_at, execution_result,
	created_at, updated_at, row_version`

func scanOperatorAction(row pgx.Row) (*domain.OperatorAction, error) {
	oa := &domain.OperatorAction{}
	var evidenceRefs []string
	var scope *string
	err := row.Scan(
		&oa.OperatorActionID,
		&oa.TenantID,
		&oa.WorkflowInstanceID,
		&oa.Kind,
		&oa.Status,
		&oa.Reason,
		&scope,
		&evidenceRefs,
		&oa.RequestedBy,
		&oa.RequestedAt,
		&oa.ApprovedBy,
		&oa.ApprovedAt,
		&oa.ExecutedAt,
		&oa.ExecutionResult,
		&oa.CreatedAt,
		&oa.UpdatedAt,
		&oa.RowVersion,
	)
	if scope != nil {
		oa.Scope = scope
	}
	oa.EvidenceRefs = evidenceRefs
	return oa, err
}

func (s *PgStore) CreateOperatorAction(ctx context.Context, params domain.CreateOperatorActionParams) (*domain.OperatorAction, error) {
	var oa *domain.OperatorAction
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		const insert = `
			INSERT INTO operator_actions (
				operator_action_id, tenant_id, workflow_instance_id, kind, status,
				reason, scope, evidence_refs, requested_by, requested_at
			) VALUES (gen_random_uuid(), $1, $2, $3, 'PENDING', $4, $5, $6, $7, NOW())
			RETURNING ` + operatorActionColumns + `;`
		var scanErr error
		oa, scanErr = scanOperatorAction(tx.QueryRow(ctx, insert,
			tenantID, params.WorkflowInstanceID, params.Kind, params.Reason,
			params.Scope, params.EvidenceRefs, params.ActorPrincipalID))
		if scanErr != nil {
			if errors.Is(mapPgError(scanErr), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}

		payload := map[string]any{
			"operator_action_id":   oa.OperatorActionID,
			"workflow_instance_id": oa.WorkflowInstanceID,
			"kind":                 oa.Kind,
			"reason":               oa.Reason,
			"requested_by":         oa.RequestedBy,
		}
		return s.outboxInsertTx(ctx, tx, "operator_action.created", "operator_action", oa.OperatorActionID, params.CorrelationID, payload, tenantID, "", params.ActorPrincipalID)
	})
	if err != nil {
		return nil, err
	}
	return oa, nil
}

func (s *PgStore) GetOperatorAction(ctx context.Context, operatorActionID string) (*domain.OperatorAction, error) {
	tenantID := tenantOf(ctx)
	if tenantID == "" {
		return nil, domain.ErrTenantMissing
	}

	const query = `SELECT ` + operatorActionColumns + ` FROM operator_actions WHERE operator_action_id = $1 AND tenant_id = $2;`

	var oa *domain.OperatorAction
	err := s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
		var scanErr error
		oa, scanErr = scanOperatorAction(tx.QueryRow(ctx, query, operatorActionID, tenantID))
		return scanErr
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrOperatorActionNotFound
		}
		if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
			return nil, domain.ErrInvalidIdentifier
		}
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return oa, nil
}

func (s *PgStore) ListOperatorActions(ctx context.Context, tenantID string, filter domain.ListOperatorActionsFilter) ([]*domain.OperatorAction, *domain.PaginationCursor, error) {
	if filter.TenantID != "" && filter.TenantID != tenantID {
		return nil, nil, domain.ErrTenantMissing
	}

	query := `SELECT ` + operatorActionColumns + ` FROM operator_actions WHERE tenant_id = $1`
	args := []any{tenantID}

	addFilter := func(clause string, value any) {
		args = append(args, value)
		query += fmt.Sprintf(" AND %s $%d", clause, len(args))
	}

	if filter.WorkflowInstanceID != nil {
		addFilter("workflow_instance_id =", *filter.WorkflowInstanceID)
	}
	if filter.Status != "" {
		addFilter("status =", filter.Status)
	}
	if filter.Kind != "" {
		addFilter("kind =", filter.Kind)
	}

	query += " ORDER BY requested_at DESC, operator_action_id DESC"

	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if filter.Cursor != nil {
		args = append(args, filter.Cursor.StartedAt)
		query += fmt.Sprintf(" AND (requested_at, operator_action_id) < ($%d, $%d)", len(args), len(args)+1)
		args = append(args, filter.Cursor.InstanceID)
	}
	args = append(args, limit+1)
	query += fmt.Sprintf(" LIMIT $%d;", len(args))

	var results []*domain.OperatorAction
	var err error
	err = s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
		rows, qErr := tx.Query(ctx, query, args...)
		if qErr != nil {
			if errors.Is(mapPgError(qErr), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			s.log.Error("pg ListOperatorActions failed", zap.Error(qErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, qErr)
		}
		defer rows.Close()

		for rows.Next() {
			oa, scanErr := scanOperatorAction(rows)
			if scanErr != nil {
				s.log.Error("pg ListOperatorActions scan failed", zap.Error(scanErr))
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
			}
			results = append(results, oa)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, nil, err
	}

	var nextCursor *domain.PaginationCursor
	if len(results) > limit {
		last := results[limit-1]
		results = results[:limit]
		nextCursor = &domain.PaginationCursor{
			StartedAt:  last.RequestedAt.UTC().Format(time.RFC3339),
			InstanceID: last.OperatorActionID,
		}
	}

	return results, nextCursor, nil
}

func (s *PgStore) ApproveOperatorAction(ctx context.Context, params domain.ApproveOperatorActionParams) (*domain.OperatorAction, error) {
	var oa *domain.OperatorAction
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		// Check if the action requires approval (high-risk kinds)
		const checkQuery = `SELECT kind, requested_by FROM operator_actions WHERE operator_action_id = $1 AND tenant_id = $2 FOR UPDATE;`
		var kind domain.OperatorActionKind
		var requestedBy string
		err := tx.QueryRow(ctx, checkQuery, params.OperatorActionID, tenantID).Scan(&kind, &requestedBy)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrOperatorActionNotFound
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		// High-risk kinds: FORCE_ESCALATE, FORCE_EXPIRE, RELEASE_CLAIM
		highRisk := kind == domain.OperatorActionForceEscalate || kind == domain.OperatorActionForceExpire || kind == domain.OperatorActionReleaseClaim
		if highRisk {
			if requestedBy == params.ActorPrincipalID {
				return domain.ErrOperatorActionRequesterCannotApprove
			}
		}

		const update = `
			UPDATE operator_actions
			SET status = 'APPROVED', approved_by = $1, approved_at = NOW(), updated_at = NOW(), row_version = row_version + 1
			WHERE operator_action_id = $2 AND tenant_id = $3
			RETURNING ` + operatorActionColumns + `;`
		var scanErr error
		oa, scanErr = scanOperatorAction(tx.QueryRow(ctx, update, params.ActorPrincipalID, params.OperatorActionID, tenantID))
		if scanErr != nil {
			if errors.Is(scanErr, pgx.ErrNoRows) {
				return domain.ErrOperatorActionNotFound
			}
			if errors.Is(mapPgError(scanErr), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}

		payload := map[string]any{
			"operator_action_id":   oa.OperatorActionID,
			"workflow_instance_id": oa.WorkflowInstanceID,
			"kind":                 oa.Kind,
			"approved_by":          oa.ApprovedBy,
		}
		return s.outboxInsertTx(ctx, tx, "operator_action.approved", "operator_action", oa.OperatorActionID, params.CorrelationID, payload, tenantID, "", params.ActorPrincipalID)
	})
	if err != nil {
		return nil, err
	}
	return oa, nil
}

func (s *PgStore) ExecuteOperatorAction(ctx context.Context, params domain.ExecuteOperatorActionParams) (*domain.OperatorAction, error) {
	var oa *domain.OperatorAction
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		const checkQuery = `SELECT status FROM operator_actions WHERE operator_action_id = $1 AND tenant_id = $2 FOR UPDATE;`
		var status domain.OperatorActionStatus
		err := tx.QueryRow(ctx, checkQuery, params.OperatorActionID, tenantID).Scan(&status)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrOperatorActionNotFound
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		if status != domain.OperatorActionStatusApproved {
			return fmt.Errorf("%w: operator action not approved", domain.ErrInvalidTransition)
		}

		const update = `
			UPDATE operator_actions
			SET status = 'EXECUTED', executed_at = NOW(), execution_result = $1, updated_at = NOW(), row_version = row_version + 1
			WHERE operator_action_id = $2 AND tenant_id = $3
			RETURNING ` + operatorActionColumns + `;`
		var scanErr error
		oa, scanErr = scanOperatorAction(tx.QueryRow(ctx, update, params.ExecutionResult, params.OperatorActionID, tenantID))
		if scanErr != nil {
			if errors.Is(scanErr, pgx.ErrNoRows) {
				return domain.ErrOperatorActionNotFound
			}
			if errors.Is(mapPgError(scanErr), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}

		// Execute the actual repair action based on kind
		if err := s.executeOperatorActionInternal(ctx, tx, oa, params.CorrelationID); err != nil {
			return err
		}

		payload := map[string]any{
			"operator_action_id":   oa.OperatorActionID,
			"workflow_instance_id": oa.WorkflowInstanceID,
			"kind":                 oa.Kind,
			"executed_by":          params.ActorPrincipalID,
			"execution_result":     params.ExecutionResult,
		}
		return s.outboxInsertTx(ctx, tx, "operator_action.executed", "operator_action", oa.OperatorActionID, params.CorrelationID, payload, tenantID, "", params.ActorPrincipalID)
	})
	if err != nil {
		return nil, err
	}
	return oa, nil
}

func (s *PgStore) executeOperatorActionInternal(ctx context.Context, tx pgx.Tx, oa *domain.OperatorAction, correlationID string) error {
	switch oa.Kind {
	case domain.OperatorActionRetryTimer:
		// Recompute next_action_at and update workflow instance
		const retryQuery = `
			UPDATE workflow_instances
			SET next_action_at = NOW() + interval '1 hour', updated_at = NOW(), row_version = row_version + 1
			WHERE workflow_instance_id = $1 AND tenant_id = $2;`
		_, err := tx.Exec(ctx, retryQuery, oa.WorkflowInstanceID, oa.TenantID)
		return err
	case domain.OperatorActionRecomputeNextAction:
		// Recompute next_action_at based on current stage
		const recomputeQuery = `
			UPDATE workflow_instances wi
			SET next_action_at = (
				SELECT due_at FROM workflow_stages ws
				WHERE ws.workflow_instance_id = wi.workflow_instance_id
				AND ws.stage_order = wi.current_stage
				AND ws.tenant_id = wi.tenant_id
			), updated_at = NOW(), row_version = row_version + 1
			WHERE wi.workflow_instance_id = $1 AND wi.tenant_id = $2;`
		_, err := tx.Exec(ctx, recomputeQuery, oa.WorkflowInstanceID, oa.TenantID)
		return err
	case domain.OperatorActionForceEscalate:
		// Force escalate the workflow
		const escalateQuery = `
			UPDATE workflow_instances
			SET workflow_status = 'ESCALATED', updated_at = NOW(), row_version = row_version + 1, escalated_at = NOW()
			WHERE workflow_instance_id = $1 AND tenant_id = $2;`
		_, err := tx.Exec(ctx, escalateQuery, oa.WorkflowInstanceID, oa.TenantID)
		return err
	case domain.OperatorActionForceExpire:
		// Force expire the workflow
		const expireQuery = `
			UPDATE workflow_instances
			SET workflow_status = 'EXPIRED', updated_at = NOW(), row_version = row_version + 1
			WHERE workflow_instance_id = $1 AND tenant_id = $2;`
		_, err := tx.Exec(ctx, expireQuery, oa.WorkflowInstanceID, oa.TenantID)
		return err
	case domain.OperatorActionRequeueParkedEvent:
		// Requeue parked outbox events
		const requeueQuery = `
			UPDATE outbox_events
			SET created_at = NOW(), headers = jsonb_set(headers, '{requeued}', 'true')
			WHERE aggregate_id = $1 AND aggregate_type = 'workflow_instance' AND headers->>'parked' = 'true';`
		_, err := tx.Exec(ctx, requeueQuery, oa.WorkflowInstanceID)
		return err
	case domain.OperatorActionReleaseClaim:
		// Release a claimed work item
		const releaseQuery = `
			UPDATE work_items
			SET state = 'UNASSIGNED', assignee_principal_id = NULL, updated_at = NOW(), row_version = row_version + 1
			WHERE workflow_instance_id = $1 AND tenant_id = $2 AND state = 'CLAIMED';`
		_, err := tx.Exec(ctx, releaseQuery, oa.WorkflowInstanceID, oa.TenantID)
		return err
	default:
		return fmt.Errorf("unknown operator action kind: %s", oa.Kind)
	}
}

// =============================================================================
// WF-S25: Emergency Approval (F31)
// =============================================================================

const emergencyApprovalColumns = `
	emergency_approval_id, tenant_id, legal_entity_id, workflow_instance_id, break_glass_session_id,
	window_start, window_end, reason_code, incident_ref, scope, max_impact,
	conditions, approved_by, approved_at, review_work_item_id,
	created_at, updated_at, row_version`

func scanEmergencyApproval(row pgx.Row) (*domain.EmergencyApproval, error) {
	ea := &domain.EmergencyApproval{}
	var conditions []string
	err := row.Scan(
		&ea.EmergencyApprovalID,
		&ea.TenantID,
		&ea.LegalEntityID,
		&ea.WorkflowInstanceID,
		&ea.BreakGlassSessionID,
		&ea.WindowStart,
		&ea.WindowEnd,
		&ea.ReasonCode,
		&ea.IncidentRef,
		&ea.Scope,
		&ea.MaxImpact,
		&conditions,
		&ea.ApprovedBy,
		&ea.ApprovedAt,
		&ea.ReviewWorkItemID,
		&ea.CreatedAt,
		&ea.UpdatedAt,
		&ea.RowVersion,
	)
	ea.Conditions = make([]domain.EmergencyCondition, len(conditions))
	for i, c := range conditions {
		ea.Conditions[i] = domain.EmergencyCondition(c)
	}
	return ea, err
}

func (s *PgStore) CreateEmergencyApproval(ctx context.Context, params domain.CreateEmergencyApprovalParams) (*domain.EmergencyApproval, error) {
	var ea *domain.EmergencyApproval
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		// Check if workflow instance exists and is in a valid state
		const checkQuery = `SELECT workflow_status, workflow_definition_id, legal_entity_id FROM workflow_instances WHERE workflow_instance_id = $1 AND tenant_id = $2;`
		var status, defID, legalEntityID string
		err := tx.QueryRow(ctx, checkQuery, params.WorkflowInstanceID, tenantID).Scan(&status, &defID, &legalEntityID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrWorkflowNotFound
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		if status != domain.WorkflowStatusPending && status != domain.WorkflowStatusEscalated {
			return fmt.Errorf("%w: workflow not in approvable state", domain.ErrInvalidTransition)
		}

		// Check if definition allows emergency approval
		const defQuery = `SELECT emergency_allowed, emergency_conditions FROM workflow_definitions WHERE workflow_definition_id = $1 AND tenant_id = $2;`
		var emergencyAllowed bool
		var emergencyConditions []string
		err = tx.QueryRow(ctx, defQuery, defID, tenantID).Scan(&emergencyAllowed, &emergencyConditions)
		if err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		if !emergencyAllowed {
			return domain.ErrProtectedTypeRefused
		}

		// Check if requested conditions are allowed by the definition
		for _, c := range params.Conditions {
			allowed := false
			for _, ec := range emergencyConditions {
				if ec == string(c) {
					allowed = true
					break
				}
			}
			if !allowed {
				return domain.ErrProtectedTypeRefused
			}
		}

		// Check break-glass session (placeholder - would call policy-svc in real implementation)
		if params.BreakGlassSessionID == "" {
			return domain.ErrBreakGlassSessionInvalid
		}

		const insert = `
			INSERT INTO emergency_approvals (
				emergency_approval_id, tenant_id, legal_entity_id, workflow_instance_id, break_glass_session_id,
				window_start, window_end, reason_code, incident_ref, scope, max_impact,
				conditions, approved_by, approved_at
			) VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, NOW())
			RETURNING ` + emergencyApprovalColumns + `;`
		var scanErr error
		ea, scanErr = scanEmergencyApproval(tx.QueryRow(ctx, insert,
			tenantID, ea.LegalEntityID, params.WorkflowInstanceID, params.BreakGlassSessionID,
			params.WindowStart, params.WindowEnd, params.ReasonCode,
			params.IncidentRef, params.Scope, params.MaxImpact,
			params.Conditions, params.ActorPrincipalID))
		if scanErr != nil {
			if errors.Is(mapPgError(scanErr), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		ea.LegalEntityID = legalEntityID

		// Create mandatory review work item
		reviewItem, err := s.CreateWorkItem(ctx, domain.CreateWorkItemParams{
			TenantID:           tenantID,
			LegalEntityID:      ea.LegalEntityID,
			WorkflowInstanceID: &ea.WorkflowInstanceID,
			ItemType:           "EMERGENCY_REVIEW",
			Mandatory:          true,
			CompletionSchema:   map[string]interface{}{"type": "object", "properties": map[string]interface{}{"approved": map[string]interface{}{"type": "boolean"}}},
			ActorPrincipalID:   params.ActorPrincipalID,
			CorrelationID:      params.CorrelationID,
			Meta:               params.Meta,
		})
		if err != nil {
			return err
		}

		// Update emergency approval with review work item ID
		const updateReview = `UPDATE emergency_approvals SET review_work_item_id = $1 WHERE emergency_approval_id = $2;`
		_, err = tx.Exec(ctx, updateReview, reviewItem.WorkItemID, ea.EmergencyApprovalID)
		if err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		ea.ReviewWorkItemID = &reviewItem.WorkItemID

		payload := map[string]any{
			"emergency_approval_id":  ea.EmergencyApprovalID,
			"workflow_instance_id":   ea.WorkflowInstanceID,
			"break_glass_session_id": ea.BreakGlassSessionID,
			"reason_code":            ea.ReasonCode,
			"conditions":             ea.Conditions,
			"approved_by":            ea.ApprovedBy,
			"review_work_item_id":    ea.ReviewWorkItemID,
		}
		return s.outboxInsertTx(ctx, tx, "workflow.emergency_approved", "workflow_instance", ea.WorkflowInstanceID, params.CorrelationID, payload, tenantID, "", params.ActorPrincipalID)
	})
	if err != nil {
		return nil, err
	}
	return ea, nil
}

func (s *PgStore) GetEmergencyApproval(ctx context.Context, emergencyApprovalID string) (*domain.EmergencyApproval, error) {
	tenantID := tenantOf(ctx)
	if tenantID == "" {
		return nil, domain.ErrTenantMissing
	}

	const query = `SELECT ` + emergencyApprovalColumns + ` FROM emergency_approvals WHERE emergency_approval_id = $1 AND tenant_id = $2;`

	var ea *domain.EmergencyApproval
	err := s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
		var scanErr error
		ea, scanErr = scanEmergencyApproval(tx.QueryRow(ctx, query, emergencyApprovalID, tenantID))
		return scanErr
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrEmergencyApprovalNotFound
		}
		if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
			return nil, domain.ErrInvalidIdentifier
		}
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return ea, nil
}

func (s *PgStore) ListEmergencyApprovals(ctx context.Context, tenantID string, filter domain.ListEmergencyApprovalsFilter) ([]*domain.EmergencyApproval, *domain.PaginationCursor, error) {
	if filter.TenantID != "" && filter.TenantID != tenantID {
		return nil, nil, domain.ErrTenantMissing
	}

	query := `SELECT ` + emergencyApprovalColumns + ` FROM emergency_approvals WHERE tenant_id = $1`
	args := []any{tenantID}

	addFilter := func(clause string, value any) {
		args = append(args, value)
		query += fmt.Sprintf(" AND %s $%d", clause, len(args))
	}

	if filter.WorkflowInstanceID != nil {
		addFilter("workflow_instance_id =", *filter.WorkflowInstanceID)
	}
	if filter.BreakGlassSessionID != nil {
		addFilter("break_glass_session_id =", *filter.BreakGlassSessionID)
	}

	query += " ORDER BY approved_at DESC, emergency_approval_id DESC"

	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if filter.Cursor != nil {
		args = append(args, filter.Cursor.StartedAt)
		query += fmt.Sprintf(" AND (approved_at, emergency_approval_id) < ($%d, $%d)", len(args), len(args)+1)
		args = append(args, filter.Cursor.InstanceID)
	}
	args = append(args, limit+1)
	query += fmt.Sprintf(" LIMIT $%d;", len(args))

	var results []*domain.EmergencyApproval
	var err error
	err = s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
		rows, qErr := tx.Query(ctx, query, args...)
		if qErr != nil {
			if errors.Is(mapPgError(qErr), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			s.log.Error("pg ListEmergencyApprovals failed", zap.Error(qErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, qErr)
		}
		defer rows.Close()

		for rows.Next() {
			ea, scanErr := scanEmergencyApproval(rows)
			if scanErr != nil {
				s.log.Error("pg ListEmergencyApprovals scan failed", zap.Error(scanErr))
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
			}
			results = append(results, ea)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, nil, err
	}

	var nextCursor *domain.PaginationCursor
	if len(results) > limit {
		last := results[limit-1]
		results = results[:limit]
		nextCursor = &domain.PaginationCursor{
			StartedAt:  last.ApprovedAt.UTC().Format(time.RFC3339),
			InstanceID: last.EmergencyApprovalID,
		}
	}

	return results, nextCursor, nil
}

func (s *PgStore) CheckBreakGlassSession(ctx context.Context, sessionID string) (bool, error) {
	// In a real implementation, this would call policy-svc to verify the break-glass session
	// For now, we just check if the session exists in our emergency_approvals table
	tenantID := tenantOf(ctx)
	if tenantID == "" {
		return false, domain.ErrTenantMissing
	}

	const query = `SELECT 1 FROM emergency_approvals WHERE break_glass_session_id = $1 AND tenant_id = $2;`
	var exists bool
	err := s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
		return tx.QueryRow(ctx, query, sessionID, tenantID).Scan(&exists)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return true, nil
}
