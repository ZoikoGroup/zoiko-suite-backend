package store_test

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/connectivity-api-bridge-svc/internal/domain"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/store"
)

// Row-level security tests for api_bridges and ingestion_logs (migration
// 002_add_rls.sql), plus the PgStore tenant predicates added alongside it.
//
// These run as a purpose-created NOSUPERUSER NOBYPASSRLS role.
// TEST_DATABASE_URL points at `postgres`, a SUPERUSER, and a superuser
// bypasses row-level security unconditionally — FORCE included — so an
// isolation assertion made over that connection would prove nothing about
// the policy.

func openAdminPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(pool.Close)

	resetGovernanceObjects(ctx, pool)

	// Every migration in filename order, never one hardcoded name — a
	// suite that names migrations individually silently skips new ones,
	// which would leave the migration under test unapplied and the run
	// green for the wrong reason.
	_, thisFile, _, _ := runtime.Caller(0)
	migDir := filepath.Join(filepath.Dir(thisFile), "../../deployments/migrations")
	entries, err := os.ReadDir(migDir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	var migs []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") && !strings.Contains(e.Name(), ".down.") {
			migs = append(migs, e.Name())
		}
	}
	if len(migs) == 0 {
		t.Fatalf("no migrations found in %s", migDir)
	}
	sort.Strings(migs)
	for _, name := range migs {
		sqlBytes, err := os.ReadFile(filepath.Join(migDir, name))
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		if _, err := pool.Exec(ctx, string(sqlBytes)); err != nil {
			t.Fatalf("apply migration %s: %v", name, err)
		}
	}
	return pool
}

func appRolePool(t *testing.T, admin *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	const appRole = "zoiko_app_test"
	const appPassword = "zoiko_app_test_pw"

	if _, err := admin.Exec(ctx, `DO $do$ BEGIN
		IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '`+appRole+`') THEN
			CREATE ROLE `+appRole+` LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
		END IF;
	END $do$;`); err != nil {
		t.Fatalf("create role: %v", err)
	}
	for _, stmt := range []string{
		`ALTER ROLE ` + appRole + ` WITH LOGIN PASSWORD '` + appPassword + `' NOSUPERUSER NOBYPASSRLS`,
		`GRANT USAGE ON SCHEMA public TO ` + appRole,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ` + appRole,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("grant (%s): %v", stmt, err)
		}
	}

	u, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	u.User = url.UserPassword(appRole, appPassword)
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("connect as %s: %v", appRole, err)
	}
	t.Cleanup(pool.Close)

	var isSuper, bypassRLS bool
	if err := pool.QueryRow(ctx,
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`,
	).Scan(&isSuper, &bypassRLS); err != nil {
		t.Fatalf("verify privileges: %v", err)
	}
	if isSuper || bypassRLS {
		t.Fatalf("%s must be NOSUPERUSER and NOBYPASSRLS, got rolsuper=%v rolbypassrls=%v", appRole, isSuper, bypassRLS)
	}
	return pool
}

func TestRLS_EnabledAndForced(t *testing.T) {
	ctx := context.Background()
	admin := openAdminPool(t)

	for _, table := range []string{"api_bridges", "ingestion_logs", "connector_changes", "connector_approvals", "connector_health",
		"connector_health_transitions", "sync_checkpoints", "checkpoint_reset_requests", "external_profiles", "eventing_outbox", "eventing_repair_log",
		"xic_connectors", "xic_contract_versions", "xic_mapping_versions", "xic_contract_gates", "xic_validation_runs", "xic_contract_transitions", "xic_idempotency",
		"xic_connections", "xic_credential_bindings", "xic_connection_events", "xic_connection_checks", "xic_certifications", "xic_authorization_sessions", "xic_connection_requests", "xic_conn_idempotency",
		"xic_handoff_targets", "xic_inbound_exchanges", "xic_inbound_events", "xic_quarantine_requests",
		"xic_outbound_attempts", "xic_outbound_events"} {
		var enabled, forced bool
		if err := admin.QueryRow(ctx,
			`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1`, table,
		).Scan(&enabled, &forced); err != nil {
			t.Fatalf("read pg_class for %s: %v", table, err)
		}
		if !enabled {
			t.Errorf("%s: row level security must be ENABLED", table)
		}
		if !forced {
			t.Errorf("%s: row level security must be FORCED", table)
		}
	}
}

// TestRLS_PgStore_TenantIsolation exercises the real PgStore as an
// ordinary role: tenant B must not reach tenant A's bridge, and tenant A
// must still reach its own.
func TestRLS_PgStore_TenantIsolation(t *testing.T) {
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool)

	ctxA := middleware.WithTenant(context.Background(), "tenant-a")
	ctxB := middleware.WithTenant(context.Background(), "tenant-b")

	bridge := &domain.ApiBridge{
		LegalEntityID: "le-a", BridgeName: "Test Bridge", Protocol: "REST",
		EndpointURL: "https://erp.example.internal/api", AuthType: "OAUTH2", Status: "ACTIVE",
		LegacyUnapproved: true, // a pre-governance row: the only way to hold ACTIVE without an approval
	}
	if err := s.CreateBridge(ctxA, bridge); err != nil {
		t.Fatalf("create tenant A's bridge: %v", err)
	}

	// Tenant B, holding tenant A's bridge_id.
	if got, err := s.GetBridgeByID(ctxB, bridge.BridgeID); err == nil {
		t.Fatalf("ISOLATION FAILURE: tenant B read tenant A's bridge: %+v", got)
	}

	// Tenant B's unfiltered list must not include tenant A's row.
	list, err := s.ListBridges(ctxB, "")
	if err != nil {
		t.Fatalf("list as tenant B: %v", err)
	}
	for _, b := range list {
		if b.TenantID != "tenant-b" {
			t.Fatalf("ISOLATION FAILURE: tenant B's unfiltered list returned tenant %q's row", b.TenantID)
		}
	}

	// Sanity: tenant A still reads its own — the policy must not
	// over-restrict into a broken read.
	own, err := s.GetBridgeByID(ctxA, bridge.BridgeID)
	if err != nil {
		t.Fatalf("tenant A must still read its own bridge: %v", err)
	}
	if own.EndpointURL != "https://erp.example.internal/api" {
		t.Fatalf("unexpected bridge returned for tenant A: %+v", own)
	}
}

// TestRLS_WithCheckRefusesForeignTenantWrite covers the write side: USING
// alone governs visibility, so without WITH CHECK a caller could insert a
// row attributed to another tenant that it then cannot read back.
func TestRLS_WithCheckRefusesForeignTenantWrite(t *testing.T) {
	ctx := context.Background()
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)

	conn, err := appPool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT set_config('app.tenant_id', 'tenant-b', false)"); err != nil {
		t.Fatalf("set tenant scope: %v", err)
	}
	_, err = conn.Exec(ctx, `
		INSERT INTO api_bridges
			(bridge_id, tenant_id, legal_entity_id, bridge_name, protocol, endpoint_url, auth_type, status)
		VALUES ('forged-1', 'tenant-a', 'le-a', 'Forged', 'REST', 'https://attacker.example/api', 'NONE', 'DRAFT')`)
	if err == nil {
		t.Fatal("WITH CHECK must refuse an insert attributed to another tenant")
	}
}

// resetGovernanceObjects drops everything the migrations create, so the suite
// can re-apply them from scratch against the same database. Order does not
// matter: CASCADE removes the dependent foreign keys and triggers.
func resetGovernanceObjects(ctx context.Context, pool *pgxpool.Pool) {
	_, _ = pool.Exec(ctx, `
		DROP TABLE IF EXISTS checkpoint_reset_requests, sync_checkpoints, connector_health_transitions, connector_health,
			connector_approvals, connector_changes, external_profiles, eventing_outbox, eventing_repair_log,
			ingestion_logs, api_bridges, xic_idempotency, xic_contract_transitions, xic_validation_runs, xic_contract_gates,
			xic_mapping_versions, xic_contract_versions, xic_connectors,
			xic_conn_idempotency, xic_connection_requests, xic_authorization_sessions, xic_certifications,
			xic_outbound_events, xic_outbound_attempts,
			xic_quarantine_requests, xic_inbound_events, xic_inbound_exchanges, xic_handoff_targets,
			xic_connection_checks, xic_connection_events, xic_credential_bindings, xic_connections CASCADE;
		DROP FUNCTION IF EXISTS connector_append_only(), sync_checkpoints_guard(), external_profiles_guard(),
			eventing_outbox_payload_immutable(), eventing_repair_log_immutable(), xic_is_platform(), xic_append_only(),
			xic_connectors_guard(), xic_cv_guard(), xic_mv_guard(), xic_gate_guard(), xic_conn_guard(), xic_conn_insert_guard(),
			xic_bind_guard(), xic_session_guard(), xic_req_guard(), xic_target_guard(), xic_ix_guard(), xic_qr_guard(), xic_oa_guard() CASCADE;`)
}
