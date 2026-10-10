//go:build integration

// Proof that consolidation-svc's run lifecycle events reach the transactional
// outbox (ZS-EVENT-001 §6) inside the same transaction as the run state
// change they report, carrying the run's own id as the aggregate and the
// pre-standard event_type existing consumers still filter on.
//
// Run: go test -v -tags=integration -count=1 -timeout=400s ./internal/store/
package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/consolidation-svc/internal/domain"
	svcmiddleware "zoiko.io/consolidation-svc/internal/middleware"
)

const (
	typeStarted           = "com.zoikosuite.accounting.consolidation-run.started"
	typeCompleted         = "com.zoikosuite.accounting.consolidation-run.completed"
	typeExceptionDetected = "com.zoikosuite.accounting.consolidation-run.exception-detected"
)

type outboxRow struct {
	eventType string
	payload   string
}

func outboxRowsFor(t *testing.T, pool *pgxpool.Pool, tenantID, aggregateID string) []outboxRow {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	// eventing_outbox is under FORCE ROW LEVEL SECURITY.
	_, err = tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID)
	require.NoError(t, err)
	rows, err := tx.Query(ctx, `
		SELECT event_type, convert_from(payload, 'UTF8')
		  FROM eventing_outbox
		 WHERE tenant_id = $1 AND aggregate_id = $2
		 ORDER BY created_at, outbox_id`, tenantID, aggregateID)
	require.NoError(t, err)
	defer rows.Close()
	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		require.NoError(t, rows.Scan(&r.eventType, &r.payload))
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

func ofType(rows []outboxRow, eventType string) []outboxRow {
	var out []outboxRow
	for _, r := range rows {
		if r.eventType == eventType {
			out = append(out, r)
		}
	}
	return out
}

func createdRun(t *testing.T, pool *pgxpool.Pool) (context.Context, string, *domain.ConsolidationRun) {
	t.Helper()
	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	run := newTestRun(tenantID, "group-le-1")
	run.Status = "RUNNING"
	require.NoError(t, newTestStore(pool).CreateRun(ctx, run, "principal-1", "corr-1"))
	return ctx, tenantID, run
}

func TestPgStore_CreateRun_EnqueuesStartedEvent(t *testing.T) {
	pool := openTestPool(t)
	_, tenantID, run := createdRun(t, pool)

	rows := outboxRowsFor(t, pool, tenantID, run.ConsolidationRunID)
	require.Len(t, rows, 1, "CreateRun must enqueue exactly one event, keyed on the run's own id")
	assert.Equal(t, typeStarted, rows[0].eventType)
	assert.Contains(t, rows[0].payload, `"event_type":"consolidation.run.started"`)
	assert.Contains(t, rows[0].payload, `"subject":"urn:zoikosuite:consolidation-run:`+run.ConsolidationRunID+`"`)
	assert.Contains(t, rows[0].payload, `"legalentityid":"group-le-1"`)
	assert.Contains(t, rows[0].payload, `"actorid":"principal-1"`)
	assert.Contains(t, rows[0].payload, `"correlationid":"corr-1"`)
}

func TestPgStore_CreateRun_InsertFailure_EnqueuesNothing(t *testing.T) {
	pool := openTestPool(t)
	ctx, tenantID, run := createdRun(t, pool)

	// Same primary key again: the INSERT fails, so the event must roll back with it.
	err := newTestStore(pool).CreateRun(ctx, run, "principal-1", "corr-2")
	require.Error(t, err)

	rows := outboxRowsFor(t, pool, tenantID, run.ConsolidationRunID)
	assert.Len(t, rows, 1, "a failed CreateRun must not leave a second started event behind")
}

func TestPgStore_CompleteRun_EnqueuesCompletedEvent(t *testing.T) {
	pool := openTestPool(t)
	ctx, tenantID, run := createdRun(t, pool)

	require.NoError(t, newTestStore(pool).CompleteRun(ctx, run.ConsolidationRunID, "COMPLETED", 0, time.Now().UTC(), "principal-1", "corr-1", 7, nil))

	rows := outboxRowsFor(t, pool, tenantID, run.ConsolidationRunID)
	completed := ofType(rows, typeCompleted)
	require.Len(t, completed, 1)
	assert.Contains(t, completed[0].payload, `"event_type":"consolidation.completed"`)
	assert.Contains(t, completed[0].payload, `"snapshot_count":7`)
	assert.Contains(t, completed[0].payload, `"group_legal_entity_id":"group-le-1"`)
	assert.Contains(t, completed[0].payload, `"fiscal_period":"2026-07"`)
	assert.Empty(t, ofType(rows, typeExceptionDetected), "no exceptions, so no exception.detected event")
}

func TestPgStore_CompleteRun_WithExceptions_EnqueuesExceptionDetectedEvent(t *testing.T) {
	pool := openTestPool(t)
	ctx, tenantID, run := createdRun(t, pool)
	exceptions := []string{"intercompany entry ic-1: source journal unavailable"}

	require.NoError(t, newTestStore(pool).CompleteRun(ctx, run.ConsolidationRunID, "COMPLETED", 1, time.Now().UTC(), "principal-1", "corr-1", 3, exceptions))

	rows := outboxRowsFor(t, pool, tenantID, run.ConsolidationRunID)
	detected := ofType(rows, typeExceptionDetected)
	require.Len(t, detected, 1)
	assert.Contains(t, detected[0].payload, `"event_type":"consolidation.exception.detected"`)
	assert.Contains(t, detected[0].payload, `"intercompany entry ic-1: source journal unavailable"`)
	require.Len(t, ofType(rows, typeCompleted), 1)
	// started, exception-detected, completed — the order the handler used to publish in.
	require.Len(t, rows, 3)
	assert.Equal(t, []string{typeStarted, typeExceptionDetected, typeCompleted},
		[]string{rows[0].eventType, rows[1].eventType, rows[2].eventType})
}

func TestPgStore_CompleteRun_Failed_EnqueuesNothing(t *testing.T) {
	pool := openTestPool(t)
	ctx, tenantID, run := createdRun(t, pool)

	require.NoError(t, newTestStore(pool).CompleteRun(ctx, run.ConsolidationRunID, "FAILED", 1, time.Now().UTC(), "principal-1", "corr-1", 0, nil))

	got, err := newTestStore(pool).GetRun(ctx, run.ConsolidationRunID)
	require.NoError(t, err)
	assert.Equal(t, "FAILED", got.Status)
	rows := outboxRowsFor(t, pool, tenantID, run.ConsolidationRunID)
	require.Len(t, rows, 1, "a FAILED run never emitted completion events, and still must not")
	assert.Equal(t, typeStarted, rows[0].eventType)
}

func TestPgStore_CompleteRun_UnknownRun_EnqueuesNothing(t *testing.T) {
	pool := openTestPool(t)
	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	missing := uuid.NewString()

	err := newTestStore(pool).CompleteRun(ctx, missing, "COMPLETED", 0, time.Now().UTC(), "principal-1", "corr-1", 0, nil)
	require.True(t, errors.Is(err, domain.ErrRunNotFound), "got %v", err)
	assert.Empty(t, outboxRowsFor(t, pool, tenantID, missing))
}
