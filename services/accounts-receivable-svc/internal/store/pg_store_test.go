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

	"zoiko.io/accounts-receivable-svc/internal/domain"
	svcmiddleware "zoiko.io/accounts-receivable-svc/internal/middleware"
	"zoiko.io/accounts-receivable-svc/internal/store"
)

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

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS customer_invoices CASCADE;`)

	sql, err := os.ReadFile(filepath.Join(base, "../../deployments/migrations/000001_initial_schema.up.sql"))
	if err != nil {
		t.Fatalf("failed to read migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("failed to apply migration: %v", err)
	}

	return pool
}

func newTestInvoice(tenantID string) *domain.CustomerInvoice {
	return &domain.CustomerInvoice{
		InvoiceID:            uuid.New().String(),
		TenantID:             tenantID,
		LegalEntityID:        uuid.New().String(),
		CustomerID:           "customer-1",
		InvoiceNumber:        "INV-" + uuid.New().String()[:8],
		Amount:               1500.50,
		CurrencyCode:         "USD",
		DueDate:              time.Now().Add(15 * 24 * time.Hour),
		Status:               domain.InvoiceStatusIssued,
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
	if got.Status != domain.InvoiceStatusIssued {
		t.Fatalf("expected status ISSUED, got %s", got.Status)
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

	// Invoice is ISSUED — attempting SENT -> OVERDUE (wrong fromStatus) must be rejected
	err := s.TransitionInvoice(ctx, tenantID, inv.InvoiceID, domain.InvoiceStatusSent, domain.InvoiceStatusOverdue, "test-admin")
	if err == nil {
		t.Fatal("expected an error transitioning from the wrong fromStatus, got nil")
	}

	got, _ := s.GetInvoice(ctx, inv.InvoiceID)
	if got.Status != domain.InvoiceStatusIssued {
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

	// Query tenant A's invoice while scoped to tenant B's context — must be hidden (returns nil, nil)
	got, err := s.GetInvoice(ctxB, invA.InvoiceID)
	if err != nil {
		t.Fatalf("GetInvoice under tenant B's context returned an error: %v", err)
	}
	if got != nil {
		t.Fatal("tenant isolation failure: tenant B was able to read tenant A's invoice")
	}

	// Verify TransitionInvoice doesn't allow tenant B to transition tenant A's invoice
	err = s.TransitionInvoice(ctxB, tenantB, invA.InvoiceID, domain.InvoiceStatusIssued, domain.InvoiceStatusSent, "attacker")
	if err == nil {
		t.Fatal("tenant isolation failure: TransitionInvoice allowed tenant B to transition tenant A's invoice")
	}

	// Sanity: correct tenant succeeds
	gotA, err := s.GetInvoice(ctxA, invA.InvoiceID)
	if err != nil {
		t.Fatalf("GetInvoice failed under correct context: %v", err)
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
