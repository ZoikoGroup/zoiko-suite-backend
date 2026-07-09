package store_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/schema-registry-svc/internal/domain"
	"zoiko.io/schema-registry-svc/internal/store"
)

func newTestStore(t *testing.T) *store.PgStore {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, filename, _, _ := runtime.Caller(0)
	migPath := filepath.Join(filepath.Dir(filename), "../../deployments/migrations/000001_initial_schema.up.sql")
	migSQL, err := os.ReadFile(migPath)
	require.NoError(t, err)

	_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS event_schemas CASCADE;")
	_, err = pool.Exec(ctx, string(migSQL))
	require.NoError(t, err)

	return store.New(pool, zap.NewNop())
}

func TestPgStore_Insert_And_LatestVersion(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	schema1 := &domain.EventSchema{
		EventName:  "identity.context.resolved",
		Version:    1,
		JSONSchema: json.RawMessage(`{"properties":{"principal_id":{"type":"string"}},"required":["principal_id"]}`),
	}
	require.NoError(t, s.Insert(ctx, schema1))

	got, err := s.LatestVersion(ctx, "identity.context.resolved")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 1, got.Version)
	assert.JSONEq(t, string(schema1.JSONSchema), string(got.JSONSchema))

	schema2 := &domain.EventSchema{
		EventName:    "identity.context.resolved",
		Version:      2,
		JSONSchema:   json.RawMessage(`{"properties":{"principal_id":{"type":"string"},"tenant_id":{"type":"string"}},"required":["principal_id"]}`),
		RegisteredBy: "principal-admin-001",
	}
	require.NoError(t, s.Insert(ctx, schema2))

	got, err = s.LatestVersion(ctx, "identity.context.resolved")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 2, got.Version)
	assert.Equal(t, "principal-admin-001", got.RegisteredBy)
}

func TestPgStore_LatestVersion_NoneRegistered_ReturnsNil(t *testing.T) {
	s := newTestStore(t)
	got, err := s.LatestVersion(context.Background(), "never.registered")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestPgStore_Version_SpecificAndMissing(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.Insert(ctx, &domain.EventSchema{
		EventName:  "tenant.created",
		Version:    1,
		JSONSchema: json.RawMessage(`{"properties":{"tenant_id":{"type":"string"}},"required":["tenant_id"]}`),
	}))

	got, err := s.Version(ctx, "tenant.created", 1)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 1, got.Version)

	missing, err := s.Version(ctx, "tenant.created", 99)
	require.NoError(t, err)
	assert.Nil(t, missing)
}

func TestPgStore_Versions_ReturnsAllOldestFirst(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for v := 1; v <= 3; v++ {
		require.NoError(t, s.Insert(ctx, &domain.EventSchema{
			EventName:  "session.invalidated",
			Version:    v,
			JSONSchema: json.RawMessage(`{"properties":{},"required":[]}`),
		}))
	}

	versions, err := s.Versions(ctx, "session.invalidated")
	require.NoError(t, err)
	require.Len(t, versions, 3)
	assert.Equal(t, []int{1, 2, 3}, []int{versions[0].Version, versions[1].Version, versions[2].Version})
}

func TestPgStore_EventNames_ListsDistinctRegisteredEvents(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.Insert(ctx, &domain.EventSchema{
		EventName: "event.a", Version: 1, JSONSchema: json.RawMessage(`{}`),
	}))
	require.NoError(t, s.Insert(ctx, &domain.EventSchema{
		EventName: "event.b", Version: 1, JSONSchema: json.RawMessage(`{}`),
	}))

	names, err := s.EventNames(ctx)
	require.NoError(t, err)
	assert.Contains(t, names, "event.a")
	assert.Contains(t, names, "event.b")
}
