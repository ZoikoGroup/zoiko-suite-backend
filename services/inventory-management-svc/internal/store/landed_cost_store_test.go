package store_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/inventory-management-svc/internal/domain"
	svcmiddleware "zoiko.io/inventory-management-svc/internal/middleware"
	"zoiko.io/inventory-management-svc/internal/store"
)

func newLandedCost(movementID, key string, amount float64, at time.Time) *domain.LandedCostAllocation {
	return &domain.LandedCostAllocation{
		AllocationID: uuid.New().String(), MovementID: movementID, IdempotencyKey: key, Amount: amount,
		ValuationEvidenceRef: "FREIGHT-INV-77", FiscalPeriod: "2026-10",
		InventoryAccountCode: "INV-ASSET", COGSAccountCode: "COGS", OffsetAccountCode: "FREIGHT-PAYABLE",
		CreatedAt: at, CreatedByPrincipalID: "approver-2",
	}
}

// TestPgStore_AllocateLandedCost_SplitHistoryAndImmutability proves against
// real Postgres: the on-hand / consumed split, that the live layer value rises
// by exactly the inventory share, that a point-in-time query BEFORE the
// allocation still returns the old value (history not rewritten), idempotent
// replay, and that the allocation row's economic fields cannot be changed.
func TestPgStore_AllocateLandedCost_SplitHistoryAndImmutability(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	itemID := newValuedTestItem(t, s, ctx, tenantID, legalEntityID, "SKU-LC-1", domain.ValuationMethodFIFO)
	locID := newActiveTestLocation(t, s, ctx, tenantID, legalEntityID, "WH-LC-1")

	now := time.Now().UTC()
	receipt := createCommittedReceipt(t, s, ctx, legalEntityID, itemID, locID, "idem-lc-db-r", 10)
	cost := 2.0
	if _, _, err := s.ValueMovement(ctx, receipt.MovementID, "preparer-1", &cost, now.Add(-3*time.Hour)); err != nil {
		t.Fatalf("value receipt failed: %v", err)
	}
	issue := createCommittedIssue(t, s, ctx, legalEntityID, itemID, locID, "idem-lc-db-i", 4)
	if _, _, err := s.ValueMovement(ctx, issue.MovementID, "preparer-1", nil, now.Add(-2*time.Hour)); err != nil {
		t.Fatalf("value issue failed: %v", err)
	}

	allocatedAt := now.Add(-1 * time.Hour)
	a := newLandedCost(receipt.MovementID, "lc-db-1", 12, allocatedAt)
	created, err := s.AllocateLandedCost(ctx, a)
	if err != nil || !created {
		t.Fatalf("AllocateLandedCost failed: created=%v err=%v", created, err)
	}
	// 6 of 10 units remain -> 7.20 inventory, 4.80 COGS true-up.
	if a.InventoryShare != 7.2 || a.COGSShare != 4.8 || a.RemainingQuantityAtAllocation != 6 || a.Status != domain.LandedCostStatusPendingPosting {
		t.Fatalf("unexpected split: %+v", a)
	}

	live, err := s.GetInventoryValue(ctx, itemID, locID)
	if err != nil || math.Abs(live-19.2) > 0.0001 {
		t.Fatalf("expected live value 19.20 (12.00 + 7.20), got %v (err %v)", live, err)
	}
	before, err := s.GetInventoryValueAsOf(ctx, itemID, locID, allocatedAt.Add(-30*time.Minute))
	if err != nil || math.Abs(before-12) > 0.0001 {
		t.Fatalf("as-of BEFORE the allocation must still be 12.00 (history not rewritten), got %v (err %v)", before, err)
	}
	after, err := s.GetInventoryValueAsOf(ctx, itemID, locID, now)
	if err != nil || math.Abs(after-19.2) > 0.0001 {
		t.Fatalf("as-of AFTER the allocation must be 19.20, got %v (err %v)", after, err)
	}

	// Replay with the same key returns the original and applies nothing more.
	replay := newLandedCost(receipt.MovementID, "lc-db-1", 12, allocatedAt)
	if created, err := s.AllocateLandedCost(ctx, replay); err != nil || created || replay.AllocationID != a.AllocationID {
		t.Fatalf("expected idempotent replay of the original, got created=%v id=%s err=%v", created, replay.AllocationID, err)
	}
	if v, _ := s.GetInventoryValue(ctx, itemID, locID); math.Abs(v-19.2) > 0.0001 {
		t.Fatalf("replay must not apply the cost again, value=%v", v)
	}
	if _, err := s.AllocateLandedCost(ctx, newLandedCost(receipt.MovementID, "lc-db-1", 99, allocatedAt)); err != domain.ErrLandedCostKeyConflict {
		t.Fatalf("expected ErrLandedCostKeyConflict, got %v", err)
	}

	// Immutability: economic fields locked; the posting link may advance once.
	if _, err := pool.Exec(context.Background(), `UPDATE inventory_landed_cost_allocations SET valuation_evidence_ref = 'EDITED' WHERE allocation_id = $1`, a.AllocationID); err == nil {
		t.Fatalf("expected the trigger to refuse changing the evidence reference")
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM inventory_landed_cost_allocations WHERE allocation_id = $1`, a.AllocationID); err == nil {
		t.Fatalf("expected the trigger to refuse DELETE")
	}
	if err := s.MarkLandedCostEmitted(ctx, a.AllocationID, "jrnl-1", now); err != nil {
		t.Fatalf("MarkLandedCostEmitted failed: %v", err)
	}
	if err := s.MarkLandedCostEmitted(ctx, a.AllocationID, "jrnl-2", now); err == nil {
		t.Fatalf("a second emit must be refused")
	}
	got, err := s.GetLandedCostAllocation(ctx, a.AllocationID)
	if err != nil || got.Status != domain.LandedCostStatusAccountingEventEmitted || got.JournalID == nil || *got.JournalID != "jrnl-1" {
		t.Fatalf("expected EMITTED with jrnl-1, got %+v (err %v)", got, err)
	}
}

func TestPgStore_AllocateLandedCost_FullyConsumed_AllToCOGS(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	itemID := newValuedTestItem(t, s, ctx, tenantID, legalEntityID, "SKU-LC-2", domain.ValuationMethodFIFO)
	locID := newActiveTestLocation(t, s, ctx, tenantID, legalEntityID, "WH-LC-2")

	now := time.Now().UTC()
	receipt := createCommittedReceipt(t, s, ctx, legalEntityID, itemID, locID, "idem-lc-db2-r", 5)
	cost := 3.0
	if _, _, err := s.ValueMovement(ctx, receipt.MovementID, "preparer-1", &cost, now.Add(-2*time.Hour)); err != nil {
		t.Fatalf("value receipt failed: %v", err)
	}
	issue := createCommittedIssue(t, s, ctx, legalEntityID, itemID, locID, "idem-lc-db2-i", 5)
	if _, _, err := s.ValueMovement(ctx, issue.MovementID, "preparer-1", nil, now.Add(-1*time.Hour)); err != nil {
		t.Fatalf("value issue failed: %v", err)
	}

	a := newLandedCost(receipt.MovementID, "lc-db-2", 10, now)
	if _, err := s.AllocateLandedCost(ctx, a); err != nil {
		t.Fatalf("AllocateLandedCost failed: %v", err)
	}
	if a.InventoryShare != 0 || a.COGSShare != 10 || a.UnitUplift != 0 {
		t.Fatalf("a fully consumed layer must send the whole amount to COGS, got %+v", a)
	}
	if v, _ := s.GetInventoryValue(ctx, itemID, locID); v != 0 {
		t.Fatalf("no units on hand -> value must stay 0, got %v", v)
	}
}

func TestPgStore_AllocateLandedCost_RefusalsLeaveNothingBehind(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	itemID := newValuedTestItem(t, s, ctx, tenantID, legalEntityID, "SKU-LC-3", domain.ValuationMethodFIFO)
	locID := newActiveTestLocation(t, s, ctx, tenantID, legalEntityID, "WH-LC-3")
	unvalued := createCommittedReceipt(t, s, ctx, legalEntityID, itemID, locID, "idem-lc-db3-r", 5)

	if _, err := s.AllocateLandedCost(ctx, newLandedCost(unvalued.MovementID, "lc-db-3", 5, time.Now().UTC())); err != domain.ErrNoCostLayerForMovement {
		t.Fatalf("expected ErrNoCostLayerForMovement for an unvalued receipt, got %v", err)
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM inventory_landed_cost_allocations WHERE tenant_id = $1`, tenantID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a refused allocation must leave no row, got %d (err %v)", n, err)
	}
}
