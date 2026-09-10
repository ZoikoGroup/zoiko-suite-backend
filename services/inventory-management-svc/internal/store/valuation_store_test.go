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

func newValuedTestItem(t *testing.T, s *store.PgStore, ctx context.Context, tenantID, legalEntityID, sku, method string) string {
	t.Helper()
	itemID := newActiveTestItem(t, s, ctx, tenantID, legalEntityID, sku)
	vp := &domain.ValuationPolicy{
		PolicyVersionID: uuid.New().String(), ItemID: itemID, ValuationMethod: method,
		EffectiveFrom: time.Now().UTC().Add(-time.Minute), CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
	if err := s.SetValuationPolicy(ctx, vp); err != nil {
		t.Fatalf("SetValuationPolicy failed: %v", err)
	}
	return itemID
}

func createCommittedReceipt(t *testing.T, s *store.PgStore, ctx context.Context, legalEntityID, itemID, locID, idemKey string, quantity float64) *domain.InventoryMovement {
	t.Helper()
	m := newDraftReceiptForEntity(legalEntityID, itemID, locID, idemKey, quantity, "")
	if err := s.CreateMovement(ctx, m); err != nil {
		t.Fatalf("CreateMovement failed: %v", err)
	}
	now := time.Now().UTC()
	if err := s.ValidateMovement(ctx, m.MovementID, now); err != nil {
		t.Fatalf("ValidateMovement failed: %v", err)
	}
	if err := s.CommitMovement(ctx, m.MovementID, "preparer-1", now); err != nil {
		t.Fatalf("CommitMovement failed: %v", err)
	}
	return m
}

func createCommittedIssue(t *testing.T, s *store.PgStore, ctx context.Context, legalEntityID, itemID, locID, idemKey string, quantity float64) *domain.InventoryMovement {
	t.Helper()
	m := &domain.InventoryMovement{
		MovementID: uuid.New().String(), LegalEntityID: legalEntityID, MovementType: domain.MovementTypeIssue, Status: domain.MovementStatusDraft,
		ItemID: itemID, SourceLocationID: &locID, Quantity: quantity, UOM: "EACH",
		SourceReference: "SO-1", SourceIdempotencyKey: idemKey, BusinessDate: time.Now().UTC(), FiscalPeriod: "2026-09",
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
	if err := s.CreateMovement(ctx, m); err != nil {
		t.Fatalf("CreateMovement (issue) failed: %v", err)
	}
	now := time.Now().UTC()
	if err := s.ValidateMovement(ctx, m.MovementID, now); err != nil {
		t.Fatalf("ValidateMovement (issue) failed: %v", err)
	}
	if err := s.CommitMovement(ctx, m.MovementID, "preparer-1", now); err != nil {
		t.Fatalf("CommitMovement (issue) failed: %v", err)
	}
	return m
}

func TestPgStore_ValueMovement_Inbound_CreatesCostLayer(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	itemID := newValuedTestItem(t, s, ctx, tenantID, legalEntityID, "SKU-VAL-1", domain.ValuationMethodFIFO)
	locID := newActiveTestLocation(t, s, ctx, tenantID, legalEntityID, "WH-VAL-1")

	receipt := createCommittedReceipt(t, s, ctx, legalEntityID, itemID, locID, "idem-store-val-1", 10)
	unitCost := 5.0
	entry, err := s.ValueMovement(ctx, receipt.MovementID, "preparer-1", &unitCost, time.Now().UTC())
	if err != nil {
		t.Fatalf("ValueMovement failed: %v", err)
	}
	if entry.Value != 50 || entry.EntryType != domain.ValuationEntryTypeInbound {
		t.Fatalf("expected value=50 INBOUND, got %+v", entry)
	}

	value, err := s.GetInventoryValue(ctx, itemID, locID)
	if err != nil {
		t.Fatalf("GetInventoryValue failed: %v", err)
	}
	if value != 50 {
		t.Fatalf("expected inventory value=50, got %v", value)
	}
}

// TestPgStore_ValueMovement_SameMovementTwice_Refused is the real proof
// of negative path #4, "Same movement consumes two cost layers twice."
func TestPgStore_ValueMovement_SameMovementTwice_Refused(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	itemID := newValuedTestItem(t, s, ctx, tenantID, legalEntityID, "SKU-VAL-2", domain.ValuationMethodFIFO)
	locID := newActiveTestLocation(t, s, ctx, tenantID, legalEntityID, "WH-VAL-2")

	receipt := createCommittedReceipt(t, s, ctx, legalEntityID, itemID, locID, "idem-store-val-2", 10)
	unitCost := 3.0
	if _, err := s.ValueMovement(ctx, receipt.MovementID, "preparer-1", &unitCost, time.Now().UTC()); err != nil {
		t.Fatalf("first ValueMovement failed: %v", err)
	}
	if _, err := s.ValueMovement(ctx, receipt.MovementID, "preparer-1", &unitCost, time.Now().UTC()); err != domain.ErrMovementAlreadyValued {
		t.Fatalf("expected ErrMovementAlreadyValued, got %v", err)
	}
}

// TestPgStore_ValueMovement_OutboundFIFO_ConsumesOldestFirst proves the
// real FIFO consumption order and value calculation against actual
// Postgres, including the ORDER BY created_at ASC ... FOR UPDATE query.
func TestPgStore_ValueMovement_OutboundFIFO_ConsumesOldestFirst(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	itemID := newValuedTestItem(t, s, ctx, tenantID, legalEntityID, "SKU-VAL-3", domain.ValuationMethodFIFO)
	locID := newActiveTestLocation(t, s, ctx, tenantID, legalEntityID, "WH-VAL-3")

	r1 := createCommittedReceipt(t, s, ctx, legalEntityID, itemID, locID, "idem-fifo-r1", 10)
	cost1 := 2.0
	if _, err := s.ValueMovement(ctx, r1.MovementID, "preparer-1", &cost1, time.Now().UTC()); err != nil {
		t.Fatalf("value r1 failed: %v", err)
	}
	r2 := createCommittedReceipt(t, s, ctx, legalEntityID, itemID, locID, "idem-fifo-r2", 10)
	cost2 := 4.0
	if _, err := s.ValueMovement(ctx, r2.MovementID, "preparer-1", &cost2, time.Now().UTC()); err != nil {
		t.Fatalf("value r2 failed: %v", err)
	}

	issue := createCommittedIssue(t, s, ctx, legalEntityID, itemID, locID, "idem-fifo-issue", 15)
	entry, err := s.ValueMovement(ctx, issue.MovementID, "preparer-1", nil, time.Now().UTC())
	if err != nil {
		t.Fatalf("value issue failed: %v", err)
	}
	if entry.Value != 40 { // 10*2 + 5*4
		t.Fatalf("expected FIFO value=40, got %v", entry.Value)
	}

	remaining, err := s.GetCostLayers(ctx, itemID, locID)
	if err != nil {
		t.Fatalf("GetCostLayers failed: %v", err)
	}
	var totalRemaining float64
	for _, l := range remaining {
		totalRemaining += l.RemainingQuantity
	}
	if totalRemaining != 5 { // 20 received - 15 issued
		t.Fatalf("expected 5 units remaining across layers, got %v", totalRemaining)
	}
}

// TestPgStore_ValueMovement_InsufficientLayers_Refused is a real
// integrity guard distinct from INV-03's own physical negative-stock
// check.
func TestPgStore_ValueMovement_InsufficientLayers_Refused(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	itemID := newValuedTestItem(t, s, ctx, tenantID, legalEntityID, "SKU-VAL-4", domain.ValuationMethodFIFO)
	locID := newActiveTestLocation(t, s, ctx, tenantID, legalEntityID, "WH-VAL-4")

	createCommittedReceipt(t, s, ctx, legalEntityID, itemID, locID, "idem-insuf-r1", 5) // never valued — no cost layer

	issue := createCommittedIssue(t, s, ctx, legalEntityID, itemID, locID, "idem-insuf-issue", 5)
	if _, err := s.ValueMovement(ctx, issue.MovementID, "preparer-1", nil, time.Now().UTC()); err != domain.ErrInsufficientCostLayers {
		t.Fatalf("expected ErrInsufficientCostLayers, got %v", err)
	}
}

// TestPgStore_ValuationRun_FullLifecycle exercises CreateValuationRun's
// own population-freeze and MarkValuationRunEmitted end to end.
func TestPgStore_ValuationRun_FullLifecycle(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	itemID := newValuedTestItem(t, s, ctx, tenantID, legalEntityID, "SKU-VAL-5", domain.ValuationMethodFIFO)
	locID := newActiveTestLocation(t, s, ctx, tenantID, legalEntityID, "WH-VAL-5")

	receipt := createCommittedReceipt(t, s, ctx, legalEntityID, itemID, locID, "idem-run-r1", 10)
	cost := 2.0
	if _, err := s.ValueMovement(ctx, receipt.MovementID, "preparer-1", &cost, time.Now().UTC()); err != nil {
		t.Fatalf("value receipt failed: %v", err)
	}
	issue := createCommittedIssue(t, s, ctx, legalEntityID, itemID, locID, "idem-run-issue", 4)
	if _, err := s.ValueMovement(ctx, issue.MovementID, "preparer-1", nil, time.Now().UTC()); err != nil {
		t.Fatalf("value issue failed: %v", err)
	}

	run := &domain.ValuationRun{
		RunID: uuid.New().String(), LegalEntityID: legalEntityID, FiscalPeriod: "2026-09",
		InventoryAccountCode: "INV-ASSET", COGSAccountCode: "COGS", CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
	frozen, err := s.CreateValuationRun(ctx, run)
	if err != nil {
		t.Fatalf("CreateValuationRun failed: %v", err)
	}
	if frozen != 2 {
		t.Fatalf("expected 2 frozen entries (1 inbound + 1 outbound), got %d", frozen)
	}

	if err := s.MarkValuationRunEmitted(ctx, run.RunID, "approver-2", "jrnl-1", time.Now().UTC()); err != nil {
		t.Fatalf("MarkValuationRunEmitted failed: %v", err)
	}
	got, err := s.GetValuationRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("GetValuationRun failed: %v", err)
	}
	if got.Status != domain.ValuationRunStatusAccountingEventEmitted || len(got.Entries) != 2 {
		t.Fatalf("expected ACCOUNTING_EVENT_EMITTED with 2 entries, got status=%q entries=%d", got.Status, len(got.Entries))
	}
}

// TestPgStore_WriteDown_CreateAndReverse is the real proof that a
// write-down's own journal reference and status round-trip correctly.
func TestPgStore_WriteDown_CreateAndReverse(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	itemID := newValuedTestItem(t, s, ctx, tenantID, legalEntityID, "SKU-VAL-6", domain.ValuationMethodFIFO)
	locID := newActiveTestLocation(t, s, ctx, tenantID, legalEntityID, "WH-VAL-6")

	wd := &domain.WriteDown{
		WriteDownID: uuid.New().String(), LegalEntityID: legalEntityID, ItemID: itemID, LocationID: locID,
		Amount: 100, ValuationEvidenceRef: "NRV-1", ExpenseAccountCode: "WD-EXP", InventoryAccountCode: "INV-ASSET",
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
	if err := s.CreateWriteDown(ctx, wd, "jrnl-wd-1"); err != nil {
		t.Fatalf("CreateWriteDown failed: %v", err)
	}
	got, err := s.GetWriteDown(ctx, wd.WriteDownID)
	if err != nil {
		t.Fatalf("GetWriteDown failed: %v", err)
	}
	if got.Status != domain.WriteDownStatusAccountingEventEmitted || got.JournalID == nil || *got.JournalID != "jrnl-wd-1" {
		t.Fatalf("unexpected write-down: %+v", got)
	}

	if err := s.ReverseWriteDown(ctx, wd.WriteDownID, "reviewer-2", "recovered value", time.Now().UTC()); err != nil {
		t.Fatalf("ReverseWriteDown failed: %v", err)
	}
	if err := s.ReverseWriteDown(ctx, wd.WriteDownID, "reviewer-3", "again", time.Now().UTC()); err != domain.ErrWriteDownAlreadyReversed {
		t.Fatalf("expected ErrWriteDownAlreadyReversed, got %v", err)
	}
}

// TestPgStore_GetInventoryValueTotal_RealDB proves the real aggregation
// query — the source financial-close-svc's ACC-06 reconciles against for
// the AST/INV/PRJ domain spec's own §9 "Inventory value → GL" assertion.
func TestPgStore_GetInventoryValueTotal_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	locID := newActiveTestLocation(t, s, ctx, tenantID, legalEntityID, "WH-VAL-TOTAL-1")

	itemA := newValuedTestItem(t, s, ctx, tenantID, legalEntityID, "SKU-VAL-TOTAL-A", domain.ValuationMethodFIFO)
	receiptA := createCommittedReceipt(t, s, ctx, legalEntityID, itemA, locID, "idem-store-val-total-1", 10)
	unitCostA := 5.0
	if _, err := s.ValueMovement(ctx, receiptA.MovementID, "preparer-1", &unitCostA, time.Now().UTC()); err != nil {
		t.Fatalf("ValueMovement (item A) failed: %v", err)
	}

	itemB := newValuedTestItem(t, s, ctx, tenantID, legalEntityID, "SKU-VAL-TOTAL-B", domain.ValuationMethodFIFO)
	receiptB := createCommittedReceipt(t, s, ctx, legalEntityID, itemB, locID, "idem-store-val-total-2", 4)
	unitCostB := 25.0
	if _, err := s.ValueMovement(ctx, receiptB.MovementID, "preparer-1", &unitCostB, time.Now().UTC()); err != nil {
		t.Fatalf("ValueMovement (item B) failed: %v", err)
	}

	total, err := s.GetInventoryValueTotal(ctx, legalEntityID)
	if err != nil {
		t.Fatalf("GetInventoryValueTotal failed: %v", err)
	}
	if total != 150 {
		t.Fatalf("expected 150 (10*5 + 4*25 across two items), got %v", total)
	}

	// A DIFFERENT legal entity must never contribute.
	otherEntityTotal, err := s.GetInventoryValueTotal(ctx, uuid.New().String())
	if err != nil {
		t.Fatalf("GetInventoryValueTotal (other entity) failed: %v", err)
	}
	if otherEntityTotal != 0 {
		t.Fatalf("expected 0 for an unrelated legal_entity_id, got %v", otherEntityTotal)
	}
}
