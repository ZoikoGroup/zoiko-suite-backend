package store_test

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/source-authority-svc/internal/domain"
	svcmiddleware "zoiko.io/source-authority-svc/internal/middleware"
	"zoiko.io/source-authority-svc/internal/outbox"
	"zoiko.io/source-authority-svc/internal/store"
)

// newTestStore connects to TEST_DATABASE_URL, drops this service's own
// tables, and applies every migration in filename order (never a hardcoded
// list). source_authority_maps has no tenant_id and no RLS — platform-wide
// reference data, same posture as schema-registry-svc — so the superuser
// TEST_DATABASE_URL connection is fine for it. normalized_facts DOES have
// FORCE ROW LEVEL SECURITY (migration 000002) and is tested separately in
// rls_test.go against a purpose-created NOSUPERUSER role, because a
// superuser connection bypasses RLS unconditionally and an isolation
// assertion made over one proves nothing.
func newTestStore(t *testing.T) *store.PgStore {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}
	requireThrowawayDatabase(t, dsn)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS normalized_facts, source_authority_maps, outbox_events CASCADE;")

	_, thisFile, _, _ := runtime.Caller(0)
	migDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "deployments", "migrations")
	migrations, err := filepath.Glob(filepath.Join(migDir, "*.up.sql"))
	require.NoError(t, err)
	require.NotEmpty(t, migrations, "no migrations found in %s", migDir)
	sort.Strings(migrations)

	for _, path := range migrations {
		migSQL, err := os.ReadFile(path)
		require.NoError(t, err, "read migration %s", path)
		_, err = pool.Exec(ctx, string(migSQL))
		require.NoError(t, err, "apply migration %s", filepath.Base(path))
	}

	return store.NewPgStore(pool)
}

// requireThrowawayDatabase refuses to run against anything not recognisably
// disposable — this suite DROPs normalized_facts and source_authority_maps.
func requireThrowawayDatabase(t *testing.T, dsn string) {
	t.Helper()
	u, err := url.Parse(dsn)
	require.NoError(t, err, "TEST_DATABASE_URL is not a parseable URL")
	dbName := strings.TrimPrefix(u.Path, "/")
	require.Contains(t, strings.ToLower(dbName), "test",
		"refusing to run: TEST_DATABASE_URL names database %q, which is not recognisably disposable, "+
			"and this suite DROPs normalized_facts and source_authority_maps. Use source_authority_test (or CI's testdb).", dbName)
}

func newMapOutboxEvt(mapID string) outbox.Event {
	return outbox.Event{
		AggregateType: "SourceAuthorityMap", AggregateID: mapID,
		EventType: "source_authority_map.created", Payload: map[string]string{"id": mapID},
		CorrelationID: "test-corr-" + mapID,
	}
}

func newFactOutboxEvt(factID, tenantID string) outbox.Event {
	return outbox.Event{
		AggregateType: "NormalizedFact", AggregateID: factID,
		EventType: "normalized_fact.recorded", Payload: map[string]string{"id": factID},
		CorrelationID: "test-corr-" + factID, TenantID: &tenantID,
	}
}

func testCtx(tenantID string) context.Context {
	return svcmiddleware.WithTenant(context.Background(), tenantID)
}

func TestPgStore_CreateSourceAuthorityMap_And_List(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	m := &domain.SourceAuthorityMap{
		SourceAuthorityMapID: uuid.NewString(), FieldFamily: "PAYROLL_GROSS_PAY", SourceSystem: "ADP",
		PrecedenceRank: 1, ConflictRoute: "x", EffectiveFrom: time.Now().UTC(),
		CreatedByPrincipalID: "p1", CorrelationID: strPtr(uuid.NewString()),
	}
	created, err := s.CreateSourceAuthorityMap(ctx, m, newMapOutboxEvt(m.SourceAuthorityMapID))
	require.NoError(t, err)
	require.True(t, created)

	maps, err := s.ListSourceAuthorityMaps(ctx, domain.ListSourceAuthorityMapsFilter{FieldFamily: "PAYROLL_GROSS_PAY"})
	require.NoError(t, err)
	require.Len(t, maps, 1)
}

func TestPgStore_CreateSourceAuthorityMap_IdempotentReplay_ReturnsOriginal(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	correlationID := uuid.NewString()

	first := &domain.SourceAuthorityMap{
		SourceAuthorityMapID: uuid.NewString(), FieldFamily: "X", SourceSystem: "Y",
		PrecedenceRank: 1, ConflictRoute: "x", EffectiveFrom: time.Now().UTC(),
		CreatedByPrincipalID: "p1", CorrelationID: &correlationID,
	}
	created, err := s.CreateSourceAuthorityMap(ctx, first, newMapOutboxEvt(first.SourceAuthorityMapID))
	require.NoError(t, err)
	require.True(t, created)
	originalID := first.SourceAuthorityMapID

	replay := &domain.SourceAuthorityMap{
		SourceAuthorityMapID: uuid.NewString(), FieldFamily: "X", SourceSystem: "Y",
		PrecedenceRank: 1, ConflictRoute: "x", EffectiveFrom: time.Now().UTC(),
		CreatedByPrincipalID: "p1", CorrelationID: &correlationID,
	}
	created, err = s.CreateSourceAuthorityMap(ctx, replay, newMapOutboxEvt(replay.SourceAuthorityMapID))
	require.NoError(t, err)
	require.False(t, created, "a replay under the same correlation_id must not be reported as created")
	require.Equal(t, originalID, replay.SourceAuthorityMapID)

	pool := testPool(t)
	var rowCount, outboxCount int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM source_authority_maps WHERE field_family = 'X'").Scan(&rowCount))
	require.Equal(t, 1, rowCount, "replay must not insert a second row")
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM outbox_events WHERE aggregate_id = $1", originalID).Scan(&outboxCount))
	require.Equal(t, 1, outboxCount, "replay must not insert a second outbox event")
}

func TestPgStore_CreateSourceAuthorityMap_GenuineConflict_IsNotAReplay(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	m1 := &domain.SourceAuthorityMap{
		SourceAuthorityMapID: uuid.NewString(), FieldFamily: "DUP", SourceSystem: "S",
		PrecedenceRank: 1, ConflictRoute: "x", EffectiveFrom: time.Now().UTC(),
		CreatedByPrincipalID: "p1", CorrelationID: strPtr(uuid.NewString()),
	}
	_, err := s.CreateSourceAuthorityMap(ctx, m1, newMapOutboxEvt(m1.SourceAuthorityMapID))
	require.NoError(t, err)

	m2 := &domain.SourceAuthorityMap{
		SourceAuthorityMapID: uuid.NewString(), FieldFamily: "DUP", SourceSystem: "S",
		PrecedenceRank: 2, ConflictRoute: "y", EffectiveFrom: m1.EffectiveFrom,
		CreatedByPrincipalID: "p1", CorrelationID: strPtr(uuid.NewString()), // different correlation_id
	}
	_, err = s.CreateSourceAuthorityMap(ctx, m2, newMapOutboxEvt(m2.SourceAuthorityMapID))
	require.ErrorIs(t, err, domain.ErrConflict)
}

func TestPgStore_SupersedeSourceAuthorityMap_EndDatesAndRecordsOutbox(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	m := &domain.SourceAuthorityMap{
		SourceAuthorityMapID: uuid.NewString(), FieldFamily: "F", SourceSystem: "S",
		PrecedenceRank: 1, ConflictRoute: "x", EffectiveFrom: time.Now().UTC().Add(-time.Hour),
		CreatedByPrincipalID: "p1", CorrelationID: strPtr(uuid.NewString()),
	}
	_, err := s.CreateSourceAuthorityMap(ctx, m, newMapOutboxEvt(m.SourceAuthorityMapID))
	require.NoError(t, err)

	effectiveTo := time.Now().UTC().Add(time.Hour)
	outboxEvt := outbox.Event{
		AggregateType: "SourceAuthorityMap", AggregateID: m.SourceAuthorityMapID,
		EventType: "source_authority_map.superseded", Payload: map[string]string{"id": m.SourceAuthorityMapID},
	}
	updated, err := s.SupersedeSourceAuthorityMap(ctx, m.SourceAuthorityMapID, effectiveTo, "p2", outboxEvt)
	require.NoError(t, err)
	require.NotNil(t, updated.SupersededAt)
	require.Equal(t, "p2", *updated.SupersededByPrincipalID)

	pool := testPool(t)
	var outboxCount int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'source_authority_map.superseded'", m.SourceAuthorityMapID).Scan(&outboxCount))
	require.Equal(t, 1, outboxCount)
}

func TestPgStore_RecordFact_IsIdempotentAndAtomic(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx("tenant-a")

	correlationID := uuid.NewString()
	f := &domain.NormalizedFact{
		NormalizedFactID: uuid.NewString(), TenantID: "tenant-a", FieldFamily: "F", EntityRef: "e-1",
		SourceSystem: "S", SourceRecord: "r-1", FactValue: json.RawMessage(`"v"`),
		ObservedAt: time.Now().UTC(), EffectiveAt: time.Now().UTC(),
		AuthorityClass: domain.AuthorityClassAuthoritative, CreatedByPrincipalID: "p1",
		CorrelationID: &correlationID,
	}
	created, err := s.RecordFact(ctx, f, newFactOutboxEvt(f.NormalizedFactID, "tenant-a"))
	require.NoError(t, err)
	require.True(t, created)
	originalID := f.NormalizedFactID

	replay := &domain.NormalizedFact{
		NormalizedFactID: uuid.NewString(), TenantID: "tenant-a", FieldFamily: "F", EntityRef: "e-1",
		SourceSystem: "S", SourceRecord: "r-1", FactValue: json.RawMessage(`"v"`),
		ObservedAt: time.Now().UTC(), EffectiveAt: time.Now().UTC(),
		AuthorityClass: domain.AuthorityClassAuthoritative, CreatedByPrincipalID: "p1",
		CorrelationID: &correlationID,
	}
	created, err = s.RecordFact(ctx, replay, newFactOutboxEvt(replay.NormalizedFactID, "tenant-a"))
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, originalID, replay.NormalizedFactID)

	pool := testPool(t)
	var rowCount, outboxCount int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM normalized_facts WHERE tenant_id = 'tenant-a' AND correlation_id = $1", correlationID).Scan(&rowCount))
	require.Equal(t, 1, rowCount, "replay must not append a second observation")
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM outbox_events WHERE aggregate_id = $1", originalID).Scan(&outboxCount))
	require.Equal(t, 1, outboxCount, "replay must not record a second outbox event")
}

func TestPgStore_ResolveAuthoritativeFact_HighestPrecedenceWins(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx("tenant-a")

	createMapHelper(t, s, "HR_STATUS", "Kriton", 1)
	createMapHelper(t, s, "HR_STATUS", "ZoikoLogia", 2)
	recordFactHelper(t, s, "tenant-a", "HR_STATUS", "emp-1", "Kriton", `"TERMINATED"`)
	recordFactHelper(t, s, "tenant-a", "HR_STATUS", "emp-1", "ZoikoLogia", `"ACTIVE"`)

	res, err := s.ResolveAuthoritativeFact(ctx, "tenant-a", "HR_STATUS", "emp-1")
	require.NoError(t, err)
	require.False(t, res.Ambiguous)
	require.NotNil(t, res.AuthoritativeFact)
	require.Equal(t, "Kriton", res.AuthoritativeFact.SourceSystem)
}

func TestPgStore_ResolveAuthoritativeFact_SameTierDisagreement_IsAmbiguous(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx("tenant-a")

	createMapHelper(t, s, "BILLING_EMAIL", "ADP", 1)
	createMapHelper(t, s, "BILLING_EMAIL", "NetSuite", 1)
	recordFactHelper(t, s, "tenant-a", "BILLING_EMAIL", "acct-1", "ADP", `"a@x.com"`)
	recordFactHelper(t, s, "tenant-a", "BILLING_EMAIL", "acct-1", "NetSuite", `"b@x.com"`)

	res, err := s.ResolveAuthoritativeFact(ctx, "tenant-a", "BILLING_EMAIL", "acct-1")
	require.NoError(t, err)
	require.True(t, res.Ambiguous)
	require.Nil(t, res.AuthoritativeFact)
	require.Len(t, res.ConflictingFacts, 2)
}

func TestPgStore_ResolveAuthoritativeFact_UnmappedSourceIsReported(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx("tenant-a")

	createMapHelper(t, s, "TAX_JURISDICTION", "Kriton", 1)
	recordFactHelper(t, s, "tenant-a", "TAX_JURISDICTION", "entity-1", "Kriton", `"IN-KA"`)
	recordFactHelper(t, s, "tenant-a", "TAX_JURISDICTION", "entity-1", "UnrankedHRIS", `"IN-MH"`)

	res, err := s.ResolveAuthoritativeFact(ctx, "tenant-a", "TAX_JURISDICTION", "entity-1")
	require.NoError(t, err)
	require.NotNil(t, res.AuthoritativeFact)
	require.Equal(t, "Kriton", res.AuthoritativeFact.SourceSystem)
	require.Contains(t, res.UnmappedSources, "UnrankedHRIS")
}

// TestPgStore_Outbox_AtomicityAndRelay proves RecordFact inserts its
// outbox_events row in the same transaction as the fact row, and that the
// relay correctly retries a failed publish and marks a successful one.
// Regression coverage for the swallowed-error direct-publish pattern this
// service used exclusively before this pass.
func TestPgStore_Outbox_AtomicityAndRelay(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx("tenant-a")

	f := &domain.NormalizedFact{
		NormalizedFactID: uuid.NewString(), TenantID: "tenant-a", FieldFamily: "F", EntityRef: "e-1",
		SourceSystem: "S", SourceRecord: "r-1", FactValue: json.RawMessage(`"v"`),
		ObservedAt: time.Now().UTC(), EffectiveAt: time.Now().UTC(),
		AuthorityClass: domain.AuthorityClassAuthoritative, CreatedByPrincipalID: "p1",
		CorrelationID: strPtr(uuid.NewString()),
	}
	_, err := s.RecordFact(ctx, f, newFactOutboxEvt(f.NormalizedFactID, "tenant-a"))
	require.NoError(t, err)

	pool := testPool(t)
	var outboxCount int
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND published_at IS NULL", f.NormalizedFactID,
	).Scan(&outboxCount))
	require.Equal(t, 1, outboxCount, "expected exactly 1 unpublished outbox event inserted atomically with the fact row")

	mockPub := &failingThenSucceedingPublisher{failFirst: true}
	relay := outbox.NewRelay(pool, mockPub, 0, 10, zap.NewNop())

	count, err := relay.RelayOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, count)

	var attempts int
	var lastErr *string
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT publish_attempts, last_error FROM outbox_events WHERE aggregate_id = $1", f.NormalizedFactID,
	).Scan(&attempts, &lastErr))
	require.Equal(t, 1, attempts)
	require.NotNil(t, lastErr)

	count, err = relay.RelayOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, count, "the retried event must be picked up and succeed on the second attempt")

	require.NoError(t, pool.QueryRow(ctx,
		"SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND published_at IS NULL", f.NormalizedFactID,
	).Scan(&outboxCount))
	require.Equal(t, 0, outboxCount, "the event must be marked published after a successful relay attempt")
}

func createMapHelper(t *testing.T, s *store.PgStore, fieldFamily, sourceSystem string, rank int) {
	t.Helper()
	m := &domain.SourceAuthorityMap{
		SourceAuthorityMapID: uuid.NewString(), FieldFamily: fieldFamily, SourceSystem: sourceSystem,
		PrecedenceRank: rank, ConflictRoute: "route", EffectiveFrom: time.Now().UTC().Add(-time.Hour),
		CreatedByPrincipalID: "p1", CorrelationID: strPtr(uuid.NewString()),
	}
	_, err := s.CreateSourceAuthorityMap(context.Background(), m, newMapOutboxEvt(m.SourceAuthorityMapID))
	require.NoError(t, err)
}

func recordFactHelper(t *testing.T, s *store.PgStore, tenantID, fieldFamily, entityRef, sourceSystem, value string) {
	t.Helper()
	f := &domain.NormalizedFact{
		NormalizedFactID: uuid.NewString(), TenantID: tenantID, FieldFamily: fieldFamily, EntityRef: entityRef,
		SourceSystem: sourceSystem, SourceRecord: "rec-1", FactValue: json.RawMessage(value),
		ObservedAt: time.Now().UTC(), EffectiveAt: time.Now().UTC(),
		AuthorityClass: domain.AuthorityClassAuthoritative, CreatedByPrincipalID: "p1",
		CorrelationID: strPtr(uuid.NewString()),
	}
	_, err := s.RecordFact(testCtx(tenantID), f, newFactOutboxEvt(f.NormalizedFactID, tenantID))
	require.NoError(t, err)
}

func strPtr(s string) *string { return &s }

type failingThenSucceedingPublisher struct {
	failFirst bool
}

func (p *failingThenSucceedingPublisher) PublishOutbox(_ context.Context, _, _ string, _ []byte) error {
	if p.failFirst {
		p.failFirst = false
		return errSimulatedFailure
	}
	return nil
}

var errSimulatedFailure = &simulatedFailure{}

type simulatedFailure struct{}

func (*simulatedFailure) Error() string { return "simulated publish failure" }

// testPool re-opens a pool against the same TEST_DATABASE_URL, for
// assertions that need direct SQL against outbox_events (the Store
// interface has no outbox-inspection method, deliberately).
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	pool, err := pgxpool.New(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}
