package store_test

// Real-Postgres coverage for AST-03 book re-base (IMPAIRMENT / ADDITION /
// REVALUATION) — see internal/store/asset_event_rebase.go.

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/asset-management-svc/internal/domain"
	svcmiddleware "zoiko.io/asset-management-svc/internal/middleware"
	"zoiko.io/asset-management-svc/internal/store"
)

type rebaseEnv struct {
	t        *testing.T
	pool     *pgxpool.Pool
	s        *store.PgStore
	ctx      context.Context
	tenantID string
	asset    *domain.FixedAsset
	sch      *domain.DepreciationSchedule
	clock    time.Time
	periodNo int
}

// newRebaseEnv builds an ACTIVE asset with a 12000 / 12-month / residual-0
// schedule on book-1 (1000 per month).
func newRebaseEnv(t *testing.T) *rebaseEnv {
	pool := openTestPool(t)
	s := store.New(pool)
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	a := activeAssetInStore(t, ctx, s, tenantID, "le-1")
	sch := newTestSchedule(tenantID, a)
	if err := s.CreateDepreciationSchedule(ctx, sch); err != nil {
		t.Fatalf("CreateDepreciationSchedule: %v", err)
	}
	return &rebaseEnv{t: t, pool: pool, s: s, ctx: ctx, tenantID: tenantID, asset: a, sch: sch, clock: time.Now().UTC()}
}

func (e *rebaseEnv) tick() time.Time {
	e.clock = e.clock.Add(time.Minute)
	return e.clock
}

// runPeriod runs one depreciation period through Freeze+Validate and, when
// finish is true, Approve+Emit (so the run is no longer "in flight").
func (e *rebaseEnv) runPeriod(finish bool) string {
	e.t.Helper()
	e.periodNo++
	run := &domain.DepreciationRun{
		RunID: uuid.New().String(), LegalEntityID: "le-1", FiscalPeriod: time.Date(2026, time.Month(e.periodNo), 1, 0, 0, 0, 0, time.UTC).Format("2006-01"),
		DepreciationExpenseAccountCode: "6400-Depr", AccumulatedDepreciationAccountCode: "1590-AccumDepr",
		Status: domain.DepreciationRunStatusDraft, CreatedAt: e.tick(), CreatedByPrincipalID: "preparer-1",
	}
	if err := e.s.CreateDepreciationRun(e.ctx, run); err != nil {
		e.t.Fatalf("CreateDepreciationRun: %v", err)
	}
	if _, err := e.s.FreezeDepreciationPopulation(e.ctx, run.RunID, "le-1", e.tick()); err != nil {
		e.t.Fatalf("Freeze: %v", err)
	}
	if _, err := e.s.ValidateDepreciationRun(e.ctx, run.RunID, e.tick()); err != nil {
		e.t.Fatalf("Validate: %v", err)
	}
	if finish {
		if err := e.s.ApproveDepreciationRun(e.ctx, run.RunID, "approver-1", e.tick()); err != nil {
			e.t.Fatalf("Approve: %v", err)
		}
		if err := e.s.MarkDepreciationRunEmitted(e.ctx, run.RunID, "j-"+run.RunID, e.tick()); err != nil {
			e.t.Fatalf("Emit: %v", err)
		}
	}
	return run.RunID
}

func (e *rebaseEnv) newEvent(eventType, book string, amount float64) *domain.AssetEvent {
	ev := newTestAssetEvent(e.tenantID, e.asset, eventType)
	if book != "" {
		ev.BookID = &book
	}
	ev.Amount = &amount
	if err := e.s.CreateAssetEvent(e.ctx, ev); err != nil {
		e.t.Fatalf("CreateAssetEvent: %v", err)
	}
	if err := e.s.ValidateAssetEvent(e.ctx, ev.EventID, e.tick()); err != nil {
		e.t.Fatalf("Validate: %v", err)
	}
	if err := e.s.ApproveAssetEvent(e.ctx, ev.EventID, "approver-1", e.tick()); err != nil {
		e.t.Fatalf("Approve: %v", err)
	}
	return ev
}

// apply applies with a journal id (-> ACCOUNTING_EVENT_EMITTED, reversible).
func (e *rebaseEnv) apply(ev *domain.AssetEvent) error {
	j := "journal-" + ev.EventID
	return e.s.ApplyAssetEvent(e.ctx, ev.EventID, e.tick(), &j)
}

func (e *rebaseEnv) current() *domain.DepreciationSchedule {
	e.t.Helper()
	c, err := e.s.GetCurrentDepreciationSchedule(e.ctx, e.sch.ScheduleID)
	if err != nil {
		e.t.Fatalf("GetCurrentDepreciationSchedule: %v", err)
	}
	return c
}

func (e *rebaseEnv) nbv() float64 {
	e.t.Helper()
	v, err := e.s.GetNetBookValueTotal(e.ctx, "le-1", "book-1")
	if err != nil {
		e.t.Fatalf("GetNetBookValueTotal: %v", err)
	}
	return v
}

func near(a, b float64) bool { return math.Abs(a-b) < 0.005 }

func (e *rebaseEnv) expectSchedule(label string, cost float64, life, version int) {
	e.t.Helper()
	c := e.current()
	if !near(c.CostBasis, cost) || c.UsefulLifeMonths != life || c.Version != version {
		e.t.Fatalf("%s: expected cost=%v life=%d version=%d, got cost=%v life=%d version=%d",
			label, cost, life, version, c.CostBasis, c.UsefulLifeMonths, c.Version)
	}
}

func (e *rebaseEnv) effectCount(eventID string) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM asset_event_schedule_effects WHERE event_id = $1`, eventID).Scan(&n); err != nil {
		e.t.Fatalf("count effects: %v", err)
	}
	return n
}

func TestRebase_Impairment_ArithmeticAndOldVersionUntouched(t *testing.T) {
	e := newRebaseEnv(t)
	e.runPeriod(true)
	e.runPeriod(true)
	e.runPeriod(true) // 3000 depreciated: carrying 9000, remaining 9
	if !near(e.nbv(), 9000) {
		t.Fatalf("pre-impairment NBV expected 9000, got %v", e.nbv())
	}
	oldVersionID := e.current().ScheduleVersionID

	ev := e.newEvent(domain.AssetEventTypeImpairment, "book-1", 1800)
	if err := e.apply(ev); err != nil {
		t.Fatalf("apply impairment: %v", err)
	}

	e.expectSchedule("after impairment", 7200, 9, 2)
	if !near(e.nbv(), 7200) {
		t.Fatalf("NBV after impairment expected 7200, got %v", e.nbv())
	}
	if e.effectCount(ev.EventID) != 1 {
		t.Fatalf("expected 1 effects row")
	}

	// Old version: superseded, end-dated, and its 3 lines untouched.
	var status string
	var endDated bool
	var cost float64
	if err := e.pool.QueryRow(context.Background(), `SELECT status, effective_to IS NOT NULL, cost_basis FROM depreciation_schedules WHERE schedule_version_id = $1`, oldVersionID).Scan(&status, &endDated, &cost); err != nil {
		t.Fatal(err)
	}
	if status != domain.DepreciationScheduleStatusSuperseded || !endDated || !near(cost, 12000) {
		t.Fatalf("old version not preserved: status=%s endDated=%v cost=%v", status, endDated, cost)
	}
	var lines int
	var sum float64
	if err := e.pool.QueryRow(context.Background(), `SELECT COUNT(*), COALESCE(SUM(period_amount),0) FROM depreciation_lines WHERE schedule_version_id = $1`, oldVersionID).Scan(&lines, &sum); err != nil {
		t.Fatal(err)
	}
	if lines != 3 || !near(sum, 3000) {
		t.Fatalf("old version lines changed: %d lines, sum %v", lines, sum)
	}

	// Next period depreciates the NEW version: 7200/9 = 800.
	e.runPeriod(true)
	if !near(e.nbv(), 6400) {
		t.Fatalf("NBV after next run expected 6400, got %v", e.nbv())
	}
}

func TestRebase_Addition(t *testing.T) {
	e := newRebaseEnv(t)
	e.runPeriod(true)
	e.runPeriod(true)
	e.runPeriod(true)
	ev := e.newEvent(domain.AssetEventTypeAddition, "book-1", 3000)
	if err := e.apply(ev); err != nil {
		t.Fatalf("apply addition: %v", err)
	}
	e.expectSchedule("after addition", 12000, 9, 2)
	if !near(e.nbv(), 12000) {
		t.Fatalf("NBV expected 12000, got %v", e.nbv())
	}
}

func TestRebase_Revaluation_AmountIsRevaluedCarrying(t *testing.T) {
	e := newRebaseEnv(t)
	e.runPeriod(true)
	e.runPeriod(true)
	e.runPeriod(true)
	ev := e.newEvent(domain.AssetEventTypeRevaluation, "book-1", 10000)
	if err := e.apply(ev); err != nil {
		t.Fatalf("apply revaluation: %v", err)
	}
	e.expectSchedule("after revaluation", 10000, 9, 2)
	if !near(e.nbv(), 10000) {
		t.Fatalf("NBV expected 10000, got %v", e.nbv())
	}
}

func TestRebase_Impairment_AtOrAboveCarrying_Refused(t *testing.T) {
	e := newRebaseEnv(t)
	e.runPeriod(true)
	e.runPeriod(true)
	e.runPeriod(true) // carrying 9000
	for _, amt := range []float64{9000, 9500} {
		ev := e.newEvent(domain.AssetEventTypeImpairment, "book-1", amt)
		err := e.apply(ev)
		if !errors.Is(err, domain.ErrImpairmentExceedsCarrying) {
			t.Fatalf("amount %v: expected ErrImpairmentExceedsCarrying, got %v", amt, err)
		}
		got, _ := e.s.GetAssetEvent(e.ctx, ev.EventID)
		if got.Status != domain.AssetEventStatusApproved {
			t.Fatalf("refused event must stay APPROVED, got %s", got.Status)
		}
	}
	e.expectSchedule("unchanged", 12000, 12, 1)
}

func TestRebase_FullyDepreciated_Refused(t *testing.T) {
	e := newRebaseEnv(t)
	for i := 0; i < 12; i++ {
		e.runPeriod(true)
	}
	ev := e.newEvent(domain.AssetEventTypeImpairment, "book-1", 1)
	if err := e.apply(ev); !errors.Is(err, domain.ErrScheduleFullyDepreciated) {
		t.Fatalf("expected ErrScheduleFullyDepreciated, got %v", err)
	}
}

func TestRebase_RunInFlight_Refused_ThenAllowedAfterEmit(t *testing.T) {
	e := newRebaseEnv(t)
	e.runPeriod(true)
	e.runPeriod(false) // VALIDATED, holds the current schedule version
	ev := e.newEvent(domain.AssetEventTypeImpairment, "book-1", 500)
	if err := e.apply(ev); !errors.Is(err, domain.ErrDepreciationRunInFlight) {
		t.Fatalf("expected ErrDepreciationRunInFlight, got %v", err)
	}
	e.expectSchedule("unchanged while in flight", 12000, 12, 1)
	got, _ := e.s.GetAssetEvent(e.ctx, ev.EventID)
	if got.Status != domain.AssetEventStatusApproved {
		t.Fatalf("refused event must stay APPROVED, got %s", got.Status)
	}

	// Approve + emit the in-flight run (period 2) -> re-base allowed.
	var runID string
	if err := e.pool.QueryRow(context.Background(), `SELECT run_id FROM depreciation_runs WHERE tenant_id = $1 AND status = 'VALIDATED'`, e.tenantID).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	if err := e.s.ApproveDepreciationRun(e.ctx, runID, "approver-1", e.tick()); err != nil {
		t.Fatal(err)
	}
	if err := e.s.MarkDepreciationRunEmitted(e.ctx, runID, "j", e.tick()); err != nil {
		t.Fatal(err)
	}
	if err := e.apply(ev); err != nil {
		t.Fatalf("apply after emit: %v", err)
	}
	e.expectSchedule("after emit", 10000-500, 10, 2) // carrying 12000-2000-500, remaining 10
}

func TestRebase_NoScheduleForBook_Refused(t *testing.T) {
	e := newRebaseEnv(t)
	ev := e.newEvent(domain.AssetEventTypeAddition, "book-OTHER", 100)
	if err := e.apply(ev); !errors.Is(err, domain.ErrNoActiveScheduleForBook) {
		t.Fatalf("expected ErrNoActiveScheduleForBook, got %v", err)
	}
	got, _ := e.s.GetAssetEvent(e.ctx, ev.EventID)
	if got.Status != domain.AssetEventStatusApproved {
		t.Fatalf("expected event to stay APPROVED, got %s", got.Status)
	}
}

func TestRebase_MissingBookOrAmount_RefusedAtApply(t *testing.T) {
	e := newRebaseEnv(t)
	ev := e.newEvent(domain.AssetEventTypeAddition, "", 100)
	if err := e.apply(ev); !errors.Is(err, domain.ErrBookRequiredForBookEvent) {
		t.Fatalf("expected ErrBookRequiredForBookEvent, got %v", err)
	}
}

func TestRebase_ReverseImpairment_RestoresNBV(t *testing.T) {
	e := newRebaseEnv(t)
	e.runPeriod(true)
	e.runPeriod(true)
	e.runPeriod(true)
	ev := e.newEvent(domain.AssetEventTypeImpairment, "book-1", 1800)
	if err := e.apply(ev); err != nil {
		t.Fatal(err)
	}
	e.runPeriod(true) // 800 depreciated on v2: carrying 6400, remaining 8
	if !near(e.nbv(), 6400) {
		t.Fatalf("expected 6400, got %v", e.nbv())
	}
	if err := e.s.ReverseAssetEvent(e.ctx, ev.EventID, "approver-1", "mistake", e.tick()); err != nil {
		t.Fatalf("reverse: %v", err)
	}
	e.expectSchedule("after reversal", 8200, 8, 3) // 6400 + 1800
	if !near(e.nbv(), 8200) {
		t.Fatalf("NBV expected 8200, got %v", e.nbv())
	}
	if e.effectCount(ev.EventID) != 2 {
		t.Fatalf("expected APPLY+REVERSE effects rows, got %d", e.effectCount(ev.EventID))
	}
}

func TestRebase_SupersedeAddition_UndoesAddition(t *testing.T) {
	e := newRebaseEnv(t)
	e.runPeriod(true)
	ev := e.newEvent(domain.AssetEventTypeAddition, "book-1", 2000)
	if err := e.apply(ev); err != nil {
		t.Fatal(err)
	}
	e.expectSchedule("after addition", 13000, 11, 2) // 11000 + 2000
	if err := e.s.SupersedeAssetEvent(e.ctx, ev.EventID, "approver-1", "wrong", e.tick()); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	e.expectSchedule("after supersede", 11000, 11, 3)
}

func TestRebase_ReverseRevaluation_RestoresPreEventCarryingLessDepreciation(t *testing.T) {
	e := newRebaseEnv(t)
	e.runPeriod(true)
	e.runPeriod(true)
	e.runPeriod(true) // carrying 9000, remaining 9
	ev := e.newEvent(domain.AssetEventTypeRevaluation, "book-1", 9900)
	if err := e.apply(ev); err != nil {
		t.Fatal(err)
	}
	e.runPeriod(true) // 9900/9 = 1100 -> carrying 8800, remaining 8
	if !near(e.nbv(), 8800) {
		t.Fatalf("expected 8800, got %v", e.nbv())
	}
	if err := e.s.ReverseAssetEvent(e.ctx, ev.EventID, "approver-1", "bad valuation", e.tick()); err != nil {
		t.Fatalf("reverse: %v", err)
	}
	// old_carrying 9000 - 1100 depreciated since = 7900.
	e.expectSchedule("after reversal", 7900, 8, 3)
}

func TestRebase_ReverseRevaluation_OutOfOrder_Refused(t *testing.T) {
	e := newRebaseEnv(t)
	reval := e.newEvent(domain.AssetEventTypeRevaluation, "book-1", 10000)
	if err := e.apply(reval); err != nil {
		t.Fatal(err)
	}
	add := e.newEvent(domain.AssetEventTypeAddition, "book-1", 500)
	if err := e.apply(add); err != nil {
		t.Fatal(err)
	}
	if err := e.s.ReverseAssetEvent(e.ctx, reval.EventID, "approver-1", "x", e.tick()); !errors.Is(err, domain.ErrReversalOutOfOrder) {
		t.Fatalf("expected ErrReversalOutOfOrder, got %v", err)
	}
}

func TestRebase_Reverse_RunInFlight_Refused(t *testing.T) {
	e := newRebaseEnv(t)
	ev := e.newEvent(domain.AssetEventTypeImpairment, "book-1", 1000)
	if err := e.apply(ev); err != nil {
		t.Fatal(err)
	}
	e.runPeriod(false)
	if err := e.s.ReverseAssetEvent(e.ctx, ev.EventID, "approver-1", "x", e.tick()); !errors.Is(err, domain.ErrDepreciationRunInFlight) {
		t.Fatalf("expected ErrDepreciationRunInFlight, got %v", err)
	}
	got, _ := e.s.GetAssetEvent(e.ctx, ev.EventID)
	if got.Status != domain.AssetEventStatusAccountingEventEmitted {
		t.Fatalf("refused reversal must leave event EMITTED, got %s", got.Status)
	}
}

func TestRebase_Reverse_NoCurrentSchedule_Refused(t *testing.T) {
	e := newRebaseEnv(t)
	ev := e.newEvent(domain.AssetEventTypeImpairment, "book-1", 1000)
	if err := e.apply(ev); err != nil {
		t.Fatal(err)
	}
	// Remove the schedule's ACTIVE status out-of-band (superuser).
	if _, err := e.pool.Exec(context.Background(), `UPDATE depreciation_schedules SET status='SUPERSEDED', effective_to=now() WHERE tenant_id=$1 AND effective_to IS NULL`, e.tenantID); err != nil {
		t.Fatal(err)
	}
	if err := e.s.ReverseAssetEvent(e.ctx, ev.EventID, "approver-1", "x", e.tick()); !errors.Is(err, domain.ErrNoActiveScheduleForBook) {
		t.Fatalf("expected ErrNoActiveScheduleForBook, got %v", err)
	}
}

func TestRebase_RecordOnlyTypes_DoNotTouchSchedule(t *testing.T) {
	e := newRebaseEnv(t)
	for _, typ := range []string{domain.AssetEventTypeCapitalization, domain.AssetEventTypeTransfer} {
		ev := e.newEvent(typ, "", 100)
		if err := e.apply(ev); err != nil {
			t.Fatalf("%s apply: %v", typ, err)
		}
	}
	e.expectSchedule("untouched", 12000, 12, 1)
}

func TestRebase_EffectsTable_InsertOnly(t *testing.T) {
	e := newRebaseEnv(t)
	ev := e.newEvent(domain.AssetEventTypeAddition, "book-1", 100)
	if err := e.apply(ev); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE asset_event_schedule_effects SET new_carrying = 1 WHERE event_id = $1`, ev.EventID); err == nil {
		t.Fatal("expected UPDATE of asset_event_schedule_effects to be rejected")
	}
	if _, err := e.pool.Exec(context.Background(), `DELETE FROM asset_event_schedule_effects WHERE event_id = $1`, ev.EventID); err == nil {
		t.Fatal("expected DELETE from asset_event_schedule_effects to be rejected")
	}
}

func TestRebase_BookID_ImmutableOnceApplied(t *testing.T) {
	e := newRebaseEnv(t)
	ev := e.newEvent(domain.AssetEventTypeAddition, "book-1", 100)
	if err := e.apply(ev); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE asset_events SET book_id = 'book-2' WHERE event_id = $1`, ev.EventID); err == nil {
		t.Fatal("expected book_id UPDATE on an applied event to be rejected by the economic-mutation trigger")
	}
}
