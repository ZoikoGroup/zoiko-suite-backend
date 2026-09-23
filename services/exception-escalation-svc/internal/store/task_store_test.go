package store_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"zoiko.io/exception-escalation-svc/internal/domain"
	"zoiko.io/exception-escalation-svc/internal/store"
)

func TestPgStore_CreateCase_ThenClose_ThenRejectsDoubleClose(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	c, err := s.CreateCase(ctx(), domain.CreateCaseParams{
		TenantID: "default", LegalEntityID: "le-1", CaseType: "COMPLIANCE_REVIEW", Purpose: "Quarterly review", CreatedByPrincipalID: "owner-1",
	})
	require.NoError(t, err)
	require.Equal(t, domain.WorkCaseOpen, c.Status)

	got, err := s.GetCase(ctx(), c.CaseID)
	require.NoError(t, err)
	require.Equal(t, c.CaseID, got.CaseID)

	closed, err := s.CloseCase(ctx(), domain.CloseCaseParams{CaseID: c.CaseID, TenantID: "default", ActorPrincipalID: "supervisor-1", ClosureReason: "complete"})
	require.NoError(t, err)
	require.Equal(t, domain.WorkCaseClosed, closed.Status)
	require.NotNil(t, closed.ClosedAt)

	_, err = s.CloseCase(ctx(), domain.CloseCaseParams{CaseID: c.CaseID, TenantID: "default", ActorPrincipalID: "supervisor-1", ClosureReason: "again"})
	require.ErrorIs(t, err, domain.ErrCaseAlreadyClosedWC)
}

func TestPgStore_GetCase_UnknownCase_ReturnsNotFound(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	_, err := s.GetCase(ctx(), "case-does-not-exist")
	require.ErrorIs(t, err, domain.ErrCaseNotFound)
}

func TestPgStore_CreateTask_LandsNewAndRecordsHistory(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	task, err := s.CreateTask(ctx(), domain.CreateTaskParams{
		TenantID: "default", LegalEntityID: "le-1", TaskType: "EXPENSE_REVIEW", Priority: domain.TaskPriorityHigh,
		LinkedObjectType: "EXPENSE_CLAIM", LinkedObjectID: "exp-100", CreatedByPrincipalID: "creator-1",
	})
	require.NoError(t, err)
	require.Equal(t, domain.TaskStatusNew, task.Status)
	require.Equal(t, domain.TaskPriorityHigh, task.Priority)

	got, err := s.GetTask(ctx(), task.TaskID)
	require.NoError(t, err)
	require.Equal(t, task.TaskID, got.TaskID)

	history, err := s.GetTaskHistory(ctx(), "default", task.TaskID)
	require.NoError(t, err)
	require.Len(t, history, 1)
	require.Equal(t, "", history[0].FromStatus)
	require.Equal(t, string(domain.TaskStatusNew), history[0].ToStatus)
}

func TestPgStore_CreateTask_DefaultsPriorityToMedium(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	task, err := s.CreateTask(ctx(), domain.CreateTaskParams{
		TenantID: "default", LegalEntityID: "le-1", TaskType: "GENERIC", LinkedObjectType: "INVOICE", LinkedObjectID: "inv-1", CreatedByPrincipalID: "creator-1",
	})
	require.NoError(t, err)
	require.Equal(t, domain.TaskPriorityMedium, task.Priority)
}

func TestPgStore_AssignTask_ThenStart_ThenRejectsStartWithoutAssign(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	task, err := s.CreateTask(ctx(), domain.CreateTaskParams{
		TenantID: "default", LegalEntityID: "le-1", TaskType: "EXPENSE_REVIEW",
		LinkedObjectType: "EXPENSE_CLAIM", LinkedObjectID: "exp-101", CreatedByPrincipalID: "creator-1",
	})
	require.NoError(t, err)

	// Start before assignment is refused — an unassigned task has no one
	// to start work on it.
	_, err = s.StartTask(ctx(), domain.StartTaskParams{TaskID: task.TaskID, TenantID: "default", ActorPrincipalID: "assignee-1"})
	require.ErrorIs(t, err, domain.ErrTaskInvalidState)

	assigned, err := s.AssignTask(ctx(), domain.AssignTaskParams{
		TaskID: task.TaskID, TenantID: "default", ActorPrincipalID: "supervisor-1", AssignedToUser: "assignee-1",
	})
	require.NoError(t, err)
	require.Equal(t, domain.TaskStatusAssigned, assigned.Status)
	require.Equal(t, "assignee-1", assigned.AssignedToUser)

	started, err := s.StartTask(ctx(), domain.StartTaskParams{TaskID: task.TaskID, TenantID: "default", ActorPrincipalID: "assignee-1"})
	require.NoError(t, err)
	require.Equal(t, domain.TaskStatusInProgress, started.Status)

	// A second Start (already IN_PROGRESS) is refused.
	_, err = s.StartTask(ctx(), domain.StartTaskParams{TaskID: task.TaskID, TenantID: "default", ActorPrincipalID: "assignee-1"})
	require.ErrorIs(t, err, domain.ErrTaskInvalidState)

	history, err := s.GetTaskHistory(ctx(), "default", task.TaskID)
	require.NoError(t, err)
	require.Len(t, history, 3, "expected created, assigned, started transitions")
	require.Equal(t, string(domain.TaskStatusAssigned), history[1].ToStatus)
	require.Equal(t, string(domain.TaskStatusInProgress), history[2].ToStatus)
}

func TestPgStore_GetTask_UnknownTask_ReturnsNotFound(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	_, err := s.GetTask(ctx(), "task-does-not-exist")
	require.ErrorIs(t, err, domain.ErrTaskNotFound)
}

// TestPgStore_TaskTransitions_AreAppendOnly is the negative-controlled
// proof of the append-only trigger — a raw UPDATE or DELETE against
// task_transitions is refused outright.
func TestPgStore_TaskTransitions_AreAppendOnly(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	task, err := s.CreateTask(ctx(), domain.CreateTaskParams{
		TenantID: "default", LegalEntityID: "le-1", TaskType: "EXPENSE_REVIEW",
		LinkedObjectType: "EXPENSE_CLAIM", LinkedObjectID: "exp-102", CreatedByPrincipalID: "creator-1",
	})
	require.NoError(t, err)
	history, err := s.GetTaskHistory(ctx(), "default", task.TaskID)
	require.NoError(t, err)
	require.Len(t, history, 1)

	_, err = pool.Exec(ctx(), `UPDATE task_transitions SET reason='tampered' WHERE transition_id=$1`, history[0].TransitionID)
	require.Error(t, err, "expected the trigger to refuse mutating a task transition")

	_, err = pool.Exec(ctx(), `DELETE FROM task_transitions WHERE transition_id=$1`, history[0].TransitionID)
	require.Error(t, err, "expected the trigger to refuse deleting a task transition")
}

// TestPgStore_CreateTask_WithCase links a task into a case via the
// optional case_id and proves the case itself is unaffected by the
// task's own lifecycle.
func TestPgStore_CreateTask_WithCase(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	c, err := s.CreateCase(ctx(), domain.CreateCaseParams{
		TenantID: "default", LegalEntityID: "le-1", CaseType: "COMPLIANCE_REVIEW", CreatedByPrincipalID: "owner-1",
	})
	require.NoError(t, err)

	task, err := s.CreateTask(ctx(), domain.CreateTaskParams{
		TenantID: "default", LegalEntityID: "le-1", CaseID: c.CaseID, TaskType: "SUB_TASK",
		LinkedObjectType: "EXPENSE_CLAIM", LinkedObjectID: "exp-103", CreatedByPrincipalID: "creator-1",
	})
	require.NoError(t, err)
	require.NotNil(t, task.CaseID)
	require.Equal(t, c.CaseID, *task.CaseID)

	stillOpen, err := s.GetCase(ctx(), c.CaseID)
	require.NoError(t, err)
	require.Equal(t, domain.WorkCaseOpen, stillOpen.Status)
}
