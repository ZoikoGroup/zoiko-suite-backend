package store_test

import (
	"testing"
	"time"

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

// inProgressTaskForTest walks a task through NEW -> ASSIGNED ->
// IN_PROGRESS, the precondition most Wave 2 commands need.
func inProgressTaskForTest(t *testing.T, s *store.PgStore, linkedObjectID string) *domain.Task {
	t.Helper()
	task, err := s.CreateTask(ctx(), domain.CreateTaskParams{
		TenantID: "default", LegalEntityID: "le-1", TaskType: "EXPENSE_REVIEW",
		LinkedObjectType: "EXPENSE_CLAIM", LinkedObjectID: linkedObjectID, CreatedByPrincipalID: "creator-1",
	})
	require.NoError(t, err)
	_, err = s.AssignTask(ctx(), domain.AssignTaskParams{TaskID: task.TaskID, TenantID: "default", ActorPrincipalID: "supervisor-1", AssignedToUser: "assignee-1"})
	require.NoError(t, err)
	started, err := s.StartTask(ctx(), domain.StartTaskParams{TaskID: task.TaskID, TenantID: "default", ActorPrincipalID: "assignee-1"})
	require.NoError(t, err)
	return started
}

func TestPgStore_BlockTask_ThenRejectsBlockFromWrongState(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	task := inProgressTaskForTest(t, s, "exp-200")

	blocked, err := s.BlockTask(ctx(), domain.BlockTaskParams{TaskID: task.TaskID, TenantID: "default", ActorPrincipalID: "assignee-1", Reason: "waiting on receipts"})
	require.NoError(t, err)
	require.Equal(t, domain.TaskStatusBlocked, blocked.Status)
	require.Equal(t, "waiting on receipts", blocked.BlockedReason)

	// A task already BLOCKED cannot be blocked again.
	_, err = s.BlockTask(ctx(), domain.BlockTaskParams{TaskID: task.TaskID, TenantID: "default", ActorPrincipalID: "assignee-1", Reason: "again"})
	require.ErrorIs(t, err, domain.ErrTaskInvalidState)
}

func TestPgStore_EscalateTask_FromInProgressAndFromBlocked(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	// From IN_PROGRESS directly.
	task1 := inProgressTaskForTest(t, s, "exp-201")
	escalated1, err := s.EscalateTask(ctx(), domain.EscalateTaskParams{TaskID: task1.TaskID, TenantID: "default", ActorPrincipalID: "assignee-1", EscalatedToRole: "manager", Reason: "overdue"})
	require.NoError(t, err)
	require.Equal(t, domain.TaskStatusEscalated, escalated1.Status)
	require.Equal(t, "manager", escalated1.EscalatedToRole)
	require.NotNil(t, escalated1.EscalatedAt)

	// From BLOCKED.
	task2 := inProgressTaskForTest(t, s, "exp-202")
	_, err = s.BlockTask(ctx(), domain.BlockTaskParams{TaskID: task2.TaskID, TenantID: "default", ActorPrincipalID: "assignee-1", Reason: "blocked"})
	require.NoError(t, err)
	escalated2, err := s.EscalateTask(ctx(), domain.EscalateTaskParams{TaskID: task2.TaskID, TenantID: "default", ActorPrincipalID: "assignee-1", EscalatedToRole: "manager", Reason: "still blocked"})
	require.NoError(t, err)
	require.Equal(t, domain.TaskStatusEscalated, escalated2.Status)

	// Not valid from NEW.
	task3, err := s.CreateTask(ctx(), domain.CreateTaskParams{TenantID: "default", LegalEntityID: "le-1", TaskType: "X", LinkedObjectType: "Y", LinkedObjectID: "z", CreatedByPrincipalID: "creator-1"})
	require.NoError(t, err)
	_, err = s.EscalateTask(ctx(), domain.EscalateTaskParams{TaskID: task3.TaskID, TenantID: "default", ActorPrincipalID: "creator-1", EscalatedToRole: "manager", Reason: "x"})
	require.ErrorIs(t, err, domain.ErrTaskInvalidState)
}

func TestPgStore_CompleteTask_FromAnyActiveState(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	for _, tc := range []struct {
		name string
		prep func(taskID string) error
	}{
		{"in_progress", func(string) error { return nil }},
		{"blocked", func(taskID string) error {
			_, err := s.BlockTask(ctx(), domain.BlockTaskParams{TaskID: taskID, TenantID: "default", ActorPrincipalID: "assignee-1", Reason: "r"})
			return err
		}},
		{"escalated", func(taskID string) error {
			_, err := s.EscalateTask(ctx(), domain.EscalateTaskParams{TaskID: taskID, TenantID: "default", ActorPrincipalID: "assignee-1", EscalatedToRole: "manager", Reason: "r"})
			return err
		}},
	} {
		task := inProgressTaskForTest(t, s, "exp-complete-"+tc.name)
		require.NoError(t, tc.prep(task.TaskID))
		completed, err := s.CompleteTask(ctx(), domain.CompleteTaskParams{TaskID: task.TaskID, TenantID: "default", ActorPrincipalID: "assignee-1", CompletionNotes: "done"})
		require.NoError(t, err, tc.name)
		require.Equal(t, domain.TaskStatusCompleted, completed.Status, tc.name)
		require.NotNil(t, completed.CompletedAt, tc.name)
	}

	// Not valid from NEW.
	task, err := s.CreateTask(ctx(), domain.CreateTaskParams{TenantID: "default", LegalEntityID: "le-1", TaskType: "X", LinkedObjectType: "Y", LinkedObjectID: "z2", CreatedByPrincipalID: "creator-1"})
	require.NoError(t, err)
	_, err = s.CompleteTask(ctx(), domain.CompleteTaskParams{TaskID: task.TaskID, TenantID: "default", ActorPrincipalID: "creator-1"})
	require.ErrorIs(t, err, domain.ErrTaskInvalidState)
}

func TestPgStore_ReopenTask_FromCompleted_ThenRejectsFromInProgress(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	task := inProgressTaskForTest(t, s, "exp-203")
	completed, err := s.CompleteTask(ctx(), domain.CompleteTaskParams{TaskID: task.TaskID, TenantID: "default", ActorPrincipalID: "assignee-1"})
	require.NoError(t, err)
	require.Equal(t, 0, completed.ReopenedCount)

	reopened, err := s.ReopenTask(ctx(), domain.ReopenTaskParams{TaskID: task.TaskID, TenantID: "default", ActorPrincipalID: "supervisor-1", Reason: "needs more work"})
	require.NoError(t, err)
	require.Equal(t, domain.TaskStatusInProgress, reopened.Status)
	require.Equal(t, 1, reopened.ReopenedCount)
	require.Nil(t, reopened.CompletedAt)

	// Reopen is not valid from IN_PROGRESS.
	_, err = s.ReopenTask(ctx(), domain.ReopenTaskParams{TaskID: task.TaskID, TenantID: "default", ActorPrincipalID: "supervisor-1", Reason: "again"})
	require.ErrorIs(t, err, domain.ErrTaskInvalidState)
}

func TestPgStore_CancelTask_FromActiveStates_ThenRejectsFromTerminal(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	task, err := s.CreateTask(ctx(), domain.CreateTaskParams{TenantID: "default", LegalEntityID: "le-1", TaskType: "X", LinkedObjectType: "Y", LinkedObjectID: "cancel-1", CreatedByPrincipalID: "creator-1"})
	require.NoError(t, err)
	cancelled, err := s.CancelTask(ctx(), domain.CancelTaskParams{TaskID: task.TaskID, TenantID: "default", ActorPrincipalID: "creator-1", Reason: "no longer needed"})
	require.NoError(t, err)
	require.Equal(t, domain.TaskStatusCancelled, cancelled.Status)

	// Cannot cancel an already-CANCELLED task.
	_, err = s.CancelTask(ctx(), domain.CancelTaskParams{TaskID: task.TaskID, TenantID: "default", ActorPrincipalID: "creator-1", Reason: "again"})
	require.ErrorIs(t, err, domain.ErrTaskInvalidState)

	// Cannot cancel a COMPLETED task.
	completedTask := inProgressTaskForTest(t, s, "cancel-2")
	_, err = s.CompleteTask(ctx(), domain.CompleteTaskParams{TaskID: completedTask.TaskID, TenantID: "default", ActorPrincipalID: "assignee-1"})
	require.NoError(t, err)
	_, err = s.CancelTask(ctx(), domain.CancelTaskParams{TaskID: completedTask.TaskID, TenantID: "default", ActorPrincipalID: "creator-1", Reason: "x"})
	require.ErrorIs(t, err, domain.ErrTaskInvalidState)
}

func TestPgStore_ListQueue_FiltersByAssigneeAndStatus(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	_, err := s.CreateTask(ctx(), domain.CreateTaskParams{TenantID: "default", LegalEntityID: "le-queue", TaskType: "X", LinkedObjectType: "Y", LinkedObjectID: "q1", CreatedByPrincipalID: "creator-1"})
	require.NoError(t, err)
	assigned, err := s.CreateTask(ctx(), domain.CreateTaskParams{TenantID: "default", LegalEntityID: "le-queue", TaskType: "X", LinkedObjectType: "Y", LinkedObjectID: "q2", CreatedByPrincipalID: "creator-1"})
	require.NoError(t, err)
	_, err = s.AssignTask(ctx(), domain.AssignTaskParams{TaskID: assigned.TaskID, TenantID: "default", ActorPrincipalID: "supervisor-1", AssignedToUser: "assignee-queue"})
	require.NoError(t, err)

	all, err := s.ListQueue(ctx(), domain.ListQueueParams{TenantID: "default", LegalEntityID: "le-queue"}, 100, 0)
	require.NoError(t, err)
	require.Len(t, all, 2)

	mine, err := s.ListQueue(ctx(), domain.ListQueueParams{TenantID: "default", LegalEntityID: "le-queue", AssignedToUser: "assignee-queue"}, 100, 0)
	require.NoError(t, err)
	require.Len(t, mine, 1)
	require.Equal(t, assigned.TaskID, mine[0].TaskID)

	newOnly, err := s.ListQueue(ctx(), domain.ListQueueParams{TenantID: "default", LegalEntityID: "le-queue", Status: string(domain.TaskStatusNew)}, 100, 0)
	require.NoError(t, err)
	require.Len(t, newOnly, 1)
}

func TestPgStore_GetSLAState_OverdueAndNotOverdueAndUnset(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	past := timeMustParse(t, "2020-01-01T00:00:00Z")
	overdueTask, err := s.CreateTask(ctx(), domain.CreateTaskParams{
		TenantID: "default", LegalEntityID: "le-1", TaskType: "X", LinkedObjectType: "Y", LinkedObjectID: "sla-1",
		SLADeadline: &past, CreatedByPrincipalID: "creator-1",
	})
	require.NoError(t, err)
	state, err := s.GetSLAState(ctx(), "default", overdueTask.TaskID)
	require.NoError(t, err)
	require.True(t, state.Overdue)
	require.Nil(t, state.TimeRemainingSeconds)

	future := timeMustParse(t, "2099-01-01T00:00:00Z")
	futureTask, err := s.CreateTask(ctx(), domain.CreateTaskParams{
		TenantID: "default", LegalEntityID: "le-1", TaskType: "X", LinkedObjectType: "Y", LinkedObjectID: "sla-2",
		SLADeadline: &future, CreatedByPrincipalID: "creator-1",
	})
	require.NoError(t, err)
	state2, err := s.GetSLAState(ctx(), "default", futureTask.TaskID)
	require.NoError(t, err)
	require.False(t, state2.Overdue)
	require.NotNil(t, state2.TimeRemainingSeconds)
	require.Greater(t, *state2.TimeRemainingSeconds, int64(0))

	noDeadlineTask, err := s.CreateTask(ctx(), domain.CreateTaskParams{TenantID: "default", LegalEntityID: "le-1", TaskType: "X", LinkedObjectType: "Y", LinkedObjectID: "sla-3", CreatedByPrincipalID: "creator-1"})
	require.NoError(t, err)
	state3, err := s.GetSLAState(ctx(), "default", noDeadlineTask.TaskID)
	require.NoError(t, err)
	require.False(t, state3.Overdue)
	require.Nil(t, state3.SLADeadline)
	require.Nil(t, state3.TimeRemainingSeconds)
}

func TestPgStore_GetLinkedObjectStatus_IsAPassthroughNotALiveLookup(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	task, err := s.CreateTask(ctx(), domain.CreateTaskParams{
		TenantID: "default", LegalEntityID: "le-1", TaskType: "X", LinkedObjectType: "EXPENSE_CLAIM", LinkedObjectID: "exp-999", CreatedByPrincipalID: "creator-1",
	})
	require.NoError(t, err)
	status, err := s.GetLinkedObjectStatus(ctx(), "default", task.TaskID)
	require.NoError(t, err)
	require.Equal(t, "EXPENSE_CLAIM", status.LinkedObjectType)
	require.Equal(t, "exp-999", status.LinkedObjectID)
	require.False(t, status.Tracked, "BIZ-05 must not claim to track linked-object state it does not own")
}

// TestPgStore_CloseCase_CascadesCompletedTasksToClosedOnly proves the
// documented CloseCase cascade decision: a COMPLETED task belonging to
// the case is closed too, but a task still IN_PROGRESS is left exactly
// as it was — never fabricated into COMPLETED or CLOSED.
func TestPgStore_CloseCase_CascadesCompletedTasksToClosedOnly(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	c, err := s.CreateCase(ctx(), domain.CreateCaseParams{TenantID: "default", LegalEntityID: "le-1", CaseType: "COMPLIANCE_REVIEW", CreatedByPrincipalID: "owner-1"})
	require.NoError(t, err)

	completedTask, err := s.CreateTask(ctx(), domain.CreateTaskParams{TenantID: "default", LegalEntityID: "le-1", CaseID: c.CaseID, TaskType: "SUB", LinkedObjectType: "Y", LinkedObjectID: "case-t1", CreatedByPrincipalID: "creator-1"})
	require.NoError(t, err)
	_, err = s.AssignTask(ctx(), domain.AssignTaskParams{TaskID: completedTask.TaskID, TenantID: "default", ActorPrincipalID: "supervisor-1", AssignedToUser: "assignee-1"})
	require.NoError(t, err)
	_, err = s.StartTask(ctx(), domain.StartTaskParams{TaskID: completedTask.TaskID, TenantID: "default", ActorPrincipalID: "assignee-1"})
	require.NoError(t, err)
	_, err = s.CompleteTask(ctx(), domain.CompleteTaskParams{TaskID: completedTask.TaskID, TenantID: "default", ActorPrincipalID: "assignee-1"})
	require.NoError(t, err)

	inProgressTask, err := s.CreateTask(ctx(), domain.CreateTaskParams{TenantID: "default", LegalEntityID: "le-1", CaseID: c.CaseID, TaskType: "SUB", LinkedObjectType: "Y", LinkedObjectID: "case-t2", CreatedByPrincipalID: "creator-1"})
	require.NoError(t, err)
	_, err = s.AssignTask(ctx(), domain.AssignTaskParams{TaskID: inProgressTask.TaskID, TenantID: "default", ActorPrincipalID: "supervisor-1", AssignedToUser: "assignee-1"})
	require.NoError(t, err)
	_, err = s.StartTask(ctx(), domain.StartTaskParams{TaskID: inProgressTask.TaskID, TenantID: "default", ActorPrincipalID: "assignee-1"})
	require.NoError(t, err)

	_, err = s.CloseCase(ctx(), domain.CloseCaseParams{CaseID: c.CaseID, TenantID: "default", ActorPrincipalID: "supervisor-1", ClosureReason: "done"})
	require.NoError(t, err)

	closedTask, err := s.GetTask(ctx(), completedTask.TaskID)
	require.NoError(t, err)
	require.Equal(t, domain.TaskStatusClosed, closedTask.Status)

	stillInProgress, err := s.GetTask(ctx(), inProgressTask.TaskID)
	require.NoError(t, err)
	require.Equal(t, domain.TaskStatusInProgress, stillInProgress.Status, "an in-progress task must never be silently fabricated into COMPLETED/CLOSED")
}

func timeMustParse(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	require.NoError(t, err)
	return ts
}
