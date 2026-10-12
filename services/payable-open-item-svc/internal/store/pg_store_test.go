package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/payable-open-item-svc/internal/domain"
	"zoiko.io/payable-open-item-svc/internal/middleware"
)

// TestPgStore_SettlementWithholdingAndReplay runs against a real, fully
// migrated database: per-tenant source uniqueness, net+withholding
// settlement, idempotent replay (which must not abort the transaction), and
// cent rounding of the residual.
func TestPgStore_SettlementWithholdingAndReplay(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}
	tenantA, tenantB := uuid.New().String(), uuid.New().String()
	ctxA := middleware.WithTenant(context.Background(), tenantA)
	ctxB := middleware.WithTenant(context.Background(), tenantB)
	pool, err := pgxpool.New(ctxA, dsn)
	if err != nil {
		t.Fatal(err)
	}
	s := NewPgStore(pool, zap.NewNop())
	le := uuid.New().String()
	req := domain.CreatePayableRequest{LegalEntityID: le, SourceType: domain.SourceSupplierInvoice, SourceReference: "INV-100",
		PayeeRef: "sup", OriginalAmount: 100, Currency: "USD", DueDate: time.Now()}

	p, err := s.CreatePayable(ctxA, tenantA, req, "p")
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	if _, err := s.CreatePayable(ctxB, tenantB, req, "p"); err != nil {
		t.Fatalf("same source in another tenant must be allowed now: %v", err)
	}

	u, applied, err := s.ApplyConfirmedPayment(ctxA, p.PayableID, domain.ApplyConfirmedPaymentRequest{Amount: 90, WithholdingAmount: 10, ProviderPaymentRef: "pay-1", Bnk07PaymentID: "pay-1"}, "p")
	if err != nil || !applied || u.Status != domain.StatusSettled || u.ResidualAmount != 0 {
		t.Fatalf("apply: %v %v %+v", err, applied, u)
	}
	_, applied, err = s.ApplyConfirmedPayment(ctxA, p.PayableID, domain.ApplyConfirmedPaymentRequest{Amount: 90, WithholdingAmount: 10, ProviderPaymentRef: "pay-1", Bnk07PaymentID: "pay-1"}, "p")
	if err != nil || applied {
		t.Fatalf("replay must be a no-op: %v %v", err, applied)
	}

	// float noise: 0.3 paid as 0.1 + 0.2 must fully settle
	req.SourceReference = "INV-FLOAT"
	req.OriginalAmount = 0.3
	f, _ := s.CreatePayable(ctxA, tenantA, req, "p")
	if _, _, err := s.ApplyConfirmedPayment(ctxA, f.PayableID, domain.ApplyConfirmedPaymentRequest{Amount: 0.1, WithholdingAmount: 0.2, ProviderPaymentRef: "pay-f", Bnk07PaymentID: "pay-f"}, "p"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.FindPayable(ctxA, f.PayableID)
	if got.Status != domain.StatusSettled {
		t.Fatalf("expected SETTLED after 0.1+0.2 on 0.3, got %s %.4f", got.Status, got.ResidualAmount)
	}
}
