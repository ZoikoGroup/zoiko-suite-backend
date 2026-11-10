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

// Migration 000013: the database itself refuses ACTIVE without TAX and
// ACCOUNTING mappings, even for a writer that bypasses the handler.
func TestPgStore_Catalog_ActivationRequiresMappings_EnforcedInDatabase(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	now := time.Now().UTC()

	o, v := newTestOffering(tenantID, uuid.New().String(), "SKU-TRIG-1")
	if err := s.CreateOffering(ctx, o, v, nil); err != nil {
		t.Fatalf("CreateOffering failed: %v", err)
	}
	if err := s.ApproveOfferingVersion(ctx, v.VersionID, "reviewer-1", now); err != nil {
		t.Fatalf("approve failed: %v", err)
	}

	// Store path with no mappings: blocked by the trigger.
	if _, err := s.ActivateOfferingVersion(ctx, o.OfferingID, v.VersionID, "reviewer-1", now); err == nil {
		t.Fatal("expected activation without mappings to be refused by the database")
	}

	// Raw SQL bypassing the store: also blocked.
	_, err := pool.Exec(context.Background(), `UPDATE inventory_catalog_offering_versions SET status = 'ACTIVE' WHERE version_id = $1`, v.VersionID)
	if err == nil {
		t.Fatal("expected direct UPDATE to ACTIVE to be refused by the database")
	}

	// TAX only: still blocked.
	if err := s.LinkMapping(ctx, &domain.CatalogMapping{MappingID: uuid.New().String(), VersionID: v.VersionID, MappingType: "TAX", MappingRef: "T", LinkedAt: now, LinkedByPrincipalID: "s"}); err != nil {
		t.Fatalf("link TAX failed: %v", err)
	}
	if _, err := s.ActivateOfferingVersion(ctx, o.OfferingID, v.VersionID, "reviewer-1", now); err == nil {
		t.Fatal("expected activation with TAX only to be refused")
	}

	// Both required: succeeds.
	if err := s.LinkMapping(ctx, &domain.CatalogMapping{MappingID: uuid.New().String(), VersionID: v.VersionID, MappingType: "ACCOUNTING", MappingRef: "A", LinkedAt: now, LinkedByPrincipalID: "s"}); err != nil {
		t.Fatalf("link ACCOUNTING failed: %v", err)
	}
	if _, err := s.ActivateOfferingVersion(ctx, o.OfferingID, v.VersionID, "reviewer-1", now); err != nil {
		t.Fatalf("activation with both mappings failed: %v", err)
	}
}
