package store

import (
	"context"
	"errors"
	"fmt"

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
