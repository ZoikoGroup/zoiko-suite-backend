package store_test

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/configuration-feature-flag-svc/internal/domain"
	"zoiko.io/configuration-feature-flag-svc/internal/store"
)

// Row-level security tests for config_entries and feature_flags
// (migration 000002).
//
// Every test here runs the store as a purpose-created NOSUPERUSER
// NOBYPASSRLS role (appRolePool). That is not incidental: TEST_DATABASE_URL
// points at `postgres`, a SUPERUSER, and a superuser bypasses row-level
// security unconditionally — FORCE included. Asserting isolation over that
// connection would prove only that the application predicate works, which
// is a different (already-tested) claim.

// TestRLS_PolicyIsEnabledAndForced guards the migration itself. Without
// this, every assertion below could pass simply because no policy is
// active — the failure mode is silent, so it gets its own explicit check.
func TestRLS_PolicyIsEnabledAndForced(t *testing.T) {
	ctx := context.Background()
	admin := openTestPool(t)

	for _, table := range []string{"config_entries", "feature_flags"} {
		var enabled, forced bool
		if err := admin.QueryRow(ctx,
			`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1`, table,
		).Scan(&enabled, &forced); err != nil {
			t.Fatalf("read pg_class for %s: %v", table, err)
		}
		if !enabled {
			t.Errorf("%s: migration 000002 must ENABLE row level security", table)
		}
		if !forced {
			t.Errorf("%s: migration 000002 must FORCE row level security", table)
		}
	}
}

// TestRLS_ConfigEntry_TenantIsolatedAtDatabaseLayer proves the policy
// isolates tenants independently of the query's own predicate: tenant B's
// session cannot see tenant A's row even on a SELECT carrying no tenant
// filter at all.
func TestRLS_ConfigEntry_TenantIsolatedAtDatabaseLayer(t *testing.T) {
	ctx := context.Background()
	admin := openTestPool(t)
	appPool := appRolePool(t, admin)

	tenantA := "11111111-1111-1111-1111-111111111111"
	tenantB := "22222222-2222-2222-2222-222222222222"

	s := store.New(appPool, zap.NewNop())
	if _, _, err := s.UpsertConfigEntry(ctx, domain.UpsertConfigEntryParams{
		Key: "payroll.batch_size", Environment: "prod", TenantID: strPtr(tenantA),
		Value: []byte(`100`), CreatedByPrincipalID: "admin-1",
	}); err != nil {
		t.Fatalf("write tenant A's entry: %v", err)
	}
	// A global default — NULL tenant_id — which must stay visible to everyone.
	if _, _, err := s.UpsertConfigEntry(ctx, domain.UpsertConfigEntryParams{
		Key: "payroll.batch_size", Environment: "prod", TenantID: nil,
		Value: []byte(`50`), CreatedByPrincipalID: "admin-1",
	}); err != nil {
		t.Fatalf("write global default: %v", err)
	}

	// Tenant B's session, deliberately querying with NO tenant predicate.
	// Only the policy can exclude tenant A's row here.
	conn, err := appPool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire conn: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT set_config('app.tenant_id', $1, false)", tenantB); err != nil {
		t.Fatalf("set tenant scope: %v", err)
	}

	rows, err := conn.Query(ctx, `SELECT tenant_id FROM config_entries`)
	if err != nil {
		t.Fatalf("unfiltered select: %v", err)
	}
	defer rows.Close()

	sawGlobal := false
	for rows.Next() {
		var tid *string
		if err := rows.Scan(&tid); err != nil {
			t.Fatalf("scan: %v", err)
		}
		switch {
		case tid == nil:
			sawGlobal = true
		case *tid == tenantA:
			t.Fatalf("ISOLATION FAILURE: tenant B's session saw tenant A's config row")
		case *tid != tenantB:
			t.Fatalf("unexpected tenant_id %q visible to tenant B", *tid)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}

	// The global default must NOT have been hidden. A plain
	// `tenant_id = app.tenant_id` policy would swallow it, turning
	// configuration that is meant to apply to every tenant into a silent
	// "not found" — a behaviour change dressed as security.
	if !sawGlobal {
		t.Fatalf("global default (NULL tenant_id) must remain visible to every tenant — the policy is over-restrictive")
	}
}

// TestRLS_FeatureFlag_TenantIsolatedAtDatabaseLayer is the feature_flags
// counterpart — the two tables carry separate policies, so a fix applied
// to one and missed on the other would otherwise go unnoticed.
func TestRLS_FeatureFlag_TenantIsolatedAtDatabaseLayer(t *testing.T) {
	ctx := context.Background()
	admin := openTestPool(t)
	appPool := appRolePool(t, admin)

	tenantA := "11111111-1111-1111-1111-111111111111"
	tenantB := "22222222-2222-2222-2222-222222222222"

	s := store.New(appPool, zap.NewNop())
	if _, _, err := s.UpsertFeatureFlag(ctx, domain.UpsertFeatureFlagParams{
		Key: "new-payroll-ui", Environment: "prod", TenantID: strPtr(tenantA),
		Enabled: true, CreatedByPrincipalID: "admin-1",
	}); err != nil {
		t.Fatalf("write tenant A's flag: %v", err)
	}

	conn, err := appPool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire conn: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT set_config('app.tenant_id', $1, false)", tenantB); err != nil {
		t.Fatalf("set tenant scope: %v", err)
	}

	var visible int
	if err := conn.QueryRow(ctx,
		`SELECT COUNT(*) FROM feature_flags WHERE tenant_id = $1`, tenantA,
	).Scan(&visible); err != nil {
		t.Fatalf("count tenant A's flags from tenant B's session: %v", err)
	}
	if visible != 0 {
		t.Fatalf("ISOLATION FAILURE: tenant B's session saw %d of tenant A's feature flags", visible)
	}
}

// TestRLS_WithCheckRefusesForeignTenantWrite covers the write side.
// USING alone governs visibility; without WITH CHECK a caller could
// INSERT a row attributed to another tenant that it then cannot read
// back — a silent, invisible write into someone else's configuration.
func TestRLS_WithCheckRefusesForeignTenantWrite(t *testing.T) {
	ctx := context.Background()
	admin := openTestPool(t)
	appPool := appRolePool(t, admin)

	tenantA := "11111111-1111-1111-1111-111111111111"
	tenantB := "22222222-2222-2222-2222-222222222222"

	conn, err := appPool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire conn: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT set_config('app.tenant_id', $1, false)", tenantB); err != nil {
		t.Fatalf("set tenant scope: %v", err)
	}

	for _, tc := range []struct{ table, cols, vals string }{
		{"config_entries",
			"(key, environment, tenant_id, value, created_by_principal_id)",
			"('forged.key', 'prod', $1, '1'::jsonb, 'attacker')"},
		{"feature_flags",
			"(key, environment, tenant_id, enabled, created_by_principal_id)",
			"('forged-flag', 'prod', $1, true, 'attacker')"},
	} {
		_, err := conn.Exec(ctx,
			"INSERT INTO "+tc.table+" "+tc.cols+" VALUES "+tc.vals, tenantA)
		if err == nil {
			t.Errorf("%s: WITH CHECK must refuse an insert attributed to another tenant", tc.table)
		}
	}
}
