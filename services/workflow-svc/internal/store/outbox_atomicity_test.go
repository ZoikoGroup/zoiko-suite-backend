package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/workflow-svc/internal/outbox"
)

// TestPgStore_Outbox_ForcedFailure_RollbackAtomicity_RealDB proves that when an operation
// fails mid-transaction (demonstrated here by a forced duplicate key failure on outbox_events),
// rolling back the transaction ensures that neither the domain row (workflow_instances)
// nor the outbox row persists in the database.
func TestPgStore_Outbox_ForcedFailure_RollbackAtomicity_RealDB(t *testing.T) {
	pool := getTestPool(t)
	ctx := context.Background()

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	workflowInstanceID := uuid.New().String()
	correlationID := uuid.New().String()
	actorID := "user-actor-1"
	outboxEventID := uuid.New().String()

	// 1. Begin a real transaction
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)

	// Set tenant context for RLS in this tx
	_, err = tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID)
	require.NoError(t, err)

	// 2. Write the domain business row into workflow_instances
	const insertInstance = `
		INSERT INTO workflow_instances (
			workflow_instance_id, tenant_id, legal_entity_id, workflow_type, initiated_by, correlation_id
		) VALUES ($1, $2, $3, $4, $5, $6);
	`
	_, err = tx.Exec(ctx, insertInstance, workflowInstanceID, tenantID, legalEntityID, "PURCHASE_APPROVAL", actorID, correlationID)
	require.NoError(t, err)

	// 3. Write the outbox row
	err = outbox.Insert(ctx, tx, outbox.Event{
		OutboxEventID: outboxEventID,
		AggregateType: "WORKFLOW_INSTANCE",
		AggregateID:   workflowInstanceID,
		EventType:     "workflow.created",
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		ActorID:       &actorID,
		CorrelationID: &correlationID,
		Payload:       map[string]any{"workflow_instance_id": workflowInstanceID},
	})
	require.NoError(t, err)

	// 4. Deliberately fail the transaction before commit
	// (Trigger a primary key constraint collision on outbox_events, forcing a failure)
	_, err = tx.Exec(ctx, "INSERT INTO outbox_events (outbox_event_id) VALUES ($1)", outboxEventID)
	require.Error(t, err, "expected duplicate primary key constraint error")

	// Explicitly roll back the transaction
	err = tx.Rollback(ctx)
	require.NoError(t, err)

	// 5. Query fresh connection (pool) and assert NEITHER row exists
	var instanceCount, outboxCount int
	verifyTx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer verifyTx.Rollback(ctx) //nolint:errcheck

	_, err = verifyTx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID)
	require.NoError(t, err)

	err = verifyTx.QueryRow(ctx, "SELECT count(*) FROM workflow_instances WHERE workflow_instance_id = $1", workflowInstanceID).Scan(&instanceCount)
	require.NoError(t, err)

	err = verifyTx.QueryRow(ctx, "SELECT count(*) FROM outbox_events WHERE aggregate_id = $1", workflowInstanceID).Scan(&outboxCount)
	require.NoError(t, err)

	assert.Equal(t, 0, instanceCount, "domain workflow_instance row must not exist after rollback")
	assert.Equal(t, 0, outboxCount, "outbox row must not exist after rollback")
}
