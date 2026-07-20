//go:build integration

package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/domain"
	svcmiddleware "zoiko.io/accounts-receivable-svc/internal/middleware"
	"zoiko.io/accounts-receivable-svc/internal/store"
)

var (
	testPool  *pgxpool.Pool
	testStore *store.PgStore
)

func TestMain(m *testing.M) {
	dbPort := uint32(15901 + uint32(os.Getpid()%499))
	pg := embeddedpostgres.NewDatabase(
		embeddedpostgres.DefaultConfig().
			Version(embeddedpostgres.PostgresVersion("16.1.0")).
			Port(dbPort).
			Database("ar_isolation_test").
			Username("postgres").
			Password("postgres"),
	)
	if err := pg.Start(); err != nil {
		fmt.Printf("failed to start embedded postgres: %v\n", err)
		os.Exit(1)
	}

	dsn := fmt.Sprintf(
		"host=localhost port=%d dbname=ar_isolation_test user=postgres password=postgres sslmode=disable",
		dbPort,
	)

	ctx := context.Background()
	var err error
	testPool, err = pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Printf("failed to connect to postgres: %v\n", err)
		_ = pg.Stop()
		os.Exit(1)
	}

	for i := 0; i < 75; i++ {
		if err = testPool.Ping(ctx); err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		fmt.Printf("postgres did not become ready: %v\n", err)
		testPool.Close()
		_ = pg.Stop()
		os.Exit(1)
	}

	sql, err := os.ReadFile("../../deployments/migrations/000001_initial_schema.up.sql")
	if err != nil {
		fmt.Printf("failed to read migration: %v\n", err)
		testPool.Close()
		_ = pg.Stop()
		os.Exit(1)
	}
	if _, err = testPool.Exec(ctx, string(sql)); err != nil {
		fmt.Printf("failed to apply migration: %v\n", err)
		testPool.Close()
		_ = pg.Stop()
		os.Exit(1)
	}

	testStore = store.New(testPool, zap.NewNop())

	code := m.Run()

	testPool.Close()
	_ = pg.Stop()
	os.Exit(code)
}

func cleanTable(t *testing.T) {
	t.Helper()
	_, err := testPool.Exec(context.Background(), "DELETE FROM customer_invoices;")
	if err != nil {
		t.Fatalf("failed to clean customer_invoices: %v", err)
	}
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
	cleanTable(t)
	s := testStore

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
	cleanTable(t)
	s := testStore

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
	cleanTable(t)
	s := testStore

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
	cleanTable(t)
	s := testStore

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
