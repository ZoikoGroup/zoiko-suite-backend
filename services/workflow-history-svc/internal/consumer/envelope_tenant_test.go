package consumer_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/workflow-history-svc/internal/consumer"
)

// Envelope tenant integrity: when the producer stamps a tenant on the event
// envelope it must be the tenant the event would be stored under. Doc 03 §19
// requires a tenant ID on every published event, and workflow-svc's publisher
// sets it on every workflow event.

// stamped returns raw with the envelope-level tenant_id set.
func stamped(t *testing.T, raw []byte, tenant string) []byte {
	t.Helper()
	var env map[string]any
	require.NoError(t, json.Unmarshal(raw, &env))
	env["tenant_id"] = tenant
	out, err := json.Marshal(env)
	require.NoError(t, err)
	return out
}

func startedFor(t *testing.T, instance, tenant string) []byte {
	return workflowEvent(t, "workflow.started", "corr", map[string]any{
		"workflow_instance_id": instance, "tenant_id": tenant, "legal_entity_id": "e-1",
	})
}

func grantedFor(t *testing.T, instance string) []byte {
	return workflowEvent(t, "approval.granted", "corr", map[string]any{
		"workflow_instance_id": instance, "stage_order": 1, "approver_principal_id": "u-2",
	})
}

func TestEnvelopeTenant_TransitionWithMismatchingTenant_IsRefusedAndNotStored(t *testing.T) {
	c, fake := newConsumer(t)
	ctx := context.Background()
	require.NoError(t, c.Handle(ctx, "s1", startedFor(t, "wf-1", "t-A")))

	err := c.Handle(ctx, "bad", stamped(t, grantedFor(t, "wf-1"), "t-B"))
	require.Error(t, err, "fail closed: the runner retries and then dead-letters it")
	assert.ErrorIs(t, err, consumer.ErrEnvelopeTenantMismatch)
	assert.Equal(t, 1, fake.Count(), "nothing was stored for the mismatched event")

	evs, _ := fake.ListByInstance(ctx, "t-A", "wf-1")
	assert.Len(t, evs, 1, "tenant A's history is untouched")
	other, _ := fake.ListByInstance(ctx, "t-B", "wf-1")
	assert.Empty(t, other, "and nothing was filed under tenant B")
}

// The hijack the check exists for: tenant B's event names tenant A's workflow
// instance id. Without the check it would be filed into A's history.
func TestEnvelopeTenant_CannotAppendToAnotherTenantsWorkflow(t *testing.T) {
	c, fake := newConsumer(t)
	ctx := context.Background()
	require.NoError(t, c.Handle(ctx, "a-start", startedFor(t, "wf-a", "t-A")))
	require.NoError(t, c.Handle(ctx, "b-start", startedFor(t, "wf-b", "t-B")))

	err := c.Handle(ctx, "hijack", stamped(t, grantedFor(t, "wf-a"), "t-B"))
	require.ErrorIs(t, err, consumer.ErrEnvelopeTenantMismatch)
	assert.Equal(t, 2, fake.Count())
}

func TestEnvelopeTenant_MatchingTenant_IsStored(t *testing.T) {
	c, fake := newConsumer(t)
	ctx := context.Background()
	require.NoError(t, c.Handle(ctx, "s1", stamped(t, startedFor(t, "wf-1", "t-A"), "t-A")))
	require.NoError(t, c.Handle(ctx, "g1", stamped(t, grantedFor(t, "wf-1"), "t-A")))
	assert.Equal(t, 2, fake.Count())
}

// An envelope with no tenant cannot be checked, and is accepted exactly as
// before: workflow-svc marks the field omitempty rather than fabricating a
// default, and nothing authoritative says such an event must be refused.
func TestEnvelopeTenant_AbsentTenant_BehavesAsBefore(t *testing.T) {
	c, fake := newConsumer(t)
	ctx := context.Background()
	require.NoError(t, c.Handle(ctx, "s1", startedFor(t, "wf-1", "t-A")))
	require.NoError(t, c.Handle(ctx, "g1", grantedFor(t, "wf-1")))
	assert.Equal(t, 2, fake.Count())
}

func TestEnvelopeTenant_StartedEvent_EnvelopeMustAgreeWithPayload(t *testing.T) {
	c, fake := newConsumer(t)
	ctx := context.Background()

	err := c.Handle(ctx, "s-bad", stamped(t, startedFor(t, "wf-1", "t-A"), "t-B"))
	require.ErrorIs(t, err, consumer.ErrEnvelopeTenantMismatch)
	assert.Equal(t, 0, fake.Count(), "a started row is the authority for later events and must not be filed on a disputed tenant")

	require.NoError(t, c.Handle(ctx, "s-ok", stamped(t, startedFor(t, "wf-1", "t-A"), "t-A")))
	assert.Equal(t, 1, fake.Count())
}
