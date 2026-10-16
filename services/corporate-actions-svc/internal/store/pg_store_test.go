package store_test

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/corporate-actions-svc/internal/domain"
	"zoiko.io/corporate-actions-svc/internal/middleware"
	"zoiko.io/corporate-actions-svc/internal/store"
)

func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}
	requireThrowawayDatabase(t, dsn)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to connect to postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	_, filename, _, _ := runtime.Caller(0)
	base := filepath.Dir(filename)

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS corporate_actions CASCADE;`)

	for _, name := range []string{
		"000001_initial_schema.up.sql",
	} {
		sql, err := os.ReadFile(filepath.Join(base, "../../deployments/migrations", name))
		if err != nil {
			t.Fatalf("failed to read migration %s: %v", name, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("failed to apply migration %s: %v", name, err)
		}
	}

	return pool
}

func requireThrowawayDatabase(t *testing.T, dsn string) {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("refusing to run: TEST_DATABASE_URL is not a parseable URL: %v", err)
	}
	dbName := strings.TrimPrefix(u.Path, "/")
	if !strings.Contains(strings.ToLower(dbName), "test") {
		t.Fatalf("refusing to run: TEST_DATABASE_URL names database %q, which is not recognisably disposable", dbName)
	}
}

func tenantCtx(tenantID string) context.Context {
	return middleware.WithTenant(context.Background(), tenantID)
}

func TestPgStore_CreateAndGet(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := tenantCtx("tenant-a")

	act := &domain.CorporateAction{
		LegalEntityID: "le-001",
		Title:         "Share Issuance Series A",
		ActionType:    domain.ActionTypeShareIssuance,
		EffectiveDate: "2026-05-01",
		CreatedBy:     "user-001",
	}

	if err := s.CreateAction(ctx, act); err != nil {
		t.Fatalf("CreateAction failed: %v", err)
	}

	got, err := s.GetAction(ctx, act.ActionID)
	if err != nil {
		t.Fatalf("GetAction failed: %v", err)
	}
	if got.Title != act.Title {
		t.Errorf("expected title %q, got %q", act.Title, got.Title)
	}
}
