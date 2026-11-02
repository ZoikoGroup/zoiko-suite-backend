//go:build integration

// Row-level security on the evidence tables (hardened by migration 000006):
// ENABLE + FORCE, an explicit WITH CHECK, and isolation proved as the
// NOSUPERUSER NOBYPASSRLS application role.
package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var tenantTables = []string{"migration_jobs", "migration_integrity_checks", "migration_audit_entries", "migration_idempotency_keys"}

func TestRLS_EvidenceTablesForcedWithCheck(t *testing.T) {
	ctx := context.Background()
	var isSuper, bypass bool
	require.NoError(t, appPool.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&isSuper, &bypass))
	require.False(t, isSuper || bypass, "the app pool must not bypass RLS")

	for _, table := range tenantTables {
		var enabled, forced bool
		require.NoError(t, superPool.QueryRow(ctx,
			`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1`, table).Scan(&enabled, &forced))
		assert.True(t, enabled, table)
		assert.True(t, forced, table)
		var policies, noCheck int
		require.NoError(t, superPool.QueryRow(ctx,
			`SELECT count(*), count(*) FILTER (WHERE with_check IS NULL) FROM pg_policies WHERE tablename = $1`, table).Scan(&policies, &noCheck))
		assert.Positive(t, policies, table)
		assert.Zero(t, noCheck, table+": every policy states WITH CHECK")
	}
}

func TestRLS_AppRoleIsolatesTenants(t *testing.T) {
	ctx := context.Background()
	tenantA, tenantB := uuid.NewString(), uuid.NewString()
	job := createJob(t, newStore(), tenantA, 2)

	count := func(tenant, table string) int {
		tx, err := appPool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()
		if tenant != "" {
			_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant)
			require.NoError(t, err)
		}
		var n int
		require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE tenant_id = $1`, tenantA).Scan(&n))
		return n
	}
	for _, table := range []string{"migration_jobs", "migration_audit_entries"} {
		assert.Positive(t, count(tenantA, table), table+" visible to its own tenant")
		assert.Zero(t, count(tenantB, table), table+" hidden from another tenant")
		assert.Zero(t, count("", table), table+" hidden without a tenant")
	}

	tx, err := appPool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantB)
	require.NoError(t, err)
	tag, err := tx.Exec(ctx, `UPDATE migration_jobs SET status = 'ARCHIVED' WHERE id = $1`, job.ID)
	require.NoError(t, err)
	assert.Zero(t, tag.RowsAffected(), "tenant B cannot touch tenant A's job")
	_, err = tx.Exec(ctx, `INSERT INTO migration_idempotency_keys (tenant_id, principal_id, idempotency_key, operation, request_sha256)
		VALUES ($1, 'p', 'k', 'op', 'h')`, tenantA)
	assert.Error(t, err, "WITH CHECK refuses a row labelled with another tenant")
}
