// Package consumer_test exercises the workflow event consumer with a FakeStore.
// These are pure unit tests — no Postgres, no Kafka required.
package consumer_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/workflow-history-svc/internal/consumer"
	"zoiko.io/workflow-history-svc/internal/store"
)

func newConsumer(t *testing.T) (*consumer.Consumer, *store.FakeStore) {
	t.Helper()
	fake := store.NewFakeStore()
	log, err := zap.NewDevelopment()
	require.NoError(t, err)
	return consumer.New(fake, fake, log), fake
}

func workflowEvent(t *testing.T, eventType, correlationID string, payload map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	env := map[string]any{
		"event_type":     eventType,
		"emitted_at":     "2026-07-15T09:00:00Z",
		"schema_version": "1.0",
		"source_service": "workflow-svc",
		"correlation_id": correlationID,
		"payload":        json.RawMessage(raw),
	}
	data, err := json.Marshal(env)
	require.NoError(t, err)
	return data
}

// ─── workflow.started ──────────────────────────────────────────────────────

func TestHandle_WorkflowStarted_Stored(t *testing.T) {
	c, fake := newConsumer(t)

	raw := workflowEvent(t, "workflow.started", "corr-001", map[string]any{
		"workflow_instance_id": "wf-001",
		"tenant_id":            "t-001",
		"legal_entity_id":      "e-001",
		"workflow_type":        "invoice_approval",
		"initiated_by":         "u-001",
		"started_at":           "2026-07-15T09:00:00Z",
	})

	err := c.Handle(context.Background(), "evt-001", raw)
	require.NoError(t, err)
	assert.Equal(t, 1, fake.Count())
}

func TestHandle_WorkflowStarted_Idempotent(t *testing.T) {
	c, fake := newConsumer(t)

	raw := workflowEvent(t, "workflow.started", "corr-001", map[string]any{
		"workflow_instance_id": "wf-001",
		"tenant_id":            "t-001",
		"legal_entity_id":      "e-001",
	})

	require.NoError(t, c.Handle(context.Background(), "evt-001", raw))
	require.NoError(t, c.Handle(context.Background(), "evt-001", raw)) // duplicate
	assert.Equal(t, 1, fake.Count(), "duplicate event_id must not produce a second row")
}

func TestHandle_WorkflowStarted_MissingTenantID(t *testing.T) {
	c, fake := newConsumer(t)

	raw := workflowEvent(t, "workflow.started", "corr-001", map[string]any{
		"workflow_instance_id": "wf-001",
		// tenant_id missing
		"legal_entity_id": "e-001",
	})

	err := c.Handle(context.Background(), "evt-001", raw)
	require.NoError(t, err) // rejection is non-fatal
	assert.Equal(t, 0, fake.Count())
}

func TestHandle_WorkflowStarted_MissingInstanceID(t *testing.T) {
	c, fake := newConsumer(t)

	raw := workflowEvent(t, "workflow.started", "corr-001", map[string]any{
		// workflow_instance_id missing
		"tenant_id":       "t-001",
		"legal_entity_id": "e-001",
	})

	err := c.Handle(context.Background(), "evt-001", raw)
	require.NoError(t, err)
	assert.Equal(t, 0, fake.Count())
}

// ─── approval.granted ──────────────────────────────────────────────────────

func TestHandle_ApprovalGranted_InheritsContext(t *testing.T) {
	c, fake := newConsumer(t)
	ctx := context.Background()

	// First, store the started event to establish tenant context.
	startedRaw := workflowEvent(t, "workflow.started", "corr-001", map[string]any{
		"workflow_instance_id": "wf-001",
		"tenant_id":            "t-001",
		"legal_entity_id":      "e-001",
	})
	require.NoError(t, c.Handle(ctx, "evt-001", startedRaw))

	// Now process an approval.granted event.
	grantedRaw := workflowEvent(t, "approval.granted", "corr-001", map[string]any{
		"workflow_instance_id":  "wf-001",
		"stage_order":           1,
		"approver_principal_id": "u-002",
	})
	require.NoError(t, c.Handle(ctx, "evt-002", grantedRaw))

	assert.Equal(t, 2, fake.Count())

	// Verify the approval event inherited the tenant context.
	events, err := fake.ListByInstance(ctx, "wf-001")
	require.NoError(t, err)
	require.Len(t, events, 2)
	assert.Equal(t, "t-001", events[1].TenantID)
	assert.Equal(t, "e-001", events[1].LegalEntityID)
	assert.Equal(t, "approval.granted", events[1].EventType)
}

func TestHandle_ApprovalGranted_NoStartedRow_ReturnsError(t *testing.T) {
	c, fake := newConsumer(t)

	// No workflow.started event → tenant context unavailable.
	raw := workflowEvent(t, "approval.granted", "corr-001", map[string]any{
		"workflow_instance_id":  "wf-orphan",
		"stage_order":           1,
		"approver_principal_id": "u-002",
	})

	err := c.Handle(context.Background(), "evt-002", raw)
	// Must return a non-nil error so the runner does NOT commit the offset.
	require.Error(t, err)
	assert.Equal(t, 0, fake.Count())
}

// ─── workflow.completed ────────────────────────────────────────────────────

func TestHandle_WorkflowCompleted_FullChain(t *testing.T) {
	c, fake := newConsumer(t)
	ctx := context.Background()

	events := []struct {
		id      string
		evtType string
		payload map[string]any
	}{
		{"evt-001", "workflow.started", map[string]any{
			"workflow_instance_id": "wf-001", "tenant_id": "t-001", "legal_entity_id": "e-001",
		}},
		{"evt-002", "approval.granted", map[string]any{
			"workflow_instance_id": "wf-001", "stage_order": 1, "approver_principal_id": "u-002",
		}},
		{"evt-003", "workflow.completed", map[string]any{
			"workflow_instance_id": "wf-001", "workflow_status": "approved", "completed_at": "2026-07-15T10:00:00Z",
		}},
	}

	for _, ev := range events {
		raw := workflowEvent(t, ev.evtType, "corr-001", ev.payload)
		require.NoError(t, c.Handle(ctx, ev.id, raw))
	}

	assert.Equal(t, 3, fake.Count())

	history, err := fake.ListByInstance(ctx, "wf-001")
	require.NoError(t, err)
	require.Len(t, history, 3)
	assert.Equal(t, "workflow.started", history[0].EventType)
	assert.Equal(t, "approval.granted", history[1].EventType)
	assert.Equal(t, "workflow.completed", history[2].EventType)
}

// ─── Unknown event type ────────────────────────────────────────────────────

func TestHandle_UnknownEventType_Skipped(t *testing.T) {
	c, fake := newConsumer(t)

	raw := workflowEvent(t, "some.unknown.event", "corr-001", map[string]any{})
	err := c.Handle(context.Background(), "evt-001", raw)
	require.NoError(t, err) // should not crash or error
	assert.Equal(t, 0, fake.Count())
}

// ─── Envelope validation ───────────────────────────────────────────────────

func TestHandle_EmptyEventID_Rejected(t *testing.T) {
	c, _ := newConsumer(t)
	raw := workflowEvent(t, "workflow.started", "corr-001", map[string]any{
		"workflow_instance_id": "wf-001", "tenant_id": "t-001", "legal_entity_id": "e-001",
	})
	err := c.Handle(context.Background(), "", raw)
	require.NoError(t, err)
}

func TestHandle_MalformedJSON_Rejected(t *testing.T) {
	c, _ := newConsumer(t)
	err := c.Handle(context.Background(), "evt-001", []byte("not-json"))
	require.NoError(t, err)
}

func TestHandle_MissingSourceService_Rejected(t *testing.T) {
	c, fake := newConsumer(t)

	env := map[string]any{
		"event_type":     "workflow.started",
		"emitted_at":     "2026-07-15T09:00:00Z",
		"schema_version": "1.0",
		// source_service missing
		"correlation_id": "corr-001",
		"payload":        map[string]any{"workflow_instance_id": "wf-001", "tenant_id": "t-001", "legal_entity_id": "e-001"},
	}
	data, _ := json.Marshal(env)
	err := c.Handle(context.Background(), "evt-001", data)
	require.NoError(t, err)
	assert.Equal(t, 0, fake.Count())
}
