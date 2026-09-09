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

func newTestLocation(tenantID, legalEntityID, code string) *domain.InventoryLocation {
	return &domain.InventoryLocation{
		LocationID: uuid.New().String(), TenantID: tenantID, LegalEntityID: legalEntityID,
		LocationCode: code, LocationType: domain.LocationTypeWarehouse,
		Status: domain.LocationStatusDraft, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
}

func TestPgStore_CreateLocation_And_GetLocation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	l := newTestLocation(tenantID, legalEntityID, "WH-1")
	if err := s.CreateLocation(ctx, l, nil); err != nil {
		t.Fatalf("CreateLocation failed: %v", err)
	}
	got, err := s.GetLocation(ctx, l.LocationID)
	if err != nil {
		t.Fatalf("GetLocation failed: %v", err)
	}
	if got.LocationCode != "WH-1" || got.Status != domain.LocationStatusDraft {
		t.Fatalf("unexpected location: %+v", got)
	}
	parent, err := s.GetCurrentParent(ctx, l.LocationID)
	if err != nil {
		t.Fatalf("GetCurrentParent failed: %v", err)
	}
	if parent != nil {
		t.Fatalf("expected root location to have no parent, got %v", *parent)
	}
}

func TestPgStore_CreateLocation_DuplicateCode_Refused(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	first := newTestLocation(tenantID, legalEntityID, "WH-DUP")
	if err := s.CreateLocation(ctx, first, nil); err != nil {
		t.Fatalf("first CreateLocation failed: %v", err)
	}
	second := newTestLocation(tenantID, legalEntityID, "WH-DUP")
	if err := s.CreateLocation(ctx, second, nil); err != domain.ErrDuplicateLocationCode {
		t.Fatalf("expected ErrDuplicateLocationCode, got %v", err)
	}
}

// TestPgStore_ReparentLocation_Versions proves ReparentLocation never
// mutates the prior hierarchy row in place — it is end-dated and a new
// version takes over, the real structural answer to "hierarchy changes
// versioned."
func TestPgStore_ReparentLocation_Versions(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	a := newTestLocation(tenantID, legalEntityID, "WH-A")
	if err := s.CreateLocation(ctx, a, nil); err != nil {
		t.Fatalf("create a failed: %v", err)
	}
	b := newTestLocation(tenantID, legalEntityID, "WH-B")
	if err := s.CreateLocation(ctx, b, nil); err != nil {
		t.Fatalf("create b failed: %v", err)
	}

	now := time.Now().UTC()
	if err := s.ReparentLocation(ctx, b.LocationID, a.LocationID, "preparer-1", now); err != nil {
		t.Fatalf("ReparentLocation failed: %v", err)
	}
	parent, err := s.GetCurrentParent(ctx, b.LocationID)
	if err != nil {
		t.Fatalf("GetCurrentParent failed: %v", err)
	}
	if parent == nil || *parent != a.LocationID {
		t.Fatalf("expected b's current parent to be a, got %v", parent)
	}

	// As-of a moment BEFORE the reparent must still show the root (no parent).
	pastParent, err := s.GetParentAsOf(ctx, b.LocationID, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("GetParentAsOf (past) failed: %v", err)
	}
	if pastParent != nil {
		t.Fatalf("expected no parent as-of before the reparent, got %v", *pastParent)
	}
}

// TestPgStore_ReparentLocation_CrossEntity_Refused is the real,
// transaction-level proof of negative path #1.
func TestPgStore_ReparentLocation_CrossEntity_Refused(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	a := newTestLocation(tenantID, uuid.New().String(), "WH-XE-A")
	if err := s.CreateLocation(ctx, a, nil); err != nil {
		t.Fatalf("create a failed: %v", err)
	}
	b := newTestLocation(tenantID, uuid.New().String(), "WH-XE-B")
	if err := s.CreateLocation(ctx, b, nil); err != nil {
		t.Fatalf("create b failed: %v", err)
	}

	if err := s.ReparentLocation(ctx, b.LocationID, a.LocationID, "preparer-1", time.Now().UTC()); err != domain.ErrReparentAcrossLegalEntities {
		t.Fatalf("expected ErrReparentAcrossLegalEntities, got %v", err)
	}
}

// TestPgStore_ReparentLocation_Circular_Refused is the real,
// transaction-level proof of negative path #2.
func TestPgStore_ReparentLocation_Circular_Refused(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	a := newTestLocation(tenantID, legalEntityID, "WH-CYC-A")
	if err := s.CreateLocation(ctx, a, nil); err != nil {
		t.Fatalf("create a failed: %v", err)
	}
	b := newTestLocation(tenantID, legalEntityID, "WH-CYC-B")
	if err := s.CreateLocation(ctx, b, &a.LocationID); err != nil { // b's parent is a
		t.Fatalf("create b failed: %v", err)
	}

	if err := s.ReparentLocation(ctx, a.LocationID, b.LocationID, "preparer-1", time.Now().UTC()); err != domain.ErrCircularLocationHierarchy {
		t.Fatalf("expected ErrCircularLocationHierarchy, got %v", err)
	}
}

// TestPgStore_SetQuarantine_SelfReleaseRefused is the real proof of
// negative path #4.
func TestPgStore_SetQuarantine_SelfReleaseRefused(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	l := newTestLocation(tenantID, legalEntityID, "WH-Q")
	if err := s.CreateLocation(ctx, l, nil); err != nil {
		t.Fatalf("create failed: %v", err)
	}
	now := time.Now().UTC()
	if err := s.ActivateLocation(ctx, l.LocationID, "approver-1", now); err != nil {
		t.Fatalf("activate failed: %v", err)
	}
	if err := s.SetQuarantine(ctx, l.LocationID, "inspector-1", "contamination", true, now); err != nil {
		t.Fatalf("set quarantine failed: %v", err)
	}
	if err := s.SetQuarantine(ctx, l.LocationID, "inspector-1", "cleared", false, now); err != domain.ErrSelfQuarantineReleaseNotPermitted {
		t.Fatalf("expected ErrSelfQuarantineReleaseNotPermitted, got %v", err)
	}
	if err := s.SetQuarantine(ctx, l.LocationID, "inspector-2", "cleared", false, now); err != nil {
		t.Fatalf("expected release by a different principal to succeed, got %v", err)
	}
	got, err := s.GetLocation(ctx, l.LocationID)
	if err != nil {
		t.Fatalf("GetLocation failed: %v", err)
	}
	if got.Status != domain.LocationStatusActive {
		t.Fatalf("expected ACTIVE after release, got %q", got.Status)
	}
}
