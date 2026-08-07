package store_test

import (
	"context"
	"encoding/json"
	"errors"
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
	migDir := filepath.Join(filepath.Dir(filename), "..", "..", "deployments", "migrations")

	_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS event_schemas CASCADE;")

	// Every up migration in order, rather than naming one inline. A migration
	// added later is then applied by the tests automatically — the alternative
	// has already caused a live incident in this repo, where an unapplied
	// migration made every write fail in a way that read as a code bug.
	for _, name := range []string{
		"000001_initial_schema.up.sql",
		"000002_add_compatibility_mode.up.sql",
	} {
		migSQL, err := os.ReadFile(filepath.Join(migDir, name))
		require.NoError(t, err, "read migration %s", name)
		_, err = pool.Exec(ctx, string(migSQL))
		require.NoError(t, err, "apply migration %s", name)
	}

	return store.New(pool, zap.NewNop())
}

func TestPgStore_Insert_And_LatestVersion(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	schema1 := &domain.EventSchema{
		EventName:         "identity.context.resolved",
		JSONSchema:        json.RawMessage(`{"properties":{"principal_id":{"type":"string"}},"required":["principal_id"]}`),
		CompatibilityMode: domain.CompatibilityBackward,
	}
	// Version is assigned inside the INSERT; 0 means "no version yet".
	stored1, err := s.Insert(ctx, schema1, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, stored1.Version)

	got, err := s.LatestVersion(ctx, "identity.context.resolved")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 1, got.Version)
	assert.Equal(t, domain.CompatibilityBackward, got.CompatibilityMode,
		"an omitted mode must be stored as BACKWARD, not left blank")
	assert.JSONEq(t, string(schema1.JSONSchema), string(got.JSONSchema))

	schema2 := &domain.EventSchema{
		EventName:         "identity.context.resolved",
		JSONSchema:        json.RawMessage(`{"properties":{"principal_id":{"type":"string"},"tenant_id":{"type":"string"}},"required":["principal_id"]}`),
		CompatibilityMode: domain.CompatibilityBackward,
		RegisteredBy:      "principal-admin-001",
		OwningService:     "identity-context-svc",
	}
	stored2, err := s.Insert(ctx, schema2, 1)
	require.NoError(t, err)
	assert.Equal(t, 2, stored2.Version)

	got, err = s.LatestVersion(ctx, "identity.context.resolved")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 2, got.Version)
	assert.Equal(t, "principal-admin-001", got.RegisteredBy)
	assert.Equal(t, "identity-context-svc", got.OwningService)
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

	_, err := s.Insert(ctx, &domain.EventSchema{
		EventName:         "tenant.created",
		JSONSchema:        json.RawMessage(`{"properties":{"tenant_id":{"type":"string"}},"required":["tenant_id"]}`),
		CompatibilityMode: domain.CompatibilityBackward,
	}, 0)
	require.NoError(t, err)

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
		_, err := s.Insert(ctx, &domain.EventSchema{
			EventName:         "session.invalidated",
			JSONSchema:        json.RawMessage(`{"properties":{},"required":[]}`),
			CompatibilityMode: domain.CompatibilityBackward,
		}, v-1)
		require.NoError(t, err)
	}

	versions, err := s.Versions(ctx, "session.invalidated")
	require.NoError(t, err)
	require.Len(t, versions, 3)
	assert.Equal(t, []int{1, 2, 3}, []int{versions[0].Version, versions[1].Version, versions[2].Version})
}

func TestPgStore_EventNames_ListsDistinctRegisteredEvents(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, err := s.Insert(ctx, &domain.EventSchema{
		EventName: "event.a", JSONSchema: json.RawMessage(`{}`), CompatibilityMode: domain.CompatibilityBackward,
	}, 0)
	require.NoError(t, err)
	_, err = s.Insert(ctx, &domain.EventSchema{
		EventName: "event.b", JSONSchema: json.RawMessage(`{}`), CompatibilityMode: domain.CompatibilityBackward,
	}, 0)
	require.NoError(t, err)

	names, err := s.EventNames(ctx)
	require.NoError(t, err)
	assert.Contains(t, names, "event.a")
	assert.Contains(t, names, "event.b")
}

// TestPgStore_Insert_RefusesStaleBaseline is the regression test for a
// time-of-check/time-of-use race on version assignment.
//
// The handler previously read the latest version, added one, and inserted.
// Two concurrent registrations therefore computed the same number, and the
// loser collided with the (event_name, version) primary key — which the
// handler reported as a store failure, i.e. a 503. An ordinary race looked
// like a database outage.
//
// Worse than the status code: the loser's schema had been checked for
// compatibility against a version that was no longer latest, so retrying it
// server-side would have admitted a change nobody validated against the
// version that actually won. The version is now assigned inside the INSERT
// and guarded by the caller's baseline, so a stale baseline is refused.
func TestPgStore_Insert_RefusesStaleBaseline(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	base := func() *domain.EventSchema {
		return &domain.EventSchema{
			EventName:         "race.probe",
			JSONSchema:        json.RawMessage(`{"properties":{"a":{"type":"string"}},"required":[]}`),
			CompatibilityMode: domain.CompatibilityBackward,
		}
	}

	first, err := s.Insert(ctx, base(), 0)
	require.NoError(t, err)
	assert.Equal(t, 1, first.Version)

	// A second writer that still believes version 0 is latest — exactly the
	// state of a request that read before `first` landed.
	_, err = s.Insert(ctx, base(), 0)
	require.ErrorIs(t, err, domain.ErrVersionRaced,
		"a write against a stale baseline must be refused, not silently versioned")

	// The winner is untouched and no phantom version was created.
	versions, err := s.Versions(ctx, "race.probe")
	require.NoError(t, err)
	assert.Len(t, versions, 1, "the refused write must not have inserted anything")

	// Re-reading and retrying against the current baseline succeeds.
	third, err := s.Insert(ctx, base(), 1)
	require.NoError(t, err)
	assert.Equal(t, 2, third.Version)
}

// TestPgStore_Insert_ConcurrentRegistrationsProduceNoDuplicates drives the
// race for real rather than simulating it: N writers all read the same
// baseline and register at once. Exactly one must win, and the rest must be
// told they raced — never a duplicate version, never a store error.
func TestPgStore_Insert_ConcurrentRegistrationsProduceNoDuplicates(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	const writers = 8
	results := make(chan error, writers)
	for i := 0; i < writers; i++ {
		go func() {
			_, err := s.Insert(ctx, &domain.EventSchema{
				EventName:         "concurrent.probe",
				JSONSchema:        json.RawMessage(`{"properties":{},"required":[]}`),
				CompatibilityMode: domain.CompatibilityBackward,
			}, 0)
			results <- err
		}()
	}

	won, raced := 0, 0
	for i := 0; i < writers; i++ {
		switch err := <-results; {
		case err == nil:
			won++
		case errors.Is(err, domain.ErrVersionRaced):
			raced++
		default:
			t.Fatalf("unexpected error from a concurrent registration: %v", err)
		}
	}

	assert.Equal(t, 1, won, "exactly one writer may claim version 1")
	assert.Equal(t, writers-1, raced, "every other writer must be told it raced")

	versions, err := s.Versions(ctx, "concurrent.probe")
	require.NoError(t, err)
	assert.Len(t, versions, 1, "concurrent registrations must not produce duplicate versions")
}
