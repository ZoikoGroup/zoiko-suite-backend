package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/evidence-manifest-svc/internal/domain"
	"zoiko.io/evidence-manifest-svc/internal/store"
)

// AUD-03 / AUD-04 events. For every event the same four properties are pinned:
//   1. a real transition writes exactly ONE event, with the verified actor,
//      correlation id, tenant and legal entity;
//   2. an idempotent replay or refused request writes none;
//   3. a failed transaction writes none and leaves the state unchanged
//      (the event and the state change are one atomic unit);
//   4. no unrelated event appears alongside it.

type outboxRow struct {
	EventType, AggregateType, AggregateID, TenantID, LegalEntityID, Actor, Correlation string
	Payload                                                                            map[string]any
}

func eventRows(t *testing.T, pool *pgxpool.Pool, eventType, aggregateID string) []outboxRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT event_type, aggregate_type, aggregate_id, tenant_id::text, COALESCE(legal_entity_id::text,''),
		       COALESCE(actor_id,''), correlation_id, payload
		FROM outbox_events WHERE event_type=$1 AND aggregate_id=$2 ORDER BY created_at`, eventType, aggregateID)
	require.NoError(t, err)
	defer rows.Close()
	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		var payload []byte
		require.NoError(t, rows.Scan(&r.EventType, &r.AggregateType, &r.AggregateID, &r.TenantID, &r.LegalEntityID, &r.Actor, &r.Correlation, &payload))
		require.NoError(t, json.Unmarshal(payload, &r.Payload))
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

func countEvents(t *testing.T, pool *pgxpool.Pool, eventType, aggregateID string) int {
	t.Helper()
	return len(eventRows(t, pool, eventType, aggregateID))
}

func assertOneEvent(t *testing.T, pool *pgxpool.Pool, eventType, aggregateID, actor, correlation, legalEntity string) outboxRow {
	t.Helper()
	got := eventRows(t, pool, eventType, aggregateID)
	require.Len(t, got, 1, "expected exactly one %s event", eventType)
	e := got[0]
	assert.Equal(t, testTenant, e.TenantID)
	assert.Equal(t, legalEntity, e.LegalEntityID)
	assert.Equal(t, actor, e.Actor)
	assert.Equal(t, correlation, e.Correlation)
	assert.Equal(t, eventType, e.Payload["event_type"])
	assert.Equal(t, "evidence-manifest-svc", e.Payload["source_service"])
	assert.Equal(t, "1.0", e.Payload["event_version"])
	assert.Equal(t, aggregateID, e.Payload["aggregate_id"])
	assert.NotEmpty(t, e.Payload["event_id"])
	assert.NotEmpty(t, e.Payload["occurred_at"])
	return e
}

// ── AUD-03 ───────────────────────────────────────────────────────────────────

func TestEvent_AuditPopulationBuilt(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenant)

	pop, _, err := s.DefinePopulation(ctx, definePopulationParams("evb-define"))
	require.NoError(t, err)
	assert.Zero(t, countEvents(t, pool, store.EventAuditPopulationBuilt, pop.PopulationID), "defining is not building")

	params := domain.BuildPopulationParams{PopulationID: pop.PopulationID, TenantID: testTenant, CorrelationID: "evb-build", ActorPrincipalID: "builder-1",
		Rows: []domain.BuildPopulationRow{{SourceRecordID: "JE-1", Amount: floatPtr(1), RowSnapshot: []byte(`{}`)}}}
	_, changed, err := s.BuildPopulation(ctx, params)
	require.NoError(t, err)
	require.True(t, changed)
	e := assertOneEvent(t, pool, store.EventAuditPopulationBuilt, pop.PopulationID, "builder-1", "evb-build", pop.LegalEntityID)
	assert.Equal(t, "AUDIT_POPULATION", e.AggregateType)

	// Replay: no transition, no second event.
	_, changed, err = s.BuildPopulation(ctx, params)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, 1, countEvents(t, pool, store.EventAuditPopulationBuilt, pop.PopulationID))
}

func TestEvent_AuditPopulationFrozen_AndNoValidationFailedOnSuccess(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenant)

	pop := buildValidating(t, s, ctx, "evf", map[string]float64{"record_count": 2, "sum_amount": 300})
	params := domain.FreezePopulationParams{PopulationID: pop.PopulationID, TenantID: testTenant, CorrelationID: "evf-freeze", ActorPrincipalID: "freezer-1"}

	_, changed, err := s.FreezePopulation(ctx, params)
	require.NoError(t, err)
	require.True(t, changed)
	e := assertOneEvent(t, pool, store.EventAuditPopulationFrozen, pop.PopulationID, "freezer-1", "evf-freeze", pop.LegalEntityID)
	assert.NotEmpty(t, e.Payload["details"].(map[string]any)["digest_sha256"])
	assert.Zero(t, countEvents(t, pool, store.EventPopulationValidationFail, pop.PopulationID))

	_, changed, err = s.FreezePopulation(ctx, params) // idempotent replay
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, 1, countEvents(t, pool, store.EventAuditPopulationFrozen, pop.PopulationID))
}

func TestEvent_PopulationValidationFailed_OnMismatchQuarantine_Only(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenant)

	pop := buildValidating(t, s, ctx, "evvf", map[string]float64{"record_count": 2, "sum_amount": 999})
	params := domain.FreezePopulationParams{PopulationID: pop.PopulationID, TenantID: testTenant, CorrelationID: "evvf-freeze", ActorPrincipalID: "freezer-1"}

	_, _, err := s.FreezePopulation(ctx, params)
	require.ErrorIs(t, err, domain.ErrPopulationControlTotalMismatch)
	e := assertOneEvent(t, pool, store.EventPopulationValidationFail, pop.PopulationID, "freezer-1", "evvf-freeze", pop.LegalEntityID)
	assert.Contains(t, e.Payload["details"].(map[string]any)["reason"], "sum_amount")
	assert.Zero(t, countEvents(t, pool, store.EventAuditPopulationFrozen, pop.PopulationID), "a quarantined population is not frozen")

	// Retrying a quarantined population is refused and emits nothing more.
	_, _, err = s.FreezePopulation(ctx, params)
	require.ErrorIs(t, err, domain.ErrPopulationInvalidState)
	assert.Equal(t, 1, countEvents(t, pool, store.EventPopulationValidationFail, pop.PopulationID))
}

func TestEvent_PopulationSuperseded_ViaSupersede(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenant)

	pop := buildAndFreezePopulation(t, s, ctx, "evs", 3)
	params := domain.SupersedePopulationParams{PopulationID: pop.PopulationID, TenantID: testTenant, Reason: "restated", CorrelationID: "evs-sup", ActorPrincipalID: "sup-1"}

	_, changed, err := s.SupersedePopulation(ctx, params)
	require.NoError(t, err)
	require.True(t, changed)
	assertOneEvent(t, pool, store.EventPopulationSuperseded, pop.PopulationID, "sup-1", "evs-sup", pop.LegalEntityID)

	_, changed, err = s.SupersedePopulation(ctx, params)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, 1, countEvents(t, pool, store.EventPopulationSuperseded, pop.PopulationID))
}

func TestEvent_PopulationDeltaAdded_AndOldVersionSuperseded_Once_NoDuplicateOnRetry(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenant)

	pop := buildAndFreezePopulation(t, s, ctx, "evd", 3)
	params := domain.AddControlledDeltaParams{PopulationID: pop.PopulationID, TenantID: testTenant, Reason: "late item", CreatedByPrincipalID: "delta-1", CorrelationID: "evd-delta",
		Rows: []domain.BuildPopulationRow{{SourceRecordID: "LATE-1", Amount: floatPtr(5), RowSnapshot: []byte(`{}`)}}}

	newPop, created, err := s.AddControlledDelta(ctx, params)
	require.NoError(t, err)
	require.True(t, created)

	d := assertOneEvent(t, pool, store.EventPopulationDeltaAdded, pop.PopulationID, "delta-1", "evd-delta", pop.LegalEntityID)
	assert.Equal(t, newPop.PopulationID, d.Payload["details"].(map[string]any)["new_population_id"])
	sup := assertOneEvent(t, pool, store.EventPopulationSuperseded, pop.PopulationID, "delta-1", "evd-delta", pop.LegalEntityID)
	assert.Equal(t, newPop.PopulationID, sup.Payload["details"].(map[string]any)["superseded_by_population_id"])

	// Same correlation id replays to the same new version and emits nothing.
	again, created, err := s.AddControlledDelta(ctx, params)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, newPop.PopulationID, again.PopulationID)
	assert.Equal(t, 1, countEvents(t, pool, store.EventPopulationDeltaAdded, pop.PopulationID))
	assert.Equal(t, 1, countEvents(t, pool, store.EventPopulationSuperseded, pop.PopulationID))
}

func TestEvent_FailedTransaction_WritesNoEvent_AndKeepsState(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenant)

	pop := buildValidating(t, s, ctx, "evtx", map[string]float64{"record_count": 2, "sum_amount": 300})
	failWhen(t, pool, "inj_aud_outbox", "outbox_events", "BEFORE INSERT", "NEW.event_type = 'AuditPopulationFrozen'")

	_, _, err := s.FreezePopulation(ctx, domain.FreezePopulationParams{PopulationID: pop.PopulationID, TenantID: testTenant, CorrelationID: "evtx-freeze", ActorPrincipalID: "freezer-1"})
	require.Error(t, err)
	assert.Zero(t, countEvents(t, pool, store.EventAuditPopulationFrozen, pop.PopulationID))

	persisted, err := s.GetAuditPopulation(ctx, testTenant, pop.PopulationID)
	require.NoError(t, err)
	assert.Equal(t, domain.PopulationValidating, persisted.Status, "the transition must roll back together with its event")
}

// ── AUD-04 ───────────────────────────────────────────────────────────────────

func TestEvent_SampleDesignApproved(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenant)

	pop := buildAndFreezePopulation(t, s, ctx, "eva", 5)
	ps, err := s.CreateSamplingParameterSet(ctx, domain.CreateSamplingParameterSetParams{Approach: domain.SamplingApproachSystematic})
	require.NoError(t, err)
	design, _, err := s.CreateSampleDesign(ctx, domain.CreateSampleDesignParams{PopulationID: pop.PopulationID, Objective: "TEST_COMPLETENESS", ParamSetID: ps.ParamSetID, SampleSize: 2, CreatedByPrincipalID: "preparer-1", CorrelationID: "eva-design"})
	require.NoError(t, err)

	// Refused self-approval: nothing is written.
	_, _, err = s.ApproveSampleDesign(ctx, domain.ApproveSampleDesignParams{DesignID: design.DesignID, ActorPrincipalID: "preparer-1", CorrelationID: "eva-self"})
	require.ErrorIs(t, err, domain.ErrSelfApprovalForbidden)
	assert.Zero(t, countEvents(t, pool, store.EventSampleDesignApproved, design.DesignID))

	// A failing outbox write rolls the approval back with it.
	failWhen(t, pool, "inj_approve", "outbox_events", "BEFORE INSERT", "NEW.event_type = 'SampleDesignApproved'")
	_, _, err = s.ApproveSampleDesign(ctx, domain.ApproveSampleDesignParams{DesignID: design.DesignID, ActorPrincipalID: "reviewer-1", CorrelationID: "eva-fail"})
	require.Error(t, err)
	got, err := s.GetSampleDesign(ctx, testTenant, design.DesignID)
	require.NoError(t, err)
	assert.Equal(t, domain.SampleDesignDraft, got.Status)
	assert.Zero(t, countEvents(t, pool, store.EventSampleDesignApproved, design.DesignID))
	_, _ = pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS inj_approve ON outbox_events`)

	approve := domain.ApproveSampleDesignParams{DesignID: design.DesignID, ActorPrincipalID: "reviewer-1", CorrelationID: "eva-ok"}
	_, changed, err := s.ApproveSampleDesign(ctx, approve)
	require.NoError(t, err)
	require.True(t, changed)
	assertOneEvent(t, pool, store.EventSampleDesignApproved, design.DesignID, "reviewer-1", "eva-ok", pop.LegalEntityID)

	_, changed, err = s.ApproveSampleDesign(ctx, approve) // replay
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, 1, countEvents(t, pool, store.EventSampleDesignApproved, design.DesignID))
}

func TestEvent_SampleSelected(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenant)

	pop, design := fullSampleDesignSetup(t, s, ctx, "evsel", 10, 3, nil)
	params := domain.SelectSampleParams{DesignID: design.DesignID, TenantID: testTenant, CorrelationID: "evsel-select", ActorPrincipalID: "selector-1"}

	_, items, created, err := s.SelectSample(ctx, params)
	require.NoError(t, err)
	require.True(t, created)
	e := assertOneEvent(t, pool, store.EventSampleSelected, design.DesignID, "selector-1", "evsel-select", pop.LegalEntityID)
	assert.EqualValues(t, len(items), e.Payload["details"].(map[string]any)["item_count"])

	_, _, created, err = s.SelectSample(ctx, params) // replay
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, 1, countEvents(t, pool, store.EventSampleSelected, design.DesignID))
}

func TestEvent_SampleItemExceptionRecorded_OnlyWhenAnExceptionIsRecorded(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenant)

	pop, design := fullSampleDesignSetup(t, s, ctx, "evex", 10, 3, nil)
	_, items, _, err := s.SelectSample(ctx, domain.SelectSampleParams{DesignID: design.DesignID, TenantID: testTenant, CorrelationID: "evex-select"})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(items), 3)

	// No exception amount: the result is recorded, no exception event.
	_, _, err = s.RecordItemResult(ctx, domain.RecordItemResultParams{ItemID: items[0].ItemID, TenantID: testTenant, ActorPrincipalID: "tester-1",
		ObjectiveTested: "TEST_COMPLETENESS", Result: "NO_EXCEPTION", CorrelationID: "evex-r0"})
	require.NoError(t, err)
	assert.Zero(t, countEvents(t, pool, store.EventSampleItemExceptionRecord, design.DesignID))

	// With an exception amount: exactly one event.
	_, _, err = s.RecordItemResult(ctx, domain.RecordItemResultParams{ItemID: items[1].ItemID, TenantID: testTenant, ActorPrincipalID: "tester-1",
		ObjectiveTested: "TEST_COMPLETENESS", Result: "EXCEPTION", ExceptionAmount: floatPtr(12.5), CorrelationID: "evex-r1"})
	require.NoError(t, err)
	e := assertOneEvent(t, pool, store.EventSampleItemExceptionRecord, design.DesignID, "tester-1", "evex-r1", pop.LegalEntityID)
	assert.Equal(t, items[1].ItemID, e.Payload["details"].(map[string]any)["item_id"])
	assert.EqualValues(t, 12.5, e.Payload["details"].(map[string]any)["exception_amount"])

	// A second result on the same (now TESTED) item is refused and emits nothing.
	_, _, err = s.RecordItemResult(ctx, domain.RecordItemResultParams{ItemID: items[1].ItemID, TenantID: testTenant, ActorPrincipalID: "tester-1",
		ObjectiveTested: "TEST_COMPLETENESS", Result: "EXCEPTION", ExceptionAmount: floatPtr(99), CorrelationID: "evex-r1b"})
	require.ErrorIs(t, err, domain.ErrSampleItemInvalidState)
	assert.Equal(t, 1, countEvents(t, pool, store.EventSampleItemExceptionRecord, design.DesignID))
}

func TestEvent_SampleEvaluated_OnlyWhenDesignReachesEvaluated(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenant)

	pop, design := fullSampleDesignSetup(t, s, ctx, "evev", 10, 3, nil)
	_, items, _, err := s.SelectSample(ctx, domain.SelectSampleParams{DesignID: design.DesignID, TenantID: testTenant, CorrelationID: "evev-select"})
	require.NoError(t, err)

	// Items still SELECTED: evaluation is incomplete, the design is not
	// evaluated, and no SampleEvaluated event may exist.
	_, _, err = s.EvaluateSample(ctx, domain.EvaluateSampleParams{DesignID: design.DesignID, TenantID: testTenant, CorrelationID: "evev-eval-1", ActorPrincipalID: "eval-1"})
	require.ErrorIs(t, err, domain.ErrSampleEvaluationIncomplete)
	assert.Zero(t, countEvents(t, pool, store.EventSampleEvaluated, design.DesignID))

	for _, it := range items {
		_, _, err := s.RecordItemResult(ctx, domain.RecordItemResultParams{ItemID: it.ItemID, TenantID: testTenant, ActorPrincipalID: "tester-1",
			ObjectiveTested: "TEST_COMPLETENESS", Result: "NO_EXCEPTION", CorrelationID: "evev-r-" + it.ItemID})
		require.NoError(t, err)
	}
	ev, changed, err := s.EvaluateSample(ctx, domain.EvaluateSampleParams{DesignID: design.DesignID, TenantID: testTenant, CorrelationID: "evev-eval-2", ActorPrincipalID: "eval-1"})
	require.NoError(t, err)
	require.True(t, changed)
	e := assertOneEvent(t, pool, store.EventSampleEvaluated, design.DesignID, "eval-1", "evev-eval-2", pop.LegalEntityID)
	assert.Equal(t, ev.EvaluationID, e.Payload["details"].(map[string]any)["evaluation_id"])

	// The design is now EVALUATED; evaluating again is refused and silent.
	_, _, err = s.EvaluateSample(ctx, domain.EvaluateSampleParams{DesignID: design.DesignID, TenantID: testTenant, CorrelationID: "evev-eval-3", ActorPrincipalID: "eval-1"})
	require.ErrorIs(t, err, domain.ErrSampleDesignInvalidState)
	assert.Equal(t, 1, countEvents(t, pool, store.EventSampleEvaluated, design.DesignID))
}

func TestEvent_SampleSuperseded(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenant)

	pop, design := fullSampleDesignSetup(t, s, ctx, "evsup", 10, 3, nil)
	ps2, err := s.CreateSamplingParameterSet(ctx, domain.CreateSamplingParameterSetParams{Approach: domain.SamplingApproachSystematic})
	require.NoError(t, err)
	params := domain.SupersedeSampleParams{DesignID: design.DesignID, TenantID: testTenant, Reason: "new params", NewParamSetID: ps2.ParamSetID, ActorPrincipalID: "sup-1", CorrelationID: "evsup-sup"}

	newDesign, created, err := s.SupersedeSample(ctx, params)
	require.NoError(t, err)
	require.True(t, created)
	e := assertOneEvent(t, pool, store.EventSampleSuperseded, design.DesignID, "sup-1", "evsup-sup", pop.LegalEntityID)
	assert.Equal(t, newDesign.DesignID, e.Payload["details"].(map[string]any)["new_design_id"])

	// The old design is INVALIDATED; superseding it again is refused and silent.
	_, _, err = s.SupersedeSample(ctx, params)
	require.ErrorIs(t, err, domain.ErrSampleDesignInvalidState)
	assert.Equal(t, 1, countEvents(t, pool, store.EventSampleSuperseded, design.DesignID))
}
