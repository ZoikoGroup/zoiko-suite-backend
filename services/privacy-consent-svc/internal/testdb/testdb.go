// Package testdb provisions a real, throwaway PostgreSQL database for
// integration tests, using THIS service's own migrations.
//
// It exists because the outbox bug this package was written to catch is
// invisible to every in-memory test: event_outbox is FORCE ROW LEVEL
// SECURITY, and a stub store has no row-level security to violate. The
// relay's bookkeeping UPDATEs silently matched zero rows under the real
// application role and nothing failed — handler tests, vet and build all
// passed.
//
// The application pool connects as a restricted role (NOSUPERUSER,
// NOBYPASSRLS, not the table owner), mirroring the zoiko_app role the
// service runs as in compose. A superuser connection would bypass RLS and
// make every one of these tests meaningless, so Setup refuses to hand back a
// pool whose role could bypass it.
//
// Not imported by production code.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// EnvURL names a SUPERUSER connection URL to a maintenance database, e.g.
	// postgres://postgres:secret@localhost:55432/postgres?sslmode=disable.
	EnvURL = "TEST_DATABASE_URL"
	// EnvRequire makes a missing EnvURL a test FAILURE instead of a skip. CI
	// sets it, so the store/outbox suites can never "pass" with every test
	// skipped.
	EnvRequire = "REQUIRE_DB_TESTS"

	appRole     = "pcs_app_test"
	appPassword = "pcs_app_test"
)

// DB is a provisioned database. App is the restricted application pool the
// code under test must use; Admin is a superuser pool for arranging and
// inspecting state past RLS (never to be handed to production code).
type DB struct {
	App   *pgxpool.Pool
	Admin *pgxpool.Pool
}

// Setup creates a fresh database, applies every *.up.sql migration in
// order, and returns pools for it. The database is dropped on test cleanup.
func Setup(t *testing.T) *DB {
	t.Helper()
	adminURL := os.Getenv(EnvURL)
	if adminURL == "" {
		if os.Getenv(EnvRequire) == "1" {
			t.Fatalf("%s is not set but %s=1: refusing to skip the real-PostgreSQL suite", EnvURL, EnvRequire)
		}
		t.Skipf("%s not set: skipping real-PostgreSQL integration test", EnvURL)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	maint, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect to maintenance database: %v", err)
	}
	defer func() { _ = maint.Close(context.Background()) }()

	// Roles are cluster-wide and go test runs packages in parallel, so two
	// packages can race to create it: a duplicate-object error is success.
	_, err = maint.Exec(ctx, fmt.Sprintf(
		`CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE`, appRole, appPassword))
	var pgErr *pgconn.PgError
	if err != nil && !(errors.As(err, &pgErr) && (pgErr.Code == "42710" || pgErr.Code == "23505")) {
		t.Fatalf("create app role: %v", err)
	}

	dbName := "pcs_" + randHex(6)
	if _, err := maint.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create database: %v", err)
	}

	adminDBURL := withDatabase(t, adminURL, dbName, "", "")
	admin, err := pgxpool.New(ctx, adminDBURL)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}

	applyMigrations(ctx, t, admin)

	for _, stmt := range []string{
		fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s", dbName, appRole),
		fmt.Sprintf("GRANT USAGE ON SCHEMA public TO %s", appRole),
		fmt.Sprintf("GRANT ALL ON ALL TABLES IN SCHEMA public TO %s", appRole),
		fmt.Sprintf("GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO %s", appRole),
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("grant (%s): %v", stmt, err)
		}
	}

	app, err := pgxpool.New(ctx, withDatabase(t, adminURL, dbName, appRole, appPassword))
	if err != nil {
		t.Fatalf("app pool: %v", err)
	}

	// Refuse to continue if the role could bypass RLS: every test below
	// would then pass vacuously.
	var super, bypass bool
	if err := app.QueryRow(ctx,
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass); err != nil {
		t.Fatalf("inspect app role: %v", err)
	}
	if super || bypass {
		t.Fatalf("app role %s has rolsuper=%v rolbypassrls=%v: RLS would not apply, integration tests would be meaningless", appRole, super, bypass)
	}

	t.Cleanup(func() {
		app.Close()
		admin.Close()
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		m, err := pgx.Connect(c, adminURL)
		if err != nil {
			return
		}
		defer func() { _ = m.Close(context.Background()) }()
		_, _ = m.Exec(c, "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
	})

	return &DB{App: app, Admin: admin}
}

func applyMigrations(ctx context.Context, t *testing.T, admin *pgxpool.Pool) {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(thisFile), "..", "..", "deployments", "migrations")
	files, err := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations found in %s (err=%v)", dir, err)
	}
	sort.Strings(files)
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		// No arguments => pgx uses the simple protocol, so a multi-statement
		// migration file runs as written.
		if _, err := admin.Exec(ctx, string(body)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(f), err)
		}
	}
}

func withDatabase(t *testing.T, raw, db, user, pass string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", EnvURL, err)
	}
	u.Path = "/" + strings.TrimPrefix(db, "/")
	if user != "" {
		u.User = url.UserPassword(user, pass)
	}
	return u.String()
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
