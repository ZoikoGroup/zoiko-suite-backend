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

	"zoiko.io/hris-connector-svc/internal/domain"
	"zoiko.io/hris-connector-svc/internal/middleware"
	"zoiko.io/hris-connector-svc/internal/store"
)

// Row-level security tests for hris_integrations and sync_jobs (migration
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

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS sync_jobs, hris_integrations CASCADE;`)

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

	for _, table := range []string{"hris_integrations", "sync_jobs"} {
		var enabled, forced bool
		if err := admin.QueryRow(ctx,
			`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1`, table,
		).Scan(&enabled, &forced); err != nil {
			t.Fatalf("read pg_class for %s: %v", table, err)
		}
		if !enabled {
			t.Errorf("%s: migration 002 must ENABLE row level security", table)
		}
		if !forced {
			t.Errorf("%s: migration 002 must FORCE row level security", table)
		}
	}
}

// TestRLS_PgStore_TenantIsolation exercises the real PgStore as an
// ordinary role across both tables. api_endpoint is the address of the HR
// system holding a tenant's workforce data, so leaking it is the point of
// concern here.
func TestRLS_PgStore_TenantIsolation(t *testing.T) {
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool)

	ctxA := middleware.WithTenant(context.Background(), "tenant-a")
	ctxB := middleware.WithTenant(context.Background(), "tenant-b")

	integ := &domain.HrisIntegration{
		LegalEntityID: "le-a", ProviderName: "WORKDAY",
		ApiEndpoint: "https://tenant-a.workday.example/api", Status: "ACTIVE",
	}
	if err := s.CreateIntegration(ctxA, integ); err != nil {
		t.Fatalf("create tenant A's integration: %v", err)
	}

	if got, err := s.GetIntegrationByID(ctxB, integ.IntegrationID); err == nil {
		t.Fatalf("ISOLATION FAILURE: tenant B read tenant A's HR integration: %+v", got)
	}

	list, err := s.ListIntegrations(ctxB, "")
	if err != nil {
		t.Fatalf("list as tenant B: %v", err)
	}
	for _, x := range list {
		if x.TenantID != "tenant-b" {
			t.Fatalf("ISOLATION FAILURE: tenant B's unfiltered list returned tenant %q's row", x.TenantID)
		}
	}

	own, err := s.GetIntegrationByID(ctxA, integ.IntegrationID)
	if err != nil {
		t.Fatalf("tenant A must still read its own integration: %v", err)
	}
	if own.ApiEndpoint != "https://tenant-a.workday.example/api" {
		t.Fatalf("unexpected integration returned for tenant A: %+v", own)
	}
}

// TestRLS_PgStore_SyncJobs_Isolated covers the sync_jobs table, whose
// ListSyncJobs had a self-disabling filter: the ONLY predicate matched
// when the integration_id parameter was the empty string OR equalled the
// column, so calling it with no integration_id returned every tenant's
// sync history.
func TestRLS_PgStore_SyncJobs_Isolated(t *testing.T) {
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool)

	ctxA := middleware.WithTenant(context.Background(), "tenant-a")
	ctxB := middleware.WithTenant(context.Background(), "tenant-b")

	integ := &domain.HrisIntegration{
		LegalEntityID: "le-a", ProviderName: "WORKDAY",
		ApiEndpoint: "https://tenant-a.workday.example/api", Status: "ACTIVE",
	}
	if err := s.CreateIntegration(ctxA, integ); err != nil {
		t.Fatalf("create integration: %v", err)
	}
	job := &domain.SyncJob{
		IntegrationID: integ.IntegrationID, SyncType: "EMPLOYEE_FULL",
		RecordsSynced: 4211, Status: "COMPLETED",
	}
	if err := s.CreateSyncJob(ctxA, job); err != nil {
		t.Fatalf("create tenant A's sync job: %v", err)
	}

	if got, err := s.GetSyncJobByID(ctxB, job.JobID); err == nil {
		t.Fatalf("ISOLATION FAILURE: tenant B read tenant A's sync job: %+v", got)
	}

	// No integration filter — the call that used to leak everything.
	jobs, err := s.ListSyncJobs(ctxB, "")
	if err != nil {
		t.Fatalf("list sync jobs as tenant B: %v", err)
	}
	for _, j := range jobs {
		if j.TenantID != "tenant-b" {
			t.Fatalf("ISOLATION FAILURE: tenant B's unfiltered sync-job list returned tenant %q's job %s (records_synced=%d)", j.TenantID, j.JobID, j.RecordsSynced)
		}
	}
	if len(jobs) != 0 {
		t.Fatalf("expected tenant B to see no sync jobs, got %d", len(jobs))
	}

	ownJobs, err := s.ListSyncJobs(ctxA, "")
	if err != nil {
		t.Fatalf("list sync jobs as tenant A: %v", err)
	}
	if len(ownJobs) != 1 {
		t.Fatalf("expected tenant A to see its own 1 sync job, got %d", len(ownJobs))
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
		INSERT INTO hris_integrations
			(integration_id, tenant_id, legal_entity_id, provider_name, api_endpoint, status)
		VALUES ('forged-1', 'tenant-a', 'le-a', 'FORGED', 'https://attacker.example/api', 'ACTIVE')`)
	if err == nil {
		t.Fatal("WITH CHECK must refuse an insert attributed to another tenant")
	}
}
