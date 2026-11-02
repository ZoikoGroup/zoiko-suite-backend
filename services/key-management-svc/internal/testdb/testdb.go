// Package testdb opens the Postgres pools the integration tests run against.
// Imported only from _test.go files; nothing in cmd/ depends on it.
//
//	TEST_DATABASE_URL      owner/superuser connection. Applies every
//	                       deployments/migrations/*.up.sql (all idempotent)
//	                       and grants DML to the app role. Unset: skip.
//	TEST_APP_DATABASE_URL  an ordinary NOSUPERUSER NOBYPASSRLS login role.
//	                       The stores under test run as this role, because a
//	                       superuser bypasses row-level security entirely and
//	                       would make every RLS assertion vacuous.
package testdb

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pools is an owner pool (migrations, fixtures that must bypass the store)
// and an app pool (what the code under test uses).
type Pools struct {
	Admin *pgxpool.Pool
	App   *pgxpool.Pool
	// AppDSN reopens the app role, for restart-persistence tests.
	AppDSN string
}

// Open returns migrated pools or skips the test when TEST_DATABASE_URL is
// unset. When TEST_APP_DATABASE_URL is unset, App is nil and RequireApp skips.
func Open(t *testing.T) *Pools {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(admin.Close)
	applyMigrations(t, admin)

	p := &Pools{Admin: admin}
	appDSN := os.Getenv("TEST_APP_DATABASE_URL")
	if appDSN == "" {
		return p
	}
	app, err := pgxpool.New(ctx, appDSN)
	if err != nil {
		t.Fatalf("connect TEST_APP_DATABASE_URL: %v", err)
	}
	t.Cleanup(app.Close)
	var user string
	var super, bypass bool
	if err := app.QueryRow(ctx, `SELECT current_user, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).
		Scan(&user, &super, &bypass); err != nil {
		t.Fatalf("inspect app role: %v", err)
	}
	if super || bypass {
		t.Fatalf("TEST_APP_DATABASE_URL role %q is SUPERUSER or BYPASSRLS; RLS would not apply to it", user)
	}
	ident := pgx.Identifier{user}.Sanitize()
	if _, err := admin.Exec(ctx, `GRANT USAGE ON SCHEMA public TO `+ident+`;
		GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO `+ident); err != nil {
		t.Fatalf("grant app role: %v", err)
	}
	p.App = app
	p.AppDSN = appDSN
	return p
}

// RequireApp skips unless the ordinary-role pool is configured.
func (p *Pools) RequireApp(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if p.App == nil {
		t.Skip("Skipping RLS test: TEST_APP_DATABASE_URL (NOSUPERUSER NOBYPASSRLS role) not set")
	}
	return p.App
}

// Store returns the pool the code under test should use: the app role when
// configured, otherwise the owner.
func (p *Pools) Store() *pgxpool.Pool {
	if p.App != nil {
		return p.App
	}
	return p.Admin
}

// MigrationFiles lists the migration files with the given suffix, in order.
func MigrationFiles(t *testing.T, suffix string) []string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(file), "..", "..", "deployments", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), suffix) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

func applyMigrations(t *testing.T, admin *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	conn, err := admin.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	// Test packages run in parallel against the same database; serialise DDL.
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(810810)`); err != nil {
		t.Fatalf("advisory lock: %v", err)
	}
	defer func() { _, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock(810810)`) }()
	for _, f := range MigrationFiles(t, ".up.sql") {
		sql, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := conn.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(f), err)
		}
	}
}
