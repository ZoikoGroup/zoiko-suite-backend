package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"zoiko.io/org-structure-svc/internal/domain"
	"zoiko.io/org-structure-svc/internal/store"
)

func mkUnit(t *testing.T, s *store.PgStore, ctx context.Context, entity, code string, parent *string) *domain.OrgUnit {
	t.Helper()
	req := newUnit(entity, code)
	req.ParentUnitID = parent
	u, _, err := s.CreateOrgUnit(ctx, req, "a")
	if err != nil {
		t.Fatalf("create %s: %v", code, err)
	}
	return u
}

func moveTo(parent *string, at string) *domain.ReparentOrgUnitRequest {
	return &domain.ReparentOrgUnitRequest{NewParentUnitID: parent, EffectiveAt: at, Reason: "test"}
}

func parentAt(t *testing.T, s *store.PgStore, ctx context.Context, id, asOf string) string {
	t.Helper()
	u, err := s.GetOrgUnit(ctx, id, asOf)
	if err != nil {
		t.Fatalf("get %s as of %s: %v", id, asOf, err)
	}
	if u.ParentUnitID == nil {
		return ""
	}
	return *u.ParentUnitID
}

// Spec acceptance: reparenting is effective-dated, and the past tree stays
// reconstructable because the old placement is closed, never rewritten.
func TestReparent_KeepsHistoryAndPastTree(t *testing.T) {
	pool := openOrgUnitPool(t)
	s := store.New(pool)
	ctx := orgCtx("tenant-a")

	root := mkUnit(t, s, ctx, "e1", "ROOT", nil)
	mid := mkUnit(t, s, ctx, "e1", "MID", &root.UnitID)
	leaf := mkUnit(t, s, ctx, "e1", "LEAF", &mid.UnitID)

	u, err := s.ReparentOrgUnit(ctx, leaf.UnitID, moveTo(&root.UnitID, "2026-03-01"), 1, "actor")
	if err != nil || u.RecordVersion != 2 {
		t.Fatalf("reparent: v=%v err=%v", u, err)
	}

	if got := parentAt(t, s, ctx, leaf.UnitID, "2026-02-28"); got != mid.UnitID {
		t.Fatalf("before the move the leaf is under mid, got %q", got)
	}
	if got := parentAt(t, s, ctx, leaf.UnitID, "2026-03-01"); got != root.UnitID {
		t.Fatalf("from the effective date the leaf is under root, got %q", got)
	}

	var versions, closed int
	if err := pool.QueryRow(context.Background(), `
		SELECT COUNT(*), COUNT(valid_to) FROM org_unit_hierarchy_versions WHERE unit_id = $1`, leaf.UnitID).Scan(&versions, &closed); err != nil {
		t.Fatal(err)
	}
	if versions != 2 || closed != 1 {
		t.Fatalf("want 2 placements with the old one closed, got %d / %d closed", versions, closed)
	}
}

func TestReparent_FutureAndBackdated(t *testing.T) {
	s := store.New(openOrgUnitPool(t))
	ctx := orgCtx("tenant-a")

	a := mkUnit(t, s, ctx, "e1", "A", nil)
	b := mkUnit(t, s, ctx, "e1", "B", nil)
	c := mkUnit(t, s, ctx, "e1", "C", &a.UnitID)

	// Future-dated: the unit stays under A until 2099.
	if _, err := s.ReparentOrgUnit(ctx, c.UnitID, moveTo(&b.UnitID, "2099-01-01"), 1, "x"); err != nil {
		t.Fatalf("future-dated: %v", err)
	}
	if got := parentAt(t, s, ctx, c.UnitID, ""); got != a.UnitID {
		t.Fatalf("today the unit is still under A, got %q", got)
	}
	if got := parentAt(t, s, ctx, c.UnitID, "2099-01-01"); got != b.UnitID {
		t.Fatalf("in 2099 it is under B, got %q", got)
	}

	// Backdated, but after the unit's own placement began (2026-01-01).
	d := mkUnit(t, s, ctx, "e1", "D", &a.UnitID)
	if _, err := s.ReparentOrgUnit(ctx, d.UnitID, moveTo(&b.UnitID, "2026-02-01"), 1, "x"); err != nil {
		t.Fatalf("backdated: %v", err)
	}
	if got := parentAt(t, s, ctx, d.UnitID, "2026-01-15"); got != a.UnitID {
		t.Fatalf("before the backdated move D is under A, got %q", got)
	}
}

func TestReparent_RefusalsLeaveNoTrace(t *testing.T) {
	pool := openOrgUnitPool(t)
	s := store.New(pool)
	ctx := orgCtx("tenant-a")

	root := mkUnit(t, s, ctx, "e1", "ROOT", nil)
	mid := mkUnit(t, s, ctx, "e1", "MID", &root.UnitID)
	leaf := mkUnit(t, s, ctx, "e1", "LEAF", &mid.UnitID)
	foreign := mkUnit(t, s, ctx, "e2", "FOREIGN", nil)

	cases := []struct {
		name string
		unit *domain.OrgUnit
		req  *domain.ReparentOrgUnitRequest
		ver  int64
		want error
	}{
		{"loop under grandchild", root, moveTo(&leaf.UnitID, "2026-03-01"), 1, domain.ErrHierarchyLoop},
		{"loop under child", root, moveTo(&mid.UnitID, "2026-03-01"), 1, domain.ErrHierarchyLoop},
		{"loop under itself", mid, moveTo(&mid.UnitID, "2026-03-01"), 1, domain.ErrHierarchyLoop},
		{"same parent", leaf, moveTo(&mid.UnitID, "2026-03-01"), 1, domain.ErrNoHierarchyChange},
		{"root to root", root, moveTo(nil, "2026-03-01"), 1, domain.ErrNoHierarchyChange},
		{"effective on placement start", leaf, moveTo(&root.UnitID, "2026-01-01"), 1, domain.ErrEffectiveDateInvalid},
		{"effective before placement start", leaf, moveTo(&root.UnitID, "2025-01-01"), 1, domain.ErrEffectiveDateInvalid},
		{"other legal entity", leaf, moveTo(&foreign.UnitID, "2026-03-01"), 1, domain.ErrParentInvalid},
		{"missing parent", leaf, moveTo(strP("00000000-0000-0000-0000-000000000000"), "2026-03-01"), 1, domain.ErrParentInvalid},
		{"stale version", leaf, moveTo(&root.UnitID, "2026-03-01"), 9, domain.ErrVersionConflict},
	}
	for _, c := range cases {
		if _, err := s.ReparentOrgUnit(ctx, c.unit.UnitID, c.req, c.ver, "x"); !errors.Is(err, c.want) {
			t.Errorf("%s: want %v, got %v", c.name, c.want, err)
		}
	}

	// None of the refusals touched anything.
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM org_unit_hierarchy_versions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("refusals must add no placements: want 4 rows, got %d", n)
	}
	got, _ := s.GetOrgUnit(ctx, root.UnitID, "")
	if got.RecordVersion != 1 {
		t.Fatalf("a refused move must not bump the version, got %d", got.RecordVersion)
	}
}

func strP(s string) *string { return &s }

// A move that is safe today but forms a loop through an already-scheduled
// change is refused too (the check is conservative about the future).
func TestReparent_RefusesLoopThroughScheduledChange(t *testing.T) {
	s := store.New(openOrgUnitPool(t))
	ctx := orgCtx("tenant-a")

	p := mkUnit(t, s, ctx, "e1", "P", nil)
	q := mkUnit(t, s, ctx, "e1", "Q", nil)

	if _, err := s.ReparentOrgUnit(ctx, p.UnitID, moveTo(&q.UnitID, "2099-01-01"), 1, "x"); err != nil {
		t.Fatalf("schedule P under Q: %v", err)
	}
	if _, err := s.ReparentOrgUnit(ctx, q.UnitID, moveTo(&p.UnitID, "2050-01-01"), 1, "x"); !errors.Is(err, domain.ErrHierarchyLoop) {
		t.Fatalf("Q under P would loop once P moves under Q: want ErrHierarchyLoop, got %v", err)
	}
}

func TestReparent_InactiveParentAndUnitRefused(t *testing.T) {
	s := store.New(openOrgUnitPool(t))
	ctx := orgCtx("tenant-a")

	a := mkUnit(t, s, ctx, "e1", "A", nil)
	b := mkUnit(t, s, ctx, "e1", "B", nil)
	for _, u := range []*domain.OrgUnit{b} {
		if _, err := s.TransitionOrgUnit(ctx, u.UnitID, domain.UnitCommandActivate, 1); err != nil {
			t.Fatal(err)
		}
		if _, err := s.TransitionOrgUnit(ctx, u.UnitID, domain.UnitCommandDeactivate, 2); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ReparentOrgUnit(ctx, a.UnitID, moveTo(&b.UnitID, "2026-03-01"), 1, "x"); !errors.Is(err, domain.ErrParentNotActive) {
		t.Fatalf("inactive parent: want ErrParentNotActive, got %v", err)
	}
	if _, err := s.ReparentOrgUnit(ctx, b.UnitID, moveTo(&a.UnitID, "2026-03-01"), 3, "x"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("inactive unit: want ErrInvalidTransition, got %v", err)
	}
}

// Two moves that are each fine alone but together form a loop (A under B,
// B under A) race: exactly one must win, the other must see the loop.
func TestReparent_ConcurrentMovesCannotFormALoop(t *testing.T) {
	s := store.New(openOrgUnitPool(t))
	ctx := orgCtx("tenant-a")

	for round := 0; round < 8; round++ {
		a := mkUnit(t, s, ctx, "e1", "A"+string(rune('0'+round)), nil)
		b := mkUnit(t, s, ctx, "e1", "B"+string(rune('0'+round)), nil)

		var wg sync.WaitGroup
		errs := make([]error, 2)
		start := make(chan struct{})
		moves := []struct{ unit, under *domain.OrgUnit }{{a, b}, {b, a}}
		for i, m := range moves {
			wg.Add(1)
			go func(i int, unit, under *domain.OrgUnit) {
				defer wg.Done()
				<-start
				_, errs[i] = s.ReparentOrgUnit(ctx, unit.UnitID, moveTo(&under.UnitID, "2026-03-01"), 1, "x")
			}(i, m.unit, m.under)
		}
		close(start)
		wg.Wait()

		ok, loops := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, domain.ErrHierarchyLoop):
				loops++
			default:
				t.Fatalf("round %d: unexpected error %v", round, err)
			}
		}
		if ok != 1 || loops != 1 {
			t.Fatalf("round %d: want exactly one success and one loop refusal, got %d ok / %d loop (%v)", round, ok, loops, errs)
		}
	}
}
