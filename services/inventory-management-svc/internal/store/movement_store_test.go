package store_test

import (
	"context"
	"sync"
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

// TestPgStore_GetLocationInventorySummary_NetsAcrossItems is the real proof
// of INV-02's own GetLocationInventorySummary query — that it aggregates
// net on-hand per item at a location the same way liveOnHand aggregates a
// single item, and that a fully-consumed item (net zero) drops out.
func TestPgStore_GetLocationInventorySummary_NetsAcrossItems(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	itemA := newActiveTestItem(t, s, ctx, tenantID, legalEntityID, "SKU-SUM-A")
	itemB := newActiveTestItem(t, s, ctx, tenantID, legalEntityID, "SKU-SUM-B")
	itemC := newActiveTestItem(t, s, ctx, tenantID, legalEntityID, "SKU-SUM-C")
	locID := newActiveTestLocation(t, s, ctx, tenantID, legalEntityID, "WH-SUM-1")

	now := time.Now().UTC()
	commitReceipt := func(itemID, idemKey string, qty float64) {
		m := newDraftReceipt(itemID, locID, idemKey, qty, "")
		if err := s.CreateMovement(ctx, m); err != nil {
			t.Fatalf("CreateMovement failed: %v", err)
		}
		if err := s.ValidateMovement(ctx, m.MovementID, now); err != nil {
			t.Fatalf("ValidateMovement failed: %v", err)
		}
		if err := s.CommitMovement(ctx, m.MovementID, "preparer-1", now); err != nil {
			t.Fatalf("CommitMovement failed: %v", err)
		}
	}
	commitReceipt(itemA, "idem-sum-a1", 12)
	commitReceipt(itemB, "idem-sum-b1", 8)
	commitReceipt(itemC, "idem-sum-c1", 5)

	// Fully consume item C so it nets to zero and must not appear.
	issue := &domain.InventoryMovement{
		MovementID: uuid.New().String(), MovementType: domain.MovementTypeIssue, Status: domain.MovementStatusDraft,
		ItemID: itemC, SourceLocationID: &locID, Quantity: 5, UOM: "EACH",
		SourceReference: "SO-1", SourceIdempotencyKey: "idem-sum-c-issue", BusinessDate: now, FiscalPeriod: "2026-09",
		CreatedAt: now, CreatedByPrincipalID: "preparer-1",
	}
	if err := s.CreateMovement(ctx, issue); err != nil {
		t.Fatalf("CreateMovement (issue) failed: %v", err)
	}
	if err := s.ValidateMovement(ctx, issue.MovementID, now); err != nil {
		t.Fatalf("ValidateMovement (issue) failed: %v", err)
	}
	if err := s.CommitMovement(ctx, issue.MovementID, "preparer-1", now); err != nil {
		t.Fatalf("CommitMovement (issue) failed: %v", err)
	}

	lines, err := s.GetLocationInventorySummary(ctx, locID)
	if err != nil {
		t.Fatalf("GetLocationInventorySummary failed: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("expected 2 items with non-zero on-hand (C nets to zero), got %+v", lines)
	}
	byItem := map[string]float64{}
	for _, l := range lines {
		byItem[l.ItemID] = l.OnHandQuantity
	}
	if byItem[itemA] != 12 || byItem[itemB] != 8 {
		t.Fatalf("expected A=12 B=8, got %+v", byItem)
	}
	if _, present := byItem[itemC]; present {
		t.Fatalf("expected item C (net zero) to be absent, got %+v", byItem)
	}
}

// TestPgStore_MovementLineageAndChain_CorrectionOfACorrection is the real
// recursive-CTE proof behind INV-03's own GetMovementLineage/GetMovementChain
// queries: a chain of length 3 (original -> reversal -> supersession of
// the reversal) is reachable end-to-end, lineage is forward-only from any
// point, and chain finds the same 3 movements regardless of which one you
// start from.
func TestPgStore_MovementLineageAndChain_CorrectionOfACorrection(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	itemID := newActiveTestItem(t, s, ctx, tenantID, legalEntityID, "SKU-CHAIN-1")
	locID := newActiveTestLocation(t, s, ctx, tenantID, legalEntityID, "WH-CHAIN-1")

	m := newDraftReceipt(itemID, locID, "idem-chain-1", 20, "")
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

	reversal, err := s.CreateCorrectionMovement(ctx, m.MovementID, "preparer-2", "wrong quantity", false, uuid.New().String(), time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateCorrectionMovement (reversal) failed: %v", err)
	}
	supersession, err := s.CreateCorrectionMovement(ctx, reversal.MovementID, "preparer-3", "correcting the reversal itself", true, uuid.New().String(), time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateCorrectionMovement (supersession) failed: %v", err)
	}

	lineage, err := s.GetMovementLineage(ctx, m.MovementID)
	if err != nil {
		t.Fatalf("GetMovementLineage failed: %v", err)
	}
	if len(lineage) != 3 {
		t.Fatalf("expected lineage of 3 from the original, got %d: %+v", len(lineage), lineage)
	}

	leafLineage, err := s.GetMovementLineage(ctx, supersession.MovementID)
	if err != nil {
		t.Fatalf("GetMovementLineage (leaf) failed: %v", err)
	}
	if len(leafLineage) != 1 {
		t.Fatalf("expected lineage of 1 from the leaf (forward-only), got %d: %+v", len(leafLineage), leafLineage)
	}

	chain, err := s.GetMovementChain(ctx, supersession.MovementID)
	if err != nil {
		t.Fatalf("GetMovementChain failed: %v", err)
	}
	if len(chain) != 3 {
		t.Fatalf("expected chain of 3 starting from the leaf, got %d: %+v", len(chain), chain)
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

// TestPgStore_NegativeStockPolicy_AllowedCommitsAndCarriesForward proves the
// INV-01 policy against real Postgres: ALLOWED lets an over-issue commit
// (on-hand goes to -5), and a later policy version that omits the field
// carries ALLOWED forward instead of silently resetting to PROHIBITED.
func TestPgStore_NegativeStockPolicy_AllowedCommitsAndCarriesForward(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	itemID := newActiveTestItem(t, s, ctx, tenantID, legalEntityID, "SKU-MV-NEGPOL")
	locID := newActiveTestLocation(t, s, ctx, tenantID, legalEntityID, "WH-MV-NEGPOL")
	createCommittedReceipt(t, s, ctx, legalEntityID, itemID, locID, "idem-negpol-r", 5)

	now := time.Now().UTC()
	setPolicy := func(neg string) *domain.TrackingPolicy {
		p := &domain.TrackingPolicy{
			PolicyVersionID: uuid.NewString(), ItemID: itemID, NegativeStockPolicy: neg,
			EffectiveFrom: now, CreatedAt: now, CreatedByPrincipalID: "policy-1",
		}
		if err := s.SetTrackingPolicy(ctx, p, now); err != nil {
			t.Fatalf("SetTrackingPolicy failed: %v", err)
		}
		return p
	}
	issue := func(key string) error {
		m := &domain.InventoryMovement{
			MovementID: uuid.New().String(), LegalEntityID: legalEntityID, MovementType: domain.MovementTypeIssue, Status: domain.MovementStatusDraft,
			ItemID: itemID, SourceLocationID: &locID, Quantity: 10, UOM: "EACH",
			SourceReference: "SO-NEG", SourceIdempotencyKey: key, BusinessDate: now, FiscalPeriod: "2026-09",
			CreatedAt: now, CreatedByPrincipalID: "preparer-1",
		}
		if err := s.CreateMovement(ctx, m); err != nil {
			t.Fatalf("CreateMovement failed: %v", err)
		}
		if err := s.ValidateMovement(ctx, m.MovementID, now); err != nil {
			t.Fatalf("ValidateMovement failed: %v", err)
		}
		return s.CommitMovement(ctx, m.MovementID, "preparer-1", now)
	}

	if p := setPolicy(""); p.NegativeStockPolicy != domain.NegativeStockProhibited {
		t.Fatalf("expected default PROHIBITED, got %q", p.NegativeStockPolicy)
	}
	if err := issue("idem-negpol-1"); err != domain.ErrNegativeStockNotAllowed {
		t.Fatalf("expected ErrNegativeStockNotAllowed under PROHIBITED, got %v", err)
	}
	setPolicy(domain.NegativeStockAllowed)
	if err := issue("idem-negpol-2"); err != nil {
		t.Fatalf("expected over-issue to commit under ALLOWED, got %v", err)
	}
	if onHand, err := s.GetOnHand(ctx, itemID, locID); err != nil || onHand != -5 {
		t.Fatalf("expected on-hand -5, got %v (err %v)", onHand, err)
	}
	if p := setPolicy(""); p.NegativeStockPolicy != domain.NegativeStockAllowed {
		t.Fatalf("expected ALLOWED carried forward, got %q", p.NegativeStockPolicy)
	}
}

// TestPgStore_CommitMovement_BlocksOnItemLocationLock is the deterministic
// proof of the per-(item, location) lock: while another transaction holds the
// advisory lock for that key, CommitMovement of an issue from that location
// must wait; it proceeds once the lock is released. (The race itself is too
// narrow to hit reliably, so the concurrent test below is only a smoke test.)
func TestPgStore_CommitMovement_BlocksOnItemLocationLock(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	itemID := newActiveTestItem(t, s, ctx, tenantID, legalEntityID, "SKU-MV-LOCK")
	locID := newActiveTestLocation(t, s, ctx, tenantID, legalEntityID, "WH-MV-LOCK")
	createCommittedReceipt(t, s, ctx, legalEntityID, itemID, locID, "idem-lock-r", 10)

	now := time.Now().UTC()
	issue := &domain.InventoryMovement{
		MovementID: uuid.New().String(), LegalEntityID: legalEntityID, MovementType: domain.MovementTypeIssue, Status: domain.MovementStatusDraft,
		ItemID: itemID, SourceLocationID: &locID, Quantity: 5, UOM: "EACH",
		SourceReference: "SO-LOCK", SourceIdempotencyKey: "idem-lock-i", BusinessDate: now, FiscalPeriod: "2026-09",
		CreatedAt: now, CreatedByPrincipalID: "preparer-1",
	}
	if err := s.CreateMovement(ctx, issue); err != nil {
		t.Fatalf("CreateMovement failed: %v", err)
	}
	if err := s.ValidateMovement(ctx, issue.MovementID, now); err != nil {
		t.Fatalf("ValidateMovement failed: %v", err)
	}

	// Hold the lock in a separate transaction.
	holder, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin holder tx failed: %v", err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	if _, err := holder.Exec(context.Background(), `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		tenantID+"|"+itemID+"|"+locID); err != nil {
		t.Fatalf("acquiring holder lock failed: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- s.CommitMovement(ctx, issue.MovementID, "preparer-1", time.Now().UTC()) }()

	select {
	case err := <-done:
		t.Fatalf("CommitMovement finished while the item/location lock was held (err=%v) — it is not taking the lock", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := holder.Rollback(context.Background()); err != nil {
		t.Fatalf("releasing holder lock failed: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected CommitMovement to succeed once the lock was released, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("CommitMovement did not proceed after the lock was released")
	}
}

// TestPgStore_CommitMovement_ConcurrentIssues_OnlyOneSucceeds proves the
// per-(item, location) lock: stock of 10, two concurrent issues of 10 —
// exactly one may commit, the other must get ErrNegativeStockNotAllowed.
func TestPgStore_CommitMovement_ConcurrentIssues_OnlyOneSucceeds(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	itemID := newActiveTestItem(t, s, ctx, tenantID, legalEntityID, "SKU-MV-CONC")
	locID := newActiveTestLocation(t, s, ctx, tenantID, legalEntityID, "WH-MV-CONC")
	createCommittedReceipt(t, s, ctx, legalEntityID, itemID, locID, "idem-conc-r", 10)

	const n = 8
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		issue := &domain.InventoryMovement{
			MovementID: uuid.New().String(), LegalEntityID: legalEntityID, MovementType: domain.MovementTypeIssue, Status: domain.MovementStatusDraft,
			ItemID: itemID, SourceLocationID: &locID, Quantity: 10, UOM: "EACH",
			SourceReference: "SO-CONC", SourceIdempotencyKey: "idem-conc-" + uuid.NewString(), BusinessDate: time.Now().UTC(), FiscalPeriod: "2026-09",
			CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
		}
		if err := s.CreateMovement(ctx, issue); err != nil {
			t.Fatalf("CreateMovement failed: %v", err)
		}
		if err := s.ValidateMovement(ctx, issue.MovementID, time.Now().UTC()); err != nil {
			t.Fatalf("ValidateMovement failed: %v", err)
		}
		ids[i] = issue.MovementID
	}

	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = s.CommitMovement(ctx, ids[i], "preparer-1", time.Now().UTC())
		}(i)
	}
	close(start)
	wg.Wait()

	ok := 0
	for _, err := range errs {
		switch err {
		case nil:
			ok++
		case domain.ErrNegativeStockNotAllowed:
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("expected exactly 1 of %d concurrent issues to commit, got %d", n, ok)
	}
	onHand, err := s.GetOnHand(ctx, itemID, locID)
	if err != nil || onHand != 0 {
		t.Fatalf("expected on-hand 0, got %v (err %v)", onHand, err)
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
