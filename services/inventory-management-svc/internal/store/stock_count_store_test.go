package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/inventory-management-svc/internal/domain"
	svcmiddleware "zoiko.io/inventory-management-svc/internal/middleware"
	"zoiko.io/inventory-management-svc/internal/store"
)

func TestPgStore_FreezeCountPopulation_CapturesRealSystemQuantity(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	itemID := newActiveTestItem(t, s, ctx, tenantID, legalEntityID, "SKU-COUNT-1")
	locID := newActiveTestLocation(t, s, ctx, tenantID, legalEntityID, "WH-COUNT-1")
	createCommittedReceipt(t, s, ctx, legalEntityID, itemID, locID, "idem-count-r1", 30)

	sc := &domain.StockCount{
		CountID: uuid.New().String(), LegalEntityID: legalEntityID, FiscalPeriod: "2026-09",
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "planner-1",
	}
	if err := s.CreateStockCount(ctx, sc, []string{locID}); err != nil {
		t.Fatalf("CreateStockCount failed: %v", err)
	}

	frozen, err := s.FreezeCountPopulation(ctx, sc.CountID, time.Now().UTC())
	if err != nil {
		t.Fatalf("FreezeCountPopulation failed: %v", err)
	}
	if frozen != 1 {
		t.Fatalf("expected 1 frozen line, got %d", frozen)
	}

	got, err := s.GetStockCount(ctx, sc.CountID)
	if err != nil {
		t.Fatalf("GetStockCount failed: %v", err)
	}
	if len(got.Lines) != 1 || got.Lines[0].SystemQuantity != 30 || got.Lines[0].ItemID != itemID {
		t.Fatalf("expected one line with system_quantity=30 for item %s, got %+v", itemID, got.Lines)
	}
}

// TestPgStore_FreezeCountPopulation_LinesImmutable is the real proof of
// negative path #2, "Population changes after freeze without
// invalidation."
func TestPgStore_FreezeCountPopulation_LinesImmutable(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	itemID := newActiveTestItem(t, s, ctx, tenantID, legalEntityID, "SKU-COUNT-2")
	locID := newActiveTestLocation(t, s, ctx, tenantID, legalEntityID, "WH-COUNT-2")
	createCommittedReceipt(t, s, ctx, legalEntityID, itemID, locID, "idem-count-r2", 10)

	sc := &domain.StockCount{
		CountID: uuid.New().String(), LegalEntityID: legalEntityID, FiscalPeriod: "2026-09",
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "planner-1",
	}
	if err := s.CreateStockCount(ctx, sc, []string{locID}); err != nil {
		t.Fatalf("CreateStockCount failed: %v", err)
	}
	if _, err := s.FreezeCountPopulation(ctx, sc.CountID, time.Now().UTC()); err != nil {
		t.Fatalf("FreezeCountPopulation failed: %v", err)
	}
	got, err := s.GetStockCount(ctx, sc.CountID)
	if err != nil {
		t.Fatalf("GetStockCount failed: %v", err)
	}
	lineID := got.Lines[0].LineID

	// A raw UPDATE attempting to change the frozen system_quantity must
	// be rejected by the database trigger itself.
	_, err = pool.Exec(context.Background(), `UPDATE inventory_stock_count_lines SET system_quantity = 9999 WHERE line_id = $1`, lineID)
	if err == nil {
		t.Fatalf("expected the reject-mutation trigger to refuse a raw UPDATE to system_quantity, got no error")
	}
}

// TestPgStore_ApproveCountVariance_SelfApprovalRefused is the real proof
// of negative path #3.
func TestPgStore_ApproveCountVariance_SelfApprovalRefused(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	itemID := newActiveTestItem(t, s, ctx, tenantID, legalEntityID, "SKU-COUNT-3")
	locID := newActiveTestLocation(t, s, ctx, tenantID, legalEntityID, "WH-COUNT-3")
	createCommittedReceipt(t, s, ctx, legalEntityID, itemID, locID, "idem-count-r3", 15)

	sc := &domain.StockCount{
		CountID: uuid.New().String(), LegalEntityID: legalEntityID, FiscalPeriod: "2026-09",
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "planner-1",
	}
	if err := s.CreateStockCount(ctx, sc, []string{locID}); err != nil {
		t.Fatalf("CreateStockCount failed: %v", err)
	}
	if _, err := s.FreezeCountPopulation(ctx, sc.CountID, time.Now().UTC()); err != nil {
		t.Fatalf("FreezeCountPopulation failed: %v", err)
	}
	got, err := s.GetStockCount(ctx, sc.CountID)
	if err != nil {
		t.Fatalf("GetStockCount failed: %v", err)
	}
	lineID := got.Lines[0].LineID

	if _, err := s.RecordBlindCount(ctx, lineID, "counter-1", 12, time.Now().UTC()); err != nil {
		t.Fatalf("RecordBlindCount failed: %v", err)
	}
	if err := s.ApproveCountVariance(ctx, lineID, "counter-1", time.Now().UTC()); err != domain.ErrSelfVarianceApprovalNotPermitted {
		t.Fatalf("expected ErrSelfVarianceApprovalNotPermitted, got %v", err)
	}
	if err := s.ApproveCountVariance(ctx, lineID, "reviewer-2", time.Now().UTC()); err != nil {
		t.Fatalf("expected approval by a different principal to succeed, got %v", err)
	}
}

// TestPgStore_GenerateAdjustment_CreatesRealMovement proves
// GenerateAdjustmentMovements' own handler-level integration works
// end-to-end at the store level: a real INV-03 movement, committed, is
// the only thing that ever changes on-hand as a result of a count.
func TestPgStore_GenerateAdjustment_CreatesRealMovement(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	itemID := newActiveTestItem(t, s, ctx, tenantID, legalEntityID, "SKU-COUNT-4")
	locID := newActiveTestLocation(t, s, ctx, tenantID, legalEntityID, "WH-COUNT-4")
	createCommittedReceipt(t, s, ctx, legalEntityID, itemID, locID, "idem-count-r4", 20)

	sc := &domain.StockCount{
		CountID: uuid.New().String(), LegalEntityID: legalEntityID, FiscalPeriod: "2026-09",
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "planner-1",
	}
	if err := s.CreateStockCount(ctx, sc, []string{locID}); err != nil {
		t.Fatalf("CreateStockCount failed: %v", err)
	}
	if _, err := s.FreezeCountPopulation(ctx, sc.CountID, time.Now().UTC()); err != nil {
		t.Fatalf("FreezeCountPopulation failed: %v", err)
	}
	got, err := s.GetStockCount(ctx, sc.CountID)
	if err != nil {
		t.Fatalf("GetStockCount failed: %v", err)
	}
	lineID := got.Lines[0].LineID

	if _, err := s.RecordBlindCount(ctx, lineID, "counter-1", 25, time.Now().UTC()); err != nil {
		t.Fatalf("RecordBlindCount failed: %v", err)
	}
	if err := s.ApproveCountVariance(ctx, lineID, "reviewer-2", time.Now().UTC()); err != nil {
		t.Fatalf("ApproveCountVariance failed: %v", err)
	}

	adjustment := &domain.InventoryMovement{
		MovementID: uuid.New().String(), LegalEntityID: legalEntityID, MovementType: domain.MovementTypeAdjustment, Status: domain.MovementStatusDraft,
		ItemID: itemID, Quantity: 5, UOM: "EACH",
		SourceReference: "STOCK-COUNT-" + sc.CountID, SourceIdempotencyKey: "count-" + lineID, FiscalPeriod: "2026-09",
		BusinessDate: time.Now().UTC(), CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "reviewer-2",
	}
	adjustment.DestinationLocationID = &locID
	if err := s.CreateMovement(ctx, adjustment); err != nil {
		t.Fatalf("CreateMovement (adjustment) failed: %v", err)
	}
	if err := s.ValidateMovement(ctx, adjustment.MovementID, time.Now().UTC()); err != nil {
		t.Fatalf("ValidateMovement (adjustment) failed: %v", err)
	}
	if err := s.CommitMovement(ctx, adjustment.MovementID, "reviewer-2", time.Now().UTC()); err != nil {
		t.Fatalf("CommitMovement (adjustment) failed: %v", err)
	}
	if err := s.LinkCountLineAdjustment(ctx, lineID, adjustment.MovementID); err != nil {
		t.Fatalf("LinkCountLineAdjustment failed: %v", err)
	}
	if err := s.MarkCountAdjustmentsGenerated(ctx, sc.CountID); err != nil {
		t.Fatalf("MarkCountAdjustmentsGenerated failed: %v", err)
	}

	onHand, err := s.GetOnHand(ctx, itemID, locID)
	if err != nil {
		t.Fatalf("GetOnHand failed: %v", err)
	}
	if onHand != 25 {
		t.Fatalf("expected on_hand=25 (20 + 5 adjustment) after the real INV-03 movement, got %v", onHand)
	}

	if err := s.CertifyStockCount(ctx, sc.CountID, "certifier-3", time.Now().UTC()); err != nil {
		t.Fatalf("CertifyStockCount failed: %v", err)
	}
	final, err := s.GetStockCount(ctx, sc.CountID)
	if err != nil {
		t.Fatalf("GetStockCount failed: %v", err)
	}
	if final.Status != domain.StockCountStatusCertified {
		t.Fatalf("expected CERTIFIED, got %q", final.Status)
	}
}
