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

	"zoiko.io/esignature-integration-svc/internal/domain"
	"zoiko.io/esignature-integration-svc/internal/middleware"
	"zoiko.io/esignature-integration-svc/internal/store"
)

// Row-level security tests for signature_envelopes (migration
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

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS signature_envelopes CASCADE;`)

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

	var enabled, forced bool
	if err := admin.QueryRow(ctx,
		`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = 'signature_envelopes'`,
	).Scan(&enabled, &forced); err != nil {
		t.Fatalf("read pg_class: %v", err)
	}
	if !enabled {
		t.Error("migration 002 must ENABLE row level security on signature_envelopes")
	}
	if !forced {
		t.Error("migration 002 must FORCE row level security on signature_envelopes")
	}
}

// TestRLS_PgStore_TenantIsolation exercises the real PgStore as an
// ordinary role: tenant B must not read tenant A's envelope, and tenant A
// must still read its own.
func TestRLS_PgStore_TenantIsolation(t *testing.T) {
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool)

	ctxA := middleware.WithTenant(context.Background(), "tenant-a")
	ctxB := middleware.WithTenant(context.Background(), "tenant-b")

	env := &domain.SignatureEnvelope{
		LegalEntityID: "le-a", Provider: "DOCUSIGN",
		DocumentTitle: "Board Resolution 2026-01",
		SignerEmail:   "signer@example.com", SignerName: "A Signer", Status: "SENT",
	}
	if err := s.CreateEnvelope(ctxA, env); err != nil {
		t.Fatalf("create tenant A's envelope: %v", err)
	}

	if got, err := s.GetEnvelopeByID(ctxB, env.EnvelopeID); err == nil {
		t.Fatalf("ISOLATION FAILURE: tenant B read tenant A's envelope: %+v", got)
	}

	list, err := s.ListEnvelopes(ctxB, "")
	if err != nil {
		t.Fatalf("list as tenant B: %v", err)
	}
	for _, e := range list {
		if e.TenantID != "tenant-b" {
			t.Fatalf("ISOLATION FAILURE: tenant B's unfiltered list returned tenant %q's row", e.TenantID)
		}
	}

	own, err := s.GetEnvelopeByID(ctxA, env.EnvelopeID)
	if err != nil {
		t.Fatalf("tenant A must still read its own envelope: %v", err)
	}
	if own.SignerEmail != "signer@example.com" {
		t.Fatalf("unexpected envelope returned for tenant A: %+v", own)
	}
}

// TestRLS_PgStore_UpdateStatus_ForeignTenantRefused covers the unscoped
// WRITE, the most serious hole in this service: UpdateEnvelopeStatus was
// `WHERE envelope_id = $4` alone, so any caller holding another tenant's
// envelope_id could mark that tenant's document signed or completed.
//
// Asserts both that the update is refused AND that tenant A's row is
// genuinely unchanged — a refusal that still mutated would be worse.
func TestRLS_PgStore_UpdateStatus_ForeignTenantRefused(t *testing.T) {
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool)

	ctxA := middleware.WithTenant(context.Background(), "tenant-a")
	ctxB := middleware.WithTenant(context.Background(), "tenant-b")

	env := &domain.SignatureEnvelope{
		LegalEntityID: "le-a", Provider: "DOCUSIGN", DocumentTitle: "Contract",
		SignerEmail: "signer@example.com", SignerName: "A Signer", Status: "SENT",
	}
	if err := s.CreateEnvelope(ctxA, env); err != nil {
		t.Fatalf("create tenant A's envelope: %v", err)
	}

	if got, err := s.UpdateEnvelopeStatus(ctxB, env.EnvelopeID, &domain.UpdateStatusRequest{
		Status: "COMPLETED", ExternalRef: "forged-ref",
	}); err == nil {
		t.Fatalf("ISOLATION FAILURE: tenant B changed the status of tenant A's envelope: %+v", got)
	}

	after, err := s.GetEnvelopeByID(ctxA, env.EnvelopeID)
	if err != nil {
		t.Fatalf("reload tenant A's envelope: %v", err)
	}
	if after.Status != "SENT" || after.ExternalRef == "forged-ref" {
		t.Fatalf("ISOLATION FAILURE: tenant A's envelope was mutated by tenant B: status=%q external_ref=%q", after.Status, after.ExternalRef)
	}
}

// TestRLS_WithCheckRefusesForeignTenantWrite covers the raw write side:
// USING alone governs visibility, so without WITH CHECK a caller could
// insert a row attributed to another tenant that it then cannot read back.
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
		INSERT INTO signature_envelopes
			(envelope_id, tenant_id, legal_entity_id, provider, document_title, signer_email, signer_name, status)
		VALUES ('forged-1', 'tenant-a', 'le-a', 'DOCUSIGN', 'Forged', 'x@example.com', 'X', 'COMPLETED')`)
	if err == nil {
		t.Fatal("WITH CHECK must refuse an insert attributed to another tenant")
	}
}
