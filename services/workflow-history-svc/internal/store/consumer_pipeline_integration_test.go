//go:build integration

// Package store_test provides consumer-pipeline integration tests.
//
// These tests exercise the FULL pipeline that was previously untested:
//
//	raw Kafka message bytes
//	     → consumer.Consumer.Handle()
//	          → store.PgStore.Append()   (ON CONFLICT dedup)
//	               → PostgreSQL (embedded, real binary)
//	                    → store.PgStore.ListByInstance() read-back
//
// This is the gap the reviewer identified: the cmd/server integration tests
// inserted rows via pgStore.Append directly, never exercising consumer.Handle.
// These tests prove:
//   - The consumer correctly parses the event envelope workflow-svc emits.
//   - tenant_id/legal_entity_id are stored from workflow.started payloads.
//   - Subsequent transition events inherit tenant context from the started row.
//   - ON CONFLICT idempotency works at the PgStore level.
//   - The fail-closed behaviour (no started row → error, no commit) fires correctly.
//
// No Kafka broker is needed — the Kafka Runner is NOT wired here. The test
// calls consumer.Handle with the exact bytes that workflow-svc's publisher would
// produce, proving the message format contract is correctly implemented.
//
// Run with:
//
//	go test -v -tags=integration -count=1 -timeout=120s ./internal/store/
package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/workflow-history-svc/internal/consumer"
	"zoiko.io/workflow-history-svc/internal/store"
)

// buildMessage constructs the exact JSON envelope that workflow-svc's
// internal/events/publisher.go emits (as of the publisher fix in this PR).
// The format is:
//
//	{
//	  "event_type":     "...",
//	  "emitted_at":     "...",
//	  "schema_version": "1.0",
//	  "source_service": "workflow-svc",
//	  "correlation_id": "...",
//	  "payload":        {...}
//	}
//
// This is the exact contract consumer.Consumer.Handle() expects. If the
// publisher changes the shape, this test must be updated — that coupling is
// intentional: it detects shape drift between producer and consumer.
func buildMessage(t *testing.T, eventType, correlationID string, payload map[string]any) []byte {
	t.Helper()
	rawPayload, err := json.Marshal(payload)
	require.NoError(t, err)
	env := map[string]any{
		"event_type":     eventType,
		"emitted_at":     time.Now().UTC().Format(time.RFC3339),
		"schema_version": "1.0",
		"source_service": "workflow-svc",
		"correlation_id": correlationID,
		"payload":        json.RawMessage(rawPayload),
	}
	data, err := json.Marshal(env)
	require.NoError(t, err)
	return data
}

// TestConsumerPipelineIntegration boots a real embedded Postgres, applies the
// migration, and runs the full consumer → store pipeline against real SQL.
func TestConsumerPipelineIntegration(t *testing.T) {
	// ── Boot embedded Postgres ────────────────────────────────────────────────
	dbPort := uint32(15433 + uint32(os.Getpid()%500))
	pg := embeddedpostgres.NewDatabase(
		embeddedpostgres.DefaultConfig().
			Version(embeddedpostgres.PostgresVersion("16.0.0")).
			Port(dbPort).
			Database("workflow_history_consumer_test").
			Username("postgres").
			Password("postgres"),
	)
	require.NoError(t, pg.Start(), "embedded postgres failed to start")
	defer func() { _ = pg.Stop() }()

	dsn := fmt.Sprintf(
		"host=localhost port=%d dbname=workflow_history_consumer_test user=postgres password=postgres sslmode=disable",
		dbPort,
	)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()

	require.Eventually(t, func() bool {
		return pool.Ping(ctx) == nil
	}, 10*time.Second, 200*time.Millisecond, "embedded postgres did not become ready")

	migrationSQL, err := os.ReadFile("../../deployments/migrations/000001_initial_schema.up.sql")
	require.NoError(t, err, "could not read migration file")
	_, err = pool.Exec(ctx, string(migrationSQL))
	require.NoError(t, err, "migration failed")

	// ── Wire real PgStore + Consumer ─────────────────────────────────────────
	log := zap.NewNop()
	pgStore := store.NewPgStore(pool, log)
	c := consumer.New(pgStore, pgStore, log)

	// ── Subtests ─────────────────────────────────────────────────────────────

	t.Run("workflow.started is stored with tenant context", func(t *testing.T) {
		raw := buildMessage(t, "workflow.started", "corr-001", map[string]any{
			"workflow_instance_id": "wf-pipeline-001",
			"tenant_id":            "t-pipeline",
			"legal_entity_id":      "e-pipeline",
			"workflow_type":        "invoice_approval",
			"initiated_by":         "u-001",
			"started_at":           time.Now().UTC().Format(time.RFC3339),
		})

		err := c.Handle(ctx, "evt-pipeline-001", raw)
		require.NoError(t, err)

		events, err := pgStore.ListByInstance(ctx, "wf-pipeline-001")
		require.NoError(t, err)
		require.Len(t, events, 1)
		assert.Equal(t, "workflow.started", events[0].EventType)
		assert.Equal(t, "t-pipeline", events[0].TenantID)
		assert.Equal(t, "e-pipeline", events[0].LegalEntityID)
		assert.Equal(t, "corr-001", events[0].CorrelationID)
		assert.Equal(t, "evt-pipeline-001", events[0].EventID)
	})

	t.Run("approval.granted inherits tenant context from started row", func(t *testing.T) {
		// The workflow.started row was already stored by the previous subtest.
		raw := buildMessage(t, "approval.granted", "corr-001", map[string]any{
			"workflow_instance_id":  "wf-pipeline-001",
			"stage_order":           1,
			"approver_principal_id": "u-approver-001",
		})

		err := c.Handle(ctx, "evt-pipeline-002", raw)
		require.NoError(t, err)

		events, err := pgStore.ListByInstance(ctx, "wf-pipeline-001")
		require.NoError(t, err)
		require.Len(t, events, 2)

		granted := events[1]
		assert.Equal(t, "approval.granted", granted.EventType)
		// Must inherit tenant context — not sourced from the payload.
		assert.Equal(t, "t-pipeline", granted.TenantID)
		assert.Equal(t, "e-pipeline", granted.LegalEntityID)
	})

	t.Run("workflow.completed stored with inherited tenant context", func(t *testing.T) {
		raw := buildMessage(t, "workflow.completed", "corr-001", map[string]any{
			"workflow_instance_id": "wf-pipeline-001",
			"workflow_status":      "approved",
			"completed_at":         time.Now().UTC().Format(time.RFC3339),
		})

		err := c.Handle(ctx, "evt-pipeline-003", raw)
		require.NoError(t, err)

		events, err := pgStore.ListByInstance(ctx, "wf-pipeline-001")
		require.NoError(t, err)
		require.Len(t, events, 3)
		assert.Equal(t, "workflow.completed", events[2].EventType)
		assert.Equal(t, "t-pipeline", events[2].TenantID)
	})

	t.Run("ON CONFLICT idempotency — duplicate event_id is a no-op at PgStore level", func(t *testing.T) {
		raw := buildMessage(t, "workflow.started", "corr-idem", map[string]any{
			"workflow_instance_id": "wf-idem-pipeline",
			"tenant_id":            "t-idem",
			"legal_entity_id":      "e-idem",
		})

		// First delivery.
		require.NoError(t, c.Handle(ctx, "evt-idem-001", raw))
		// Broker redelivery — same event_id, must be silently ignored.
		require.NoError(t, c.Handle(ctx, "evt-idem-001", raw))

		events, err := pgStore.ListByInstance(ctx, "wf-idem-pipeline")
		require.NoError(t, err)
		assert.Len(t, events, 1, "duplicate event_id must produce exactly one row")
	})

	t.Run("fail-closed: transition without started row returns error (no commit)", func(t *testing.T) {
		raw := buildMessage(t, "approval.granted", "corr-orphan", map[string]any{
			"workflow_instance_id":  "wf-orphan-pipeline",
			"stage_order":           1,
			"approver_principal_id": "u-002",
		})

		// No workflow.started row exists for wf-orphan-pipeline.
		// Consumer must return a non-nil error so the runner does NOT commit.
		err := c.Handle(ctx, "evt-orphan-001", raw)
		require.Error(t, err, "expected fail-closed error when no started row exists")

		// No row should have been stored.
		events, qErr := pgStore.ListByInstance(ctx, "wf-orphan-pipeline")
		require.NoError(t, qErr)
		assert.Empty(t, events)
	})

	t.Run("envelope validation: missing source_service rejected before store", func(t *testing.T) {
		env := map[string]any{
			"event_type":     "workflow.started",
			"emitted_at":     time.Now().UTC().Format(time.RFC3339),
			"schema_version": "1.0",
			// source_service intentionally absent
			"correlation_id": "corr-nosvc",
			"payload": map[string]any{
				"workflow_instance_id": "wf-nosvc",
				"tenant_id":            "t-nosvc",
				"legal_entity_id":      "e-nosvc",
			},
		}
		data, _ := json.Marshal(env)

		err := c.Handle(ctx, "evt-nosvc-001", data)
		require.NoError(t, err) // rejection is non-fatal

		events, qErr := pgStore.ListByInstance(ctx, "wf-nosvc")
		require.NoError(t, qErr)
		assert.Empty(t, events, "malformed message must not produce a DB row")
	})

	t.Run("all five event types processed end-to-end", func(t *testing.T) {
		instanceID := "wf-all-types"

		steps := []struct {
			id      string
			evtType string
			payload map[string]any
		}{
			{"evt-all-001", "workflow.started", map[string]any{
				"workflow_instance_id": instanceID, "tenant_id": "t-all", "legal_entity_id": "e-all",
			}},
			{"evt-all-002", "approval.granted", map[string]any{
				"workflow_instance_id": instanceID, "stage_order": 1, "approver_principal_id": "u-a",
			}},
			{"evt-all-003", "approval.rejected", map[string]any{
				"workflow_instance_id": instanceID, "stage_order": 2, "approver_principal_id": "u-b",
			}},
			{"evt-all-004", "workflow.escalated", map[string]any{
				"workflow_instance_id": instanceID, "current_stage": 2,
			}},
			{"evt-all-005", "workflow.completed", map[string]any{
				"workflow_instance_id": instanceID, "workflow_status": "approved",
			}},
		}

		for _, s := range steps {
			raw := buildMessage(t, s.evtType, "corr-all", s.payload)
			require.NoError(t, c.Handle(ctx, s.id, raw), "failed on event type %q", s.evtType)
		}

		events, err := pgStore.ListByInstance(ctx, instanceID)
		require.NoError(t, err)
		require.Len(t, events, 5, "all five event types must be persisted")

		expectedTypes := []string{
			"workflow.started",
			"approval.granted",
			"approval.rejected",
			"workflow.escalated",
			"workflow.completed",
		}
		for i, e := range events {
			assert.Equal(t, expectedTypes[i], e.EventType)
			assert.Equal(t, "t-all", e.TenantID)
			assert.Equal(t, "e-all", e.LegalEntityID)
		}
	})
}
