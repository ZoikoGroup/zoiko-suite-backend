package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/inventory-management-svc/internal/domain"
	svcmiddleware "zoiko.io/inventory-management-svc/internal/middleware"
	"zoiko.io/inventory-management-svc/internal/store"
)

// BIZ-07 implementation gate: tenant isolation. Tenant B must not be able to
// read, search, transition or map tenant A's offering, and nothing B attempts
// may change A's data.
func TestPgStore_Catalog_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenantA, tenantB := uuid.New().String(), uuid.New().String()
	legalEntity := uuid.New().String() // same legal-entity id string on purpose
	ctxA := svcmiddleware.WithTenant(context.Background(), tenantA)
	ctxB := svcmiddleware.WithTenant(context.Background(), tenantB)
	now := time.Now().UTC()

	o, v := newTestOffering(tenantA, legalEntity, "SKU-ISO-1")
	if err := s.CreateOffering(ctxA, o, v, []domain.CatalogVariantInput{{VariantCode: "STD", VariantName: "Standard"}}); err != nil {
		t.Fatalf("tenant A CreateOffering failed: %v", err)
	}
	if err := s.ApproveOfferingVersion(ctxA, v.VersionID, "reviewer-1", now); err != nil {
		t.Fatalf("tenant A approve failed: %v", err)
	}
	mA := &domain.CatalogMapping{MappingID: uuid.New().String(), VersionID: v.VersionID, MappingType: "TAX", MappingRef: "TAX-A", LinkedAt: now, LinkedByPrincipalID: "specialist-1"}
	if err := s.LinkMapping(ctxA, mA); err != nil {
		t.Fatalf("tenant A LinkMapping failed: %v", err)
	}

	// Reads: B sees nothing of A's.
	if _, err := s.GetOffering(ctxB, o.OfferingID); !errors.Is(err, domain.ErrOfferingNotFound) {
		t.Fatalf("GetOffering as B: expected ErrOfferingNotFound, got %v", err)
	}
	if _, err := s.GetOfferingVersion(ctxB, v.VersionID); !errors.Is(err, domain.ErrOfferingVersionNotFound) {
		t.Fatalf("GetOfferingVersion as B: expected ErrOfferingVersionNotFound, got %v", err)
	}
	if list, err := s.SearchCatalog(ctxB, legalEntity, ""); err != nil || len(list) != 0 {
		t.Fatalf("SearchCatalog as B: expected empty, got %d (err %v)", len(list), err)
	}
	if vs, err := s.ListVariants(ctxB, v.VersionID); err != nil || len(vs) != 0 {
		t.Fatalf("ListVariants as B: expected empty, got %d (err %v)", len(vs), err)
	}
	if ms, err := s.GetMappings(ctxB, v.VersionID); err != nil || len(ms) != 0 {
		t.Fatalf("GetMappings as B: expected empty, got %d (err %v)", len(ms), err)
	}
	if _, err := s.GetVersionAsOf(ctxB, o.OfferingID, now.Add(time.Hour)); !errors.Is(err, domain.ErrCatalogVersionInvalid) {
		t.Fatalf("GetVersionAsOf as B: expected ErrCatalogVersionInvalid, got %v", err)
	}

	// RLS itself, bypassing the store's own tenant_id predicates: with B's
	// tenant context and no WHERE tenant_id, A's rows must still be invisible.
	// Only meaningful when the connecting role is not a superuser / BYPASSRLS;
	// otherwise it is skipped rather than reported as a pass.
	ctx := context.Background()
	var bypass bool
	if err := pool.QueryRow(ctx, `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&bypass); err != nil {
		t.Fatalf("role probe failed: %v", err)
	}
	if bypass {
		t.Log("connecting role bypasses RLS; skipping the raw RLS probe")
	} else {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantB); err != nil {
			t.Fatalf("set tenant: %v", err)
		}
		for _, table := range []string{"inventory_catalog_offerings", "inventory_catalog_offering_versions", "inventory_catalog_variants", "inventory_catalog_mappings"} {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE tenant_id = $1`, tenantA).Scan(&n); err != nil {
				t.Fatalf("%s: %v", table, err)
			}
			if n != 0 {
				t.Fatalf("RLS leak: tenant B context sees %d of tenant A's rows in %s", n, table)
			}
		}
	}

	// Writes: B cannot map, activate, suspend or retire A's version.
	mB := &domain.CatalogMapping{MappingID: uuid.New().String(), VersionID: v.VersionID, MappingType: "ACCOUNTING", MappingRef: "ACC-B", LinkedAt: now, LinkedByPrincipalID: "attacker"}
	if err := s.LinkMapping(ctxB, mB); !errors.Is(err, domain.ErrOfferingVersionNotFound) {
		t.Fatalf("LinkMapping as B: expected ErrOfferingVersionNotFound, got %v", err)
	}
	if _, err := s.ActivateOfferingVersion(ctxB, o.OfferingID, v.VersionID, "attacker", now); !errors.Is(err, domain.ErrInvalidCatalogVersionTransition) {
		t.Fatalf("Activate as B: expected ErrInvalidCatalogVersionTransition, got %v", err)
	}
	if err := s.SuspendOfferingVersion(ctxB, v.VersionID, "attacker", "x", now); !errors.Is(err, domain.ErrInvalidCatalogVersionTransition) {
		t.Fatalf("Suspend as B: expected ErrInvalidCatalogVersionTransition, got %v", err)
	}
	if err := s.RetireOfferingVersion(ctxB, v.VersionID, "attacker", "x", now); !errors.Is(err, domain.ErrInvalidCatalogVersionTransition) {
		t.Fatalf("Retire as B: expected ErrInvalidCatalogVersionTransition, got %v", err)
	}

	// A's data is unchanged by everything B attempted.
	got, err := s.GetOfferingVersion(ctxA, v.VersionID)
	if err != nil || got.Status != domain.CatalogVersionStatusApproved {
		t.Fatalf("tenant A version should still be APPROVED, got %+v (err %v)", got, err)
	}
	ms, err := s.GetMappings(ctxA, v.VersionID)
	if err != nil || len(ms) != 1 || ms[0].MappingRef != "TAX-A" {
		t.Fatalf("tenant A mappings should be exactly [TAX-A], got %+v (err %v)", ms, err)
	}

	// SKU uniqueness is per tenant: B may reuse A's SKU for the same entity id.
	oB, vB := newTestOffering(tenantB, legalEntity, "SKU-ISO-1")
	if err := s.CreateOffering(ctxB, oB, vB, nil); err != nil {
		t.Fatalf("tenant B reusing A's SKU should succeed, got %v", err)
	}
}
