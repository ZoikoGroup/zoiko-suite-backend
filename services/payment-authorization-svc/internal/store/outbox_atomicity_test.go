package store_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payment-authorization-svc/internal/outbox"
)

func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to connect to postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	_, filename, _, _ := runtime.Caller(0)
	base := filepath.Dir(filename)

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS outbox_events CASCADE;`)
	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS authorization_events CASCADE;`)
	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS authorization_payee_snapshots CASCADE;`)
	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS payment_authorizations CASCADE;`)

	migrationDir := filepath.Join(base, "../../deployments/migrations")
	migrations, err := filepath.Glob(filepath.Join(migrationDir, "*.up.sql"))
	if err != nil {
		t.Fatalf("failed to glob migrations: %v", err)
	}
	sort.Strings(migrations)

	for _, migration := range migrations {
		sql, err := os.ReadFile(migration)
		if err != nil {
			t.Fatalf("failed to read migration %s: %v", filepath.Base(migration), err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("failed to apply migration %s: %v", filepath.Base(migration), err)
		}
	}

	return pool
}

func TestPgStore_Outbox_ForcedFailure_RollbackAtomicity_RealDB(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	authID := uuid.New().String()
	proposalID := uuid.New().String()
	correlationID := uuid.New().String()
	outboxEventID := uuid.New().String()

	// 1. Begin raw transaction
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx) //nolint:errcheck

	// Set tenant context for RLS in this tx
	_, err = tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID)
	require.NoError(t, err)

	// 2. Write the domain business row into payment_authorizations
	const insertAuthSQL = `
		INSERT INTO payment_authorizations (
			authorization_id, tenant_id, legal_entity_id, proposal_id,
			proposal_fingerprint, net_amount, currency, status,
			requested_by_principal_id, created_at, updated_at
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7, $8,
			$9, now(), now()
		)
	`
	_, err = tx.Exec(ctx, insertAuthSQL,
		authID, tenantID, legalEntityID, proposalID,
		"fp-001", 5000.0, "USD", "PENDING",
		"requester-1",
	)
	require.NoError(t, err)

	// 3. Write the outbox row
	err = outbox.Insert(ctx, tx, outbox.Event{
		OutboxEventID: outboxEventID,
		AggregateType: "PAYMENT_AUTHORIZATION",
		AggregateID:   authID,
		EventType:     "payment_authorization.requested",
		TenantID:      &tenantID,
		LegalEntityID: legalEntityID,
		CorrelationID: &correlationID,
		Payload:       map[string]any{"authorization_id": authID},
	})
	require.NoError(t, err)

	// 4. Deliberately fail the transaction before commit
	// (Trigger duplicate primary key on outbox_events)
	_, err = tx.Exec(ctx, "INSERT INTO outbox_events (outbox_event_id) VALUES ($1)", outboxEventID)
	require.Error(t, err, "expected duplicate primary key constraint collision")

	err = tx.Rollback(ctx)
	require.NoError(t, err)

	// 5. Query fresh connection (pool) and assert NEITHER row exists
	var authCount, outboxCount int
	err = pool.QueryRow(ctx, "SELECT count(*) FROM payment_authorizations WHERE authorization_id = $1", authID).Scan(&authCount)
	require.NoError(t, err)

	err = pool.QueryRow(ctx, "SELECT count(*) FROM outbox_events WHERE aggregate_id = $1", authID).Scan(&outboxCount)
	require.NoError(t, err)

	assert.Equal(t, 0, authCount, "domain payment_authorization row must not exist after rollback")
	assert.Equal(t, 0, outboxCount, "outbox row must not exist after rollback")
}
