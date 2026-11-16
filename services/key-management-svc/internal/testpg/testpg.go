// Package testpg gives this service's tests a real Postgres: a database of
// their own with deployments/migrations applied, reached through a
// NOSUPERUSER NOBYPASSRLS role so Row-Level Security is actually enforced.
//
// Convention, chosen from what this service's own tests assumed: none of them
// used a database or a build tag, and they all run under a plain
// `go test ./...`. So, like anomaly-detection-svc's store tests, these need
// TEST_DATABASE_URL (a superuser DSN) and SKIP without it, rather than
// introducing an -tags integration split the service never had. A plain run
// still exercises every handler path against MemoryStore; the Postgres runs
// are the same tests against PgStore.
//
// Each caller names its own database, so test packages that `go test` runs in
// parallel never drop and re-migrate each other's tables mid-test.
//
// Imported only from _test.go files.
package testpg

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	appRole     = "kms_app_test"
	appPassword = "kms_app_test_pw"
)

var (
	mu      sync.Mutex
	created = map[string]bool{}
)

// Pools is an admin (superuser) pool, for inspecting rows as they really are,
// and an app pool that is subject to RLS, for the store under test.
type Pools struct {
	Admin *pgxpool.Pool
	App   *pgxpool.Pool
}

// Open returns pools on database name, creating and migrating it the first
// time it is asked for in this test binary. It skips t when TEST_DATABASE_URL
// is unset.
func Open(t testing.TB, name string) Pools {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres test: TEST_DATABASE_URL not set")
	}
	base, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	ctx := context.Background()

	mu.Lock()
	defer mu.Unlock()
	if !created[name] {
		setup(t, ctx, base, name)
		created[name] = true
	}

	admin := connect(t, ctx, withDB(base, name))
	appURL := withDB(base, name)
	appURL.User = url.UserPassword(appRole, appPassword)
	return Pools{Admin: admin, App: connect(t, ctx, appURL)}
}

func setup(t testing.TB, ctx context.Context, base *url.URL, name string) {
	t.Helper()
	root := connect(t, ctx, base)
	defer root.Close()
	for _, stmt := range []string{
		`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`,
		`CREATE DATABASE ` + name,
		// Roles are cluster-wide and two test binaries may race to create
		// this one, so a concurrent creation is not an error.
		`DO $do$ BEGIN
			CREATE ROLE ` + appRole + ` LOGIN PASSWORD '` + appPassword + `' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
		EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL;
		END $do$`,
	} {
		if _, err := root.Exec(ctx, stmt); err != nil {
			t.Fatalf("testpg setup (%s): %v", firstLine(stmt), err)
		}
	}

	db := connect(t, ctx, withDB(base, name))
	defer db.Close()
	for _, file := range migrations(t) {
		sql, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(file), err)
		}
	}
	// The same grants deployments/init-db-phase6.sh gives zoiko_app.
	if _, err := db.Exec(ctx, `
		GRANT USAGE ON SCHEMA public TO `+appRole+`;
		GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO `+appRole+`;`); err != nil {
		t.Fatalf("grant app role: %v", err)
	}
}

func migrations(t testing.TB) []string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(here), "..", "..", "deployments", "migrations")
	files, err := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations in %s: %v", dir, err)
	}
	sort.Strings(files)
	return files
}

func withDB(base *url.URL, name string) *url.URL {
	u := *base
	u.Path = "/" + name
	return &u
}

func connect(t testing.TB, ctx context.Context, u *url.URL) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, u.String())
	if err == nil {
		err = pool.Ping(ctx)
	}
	if err != nil {
		t.Fatalf("connect %s@%s: %v", u.User.Username(), u.Path, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	return s
}
