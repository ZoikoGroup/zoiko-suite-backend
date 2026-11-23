package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"zoiko.io/org-structure-svc/internal/domain"
	"zoiko.io/org-structure-svc/internal/store"
)

func newUnitWithManager(t *testing.T, s *store.PgStore, ctx context.Context, entity, code, manager string) *domain.OrgUnit {
	t.Helper()
	req := newUnit(entity, code)
	if manager != "" {
		req.ManagerEmployeeID = &manager
	}
	u, _, err := s.CreateOrgUnit(ctx, req, "a")
	if err != nil {
		t.Fatalf("create %s: %v", code, err)
	}
	return u
}

func managerAt(t *testing.T, s *store.PgStore, ctx context.Context, id, asOf string) string {
	t.Helper()
	u, err := s.GetOrgUnit(ctx, id, asOf)
	if err != nil {
		t.Fatalf("get %s as of %s: %v", id, asOf, err)
	}
	if u.ManagerEmployeeID == nil {
		return ""
	}
	return *u.ManagerEmployeeID
}

func changeTo(manager, at string) *domain.ChangeManagerRequest {
	return &domain.ChangeManagerRequest{ManagerEmployeeID: manager, EffectiveAt: at, Reason: "test"}
}

func TestChangeManager_KeepsPreviousManagerInHistory(t *testing.T) {
	pool := openOrgUnitPool(t)
	s := store.New(pool)
	ctx := orgCtx("tenant-a")

	oldM, newM := uuid.NewString(), uuid.NewString()
	u := newUnitWithManager(t, s, ctx, "e1", "FIN", oldM)

	got, err := s.ChangeOrgUnitManager(ctx, u.UnitID, changeTo(newM, "2026-03-01"), 1, "actor")
	if err != nil || got.RecordVersion != 2 {
		t.Fatalf("change: %+v err=%v", got, err)
	}
	if m := managerAt(t, s, ctx, u.UnitID, "2026-02-28"); m != oldM {
		t.Fatalf("before the change the old manager applies, got %q", m)
	}
	if m := managerAt(t, s, ctx, u.UnitID, "2026-03-01"); m != newM {
		t.Fatalf("from the effective date the new manager applies, got %q", m)
	}

	var refs, closed int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*), COUNT(valid_to) FROM org_unit_manager_refs WHERE unit_id = $1`, u.UnitID).Scan(&refs, &closed); err != nil {
		t.Fatal(err)
	}
	if refs != 2 || closed != 1 {
		t.Fatalf("want 2 manager rows with the old one closed, got %d / %d closed", refs, closed)
	}
}

func TestChangeManager_FirstManagerOnUnitWithNone(t *testing.T) {
	s := store.New(openOrgUnitPool(t))
	ctx := orgCtx("tenant-a")

	m := uuid.NewString()
	u := newUnitWithManager(t, s, ctx, "e1", "FIN", "")
	if _, err := s.ChangeOrgUnitManager(ctx, u.UnitID, changeTo(m, "2026-06-01"), 1, "a"); err != nil {
		t.Fatalf("first manager: %v", err)
	}
	if got := managerAt(t, s, ctx, u.UnitID, "2026-06-01"); got != m {
		t.Fatalf("want %s, got %q", m, got)
	}
	if got := managerAt(t, s, ctx, u.UnitID, "2026-05-31"); got != "" {
		t.Fatalf("no manager before the first appointment, got %q", got)
	}
}

func TestChangeManager_RefusalsLeaveNoTrace(t *testing.T) {
	pool := openOrgUnitPool(t)
	s := store.New(pool)
	ctx := orgCtx("tenant-a")

	m1, m2 := uuid.NewString(), uuid.NewString()
	u := newUnitWithManager(t, s, ctx, "e1", "FIN", m1)

	cases := []struct {
		name string
		req  *domain.ChangeManagerRequest
		ver  int64
		want error
	}{
		{"same manager", changeTo(m1, "2026-03-01"), 1, domain.ErrNoManagerChange},
		{"effective on the manager's start", changeTo(m2, "2026-01-01"), 1, domain.ErrEffectiveDateInvalid},
		{"effective before the manager's start", changeTo(m2, "2025-01-01"), 1, domain.ErrEffectiveDateInvalid},
		{"stale version", changeTo(m2, "2026-03-01"), 7, domain.ErrVersionConflict},
		{"not an employee id", changeTo("not-a-uuid", "2026-03-01"), 1, domain.ErrManagerNotFound},
	}
	for _, c := range cases {
		if _, err := s.ChangeOrgUnitManager(ctx, u.UnitID, c.req, c.ver, "x"); !errors.Is(err, c.want) {
			t.Errorf("%s: want %v, got %v", c.name, c.want, err)
		}
	}
	if _, err := s.ChangeOrgUnitManager(ctx, uuid.NewString(), changeTo(m2, "2026-03-01"), 1, "x"); !errors.Is(err, domain.ErrOrgUnitNotFound) {
		t.Errorf("unknown unit: want ErrOrgUnitNotFound, got %v", err)
	}

	var n int
	if err := pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM org_unit_manager_refs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("refusals must add no manager rows: want 1, got %d", n)
	}
	if got, _ := s.GetOrgUnit(ctx, u.UnitID, ""); got.RecordVersion != 1 {
		t.Fatalf("version must stay 1, got %d", got.RecordVersion)
	}
}

func TestChangeManager_InactiveUnitRefused(t *testing.T) {
	s := store.New(openOrgUnitPool(t))
	ctx := orgCtx("tenant-a")

	u := newUnitWithManager(t, s, ctx, "e1", "FIN", uuid.NewString())
	if _, err := s.TransitionOrgUnit(ctx, u.UnitID, domain.UnitCommandActivate, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionOrgUnit(ctx, u.UnitID, domain.UnitCommandDeactivate, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ChangeOrgUnitManager(ctx, u.UnitID, changeTo(uuid.NewString(), "2026-03-01"), 3, "x"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("want ErrInvalidTransition, got %v", err)
	}
}

// Two callers who both read version 1 race to change the manager: exactly one
// wins; the other gets a version conflict rather than silently overwriting.
func TestChangeManager_ConcurrentChangesDoNotOverwrite(t *testing.T) {
	s := store.New(openOrgUnitPool(t))
	ctx := orgCtx("tenant-a")

	for round := 0; round < 6; round++ {
		u := newUnitWithManager(t, s, ctx, "e1", "U"+string(rune('0'+round)), uuid.NewString())

		var wg sync.WaitGroup
		errs := make([]error, 2)
		start := make(chan struct{})
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, errs[i] = s.ChangeOrgUnitManager(ctx, u.UnitID, changeTo(uuid.NewString(), "2026-03-01"), 1, "x")
			}(i)
		}
		close(start)
		wg.Wait()

		ok, conflicts := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, domain.ErrVersionConflict):
				conflicts++
			default:
				t.Fatalf("round %d: unexpected error %v", round, err)
			}
		}
		if ok != 1 || conflicts != 1 {
			t.Fatalf("round %d: want 1 success and 1 conflict, got %d / %d (%v)", round, ok, conflicts, errs)
		}
	}
}

func TestFindUnitsByManager_AsOfTenantAndInactive(t *testing.T) {
	s := store.New(openOrgUnitPool(t))
	ctx := orgCtx("tenant-a")

	m, other := uuid.NewString(), uuid.NewString()
	fin := newUnitWithManager(t, s, ctx, "e1", "FIN", m)
	ops := newUnitWithManager(t, s, ctx, "e2", "OPS", m)
	newUnitWithManager(t, s, ctx, "e1", "HR", other)

	list, err := s.FindUnitsByManager(ctx, m, "")
	if err != nil || len(list) != 2 {
		t.Fatalf("both units: len=%d err=%v", len(list), err)
	}
	if list[0].LegalEntityID != "e1" || list[1].LegalEntityID != "e2" {
		t.Fatalf("ordered by entity then code, got %s then %s", list[0].LegalEntityID, list[1].LegalEntityID)
	}

	// FIN is handed over from March; before that m still manages it.
	if _, err := s.ChangeOrgUnitManager(ctx, fin.UnitID, changeTo(other, "2026-03-01"), 1, "x"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.FindUnitsByManager(ctx, m, "2026-02-01"); len(got) != 2 {
		t.Fatalf("before the handover m manages both, got %d", len(got))
	}
	if got, _ := s.FindUnitsByManager(ctx, m, "2026-03-01"); len(got) != 1 || got[0].UnitID != ops.UnitID {
		t.Fatalf("from the handover m manages only OPS, got %+v", got)
	}

	// A retired unit has nothing to manage.
	if _, err := s.TransitionOrgUnit(ctx, ops.UnitID, domain.UnitCommandActivate, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionOrgUnit(ctx, ops.UnitID, domain.UnitCommandDeactivate, 2); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.FindUnitsByManager(ctx, m, "2026-03-01"); len(got) != 0 {
		t.Fatalf("inactive units are left out, got %d", len(got))
	}

	// Another tenant sees nothing, and a malformed id is simply "manages nothing".
	if got, err := s.FindUnitsByManager(orgCtx("tenant-b"), other, ""); err != nil || len(got) != 0 {
		t.Fatalf("tenant-b: len=%d err=%v", len(got), err)
	}
	if got, err := s.FindUnitsByManager(ctx, "not-a-uuid", ""); err != nil || len(got) != 0 {
		t.Fatalf("malformed id: len=%d err=%v", len(got), err)
	}
}
