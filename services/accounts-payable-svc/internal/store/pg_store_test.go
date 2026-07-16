package store_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/accounts-payable-svc/internal/domain"
	svcmiddleware "zoiko.io/accounts-payable-svc/internal/middleware"
	"zoiko.io/accounts-payable-svc/internal/store"
)

// openTestPool connects to a real Postgres and reapplies the migration from
// a clean slate. Skips (not fails) if TEST_DATABASE_URL isn't set — same
// convention as every other service in this platform.
func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to connect to postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	_, filename, _, _ := runtime.Caller(0)
	base := filepath.Dir(filename)

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS vendor_invoices CASCADE;`)

	sql, err := os.ReadFile(filepath.Join(base, "../../deployments/migrations/000001_initial_schema.up.sql"))
	if err != nil {
		t.Fatalf("failed to read migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("failed to apply migration: %v", err)
	}

	return pool
}

func newTestInvoice(tenantID string) *domain.VendorInvoice {
	return &domain.VendorInvoice{
		InvoiceID:            uuid.New().String(),
		TenantID:             tenantID,
		LegalEntityID:        uuid.New().String(),
		VendorID:             "vendor-1",
		InvoiceNumber:        "INV-" + uuid.New().String()[:8],
		Amount:               1000,
		CurrencyCode:         "USD",
		DueDate:              time.Now().Add(30 * 24 * time.Hour),
		Status:               domain.InvoiceStatusReceived,
		CreatedByPrincipalID: "test-admin",
		CorrelationID:        "corr-1",
	}
}

func TestPgStore_CreateInvoice_And_GetInvoice(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	inv := newTestInvoice(tenantID)
	if err := s.CreateInvoice(ctx, inv); err != nil {
		t.Fatalf("CreateInvoice failed: %v", err)
	}

	got, err := s.GetInvoice(ctx, inv.InvoiceID)
	if err != nil {
		t.Fatalf("GetInvoice failed: %v", err)
	}
	if got == nil {
		t.Fatal("expected invoice to be found")
	}
	if got.Status != domain.InvoiceStatusReceived {
		t.Fatalf("expected status RECEIVED, got %s", got.Status)
	}
}

func TestPgStore_TransitionInvoice_WrongFromStatus_Rejected(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	inv := newTestInvoice(tenantID)
	if err := s.CreateInvoice(ctx, inv); err != nil {
		t.Fatalf("CreateInvoice failed: %v", err)
	}

	// Invoice is RECEIVED — attempting VALIDATED -> APPROVED (wrong
	// fromStatus) must be rejected as a no-op (0 rows affected), never
	// silently succeed.
	err := s.TransitionInvoice(ctx, tenantID, inv.InvoiceID, domain.InvoiceStatusValidated, domain.InvoiceStatusApproved, "test-admin")
	if err == nil {
		t.Fatal("expected an error transitioning from the wrong fromStatus, got nil")
	}

	got, _ := s.GetInvoice(ctx, inv.InvoiceID)
	if got.Status != domain.InvoiceStatusReceived {
		t.Fatalf("invoice status must remain unchanged after a rejected transition, got %s", got.Status)
	}
}

func TestPgStore_RLS_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantA := uuid.New().String()
	tenantB := uuid.New().String()
	ctxA := svcmiddleware.WithTenant(context.Background(), tenantA)
	ctxB := svcmiddleware.WithTenant(context.Background(), tenantB)

	invA := newTestInvoice(tenantA)
	if err := s.CreateInvoice(ctxA, invA); err != nil {
		t.Fatalf("CreateInvoice (tenant A) failed: %v", err)
	}

	// Query tenant A's invoice while scoped to tenant B's context — this must
	// be hidden entirely, proving tenant isolation actually holds (not just
	// that the column exists). This is the exact class of check that caught
	// a real cross-tenant leak in general-ledger-svc via CI — every method
	// here filters explicitly by tenant_id for that reason, RLS alone is
	// insufficient given this pool connects as a Postgres superuser.
	got, err := s.GetInvoice(ctxB, invA.InvoiceID)
	if err != nil {
		t.Fatalf("GetInvoice under tenant B's context returned an error rather than a clean not-found: %v", err)
	}
	if got != nil {
		t.Fatal("tenant isolation failure: tenant B's session was able to read tenant A's invoice")
	}

	// Also verify TransitionInvoice can't be used to mutate another tenant's
	// row by supplying a mismatched tenantID explicitly.
	err = s.TransitionInvoice(ctxB, tenantB, invA.InvoiceID, domain.InvoiceStatusReceived, domain.InvoiceStatusValidated, "attacker")
	if err == nil {
		t.Fatal("tenant isolation failure: TransitionInvoice allowed tenant B to transition tenant A's invoice")
	}

	// Sanity: the same lookup under the correct tenant's context succeeds.
	gotA, err := s.GetInvoice(ctxA, invA.InvoiceID)
	if err != nil {
		t.Fatalf("GetInvoice under the correct tenant context failed: %v", err)
	}
	if gotA == nil {
		t.Fatal("expected invoice to be found under its own tenant's context")
	}
}

func TestPgStore_ListInvoices_TenantScoped(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantA := uuid.New().String()
	tenantB := uuid.New().String()
	ctxA := svcmiddleware.WithTenant(context.Background(), tenantA)

	invA := newTestInvoice(tenantA)
	if err := s.CreateInvoice(ctxA, invA); err != nil {
		t.Fatalf("CreateInvoice failed: %v", err)
	}

	listA, err := s.ListInvoices(ctxA, domain.ListInvoicesFilter{TenantID: tenantA})
	if err != nil {
		t.Fatalf("ListInvoices (tenant A) failed: %v", err)
	}
	if len(listA) != 1 {
		t.Fatalf("expected 1 invoice for tenant A, got %d", len(listA))
	}

	listB, err := s.ListInvoices(ctxA, domain.ListInvoicesFilter{TenantID: tenantB})
	if err != nil {
		t.Fatalf("ListInvoices (tenant B) failed: %v", err)
	}
	if len(listB) != 0 {
		t.Fatalf("expected 0 invoices for tenant B, got %d", len(listB))
	}
}
