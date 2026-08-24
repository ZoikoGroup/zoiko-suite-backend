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

	"zoiko.io/tax-authority-interface-svc/internal/domain"
	"zoiko.io/tax-authority-interface-svc/internal/middleware"
	"zoiko.io/tax-authority-interface-svc/internal/store"
)

// Row-level security tests for tax_interfaces and tax_filing_submissions
// (migration 002_add_rls.sql), plus the PgStore tenant predicates added
// alongside it.
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

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS tax_filing_submissions, tax_interfaces CASCADE;`)

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

	for _, table := range []string{"tax_interfaces", "tax_filing_submissions"} {
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
// ordinary role: tenant B must not reach tenant A's interface, and tenant
// A must still reach its own.
func TestRLS_PgStore_TenantIsolation(t *testing.T) {
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool)

	ctxA := middleware.WithTenant(context.Background(), "tenant-a")
	ctxB := middleware.WithTenant(context.Background(), "tenant-b")

	tf := &domain.TaxInterface{
		LegalEntityID: "le-a", Jurisdiction: "GB",
		AuthorityName: "HMRC MTD UK", Protocol: "REST/OAuth2", Status: "ACTIVE",
	}
	if err := s.CreateInterface(ctxA, tf); err != nil {
		t.Fatalf("create tenant A's interface: %v", err)
	}

	if got, err := s.GetInterfaceByID(ctxB, tf.InterfaceID); err == nil {
		t.Fatalf("ISOLATION FAILURE: tenant B read tenant A's interface: %+v", got)
	}

	list, err := s.ListInterfaces(ctxB, "")
	if err != nil {
		t.Fatalf("list as tenant B: %v", err)
	}
	for _, x := range list {
		if x.TenantID != "tenant-b" {
			t.Fatalf("ISOLATION FAILURE: tenant B's unfiltered list returned tenant %q's row", x.TenantID)
		}
	}

	own, err := s.GetInterfaceByID(ctxA, tf.InterfaceID)
	if err != nil {
		t.Fatalf("tenant A must still read its own interface: %v", err)
	}
	if own.AuthorityName != "HMRC MTD UK" {
		t.Fatalf("unexpected interface returned for tenant A: %+v", own)
	}
}

// TestRLS_PgStore_ListSubmissions_NoFilter_DoesNotLeak covers this
// service's worst self-disabling filter: ListSubmissions' ONLY predicate
// matched when the interface_id parameter was the empty string OR equalled
// the column, so calling it with no interface_id returned every tenant's
// tax filings — tax_period, filing_type, tax_amount and the authority's
// ack_reference.
func TestRLS_PgStore_ListSubmissions_NoFilter_DoesNotLeak(t *testing.T) {
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool)

	ctxA := middleware.WithTenant(context.Background(), "tenant-a")
	ctxB := middleware.WithTenant(context.Background(), "tenant-b")

	tfA := &domain.TaxInterface{
		LegalEntityID: "le-a", Jurisdiction: "GB",
		AuthorityName: "HMRC MTD UK", Protocol: "REST/OAuth2", Status: "ACTIVE",
	}
	if err := s.CreateInterface(ctxA, tfA); err != nil {
		t.Fatalf("create tenant A's interface: %v", err)
	}
	if err := s.CreateSubmission(ctxA, &domain.TaxFilingSubmission{
		InterfaceID: tfA.InterfaceID, TaxPeriod: "2026-Q1", FilingType: "VAT",
		TaxAmount: 987654.32, Status: domain.TaxFilingSubmitted, AckReference: "HMRC-ACK-A-001",
	}); err != nil {
		t.Fatalf("create tenant A's submission: %v", err)
	}

	// Tenant B, no interface filter at all — the exact call that used to leak.
	subs, err := s.ListSubmissions(ctxB, "")
	if err != nil {
		t.Fatalf("list submissions as tenant B: %v", err)
	}
	for _, sub := range subs {
		if sub.TenantID != "tenant-b" {
			t.Fatalf("ISOLATION FAILURE: tenant B's unfiltered ListSubmissions returned tenant %q's filing %s (amount=%v ack=%q)",
				sub.TenantID, sub.SubmissionID, sub.TaxAmount, sub.AckReference)
		}
	}
	if len(subs) != 0 {
		t.Fatalf("expected tenant B to see no filings, got %d: %+v", len(subs), subs)
	}

	// Sanity: tenant A does see its own filing.
	ownSubs, err := s.ListSubmissions(ctxA, "")
	if err != nil {
		t.Fatalf("list submissions as tenant A: %v", err)
	}
	if len(ownSubs) != 1 {
		t.Fatalf("expected tenant A to see its own 1 filing, got %d", len(ownSubs))
	}
	if ownSubs[0].TaxAmount != 987654.32 {
		t.Fatalf("tenant A's own filing came back wrong: %+v", ownSubs[0])
	}
}

// TestRLS_PgStore_GetSubmissionByID_OtherTenant covers the most sensitive
// read in the service: the lookup was `WHERE submission_id = $1` alone, so
// a caller holding another tenant's submission_id could read its actual
// tax figures.
func TestRLS_PgStore_GetSubmissionByID_OtherTenant(t *testing.T) {
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool)

	ctxA := middleware.WithTenant(context.Background(), "tenant-a")
	ctxB := middleware.WithTenant(context.Background(), "tenant-b")

	tfA := &domain.TaxInterface{
		LegalEntityID: "le-a", Jurisdiction: "GB",
		AuthorityName: "HMRC MTD UK", Protocol: "REST/OAuth2", Status: "ACTIVE",
	}
	if err := s.CreateInterface(ctxA, tfA); err != nil {
		t.Fatalf("create tenant A's interface: %v", err)
	}
	subA := &domain.TaxFilingSubmission{
		InterfaceID: tfA.InterfaceID, TaxPeriod: "2026-Q1", FilingType: "VAT",
		TaxAmount: 987654.32, Status: domain.TaxFilingSubmitted, AckReference: "HMRC-ACK-A-001",
	}
	if err := s.CreateSubmission(ctxA, subA); err != nil {
		t.Fatalf("create tenant A's submission: %v", err)
	}

	if got, err := s.GetSubmissionByID(ctxB, subA.SubmissionID); err == nil {
		t.Fatalf("ISOLATION FAILURE: tenant B read tenant A's filing: %+v", got)
	}

	own, err := s.GetSubmissionByID(ctxA, subA.SubmissionID)
	if err != nil {
		t.Fatalf("tenant A must still read its own filing: %v", err)
	}
	if own.AckReference != "HMRC-ACK-A-001" {
		t.Fatalf("unexpected filing returned for tenant A: %+v", own)
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
		INSERT INTO tax_interfaces
			(interface_id, tenant_id, legal_entity_id, jurisdiction, authority_name, protocol, status)
		VALUES ('forged-1', 'tenant-a', 'le-a', 'GB', 'FORGED AUTHORITY', 'REST', 'ACTIVE')`)
	if err == nil {
		t.Fatal("WITH CHECK must refuse an insert attributed to another tenant")
	}
}
