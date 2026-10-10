//go:build integration

package store_test

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	svcmiddleware "zoiko.io/consolidation-svc/internal/middleware"
	"zoiko.io/consolidation-svc/internal/store"
	"zoiko.io/eventing/outbox"
)

// appPool connects as a NOSUPERUSER NOBYPASSRLS role, like the deployed
// zoiko_app. The embedded superuser bypasses row-level security entirely, so
// only a test running as such a role can see an RLS policy that locks the
// outbox relay out.
func appPool(t *testing.T, super *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	_, err := super.Exec(ctx, `
		DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cons_app') THEN
				CREATE ROLE cons_app WITH LOGIN PASSWORD 'cons_app' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
			END IF;
		END $$;
		GRANT USAGE ON SCHEMA public TO cons_app;
		GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO cons_app;`)
	require.NoError(t, err)
	dsn := os.Getenv("TEST_DATABASE_URL")
	require.Contains(t, dsn, "postgres:postgres@")
	pool, err := pgxpool.New(ctx, strings.Replace(dsn, "postgres:postgres@", "cons_app:cons_app@", 1))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

type capturingWriter struct {
	mu   sync.Mutex
	msgs []outbox.Message
}

func (w *capturingWriter) WriteMessages(_ context.Context, msgs ...outbox.Message) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.msgs = append(w.msgs, msgs...)
	return nil
}

func (w *capturingWriter) Probe(context.Context) error { return nil }

// TestRelay_DeliversUnderAppRole: what the store enqueues as the app role, the
// relay connected as the same role must be able to claim and publish.
func TestRelay_DeliversUnderAppRole(t *testing.T) {
	app := appPool(t, openTestPool(t))
	s := store.New(app, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	run := newTestRun(tenantID, "group-le-1")
	run.Status = "RUNNING"
	require.NoError(t, s.CreateRun(ctx, run, "principal-1", "corr-1"))

	w := &capturingWriter{}
	relay, err := outbox.NewRelay(app, w, outbox.DefaultConfig(), zap.NewNop())
	require.NoError(t, err)
	for i := 0; i < 50; i++ {
		res, err := relay.DrainOnce(context.Background())
		require.NoError(t, err)
		if res.Claimed == 0 {
			break
		}
	}

	var delivered []string
	for _, m := range w.msgs {
		if strings.Contains(string(m.Value), `"subject":"urn:zoikosuite:consolidation-run:`+run.ConsolidationRunID+`"`) {
			delivered = append(delivered, string(m.Value))
		}
	}
	require.Len(t, delivered, 1, "the relay must deliver the run's event under the app role")
	assert.Contains(t, delivered[0], `"event_type":"consolidation.run.started"`)
}

// TestOutboxRLS_TenantCannotSeeAnotherTenantsEvents: the relay exception must
// not open the table to ordinary request-path reads.
func TestOutboxRLS_TenantCannotSeeAnotherTenantsEvents(t *testing.T) {
	app := appPool(t, openTestPool(t))
	s := store.New(app, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.NewString()
	run := newTestRun(tenantID, "group-le-1")
	require.NoError(t, s.CreateRun(svcmiddleware.WithTenant(context.Background(), tenantID), run, "principal-1", "corr-1"))

	ctx := context.Background()
	tx, err := app.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", uuid.NewString())
	require.NoError(t, err)
	var n int
	require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM eventing_outbox WHERE aggregate_id = $1`, run.ConsolidationRunID).Scan(&n))
	assert.Zero(t, n)
}
