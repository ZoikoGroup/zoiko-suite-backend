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

// TestPgStore_ReceiptLink_IdempotentConflictAndImmutable proves, against real
// Postgres: re-linking the same (movement, receipt) is a no-op, a different
// receipt is refused, and the link row can never be updated or deleted.
func TestPgStore_ReceiptLink_IdempotentConflictAndImmutable(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	itemID := newActiveTestItem(t, s, ctx, tenantID, legalEntityID, "SKU-LINK-1")
	locID := newActiveTestLocation(t, s, ctx, tenantID, legalEntityID, "WH-LINK-1")
	receipt := createCommittedReceipt(t, s, ctx, legalEntityID, itemID, locID, "idem-link-1", 10)

	now := time.Now().UTC()
	if err := s.LinkMovementReceipt(ctx, receipt.MovementID, "rcpt-A", "preparer-1", now); err != nil {
		t.Fatalf("first link failed: %v", err)
	}
	if err := s.LinkMovementReceipt(ctx, receipt.MovementID, "rcpt-A", "preparer-1", now); err != nil {
		t.Fatalf("idempotent re-link should succeed, got %v", err)
	}
	if err := s.LinkMovementReceipt(ctx, receipt.MovementID, "rcpt-B", "preparer-1", now); err != domain.ErrReceiptLinkConflict {
		t.Fatalf("expected ErrReceiptLinkConflict for a different receipt, got %v", err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE inventory_movement_receipt_links SET ap_receipt_id = 'x' WHERE movement_id = $1`, receipt.MovementID); err == nil {
		t.Fatalf("expected the immutability trigger to refuse an UPDATE")
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM inventory_movement_receipt_links WHERE movement_id = $1`, receipt.MovementID); err == nil {
		t.Fatalf("expected the immutability trigger to refuse a DELETE")
	}
}

// TestPgStore_GetRunInboundValue_SplitsLinkedAndUnlinked proves the posting
// base: only INBOUND value whose movement is AP-linked is reclass-eligible;
// the rest is reported as unlinked, never silently posted.
func TestPgStore_GetRunInboundValue_SplitsLinkedAndUnlinked(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	itemID := newValuedTestItem(t, s, ctx, tenantID, legalEntityID, "SKU-LINK-2", domain.ValuationMethodFIFO)
	locID := newActiveTestLocation(t, s, ctx, tenantID, legalEntityID, "WH-LINK-2")

	linked := createCommittedReceipt(t, s, ctx, legalEntityID, itemID, locID, "idem-link-2a", 10)
	unlinked := createCommittedReceipt(t, s, ctx, legalEntityID, itemID, locID, "idem-link-2b", 5)
	now := time.Now().UTC()
	if err := s.LinkMovementReceipt(ctx, linked.MovementID, "rcpt-100", "preparer-1", now); err != nil {
		t.Fatalf("link failed: %v", err)
	}
	costA, costB := 3.0, 4.0
	if _, _, err := s.ValueMovement(ctx, linked.MovementID, "preparer-1", &costA, now); err != nil { // 30
		t.Fatalf("value linked failed: %v", err)
	}
	if _, _, err := s.ValueMovement(ctx, unlinked.MovementID, "preparer-1", &costB, now); err != nil { // 20
		t.Fatalf("value unlinked failed: %v", err)
	}

	run := &domain.ValuationRun{
		RunID: uuid.New().String(), LegalEntityID: legalEntityID, FiscalPeriod: linked.FiscalPeriod,
		InventoryAccountCode: "INV-ASSET", COGSAccountCode: "COGS",
		CreatedAt: now, CreatedByPrincipalID: "preparer-1",
	}
	if frozen, err := s.CreateValuationRun(ctx, run); err != nil || frozen != 2 {
		t.Fatalf("expected 2 frozen entries, got %d (err %v)", frozen, err)
	}

	got, err := s.GetRunInboundValue(ctx, run.RunID)
	if err != nil {
		t.Fatalf("GetRunInboundValue failed: %v", err)
	}
	if got.ReceiptLinked != 30 || got.Unlinked != 20 {
		t.Fatalf("expected linked=30 unlinked=20, got %+v", got)
	}
}
