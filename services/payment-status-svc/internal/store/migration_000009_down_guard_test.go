//go:build integration

package store_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// connectAs opens a pool to the same test database under a different
// Postgres role/password, so a test can exercise behavior that depends on
// who is running the query (ownership, RLS, BYPASSRLS) rather than always
// going through the superuser connection TestMain sets up.
func connectAs(t *testing.T, user, password string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(testDSN)
	require.NoError(t, err)
	cfg.ConnConfig.User, cfg.ConnConfig.Password = user, password
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	require.NoError(t, err)
	return pool
}

// readMigrationFile reads one of 000009's own migration files, never a
// hand-copied inline string, so the test exercises exactly what ships.
func readMigrationFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "deployments", "migrations", name))
	require.NoError(t, err)
	return string(b)
}

// assertHasPendingUnknown checks whether the status CHECK constraint and the
// unresolved-source-reference unique index currently include PENDING_UNKNOWN
// (the up shape) or not (the down / pre-000009 shape).
func assertHasPendingUnknown(t *testing.T, ctx context.Context, want bool) {
	t.Helper()
	var checkDef string
	err := testPool.QueryRow(ctx, `
		SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid = 'payment_execution_states'::regclass
		  AND conname = 'payment_execution_states_status_check'`).Scan(&checkDef)
	require.NoError(t, err)
	var indexDef string
	err = testPool.QueryRow(ctx, `
		SELECT pg_get_indexdef(indexrelid) FROM pg_index
		JOIN pg_class ON pg_class.oid = pg_index.indexrelid
		WHERE pg_class.relname = 'uq_payment_execution_states_unresolved_source_reference'`).Scan(&indexDef)
	require.NoError(t, err)
	if want {
		require.Contains(t, checkDef, "PENDING_UNKNOWN")
		require.Contains(t, indexDef, "PENDING_UNKNOWN")
	} else {
		require.NotContains(t, checkDef, "PENDING_UNKNOWN")
		require.NotContains(t, indexDef, "PENDING_UNKNOWN")
	}
}

// TestMigration000009DownGuard reproduces, by hand, the exact blind-guard
// failure mode this week's other migrations were found to have (a
// DO $$ ... RAISE EXCEPTION ... EXISTS(...) $$ guard that silently passes
// under FORCE ROW LEVEL SECURITY instead of refusing) and confirms
// 000009_pending_unknown_state.down.sql does not have it — for both
// realistic roles that might ever run a migration.
//
// TestMain's own connection is the embedded-Postgres superuser, which always
// bypasses RLS no matter what this guard does, so it cannot exercise any of
// this; every check below runs through a dedicated, unprivileged connection.
func TestMigration000009DownGuard(t *testing.T) {
	ctx := context.Background()
	downSQL := readMigrationFile(t, "000009_pending_unknown_state.down.sql")
	upSQL := readMigrationFile(t, "000009_pending_unknown_state.up.sql")

	// downSQLWithoutGuard is the historical, buggy shape: identical to the
	// shipped down migration minus the one line this fix adds. Running it is
	// how the original defect is reproduced, not assumed.
	downSQLWithoutGuard := strings.Replace(downSQL, "SET LOCAL row_security = off;", "", 1)
	require.NotEqual(t, downSQL, downSQLWithoutGuard, "test setup: the guard line must actually be present in the shipped down migration")

	t.Run("NOBYPASSRLS owner: always refuses, by Postgres's own FORCE RLS rule", func(t *testing.T) {
		const role = "pss_migrator_nobypass"
		_, err := testPool.Exec(ctx, `
			DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '`+role+`') THEN
			  CREATE ROLE `+role+` LOGIN PASSWORD 'migrator' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS; END IF; END $$;
			GRANT USAGE, CREATE ON SCHEMA public TO `+role+`;
			GRANT ALL ON ALL TABLES IN SCHEMA public TO `+role+`;
			GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO `+role+`;
			ALTER TABLE payment_execution_states OWNER TO `+role+`;`)
		require.NoError(t, err)
		t.Cleanup(func() {
			_, _ = testPool.Exec(ctx, `ALTER TABLE payment_execution_states OWNER TO postgres;`)
		})

		pool := connectAs(t, role, "migrator")
		defer pool.Close()

		// This role's own query against a FORCE RLS table, with row_security
		// off and no BYPASSRLS, is refused by Postgres itself before the
		// guard's own RAISE EXCEPTION can even run — a stronger, built-in
		// refusal than the custom message, and true with or without any
		// PENDING_UNKNOWN row existing.
		_, err = pool.Exec(ctx, downSQL)
		require.Error(t, err)
		require.Contains(t, err.Error(), "row-level security policy")
		assertHasPendingUnknown(t, ctx, true)
	})

	t.Run("NOBYPASSRLS owner without the guard line: EXISTS() is blind, but ADD CONSTRAINT still catches it", func(t *testing.T) {
		const role = "pss_migrator_blind"
		_, err := testPool.Exec(ctx, `
			DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '`+role+`') THEN
			  CREATE ROLE `+role+` LOGIN PASSWORD 'migrator' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS; END IF; END $$;
			GRANT USAGE, CREATE ON SCHEMA public TO `+role+`;
			GRANT ALL ON ALL TABLES IN SCHEMA public TO `+role+`;
			GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO `+role+`;
			ALTER TABLE payment_execution_states OWNER TO `+role+`;`)
		require.NoError(t, err)
		t.Cleanup(func() {
			_, _ = testPool.Exec(ctx, `ALTER TABLE payment_execution_states OWNER TO postgres;`)
		})

		// A PENDING_UNKNOWN row that belongs to a real tenant: this role's
		// session never sets app.tenant_id, so the tenant_isolation policy
		// (tenant_id IS NULL OR tenant_id::text = app.tenant_id) would
		// normally filter it out of view entirely — the exact blind spot
		// the fix exists to close.
		var paymentID string
		err = testPool.QueryRow(ctx, `
			INSERT INTO payment_execution_states (tenant_id, legal_entity_id, created_by_principal_id, status)
			VALUES (gen_random_uuid(), gen_random_uuid(), 'test-actor', 'PENDING_UNKNOWN')
			RETURNING payment_id`).Scan(&paymentID)
		require.NoError(t, err)
		t.Cleanup(func() {
			_, _ = testPool.Exec(ctx, `UPDATE payment_execution_states SET status = 'SETTLED' WHERE payment_id = $1`, paymentID)
		})

		pool := connectAs(t, role, "migrator")
		defer pool.Close()

		// Without the guard line, row_security stays "on" (the default) for
		// this FORCE RLS table, so the owner's own EXISTS() check is
		// silently filtered to the empty set instead of erroring — the
		// blind-guard defect every other migration this week was found to
		// have, reproduced here, not assumed: it really does return false
		// while a real tenant's payment is still PENDING_UNKNOWN.
		//
		// But unlike those other migrations, this one does not actually go
		// on to corrupt data: the very next ALTER TABLE ... ADD CONSTRAINT
		// re-validates every physical row against the narrowed CHECK,
		// independent of RLS (constraint validation is not a policy-gated
		// read), and fails with Postgres's own generic constraint-violation
		// error instead of the named one. The DO-block guard only upgrades
		// that outcome from a confusing error to a legible one before the
		// index is even touched (see the migration's own comment) — it is
		// not, for this particular migration, the only thing standing
		// between a blind EXISTS() and silently losing data.
		_, err = pool.Exec(ctx, downSQLWithoutGuard)
		require.Error(t, err, "EXISTS() is blind here, but the ALTER TABLE ... ADD CONSTRAINT right after it is not — it must still refuse")
		require.Contains(t, err.Error(), "violated by some row")
		// Nothing to restore: the whole multi-statement script ran as one
		// implicit transaction, so the failed ADD CONSTRAINT rolled back the
		// DROP/CREATE INDEX that preceded it too.
		assertHasPendingUnknown(t, ctx, true)
	})

	t.Run("BYPASSRLS owner: refuses via the named guard while PENDING_UNKNOWN exists, succeeds once resolved", func(t *testing.T) {
		const role = "pss_migrator_bypass"
		_, err := testPool.Exec(ctx, `
			DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '`+role+`') THEN
			  CREATE ROLE `+role+` LOGIN PASSWORD 'migrator' NOSUPERUSER NOCREATEDB NOCREATEROLE BYPASSRLS; END IF; END $$;
			GRANT USAGE, CREATE ON SCHEMA public TO `+role+`;
			GRANT ALL ON ALL TABLES IN SCHEMA public TO `+role+`;
			GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO `+role+`;
			ALTER TABLE payment_execution_states OWNER TO `+role+`;`)
		require.NoError(t, err)
		t.Cleanup(func() {
			_, _ = testPool.Exec(ctx, `ALTER TABLE payment_execution_states OWNER TO postgres;`)
		})

		var paymentID string
		err = testPool.QueryRow(ctx, `
			INSERT INTO payment_execution_states (tenant_id, legal_entity_id, created_by_principal_id, status)
			VALUES (gen_random_uuid(), gen_random_uuid(), 'test-actor', 'PENDING_UNKNOWN')
			RETURNING payment_id`).Scan(&paymentID)
		require.NoError(t, err)
		t.Cleanup(func() {
			_, _ = testPool.Exec(ctx, `UPDATE payment_execution_states SET status = 'SETTLED' WHERE payment_id = $1`, paymentID)
		})

		pool := connectAs(t, role, "migrator")
		defer pool.Close()

		// BYPASSRLS sees every tenant's rows even with row_security off, so
		// the guard's own EXISTS() check runs for real and finds the row.
		_, err = pool.Exec(ctx, downSQL)
		require.Error(t, err, "the down migration must refuse while a row is PENDING_UNKNOWN, for a role that can actually see it")
		require.Contains(t, err.Error(), "cannot roll back 000009")
		assertHasPendingUnknown(t, ctx, true)

		// payment_execution_states rows are never deleted (trigger
		// reject_execution_state_mutation) — "cleared" means resolved to
		// another status, not removed.
		_, err = testPool.Exec(ctx, `UPDATE payment_execution_states SET status = 'SETTLED' WHERE payment_id = $1`, paymentID)
		require.NoError(t, err)

		_, err = pool.Exec(ctx, downSQL)
		require.NoError(t, err, "the down migration must succeed once no row is PENDING_UNKNOWN")
		assertHasPendingUnknown(t, ctx, false)

		// Restore the up shape so every other test in this package still
		// runs against the schema TestMain actually migrated to.
		_, err = testPool.Exec(ctx, upSQL)
		require.NoError(t, err)
		assertHasPendingUnknown(t, ctx, true)
	})
}
