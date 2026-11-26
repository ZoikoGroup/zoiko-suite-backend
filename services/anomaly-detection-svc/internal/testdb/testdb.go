// Package testdb provisions a real-Postgres fixture for integration tests.
//
// Open gives each test package its own schema (so packages that `go test
// ./...` runs in parallel never drop each other's tables), applies every
// deployments/migrations/*.up.sql in order into it, and returns two pools:
//
//   - admin: the TEST_DATABASE_URL user (a superuser in CI), for setup and
//     raw verification SQL;
//   - app:   a NOSUPERUSER NOBYPASSRLS login role, the way the service runs
//     in production, so FORCE ROW LEVEL SECURITY is actually enforced.
//
// Environment:
//
//	TEST_DATABASE_URL  required; tests are skipped when unset
//	TEST_APP_ROLE      app role name (default zoiko_app_test)
//	TEST_APP_PASSWORD  app role password (default zoiko_app_test_pw)
package testdb

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

var identifier = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// Open returns (admin, app) pools whose search_path is schema, after a fresh
// schema has been created and all up migrations applied.
func Open(t testing.TB, schema string) (*pgxpool.Pool, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}
	role := envOr("TEST_APP_ROLE", "zoiko_app_test")
	password := envOr("TEST_APP_PASSWORD", "zoiko_app_test_pw")
	if !identifier.MatchString(schema) || !identifier.MatchString(role) {
		t.Fatalf("testdb: schema %q / role %q must be lower-case identifiers", schema, role)
	}
	ctx := context.Background()

	bootstrap, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("testdb: connect TEST_DATABASE_URL: %v", err)
	}
	defer bootstrap.Close()
	if _, err := bootstrap.Exec(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE; CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("testdb: recreate schema %s: %v", schema, err)
	}
	ensureRole(t, bootstrap, role, password)

	admin := newPool(t, dsn, schema, nil)
	applyMigrations(t, admin)

	for _, stmt := range []string{
		`GRANT USAGE ON SCHEMA ` + schema + ` TO ` + role,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA ` + schema + ` TO ` + role,
		`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA ` + schema + ` TO ` + role,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("testdb: %s: %v", stmt, err)
		}
	}

	app := newPool(t, dsn, schema, url.UserPassword(role, password))
	var superuser, bypass bool
	if err := app.QueryRow(ctx,
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&superuser, &bypass); err != nil {
		t.Fatalf("testdb: inspect app role: %v", err)
	}
	if superuser || bypass {
		t.Fatalf("testdb: app role %s must be NOSUPERUSER NOBYPASSRLS (super=%v bypassrls=%v)", role, superuser, bypass)
	}
	return admin, app
}

// ensureRole creates/normalises the app role under an advisory lock, so two
// test packages running concurrently do not race on pg_authid.
func ensureRole(t testing.TB, pool *pgxpool.Pool, role, password string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("testdb: acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtext('testdb-app-role'))`); err != nil {
		t.Fatalf("testdb: advisory lock: %v", err)
	}
	defer func() { _, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtext('testdb-app-role'))`) }()

	quotedPassword := strings.ReplaceAll(password, `'`, `''`)
	stmts := []string{
		`DO $do$ BEGIN
			IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '` + role + `') THEN
				CREATE ROLE ` + role + ` LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
			END IF;
		END $do$`,
		`ALTER ROLE ` + role + ` WITH LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '` + quotedPassword + `'`,
	}
	for _, stmt := range stmts {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("testdb: prepare role %s: %v", role, err)
		}
	}
}

func newPool(t testing.TB, dsn, schema string, user *url.Userinfo) *pgxpool.Pool {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("testdb: parse TEST_DATABASE_URL: %v", err)
	}
	if user != nil {
		u.User = user
	}
	cfg, err := pgxpool.ParseConfig(u.String())
	if err != nil {
		t.Fatalf("testdb: pool config: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("testdb: connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func applyMigrations(t testing.TB, admin *pgxpool.Pool) {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(thisFile), "..", "..", "deployments", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("testdb: read migrations: %v", err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		t.Fatalf("testdb: no migrations in %s", dir)
	}
	sort.Strings(names)
	for _, name := range names {
		sql, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("testdb: read %s: %v", name, err)
		}
		if _, err := admin.Exec(context.Background(), string(sql)); err != nil {
			t.Fatalf("testdb: apply %s: %v", name, err)
		}
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
