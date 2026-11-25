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
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/procurement-workflow-svc/internal/domain"
	svcmiddleware "zoiko.io/procurement-workflow-svc/internal/middleware"
	"zoiko.io/procurement-workflow-svc/internal/store"
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

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS procurement_cases CASCADE;`)
	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS outbox_events CASCADE;`)

	// Every *.up.sql, sorted: this applied 000001 alone, so neither FORCE RLS
	// (000002) nor the outbox (000004) was in the schema under test.
	migrations, err := filepath.Glob(filepath.Join(base, "../../deployments/migrations", "*.up.sql"))
	if err != nil || len(migrations) == 0 {
		t.Fatalf("no *.up.sql migrations found: %v", err)
	}
	sort.Strings(migrations)
	for _, path := range migrations {
		sql, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("failed to read migration %s: %v", filepath.Base(path), err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("failed to apply migration %s: %v", filepath.Base(path), err)
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

func TestPgStore_CreateAndGet(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := svcmiddleware.WithTenant(context.Background(), "tenant-a")

	vendorID := "v-001"
	now := time.Now().UTC()
	pc := &domain.ProcurementCase{
		CaseID:                 "0e1c5a4e-0000-4000-8000-000000000001",
		TenantID:               "tenant-a",
		LegalEntityID:          "le-001",
		RequestedByPrincipalID: "user-001",
		Description:            "Laptops",
		Category:               "HARDWARE",
		Amount:                 2500,
		CurrencyCode:           "USD",
		VendorProfileID:        &vendorID,
		Status:                 domain.CaseStatusRequested,
		CorrelationID:          "corr-001",
		CreatedAt:              now,
		UpdatedAt:              now,
	}

	created, err := s.CreateCase(ctx, pc)
	if err != nil {
		t.Fatalf("CreateCase failed: %v", err)
	}
	if !created {
		t.Fatalf("expected created to be true")
	}

	got, err := s.GetCase(ctx, "0e1c5a4e-0000-4000-8000-000000000001")
	if err != nil {
		t.Fatalf("GetCase failed: %v", err)
	}
	if got.Description != pc.Description {
		t.Errorf("expected description %q, got %q", pc.Description, got.Description)
	}
}
