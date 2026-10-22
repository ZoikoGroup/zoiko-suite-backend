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
	EnvURL      = "TEST_DATABASE_URL"
	EnvRequire  = "REQUIRE_DB_TESTS"
	DefaultURL  = "postgres://postgres:pgsuper@localhost:55432/postgres?sslmode=disable"
	appRole     = "ppr_app_test"
	appPassword = "ppr_app_test"
)

type DB struct {
	App   *pgxpool.Pool
	Admin *pgxpool.Pool
}

func Setup(t *testing.T) *DB {
	t.Helper()
	adminURL := os.Getenv(EnvURL)
	if adminURL == "" {
		adminURL = DefaultURL
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	maint, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		if os.Getenv(EnvRequire) == "1" {
			t.Fatalf("connect to maintenance database %s: %v", adminURL, err)
		}
		t.Skipf("cannot connect to postgres (%v): skipping real PostgreSQL integration test", err)
		return nil
	}
	defer func() { _ = maint.Close(context.Background()) }()

	_, err = maint.Exec(ctx, fmt.Sprintf(
		`CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE`, appRole, appPassword))
	var pgErr *pgconn.PgError
	if err != nil && !(errors.As(err, &pgErr) && (pgErr.Code == "42710" || pgErr.Code == "23505")) {
		t.Fatalf("create app role: %v", err)
	}

	dbName := "ppr_" + randHex(6)
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
