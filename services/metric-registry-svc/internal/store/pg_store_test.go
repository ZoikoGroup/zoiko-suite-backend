package store_test

import (
	"context"
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

	"zoiko.io/metric-registry-svc/internal/domain"
	"zoiko.io/metric-registry-svc/internal/outbox"
	"zoiko.io/metric-registry-svc/internal/store"
)

// newTestStore connects to TEST_DATABASE_URL, drops this service's own
// tables, and applies every migration in order (discovered by glob, never a
// hardcoded list — see schema-registry-svc's identical helper for why that
// matters once a later migration is added). report_metric_definitions has
// no tenant_id and no RLS: this registry is platform-wide reference data,
// same posture as schema-registry-svc, so unlike the tenant-scoped
// services' store suites there is no NOSUPERUSER test role to stand up.
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

	_, filename, _, _ := runtime.Caller(0)
	migDir := filepath.Join(filepath.Dir(filename), "..", "..", "deployments", "migrations")

	_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS report_metric_definitions, outbox_events CASCADE;")

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
// disposable — this suite DROPs report_metric_definitions. Same guard as
// schema-registry-svc's identical helper.
func requireThrowawayDatabase(t *testing.T, dsn string) {
	t.Helper()
	u, err := url.Parse(dsn)
	require.NoError(t, err, "TEST_DATABASE_URL is not a parseable URL")
	dbName := strings.TrimPrefix(u.Path, "/")
	require.Contains(t, strings.ToLower(dbName), "test",
		"refusing to run: TEST_DATABASE_URL names database %q, which is not recognisably disposable, "+
			"and this suite DROPs report_metric_definitions. Use metric_registry_test (or CI's testdb).", dbName)
}

func newOutboxEvt(aggregateID, eventType string, payload any) outbox.Event {
	return outbox.Event{
		AggregateType: "ReportMetricDefinition",
		AggregateID:   aggregateID,
		EventType:     eventType,
		Payload:       payload,
		CorrelationID: "test-corr-" + aggregateID,
	}
}

func TestPgStore_CreateMetricDefinition_And_GetActive(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	d := &domain.ReportMetricDefinition{
		MetricDefinitionID:   uuid.NewString(),
		MetricCode:           "MRR",
		MetricName:           "Monthly Recurring Revenue",
		FormulaDescription:   "sum of active subscriptions",
		OwnerPrincipalID:     "finance-metrics-owner-1",
		Version:              1,
		EffectiveFrom:        time.Now().UTC(),
		CreatedByPrincipalID: "finance-metrics-owner-1",
	}
	err := s.CreateMetricDefinition(ctx, d, newOutboxEvt(d.MetricDefinitionID, "report_metric.created", d))
	require.NoError(t, err)

	got, err := s.GetActiveMetricDefinition(ctx, "MRR")
	require.NoError(t, err)
	require.Equal(t, 1, got.Version)
	require.Equal(t, "ACTIVE", got.DefinitionStatus)
}

func TestPgStore_CreateMetricDefinition_DuplicateCode_IsConflict(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	d := &domain.ReportMetricDefinition{
		MetricDefinitionID: uuid.NewString(), MetricCode: "ARR", MetricName: "x", FormulaDescription: "x",
		OwnerPrincipalID: "p1", Version: 1, EffectiveFrom: time.Now().UTC(), CreatedByPrincipalID: "p1",
	}
	require.NoError(t, s.CreateMetricDefinition(ctx, d, newOutboxEvt(d.MetricDefinitionID, "report_metric.created", d)))

	dup := &domain.ReportMetricDefinition{
		MetricDefinitionID: uuid.NewString(), MetricCode: "ARR", MetricName: "y", FormulaDescription: "y",
		OwnerPrincipalID: "p1", Version: 1, EffectiveFrom: time.Now().UTC(), CreatedByPrincipalID: "p1",
	}
	err := s.CreateMetricDefinition(ctx, dup, newOutboxEvt(dup.MetricDefinitionID, "report_metric.created", dup))
	require.ErrorIs(t, err, domain.ErrConflict)
}

func TestPgStore_PublishNewVersion_SupersedesAndListsHistory(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	v1 := &domain.ReportMetricDefinition{
		MetricDefinitionID: uuid.NewString(), MetricCode: "CHURN", MetricName: "Churn", FormulaDescription: "v1",
		OwnerPrincipalID: "p1", Version: 1, EffectiveFrom: time.Now().UTC(), CreatedByPrincipalID: "p1",
	}
	require.NoError(t, s.CreateMetricDefinition(ctx, v1, newOutboxEvt(v1.MetricDefinitionID, "report_metric.created", v1)))

	v2 := &domain.ReportMetricDefinition{
		MetricDefinitionID: uuid.NewString(), MetricCode: "CHURN", MetricName: "Churn", FormulaDescription: "v2",
		OwnerPrincipalID: "p1", Version: 2, EffectiveFrom: time.Now().UTC(), CreatedByPrincipalID: "p1",
	}
	require.NoError(t, s.PublishNewVersion(ctx, v2, newOutboxEvt(v2.MetricDefinitionID, "report_metric.version_published", v2)))

	active, err := s.GetActiveMetricDefinition(ctx, "CHURN")
	require.NoError(t, err)
	require.Equal(t, 2, active.Version)

	history, err := s.ListMetricVersions(ctx, "CHURN")
	require.NoError(t, err)
	require.Len(t, history, 2, "v1 must survive, superseded but never deleted")
}

// TestPgStore_ListActiveMetricDefinitions_ReturnsOneRowPerCode is the real-
// database counterpart of the handler-level regression test for MET-02:
// the query backing GET /v1/report-metrics never existed before this pass.
func TestPgStore_ListActiveMetricDefinitions_ReturnsOneRowPerCode(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for _, code := range []string{"ARR", "MRR", "CHURN"} {
		d := &domain.ReportMetricDefinition{
			MetricDefinitionID: uuid.NewString(), MetricCode: code, MetricName: code, FormulaDescription: "x",
			OwnerPrincipalID: "p1", Version: 1, EffectiveFrom: time.Now().UTC(), CreatedByPrincipalID: "p1",
		}
		require.NoError(t, s.CreateMetricDefinition(ctx, d, newOutboxEvt(d.MetricDefinitionID, "report_metric.created", d)))
	}
	// A second version of one code must not duplicate it in the catalog —
	// only the ACTIVE row should appear.
	v2 := &domain.ReportMetricDefinition{
		MetricDefinitionID: uuid.NewString(), MetricCode: "ARR", MetricName: "ARR", FormulaDescription: "v2",
		OwnerPrincipalID: "p1", Version: 2, EffectiveFrom: time.Now().UTC(), CreatedByPrincipalID: "p1",
	}
	require.NoError(t, s.PublishNewVersion(ctx, v2, newOutboxEvt(v2.MetricDefinitionID, "report_metric.version_published", v2)))

	catalog, err := s.ListActiveMetricDefinitions(ctx, 100, 0)
	require.NoError(t, err)
	require.Len(t, catalog, 3, "exactly one ACTIVE row per metric_code, not one per version")

	byCode := map[string]int{}
	for _, d := range catalog {
		byCode[d.MetricCode] = d.Version
	}
	require.Equal(t, 2, byCode["ARR"], "the catalog must show ARR's current (v2) definition, not v1")
}

// TestPgStore_Outbox_AtomicityAndRelay proves CreateMetricDefinition and
// PublishNewVersion insert their outbox_events row in the same transaction
// as the domain row, and that the relay correctly retries a failed publish
// and marks a successful one. Regression coverage for the swallowed-error
// direct-publish pattern this service used exclusively before this pass.
func TestPgStore_Outbox_AtomicityAndRelay(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	d := &domain.ReportMetricDefinition{
		MetricDefinitionID: uuid.NewString(), MetricCode: "NRR", MetricName: "Net Revenue Retention", FormulaDescription: "x",
		OwnerPrincipalID: "p1", Version: 1, EffectiveFrom: time.Now().UTC(), CreatedByPrincipalID: "p1",
	}
	outboxEvt := newOutboxEvt(d.MetricDefinitionID, "report_metric.created", d)
	require.NoError(t, s.CreateMetricDefinition(ctx, d, outboxEvt))

	pool := testPool(t)
	var outboxCount int
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND published_at IS NULL", d.MetricDefinitionID,
	).Scan(&outboxCount))
	require.Equal(t, 1, outboxCount, "expected exactly 1 unpublished outbox event inserted atomically with the domain row")

	mockPub := &failingThenSucceedingPublisher{failFirst: true}
	relay := outbox.NewRelay(pool, mockPub, 0, 10, zap.NewNop())

	count, err := relay.RelayOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, count)

	var attempts int
	var lastErr *string
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT publish_attempts, last_error FROM outbox_events WHERE aggregate_id = $1", d.MetricDefinitionID,
	).Scan(&attempts, &lastErr))
	require.Equal(t, 1, attempts)
	require.NotNil(t, lastErr)

	count, err = relay.RelayOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, count, "the retried event must be picked up and succeed on the second attempt")

	require.NoError(t, pool.QueryRow(ctx,
		"SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND published_at IS NULL", d.MetricDefinitionID,
	).Scan(&outboxCount))
	require.Equal(t, 0, outboxCount, "the event must be marked published after a successful relay attempt")
}

// TestPgStore_CreateMetricDefinition_IdempotentReplay_ReturnsOriginalRow is
// the real-database proof of INV-08 replay protection: a second
// CreateMetricDefinition call for the same (metric_code, idempotency_key)
// must return the original row, not a conflict error, and must not insert a
// second outbox event.
func TestPgStore_CreateMetricDefinition_IdempotentReplay_ReturnsOriginalRow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	first := &domain.ReportMetricDefinition{
		MetricDefinitionID: uuid.NewString(), MetricCode: "IDEMPOTENT_DB", MetricName: "x", FormulaDescription: "x",
		OwnerPrincipalID: "p1", Version: 1, EffectiveFrom: time.Now().UTC(), CreatedByPrincipalID: "p1",
		IdempotencyKey: "db-replay-key-1",
	}
	require.NoError(t, s.CreateMetricDefinition(ctx, first, newOutboxEvt(first.MetricDefinitionID, "report_metric.created", first)))
	originalID := first.MetricDefinitionID

	replay := &domain.ReportMetricDefinition{
		MetricDefinitionID: uuid.NewString(), MetricCode: "IDEMPOTENT_DB", MetricName: "x", FormulaDescription: "x",
		OwnerPrincipalID: "p1", Version: 1, EffectiveFrom: time.Now().UTC(), CreatedByPrincipalID: "p1",
		IdempotencyKey: "db-replay-key-1",
	}
	err := s.CreateMetricDefinition(ctx, replay, newOutboxEvt(replay.MetricDefinitionID, "report_metric.created", replay))
	require.NoError(t, err, "a replay under the same idempotency key must not be a conflict")
	require.Equal(t, originalID, replay.MetricDefinitionID, "replay must return the ORIGINAL row")

	pool := testPool(t)
	var rowCount, outboxCount int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM report_metric_definitions WHERE metric_code = $1", "IDEMPOTENT_DB").Scan(&rowCount))
	require.Equal(t, 1, rowCount, "replay must not insert a second row")
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM outbox_events WHERE aggregate_id = $1", originalID).Scan(&outboxCount))
	require.Equal(t, 1, outboxCount, "replay must not insert a second outbox event")
}

// TestPgStore_PublishNewVersion_IdempotentReplay_DoesNotDoubleSupersede is
// the real-database proof of the more serious half of INV-08 for this
// service: a replayed publish must not re-run the supersede step, which
// would otherwise un-supersede history or corrupt whichever version is
// ACTIVE at replay time.
func TestPgStore_PublishNewVersion_IdempotentReplay_DoesNotDoubleSupersede(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	v1 := &domain.ReportMetricDefinition{
		MetricDefinitionID: uuid.NewString(), MetricCode: "IDEMPOTENT_PUB_DB", MetricName: "x", FormulaDescription: "v1",
		OwnerPrincipalID: "p1", Version: 1, EffectiveFrom: time.Now().UTC(), CreatedByPrincipalID: "p1",
	}
	require.NoError(t, s.CreateMetricDefinition(ctx, v1, newOutboxEvt(v1.MetricDefinitionID, "report_metric.created", v1)))

	v2 := &domain.ReportMetricDefinition{
		MetricDefinitionID: uuid.NewString(), MetricCode: "IDEMPOTENT_PUB_DB", MetricName: "x", FormulaDescription: "v2",
		OwnerPrincipalID: "p1", Version: 2, EffectiveFrom: time.Now().UTC(), CreatedByPrincipalID: "p1",
		IdempotencyKey: "db-replay-key-publish-1",
	}
	require.NoError(t, s.PublishNewVersion(ctx, v2, newOutboxEvt(v2.MetricDefinitionID, "report_metric.version_published", v2)))
	v2ID := v2.MetricDefinitionID

	v3 := &domain.ReportMetricDefinition{
		MetricDefinitionID: uuid.NewString(), MetricCode: "IDEMPOTENT_PUB_DB", MetricName: "x", FormulaDescription: "v3",
		OwnerPrincipalID: "p1", Version: 3, EffectiveFrom: time.Now().UTC(), CreatedByPrincipalID: "p1",
		IdempotencyKey: "db-later-key-publish-2", // gitleaks:allow -- test fixture idempotency key, not a secret
	}
	require.NoError(t, s.PublishNewVersion(ctx, v3, newOutboxEvt(v3.MetricDefinitionID, "report_metric.version_published", v3)))

	// Replay the v2 publish (same metric_code + idempotency_key as v2).
	replay := &domain.ReportMetricDefinition{
		MetricDefinitionID: uuid.NewString(), MetricCode: "IDEMPOTENT_PUB_DB", MetricName: "x", FormulaDescription: "v2",
		OwnerPrincipalID: "p1", Version: 2, EffectiveFrom: time.Now().UTC(), CreatedByPrincipalID: "p1",
		IdempotencyKey: "db-replay-key-publish-1",
	}
	err := s.PublishNewVersion(ctx, replay, newOutboxEvt(replay.MetricDefinitionID, "report_metric.version_published", replay))
	require.NoError(t, err)
	require.Equal(t, v2ID, replay.MetricDefinitionID, "replay must return the ORIGINAL v2 row")

	active, err := s.GetActiveMetricDefinition(ctx, "IDEMPOTENT_PUB_DB")
	require.NoError(t, err)
	require.Equal(t, 3, active.Version, "replaying the stale v2 publish must not disturb v3's ACTIVE status")

	history, err := s.ListMetricVersions(ctx, "IDEMPOTENT_PUB_DB")
	require.NoError(t, err)
	require.Len(t, history, 3, "the replay must not have inserted a fourth row")
}

type failingThenSucceedingPublisher struct {
	failFirst bool
	published []string
}

func (p *failingThenSucceedingPublisher) PublishOutbox(_ context.Context, outboxEventID, _ string, _ []byte) error {
	if p.failFirst {
		p.failFirst = false
		return errorSimulatedFailure
	}
	p.published = append(p.published, outboxEventID)
	return nil
}

var errorSimulatedFailure = &simulatedFailure{}

type simulatedFailure struct{}

func (*simulatedFailure) Error() string { return "simulated publish failure" }

// testPool re-opens a pool against the same TEST_DATABASE_URL, for
// assertions that need direct SQL against outbox_events (the Store
// interface has no outbox-inspection method, deliberately — that table is
// the relay's concern, not the domain store's).
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	pool, err := pgxpool.New(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}
