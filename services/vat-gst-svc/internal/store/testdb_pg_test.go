package store_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// freshTestDB connects to the disposable Postgres named by TEST_DATABASE_URL
// (e.g. postgres://postgres:postgres@127.0.0.1:5433/<svc>_test), resets the
// public schema and replays every up-migration. Skipped when unset.
func freshTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping real-Postgres integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	_, filename, _, _ := runtime.Caller(0)
	migDir := filepath.Join(filepath.Dir(filename), "..", "..", "deployments", "migrations")
	migrations, err := filepath.Glob(filepath.Join(migDir, "*.up.sql"))
	if err != nil || len(migrations) == 0 {
		t.Fatalf("no migrations in %s: %v", migDir, err)
	}
	sort.Strings(migrations)
	for _, path := range migrations {
		sql, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(path), err)
		}
	}
	grantAppRole(t, pool)
	return pool
}

// grantAppRole gives the ordinary role named by TEST_APP_DATABASE_URL access
// to the freshly migrated schema, so RLS tests can run as that role.
func grantAppRole(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("TEST_APP_DATABASE_URL")
	if dsn == "" {
		return
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse TEST_APP_DATABASE_URL: %v", err)
	}
	role := pgx.Identifier{cfg.ConnConfig.User}.Sanitize()
	if _, err := pool.Exec(context.Background(), `GRANT USAGE ON SCHEMA public TO `+role+`;
		GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO `+role); err != nil {
		t.Fatalf("grant app role: %v", err)
	}
}

// appTestPool connects as the ordinary NOSUPERUSER NOBYPASSRLS role the
// service runs as, so FORCE ROW LEVEL SECURITY really binds. A superuser
// bypasses RLS entirely, which is why the superuser pool cannot prove it.
func appTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_APP_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_APP_DATABASE_URL not set: needs an ordinary NOBYPASSRLS role for RLS")
	}
	p, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect app role: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}
