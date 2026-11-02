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

func mkMapped(t *testing.T, s *store.PgStore, ctx context.Context, entity, code string, parent *string, costCenter, profitCenter string) *domain.OrgUnit {
	t.Helper()
	req := newUnit(entity, code)
	req.ParentUnitID = parent
	req.CostCenterCode, req.ProfitCenterCode = costCenter, profitCenter
	u, _, err := s.CreateOrgUnit(ctx, req, "a")
	if err != nil {
		t.Fatalf("create %s: %v", code, err)
	}
	return u
}

func propose(t *testing.T, s *store.PgStore, ctx context.Context, unit *domain.OrgUnit, to *string, at string) (*domain.ChangeRequest, bool, error) {
	t.Helper()
	req := moveTo(to, at)
	im, err := s.ReparentImpact(ctx, unit.UnitID, at)
	if err != nil {
		t.Fatalf("impact: %v", err)
	}
	return s.CreateReparentRequest(ctx, unit.UnitID, req, unit.RecordVersion, "maker", *im)
}

func TestImpact_ReachesTheSubtreeAndItsMappings(t *testing.T) {
	s := store.New(openOrgUnitPool(t))
	ctx := orgCtx("tenant-a")

	root := mkMapped(t, s, ctx, "e1", "ROOT", nil, "", "")
	fin := mkMapped(t, s, ctx, "e1", "FIN", &root.UnitID, "CC1", "")
	sub := mkMapped(t, s, ctx, "e1", "SUB", &fin.UnitID, "", "PC1")
	plain := mkMapped(t, s, ctx, "e1", "PLAIN", &root.UnitID, "", "")
	other := mkMapped(t, s, ctx, "e1", "OTHER", nil, "CC9", "")
	_, _, _ = sub, plain, other

	// A unit can only be activated under an active parent, so ROOT goes first.
	if _, err := s.TransitionOrgUnit(ctx, root.UnitID, domain.UnitCommandActivate, 1); err != nil {
		t.Fatal(err)
	}
	old := mkMapped(t, s, ctx, "e1", "OLD", &root.UnitID, "CC7", "")
	if _, err := s.TransitionOrgUnit(ctx, old.UnitID, domain.UnitCommandActivate, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionOrgUnit(ctx, old.UnitID, domain.UnitCommandDeactivate, 2); err != nil {
		t.Fatal(err)
	}

	im, err := s.ReparentImpact(ctx, root.UnitID, "2026-03-01")
	if err != nil {
		t.Fatalf("impact: %v", err)
	}
	if im.UnitsAffected != 4 {
		t.Fatalf("ROOT, FIN, SUB and PLAIN are reached (the retired OLD is not), got %d", im.UnitsAffected)
	}
	if len(im.MappedUnits) != 2 || im.MappedUnits[0].Code != "FIN" || im.MappedUnits[0].CostCenterCode != "CC1" ||
		im.MappedUnits[1].Code != "SUB" || im.MappedUnits[1].ProfitCenterCode != "PC1" {
		t.Fatalf("mapped units must be FIN and SUB, ordered by code, without the retired OLD: %+v", im.MappedUnits)
	}
	if !im.RequiresApproval {
		t.Fatal("mapped units below the unit require approval")
	}

	if im, _ := s.ReparentImpact(ctx, plain.UnitID, "2026-03-01"); im.RequiresApproval || im.UnitsAffected != 1 {
		t.Fatalf("PLAIN has nothing mapped beneath it: %+v", im)
	}
	if _, err := s.ReparentImpact(ctx, uuid.NewString(), "2026-03-01"); !errors.Is(err, domain.ErrOrgUnitNotFound) {
		t.Fatalf("unknown unit: want ErrOrgUnitNotFound, got %v", err)
	}
	if _, err := s.ReparentImpact(ctx, "not-a-uuid", "2026-03-01"); !errors.Is(err, domain.ErrOrgUnitNotFound) {
		t.Fatalf("malformed id: want ErrOrgUnitNotFound, got %v", err)
	}
}

// The impact is as of the effective date: a unit not yet placed that day is not reached.
func TestImpact_UsesTheEffectiveDate(t *testing.T) {
	s := store.New(openOrgUnitPool(t))
	ctx := orgCtx("tenant-a")

	root := mkMapped(t, s, ctx, "e1", "ROOT", nil, "", "")
	late := newUnit("e1", "LATE")
	late.ParentUnitID, late.CostCenterCode, late.EffectiveFrom = &root.UnitID, "CC1", "2026-06-01"
	if _, _, err := s.CreateOrgUnit(ctx, late, "a"); err != nil {
		t.Fatal(err)
	}

	if im, _ := s.ReparentImpact(ctx, root.UnitID, "2026-05-01"); im.RequiresApproval || im.UnitsAffected != 1 {
		t.Fatalf("before LATE is placed, moving ROOT reaches nothing mapped: %+v", im)
	}
	if im, _ := s.ReparentImpact(ctx, root.UnitID, "2026-06-01"); !im.RequiresApproval || im.UnitsAffected != 2 {
		t.Fatalf("from LATE's start it is reached: %+v", im)
	}
}

// The proposal runs the move for real and rolls it back, so it is checked
// thoroughly yet leaves nothing behind except the request.
func TestCreateRequest_DryRunLeavesNoTrace(t *testing.T) {
	pool := openOrgUnitPool(t)
	s := store.New(pool)
	ctx := orgCtx("tenant-a")

	a := mkMapped(t, s, ctx, "e1", "A", nil, "", "")
	b := mkMapped(t, s, ctx, "e1", "B", nil, "", "")
	fin := mkMapped(t, s, ctx, "e1", "FIN", &a.UnitID, "CC1", "")

	var before int
	_ = pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM org_unit_hierarchy_versions`).Scan(&before)

	cr, created, err := propose(t, s, ctx, fin, &b.UnitID, "2026-03-01")
	if err != nil || !created || cr.Status != domain.ChangePending {
		t.Fatalf("propose: %+v created=%v err=%v", cr, created, err)
	}
	if cr.UnitVersion != 1 || cr.RequestedBy != "maker" || !cr.Impact.RequiresApproval || len(cr.Impact.MappedUnits) != 1 {
		t.Fatalf("the request must carry the version, maker and impact: %+v", cr)
	}

	var after int
	_ = pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM org_unit_hierarchy_versions`).Scan(&after)
	got, _ := s.GetOrgUnit(ctx, fin.UnitID, "")
	if after != before || got.RecordVersion != 1 || *got.ParentUnitID != a.UnitID {
		t.Fatalf("the dry run must leave no placement and no version bump: rows %d->%d v%d", before, after, got.RecordVersion)
	}
}

func TestCreateRequest_ImpossibleMovesAreRefusedWithoutARequest(t *testing.T) {
	s := store.New(openOrgUnitPool(t))
	ctx := orgCtx("tenant-a")

	root := mkMapped(t, s, ctx, "e1", "ROOT", nil, "", "")
	fin := mkMapped(t, s, ctx, "e1", "FIN", &root.UnitID, "CC1", "")

	if _, _, err := propose(t, s, ctx, root, &fin.UnitID, "2026-03-01"); !errors.Is(err, domain.ErrHierarchyLoop) {
		t.Fatalf("loop: want ErrHierarchyLoop, got %v", err)
	}
	if _, _, err := propose(t, s, ctx, fin, &root.UnitID, "2026-03-01"); !errors.Is(err, domain.ErrNoHierarchyChange) {
		t.Fatalf("same parent: want ErrNoHierarchyChange, got %v", err)
	}
	stale := *fin
	stale.RecordVersion = 9
	if _, _, err := propose(t, s, ctx, &stale, nil, "2026-03-01"); !errors.Is(err, domain.ErrVersionConflict) {
		t.Fatalf("stale: want ErrVersionConflict, got %v", err)
	}
	if n := requestCount(t, s, ctx); n != 0 {
		t.Fatalf("refused proposals leave no request, got %d", n)
	}
}

func TestCreateRequest_OnePendingChangePerUnit(t *testing.T) {
	s := store.New(openOrgUnitPool(t))
	ctx := orgCtx("tenant-a")

	a := mkMapped(t, s, ctx, "e1", "A", nil, "", "")
	b := mkMapped(t, s, ctx, "e1", "B", nil, "", "")
	fin := mkMapped(t, s, ctx, "e1", "FIN", &a.UnitID, "CC1", "")

	first, created, err := propose(t, s, ctx, fin, &b.UnitID, "2026-03-01")
	if err != nil || !created {
		t.Fatalf("first: created=%v err=%v", created, err)
	}
	again, created, err := propose(t, s, ctx, fin, &b.UnitID, "2026-03-01")
	if err != nil || created || again.RequestID != first.RequestID {
		t.Fatalf("the same move returns the pending request: created=%v err=%v", created, err)
	}
	if _, _, err := propose(t, s, ctx, fin, &b.UnitID, "2026-04-01"); !errors.Is(err, domain.ErrChangePending) {
		t.Fatalf("a different move for the same unit: want ErrChangePending, got %v", err)
	}

	if _, err := s.RejectChangeRequest(ctx, first.RequestID, "checker", "no"); err != nil {
		t.Fatal(err)
	}
	if _, created, err := propose(t, s, ctx, fin, &b.UnitID, "2026-04-01"); err != nil || !created {
		t.Fatalf("a closed request frees the unit: created=%v err=%v", created, err)
	}
}

func TestApprove_AppliesTheMoveAndKeepsTheEvidence(t *testing.T) {
	pool := openOrgUnitPool(t)
	s := store.New(pool)
	ctx := orgCtx("tenant-a")

	a := mkMapped(t, s, ctx, "e1", "A", nil, "", "")
	b := mkMapped(t, s, ctx, "e1", "B", nil, "", "")
	fin := mkMapped(t, s, ctx, "e1", "FIN", &a.UnitID, "CC1", "")
	cr, _, err := propose(t, s, ctx, fin, &b.UnitID, "2026-03-01")
	if err != nil {
		t.Fatal(err)
	}

	decided, unit, err := s.ApproveChangeRequest(ctx, cr.RequestID, "checker")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if decided.Status != domain.ChangeApproved || *decided.DecidedBy != "checker" || decided.DecidedAt == nil {
		t.Fatalf("approval must record who and when: %+v", decided)
	}
	if *unit.ParentUnitID != b.UnitID || unit.RecordVersion != 2 {
		t.Fatalf("FIN must now sit under B at v2: %+v", unit)
	}
	if got := parentAt(t, s, ctx, fin.UnitID, "2026-02-28"); got != a.UnitID {
		t.Fatalf("the past tree is intact, got %q", got)
	}

	// The new placement is attributed to the maker and linked to the approval.
	var actor, approvalRef string
	if err := pool.QueryRow(context.Background(), `
		SELECT actor_id, approval_ref FROM org_unit_hierarchy_versions
		WHERE unit_id = $1 AND valid_to IS NULL`, fin.UnitID).Scan(&actor, &approvalRef); err != nil {
		t.Fatal(err)
	}
	if actor != "maker" || approvalRef != cr.RequestID {
		t.Fatalf("placement evidence: actor=%q approval_ref=%q (want maker / %s)", actor, approvalRef, cr.RequestID)
	}
}

func TestApprove_SelfApprovalRefusedByServiceAndDatabase(t *testing.T) {
	pool := openOrgUnitPool(t)
	s := store.New(pool)
	ctx := orgCtx("tenant-a")

	a := mkMapped(t, s, ctx, "e1", "A", nil, "", "")
	b := mkMapped(t, s, ctx, "e1", "B", nil, "", "")
	fin := mkMapped(t, s, ctx, "e1", "FIN", &a.UnitID, "CC1", "")
	cr, _, _ := propose(t, s, ctx, fin, &b.UnitID, "2026-03-01")

	if _, _, err := s.ApproveChangeRequest(ctx, cr.RequestID, "maker"); !errors.Is(err, domain.ErrSelfApproval) {
		t.Fatalf("want ErrSelfApproval, got %v", err)
	}
	if got, _ := s.GetChangeRequest(ctx, cr.RequestID); got.Status != domain.ChangePending {
		t.Fatalf("a refused self-approval leaves the request pending, got %s", got.Status)
	}

	// Even a writer that bypasses the service cannot record a self-approval.
	_, err := pool.Exec(context.Background(), `
		UPDATE org_unit_change_requests SET status = 'APPROVED', decided_by = requested_by, decided_at = now()
		WHERE request_id = $1`, cr.RequestID)
	if err == nil {
		t.Fatal("the database must refuse a request approved by its own maker")
	}
	// ... and a decision must name its decider.
	if _, err := pool.Exec(context.Background(), `
		UPDATE org_unit_change_requests SET status = 'REJECTED' WHERE request_id = $1`, cr.RequestID); err == nil {
		t.Fatal("a decided request must record decided_by and decided_at")
	}
}

// A proposal that can no longer be applied is closed as FAILED, durably, and
// the structure is untouched: approval cannot force it through.
func TestApprove_StaleProposalIsClosedAsFailed(t *testing.T) {
	s := store.New(openOrgUnitPool(t))
	ctx := orgCtx("tenant-a")

	a := mkMapped(t, s, ctx, "e1", "A", nil, "", "")
	b := mkMapped(t, s, ctx, "e1", "B", nil, "", "")
	fin := mkMapped(t, s, ctx, "e1", "FIN", &a.UnitID, "CC1", "")

	// 1. The unit changed after the proposal (version moved on).
	cr, _, _ := propose(t, s, ctx, fin, &b.UnitID, "2026-03-01")
	if _, err := s.ChangeOrgUnitManager(ctx, fin.UnitID, changeTo(uuid.NewString(), "2026-02-01"), 1, "x"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ApproveChangeRequest(ctx, cr.RequestID, "checker"); !errors.Is(err, domain.ErrVersionConflict) {
		t.Fatalf("want ErrVersionConflict, got %v", err)
	}
	got, _ := s.GetChangeRequest(ctx, cr.RequestID)
	if got.Status != domain.ChangeFailed || got.FailureCode != "version_conflict" || got.DecidedBy == nil || *got.DecidedBy != "checker" {
		t.Fatalf("the request must persist as FAILED with its reason: %+v", got)
	}
	if u, _ := s.GetOrgUnit(ctx, fin.UnitID, ""); *u.ParentUnitID != a.UnitID {
		t.Fatal("a failed approval must not move the unit")
	}
	if _, _, err := s.ApproveChangeRequest(ctx, cr.RequestID, "checker"); !errors.Is(err, domain.ErrChangeRequestDecided) {
		t.Fatalf("a failed request cannot be approved: got %v", err)
	}

	// 2. Something else made the move a loop: X is proposed under Y, then Y is
	// moved under X directly.
	x := mkMapped(t, s, ctx, "e1", "X", nil, "CC2", "")
	y := mkMapped(t, s, ctx, "e1", "Y", nil, "", "")
	cr2, _, err := propose(t, s, ctx, x, &y.UnitID, "2026-03-01")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReparentOrgUnit(ctx, y.UnitID, moveTo(&x.UnitID, "2026-03-01"), 1, "x"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ApproveChangeRequest(ctx, cr2.RequestID, "checker"); !errors.Is(err, domain.ErrHierarchyLoop) {
		t.Fatalf("want ErrHierarchyLoop, got %v", err)
	}
	if got, _ := s.GetChangeRequest(ctx, cr2.RequestID); got.Status != domain.ChangeFailed || got.FailureCode != "hierarchy_loop" {
		t.Fatalf("closed as FAILED(hierarchy_loop), got %+v", got)
	}
}

// Two approvers race on one request: exactly one applies it.
func TestApprove_ConcurrentApproversCannotBothApply(t *testing.T) {
	s := store.New(openOrgUnitPool(t))
	ctx := orgCtx("tenant-a")

	for round := 0; round < 6; round++ {
		a := mkMapped(t, s, ctx, "e1", "A"+string(rune('0'+round)), nil, "", "")
		b := mkMapped(t, s, ctx, "e1", "B"+string(rune('0'+round)), nil, "", "")
		fin := mkMapped(t, s, ctx, "e1", "F"+string(rune('0'+round)), &a.UnitID, "CC1", "")
		cr, _, err := propose(t, s, ctx, fin, &b.UnitID, "2026-03-01")
		if err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		errs := make([]error, 2)
		start := make(chan struct{})
		for i, who := range []string{"checker-1", "checker-2"} {
			wg.Add(1)
			go func(i int, who string) {
				defer wg.Done()
				<-start
				_, _, errs[i] = s.ApproveChangeRequest(ctx, cr.RequestID, who)
			}(i, who)
		}
		close(start)
		wg.Wait()

		ok, decided := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, domain.ErrChangeRequestDecided):
				decided++
			default:
				t.Fatalf("round %d: unexpected error %v", round, err)
			}
		}
		if ok != 1 || decided != 1 {
			t.Fatalf("round %d: want 1 approval and 1 refusal, got %d / %d (%v)", round, ok, decided, errs)
		}
		if u, _ := s.GetOrgUnit(ctx, fin.UnitID, ""); u.RecordVersion != 2 {
			t.Fatalf("round %d: the move must be applied exactly once, version %d", round, u.RecordVersion)
		}
	}
}

func TestChangeRequests_ListRejectAndIsolation(t *testing.T) {
	s := store.New(openOrgUnitPool(t))
	ctx := orgCtx("tenant-a")

	a := mkMapped(t, s, ctx, "e1", "A", nil, "", "")
	b := mkMapped(t, s, ctx, "e1", "B", nil, "", "")
	fin := mkMapped(t, s, ctx, "e1", "FIN", &a.UnitID, "CC1", "")
	cr, _, _ := propose(t, s, ctx, fin, &b.UnitID, "2026-03-01")

	rej, err := s.RejectChangeRequest(ctx, cr.RequestID, "checker", "wrong cost centre")
	if err != nil || rej.Status != domain.ChangeRejected || rej.DecisionReason != "wrong cost centre" {
		t.Fatalf("reject: %+v err=%v", rej, err)
	}
	if _, err := s.RejectChangeRequest(ctx, cr.RequestID, "checker", "again"); !errors.Is(err, domain.ErrChangeRequestDecided) {
		t.Fatalf("a closed request cannot be rejected again: got %v", err)
	}
	if u, _ := s.GetOrgUnit(ctx, fin.UnitID, ""); *u.ParentUnitID != a.UnitID || u.RecordVersion != 1 {
		t.Fatal("a rejected request must change nothing")
	}

	all, _ := s.ListChangeRequests(ctx, "e1", "")
	rejected, _ := s.ListChangeRequests(ctx, "e1", domain.ChangeRejected)
	pending, _ := s.ListChangeRequests(ctx, "e1", domain.ChangePending)
	otherEntity, _ := s.ListChangeRequests(ctx, "e2", "")
	if len(all) != 1 || len(rejected) != 1 || len(pending) != 0 || len(otherEntity) != 0 {
		t.Fatalf("filters: all=%d rejected=%d pending=%d other-entity=%d", len(all), len(rejected), len(pending), len(otherEntity))
	}

	if _, err := s.GetChangeRequest(orgCtx("tenant-b"), cr.RequestID); !errors.Is(err, domain.ErrChangeRequestNotFound) {
		t.Fatalf("another tenant must not see the request, got %v", err)
	}
	if _, err := s.GetChangeRequest(ctx, "not-a-uuid"); !errors.Is(err, domain.ErrChangeRequestNotFound) {
		t.Fatalf("malformed id: want not found, got %v", err)
	}
	if _, _, err := s.ApproveChangeRequest(ctx, uuid.NewString(), "checker"); !errors.Is(err, domain.ErrChangeRequestNotFound) {
		t.Fatalf("unknown request: want not found, got %v", err)
	}
}

// requestCount is how many change requests entity e1 has, in any status.
func requestCount(t *testing.T, s *store.PgStore, ctx context.Context) int {
	t.Helper()
	list, err := s.ListChangeRequests(ctx, "e1", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return len(list)
}
