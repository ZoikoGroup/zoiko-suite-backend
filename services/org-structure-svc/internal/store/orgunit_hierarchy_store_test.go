package store_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/org-structure-svc/internal/domain"
	"zoiko.io/org-structure-svc/internal/store"
)

func stamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// pause keeps recorded_at values strictly ordered across steps.
func pause() { time.Sleep(25 * time.Millisecond) }

func snapshot(t *testing.T, s *store.PgStore, ctx context.Context, entity, asOf, recorded string, inactive bool) map[string]domain.OrgUnit {
	t.Helper()
	list, err := s.HierarchySnapshot(ctx, entity, asOf, recorded, inactive)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	out := map[string]domain.OrgUnit{}
	for _, u := range list {
		out[u.Code] = u
	}
	return out
}

func parentCode(snap map[string]domain.OrgUnit, code string) string {
	u, ok := snap[code]
	if !ok || u.ParentUnitID == nil {
		return ""
	}
	for c, other := range snap {
		if other.UnitID == *u.ParentUnitID {
			return c
		}
	}
	return "?" + *u.ParentUnitID
}

// Valid time: the tree on a date is the tree that applied on that date.
func TestSnapshot_ValidTimeReconstructsThePastTree(t *testing.T) {
	s := store.New(openOrgUnitPool(t))
	ctx := orgCtx("tenant-a")

	root := mkUnit(t, s, ctx, "e1", "ROOT", nil)
	mid := mkUnit(t, s, ctx, "e1", "MID", &root.UnitID)
	leaf := mkUnit(t, s, ctx, "e1", "LEAF", &mid.UnitID)
	if _, err := s.ReparentOrgUnit(ctx, leaf.UnitID, moveTo(&root.UnitID, "2026-03-01"), 1, "x"); err != nil {
		t.Fatal(err)
	}

	before := snapshot(t, s, ctx, "e1", "2026-02-28", "", false)
	after := snapshot(t, s, ctx, "e1", "2026-03-01", "", false)
	if parentCode(before, "LEAF") != "MID" || parentCode(after, "LEAF") != "ROOT" {
		t.Fatalf("LEAF should be under MID before and ROOT from 2026-03-01, got %q then %q",
			parentCode(before, "LEAF"), parentCode(after, "LEAF"))
	}
	if len(before) != 3 || len(after) != 3 {
		t.Fatalf("each date has all three units, got %d and %d", len(before), len(after))
	}
}

// Recorded time: a report "as known at" an earlier moment must not see a change
// that was only written down later, even though that change applies earlier.
func TestSnapshot_RecordedTimeHidesLaterKnowledge(t *testing.T) {
	s := store.New(openOrgUnitPool(t))
	ctx := orgCtx("tenant-a")

	root := mkUnit(t, s, ctx, "e1", "ROOT", nil)
	mid := mkUnit(t, s, ctx, "e1", "MID", &root.UnitID)
	oldMgr, newMgr := uuid.NewString(), uuid.NewString()
	leafReq := newUnit("e1", "LEAF")
	leafReq.ParentUnitID = &mid.UnitID
	leafReq.ManagerEmployeeID = &oldMgr
	leaf, _, err := s.CreateOrgUnit(ctx, leafReq, "a")
	if err != nil {
		t.Fatal(err)
	}

	pause()
	known := stamp() // everything above was recorded; nothing below yet
	pause()

	// Recorded now, but backdated: applies from 2026-03-01.
	if _, err := s.ReparentOrgUnit(ctx, leaf.UnitID, moveTo(&root.UnitID, "2026-03-01"), 1, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ChangeOrgUnitManager(ctx, leaf.UnitID, changeTo(newMgr, "2026-03-01"), 2, "x"); err != nil {
		t.Fatal(err)
	}
	late := mkUnit(t, s, ctx, "e1", "LATE", nil)
	_ = late

	// As it applied on 2026-04-01, using what we know today: the move happened.
	now := snapshot(t, s, ctx, "e1", "2026-04-01", "", false)
	if parentCode(now, "LEAF") != "ROOT" || *now["LEAF"].ManagerEmployeeID != newMgr {
		t.Fatalf("today's knowledge: LEAF under ROOT with the new manager, got %q / %v", parentCode(now, "LEAF"), now["LEAF"].ManagerEmployeeID)
	}
	if _, ok := now["LATE"]; !ok {
		t.Fatal("a unit created later is part of today's knowledge")
	}

	// Same date, but as known before the move was recorded: the old structure.
	then := snapshot(t, s, ctx, "e1", "2026-04-01", known, false)
	if parentCode(then, "LEAF") != "MID" {
		t.Fatalf("as known then, LEAF was still under MID, got %q", parentCode(then, "LEAF"))
	}
	if m := then["LEAF"].ManagerEmployeeID; m == nil || *m != oldMgr {
		t.Fatalf("as known then, the old manager applied, got %v", m)
	}
	if _, ok := then["LATE"]; ok {
		t.Fatal("a unit created after the recorded time must not appear")
	}
	if len(then) != 3 {
		t.Fatalf("as known then there were 3 units, got %d", len(then))
	}
}

func TestSnapshot_UnitsAppearOnlyWhileTheyHaveAPlace(t *testing.T) {
	s := store.New(openOrgUnitPool(t))
	ctx := orgCtx("tenant-a")

	mkUnit(t, s, ctx, "e1", "OLD", nil) // placed from 2026-01-01
	futureReq := newUnit("e1", "NEW")
	futureReq.EffectiveFrom = "2026-06-01"
	if _, _, err := s.CreateOrgUnit(ctx, futureReq, "a"); err != nil {
		t.Fatal(err)
	}

	if got := snapshot(t, s, ctx, "e1", "2026-05-31", "", false); len(got) != 1 || got["OLD"].UnitID == "" {
		t.Fatalf("before NEW's start only OLD is placed, got %v", got)
	}
	if got := snapshot(t, s, ctx, "e1", "2026-06-01", "", false); len(got) != 2 {
		t.Fatalf("from NEW's start both are placed, got %d", len(got))
	}
	if got := snapshot(t, s, ctx, "e1", "2025-12-31", "", false); len(got) != 0 {
		t.Fatalf("before any placement the tree is empty, got %d", len(got))
	}
}

func TestSnapshot_RetiredUnitsAreOptIn(t *testing.T) {
	s := store.New(openOrgUnitPool(t))
	ctx := orgCtx("tenant-a")

	mkUnit(t, s, ctx, "e1", "KEEP", nil)
	gone := mkUnit(t, s, ctx, "e1", "GONE", nil)
	if _, err := s.TransitionOrgUnit(ctx, gone.UnitID, domain.UnitCommandActivate, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionOrgUnit(ctx, gone.UnitID, domain.UnitCommandDeactivate, 2); err != nil {
		t.Fatal(err)
	}

	if got := snapshot(t, s, ctx, "e1", "", "", false); len(got) != 1 {
		t.Fatalf("current read leaves the retired unit out, got %d", len(got))
	}
	got := snapshot(t, s, ctx, "e1", "", "", true)
	if len(got) != 2 || got["GONE"].Status != domain.UnitStatusInactive {
		t.Fatalf("include_inactive returns it with its status, got %v", got)
	}
}

func TestSnapshot_ScopeAndMapping(t *testing.T) {
	s := store.New(openOrgUnitPool(t))
	ctx := orgCtx("tenant-a")

	req := newUnit("e1", "FIN")
	req.CostCenterCode, req.ProfitCenterCode = "CC1", "PC1"
	if _, _, err := s.CreateOrgUnit(ctx, req, "a"); err != nil {
		t.Fatal(err)
	}
	mkUnit(t, s, ctx, "e2", "OTHER", nil)

	got := snapshot(t, s, ctx, "e1", "", "", false)
	if len(got) != 1 || got["FIN"].CostCenterCode != "CC1" || got["FIN"].ProfitCenterCode != "PC1" {
		t.Fatalf("only e1's unit, with its mapping, got %+v", got)
	}
	if other := snapshot(t, s, orgCtx("tenant-b"), "e1", "", "", true); len(other) != 0 {
		t.Fatalf("another tenant sees nothing, got %d", len(other))
	}
}

// 000004 backfills closed_at for rows closed before it existed, from the
// successor row written in the same transaction, and the down migration undoes it.
func TestMigration000004_BackfillsClosedAt(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS event_outbox, org_unit_change_requests, org_unit_dimension_mappings, org_unit_manager_refs, org_unit_hierarchy_versions, org_units CASCADE;`)

	_, filename, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(filename), "../../deployments/migrations")
	apply := func(name string) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	apply("000003_org_units.up.sql")

	// A unit whose placement was closed by a reparent before closed_at existed.
	unit, v1, v2 := uuid.NewString(), uuid.NewString(), uuid.NewString()
	// Postgres keeps microseconds, so compare at that precision.
	t1 := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	t2 := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Microsecond)
	mustExec := func(q string, args ...any) {
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	mustExec(`INSERT INTO org_units (unit_id, tenant_id, legal_entity_id, code, name, unit_type, status, created_at, updated_at)
	          VALUES ($1,'t','e','U','U','TEAM','ACTIVE',$2,$2)`, unit, t1)
	mustExec(`INSERT INTO org_unit_hierarchy_versions (version_id, tenant_id, unit_id, valid_from, valid_to, recorded_at)
	          VALUES ($1,'t',$2,'2026-01-01','2026-03-01',$3)`, v1, unit, t1)
	mustExec(`INSERT INTO org_unit_hierarchy_versions (version_id, tenant_id, unit_id, valid_from, recorded_at)
	          VALUES ($1,'t',$2,'2026-03-01',$3)`, v2, unit, t2)

	apply("000004_org_unit_closed_at.up.sql")

	var closedOld *time.Time
	var closedNew *time.Time
	if err := pool.QueryRow(ctx, `SELECT closed_at FROM org_unit_hierarchy_versions WHERE version_id = $1`, v1).Scan(&closedOld); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT closed_at FROM org_unit_hierarchy_versions WHERE version_id = $1`, v2).Scan(&closedNew); err != nil {
		t.Fatal(err)
	}
	if closedOld == nil || !closedOld.Equal(t2) {
		t.Fatalf("the closed row's closed_at must be its successor's recorded_at (%v), got %v", t2, closedOld)
	}
	if closedNew != nil {
		t.Fatalf("an open row has no closed_at, got %v", closedNew)
	}

	apply("000004_org_unit_closed_at.down.sql")
	var cols int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.columns WHERE table_name = 'org_unit_hierarchy_versions' AND column_name = 'closed_at'`).Scan(&cols); err != nil {
		t.Fatal(err)
	}
	if cols != 0 {
		t.Fatal("down migration must drop closed_at")
	}
}
