package consumer_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/workflow-history-svc/internal/store"
)

// Tenant/entity context for a non-started event comes from the instance's
// workflow.started row and from nothing else. These tests pin that against the
// FakeStore (which mirrors PgStore's rule); the same cases run against real
// PostgreSQL in internal/store/consumer_pipeline_integration_test.go.

func startedMsg(t *testing.T, instance, tenant, entity string) []byte {
	return workflowEvent(t, "workflow.started", "corr", map[string]any{
		"workflow_instance_id": instance, "tenant_id": tenant, "legal_entity_id": entity,
	})
}

func grantedMsg(t *testing.T, instance string) []byte {
	return workflowEvent(t, "approval.granted", "corr", map[string]any{
		"workflow_instance_id": instance, "stage_order": 1, "approver_principal_id": "u-1",
	})
}

func TestTenantContext_EarlierNonStartEventIsNeverTheSource(t *testing.T) {
	c, fake := newConsumer(t)
	ctx := context.Background()

	// A non-start row for the instance that predates the started row and
	// carries a different tenant/entity. (The consumer cannot produce this
	// itself — it refuses transitions with no started row — so it is seeded
	// directly.)
	require.NoError(t, fake.Append(ctx, store.WorkflowHistoryEvent{
		EventID: "evt-rogue", WorkflowInstanceID: "wf-1", EventType: "approval.granted",
		TenantID: "t-rogue", LegalEntityID: "e-rogue", Payload: []byte(`{}`), RecordedAt: time.Now().Add(-time.Hour),
	}))
	require.NoError(t, c.Handle(ctx, "evt-start", startedMsg(t, "wf-1", "t-real", "e-real")))
	require.NoError(t, c.Handle(ctx, "evt-next", grantedMsg(t, "wf-1")))

	got, err := fake.ListByInstance(ctx, "t-real", "wf-1")
	require.NoError(t, err)
	require.Len(t, got, 2)
	for _, e := range got {
		assert.Equal(t, "t-real", e.TenantID)
		assert.Equal(t, "e-real", e.LegalEntityID)
	}
	// Nothing was written under the rogue tenant by the consumer.
	rogue, _ := fake.ListByInstance(ctx, "t-rogue", "wf-1")
	assert.Len(t, rogue, 1, "only the seeded row exists under t-rogue")
}

func TestTenantContext_OnlyNonStartRowsExist_FailsClosed(t *testing.T) {
	c, fake := newConsumer(t)
	ctx := context.Background()
	require.NoError(t, fake.Append(ctx, store.WorkflowHistoryEvent{
		EventID: "evt-rogue", WorkflowInstanceID: "wf-1", EventType: "workflow.completed",
		TenantID: "t-rogue", LegalEntityID: "e-rogue", Payload: []byte(`{}`),
	}))

	err := c.Handle(ctx, "evt-next", grantedMsg(t, "wf-1"))
	require.Error(t, err, "a non-start row is not a source of tenant context")
	assert.Equal(t, 1, fake.Count(), "nothing stored for the refused event")
}

func TestTenantContext_SameInstanceIdAcrossTenants_IsAmbiguous_NoInheritance(t *testing.T) {
	c, fake := newConsumer(t)
	ctx := context.Background()
	require.NoError(t, c.Handle(ctx, "evt-start-a", startedMsg(t, "wf-shared", "t-a", "e-a")))
	require.NoError(t, c.Handle(ctx, "evt-start-b", startedMsg(t, "wf-shared", "t-b", "e-b")))

	err := c.Handle(ctx, "evt-next", grantedMsg(t, "wf-shared"))
	require.ErrorIs(t, err, store.ErrAmbiguousTenantContext)
	assert.Equal(t, 2, fake.Count(), "the transition is not filed under either tenant")
}

func TestTenantContext_SameTenantDifferentEntityStartedRows_IsAmbiguous(t *testing.T) {
	c, fake := newConsumer(t)
	ctx := context.Background()
	require.NoError(t, c.Handle(ctx, "evt-start-1", startedMsg(t, "wf-1", "t-a", "e-1")))
	require.NoError(t, c.Handle(ctx, "evt-start-2", startedMsg(t, "wf-1", "t-a", "e-2")))

	require.ErrorIs(t, c.Handle(ctx, "evt-next", grantedMsg(t, "wf-1")), store.ErrAmbiguousTenantContext)
	assert.Equal(t, 2, fake.Count())
}

func TestTenantContext_DuplicateStartedWithSameContext_IsNotAmbiguous(t *testing.T) {
	c, fake := newConsumer(t)
	ctx := context.Background()
	// Same context under two event ids (e.g. a producer-side retry).
	require.NoError(t, c.Handle(ctx, "evt-start-1", startedMsg(t, "wf-1", "t-a", "e-a")))
	require.NoError(t, c.Handle(ctx, "evt-start-2", startedMsg(t, "wf-1", "t-a", "e-a")))
	require.NoError(t, c.Handle(ctx, "evt-next", grantedMsg(t, "wf-1")))
	assert.Equal(t, 3, fake.Count())
}

func TestTenantContext_ManyEvents_AllInheritFromStarted(t *testing.T) {
	c, fake := newConsumer(t)
	ctx := context.Background()
	require.NoError(t, c.Handle(ctx, "evt-start", startedMsg(t, "wf-1", "t-a", "e-a")))
	for _, id := range []string{"evt-2", "evt-3", "evt-4"} {
		require.NoError(t, c.Handle(ctx, id, grantedMsg(t, "wf-1")))
	}
	got, _ := fake.ListByInstance(ctx, "t-a", "wf-1")
	require.Len(t, got, 4)
	for _, e := range got {
		assert.Equal(t, "t-a", e.TenantID)
		assert.Equal(t, "e-a", e.LegalEntityID)
	}
}
