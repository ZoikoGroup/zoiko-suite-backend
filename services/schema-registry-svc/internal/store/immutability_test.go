package store_test

// Registered schema versions are immutable at the database level (migration
// 000005). Real PostgreSQL; skipped without TEST_DATABASE_URL like the rest of
// this package's integration tests.

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/schema-registry-svc/internal/domain"
)

const immutabilityAppRole = "schema_registry_test_app"

// appRolePool connects as a NOSUPERUSER role that has been granted the
// strongest privileges the application could ever hold on the table —
// SELECT, INSERT, UPDATE, DELETE — so the only thing that can stop a mutation
// is the trigger itself.
func appRolePool(t *testing.T, admin *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	dsn, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	require.NoError(t, err)

	_, err = admin.Exec(ctx, `DO $do$ BEGIN
		IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '`+immutabilityAppRole+`') THEN
			CREATE ROLE `+immutabilityAppRole+` LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE;
		END IF;
	END $do$;`)
	require.NoError(t, err)
	for _, stmt := range []string{
		`ALTER ROLE ` + immutabilityAppRole + ` WITH LOGIN PASSWORD 'app_test_pw' NOSUPERUSER`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON event_schemas TO ` + immutabilityAppRole,
	} {
		_, err := admin.Exec(ctx, stmt)
		require.NoError(t, err, stmt)
	}
	dsn.User = url.UserPassword(immutabilityAppRole, "app_test_pw")
	pool, err := pgxpool.New(ctx, dsn.String())
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	var super bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&super))
	require.False(t, super, "the application-role assertions are meaningless over a superuser connection")
	return pool
}

func TestEventSchemas_AreImmutableInTheDatabase(t *testing.T) {
	s := newTestStore(t) // applies every migration, including 000005
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	app := appRolePool(t, admin)

	// 1. Normal registration still works (INSERT is untouched).
	v1, err := s.Insert(ctx, &domain.EventSchema{
		EventName: "immutable.probe.event", CompatibilityMode: domain.CompatibilityBackward,
		JSONSchema: json.RawMessage(`{"properties":{"a":{"type":"string"}}}`), RegisteredBy: "p1", IdempotencyKey: "k1",
	}, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, v1.Version)
	v2, err := s.Insert(ctx, &domain.EventSchema{
		EventName: "immutable.probe.event", CompatibilityMode: domain.CompatibilityBackward,
		JSONSchema: json.RawMessage(`{"properties":{"a":{"type":"string"},"b":{"type":"string"}}}`), RegisteredBy: "p1",
	}, 1)
	require.NoError(t, err)
	assert.Equal(t, 2, v2.Version)

	// 2. A duplicate still behaves per the existing contract: the same
	// idempotency key returns the original row, a stale baseline is refused.
	again, err := s.Insert(ctx, &domain.EventSchema{
		EventName: "immutable.probe.event", CompatibilityMode: domain.CompatibilityBackward,
		JSONSchema: json.RawMessage(`{"properties":{"a":{"type":"string"}}}`), RegisteredBy: "p1", IdempotencyKey: "k1",
	}, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, again.Version, "replay returns the original version")
	_, err = s.Insert(ctx, &domain.EventSchema{
		EventName: "immutable.probe.event", CompatibilityMode: domain.CompatibilityBackward,
		JSONSchema: json.RawMessage(`{"properties":{"c":{"type":"string"}}}`),
	}, 1)
	require.ErrorIs(t, err, domain.ErrVersionRaced, "a stale baseline is still a lost race")

	// 3. UPDATE and DELETE are rejected — for the application role holding the
	// privileges, and even for a superuser.
	for name, pool := range map[string]*pgxpool.Pool{"application role": app, "superuser": admin} {
		for _, stmt := range []string{
			`UPDATE event_schemas SET json_schema = '{"properties":{"evil":{"type":"string"}}}'::jsonb WHERE event_name='immutable.probe.event' AND version=1`,
			`UPDATE event_schemas SET registered_by = 'someone-else' WHERE event_name='immutable.probe.event'`,
			`DELETE FROM event_schemas WHERE event_name='immutable.probe.event' AND version=2`,
			`DELETE FROM event_schemas`,
		} {
			_, err := pool.Exec(ctx, stmt)
			require.Errorf(t, err, "%s must be refused: %s", name, stmt)
			assert.Contains(t, err.Error(), "append-only", "%s: %s", name, stmt)
		}
	}
	_, err = admin.Exec(ctx, `TRUNCATE event_schemas`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "append-only")

	// 4. Nothing changed, and every read path still works.
	got, err := s.Version(ctx, "immutable.probe.event", 1)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.JSONEq(t, `{"properties":{"a":{"type":"string"}}}`, string(got.JSONSchema))
	assert.Equal(t, "p1", got.RegisteredBy)

	latest, err := s.LatestVersion(ctx, "immutable.probe.event")
	require.NoError(t, err)
	require.NotNil(t, latest)
	assert.Equal(t, 2, latest.Version)

	all, err := s.Versions(ctx, "immutable.probe.event", 100, 0)
	require.NoError(t, err)
	require.Len(t, all, 2)
	assert.Equal(t, []int{1, 2}, []int{all[0].Version, all[1].Version}, "oldest first")

	names, err := s.EventNames(ctx, 100, 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"immutable.probe.event"}, names)

	// 5. And registration keeps working after the refused mutations.
	v3, err := s.Insert(ctx, &domain.EventSchema{
		EventName: "immutable.probe.event", CompatibilityMode: domain.CompatibilityBackward,
		JSONSchema: json.RawMessage(`{"properties":{"a":{"type":"string"},"b":{"type":"string"},"c":{"type":"string"}}}`),
	}, 2)
	require.NoError(t, err)
	assert.Equal(t, 3, v3.Version)
}
