package store

// Integration tests for the 30 Sep 2026 remediation's SQL: migrations 000003
// and 000004, and every store method they back. Same gate as store_test.go —
// TEST_DATABASE_URL, REQUIRE_DB_TESTS=1 to make a skip a failure. The RLS
// tests run the store through an UNPRIVILEGED role: a superuser bypasses RLS,
// so the same assertions as postgres would pass against no policy at all.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/search-indexer-svc/internal/domain"
)

func seedGeneration(t *testing.T, s *PgStore, c domain.IndexContract) domain.IndexGeneration {
	t.Helper()
	g := domain.IndexGeneration{
		GenerationID: uuid.NewString(), ContractID: c.ContractID, ContractVersion: c.Version,
		ScopeName: c.ScopeName, PhysicalIndex: c.ScopeName + "-g" + uuid.NewString()[:8],
		State: domain.GenerationBuilding, CreatedByPrincipalID: uuid.NewString(),
	}
	require.NoError(t, s.CreateGeneration(context.Background(), g))
	return g
}

// The embedding pin round-trips, and the CHECK refuses a half-set pin.
func TestContract_EmbeddingPinRoundTripsAndIsAllOrNothing(t *testing.T) {
	s, cleanup := testStore(t)
	defer cleanup()
	ctx := context.Background()
	src := seedSource(t, s)

	c := domain.IndexContract{
		ContractID: uuid.NewString(), SourceID: src.SourceID, ScopeName: "sem-" + uuid.NewString()[:8],
		Version: 1, SchemaDigest: "d", State: domain.ContractDraft, FreshnessClass: "S1",
		RetrievalClass: domain.RetrievalR1, AnalyzerProfile: "standard", AuthzAction: "X",
		CreatedByPrincipalID: uuid.NewString(),
		Fields: []domain.SearchFieldDefinition{{FieldID: "title", SourcePath: "title", Type: "TEXT",
			Searchable: true, Returnable: true, SensitivityClass: domain.SensitivityInternal}},
		Embedding: &domain.EmbeddingSpec{Model: "m", ModelVersion: "1", Dimensions: 384,
			SourceFields: []string{"title"}, Preprocessing: "nfkc-ws-v1", Similarity: "cosinesimil"},
	}
	require.NoError(t, s.CreateContract(ctx, c))
	got, err := s.GetContract(ctx, c.ContractID)
	require.NoError(t, err)
	require.NotNil(t, got.Embedding)
	assert.Equal(t, *c.Embedding, *got.Embedding)

	lexical := seedContract(t, s, src, "lex-"+uuid.NewString()[:8], domain.ContractDraft)
	got, err = s.GetContract(ctx, lexical.ContractID)
	require.NoError(t, err)
	assert.Nil(t, got.Embedding, "a lexical contract has no pin at all")

	_, err = s.pool.Exec(ctx, `UPDATE index_contracts SET embedding_model = 'x' WHERE contract_id = $1`, lexical.ContractID)
	require.Error(t, err, "index_contracts_embedding_complete must refuse a partial pin")
}

// A checkpoint is read by partition: a retired generation's row never speaks
// for the active one.
func TestGetCheckpoint_IsByPartition(t *testing.T) {
	s, cleanup := testStore(t)
	defer cleanup()
	ctx := context.Background()

	require.NoError(t, s.UpsertCheckpoint(ctx, domain.IndexCheckpoint{ScopeName: "sc", SourcePartition: "old",
		Watermark: 5, CommittedAt: time.Now(), Freshness: domain.FreshnessCurrent}))
	_, err := s.GetCheckpoint(ctx, "sc", "new")
	assert.True(t, errors.Is(err, domain.ErrNotFound))

	require.NoError(t, s.UpsertCheckpoint(ctx, domain.IndexCheckpoint{ScopeName: "sc", SourcePartition: "new",
		Watermark: 1, CommittedAt: time.Now(), Freshness: domain.FreshnessStale, LagMS: 900}))
	cp, err := s.GetCheckpoint(ctx, "sc", "new")
	require.NoError(t, err)
	assert.Equal(t, domain.FreshnessStale, cp.Freshness)
	assert.Equal(t, int64(900), cp.LagMS)
}

// ESR-018's query, through RLS as a non-superuser: only this tenant's FAILED
// restrictions in this scope, and limit+1 rows so "more than the cap" is
// detectable.
func TestListFailedRestrictions_TenantScopedUnderRLS(t *testing.T) {
	s, cleanup := testStore(t)
	defer cleanup()
	ctx := context.Background()
	tenantA, tenantB := uuid.NewString(), uuid.NewString()

	for i, tenant := range []string{tenantA, tenantA, tenantA, tenantB} {
		tomb := domain.RestrictionTombstone{TenantID: tenant, ScopeName: "sc", SourceType: "doc",
			SourceID: "r" + string(rune('0'+i)), Reason: "x", Epoch: int64(i + 1),
			SourceEventID: uuid.NewString(), EffectiveAt: time.Now()}
		_, err := s.UpsertTombstone(ctx, tomb, uuid.NewString())
		require.NoError(t, err)
		state := domain.PropagationFailed
		if i == 2 {
			state = domain.PropagationVerified
		}
		require.NoError(t, s.MarkTombstoneState(ctx, tenant, "doc", tomb.SourceID, tomb.SourceEventID, state, ""))
	}

	pool := nonSuperuserPool(t, s)
	defer pool.Close()
	app := New(pool, zap.NewNop())

	ids, err := app.ListFailedRestrictions(ctx, tenantA, "sc", 500)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"r0", "r1"}, ids, "FAILED only, this tenant only")

	ids, err = app.ListFailedRestrictions(ctx, tenantA, "sc", 1)
	require.NoError(t, err)
	assert.Len(t, ids, 2, "limit+1 rows, so the caller can see the cap was exceeded")

	_, err = app.ListFailedRestrictions(ctx, "", "sc", 10)
	assert.ErrorIs(t, err, domain.ErrTenantRequired)
}

// The critical fix's SQL: a restriction stamped into the ledger at its epoch
// refuses every later ordinary projection — including a NEWER source version.
func TestLedger_RestrictionEpochRefusesLaterOrdinaryUpdates(t *testing.T) {
	s, cleanup := testStore(t)
	defer cleanup()
	ctx := context.Background()
	tenant := uuid.NewString()
	rec := domain.ProjectionRecord{TenantID: tenant, ScopeName: "sc", SourceType: "doc", SourceID: "r3", SourceVersion: 10}

	applied, err := s.UpsertProjectionRecord(ctx, rec)
	require.NoError(t, err)
	require.True(t, applied)

	erased := rec
	erased.RestrictionEpoch, erased.Tombstoned = time.Now().UnixMilli(), true
	applied, err = s.UpsertProjectionRecord(ctx, erased)
	require.NoError(t, err)
	require.True(t, applied, "a newer epoch is accepted")

	later := rec
	later.SourceVersion = 999
	applied, err = s.UpsertProjectionRecord(ctx, later)
	require.NoError(t, err)
	assert.False(t, applied, "an ordinary update — even a newer version — cannot resurrect an erased record")

	got, err := s.GetProjectionRecord(ctx, tenant, "sc", "doc", "r3")
	require.NoError(t, err)
	assert.True(t, got.Tombstoned)
}

// Idempotency, through RLS as a non-superuser.
func TestIdempotency_ClaimReplayMismatchReleaseUnderRLS(t *testing.T) {
	s, cleanup := testStore(t)
	defer cleanup()
	ctx := context.Background()
	pool := nonSuperuserPool(t, s)
	defer pool.Close()
	app := New(pool, zap.NewNop())
	tenant := uuid.NewString()

	rec, err := app.ClaimIdempotencyKey(ctx, tenant, "POST /v1/search-exports", "k1", "fp-a")
	require.NoError(t, err)
	require.Nil(t, rec, "first claim executes")

	rec, err = app.ClaimIdempotencyKey(ctx, tenant, "POST /v1/search-exports", "k1", "fp-a")
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Zero(t, rec.ResponseStatus, "in flight")

	require.NoError(t, app.CompleteIdempotencyKey(ctx, tenant, "POST /v1/search-exports", "k1", 202, []byte(`{"export_id":"x"}`)))
	rec, err = app.ClaimIdempotencyKey(ctx, tenant, "POST /v1/search-exports", "k1", "fp-a")
	require.NoError(t, err)
	assert.Equal(t, 202, rec.ResponseStatus)
	assert.JSONEq(t, `{"export_id":"x"}`, string(rec.ResponseBody))

	_, err = app.ClaimIdempotencyKey(ctx, tenant, "POST /v1/search-exports", "k1", "fp-b")
	assert.ErrorIs(t, err, ErrIdempotencyFingerprintMismatch)

	// Another tenant's identical key is a different key: RLS and the primary key.
	rec, err = app.ClaimIdempotencyKey(ctx, uuid.NewString(), "POST /v1/search-exports", "k1", "fp-b")
	require.NoError(t, err)
	assert.Nil(t, rec)

	// A released in-flight claim can be claimed again.
	_, err = app.ClaimIdempotencyKey(ctx, tenant, "POST /v1/index-contracts", "k2", "fp")
	require.NoError(t, err)
	require.NoError(t, app.ReleaseIdempotencyKey(ctx, tenant, "POST /v1/index-contracts", "k2"))
	rec, err = app.ClaimIdempotencyKey(ctx, tenant, "POST /v1/index-contracts", "k2", "fp")
	require.NoError(t, err)
	assert.Nil(t, rec)

	// The purge runs platform-scoped and sees every tenant's rows.
	n, err := app.PurgeIdempotencyKeysBefore(ctx, time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, int64(3))
}

// An abandoned claim (process died mid-command) is taken over by the SAME
// request after the lease, and still refused to a different one.
func TestIdempotency_AbandonedClaimTakeover(t *testing.T) {
	s, cleanup := testStore(t)
	defer cleanup()
	ctx := context.Background()
	tenant := uuid.NewString()

	_, err := s.ClaimIdempotencyKey(ctx, tenant, "POST /x", "k", "fp")
	require.NoError(t, err)
	_, err = s.pool.Exec(ctx, `UPDATE idempotency_keys SET created_at = now() - interval '1 hour' WHERE idempotency_key = 'k'`)
	require.NoError(t, err)

	_, err = s.ClaimIdempotencyKey(ctx, tenant, "POST /x", "k", "other")
	assert.ErrorIs(t, err, ErrIdempotencyFingerprintMismatch)
	rec, err := s.ClaimIdempotencyKey(ctx, tenant, "POST /x", "k", "fp")
	require.NoError(t, err)
	assert.Nil(t, rec, "the identical retry takes the abandoned claim over and executes")
}

// Backfill state and retrieval evaluations round-trip; the latest evaluation
// is the one that counts.
func TestGeneration_BackfillStateAndEvaluations(t *testing.T) {
	s, cleanup := testStore(t)
	defer cleanup()
	ctx := context.Background()
	src := seedSource(t, s)
	c := seedContract(t, s, src, "bf-"+uuid.NewString()[:8], domain.ContractPublished)
	g := seedGeneration(t, s, c)

	got, err := s.GetGeneration(ctx, g.GenerationID)
	require.NoError(t, err)
	assert.Equal(t, domain.BackfillState(""), got.BackfillState)

	require.NoError(t, s.SetBackfillState(ctx, g.GenerationID, domain.BackfillComplete, "replayed=3 written=3"))
	got, err = s.GetGeneration(ctx, g.GenerationID)
	require.NoError(t, err)
	assert.Equal(t, domain.BackfillComplete, got.BackfillState)
	assert.Equal(t, "replayed=3 written=3", got.BackfillNote)

	_, err = s.pool.Exec(ctx, `UPDATE index_generations SET backfill_state = 'BOGUS' WHERE generation_id = $1`, g.GenerationID)
	require.Error(t, err, "index_generations_backfill_state must refuse an unknown state")

	_, err = s.LatestRetrievalEvaluation(ctx, g.GenerationID)
	assert.True(t, errors.Is(err, domain.ErrNotFound))

	for i, passed := range []bool{true, false} {
		require.NoError(t, s.CreateRetrievalEvaluation(ctx, domain.RetrievalEvaluation{
			EvaluationID: uuid.NewString(), GenerationID: g.GenerationID, ScopeName: g.ScopeName,
			PinnedModel: "m@1", K: 10, Cases: 5, MinRecall: 0.8, Recall: 0.9 - float64(i)*0.5,
			Passed: passed, CasesDigest: "d", CreatedByPrincipalID: uuid.NewString(),
		}))
		time.Sleep(10 * time.Millisecond)
	}
	latest, err := s.LatestRetrievalEvaluation(ctx, g.GenerationID)
	require.NoError(t, err)
	assert.False(t, latest.Passed, "a pass followed by a fail is a fail")
}

// Evidence records the model that answered a semantic search.
func TestEvidence_RecordsTheEmbeddingModel(t *testing.T) {
	s, cleanup := testStore(t)
	defer cleanup()
	ctx := context.Background()
	tenant := uuid.NewString()
	require.NoError(t, s.RecordEvidence(ctx, domain.SearchEvidence{
		EvidenceID: uuid.NewString(), RequestID: "r", CorrelationID: "c", TenantID: tenant,
		ActorID: uuid.NewString(), Purpose: "P", ScopeName: "sc", QueryDigest: "q",
		FiltersDigest: "f", PlanDigest: "p", IndexGeneration: "g", Completeness: domain.CompletenessComplete,
		EmbeddingModel: "m@1",
	}))
	got, err := s.ListEvidence(ctx, tenant, "sc", 10)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "m@1", got[0].EmbeddingModel)
}

// 000004 resets pre-upgrade wall-clock watermarks, so GREATEST() no longer
// pins them above every real offset.
func TestMigration000004_ResetsWallClockWatermarks(t *testing.T) {
	s, cleanup := testStore(t)
	defer cleanup()
	ctx := context.Background()
	_, err := s.pool.Exec(ctx, `INSERT INTO index_checkpoints (scope_name, source_partition, watermark, freshness)
		VALUES ('legacy', 'p', 1790000000000, 'CURRENT'), ('modern', 'p', 42, 'CURRENT')`)
	require.NoError(t, err)

	sql, err := readMigration("000004_backfill_watermark_evidence.up.sql")
	require.NoError(t, err)
	_, err = s.pool.Exec(ctx, sql)
	require.NoError(t, err, "000004 must be re-runnable")

	var legacy, modern int64
	require.NoError(t, s.pool.QueryRow(ctx, `SELECT watermark FROM index_checkpoints WHERE scope_name='legacy'`).Scan(&legacy))
	require.NoError(t, s.pool.QueryRow(ctx, `SELECT watermark FROM index_checkpoints WHERE scope_name='modern'`).Scan(&modern))
	assert.Equal(t, int64(-1), legacy)
	assert.Equal(t, int64(42), modern)
}
