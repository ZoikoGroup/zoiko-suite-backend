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

func newTestOffering(tenantID, legalEntityID, skuCode string) (*domain.Offering, *domain.OfferingVersion) {
	now := time.Now().UTC()
	o := &domain.Offering{
		OfferingID: uuid.New().String(), TenantID: tenantID, LegalEntityID: legalEntityID,
		SKUCode: skuCode, Category: "PROFESSIONAL_SERVICES", OwnerPrincipalID: "owner-1",
		CreatedAt: now, CreatedByPrincipalID: "owner-1",
	}
	v := &domain.OfferingVersion{
		VersionID: uuid.New().String(), TenantID: tenantID, OfferingID: o.OfferingID, VersionNumber: 1,
		Description: "Consulting engagement", Unit: "HOUR", AvailabilityRules: "business hours only",
		Status: domain.CatalogVersionStatusDraft, CreatedAt: now, CreatedByPrincipalID: "owner-1",
	}
	return o, v
}

func TestPgStore_CreateOffering_ThenGetOffering(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenantID, legalEntityID := uuid.New().String(), uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	o, v := newTestOffering(tenantID, legalEntityID, "SKU-CAT-1")
	variants := []domain.CatalogVariantInput{{VariantCode: "STD", VariantName: "Standard"}, {VariantCode: "PREMIUM", VariantName: "Premium"}}
	if err := s.CreateOffering(ctx, o, v, variants); err != nil {
		t.Fatalf("CreateOffering failed: %v", err)
	}

	got, err := s.GetOffering(ctx, o.OfferingID)
	if err != nil {
		t.Fatalf("GetOffering failed: %v", err)
	}
	if got.SKUCode != "SKU-CAT-1" {
		t.Fatalf("expected sku_code SKU-CAT-1, got %s", got.SKUCode)
	}

	gv, err := s.GetOfferingVersion(ctx, v.VersionID)
	if err != nil {
		t.Fatalf("GetOfferingVersion failed: %v", err)
	}
	if gv.Status != domain.CatalogVersionStatusDraft || gv.VersionNumber != 1 {
		t.Fatalf("expected v1 DRAFT, got v%d %s", gv.VersionNumber, gv.Status)
	}

	vs, err := s.ListVariants(ctx, v.VersionID)
	if err != nil {
		t.Fatalf("ListVariants failed: %v", err)
	}
	if len(vs) != 2 {
		t.Fatalf("expected 2 variants, got %d", len(vs))
	}
}

func TestPgStore_CreateOffering_DuplicateSKU_Rejected(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenantID, legalEntityID := uuid.New().String(), uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	o1, v1 := newTestOffering(tenantID, legalEntityID, "SKU-DUP")
	if err := s.CreateOffering(ctx, o1, v1, nil); err != nil {
		t.Fatalf("CreateOffering failed: %v", err)
	}
	o2, v2 := newTestOffering(tenantID, legalEntityID, "SKU-DUP")
	if err := s.CreateOffering(ctx, o2, v2, nil); err != domain.ErrDuplicateOfferingSKU {
		t.Fatalf("expected ErrDuplicateOfferingSKU, got %v", err)
	}
}

func TestPgStore_CreateVersion_ThenApproveActivate_SupersedesPrior(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenantID, legalEntityID := uuid.New().String(), uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	o, v1 := newTestOffering(tenantID, legalEntityID, "SKU-VER")
	if err := s.CreateOffering(ctx, o, v1, nil); err != nil {
		t.Fatalf("CreateOffering failed: %v", err)
	}
	now := time.Now().UTC()
	if err := s.ApproveOfferingVersion(ctx, v1.VersionID, "reviewer-1", now); err != nil {
		t.Fatalf("ApproveOfferingVersion failed: %v", err)
	}
	if superseded, err := s.ActivateOfferingVersion(ctx, o.OfferingID, v1.VersionID, "reviewer-1", now); err != nil {
		t.Fatalf("ActivateOfferingVersion failed: %v", err)
	} else if superseded != nil {
		t.Fatalf("expected no supersession on first activation, got %v", *superseded)
	}

	// Create and activate a second version — the first, now ACTIVE, must
	// be forced to SUPERSEDED in the same transaction.
	v2 := &domain.OfferingVersion{
		VersionID: uuid.New().String(), TenantID: tenantID, OfferingID: o.OfferingID,
		Description: "Revised engagement", Unit: "HOUR",
		Status: domain.CatalogVersionStatusDraft, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "owner-1",
	}
	if err := s.CreateVersion(ctx, v2, nil); err != nil {
		t.Fatalf("CreateVersion failed: %v", err)
	}
	if v2.VersionNumber != 2 {
		t.Fatalf("expected version_number 2, got %d", v2.VersionNumber)
	}
	later := now.Add(time.Hour)
	if err := s.ApproveOfferingVersion(ctx, v2.VersionID, "reviewer-1", later); err != nil {
		t.Fatalf("ApproveOfferingVersion v2 failed: %v", err)
	}
	superseded, err := s.ActivateOfferingVersion(ctx, o.OfferingID, v2.VersionID, "reviewer-1", later)
	if err != nil {
		t.Fatalf("ActivateOfferingVersion v2 failed: %v", err)
	}
	if superseded == nil || *superseded != v1.VersionID {
		t.Fatalf("expected v1 (%s) to be superseded, got %v", v1.VersionID, superseded)
	}

	gotV1, err := s.GetOfferingVersion(ctx, v1.VersionID)
	if err != nil {
		t.Fatalf("GetOfferingVersion v1 failed: %v", err)
	}
	if gotV1.Status != domain.CatalogVersionStatusSuperseded {
		t.Fatalf("expected v1 status SUPERSEDED, got %s", gotV1.Status)
	}

	current, err := s.GetCurrentOfferingVersion(ctx, o.OfferingID)
	if err != nil {
		t.Fatalf("GetCurrentOfferingVersion failed: %v", err)
	}
	if current.VersionID != v2.VersionID {
		t.Fatalf("expected current version to be v2, got %s", current.VersionID)
	}
}

// TestPgStore_OfferingVersionContent_IsImmutable is the negative control
// proving migration 000006's own trigger: pinned content
// (offering_id/version_number/description/unit/availability_rules)
// cannot be rewritten by a raw UPDATE, even though the lifecycle columns
// on the very same row are legitimately mutable.
func TestPgStore_OfferingVersionContent_IsImmutable(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenantID, legalEntityID := uuid.New().String(), uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	o, v := newTestOffering(tenantID, legalEntityID, "SKU-IMMUTABLE")
	if err := s.CreateOffering(ctx, o, v, nil); err != nil {
		t.Fatalf("CreateOffering failed: %v", err)
	}

	if _, err := pool.Exec(context.Background(), `UPDATE inventory_catalog_offering_versions SET description = 'tampered' WHERE version_id = $1`, v.VersionID); err == nil {
		t.Fatal("expected the append-only trigger to reject a direct content mutation, got no error")
	}
}

func TestPgStore_ActivateOfferingVersion_RequiresApproved(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenantID, legalEntityID := uuid.New().String(), uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	o, v := newTestOffering(tenantID, legalEntityID, "SKU-NOAPPROVE")
	if err := s.CreateOffering(ctx, o, v, nil); err != nil {
		t.Fatalf("CreateOffering failed: %v", err)
	}
	if _, err := s.ActivateOfferingVersion(ctx, o.OfferingID, v.VersionID, "reviewer-1", time.Now().UTC()); err != domain.ErrInvalidCatalogVersionTransition {
		t.Fatalf("expected ErrInvalidCatalogVersionTransition activating a DRAFT version, got %v", err)
	}
}

func TestPgStore_SuspendThenRetireOfferingVersion(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenantID, legalEntityID := uuid.New().String(), uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	o, v := newTestOffering(tenantID, legalEntityID, "SKU-LIFECYCLE")
	if err := s.CreateOffering(ctx, o, v, nil); err != nil {
		t.Fatalf("CreateOffering failed: %v", err)
	}
	now := time.Now().UTC()
	if err := s.ApproveOfferingVersion(ctx, v.VersionID, "reviewer-1", now); err != nil {
		t.Fatalf("ApproveOfferingVersion failed: %v", err)
	}
	if _, err := s.ActivateOfferingVersion(ctx, o.OfferingID, v.VersionID, "reviewer-1", now); err != nil {
		t.Fatalf("ActivateOfferingVersion failed: %v", err)
	}
	if err := s.SuspendOfferingVersion(ctx, v.VersionID, "reviewer-1", "quality hold", now); err != nil {
		t.Fatalf("SuspendOfferingVersion failed: %v", err)
	}
	if err := s.RetireOfferingVersion(ctx, v.VersionID, "reviewer-1", "discontinued", now); err != nil {
		t.Fatalf("RetireOfferingVersion failed: %v", err)
	}
	got, err := s.GetOfferingVersion(ctx, v.VersionID)
	if err != nil {
		t.Fatalf("GetOfferingVersion failed: %v", err)
	}
	if got.Status != domain.CatalogVersionStatusRetired {
		t.Fatalf("expected status RETIRED, got %s", got.Status)
	}

	// Negative control: an already-RETIRED version cannot be retired again.
	if err := s.RetireOfferingVersion(ctx, v.VersionID, "reviewer-1", "again", now); err != domain.ErrInvalidCatalogVersionTransition {
		t.Fatalf("expected ErrInvalidCatalogVersionTransition on double retire, got %v", err)
	}
}

func TestPgStore_GetVersionAsOf_PinsHistoricalVersion(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenantID, legalEntityID := uuid.New().String(), uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	o, v1 := newTestOffering(tenantID, legalEntityID, "SKU-ASOF")
	if err := s.CreateOffering(ctx, o, v1, nil); err != nil {
		t.Fatalf("CreateOffering failed: %v", err)
	}
	t1 := time.Now().UTC()
	if err := s.ApproveOfferingVersion(ctx, v1.VersionID, "reviewer-1", t1); err != nil {
		t.Fatalf("ApproveOfferingVersion v1 failed: %v", err)
	}
	if _, err := s.ActivateOfferingVersion(ctx, o.OfferingID, v1.VersionID, "reviewer-1", t1); err != nil {
		t.Fatalf("ActivateOfferingVersion v1 failed: %v", err)
	}

	midpoint := t1.Add(time.Hour)

	v2 := &domain.OfferingVersion{
		VersionID: uuid.New().String(), TenantID: tenantID, OfferingID: o.OfferingID,
		Description: "v2 content", Unit: "HOUR",
		Status: domain.CatalogVersionStatusDraft, CreatedAt: midpoint, CreatedByPrincipalID: "owner-1",
	}
	if err := s.CreateVersion(ctx, v2, nil); err != nil {
		t.Fatalf("CreateVersion failed: %v", err)
	}
	t2 := midpoint.Add(time.Hour)
	if err := s.ApproveOfferingVersion(ctx, v2.VersionID, "reviewer-1", t2); err != nil {
		t.Fatalf("ApproveOfferingVersion v2 failed: %v", err)
	}
	if _, err := s.ActivateOfferingVersion(ctx, o.OfferingID, v2.VersionID, "reviewer-1", t2); err != nil {
		t.Fatalf("ActivateOfferingVersion v2 failed: %v", err)
	}

	// A caller asking what was active BEFORE v2's activation must still
	// see v1 — even though v1 is now SUPERSEDED and v2 is the live
	// current version. This is the literal "historical transactions pin
	// the offering version" guarantee.
	asOf, err := s.GetVersionAsOf(ctx, o.OfferingID, midpoint)
	if err != nil {
		t.Fatalf("GetVersionAsOf failed: %v", err)
	}
	if asOf.VersionID != v1.VersionID {
		t.Fatalf("expected v1 (%s) to be pinned at midpoint, got %s", v1.VersionID, asOf.VersionID)
	}

	asOfLater, err := s.GetVersionAsOf(ctx, o.OfferingID, t2.Add(time.Minute))
	if err != nil {
		t.Fatalf("GetVersionAsOf (later) failed: %v", err)
	}
	if asOfLater.VersionID != v2.VersionID {
		t.Fatalf("expected v2 (%s) to be active after t2, got %s", v2.VersionID, asOfLater.VersionID)
	}
}

// TestPgStore_GetVersionAsOf_NoVersionActive_ReturnsCatalogVersionInvalid
// is the negative control for CATALOG_VERSION_INVALID, the doc's own
// named stable error.
func TestPgStore_GetVersionAsOf_NoVersionActive_ReturnsCatalogVersionInvalid(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenantID, legalEntityID := uuid.New().String(), uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	o, v := newTestOffering(tenantID, legalEntityID, "SKU-NEVERACTIVE")
	if err := s.CreateOffering(ctx, o, v, nil); err != nil {
		t.Fatalf("CreateOffering failed: %v", err)
	}
	if _, err := s.GetVersionAsOf(ctx, o.OfferingID, time.Now().UTC()); err != domain.ErrCatalogVersionInvalid {
		t.Fatalf("expected ErrCatalogVersionInvalid, got %v", err)
	}
}

func TestPgStore_SearchCatalog_FiltersByLegalEntityAndCategory(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenantID, legalEntityID, otherEntity := uuid.New().String(), uuid.New().String(), uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	o1, v1 := newTestOffering(tenantID, legalEntityID, "SKU-SEARCH-1")
	if err := s.CreateOffering(ctx, o1, v1, nil); err != nil {
		t.Fatalf("CreateOffering o1 failed: %v", err)
	}
	o2, v2 := newTestOffering(tenantID, otherEntity, "SKU-SEARCH-2")
	if err := s.CreateOffering(ctx, o2, v2, nil); err != nil {
		t.Fatalf("CreateOffering o2 failed: %v", err)
	}

	results, err := s.SearchCatalog(ctx, legalEntityID, "")
	if err != nil {
		t.Fatalf("SearchCatalog failed: %v", err)
	}
	if len(results) != 1 || results[0].OfferingID != o1.OfferingID {
		t.Fatalf("expected exactly o1 for legalEntityID, got %+v", results)
	}

	byCategory, err := s.SearchCatalog(ctx, legalEntityID, "PROFESSIONAL_SERVICES")
	if err != nil {
		t.Fatalf("SearchCatalog by category failed: %v", err)
	}
	if len(byCategory) != 1 {
		t.Fatalf("expected 1 match for category, got %d", len(byCategory))
	}

	byWrongCategory, err := s.SearchCatalog(ctx, legalEntityID, "NONEXISTENT")
	if err != nil {
		t.Fatalf("SearchCatalog by wrong category failed: %v", err)
	}
	if len(byWrongCategory) != 0 {
		t.Fatalf("expected 0 matches for a nonexistent category, got %d", len(byWrongCategory))
	}
}

func TestPgStore_LinkMapping_ThenGetMappings(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenantID, legalEntityID := uuid.New().String(), uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	o, v := newTestOffering(tenantID, legalEntityID, "SKU-MAPPING")
	if err := s.CreateOffering(ctx, o, v, nil); err != nil {
		t.Fatalf("CreateOffering failed: %v", err)
	}
	m := &domain.CatalogMapping{
		MappingID: uuid.New().String(), VersionID: v.VersionID, MappingType: domain.CatalogMappingTax,
		MappingRef: "TAX-CODE-STD", LinkedAt: time.Now().UTC(), LinkedByPrincipalID: "tax-specialist-1",
	}
	if err := s.LinkMapping(ctx, m); err != nil {
		t.Fatalf("LinkMapping failed: %v", err)
	}
	mappings, err := s.GetMappings(ctx, v.VersionID)
	if err != nil {
		t.Fatalf("GetMappings failed: %v", err)
	}
	if len(mappings) != 1 || mappings[0].MappingRef != "TAX-CODE-STD" {
		t.Fatalf("expected 1 TAX mapping, got %+v", mappings)
	}
}

// TestPgStore_CatalogMappings_AreAppendOnly is the negative control on
// migration 000006's unconditional trigger — a mapping in force when a
// transaction referenced a version must never silently change.
func TestPgStore_CatalogMappings_AreAppendOnly(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenantID, legalEntityID := uuid.New().String(), uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	o, v := newTestOffering(tenantID, legalEntityID, "SKU-MAPPING-AO")
	if err := s.CreateOffering(ctx, o, v, nil); err != nil {
		t.Fatalf("CreateOffering failed: %v", err)
	}
	m := &domain.CatalogMapping{
		MappingID: uuid.New().String(), VersionID: v.VersionID, MappingType: domain.CatalogMappingAccounting,
		MappingRef: "GL-4000", LinkedAt: time.Now().UTC(), LinkedByPrincipalID: "accountant-1",
	}
	if err := s.LinkMapping(ctx, m); err != nil {
		t.Fatalf("LinkMapping failed: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE inventory_catalog_mappings SET mapping_ref = 'GL-9999' WHERE mapping_id = $1`, m.MappingID); err == nil {
		t.Fatal("expected the append-only trigger to reject a direct mapping mutation, got no error")
	}
}

func TestPgStore_LinkMapping_UnknownVersion_ReturnsNotFound(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	m := &domain.CatalogMapping{
		MappingID: uuid.New().String(), VersionID: uuid.New().String(), MappingType: domain.CatalogMappingProduct,
		MappingRef: "PROD-1", LinkedAt: time.Now().UTC(), LinkedByPrincipalID: "specialist-1",
	}
	if err := s.LinkMapping(ctx, m); err != domain.ErrOfferingVersionNotFound {
		t.Fatalf("expected ErrOfferingVersionNotFound, got %v", err)
	}
}
