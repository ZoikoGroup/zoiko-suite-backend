package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"zoiko.io/ai-governance-svc/internal/domain"
	"zoiko.io/ai-governance-svc/internal/events"
	"zoiko.io/ai-governance-svc/internal/middleware"
	"zoiko.io/ai-governance-svc/internal/outbox"
	storepkg "zoiko.io/ai-governance-svc/internal/store"
)

type retryPublisher struct {
	events []events.Event
	fail   bool
}

func (p *retryPublisher) PublishEvent(_ context.Context, event events.Event) error {
	p.events = append(p.events, event)
	if p.fail {
		p.fail = false
		return errors.New("broker unavailable")
	}
	return nil
}

func TestTransactionalOutbox_RollbackRetryAndStableEventID(t *testing.T) {
	pool := openAdminPool(t)
	require.NoError(t, outbox.VerifySchema(context.Background(), pool))
	ctx := middleware.WithTenant(context.Background(), "11111111-1111-1111-1111-111111111111")
	ctx = middleware.WithPrincipal(ctx, "principal-outbox-test")
	ctx = middleware.WithCorrelationID(ctx, "correlation-outbox-test")
	runID := uuid.NewString()
	run := &domain.AIRun{
		AIRunID:              runID,
		TenantID:             middleware.TenantFromContext(ctx),
		RunType:              domain.AIRunType("RECOMMEND"),
		ModelID:              "model-test",
		PromptVersion:        "prompt-v1",
		AuditID:              "audit-outbox-test",
		UncertaintyState:     domain.UncertaintyNone,
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: middleware.PrincipalFromContext(ctx),
	}
	require.NoError(t, storepkg.NewPgStore(pool).CreateAIRun(ctx, run))

	var eventID, eventType, actorID, correlationID string
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT event_id, event_type, actor_id, correlation_id
		FROM ai_governance_outbox WHERE aggregate_id = $1
	`, runID).Scan(&eventID, &eventType, &actorID, &correlationID))
	require.Equal(t, "ai_run.created", eventType)
	require.Equal(t, "principal-outbox-test", actorID)
	require.Equal(t, "correlation-outbox-test", correlationID)

	rollbackID := uuid.NewString()
	tx, err := pool.Begin(context.Background())
	require.NoError(t, err)
	_, err = tx.Exec(context.Background(), `
		INSERT INTO ai_runs (ai_run_id, tenant_id, run_type, model_id, prompt_version, audit_id, created_by_principal_id)
		VALUES ($1, $2, 'RECOMMEND', 'model-test', 'prompt-v1', 'audit-rollback', 'principal-outbox-test')
	`, rollbackID, middleware.TenantFromContext(ctx))
	require.NoError(t, err)
	require.NoError(t, tx.Rollback(context.Background()))
	var rolledBackCount int
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT count(*) FROM ai_governance_outbox WHERE aggregate_id = $1
	`, rollbackID).Scan(&rolledBackCount))
	require.Zero(t, rolledBackCount, "rolled back business writes must not leave outbox events")

	publisher := &retryPublisher{fail: true}
	relay := outbox.NewRelay(pool, publisher, nil)
	worked, err := relay.PublishOne(context.Background())
	require.True(t, worked)
	require.Error(t, err)
	require.Len(t, publisher.events, 1)
	require.Equal(t, eventID, publisher.events[0].EventID)

	_, err = pool.Exec(context.Background(), `
		UPDATE ai_governance_outbox SET available_at = NOW(), lease_until = NULL
		WHERE event_id = $1
	`, eventID)
	require.NoError(t, err)
	worked, err = relay.PublishOne(context.Background())
	require.True(t, worked)
	require.NoError(t, err)
	require.Len(t, publisher.events, 2)
	require.Equal(t, eventID, publisher.events[1].EventID)

	var rowCount, attempts int
	var publishedAt time.Time
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT count(*), max(attempts), max(published_at)
		FROM ai_governance_outbox WHERE aggregate_id = $1
	`, runID).Scan(&rowCount, &attempts, &publishedAt))
	require.Equal(t, 1, rowCount, "one state mutation must create one outbox row")
	require.Equal(t, 2, attempts)
	require.False(t, publishedAt.IsZero())
}

func TestCreateExecution_IdempotencyConcurrencyAndTenantScope(t *testing.T) {
	pool := openAdminPool(t)
	st := storepkg.NewPgStore(pool)
	tenantA := "11111111-1111-1111-1111-111111111111"
	tenantB := "22222222-2222-2222-2222-222222222222"
	useCaseA := seedActiveUseCase(t, pool, tenantA)
	useCaseB := seedActiveUseCase(t, pool, tenantB)
	releaseID := seedActiveModelRelease(t, pool)
	req := domain.CreateExecutionRequest{
		UseCaseID: useCaseA, ModelReleaseID: releaseID,
		PackageID: "package-immutable-7", PackageVersion: "7.2.1",
		Input: []byte(`{"action":"classify","scope":{"b":2,"a":1}}`),
	}
	ctxA := middleware.WithTenant(context.Background(), tenantA)
	ctxA = middleware.WithPrincipal(ctxA, "maker-a")
	ctxA = middleware.WithCorrelationID(ctxA, "execution-correlation-a")

	first, replayed, err := st.CreateExecution(ctxA, req, "same-op-key", "hash-canonical")
	require.NoError(t, err)
	require.False(t, replayed)
	require.Equal(t, "BLOCKED", first.Status)
	replay, replayed, err := st.CreateExecution(ctxA, req, "same-op-key", "hash-canonical")
	require.NoError(t, err)
	require.True(t, replayed)
	require.Equal(t, first, replay)
	_, _, err = st.CreateExecution(ctxA, req, "same-op-key", "different-hash")
	require.ErrorIs(t, err, domain.ErrIdempotencyConflict)

	var wg sync.WaitGroup
	errs := make(chan error, 12)
	ids := make(chan string, 12)
	for i := 0; i < cap(errs); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, _, callErr := st.CreateExecution(ctxA, req, "concurrent-key", "same-concurrent-hash")
			if callErr != nil {
				errs <- callErr
				return
			}
			ids <- got.ExecutionID
		}()
	}
	wg.Wait()
	close(errs)
	close(ids)
	for callErr := range errs {
		require.NoError(t, callErr)
	}
	var concurrentID string
	for id := range ids {
		if concurrentID == "" {
			concurrentID = id
		}
		require.Equal(t, concurrentID, id, "concurrent retries must return the original execution")
	}

	reqB := req
	reqB.UseCaseID = useCaseB
	ctxB := middleware.WithTenant(context.Background(), tenantB)
	ctxB = middleware.WithPrincipal(ctxB, "maker-b")
	tenantExecution, replayed, err := st.CreateExecution(ctxB, reqB, "same-op-key", "hash-tenant-b")
	require.NoError(t, err)
	require.False(t, replayed)
	require.NotEqual(t, first.ExecutionID, tenantExecution.ExecutionID)
	_, err = st.GetExecution(ctxA, tenantExecution.ExecutionID)
	require.ErrorIs(t, err, domain.ErrExecutionNotFound)

	var count int
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT count(*) FROM ai_executions
		WHERE idempotency_key = 'concurrent-key' AND tenant_id = $1
	`, tenantA).Scan(&count))
	require.Equal(t, 1, count)

	_, err = pool.Exec(context.Background(), `
		INSERT INTO automation_actions (
			tenant_id, action_type, idempotency_key, proposed_by_principal_id
		) VALUES ($1, 'TEST_ACTION', 'same-op-key', 'maker-a')
	`, tenantA)
	require.NoError(t, err)
	var executionEventCount int
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT count(*) FROM ai_governance_outbox
		WHERE event_type = 'ai.execution.blocked'
	`).Scan(&executionEventCount))
	require.Equal(t, 3, executionEventCount, "one event per new execution, including concurrent and other-tenant writes")
}

func TestCreateAIIncident_P0QuarantinesAtomicallyAndReplays(t *testing.T) {
	pool := openAdminPool(t)
	st := storepkg.NewPgStore(pool)
	tenantID := "11111111-1111-1111-1111-111111111111"
	releaseID := seedActiveModelRelease(t, pool)
	ctx := middleware.WithTenant(context.Background(), tenantID)
	ctx = middleware.WithPrincipal(ctx, "incident-reviewer")
	ctx = middleware.WithCorrelationID(ctx, "incident-correlation")
	req := domain.CreateAIIncidentRequest{
		Severity: "AI-P0", ModelReleaseID: releaseID,
		Description:        "Confirmed production safety incident",
		EvidenceReferences: []string{"evidence://incident/1"},
	}

	first, replayed, err := st.CreateAIIncident(ctx, req, "incident-key", "incident-hash")
	require.NoError(t, err)
	require.False(t, replayed)
	require.Equal(t, "OPEN", first.Status)
	replay, replayed, err := st.CreateAIIncident(ctx, req, "incident-key", "incident-hash")
	require.NoError(t, err)
	require.True(t, replayed)
	require.Equal(t, first, replay)
	changed := req
	changed.Description = "changed request"
	_, _, err = st.CreateAIIncident(ctx, changed, "incident-key", "different-hash")
	require.ErrorIs(t, err, domain.ErrIdempotencyConflict)

	var releaseState string
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT release_state FROM ai_model_releases WHERE model_release_id = $1
	`, releaseID).Scan(&releaseState))
	require.Equal(t, string(domain.ReleaseQuarantined), releaseState)

	var incidentEvents, quarantineEvents int
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT count(*) FROM ai_governance_outbox
		WHERE event_type = 'ai.incident.opened' AND aggregate_id = $1
	`, first.IncidentID).Scan(&incidentEvents))
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT count(*) FROM ai_governance_outbox
		WHERE event_type = 'ai.model.release_quarantined' AND aggregate_id = $1
	`, releaseID).Scan(&quarantineEvents))
	require.Equal(t, 1, incidentEvents)
	require.Equal(t, 1, quarantineEvents)
}

func seedActiveUseCase(t *testing.T, pool *pgxpool.Pool, tenantID string) string {
	t.Helper()
	id := uuid.NewString()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO ai_use_cases (
			use_case_id, tenant_id, domain, purpose, outcome_type, operational_class,
			owner_principal_id, business_outcome, automation_level, lifecycle_state,
			created_by_principal_id
		) VALUES ($1, $2, 'test-domain', 'test-purpose', 'classification', 'A1',
			'test-owner', 'test-outcome', 'RECOMMENDATION', 'ACTIVE', 'test-creator')
	`, id, tenantID)
	require.NoError(t, err)
	return id
}

func seedActiveModelRelease(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	id := uuid.NewString()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO ai_model_releases (
			model_release_id, provider, provider_model_id, deployment_region,
			release_state, created_by_principal_id
		) VALUES ($1, 'test-provider', 'test-model', 'eu', 'ACTIVE', 'test-creator')
	`, id)
	require.NoError(t, err)
	return id
}

func seedAIRunForDisposition(t *testing.T, pool *pgxpool.Pool, tenantID string) string {
	t.Helper()
	id := uuid.NewString()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO ai_runs (
			ai_run_id, tenant_id, run_type, model_id, prompt_version, audit_id, created_by_principal_id
		) VALUES ($1, $2, 'RECOMMEND', 'model-test', 'prompt-v1', 'audit-disposition-test', 'test-creator')
	`, id, tenantID)
	require.NoError(t, err)
	return id
}

func TestCreateOutputDisposition_IdempotencyConcurrencyAndO4FailClosed(t *testing.T) {
	pool := openAdminPool(t)
	st := storepkg.NewPgStore(pool)
	tenantID := "11111111-1111-1111-1111-111111111111"
	runID := seedAIRunForDisposition(t, pool, tenantID)
	ctx := middleware.WithTenant(context.Background(), tenantID)
	ctx = middleware.WithPrincipal(ctx, "maker-disposition")
	ctx = middleware.WithCorrelationID(ctx, "disposition-correlation")

	req := domain.CreateOutputDispositionRequest{AIRunID: runID, OversightClass: "O2"}

	first, replayed, err := st.CreateOutputDisposition(ctx, req, "same-op-key", "hash-canonical")
	require.NoError(t, err)
	require.False(t, replayed)
	require.Equal(t, "REVIEW_REQUIRED", first.Status)
	replay, replayed, err := st.CreateOutputDisposition(ctx, req, "same-op-key", "hash-canonical")
	require.NoError(t, err)
	require.True(t, replayed)
	require.Equal(t, first, replay)
	_, _, err = st.CreateOutputDisposition(ctx, req, "same-op-key", "different-hash")
	require.ErrorIs(t, err, domain.ErrIdempotencyConflict)

	// A second, distinct disposition for the SAME ai_run_id must fail —
	// append-only, one disposition per output.
	_, _, err = st.CreateOutputDisposition(ctx, req, "a-different-key-same-run", "some-hash")
	require.ErrorIs(t, err, domain.ErrAIRunAlreadyHasDisposition)

	// O4 is AI-prohibited: must be refused fail-closed, no row created.
	runIDProhibited := seedAIRunForDisposition(t, pool, tenantID)
	_, _, err = st.CreateOutputDisposition(ctx, domain.CreateOutputDispositionRequest{
		AIRunID: runIDProhibited, OversightClass: "O4",
	}, "o4-key", "o4-hash")
	require.ErrorIs(t, err, domain.ErrOversightClassProhibited)
	var o4Count int
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT count(*) FROM ai_output_dispositions WHERE ai_run_id = $1
	`, runIDProhibited).Scan(&o4Count))
	require.Zero(t, o4Count, "O4 must never create a disposition row")

	// Concurrent identical requests under one new key must collapse to one row.
	runIDConcurrent := seedAIRunForDisposition(t, pool, tenantID)
	concurrentReq := domain.CreateOutputDispositionRequest{AIRunID: runIDConcurrent, OversightClass: "O3"}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	ids := make(chan string, 8)
	for i := 0; i < cap(errs); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, _, callErr := st.CreateOutputDisposition(ctx, concurrentReq, "concurrent-disposition-key", "same-concurrent-hash")
			if callErr != nil {
				errs <- callErr
				return
			}
			ids <- got.DispositionID
		}()
	}
	wg.Wait()
	close(errs)
	close(ids)
	for callErr := range errs {
		require.NoError(t, callErr)
	}
	var concurrentID string
	for id := range ids {
		if concurrentID == "" {
			concurrentID = id
		}
		require.Equal(t, concurrentID, id, "concurrent retries must return the original disposition")
	}
	var rowCount int
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT count(*) FROM ai_output_dispositions WHERE ai_run_id = $1
	`, runIDConcurrent).Scan(&rowCount))
	require.Equal(t, 1, rowCount)

	// Outbox: one created event per real write, none for replays.
	var eventCount int
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT count(*) FROM ai_governance_outbox WHERE event_type = 'ai.output_disposition.created'
	`).Scan(&eventCount))
	require.Equal(t, 2, eventCount, "one event for the first real disposition, one for the concurrent-key disposition — none for replays")
}

func TestDecideOutputDisposition_SelfApprovalBlockedDistinctReviewerSucceedsAndEmitsEvent(t *testing.T) {
	pool := openAdminPool(t)
	st := storepkg.NewPgStore(pool)
	tenantID := "11111111-1111-1111-1111-111111111111"
	runID := seedAIRunForDisposition(t, pool, tenantID)
	ctx := middleware.WithTenant(context.Background(), tenantID)
	ctx = middleware.WithPrincipal(ctx, "maker-disposition-sod")
	ctx = middleware.WithCorrelationID(ctx, "disposition-sod-correlation")

	created, _, err := st.CreateOutputDisposition(ctx, domain.CreateOutputDispositionRequest{
		AIRunID: runID, OversightClass: "O2",
	}, "sod-key", "sod-hash")
	require.NoError(t, err)

	selfCtx := middleware.WithPrincipal(middleware.WithTenant(context.Background(), tenantID), "maker-disposition-sod")
	_, err = st.DecideOutputDisposition(selfCtx, created.DispositionID, domain.DecideOutputDispositionRequest{Decision: "ACCEPTED"})
	require.ErrorIs(t, err, domain.ErrSelfApprovalBlocked)

	reviewerCtx := middleware.WithPrincipal(middleware.WithTenant(context.Background(), tenantID), "reviewer-distinct")
	decided, err := st.DecideOutputDisposition(reviewerCtx, created.DispositionID, domain.DecideOutputDispositionRequest{
		Decision: "ACCEPTED", Reason: "evidence verified",
	})
	require.NoError(t, err)
	require.Equal(t, "ACCEPTED", decided.Status)
	require.NotNil(t, decided.DecidedByPrincipalID)
	require.Equal(t, "reviewer-distinct", *decided.DecidedByPrincipalID)

	_, err = st.DecideOutputDisposition(reviewerCtx, created.DispositionID, domain.DecideOutputDispositionRequest{Decision: "REJECTED"})
	require.ErrorIs(t, err, domain.ErrDispositionNotReviewable)

	var decidedEventCount int
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT count(*) FROM ai_governance_outbox
		WHERE event_type = 'ai.output_disposition.decided' AND aggregate_id = $1
	`, created.DispositionID).Scan(&decidedEventCount))
	require.Equal(t, 1, decidedEventCount)
}
