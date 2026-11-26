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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/domain"
	svcenvelope "zoiko.io/workflow-svc/internal/envelope"
	svcmiddleware "zoiko.io/workflow-svc/internal/middleware"
	"zoiko.io/workflow-svc/internal/notify"
	"zoiko.io/workflow-svc/internal/outbox"
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
	// notices, when set (WithNotices), makes each transition write its
	// approver and requester notices to the outbox (notices.go).
	notices *noticeSettings
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

	// A tenant-wide aggregate (a definition, an operator action) has no legal
	// entity: '' is written as NULL (000030), not cast to uuid and refused.
	const query = `
		INSERT INTO outbox_events (outbox_event_id, aggregate_type, aggregate_id, event_type,
			tenant_id, legal_entity_id, actor_id, correlation_id, headers, payload, destination, created_at)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, NULLIF($5, '')::uuid, $6, $7, '{}'::jsonb, $8, 'kafka', NOW());`

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
	residency_region, expiry_hours, escalation_target, definition_status, effective_from, effective_to,
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
	// The raw stage list is what a start from this definition expands
	// (DefinitionStages) and what the published checksum covers. Until 26 Nov
	// 2026 it was parsed into d.Stages only and StagesJSON stayed empty, so
	// every start from a definition was 400 invalid_definition_stages.
	d.StagesJSON = stagesJSON
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
// FindCommandReplay returns the decision an Idempotency-Key already committed
// on this instance (nil when it committed none), or ErrIdempotencyMismatch
// when the key was used with another body. Handlers ask it before checking
// If-Match: a retried decision carries the version it was first sent with,
// which is stale once that first attempt committed, so a genuine retry was
// refused 412 instead of replaying (found by the live test of 26 Nov 2026).
func (s *PgStore) FindCommandReplay(ctx context.Context, instanceID string, meta domain.CommandMeta) (*domain.CommandReplay, error) {
	if meta.IdempotencyKey == "" {
		return nil, nil
	}
	tenantID := svcmiddleware.TenantFromContext(ctx)
	var replay domain.CommandReplay
	var hash *string
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT t.acted_by, t.request_hash, t.stage_order, t.decision_id::text
			  FROM workflow_transitions t
			  JOIN workflow_instances i ON i.workflow_instance_id = t.workflow_instance_id
			 WHERE t.workflow_instance_id = $1 AND t.idempotency_key = $2
			   AND i.tenant_id = NULLIF($3, '')::uuid`,
			instanceID, meta.IdempotencyKey, tenantID).Scan(&replay.ActedBy, &hash, &replay.StageOrder, &replay.DecisionID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
			return nil, domain.ErrWorkflowNotFound
		}
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if hash != nil && meta.RequestHash != "" && *hash != meta.RequestHash {
		return nil, domain.ErrIdempotencyMismatch
	}
	return &replay, nil
}

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
			INSERT INTO workflow_instances (workflow_instance_id, tenant_id, legal_entity_id, workflow_type, initiated_by, correlation_id, subject_type, subject_id, subject_version, subject_fingerprint, idempotency_key, escalation_target, workflow_definition_id, workflow_definition_version, created_by, due_at, next_action_at, causation_decision_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $5, $15, $16, NULLIF($17, ''))
			ON CONFLICT (tenant_id, idempotency_key) WHERE idempotency_key IS NOT NULL AND idempotency_key != '' DO NOTHING
			RETURNING ` + instanceColumns + `;`
		row := tx.QueryRow(ctx, insertInstance, params.WorkflowInstanceID, params.TenantID, params.LegalEntityID, params.WorkflowType, params.InitiatedBy, params.CorrelationID, params.SubjectType, params.SubjectID, params.SubjectVersion, params.SubjectFingerprint, idemKey, params.EscalationTarget, definitionID, definitionVersion, params.DueAt, firstStageDue, params.CausationDecisionID)
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
			INSERT INTO workflow_instances (workflow_instance_id, tenant_id, legal_entity_id, workflow_type, initiated_by, correlation_id, subject_type, subject_id, subject_version, subject_fingerprint, escalation_target, workflow_definition_id, workflow_definition_version, created_by, due_at, next_action_at, causation_decision_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $5, $14, $15, NULLIF($16, ''))
			RETURNING ` + instanceColumns + `;`
		row := tx.QueryRow(ctx, insertInstance, params.WorkflowInstanceID, params.TenantID, params.LegalEntityID, params.WorkflowType, params.InitiatedBy, params.CorrelationID, params.SubjectType, params.SubjectID, params.SubjectVersion, params.SubjectFingerprint, params.EscalationTarget, definitionID, definitionVersion, params.DueAt, firstStageDue, params.CausationDecisionID)
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

	if b := params.Signatory; b != nil {
		if _, err := tx.Exec(ctx, `
			INSERT INTO workflow_signatory_bindings (workflow_instance_id, tenant_id, signatory_decision_id,
				signatory_policy_version_id, signatory_rule_id, document_hash, requirement)
			VALUES ($1, $2, $3, $4, $5, $6, $7);`,
			params.WorkflowInstanceID, params.TenantID, b.DecisionID, b.PolicyVersionID, b.RuleID, b.DocumentHash, []byte(b.Requirement)); err != nil {
			s.log.Error("pg CreateWorkflow: insert signatory binding failed", zap.Error(err))
			return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
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
	// Stage 1's approvers are told a request waits on them.
	if err := s.enqueueNoticesTx(ctx, tx, instance, notify.Input{Moment: notify.Started, Actor: instance.InitiatedBy},
		1, correlationID); err != nil {
		return nil, nil, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		s.log.Error("pg CreateWorkflow: commit failed", zap.Error(err))
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return instance, stages, true, nil
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
	// DL-2: the governance decision the workflow resolves is part of its body.
	existingCause := ""
	if existing.CausationDecisionID != nil {
		existingCause = *existing.CausationDecisionID
	}
	if existingCause != params.CausationDecisionID {
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
			expiry_hours, escalation_target, definition_status, effective_from, effective_to,
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
			description, created_by, created_at, is_active, expiry_hours, escalation_target, definition_status, checksum)
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
	// The requester, and whoever was due to decide, are told the approval no
	// longer applies. The reason is the narrative, else the reason code.
	invalidReason := personReason(params.Narrative)
	if invalidReason == "" {
		invalidReason = notify.Humanize(params.ReasonCode)
	}
	if err := s.enqueueNoticesTx(ctx, tx, updated, notify.Input{Moment: notify.Invalidated, Actor: params.ActorPrincipalID,
		Rationale: invalidReason}, current.CurrentStage, instanceCorrelationID); err != nil {
		return nil, false, err
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

	// Build cursor condition for keyset pagination
	var cursorCondition string
	var cursorArgs []any
	if params.Cursor != nil && params.Cursor.StartedAt != "" && params.Cursor.InstanceID != "" {
		cursorCondition = "AND (started_at, workflow_instance_id) < ($2, $3)"
		cursorArgs = []any{params.Cursor.StartedAt, params.Cursor.InstanceID}
	}

	query := fmt.Sprintf(`
		SELECT %s
		FROM workflow_instances
		WHERE tenant_id = $1
		  AND workflow_status IN ('PENDING','ESCALATED')
		  %s
		ORDER BY started_at DESC, workflow_instance_id DESC
		LIMIT $%d`,
		instanceColumns, cursorCondition, len(cursorArgs)+2)

	args := make([]any, 0, 1+len(cursorArgs)+1)
	args = append(args, tenantID)
	args = append(args, cursorArgs...)
	args = append(args, limit+1) // Fetch one extra to determine if there's a next page

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.log.Error("pg ListPendingApprovals failed", zap.Error(err))
		return nil, nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return nil, nil, fmt.Errorf("%w: set_config app.tenant_id: %v", domain.ErrStoreUnavailable, err)
	}

	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		s.log.Error("pg ListPendingApprovals failed", zap.Error(err))
		return nil, nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	var instances []*domain.WorkflowInstance
	for rows.Next() {
		inst, err := scanInstance(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		instances = append(instances, inst)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	// Determine next cursor
	var nextCursor *domain.PaginationCursor
	if len(instances) > limit {
		// We fetched limit+1 rows, so there's a next page
		last := instances[limit-1] // The last item of the actual page
		instances = instances[:limit]
		nextCursor = &domain.PaginationCursor{
			StartedAt:  last.StartedAt.UTC().Format(time.RFC3339Nano),
			InstanceID: last.WorkflowInstanceID,
		}
	}

	return instances, nextCursor, nil
}

// =============================================================================
// WFC-04: Cases (WF-2)
// =============================================================================

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

	// The keyset clause belongs to WHERE, before ORDER BY: appended after
	// it, the next_cursor of page 1 produced a syntax error on page 2.
	if params.Cursor != nil && params.Cursor.StartedAt != "" && params.Cursor.InstanceID != "" {
		args = append(args, params.Cursor.StartedAt, params.Cursor.InstanceID)
		query += " AND (started_at, workflow_instance_id) < ($" + fmt.Sprint(len(args)-1) + ", $" + fmt.Sprint(len(args)) + ")"
	}
	query += " ORDER BY started_at DESC, workflow_instance_id DESC"
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
			StartedAt:  last.StartedAt.UTC().Format(time.RFC3339Nano),
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

// The definition lifecycle follows R-001 §3.2:
//
//	DRAFT → (validate) → VALIDATED → (submit-approval) → APPROVAL_PENDING
//	      → (approve, by a checker other than the author) → APPROVAL_PENDING + approved_by
//	      → (publish) → PUBLISHED; (reject) → REJECTED → DRAFT
//
// VALIDATION_PENDING is the state while validation runs; validation here is
// synchronous, so validate passes through it and records VALIDATED (or refuses
// with ErrDefinitionInvalid and leaves the row as it was). Until 26 Nov 2026
// validate stopped at VALIDATION_PENDING and approve moved the row to
// VALIDATED, so submit-approval → approve → publish could never reach
// PUBLISHED; and every command's event was written with legal_entity_id ''
// and refused (503).

func (s *PgStore) ValidateDefinition(ctx context.Context, params domain.ValidateDefinitionParams) (*domain.WorkflowDefinition, error) {
	return s.changeDefinition(ctx, definitionCommand{
		definitionID: params.WorkflowDefinitionID, actor: params.ActorPrincipalID, correlationID: params.CorrelationID,
		from: []string{"DRAFT", "VALIDATION_PENDING"}, to: "VALIDATED", event: "workflow.definition.validated",
		check: func(def *domain.WorkflowDefinition) error { return validateDefinitionGates(def) },
	})
}

func (s *PgStore) SubmitDefinitionApproval(ctx context.Context, params domain.SubmitDefinitionApprovalParams) (*domain.WorkflowDefinition, error) {
	return s.changeDefinition(ctx, definitionCommand{
		definitionID: params.WorkflowDefinitionID, actor: params.ActorPrincipalID, correlationID: params.CorrelationID,
		from: []string{"VALIDATED"}, to: "APPROVAL_PENDING", event: "workflow.definition.approval_requested",
	})
}

// ApproveDefinition records the checker's approval. The row stays
// APPROVAL_PENDING (R-001 has no APPROVED state); publish needs it.
func (s *PgStore) ApproveDefinition(ctx context.Context, params domain.ApproveDefinitionParams) (*domain.WorkflowDefinition, error) {
	return s.changeDefinition(ctx, definitionCommand{
		definitionID: params.WorkflowDefinitionID, actor: params.ActorPrincipalID, correlationID: params.CorrelationID,
		from: []string{"APPROVAL_PENDING"}, to: "APPROVAL_PENDING", event: "workflow.definition.approved", approve: true,
		reasonCode: params.ReasonCode,
		check: func(def *domain.WorkflowDefinition) error {
			if def.CreatedBy == params.ActorPrincipalID {
				return fmt.Errorf("%w: the author of a definition cannot approve it", domain.ErrSelfApprovalNotAllowed)
			}
			return nil
		},
	})
}

func (s *PgStore) RejectDefinition(ctx context.Context, params domain.RejectDefinitionParams) (*domain.WorkflowDefinition, error) {
	return s.changeDefinition(ctx, definitionCommand{
		definitionID: params.WorkflowDefinitionID, actor: params.ActorPrincipalID, correlationID: params.CorrelationID,
		from: []string{"APPROVAL_PENDING"}, to: "REJECTED", event: "workflow.definition.rejected", reasonCode: params.ReasonCode,
		check: func(def *domain.WorkflowDefinition) error {
			if def.CreatedBy == params.ActorPrincipalID {
				return fmt.Errorf("%w: the author of a definition cannot decide it", domain.ErrSelfApprovalNotAllowed)
			}
			return nil
		},
	})
}

// PublishDefinition publishes an approved definition, or resumes a suspended
// one (SUSPENDED → PUBLISHED, R-001 §3.2).
func (s *PgStore) PublishDefinition(ctx context.Context, params domain.PublishDefinitionParams) (*domain.WorkflowDefinition, error) {
	return s.changeDefinition(ctx, definitionCommand{
		definitionID: params.WorkflowDefinitionID, actor: params.ActorPrincipalID, correlationID: params.CorrelationID,
		from: []string{"APPROVAL_PENDING", "SUSPENDED"}, to: "PUBLISHED", event: "workflow.definition.published", publish: true,
		effectiveFrom: params.EffectiveFrom, effectiveTo: params.EffectiveTo,
		check: func(def *domain.WorkflowDefinition) error {
			if def.Status != nil && *def.Status == "APPROVAL_PENDING" && def.ApprovedBy == nil {
				return fmt.Errorf("%w: the definition has not been approved", domain.ErrInvalidTransition)
			}
			return nil
		},
	})
}

func (s *PgStore) SuspendDefinition(ctx context.Context, params domain.SuspendDefinitionParams) (*domain.WorkflowDefinition, error) {
	return s.changeDefinition(ctx, definitionCommand{
		definitionID: params.WorkflowDefinitionID, actor: params.ActorPrincipalID, correlationID: params.CorrelationID,
		from: []string{"PUBLISHED"}, to: "SUSPENDED", event: "workflow.definition.suspended",
	})
}

func (s *PgStore) RetireDefinition(ctx context.Context, params domain.RetireDefinitionParams) (*domain.WorkflowDefinition, error) {
	return s.changeDefinition(ctx, definitionCommand{
		definitionID: params.WorkflowDefinitionID, actor: params.ActorPrincipalID, correlationID: params.CorrelationID,
		from: []string{"DRAFT", "PUBLISHED", "SUSPENDED", "SUPERSEDED"}, to: "RETIRED", event: "workflow.definition.retired",
	})
}

// validateDefinitionGates are the static checks of R-001 §3.3 that a
// definition must pass before it can be submitted for approval: a stage list
// that parses, every stage with someone to decide it, a quorum no larger than
// its pool, and no negative deadline.
func validateDefinitionGates(def *domain.WorkflowDefinition) error {
	stages, err := DefinitionStages(def)
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrDefinitionInvalid, err)
	}
	for i, st := range stages {
		switch st.StageType {
		case domain.StageTypeSingle:
			if st.ApproverPrincipalID == "" {
				return fmt.Errorf("%w: stage %d has no approver_principal_id", domain.ErrDefinitionInvalid, i+1)
			}
		case domain.StageTypeQuorum:
			if len(st.QuorumApprovers) == 0 || st.RequiredApprovals < 1 || st.RequiredApprovals > len(st.QuorumApprovers) {
				return fmt.Errorf("%w: stage %d needs 1..%d required_approvals from its quorum_approvers", domain.ErrDefinitionInvalid, i+1, len(st.QuorumApprovers))
			}
		}
		if st.DueAtHours < 0 {
			return fmt.Errorf("%w: stage %d has a negative due_at_hours", domain.ErrDefinitionInvalid, i+1)
		}
	}
	return nil
}

// definitionCommand is one lifecycle command on a definition.
type definitionCommand struct {
	definitionID, actor, correlationID string
	from                               []string
	to, event, reasonCode              string
	approve, publish                   bool
	effectiveFrom, effectiveTo         *time.Time
	// check runs on the locked row before the change; its error refuses it.
	check func(*domain.WorkflowDefinition) error
}

// changeDefinition applies cmd under a row lock and writes its event in the
// same transaction. A definition already where the command would leave it is
// an idempotent no-op (no second event).
func (s *PgStore) changeDefinition(ctx context.Context, cmd definitionCommand) (*domain.WorkflowDefinition, error) {
	var updated *domain.WorkflowDefinition

	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		const lockQuery = `SELECT ` + definitionColumns + ` FROM workflow_definitions WHERE workflow_definition_id = $1 AND tenant_id = $2 FOR UPDATE;`
		current, err := scanDefinition(tx.QueryRow(ctx, lockQuery, cmd.definitionID, tenantID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) || errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
				return domain.ErrWorkflowNotFound
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		currentStatus := ""
		if current.Status != nil {
			currentStatus = *current.Status
		}

		// Idempotent replays: already approved by this checker, or already in
		// the target state of a state-changing command.
		if cmd.approve && current.ApprovedBy != nil && currentStatus == "APPROVAL_PENDING" {
			if *current.ApprovedBy == cmd.actor {
				updated = current
				return nil
			}
			return fmt.Errorf("%w: the definition was already approved by another checker", domain.ErrInvalidTransition)
		}
		if !cmd.approve && currentStatus == cmd.to {
			updated = current
			return nil
		}
		allowed := false
		for _, f := range cmd.from {
			if f == currentStatus {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("%w: cannot move a definition from %s to %s", domain.ErrInvalidTransition, currentStatus, cmd.to)
		}
		if cmd.check != nil {
			if err := cmd.check(current); err != nil {
				return err
			}
		}

		now := time.Now().UTC()
		setClauses := []string{"definition_status = $1", "updated_at = NOW()", "row_version = row_version + 1"}
		args := []any{cmd.to}
		add := func(column string, value any) {
			args = append(args, value)
			setClauses = append(setClauses, column+" = $"+strconv.Itoa(len(args)))
		}
		switch {
		case cmd.to == "VALIDATED":
			add("validated_by", cmd.actor)
			add("validated_at", now)
		case cmd.approve:
			add("approved_by", cmd.actor)
			add("approved_at", now)
		case cmd.to == "REJECTED":
			// A rejected definition goes back through validation and approval.
			setClauses = append(setClauses, "approved_by = NULL", "approved_at = NULL")
		case cmd.publish && currentStatus == "APPROVAL_PENDING":
			from := now
			if cmd.effectiveFrom != nil {
				from = cmd.effectiveFrom.UTC()
			}
			add("published_by", cmd.actor)
			add("published_at", now)
			add("effective_from", from)
			add("effective_to", cmd.effectiveTo)
			add("checksum", computeChecksum(current.WorkflowType, current.Version, current.StagesJSON))
		}
		args = append(args, cmd.definitionID, tenantID)
		updateQuery := `
			UPDATE workflow_definitions
			SET ` + strings.Join(setClauses, ", ") + `
			WHERE workflow_definition_id = $` + strconv.Itoa(len(args)-1) + ` AND tenant_id = $` + strconv.Itoa(len(args)) + `
			RETURNING ` + definitionColumns + `;`
		updated, err = scanDefinition(tx.QueryRow(ctx, updateQuery, args...))
		if err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		payload := map[string]any{
			"workflow_definition_id": updated.WorkflowDefinitionID,
			"workflow_type":          updated.WorkflowType,
			"version":                updated.Version,
			"previous_status":        currentStatus,
			"new_status":             cmd.to,
			"actor_principal_id":     cmd.actor,
		}
		if cmd.reasonCode != "" {
			payload["reason_code"] = cmd.reasonCode
		}
		if updated.Checksum != nil {
			payload["checksum"] = *updated.Checksum
		}
		// A definition belongs to the tenant, not to one legal entity: its
		// event carries no entity (NULL, 000030).
		return s.outboxInsertTx(ctx, tx, cmd.event, "workflow_definition", updated.WorkflowDefinitionID, cmd.correlationID, payload, tenantID, "", cmd.actor)
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
	source_type,
	source_id,
	milestone,
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
		&c.SourceType,
		&c.SourceID,
		&c.Milestone,
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
func (s *PgStore) CreateCase(ctx context.Context, params domain.CreateCaseParams) (*domain.Case, bool, error) {
	if params.WorkflowInstanceID != nil {
		_, err := s.FindWorkflowByID(ctx, *params.WorkflowInstanceID)
		if err != nil {
			return nil, false, err
		}
	}
	if params.Severity == "" {
		params.Severity = domain.CaseSeverityMedium
	}
	if params.Confidentiality == "" {
		params.Confidentiality = domain.CaseConfidentialityInternal
	}

	// A case opened from a source milestone is opened once: a retry (the
	// obligations-svc relay is at-least-once) finds the case it opened.
	const query = `
		INSERT INTO cases (
			case_id, tenant_id, legal_entity_id, workflow_instance_id,
			case_family, case_type, case_state, severity, confidentiality,
			title, description, assigned_to_principal_id, created_by_principal_id,
			source_type, source_id, milestone
		) VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, 'OPEN', $6, $7, $8, $9, $10, $11,
			NULLIF($12, ''), NULLIF($13, ''), NULLIF($14, ''))
		ON CONFLICT (tenant_id, source_type, source_id, (COALESCE(milestone, ''))) WHERE source_type IS NOT NULL DO NOTHING
		RETURNING ` + caseColumns + `;`
	const existing = `SELECT ` + caseColumns + ` FROM cases
		WHERE tenant_id = $1 AND source_type = $2 AND source_id = $3 AND COALESCE(milestone, '') = $4;`

	var c *domain.Case
	created := false
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		if params.TenantID != tenantID {
			return domain.ErrTenantMissing
		}

		var scanErr error
		c, scanErr = scanCase(tx.QueryRow(ctx, query,
			tenantID, params.LegalEntityID, params.WorkflowInstanceID,
			params.CaseFamily, params.CaseType, params.Severity, params.Confidentiality, params.Title, params.Description,
			params.AssignedToPrincipal, params.CreatedByPrincipalID,
			params.SourceType, params.SourceID, params.Milestone))
		if errors.Is(scanErr, pgx.ErrNoRows) && params.SourceType != "" {
			c, scanErr = scanCase(tx.QueryRow(ctx, existing, tenantID, params.SourceType, params.SourceID, params.Milestone))
			return scanErr
		}
		if scanErr != nil {
			if errors.Is(mapPgError(scanErr), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		created = true

		return insertOutboxEventTx(ctx, tx, tenantID, "case.created", "case", c.CaseID, params.CorrelationID,
			params.LegalEntityID, "", params.CreatedByPrincipalID, map[string]any{
				"case_id":              c.CaseID,
				"case_family":          c.CaseFamily,
				"case_type":            c.CaseType,
				"case_state":           string(c.CaseState),
				"severity":             c.Severity,
				"confidentiality":      c.Confidentiality,
				"workflow_instance_id": c.WorkflowInstanceID,
				"source_type":          c.SourceType,
				"source_id":            c.SourceID,
				"milestone":            c.Milestone,
				"row_version":          c.RowVersion,
			})
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, fmt.Errorf("%w: source case vanished between insert and read", domain.ErrStoreUnavailable)
		}
		return nil, false, err
	}
	return c, created, nil
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
			return nil, domain.ErrWorkItemNotFound
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

	if filter.LegalEntityID != "" {
		addFilter("legal_entity_id =", filter.LegalEntityID)
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

	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	// The cursor is a WHERE predicate, so it goes before ORDER BY; appended
	// after it, every second page was a syntax error.
	if filter.Cursor != nil {
		args = append(args, filter.Cursor.StartedAt)
		query += fmt.Sprintf(" AND (created_at, work_item_id) < ($%d::timestamptz, $%d::uuid)", len(args), len(args)+1)
		args = append(args, filter.Cursor.InstanceID)
	}
	query += " ORDER BY created_at DESC, work_item_id DESC"
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
			StartedAt:  last.CreatedAt.UTC().Format(time.RFC3339Nano),
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
				return domain.ErrWorkItemNotFound
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
			return fmt.Errorf("%w: work item not in claimable state", domain.ErrInvalidWorkItemTransition)
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
				return domain.ErrWorkItemNotFound
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
			return fmt.Errorf("%w: work item is not mandatory", domain.ErrInvalidWorkItemTransition)
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
				return domain.ErrWorkItemNotFound
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
				return domain.ErrWorkItemNotFound
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
			return fmt.Errorf("%w: work item not unassigned", domain.ErrInvalidWorkItemTransition)
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
				return domain.ErrWorkItemNotFound
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
			return fmt.Errorf("%w: work item not claimed", domain.ErrInvalidWorkItemTransition)
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
				return domain.ErrWorkItemNotFound
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
			return fmt.Errorf("%w: work item already terminal", domain.ErrInvalidWorkItemTransition)
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
				return domain.ErrWorkItemNotFound
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
				return domain.ErrWorkItemNotFound
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
			return fmt.Errorf("%w: work item not claimed", domain.ErrInvalidWorkItemTransition)
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
			StartedAt:  last.RequestedAt.UTC().Format(time.RFC3339Nano),
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
			StartedAt:  last.ApprovedAt.UTC().Format(time.RFC3339Nano),
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
