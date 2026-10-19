// Package testdb provisions a real, throwaway PostgreSQL database for
// integration tests, using privacy-transfer-svc's own migrations.
//
// The application pool connects as a restricted role (NOSUPERUSER,
// NOBYPASSRLS, not the table owner), mirroring the zoiko_app role the
// service runs as in compose. A superuser connection would bypass RLS and
// make tenant isolation tests meaningless, so Setup verifies RLS applicability.
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
	EnvURL     = "TEST_DATABASE_URL"
	EnvRequire = "REQUIRE_DB_TESTS"

	appRole     = "pts_app_test"
	appPassword = "pts_app_test"
)

type DB struct {
	App   *pgxpool.Pool
	Admin *pgxpool.Pool
	name  string
}

// ApplyRemaining applies every migration after the first n (use after
// SetupThrough(t, n)) and re-grants table privileges to the app role.
func (d *DB) ApplyRemaining(t *testing.T, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	applyMigrations(ctx, t, d.Admin, n, 0)
	grant(ctx, t, d.Admin, d.name)
}

func grant(ctx context.Context, t *testing.T, admin *pgxpool.Pool, dbName string) {
	t.Helper()
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
}

// Setup provisions a database with every migration applied.
func Setup(t *testing.T) *DB { t.Helper(); return setup(t, 0) }

// SetupThrough provisions a database with migrations 1..n applied only, so a
// test can create "legacy" data before applying the remaining migrations with
// (*DB).ApplyRemaining.
func SetupThrough(t *testing.T, n int) *DB { t.Helper(); return setup(t, n) }

func setup(t *testing.T, through int) *DB {
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

	_, err = maint.Exec(ctx, fmt.Sprintf(
		`CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE`, appRole, appPassword))
	var pgErr *pgconn.PgError
	if err != nil && !(errors.As(err, &pgErr) && (pgErr.Code == "42710" || pgErr.Code == "23505")) {
		t.Fatalf("create app role: %v", err)
	}

	dbName := "pts_" + randHex(6)
	if _, err := maint.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create database: %v", err)
	}

	adminDBURL := withDatabase(t, adminURL, dbName, "", "")
	admin, err := pgxpool.New(ctx, adminDBURL)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}

	applyMigrations(ctx, t, admin, 0, through)

	grant(ctx, t, admin, dbName)

	app, err := pgxpool.New(ctx, withDatabase(t, adminURL, dbName, appRole, appPassword))
	if err != nil {
		t.Fatalf("app pool: %v", err)
	}

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

	return &DB{App: app, Admin: admin, name: dbName}
}

// applyMigrations applies files with index (1-based) > after and, when upTo>0,
// <= upTo.
func applyMigrations(ctx context.Context, t *testing.T, admin *pgxpool.Pool, after, upTo int) {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(thisFile), "..", "..", "deployments", "migrations")
	files, err := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations found in %s (err=%v)", dir, err)
	}
	sort.Strings(files)
	for i, f := range files {
		if i+1 <= after || (upTo > 0 && i+1 > upTo) {
			continue
		}
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
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
