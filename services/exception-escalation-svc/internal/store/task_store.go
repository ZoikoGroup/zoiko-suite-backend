package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/exception-escalation-svc/internal/domain"
	"zoiko.io/exception-escalation-svc/internal/middleware"
)

// TaskStore is BIZ-05's own persistence contract — kept separate from
// the existing Store/FindingStore interfaces, same composition pattern
// used for AUD-08's FindingStore in this same service.
type TaskStore interface {
	CreateCase(ctx context.Context, p domain.CreateCaseParams) (*domain.Case, error)
	GetCase(ctx context.Context, caseID string) (*domain.Case, error)
	CloseCase(ctx context.Context, p domain.CloseCaseParams) (*domain.Case, error)

	CreateTask(ctx context.Context, p domain.CreateTaskParams) (*domain.Task, error)
	GetTask(ctx context.Context, taskID string) (*domain.Task, error)
	AssignTask(ctx context.Context, p domain.AssignTaskParams) (*domain.Task, error)
	StartTask(ctx context.Context, p domain.StartTaskParams) (*domain.Task, error)
	GetTaskHistory(ctx context.Context, tenantID, taskID string) ([]domain.TaskTransition, error)

	BlockTask(ctx context.Context, p domain.BlockTaskParams) (*domain.Task, error)
	EscalateTask(ctx context.Context, p domain.EscalateTaskParams) (*domain.Task, error)
	CompleteTask(ctx context.Context, p domain.CompleteTaskParams) (*domain.Task, error)
	ReopenTask(ctx context.Context, p domain.ReopenTaskParams) (*domain.Task, error)
	CancelTask(ctx context.Context, p domain.CancelTaskParams) (*domain.Task, error)
	ListQueue(ctx context.Context, p domain.ListQueueParams, limit, offset int) ([]domain.Task, error)
	GetSLAState(ctx context.Context, tenantID, taskID string) (*domain.SLAState, error)
	GetLinkedObjectStatus(ctx context.Context, tenantID, taskID string) (*domain.LinkedObjectStatus, error)
}

func (s *PgStore) taskSetRLS(ctx context.Context, tx pgx.Tx) error {
	tenantID := middleware.GetTenantID(ctx)
	_, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID)
	return err
}

const caseColumns = `case_id, tenant_id, legal_entity_id, case_type, purpose, status, created_by, created_at, closed_by, closed_at, closure_reason`

func scanCase(row pgx.Row) (*domain.Case, error) {
	c := &domain.Case{}
	err := row.Scan(&c.CaseID, &c.TenantID, &c.LegalEntityID, &c.CaseType, &c.Purpose, &c.Status,
		&c.CreatedBy, &c.CreatedAt, &c.ClosedBy, &c.ClosedAt, &c.ClosureReason)
	return c, err
}

const taskColumns = `task_id, tenant_id, legal_entity_id, case_id, task_type, priority, business_trigger,
	linked_object_type, linked_object_id, required_evidence, status, assigned_to_role, assigned_to_user,
	sla_deadline, blocked_reason, escalated_to_role, escalated_at, completion_notes, completed_at,
	closed_by, closed_at, cancel_reason, cancelled_at, reopened_count, created_by, created_at, updated_at`

func scanTask(row pgx.Row) (*domain.Task, error) {
	t := &domain.Task{}
	err := row.Scan(&t.TaskID, &t.TenantID, &t.LegalEntityID, &t.CaseID, &t.TaskType, &t.Priority, &t.BusinessTrigger,
		&t.LinkedObjectType, &t.LinkedObjectID, &t.RequiredEvidence, &t.Status, &t.AssignedToRole, &t.AssignedToUser,
		&t.SLADeadline, &t.BlockedReason, &t.EscalatedToRole, &t.EscalatedAt, &t.CompletionNotes, &t.CompletedAt,
		&t.ClosedBy, &t.ClosedAt, &t.CancelReason, &t.CancelledAt, &t.ReopenedCount, &t.CreatedBy, &t.CreatedAt, &t.UpdatedAt)
	return t, err
}

// CreateCase creates a new OPEN case — BIZ-05's own CreateCase command
// (a gap-fill; see internal/domain/task.go's own doc comment on why).
func (s *PgStore) CreateCase(ctx context.Context, p domain.CreateCaseParams) (*domain.Case, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.taskSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	caseID := "case-" + uuid.New().String()
	row := tx.QueryRow(ctx, `INSERT INTO cases (case_id, tenant_id, legal_entity_id, case_type, purpose, created_by)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING `+caseColumns,
		caseID, p.TenantID, p.LegalEntityID, p.CaseType, p.Purpose, p.CreatedByPrincipalID)
	c, err := scanCase(row)
	if err != nil {
		return nil, fmt.Errorf("insert case: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (s *PgStore) GetCase(ctx context.Context, caseID string) (*domain.Case, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.taskSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	c, err := scanCase(tx.QueryRow(ctx, `SELECT `+caseColumns+` FROM cases WHERE case_id=$1 AND tenant_id=$2`, caseID, middleware.GetTenantID(ctx)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrCaseNotFound
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

// CloseCase — BIZ-05's own CloseCase command.
func (s *PgStore) CloseCase(ctx context.Context, p domain.CloseCaseParams) (*domain.Case, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.taskSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	current, err := scanCase(tx.QueryRow(ctx, `SELECT `+caseColumns+` FROM cases WHERE case_id=$1 AND tenant_id=$2 FOR UPDATE`, p.CaseID, p.TenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrCaseNotFound
	}
	if err != nil {
		return nil, err
	}
	if current.Status == domain.WorkCaseClosed {
		return nil, domain.ErrCaseAlreadyClosedWC
	}
	c, err := scanCase(tx.QueryRow(ctx, `
		UPDATE cases SET status='CLOSED', closed_by=$3, closed_at=now(), closure_reason=$4
		WHERE case_id=$1 AND tenant_id=$2 RETURNING `+caseColumns,
		p.CaseID, p.TenantID, p.ActorPrincipalID, p.ClosureReason))
	if err != nil {
		return nil, err
	}

	// Decision, not a silent guess: the doc's command list names no
	// task-level "Close" command (only CloseCase) even though the
	// lifecycle diagram shows Completed -> Closed as its own step. Rather
	// than invent an unnamed command, CLOSED is reached for a task only
	// as this cascade: closing a case administratively closes every
	// COMPLETED task that belongs to it. A task with no case, or one
	// still short of COMPLETED when its case closes, never reaches
	// CLOSED — it stays at whatever state it was actually in, which is
	// the honest record of what happened, not a fabricated completion.
	rows, err := tx.Query(ctx, `SELECT task_id, status FROM tasks WHERE case_id=$1 AND tenant_id=$2 AND status='COMPLETED' FOR UPDATE`, p.CaseID, p.TenantID)
	if err != nil {
		return nil, err
	}
	var completedTaskIDs []string
	for rows.Next() {
		var taskID, status string
		if err := rows.Scan(&taskID, &status); err != nil {
			rows.Close()
			return nil, err
		}
		completedTaskIDs = append(completedTaskIDs, taskID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for _, taskID := range completedTaskIDs {
		if _, err := tx.Exec(ctx, `UPDATE tasks SET status='CLOSED', closed_by=$3, closed_at=now(), updated_at=now() WHERE task_id=$1 AND tenant_id=$2`,
			taskID, p.TenantID, p.ActorPrincipalID); err != nil {
			return nil, err
		}
		if err := recordTaskTransition(ctx, tx, taskID, p.TenantID, string(domain.TaskStatusCompleted), string(domain.TaskStatusClosed), p.ActorPrincipalID, "case closed"); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// recordTaskTransition inserts one append-only transition row within the
// caller's own transaction — every task-mutating command calls this
// alongside its own UPDATE, in the same transaction, so the history and
// the current state can never drift apart.
func recordTaskTransition(ctx context.Context, tx pgx.Tx, taskID, tenantID, fromStatus, toStatus, actorPrincipalID, reason string) error {
	_, err := tx.Exec(ctx, `INSERT INTO task_transitions (transition_id, task_id, tenant_id, from_status, to_status, actor_principal_id, reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		"tasktr-"+uuid.New().String(), taskID, tenantID, fromStatus, toStatus, actorPrincipalID, reason)
	return err
}

// CreateTask — BIZ-05's own CreateTask command. Lands NEW.
func (s *PgStore) CreateTask(ctx context.Context, p domain.CreateTaskParams) (*domain.Task, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.taskSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	taskID := "task-" + uuid.New().String()
	priority := p.Priority
	if priority == "" {
		priority = domain.TaskPriorityMedium
	}
	var caseID any
	if p.CaseID != "" {
		caseID = p.CaseID
	}
	row := tx.QueryRow(ctx, `INSERT INTO tasks (
			task_id, tenant_id, legal_entity_id, case_id, task_type, priority, business_trigger,
			linked_object_type, linked_object_id, required_evidence, sla_deadline, assigned_to_role, assigned_to_user, created_by
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) RETURNING `+taskColumns,
		taskID, p.TenantID, p.LegalEntityID, caseID, p.TaskType, string(priority), p.BusinessTrigger,
		p.LinkedObjectType, p.LinkedObjectID, p.RequiredEvidence, p.SLADeadline, p.AssignedToRole, p.AssignedToUser, p.CreatedByPrincipalID)
	t, err := scanTask(row)
	if err != nil {
		return nil, fmt.Errorf("insert task: %w", err)
	}
	if err := recordTaskTransition(ctx, tx, taskID, p.TenantID, "", string(domain.TaskStatusNew), p.CreatedByPrincipalID, "task created"); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return t, nil
}

func (s *PgStore) GetTask(ctx context.Context, taskID string) (*domain.Task, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.taskSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	t, err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE task_id=$1 AND tenant_id=$2`, taskID, middleware.GetTenantID(ctx)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrTaskNotFound
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

// AssignTask — BIZ-05's own Assign command. Valid from NEW or ASSIGNED
// (reassignment) — moves to ASSIGNED either way.
func (s *PgStore) AssignTask(ctx context.Context, p domain.AssignTaskParams) (*domain.Task, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.taskSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	current, err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE task_id=$1 AND tenant_id=$2 FOR UPDATE`, p.TaskID, p.TenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrTaskNotFound
	}
	if err != nil {
		return nil, err
	}
	if current.Status != domain.TaskStatusNew && current.Status != domain.TaskStatusAssigned {
		return nil, domain.ErrTaskInvalidState
	}
	t, err := scanTask(tx.QueryRow(ctx, `
		UPDATE tasks SET status='ASSIGNED', assigned_to_role=$3, assigned_to_user=$4, updated_at=now()
		WHERE task_id=$1 AND tenant_id=$2 RETURNING `+taskColumns,
		p.TaskID, p.TenantID, p.AssignedToRole, p.AssignedToUser))
	if err != nil {
		return nil, err
	}
	if err := recordTaskTransition(ctx, tx, p.TaskID, p.TenantID, string(current.Status), string(domain.TaskStatusAssigned), p.ActorPrincipalID, "assigned"); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return t, nil
}

// StartTask — BIZ-05's own Start command. Valid from ASSIGNED only — an
// unassigned task has no one to start work on it.
func (s *PgStore) StartTask(ctx context.Context, p domain.StartTaskParams) (*domain.Task, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.taskSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	current, err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE task_id=$1 AND tenant_id=$2 FOR UPDATE`, p.TaskID, p.TenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrTaskNotFound
	}
	if err != nil {
		return nil, err
	}
	if current.Status != domain.TaskStatusAssigned {
		return nil, domain.ErrTaskInvalidState
	}
	t, err := scanTask(tx.QueryRow(ctx, `
		UPDATE tasks SET status='IN_PROGRESS', updated_at=now()
		WHERE task_id=$1 AND tenant_id=$2 RETURNING `+taskColumns,
		p.TaskID, p.TenantID))
	if err != nil {
		return nil, err
	}
	if err := recordTaskTransition(ctx, tx, p.TaskID, p.TenantID, string(current.Status), string(domain.TaskStatusInProgress), p.ActorPrincipalID, "started"); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return t, nil
}

// GetTaskHistory — BIZ-05's own GetHistory query.
func (s *PgStore) GetTaskHistory(ctx context.Context, tenantID, taskID string) ([]domain.TaskTransition, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.taskSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT transition_id, task_id, tenant_id, from_status, to_status, actor_principal_id, reason, occurred_at
		FROM task_transitions WHERE task_id=$1 AND tenant_id=$2 ORDER BY occurred_at ASC`, taskID, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.TaskTransition
	for rows.Next() {
		var tr domain.TaskTransition
		if err := rows.Scan(&tr.TransitionID, &tr.TaskID, &tr.TenantID, &tr.FromStatus, &tr.ToStatus, &tr.ActorPrincipalID, &tr.Reason, &tr.OccurredAt); err != nil {
			return nil, err
		}
		out = append(out, tr)
	}
	return out, rows.Err()
}

// BlockTask — BIZ-05's own Block command. Valid from IN_PROGRESS only.
func (s *PgStore) BlockTask(ctx context.Context, p domain.BlockTaskParams) (*domain.Task, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.taskSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	current, err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE task_id=$1 AND tenant_id=$2 FOR UPDATE`, p.TaskID, p.TenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrTaskNotFound
	}
	if err != nil {
		return nil, err
	}
	if current.Status != domain.TaskStatusInProgress {
		return nil, domain.ErrTaskInvalidState
	}
	t, err := scanTask(tx.QueryRow(ctx, `
		UPDATE tasks SET status='BLOCKED', blocked_reason=$3, updated_at=now()
		WHERE task_id=$1 AND tenant_id=$2 RETURNING `+taskColumns,
		p.TaskID, p.TenantID, p.Reason))
	if err != nil {
		return nil, err
	}
	if err := recordTaskTransition(ctx, tx, p.TaskID, p.TenantID, string(current.Status), string(domain.TaskStatusBlocked), p.ActorPrincipalID, p.Reason); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return t, nil
}

// EscalateTask — BIZ-05's own Escalate command. Valid from IN_PROGRESS
// or BLOCKED.
func (s *PgStore) EscalateTask(ctx context.Context, p domain.EscalateTaskParams) (*domain.Task, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.taskSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	current, err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE task_id=$1 AND tenant_id=$2 FOR UPDATE`, p.TaskID, p.TenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrTaskNotFound
	}
	if err != nil {
		return nil, err
	}
	if current.Status != domain.TaskStatusInProgress && current.Status != domain.TaskStatusBlocked {
		return nil, domain.ErrTaskInvalidState
	}
	t, err := scanTask(tx.QueryRow(ctx, `
		UPDATE tasks SET status='ESCALATED', escalated_to_role=$3, escalated_at=now(), updated_at=now()
		WHERE task_id=$1 AND tenant_id=$2 RETURNING `+taskColumns,
		p.TaskID, p.TenantID, p.EscalatedToRole))
	if err != nil {
		return nil, err
	}
	if err := recordTaskTransition(ctx, tx, p.TaskID, p.TenantID, string(current.Status), string(domain.TaskStatusEscalated), p.ActorPrincipalID, p.Reason); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return t, nil
}

// CompleteTask — BIZ-05's own Complete command. Valid from IN_PROGRESS,
// BLOCKED, or ESCALATED — the doc names no separate "Unblock"/"Resolve
// escalation" command, so Complete is the one path back to a concluded
// state from any active working state.
func (s *PgStore) CompleteTask(ctx context.Context, p domain.CompleteTaskParams) (*domain.Task, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.taskSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	current, err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE task_id=$1 AND tenant_id=$2 FOR UPDATE`, p.TaskID, p.TenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrTaskNotFound
	}
	if err != nil {
		return nil, err
	}
	switch current.Status {
	case domain.TaskStatusInProgress, domain.TaskStatusBlocked, domain.TaskStatusEscalated:
	default:
		return nil, domain.ErrTaskInvalidState
	}
	t, err := scanTask(tx.QueryRow(ctx, `
		UPDATE tasks SET status='COMPLETED', completion_notes=$3, completed_at=now(), updated_at=now()
		WHERE task_id=$1 AND tenant_id=$2 RETURNING `+taskColumns,
		p.TaskID, p.TenantID, p.CompletionNotes))
	if err != nil {
		return nil, err
	}
	if err := recordTaskTransition(ctx, tx, p.TaskID, p.TenantID, string(current.Status), string(domain.TaskStatusCompleted), p.ActorPrincipalID, p.CompletionNotes); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return t, nil
}

// ReopenTask — BIZ-05's own Reopen command, a governed transition back
// into IN_PROGRESS from COMPLETED or CLOSED. Increments reopened_count
// so how many times a task was reopened is itself part of its record,
// not just visible via GetHistory.
func (s *PgStore) ReopenTask(ctx context.Context, p domain.ReopenTaskParams) (*domain.Task, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.taskSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	current, err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE task_id=$1 AND tenant_id=$2 FOR UPDATE`, p.TaskID, p.TenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrTaskNotFound
	}
	if err != nil {
		return nil, err
	}
	if current.Status != domain.TaskStatusCompleted && current.Status != domain.TaskStatusClosed {
		return nil, domain.ErrTaskInvalidState
	}
	t, err := scanTask(tx.QueryRow(ctx, `
		UPDATE tasks SET status='IN_PROGRESS', reopened_count=reopened_count+1, completed_at=NULL, closed_at=NULL, updated_at=now()
		WHERE task_id=$1 AND tenant_id=$2 RETURNING `+taskColumns,
		p.TaskID, p.TenantID))
	if err != nil {
		return nil, err
	}
	if err := recordTaskTransition(ctx, tx, p.TaskID, p.TenantID, string(current.Status), string(domain.TaskStatusInProgress), p.ActorPrincipalID, p.Reason); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return t, nil
}

// CancelTask — BIZ-05's own Cancel command. Valid from any non-terminal
// state.
func (s *PgStore) CancelTask(ctx context.Context, p domain.CancelTaskParams) (*domain.Task, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.taskSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	current, err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE task_id=$1 AND tenant_id=$2 FOR UPDATE`, p.TaskID, p.TenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrTaskNotFound
	}
	if err != nil {
		return nil, err
	}
	switch current.Status {
	case domain.TaskStatusCompleted, domain.TaskStatusClosed, domain.TaskStatusCancelled:
		return nil, domain.ErrTaskInvalidState
	}
	t, err := scanTask(tx.QueryRow(ctx, `
		UPDATE tasks SET status='CANCELLED', cancel_reason=$3, cancelled_at=now(), updated_at=now()
		WHERE task_id=$1 AND tenant_id=$2 RETURNING `+taskColumns,
		p.TaskID, p.TenantID, p.Reason))
	if err != nil {
		return nil, err
	}
	if err := recordTaskTransition(ctx, tx, p.TaskID, p.TenantID, string(current.Status), string(domain.TaskStatusCancelled), p.ActorPrincipalID, p.Reason); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return t, nil
}

// ListQueue — BIZ-05's own ListQueue query, the "my work"/team backlog
// view.
func (s *PgStore) ListQueue(ctx context.Context, p domain.ListQueueParams, limit, offset int) ([]domain.Task, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.taskSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	query := `SELECT ` + taskColumns + ` FROM tasks WHERE tenant_id=$1`
	args := []any{p.TenantID}
	if p.LegalEntityID != "" {
		args = append(args, p.LegalEntityID)
		query += fmt.Sprintf(" AND legal_entity_id=$%d", len(args))
	}
	if p.AssignedToUser != "" {
		args = append(args, p.AssignedToUser)
		query += fmt.Sprintf(" AND assigned_to_user=$%d", len(args))
	}
	if p.AssignedToRole != "" {
		args = append(args, p.AssignedToRole)
		query += fmt.Sprintf(" AND assigned_to_role=$%d", len(args))
	}
	if p.Status != "" {
		args = append(args, p.Status)
		query += fmt.Sprintf(" AND status=$%d", len(args))
	}
	// task_id breaks ties: created_at alone is not a total order.
	query += " ORDER BY created_at DESC, task_id DESC"
	args = append(args, limit)
	query += fmt.Sprintf(" LIMIT $%d", len(args))
	args = append(args, offset)
	query += fmt.Sprintf(" OFFSET $%d", len(args))

	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// GetSLAState — BIZ-05's own GetSLAState query. See
// domain.SLAState's own doc comment on why this is computed live rather
// than via a background clock service.
func (s *PgStore) GetSLAState(ctx context.Context, tenantID, taskID string) (*domain.SLAState, error) {
	t, err := s.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	state := &domain.SLAState{TaskID: t.TaskID, SLADeadline: t.SLADeadline}
	if t.SLADeadline == nil {
		return state, nil
	}
	now := time.Now().UTC()
	if now.After(*t.SLADeadline) {
		state.Overdue = true
		return state, nil
	}
	remaining := int64(t.SLADeadline.Sub(now).Seconds())
	state.TimeRemainingSeconds = &remaining
	return state, nil
}

// GetLinkedObjectStatus — BIZ-05's own GetLinkedObjectStatus query. See
// domain.LinkedObjectStatus's own doc comment on why this is a
// passthrough rather than a live cross-service lookup.
func (s *PgStore) GetLinkedObjectStatus(ctx context.Context, tenantID, taskID string) (*domain.LinkedObjectStatus, error) {
	t, err := s.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return &domain.LinkedObjectStatus{
		TaskID: t.TaskID, LinkedObjectType: t.LinkedObjectType, LinkedObjectID: t.LinkedObjectID,
		Tracked: false,
		Note:    "BIZ-05 does not own linked-object state; this is the reference recorded on the task, not a live status lookup.",
	}, nil
}
