package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/exception-escalation-svc/internal/domain"
	"zoiko.io/exception-escalation-svc/internal/store"
)

const taskPrefix = "com.zoikosuite.business."

// Each BIZ-05 event is written in the same transaction as the change, to the shared eventing
// outbox; payload schemas are checked for every row by verifyEventContracts. Starting,
// blocking and cancelling a task name no event in the spec and write none.
func TestTaskEvents_WrittenWithEachChange(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool, store.WithEventRegion("local"))

	c, err := s.CreateCase(ctx(), domain.CreateCaseParams{TenantID: "default", LegalEntityID: "le-ev", CaseType: "REVIEW", CreatedByPrincipalID: "owner-1"})
	require.NoError(t, err)
	p := newTaskParams("le-ev", c.CaseID, "ci-ev")
	p.CorrelationID = "corr-create"
	task, err := s.CreateTask(ctx(), p)
	require.NoError(t, err)
	require.Equal(t, []string{taskPrefix + "task.created"}, eventingTypes(t, pool, task.TaskID))

	_, err = s.AssignTask(ctx(), domain.AssignTaskParams{TaskID: task.TaskID, TenantID: "default", ActorPrincipalID: "sup-1", AssignedToUser: "a-1", CorrelationID: "corr-assign"})
	require.NoError(t, err)
	_, err = s.StartTask(ctx(), domain.StartTaskParams{TaskID: task.TaskID, TenantID: "default", ActorPrincipalID: "a-1"})
	require.NoError(t, err)
	_, err = s.BlockTask(ctx(), domain.BlockTaskParams{TaskID: task.TaskID, TenantID: "default", ActorPrincipalID: "a-1", Reason: "waiting"})
	require.NoError(t, err)
	_, err = s.EscalateTask(ctx(), domain.EscalateTaskParams{TaskID: task.TaskID, TenantID: "default", ActorPrincipalID: "a-1", EscalatedToRole: "MANAGER", Reason: "stuck", CorrelationID: "corr-esc"})
	require.NoError(t, err)
	_, err = s.CompleteTask(ctx(), domain.CompleteTaskParams{TaskID: task.TaskID, TenantID: "default", ActorPrincipalID: "a-1", CompletionNotes: "done", CorrelationID: "corr-done"})
	require.NoError(t, err)
	_, err = s.CloseCase(ctx(), domain.CloseCaseParams{CaseID: c.CaseID, TenantID: "default", ActorPrincipalID: "sup-1", ClosureReason: "finished", CorrelationID: "corr-close"})
	require.NoError(t, err)
	_, err = s.ReopenTask(ctx(), domain.ReopenTaskParams{TaskID: task.TaskID, TenantID: "default", ActorPrincipalID: "sup-1", Reason: "missed item", CorrelationID: "corr-reopen"})
	require.NoError(t, err)

	// start and block wrote nothing; the case cascade closing the task wrote no task event.
	require.Equal(t, []string{
		taskPrefix + "task.created", taskPrefix + "task.assigned", taskPrefix + "task.escalated",
		taskPrefix + "task.completed", taskPrefix + "task.reopened",
	}, eventingTypes(t, pool, task.TaskID))
	require.Equal(t, []string{taskPrefix + "case.closed"}, eventingTypes(t, pool, c.CaseID))

	var all string
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT string_agg(convert_from(payload, 'UTF8'), ' ') FROM eventing_outbox`).Scan(&all))
	for _, want := range []string{"corr-create", "corr-assign", "corr-esc", "corr-done", "corr-reopen", "corr-close", "MANAGER", "creator-1", "sup-1"} {
		assert.Contains(t, all, want)
	}
}

// A refused command writes no event, and a store with no event region cannot build the
// envelope, so the write is refused and rolled back: no task, no event.
func TestTaskEvents_RefusedAndRegionlessWritesLeaveNoEvent(t *testing.T) {
	pool := requireTestDB(t)
	good := store.NewPgStore(pool, store.WithEventRegion("local"))
	task, err := good.CreateTask(ctx(), newTaskParams("le-r", "", "ci-r"))
	require.NoError(t, err)

	// Completing a NEW task is refused (not in a working state): nothing more is written.
	_, err = good.CompleteTask(ctx(), domain.CompleteTaskParams{TaskID: task.TaskID, TenantID: "default", ActorPrincipalID: "a"})
	require.ErrorIs(t, err, domain.ErrTaskInvalidState)
	require.Equal(t, []string{taskPrefix + "task.created"}, eventingTypes(t, pool, task.TaskID))

	noRegion := store.NewPgStore(pool) // no WithEventRegion
	_, err = noRegion.CreateTask(ctx(), newTaskParams("le-r", "", "ci-r2"))
	require.Error(t, err)
	var tasks, events int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM tasks`).Scan(&tasks))
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM eventing_outbox`).Scan(&events))
	assert.Equal(t, 1, tasks, "the regionless create must not leave a task behind")
	assert.Equal(t, 1, events)

	// An assignment that cannot write its event leaves the task NEW.
	_, err = noRegion.AssignTask(ctx(), domain.AssignTaskParams{TaskID: task.TaskID, TenantID: "default", ActorPrincipalID: "sup", AssignedToUser: "a"})
	require.Error(t, err)
	got, err := good.GetTask(ctx(), task.TaskID)
	require.NoError(t, err)
	assert.Equal(t, domain.TaskStatusNew, got.Status)
}
