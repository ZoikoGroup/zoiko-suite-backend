package store_test

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/evidence-manifest-svc/internal/domain"
	svcmiddleware "zoiko.io/evidence-manifest-svc/internal/middleware"
	"zoiko.io/evidence-manifest-svc/internal/outbox"
	"zoiko.io/evidence-manifest-svc/internal/store"
)

func getTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping real Postgres test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { pool.Close() })
	return pool
}

func TestPgStore_Outbox_ForcedFailure_RollbackAtomicity_RealDB(t *testing.T) {
	pool := getTestPool(t)
	ctx := context.Background()

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	manifestID := uuid.New().String()
	correlationID := uuid.New().String()
	actorID := "user-actor-1"
	outboxEventID := uuid.New().String()

	// 1. Begin a real transaction
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx) //nolint:errcheck

	// Set tenant context for RLS in this tx
	_, err = tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID)
	require.NoError(t, err)

	// 2. Insert initial manifest
	const insertManifest = `
		INSERT INTO evidence_manifests (
			manifest_id, tenant_id, legal_entity_id, scenario_type, requested_by, status
		) VALUES ($1, $2, $3, 'AUDIT', $4, 'PENDING');
	`
	_, err = tx.Exec(ctx, insertManifest, manifestID, tenantID, legalEntityID, actorID)
	require.NoError(t, err)

	// 3. Update manifest to GENERATED
	_, err = tx.Exec(ctx, `
		UPDATE evidence_manifests SET status = 'GENERATED', checksum_sha256 = 'chk-123', generated_at = now()
		WHERE manifest_id = $1
	`, manifestID)
	require.NoError(t, err)

	// 4. Write the outbox row
	err = outbox.Insert(ctx, tx, outbox.Event{
		OutboxEventID: outboxEventID,
		AggregateType: "MANIFEST",
		AggregateID:   manifestID,
		EventType:     "evidence.manifest.generated",
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		ActorID:       &actorID,
		CorrelationID: correlationID,
		Payload:       map[string]any{"manifest_id": manifestID},
	})
	require.NoError(t, err)

	// 5. Deliberately fail the transaction before commit
	// (Trigger a primary key constraint collision on outbox_events, forcing a failure)
	_, err = tx.Exec(ctx, "INSERT INTO outbox_events (outbox_event_id) VALUES ($1)", outboxEventID)
	require.Error(t, err, "expected duplicate primary key constraint error")

	// Explicitly roll back the transaction
	err = tx.Rollback(ctx)
	require.NoError(t, err)

	// 6. Query fresh connection (pool) and assert NEITHER row exists
	var manifestCount, outboxCount int
	verifyTx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer verifyTx.Rollback(ctx) //nolint:errcheck

	_, err = verifyTx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID)
	require.NoError(t, err)

	err = verifyTx.QueryRow(ctx, "SELECT count(*) FROM evidence_manifests WHERE manifest_id = $1", manifestID).Scan(&manifestCount)
	require.NoError(t, err)

	err = verifyTx.QueryRow(ctx, "SELECT count(*) FROM outbox_events WHERE aggregate_id = $1", manifestID).Scan(&outboxCount)
	require.NoError(t, err)

	assert.Equal(t, 0, manifestCount, "domain evidence_manifests row must not exist after rollback")
	assert.Equal(t, 0, outboxCount, "outbox row must not exist after rollback")
}

func TestPgStore_FinalizeGenerated_AtomicallyCreatesOutboxEvent_RealDB(t *testing.T) {
	pool := getTestPool(t)
	ctx := context.Background()

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	s := store.New(pool, zap.NewNop())

	tenantCtx := svcmiddleware.WithTenant(ctx, tenantID)

	// Create manifest
	m := &domain.EvidenceManifest{
		LegalEntityID: legalEntityID,
		ScenarioType:  domain.ScenarioAudit,
		RequestedBy:   "tester-user",
	}
	err := s.CreateManifest(tenantCtx, m)
	require.NoError(t, err)

	// Finalize manifest
	checksum := "abcd1234ef5678"
	correlationID := "corr-test-999"
	finalized, err := s.FinalizeGenerated(tenantCtx, m.ManifestID, checksum, correlationID)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusGenerated, finalized.Status)

	// Verify outbox row exists in outbox_events
	var outboxCount int
	var eventType, eventCorrelationID string
	err = pool.QueryRow(ctx, `
		SELECT count(*), coalesce(min(event_type), ''), coalesce(min(correlation_id), '')
		FROM outbox_events
		WHERE aggregate_id = $1 AND aggregate_type = 'MANIFEST'
	`, m.ManifestID).Scan(&outboxCount, &eventType, &eventCorrelationID)
	require.NoError(t, err)

	assert.Equal(t, 1, outboxCount, "expected exactly 1 outbox event for finalized manifest")
	assert.Equal(t, "evidence.manifest.generated", eventType)
	assert.Equal(t, correlationID, eventCorrelationID)
}
