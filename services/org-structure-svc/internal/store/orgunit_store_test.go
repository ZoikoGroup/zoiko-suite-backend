package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/org-structure-svc/internal/domain"
	"zoiko.io/org-structure-svc/internal/middleware"
	"zoiko.io/org-structure-svc/internal/store"
)

// openOrgUnitPool reuses openTestPool (migrations 1 and 2) and then applies
// the ORG-05 migration from a clean slate.
func openOrgUnitPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := openTestPool(t)
	ctx := context.Background()
	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS org_unit_dimension_mappings, org_unit_manager_refs, org_unit_hierarchy_versions, org_units CASCADE;`)

	_, filename, _, _ := runtime.Caller(0)
	sql, err := os.ReadFile(filepath.Join(filepath.Dir(filename), "../../deployments/migrations/000003_org_units.up.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("apply migration: %v", err)
	}
	return pool
}

func orgCtx(tenant string) context.Context {
	return middleware.WithTenant(context.Background(), tenant)
}

func newUnit(entity, code string) *domain.CreateOrgUnitRequest {
	return &domain.CreateOrgUnitRequest{
		LegalEntityID: entity, Code: code, Name: code + " unit", UnitType: domain.UnitDepartment,
		EffectiveFrom: "2026-01-01", CorrelationID: uuid.NewString(),
	}
}

func TestOrgUnit_CreateGetAndIdempotentRetry(t *testing.T) {
	s := store.New(openOrgUnitPool(t))
	ctx := orgCtx("tenant-a")

	req := newUnit("entity-1", "FIN")
	mgr := uuid.NewString()
	req.ManagerEmployeeID = &mgr
	req.CostCenterCode, req.ProfitCenterCode = "CC100", "PC200"

	u, created, err := s.CreateOrgUnit(ctx, req, "actor-1")
	if err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	if u.Status != domain.UnitStatusDraft || u.RecordVersion != 1 {
		t.Fatalf("new unit must be DRAFT v1, got %s v%d", u.Status, u.RecordVersion)
	}

	got, err := s.GetOrgUnit(ctx, u.UnitID, "")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ManagerEmployeeID == nil || *got.ManagerEmployeeID != mgr || got.CostCenterCode != "CC100" || got.ProfitCenterCode != "PC200" {
		t.Fatalf("manager/mapping not read back: %+v", got)
	}

	again, created, err := s.CreateOrgUnit(ctx, req, "actor-1")
	if err != nil || created || again.UnitID != u.UnitID {
		t.Fatalf("retry must return the original: created=%v err=%v", created, err)
	}
}

func TestOrgUnit_DuplicateCodeRejected(t *testing.T) {
	s := store.New(openOrgUnitPool(t))
	ctx := orgCtx("tenant-a")

	if _, _, err := s.CreateOrgUnit(ctx, newUnit("entity-1", "HR"), "a"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, _, err := s.CreateOrgUnit(ctx, newUnit("entity-1", "HR"), "a"); !errors.Is(err, domain.ErrOrgUnitCodeTaken) {
		t.Fatalf("want ErrOrgUnitCodeTaken, got %v", err)
	}
	// Same code in another legal entity is fine.
	if _, _, err := s.CreateOrgUnit(ctx, newUnit("entity-2", "HR"), "a"); err != nil {
		t.Fatalf("same code in another entity must be allowed: %v", err)
	}
}

func TestOrgUnit_ParentMustShareLegalEntity(t *testing.T) {
	s := store.New(openOrgUnitPool(t))
	ctx := orgCtx("tenant-a")

	parent, _, err := s.CreateOrgUnit(ctx, newUnit("entity-1", "ROOT"), "a")
	if err != nil {
		t.Fatalf("parent: %v", err)
	}

	child := newUnit("entity-2", "KID")
	child.ParentUnitID = &parent.UnitID
	if _, _, err := s.CreateOrgUnit(ctx, child, "a"); !errors.Is(err, domain.ErrParentInvalid) {
		t.Fatalf("cross-entity parent: want ErrParentInvalid, got %v", err)
	}

	missing := uuid.NewString()
	child.ParentUnitID = &missing
	if _, _, err := s.CreateOrgUnit(ctx, child, "a"); !errors.Is(err, domain.ErrParentInvalid) {
		t.Fatalf("missing parent: want ErrParentInvalid, got %v", err)
	}

	ok := newUnit("entity-1", "KID")
	ok.ParentUnitID = &parent.UnitID
	u, _, err := s.CreateOrgUnit(ctx, ok, "a")
	if err != nil || u.ParentUnitID == nil || *u.ParentUnitID != parent.UnitID {
		t.Fatalf("same-entity parent: err=%v unit=%+v", err, u)
	}
}

func TestOrgUnit_TenantIsolation(t *testing.T) {
	s := store.New(openOrgUnitPool(t))

	u, _, err := s.CreateOrgUnit(orgCtx("tenant-a"), newUnit("entity-1", "OPS"), "a")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.GetOrgUnit(orgCtx("tenant-b"), u.UnitID, ""); !errors.Is(err, domain.ErrOrgUnitNotFound) {
		t.Fatalf("tenant-b must not see tenant-a's unit, got %v", err)
	}
	list, err := s.ListOrgUnits(orgCtx("tenant-b"), "", "")
	if err != nil || len(list) != 0 {
		t.Fatalf("tenant-b list: len=%d err=%v", len(list), err)
	}
}

func TestOrgUnit_GetBeforeEffectiveDateHasNoPlacement(t *testing.T) {
	s := store.New(openOrgUnitPool(t))
	ctx := orgCtx("tenant-a")

	parent, _, _ := s.CreateOrgUnit(ctx, newUnit("entity-1", "P"), "a")
	req := newUnit("entity-1", "C")
	req.ParentUnitID = &parent.UnitID
	req.EffectiveFrom = "2026-06-01"
	c, _, err := s.CreateOrgUnit(ctx, req, "a")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	before, err := s.GetOrgUnit(ctx, c.UnitID, "2026-05-31")
	if err != nil || before.ParentUnitID != nil {
		t.Fatalf("before effective date the unit has no parent: err=%v parent=%v", err, before.ParentUnitID)
	}
	on, err := s.GetOrgUnit(ctx, c.UnitID, "2026-06-01")
	if err != nil || on.ParentUnitID == nil {
		t.Fatalf("on the effective date the parent applies: err=%v", err)
	}
}
