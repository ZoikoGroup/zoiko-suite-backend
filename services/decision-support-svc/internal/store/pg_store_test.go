package store_test

// Postgres integration tests for PgStore and the row-level security added
// by migrations 000001/000002.
//
// TEST_DATABASE_URL must point at a superuser connection to a scratch
// database: the suite drops and recreates this service's tables, applies
// every up migration in order, then creates a NOSUPERUSER NOBYPASSRLS login
// role (TEST_RLS_ROLE, default decision_support_rls_test) and runs every
// store call over that role. A superuser bypasses row-level security —
// FORCE included — so isolation proved over the admin connection would
// prove nothing.

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/decision-support-svc/internal/domain"
	"zoiko.io/decision-support-svc/internal/events"
	"zoiko.io/decision-support-svc/internal/health"
	"zoiko.io/decision-support-svc/internal/service"
	"zoiko.io/decision-support-svc/internal/store"
	"zoiko.io/decision-support-svc/internal/testsupport"
)

var rlsTables = []string{"recommendations", "recommendation_dispositions", "idempotency_keys", "outbox"}

func openAdminPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, err = pool.Exec(ctx, `DROP TABLE IF EXISTS outbox, idempotency_keys, recommendation_dispositions, recommendations CASCADE;
		DROP FUNCTION IF EXISTS recommendation_dispositions_append_only() CASCADE;`)
	require.NoError(t, err)

	_, thisFile, _, _ := runtime.Caller(0)
	migDir := filepath.Join(filepath.Dir(thisFile), "../../deployments/migrations")
	entries, err := os.ReadDir(migDir)
	require.NoError(t, err)
	var migs []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			migs = append(migs, e.Name())
		}
	}
	require.NotEmpty(t, migs)
	sort.Strings(migs)
	for _, name := range migs {
		sqlBytes, err := os.ReadFile(filepath.Join(migDir, name))
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(sqlBytes))
		require.NoErrorf(t, err, "apply migration %s", name)
	}
	return pool
}

func appRolePool(t *testing.T, admin *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	role := os.Getenv("TEST_RLS_ROLE")
	if role == "" {
		role = "decision_support_rls_test"
	}
	password := role + "_pw"

	_, err := admin.Exec(ctx, `DO $do$ BEGIN
		IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '`+role+`') THEN
			CREATE ROLE `+role+` LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
		END IF;
	END $do$;`)
	require.NoError(t, err)
	for _, stmt := range []string{
		`ALTER ROLE ` + role + ` WITH LOGIN PASSWORD '` + password + `' NOSUPERUSER NOBYPASSRLS`,
		`GRANT USAGE ON SCHEMA public TO ` + role,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ` + role,
	} {
		_, err := admin.Exec(ctx, stmt)
		require.NoErrorf(t, err, "grant: %s", stmt)
	}

	u, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	require.NoError(t, err)
	u.User = url.UserPassword(role, password)
	pool, err := pgxpool.New(ctx, u.String())
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	var isSuper, bypass bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&isSuper, &bypass))
	require.False(t, isSuper || bypass, "%s must be NOSUPERUSER NOBYPASSRLS", role)
	return pool
}

type env struct {
	admin *pgxpool.Pool
	app   *pgxpool.Pool
	store *store.PgStore
	svc   *service.Service
}

func newEnv(t *testing.T) *env {
	admin := openAdminPool(t)
	app := appRolePool(t, admin)
	st := store.New(app)
	history := &testsupport.History{Decisions: testsupport.Outcomes("GRANTED", "GRANTED", "DENIED")}
	return &env{
		admin: admin, app: app, store: st,
		svc: service.New(st, &testsupport.Authorizer{}, history, zap.NewNop()),
	}
}

func caller(tenantID, legalEntity, principal string) domain.CallerContext {
	return domain.CallerContext{TenantID: tenantID, PrincipalID: principal, LegalEntityID: legalEntity, CorrelationID: "corr-it", RequestID: uuid.NewString()}
}

func idem() domain.IdempotencyInput {
	return domain.IdempotencyInput{Key: uuid.NewString(), RequestHash: uuid.NewString()}
}

func (e *env) create(t *testing.T, tenantID, legalEntity string) *domain.Recommendation {
	t.Helper()
	rec, replayed, err := e.svc.CreateRecommendation(context.Background(), caller(tenantID, legalEntity, "requester"), idem(),
		domain.CreateRecommendationRequest{LegalEntityID: legalEntity, SubjectType: "purchase_order", SubjectReference: "po-1", ActionType: "PO_ISSUE"})
	require.NoError(t, err)
	require.False(t, replayed)
	return rec
}

func TestRLS_EnabledForcedWithCheck(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, table := range rlsTables {
		var enabled, forced bool
		require.NoError(t, e.admin.QueryRow(ctx,
			`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1`, table).Scan(&enabled, &forced))
		assert.True(t, enabled, "%s: RLS enabled", table)
		assert.True(t, forced, "%s: RLS forced", table)

		var policies, withoutCheck int
		require.NoError(t, e.admin.QueryRow(ctx,
			`SELECT count(*), count(*) FILTER (WHERE with_check IS NULL) FROM pg_policies WHERE tablename = $1`, table,
		).Scan(&policies, &withoutCheck))
		assert.Positive(t, policies, "%s: has a policy", table)
		assert.Zero(t, withoutCheck, "%s: every policy has an explicit WITH CHECK", table)
	}
}

func TestStore_LifecyclePersistsAndIsTenantIsolated(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tenantA, tenantB, le := uuid.NewString(), uuid.NewString(), uuid.NewString()

	rec := e.create(t, tenantA, le)
	assert.Equal(t, domain.RecommendedActionApprove, rec.RecommendedAction)
	assert.Equal(t, 0.667, rec.ConfidenceScore)

	got, err := e.store.GetRecommendation(ctx, tenantA, rec.RecommendationID)
	require.NoError(t, err)
	assert.Equal(t, rec.RecommendationID, got.RecommendationID)
	assert.Equal(t, map[string]int{"GRANTED": 2, "DENIED": 1}, got.OutcomeDistribution)

	_, err = e.store.GetRecommendation(ctx, tenantB, rec.RecommendationID)
	assert.ErrorIs(t, err, domain.ErrRecommendationNotFound, "another tenant's row is indistinguishable from a missing one")

	page, err := e.store.ListRecommendations(ctx, tenantB, domain.ListFilter{Limit: 50})
	require.NoError(t, err)
	assert.Empty(t, page)
	page, err = e.store.ListRecommendations(ctx, tenantA, domain.ListFilter{LegalEntityID: le, Limit: 50})
	require.NoError(t, err)
	assert.Len(t, page, 1)

	// Accept by an independent reviewer: status, history, outbox in one tx.
	updated, _, err := e.svc.Decide(ctx, caller(tenantA, le, "reviewer"), idem(), domain.DecisionCommand{
		RecommendationID: rec.RecommendationID, TargetStatus: domain.StatusAccepted, ExpectedVersion: rec.Version,
		Request: domain.DecisionRequest{Reason: "ok"},
	})
	require.NoError(t, err)
	assert.Equal(t, domain.StatusAccepted, updated.Status)
	assert.Equal(t, 2, updated.Version)

	history, err := e.store.ListDispositions(ctx, tenantA, rec.RecommendationID)
	require.NoError(t, err)
	require.Len(t, history, 2)
	assert.Nil(t, history[0].FromStatus)
	assert.Equal(t, domain.StatusAccepted, history[1].ToStatus)
	history, err = e.store.ListDispositions(ctx, tenantB, rec.RecommendationID)
	require.NoError(t, err)
	assert.Empty(t, history)

	// A stale version loses.
	_, _, err = e.svc.Decide(ctx, caller(tenantA, le, "reviewer"), idem(), domain.DecisionCommand{
		RecommendationID: rec.RecommendationID, TargetStatus: domain.StatusRejected, ExpectedVersion: rec.Version,
		Request: domain.DecisionRequest{Reason: "late"},
	})
	assert.ErrorIs(t, err, domain.ErrVersionMismatch)

	var outboxTypes []string
	rows, err := e.admin.Query(ctx, `SELECT message_type FROM outbox WHERE tenant_id = $1 AND destination = 'KAFKA' ORDER BY created_at`, tenantA)
	require.NoError(t, err)
	for rows.Next() {
		var mt string
		require.NoError(t, rows.Scan(&mt))
		outboxTypes = append(outboxTypes, mt)
	}
	rows.Close()
	assert.Equal(t, []string{events.EventRecommendationProposed, events.EventRecommendationAccepted}, outboxTypes)
}

func TestStore_IdempotencyReplayAndRace(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tenantA, le := uuid.NewString(), uuid.NewString()
	key := idem()
	req := domain.CreateRecommendationRequest{LegalEntityID: le, SubjectType: "invoice", SubjectReference: "inv-9", ActionType: "AP_PAY"}

	first, replayed, err := e.svc.CreateRecommendation(ctx, caller(tenantA, le, "requester"), key, req)
	require.NoError(t, err)
	require.False(t, replayed)
	second, replayed, err := e.svc.CreateRecommendation(ctx, caller(tenantA, le, "requester"), key, req)
	require.NoError(t, err)
	assert.True(t, replayed)
	assert.Equal(t, first.RecommendationID, second.RecommendationID)

	var count int
	require.NoError(t, e.admin.QueryRow(ctx, `SELECT count(*) FROM recommendations WHERE tenant_id = $1`, tenantA).Scan(&count))
	assert.Equal(t, 1, count, "the replay inserted nothing")

	// The store itself refuses a second claim of the same key.
	err = e.store.InsertRecommendation(ctx, domain.NewRecommendationWrite{
		Recommendation: *first,
		Idempotency:    domain.IdempotencyRecord{TenantID: tenantA, Key: key.Key, Operation: "x", RequestHash: "h", ResourceID: first.RecommendationID, ResponseBody: json.RawMessage(`{}`)},
	})
	assert.ErrorIs(t, err, domain.ErrIdempotencyRace)

	// The same key in another tenant is a different key.
	_, replayed, err = e.svc.CreateRecommendation(ctx, caller(uuid.NewString(), le, "requester"), key, req)
	require.NoError(t, err)
	assert.False(t, replayed)
}

func TestRLS_RawSQLAsAppRole(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tenantA, tenantB, le := uuid.NewString(), uuid.NewString(), uuid.NewString()
	rec := e.create(t, tenantA, le)

	countAs := func(tenant, table string) int {
		tx, err := e.app.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()
		if tenant != "" {
			_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant)
			require.NoError(t, err)
		}
		var n int
		require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n))
		return n
	}
	for _, table := range rlsTables {
		assert.Positive(t, countAs(tenantA, table), "%s visible to its tenant", table)
		assert.Zero(t, countAs(tenantB, table), "%s hidden from another tenant", table)
		assert.Zero(t, countAs("", table), "%s hidden when no tenant is set", table)
	}

	// WITH CHECK: tenant B cannot write a row labelled tenant A.
	tx, err := e.app.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantB)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `INSERT INTO idempotency_keys (tenant_id, idempotency_key, operation, request_hash, resource_id, response_body)
		VALUES ($1, 'k', 'op', 'h', $2, '{}')`, tenantA, rec.RecommendationID)
	assert.Error(t, err, "insert of another tenant's row must violate the policy")
	_ = tx.Rollback(ctx)

	// Tenant B cannot move tenant A's row either (UPDATE sees nothing).
	tx, err = e.app.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantB)
	require.NoError(t, err)
	tag, err := tx.Exec(ctx, `UPDATE recommendations SET tenant_id = $1 WHERE recommendation_id = $2`, tenantB, rec.RecommendationID)
	require.NoError(t, err)
	assert.Zero(t, tag.RowsAffected())
	_ = tx.Rollback(ctx)

	// Disposition history is append-only, even for its own tenant.
	tx, err = e.app.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantA)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `UPDATE recommendation_dispositions SET reason = 'edited' WHERE recommendation_id = $1`, rec.RecommendationID)
	assert.Error(t, err)
	_ = tx.Rollback(ctx)
}

func TestOutboxRelay_DeliversOnceAndParksPermanentFailures(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tenantA, le := uuid.NewString(), uuid.NewString()
	e.create(t, tenantA, le)
	e.create(t, tenantA, le)

	var seen []domain.OutboxMessage
	delivered, err := e.store.DeliverPendingOutbox(ctx, 100, func(_ context.Context, m domain.OutboxMessage) error {
		seen = append(seen, m)
		if len(seen) == 2 {
			return errors.Join(store.ErrPermanentDelivery, errors.New("schema rejected"))
		}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 1, delivered)
	require.Len(t, seen, 2, "the relay (no tenant) drains rows across tenants via app.outbox_relay")

	delivered, err = e.store.DeliverPendingOutbox(ctx, 100, func(context.Context, domain.OutboxMessage) error {
		t.Fatal("nothing should be pending")
		return nil
	})
	require.NoError(t, err)
	assert.Zero(t, delivered)

	var deliveredRows, parkedRows int
	require.NoError(t, e.admin.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE delivered_at IS NOT NULL), count(*) FILTER (WHERE parked_at IS NOT NULL) FROM outbox`,
	).Scan(&deliveredRows, &parkedRows))
	assert.Equal(t, 1, deliveredRows)
	assert.Equal(t, 1, parkedRows)
}

func TestReadiness_ReflectsDatabase(t *testing.T) {
	e := newEnv(t)
	probes := health.New(e.app, zap.NewNop())
	rr := newRecorder()
	probes.Readiness(rr, newGet("/readyz"))
	assert.Equal(t, 200, rr.Code)
	assert.JSONEq(t, `{"status":"ready"}`, rr.Body.String())

	closed, err := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	require.NoError(t, err)
	closed.Close()
	rr = newRecorder()
	health.New(closed, zap.NewNop()).Readiness(rr, newGet("/readyz"))
	assert.Equal(t, 503, rr.Code)
}
