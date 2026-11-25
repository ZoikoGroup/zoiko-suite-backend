package store_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/purchase-order-svc/internal/store"
)

func appOnlyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_APP_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_APP_DATABASE_URL not set: needs an ordinary role for RLS")
	}
	p, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// 000008_outbox_rls: an ordinary session with no tenant and no relay flag
// sees no outbox row, and a tenant cannot write another tenant's event.
func TestOutboxRLS_UnscopedSeesNothing_CrossTenantInsertRefused(t *testing.T) {
	_ = openTestPool(t) // migrated, disposable database
	app := appOnlyPool(t)
	tenant := uuid.NewString()
	if _, _, err := store.New(app, zap.NewNop()).CreateIssued(tctx(tenant), legacyInput(tenant)); err != nil {
		t.Fatal(err)
	}

	var visible int
	if err := app.QueryRow(context.Background(), `SELECT count(*) FROM outbox_events`).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if visible != 0 {
		t.Fatalf("no tenant and no relay flag: RLS must hide every outbox row, saw %d", visible)
	}

	tx, err := app.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	if _, err := tx.Exec(context.Background(), `SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(context.Background(), `
		INSERT INTO outbox_events (outbox_event_id, aggregate_type, aggregate_id, event_type, tenant_id, legal_entity_id, payload)
		VALUES ($1, 'x', $2, 'Forged', $3, $4, '{}')`, uuid.New(), uuid.New(), uuid.New(), uuid.New())
	if err == nil || !strings.Contains(err.Error(), "row-level security") {
		t.Fatalf("inserting another tenant's outbox row must be refused by RLS, got %v", err)
	}
}
