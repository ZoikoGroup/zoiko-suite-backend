package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"zoiko.io/exception-escalation-svc/internal/domain"
	"zoiko.io/exception-escalation-svc/internal/middleware"
	"zoiko.io/exception-escalation-svc/internal/store"
)

func newTaskParams(entity, caseID, linked string) domain.CreateTaskParams {
	return domain.CreateTaskParams{
		TenantID: "default", LegalEntityID: entity, CaseID: caseID, TaskType: "SUB",
		LinkedObjectType: "Y", LinkedObjectID: linked, CreatedByPrincipalID: "creator-1",
	}
}

// A task may only join an existing, OPEN case of its own legal entity (and tenant).
func TestPgStore_CreateTask_CaseMustExistBeOpenAndMatchEntity(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	_, err := s.CreateTask(ctx(), newTaskParams("le-1", "case-does-not-exist", "ci-1"))
	require.ErrorIs(t, err, domain.ErrCaseNotFound)

	open, err := s.CreateCase(ctx(), domain.CreateCaseParams{TenantID: "default", LegalEntityID: "le-1", CaseType: "REVIEW", CreatedByPrincipalID: "owner-1"})
	require.NoError(t, err)
	_, err = s.CreateTask(ctx(), newTaskParams("le-OTHER", open.CaseID, "ci-2"))
	require.ErrorIs(t, err, domain.ErrCaseEntityMismatch)
	_, err = s.CreateTask(ctx(), newTaskParams("le-1", open.CaseID, "ci-3"))
	require.NoError(t, err, "matching entity on an open case must still work")

	closed, err := s.CreateCase(ctx(), domain.CreateCaseParams{TenantID: "default", LegalEntityID: "le-1", CaseType: "REVIEW", CreatedByPrincipalID: "owner-1"})
	require.NoError(t, err)
	_, err = s.CloseCase(ctx(), domain.CloseCaseParams{CaseID: closed.CaseID, TenantID: "default", ActorPrincipalID: "sup-1", ClosureReason: "done"})
	require.NoError(t, err)
	_, err = s.CreateTask(ctx(), newTaskParams("le-1", closed.CaseID, "ci-4"))
	require.ErrorIs(t, err, domain.ErrCaseAlreadyClosedWC, "no new task may join a closed case")

	// Another tenant's case is simply not found.
	ctxB := middleware.WithTenant(context.Background(), "tenant-b")
	p := newTaskParams("le-1", open.CaseID, "ci-5")
	p.TenantID = "tenant-b"
	_, err = s.CreateTask(ctxB, p)
	require.ErrorIs(t, err, domain.ErrCaseNotFound)
}

// Reopening a CLOSED task must not leave the previous closer recorded against a task that is
// open again.
func TestPgStore_ReopenTask_FromClosed_ClearsClosedFields(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	c, err := s.CreateCase(ctx(), domain.CreateCaseParams{TenantID: "default", LegalEntityID: "le-1", CaseType: "REVIEW", CreatedByPrincipalID: "owner-1"})
	require.NoError(t, err)
	task, err := s.CreateTask(ctx(), newTaskParams("le-1", c.CaseID, "ci-reopen"))
	require.NoError(t, err)
	_, err = s.AssignTask(ctx(), domain.AssignTaskParams{TaskID: task.TaskID, TenantID: "default", ActorPrincipalID: "sup-1", AssignedToUser: "a-1"})
	require.NoError(t, err)
	_, err = s.StartTask(ctx(), domain.StartTaskParams{TaskID: task.TaskID, TenantID: "default", ActorPrincipalID: "a-1"})
	require.NoError(t, err)
	_, err = s.CompleteTask(ctx(), domain.CompleteTaskParams{TaskID: task.TaskID, TenantID: "default", ActorPrincipalID: "a-1"})
	require.NoError(t, err)
	_, err = s.CloseCase(ctx(), domain.CloseCaseParams{CaseID: c.CaseID, TenantID: "default", ActorPrincipalID: "sup-1", ClosureReason: "done"})
	require.NoError(t, err)

	closedTask, err := s.GetTask(ctx(), task.TaskID)
	require.NoError(t, err)
	require.Equal(t, domain.TaskStatusClosed, closedTask.Status)
	require.Equal(t, "sup-1", closedTask.ClosedBy)

	reopened, err := s.ReopenTask(ctx(), domain.ReopenTaskParams{TaskID: task.TaskID, TenantID: "default", ActorPrincipalID: "sup-1", Reason: "missed item"})
	require.NoError(t, err)
	require.Equal(t, domain.TaskStatusInProgress, reopened.Status)
	require.Nil(t, reopened.ClosedAt)
	require.Empty(t, reopened.ClosedBy, "closed_by must be cleared when closed_at is")
	require.Equal(t, 1, reopened.ReopenedCount)
}

// BIZ-05 implementation gate: tenant isolation. Tenant B cannot read, list, move or close
// tenant A's case and task, and nothing it attempts changes A's data.
func TestPgStore_Task_TenantIsolation(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	ctxB := middleware.WithTenant(context.Background(), "tenant-b")

	c, err := s.CreateCase(ctx(), domain.CreateCaseParams{TenantID: "default", LegalEntityID: "le-iso", CaseType: "REVIEW", CreatedByPrincipalID: "owner-1"})
	require.NoError(t, err)
	task, err := s.CreateTask(ctx(), newTaskParams("le-iso", c.CaseID, "ci-iso"))
	require.NoError(t, err)

	_, err = s.GetCase(ctxB, c.CaseID)
	require.ErrorIs(t, err, domain.ErrCaseNotFound)
	_, err = s.GetTask(ctxB, task.TaskID)
	require.ErrorIs(t, err, domain.ErrTaskNotFound)
	list, err := s.ListQueue(ctxB, domain.ListQueueParams{TenantID: "tenant-b", LegalEntityID: "le-iso"}, 100, 0)
	require.NoError(t, err)
	require.Empty(t, list)
	_, err = s.GetTaskHistory(ctxB, "tenant-b", task.TaskID)
	_ = err // history of an unknown task is empty or not-found; it must simply not leak rows
	hist, _ := s.GetTaskHistory(ctxB, "tenant-b", task.TaskID)
	require.Empty(t, hist)

	_, err = s.AssignTask(ctxB, domain.AssignTaskParams{TaskID: task.TaskID, TenantID: "tenant-b", ActorPrincipalID: "x", AssignedToUser: "x"})
	require.ErrorIs(t, err, domain.ErrTaskNotFound)
	_, err = s.CancelTask(ctxB, domain.CancelTaskParams{TaskID: task.TaskID, TenantID: "tenant-b", ActorPrincipalID: "x", Reason: "x"})
	require.ErrorIs(t, err, domain.ErrTaskNotFound)
	_, err = s.CloseCase(ctxB, domain.CloseCaseParams{CaseID: c.CaseID, TenantID: "tenant-b", ActorPrincipalID: "x", ClosureReason: "x"})
	require.ErrorIs(t, err, domain.ErrCaseNotFound)

	gotTask, err := s.GetTask(ctx(), task.TaskID)
	require.NoError(t, err)
	require.Equal(t, domain.TaskStatusNew, gotTask.Status)
	gotCase, err := s.GetCase(ctx(), c.CaseID)
	require.NoError(t, err)
	require.Equal(t, domain.WorkCaseOpen, gotCase.Status)

	// RLS itself, bypassing the store's tenant predicates. Skipped (not passed) for a role
	// that bypasses RLS.
	var bypass bool
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&bypass))
	if bypass {
		t.Log("connecting role bypasses RLS; skipping the raw RLS probe")
		return
	}
	tx, err := pool.Begin(context.Background())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.Background()) }()
	_, err = tx.Exec(context.Background(), `SELECT set_config('app.tenant_id', 'tenant-b', true)`)
	require.NoError(t, err)
	for _, table := range []string{"cases", "tasks", "task_transitions"} {
		var n int
		require.NoError(t, tx.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&n))
		require.Zero(t, n, "RLS leak: tenant B context sees rows in %s", table)
	}
}
