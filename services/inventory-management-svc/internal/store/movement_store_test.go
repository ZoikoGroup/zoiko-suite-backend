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

func newActiveTestItem(t *testing.T, s *store.PgStore, ctx context.Context, tenantID, legalEntityID, sku string) string {
	t.Helper()
	it := &domain.InventoryItem{
		ItemID: uuid.New().String(), TenantID: tenantID, LegalEntityID: legalEntityID,
		SKU: sku, Description: "Widget", BaseUOM: "EACH",
		Status: domain.ItemStatusDraft, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
	if err := s.CreateItem(ctx, it); err != nil {
		t.Fatalf("CreateItem failed: %v", err)
	}
	if err := s.ActivateItem(ctx, it.ItemID, "approver-1", time.Now().UTC()); err != nil {
		t.Fatalf("ActivateItem failed: %v", err)
	}
	return it.ItemID
}

func newActiveTestLocation(t *testing.T, s *store.PgStore, ctx context.Context, tenantID, legalEntityID, code string) string {
	t.Helper()
	l := &domain.InventoryLocation{
		LocationID: uuid.New().String(), TenantID: tenantID, LegalEntityID: legalEntityID,
		LocationCode: code, LocationType: domain.LocationTypeWarehouse,
		Status: domain.LocationStatusDraft, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
	if err := s.CreateLocation(ctx, l, nil); err != nil {
		t.Fatalf("CreateLocation failed: %v", err)
	}
	if err := s.ActivateLocation(ctx, l.LocationID, "approver-1", time.Now().UTC()); err != nil {
		t.Fatalf("ActivateLocation failed: %v", err)
	}
	return l.LocationID
}

func newDraftReceipt(itemID, destLocationID, idemKey string, quantity float64, serial string) *domain.InventoryMovement {
	return newDraftReceiptForEntity("", itemID, destLocationID, idemKey, quantity, serial)
}

func newDraftReceiptForEntity(legalEntityID, itemID, destLocationID, idemKey string, quantity float64, serial string) *domain.InventoryMovement {
	m := &domain.InventoryMovement{
		MovementID: uuid.New().String(), LegalEntityID: legalEntityID, MovementType: domain.MovementTypeReceipt, Status: domain.MovementStatusDraft,
		ItemID: itemID, DestinationLocationID: &destLocationID, Quantity: quantity, UOM: "EACH",
		SourceReference: "PO-1", SourceIdempotencyKey: idemKey, BusinessDate: time.Now().UTC(), FiscalPeriod: "2026-09",
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
	if serial != "" {
		m.SerialNumber = &serial
	}
	return m
}

func TestPgStore_CreateMovement_DuplicateIdempotencyKey_ReturnsOriginal(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	itemID := newActiveTestItem(t, s, ctx, tenantID, legalEntityID, "SKU-MV-1")
	locID := newActiveTestLocation(t, s, ctx, tenantID, legalEntityID, "WH-MV-1")

	first := newDraftReceipt(itemID, locID, "idem-dup", 10, "")
	if err := s.CreateMovement(ctx, first); err != nil {
		t.Fatalf("first CreateMovement failed: %v", err)
	}
	firstID := first.MovementID

	second := newDraftReceipt(itemID, locID, "idem-dup", 10, "")
	if err := s.CreateMovement(ctx, second); err != nil {
		t.Fatalf("second CreateMovement failed: %v", err)
	}
	if second.MovementID != firstID {
		t.Fatalf("expected the original movement returned for a duplicate idempotency key, got a new one: %q vs %q", second.MovementID, firstID)
	}
}

func TestPgStore_FullReceiptLifecycle_UpdatesOnHand(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	itemID := newActiveTestItem(t, s, ctx, tenantID, legalEntityID, "SKU-MV-2")
	locID := newActiveTestLocation(t, s, ctx, tenantID, legalEntityID, "WH-MV-2")

	m := newDraftReceipt(itemID, locID, "idem-lifecycle", 25, "")
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

	onHand, err := s.GetOnHand(ctx, itemID, locID)
	if err != nil {
		t.Fatalf("GetOnHand failed: %v", err)
	}
	if onHand != 25 {
		t.Fatalf("expected on_hand=25, got %v", onHand)
	}
}

// TestPgStore_CommitMovement_CommittedRowImmutable is the real proof of
// the state model's own claim, "committed movement immutable."
func TestPgStore_CommitMovement_CommittedRowImmutable(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	itemID := newActiveTestItem(t, s, ctx, tenantID, legalEntityID, "SKU-MV-3")
	locID := newActiveTestLocation(t, s, ctx, tenantID, legalEntityID, "WH-MV-3")

	m := newDraftReceipt(itemID, locID, "idem-immutable", 5, "")
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

	// A second commit attempt must fail — it's already COMMITTED, and the
	// guarded UPDATE affects zero rows either way, but this also proves
	// the trigger doesn't block the store's OWN guarded no-op path.
	if err := s.CommitMovement(ctx, m.MovementID, "preparer-1", now); err != domain.ErrInvalidMovementTransition {
		t.Fatalf("expected ErrInvalidMovementTransition re-committing, got %v", err)
	}

	// A raw UPDATE against the committed row must be rejected by the
	// database trigger itself, regardless of application code.
	_, err := pool.Exec(context.Background(), `UPDATE inventory_movements SET source_reference = 'tampered' WHERE movement_id = $1`, m.MovementID)
	if err == nil {
		t.Fatalf("expected the reject-mutation trigger to refuse a raw UPDATE against a COMMITTED movement, got no error")
	}
}

// TestPgStore_CommitMovement_NegativeStockRefused is the real proof of
// negative path #3.
func TestPgStore_CommitMovement_NegativeStockRefused(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	itemID := newActiveTestItem(t, s, ctx, tenantID, legalEntityID, "SKU-MV-4")
	locID := newActiveTestLocation(t, s, ctx, tenantID, legalEntityID, "WH-MV-4")

	issue := &domain.InventoryMovement{
		MovementID: uuid.New().String(), MovementType: domain.MovementTypeIssue, Status: domain.MovementStatusDraft,
		ItemID: itemID, SourceLocationID: &locID, Quantity: 10, UOM: "EACH",
		SourceReference: "SO-1", SourceIdempotencyKey: "idem-negstock", BusinessDate: time.Now().UTC(), FiscalPeriod: "2026-09",
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
	if err := s.CreateMovement(ctx, issue); err != nil {
		t.Fatalf("CreateMovement failed: %v", err)
	}
	now := time.Now().UTC()
	if err := s.ValidateMovement(ctx, issue.MovementID, now); err != nil {
		t.Fatalf("ValidateMovement failed: %v", err)
	}
	if err := s.CommitMovement(ctx, issue.MovementID, "preparer-1", now); err != domain.ErrNegativeStockNotAllowed {
		t.Fatalf("expected ErrNegativeStockNotAllowed, got %v", err)
	}
}

// TestPgStore_SerialResidency_DuplicateReceiptRefused is the real proof
// of negative path #2.
func TestPgStore_SerialResidency_DuplicateReceiptRefused(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	itemID := newActiveTestItem(t, s, ctx, tenantID, legalEntityID, "SKU-MV-5")
	locID := newActiveTestLocation(t, s, ctx, tenantID, legalEntityID, "WH-MV-5")

	first := newDraftReceipt(itemID, locID, "idem-sn-1", 1, "SN-100")
	if err := s.CreateMovement(ctx, first); err != nil {
		t.Fatalf("first CreateMovement failed: %v", err)
	}
	now := time.Now().UTC()
	if err := s.ValidateMovement(ctx, first.MovementID, now); err != nil {
		t.Fatalf("first ValidateMovement failed: %v", err)
	}
	if err := s.CommitMovement(ctx, first.MovementID, "preparer-1", now); err != nil {
		t.Fatalf("first CommitMovement failed: %v", err)
	}

	second := newDraftReceipt(itemID, locID, "idem-sn-2", 1, "SN-100")
	if err := s.CreateMovement(ctx, second); err != nil {
		t.Fatalf("second CreateMovement failed: %v", err)
	}
	if err := s.ValidateMovement(ctx, second.MovementID, now); err != nil {
		t.Fatalf("second ValidateMovement failed: %v", err)
	}
	if err := s.CommitMovement(ctx, second.MovementID, "preparer-1", now); err != domain.ErrSerialAlreadyResident {
		t.Fatalf("expected ErrSerialAlreadyResident, got %v", err)
	}
}

// TestPgStore_CreateCorrectionMovement_ReversalRevertsOnHand proves
// ReverseMovement's own mechanical-inverse construction is correct end
// to end against real Postgres.
func TestPgStore_CreateCorrectionMovement_ReversalRevertsOnHand(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	itemID := newActiveTestItem(t, s, ctx, tenantID, legalEntityID, "SKU-MV-6")
	locID := newActiveTestLocation(t, s, ctx, tenantID, legalEntityID, "WH-MV-6")

	m := newDraftReceipt(itemID, locID, "idem-rev-1", 40, "")
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

	correction, err := s.CreateCorrectionMovement(ctx, m.MovementID, "preparer-2", "wrong quantity", false, uuid.New().String(), time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateCorrectionMovement failed: %v", err)
	}
	if correction.MovementType != domain.MovementTypeReversal || correction.SourceLocationID == nil || *correction.SourceLocationID != locID {
		t.Fatalf("expected a REVERSAL movement shaped like an ISSUE from %s, got %+v", locID, correction)
	}

	onHand, err := s.GetOnHand(ctx, itemID, locID)
	if err != nil {
		t.Fatalf("GetOnHand failed: %v", err)
	}
	if onHand != 0 {
		t.Fatalf("expected on_hand=0 after reversal, got %v", onHand)
	}
}

// TestPgStore_GetNegativeOnHandCount_RealDB proves the real aggregation
// query. A negative combination cannot be produced through this
// service's own CommitMovement (TestPgStore_CommitMovement_NegativeStockRefused
// already proves that refusal) — so a violating COMMITTED row is
// inserted directly, the same way a migration/backfill bypass would,
// exactly the scenario this check exists to catch.
func TestPgStore_GetNegativeOnHandCount_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	itemID := newActiveTestItem(t, s, ctx, tenantID, legalEntityID, "SKU-MV-7")
	locID := newActiveTestLocation(t, s, ctx, tenantID, legalEntityID, "WH-MV-7")

	receipt := newDraftReceiptForEntity(legalEntityID, itemID, locID, "idem-neg-real-1", 5, "")
	if err := s.CreateMovement(ctx, receipt); err != nil {
		t.Fatalf("CreateMovement failed: %v", err)
	}
	now := time.Now().UTC()
	if err := s.ValidateMovement(ctx, receipt.MovementID, now); err != nil {
		t.Fatalf("ValidateMovement failed: %v", err)
	}
	if err := s.CommitMovement(ctx, receipt.MovementID, "preparer-1", now); err != nil {
		t.Fatalf("CommitMovement failed: %v", err)
	}

	count, err := s.GetNegativeOnHandCount(ctx, legalEntityID)
	if err != nil {
		t.Fatalf("GetNegativeOnHandCount (before bypass): %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 before any violation, got %d", count)
	}

	// Bypass the application layer entirely — a raw INSERT of an
	// already-COMMITTED issue that would never survive CommitMovement's
	// own guard.
	if _, err := pool.Exec(ctx, `
		INSERT INTO inventory_movements (
			movement_id, tenant_id, legal_entity_id, movement_type, status, item_id,
			source_location_id, quantity, uom, source_reference, source_idempotency_key,
			business_date, fiscal_period, created_at, created_by_principal_id, committed_at, committed_by_principal_id
		) VALUES ($1, $2, $3, 'ISSUE', 'COMMITTED', $4, $5, 8, 'EACH', 'BYPASS', 'idem-neg-real-2', $6::date, '2026-09', $7::timestamptz, 'preparer-1', $7::timestamptz, 'preparer-1')
	`, uuid.New().String(), tenantID, legalEntityID, itemID, locID, now, now); err != nil {
		t.Fatalf("bypass INSERT failed: %v", err)
	}

	count, err = s.GetNegativeOnHandCount(ctx, legalEntityID)
	if err != nil {
		t.Fatalf("GetNegativeOnHandCount (after bypass): %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 negative combination (5 received, 8 issued via bypass, net -3), got %d", count)
	}
}
