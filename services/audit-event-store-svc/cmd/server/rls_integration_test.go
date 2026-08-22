//go:build integration

// Row-level security integration tests for audit_events (migration 000003).
//
// Run with:
//
//	go test -v -tags=integration -timeout=120s ./cmd/server/
//
// Shares freePort() and applyMigrations() with main_integration_test.go
// (same package).
package main

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/audit-event-store-svc/internal/store"
)

// startRLSPostgres boots an embedded Postgres with the real migrations
// applied and returns an admin pool plus its port/dbname.
func startRLSPostgres(t *testing.T, ctx context.Context, dbName string) (*pgxpool.Pool, int) {
	t.Helper()
	port := freePort(t)

	pg := embeddedpostgres.NewDatabase(
		embeddedpostgres.DefaultConfig().
			// Pinned for the same reason as every other suite here — see
			// the comment in main_integration_test.go.
			Version(embeddedpostgres.V16).
			Port(uint32(port)).
			Database(dbName).
			Username("postgres").
			Password("postgres"),
	)
	require.NoError(t, pg.Start(), "start embedded postgres")
	t.Cleanup(func() { _ = pg.Stop() })

	adminDSN := fmt.Sprintf("postgres://postgres:postgres@localhost:%d/%s?sslmode=disable", port, dbName)
	admin, err := pgxpool.New(ctx, adminDSN)
	require.NoError(t, err)
	t.Cleanup(admin.Close)

	require.Eventually(t, func() bool { return admin.Ping(ctx) == nil },
		10*time.Second, 200*time.Millisecond, "embedded postgres did not become ready")

	applyMigrations(t, ctx, admin)
	return admin, port
}

// appRolePool returns a pool connected as a genuine NOSUPERUSER
// NOBYPASSRLS role.
//
// This is mandatory for any assertion about RLS: embedded-postgres
// connects as its own SUPERUSER, and a superuser bypasses row-level
// security unconditionally — FORCE included. A test that "proves
// isolation" over a superuser connection proves nothing; it silently
// skips the policy entirely.
//
// Both user and password are set on a URL-built DSN rather than
// string-patched, and the role's privileges are asserted before use — a
// role that silently retained BYPASSRLS would make every assertion here
// pass for the wrong reason.
func appRolePool(t *testing.T, ctx context.Context, admin *pgxpool.Pool, port int, dbName string) *pgxpool.Pool {
	t.Helper()

	const appRole = "zoiko_app_test"
	const appPassword = "zoiko_app_test_pw"

	_, err := admin.Exec(ctx, `DO $do$ BEGIN
		IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '`+appRole+`') THEN
			CREATE ROLE `+appRole+` LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
		END IF;
	END $do$;`)
	require.NoErrorf(t, err, "create role %s", appRole)

	for _, stmt := range []string{
		`ALTER ROLE ` + appRole + ` WITH LOGIN PASSWORD '` + appPassword + `' NOSUPERUSER NOBYPASSRLS`,
		`GRANT USAGE ON SCHEMA public TO ` + appRole,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ` + appRole,
		`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO ` + appRole,
	} {
		_, err := admin.Exec(ctx, stmt)
		require.NoErrorf(t, err, "grant to %s: %s", appRole, stmt)
	}

	u := &url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(appRole, appPassword),
		Host:     fmt.Sprintf("localhost:%d", port),
		Path:     "/" + dbName,
		RawQuery: "sslmode=disable",
	}
	pool, err := pgxpool.New(ctx, u.String())
	require.NoErrorf(t, err, "connect as %s", appRole)
	t.Cleanup(pool.Close)

	var isSuper, bypassRLS bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`,
	).Scan(&isSuper, &bypassRLS), "verify role privileges")
	require.False(t, isSuper, "%s must be NOSUPERUSER for RLS assertions to mean anything", appRole)
	require.False(t, bypassRLS, "%s must be NOBYPASSRLS for RLS assertions to mean anything", appRole)

	return pool
}

// TestPgStore_RLS_WriterStillWorksUnderForcedRLS is the regression test
// for the failure mode migration 000003 was designed around, and the most
// important test in this package.
//
// audit_events is FORCE row-level security; this service sets no
// app.tenant_id (it is a Kafka consumer — there is no X-Tenant-Id to read
// one from); and the hash chain is deliberately global. Under a
// tenant-only policy the chain-tip SELECT would match zero rows, every
// event would believe it is genesis, and the INSERT's WITH CHECK would
// reject the row — the platform's append-only evidence store would stop
// accepting events, quietly, because a consumer's insert error is
// absorbed by the DLQ rather than surfaced to any caller.
//
// This runs the real PgStore as a NOSUPERUSER NOBYPASSRLS role and
// asserts the chain still forms correctly ACROSS tenants. If the
// platform-scope escape hatch is ever removed or renamed, this test fails
// instead of production going silent.
func TestPgStore_RLS_WriterStillWorksUnderForcedRLS(t *testing.T) {
	ctx := context.Background()
	admin, port := startRLSPostgres(t, ctx, "audit_rls_writer_test")

	// Confirm the migration really did enable AND force RLS — otherwise
	// everything below would pass simply because no policy is active.
	var enabled, forced bool
	require.NoError(t, admin.QueryRow(ctx,
		`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = 'audit_events'`,
	).Scan(&enabled, &forced))
	require.True(t, enabled, "migration 000003 must ENABLE row level security")
	require.True(t, forced, "migration 000003 must FORCE row level security")

	appPool := appRolePool(t, ctx, admin, port, "audit_rls_writer_test")

	log, _ := zap.NewDevelopment()
	defer func() { _ = log.Sync() }()
	pgStore := store.NewPgStore(appPool, log)

	// Two different tenants, interleaved — the chain must span both.
	events := []*store.AuditEvent{
		{EventID: "rls-1", EventType: "a.happened", TenantID: "tenant-a", LegalEntityID: "le-a",
			SourceService: "svc", SchemaVersion: "1.0", Payload: []byte(`{"n":1}`)},
		{EventID: "rls-2", EventType: "b.happened", TenantID: "tenant-b", LegalEntityID: "le-b",
			SourceService: "svc", SchemaVersion: "1.0", Payload: []byte(`{"n":2}`)},
		{EventID: "rls-3", EventType: "a.happened", TenantID: "tenant-a", LegalEntityID: "le-a",
			SourceService: "svc", SchemaVersion: "1.0", Payload: []byte(`{"n":3}`)},
	}
	for _, e := range events {
		require.NoErrorf(t, pgStore.Store(ctx, e),
			"writer must still store event %s under FORCE RLS — a failure here means the evidence store is refusing events", e.EventID)
	}

	// Gap-free sequence numbers across tenants: proof the writer read the
	// true global chain tip rather than a tenant-scoped one.
	assert.Equal(t, int64(1), events[0].SequenceNumber)
	assert.Equal(t, int64(2), events[1].SequenceNumber, "chain must continue across a tenant boundary, not restart")
	assert.Equal(t, int64(3), events[2].SequenceNumber, "chain must continue across a tenant boundary, not restart")

	// And the links must be real: each row chains to the previous one
	// regardless of which tenant it belongs to.
	assert.Empty(t, events[0].PreviousEventHash, "genesis row has no previous hash")
	assert.Equal(t, events[0].PayloadHash, events[1].PreviousEventHash,
		"tenant-b's row must chain to tenant-a's — a per-tenant fork means the chain-tip read got scoped")
	assert.Equal(t, events[1].PayloadHash, events[2].PreviousEventHash)

	t.Log("Writer verified against FORCE RLS as NOSUPERUSER NOBYPASSRLS: 3-row cross-tenant chain intact.")
}

// TestPgStore_RLS_TenantScopedReaderIsIsolated proves the policy actually
// does something — that it is not inert.
//
// The only caller today is the writer, which is deliberately exempt (see
// above), so this test stands in for the query API Doc 03 §14.1 requires
// ("Records must be immutable and queryable by actor, entity, action,
// workflow, or time range") and which does not exist yet. It connects as
// the ordinary role WITHOUT platform scope, sets app.tenant_id, and
// asserts it sees only that tenant's rows. When the real query API is
// built it inherits this scoping by default, rather than needing RLS
// retrofitted onto a live evidence store afterwards.
func TestPgStore_RLS_TenantScopedReaderIsIsolated(t *testing.T) {
	ctx := context.Background()
	admin, port := startRLSPostgres(t, ctx, "audit_rls_reader_test")
	appPool := appRolePool(t, ctx, admin, port, "audit_rls_reader_test")

	log, _ := zap.NewDevelopment()
	defer func() { _ = log.Sync() }()
	pgStore := store.NewPgStore(appPool, log)

	for _, e := range []*store.AuditEvent{
		{EventID: "iso-a", EventType: "a", TenantID: "tenant-a", LegalEntityID: "le-a",
			SourceService: "svc", SchemaVersion: "1.0", Payload: []byte(`{"t":"a"}`)},
		{EventID: "iso-b", EventType: "b", TenantID: "tenant-b", LegalEntityID: "le-b",
			SourceService: "svc", SchemaVersion: "1.0", Payload: []byte(`{"t":"b"}`)},
	} {
		require.NoError(t, pgStore.Store(ctx, e))
	}

	// A future reader: ordinary role, tenant scope set, NO platform scope.
	// Acquired as a single connection so set_config applies to the same
	// session as the query.
	conn, err := appPool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()
	_, err = conn.Exec(ctx, "SELECT set_config('app.tenant_id', 'tenant-a', false)")
	require.NoError(t, err)

	rows, err := conn.Query(ctx, `SELECT event_id, tenant_id FROM audit_events ORDER BY sequence_number`)
	require.NoError(t, err)

	var seen []string
	for rows.Next() {
		var id, tenant string
		require.NoError(t, rows.Scan(&id, &tenant))
		assert.Equal(t, "tenant-a", tenant,
			"ISOLATION FAILURE: a tenant-a-scoped reader saw tenant %q's row %q", tenant, id)
		seen = append(seen, id)
	}
	require.NoError(t, rows.Err())
	rows.Close()

	assert.Equal(t, []string{"iso-a"}, seen,
		"a tenant-scoped reader must see exactly its own tenant's events — if tenant-b's row appears the policy is inert")

	// A write naming another tenant must be refused by WITH CHECK, not
	// silently accepted: USING alone would allow an insert the caller then
	// cannot read back.
	_, err = conn.Exec(ctx,
		`INSERT INTO audit_events
			(event_id, event_type, tenant_id, legal_entity_id, source_service, schema_version, payload, sequence_number, payload_hash)
		 VALUES ('iso-forged', 'forged', 'tenant-b', 'le-b', 'svc', '1.0', '{}', 99, 'deadbeef')`)
	assert.Error(t, err, "WITH CHECK must refuse an insert attributed to another tenant")
}
