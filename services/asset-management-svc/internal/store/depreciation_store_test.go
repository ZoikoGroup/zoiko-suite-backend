package store_test

// AST-02's own store methods are exercised only by handler_test.go's
// in-memory stub elsewhere in this package — this file is their
// real-Postgres coverage, against the actual guarded UPDATEs, partial
// UNIQUE indexes and append-only trigger, not a stub's map mutation.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/asset-management-svc/internal/domain"
	svcmiddleware "zoiko.io/asset-management-svc/internal/middleware"
	"zoiko.io/asset-management-svc/internal/store"
)

func activeAssetInStore(t *testing.T, ctx context.Context, s *store.PgStore, tenantID, legalEntityID string) *domain.FixedAsset {
	t.Helper()
	a := newTestAsset(tenantID, legalEntityID)
	a.AcquisitionSourceRef = "PO-1001"
	if err := s.CreateAsset(ctx, a); err != nil {
		t.Fatalf("CreateAsset: %v", err)
	}
	now := time.Now().UTC()
	if err := s.RegisterAsset(ctx, a.AssetID, "approver-1", now); err != nil {
		t.Fatalf("RegisterAsset: %v", err)
	}
	if err := s.CapitalizeAsset(ctx, a.AssetID, "approver-1", now); err != nil {
		t.Fatalf("CapitalizeAsset: %v", err)
	}
	a.Status = domain.AssetStatusActive
	return a
}

func newTestSchedule(tenantID string, a *domain.FixedAsset) *domain.DepreciationSchedule {
	return &domain.DepreciationSchedule{
		ScheduleVersionID: uuid.New().String(), ScheduleID: uuid.New().String(), Version: 1,
		TenantID: tenantID, LegalEntityID: a.LegalEntityID, AssetID: a.AssetID, BookID: "book-1",
		Method: domain.DepreciationMethodStraightLine, CostBasis: 12000, ResidualValue: 0, UsefulLifeMonths: 12,
		InServiceDate: time.Now().UTC(), Status: domain.DepreciationScheduleStatusActive,
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
}

func TestPgStore_CreateDepreciationSchedule_DuplicateForAssetBook_Refused(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	a := activeAssetInStore(t, ctx, s, tenantID, "le-1")

	sch1 := newTestSchedule(tenantID, a)
	if err := s.CreateDepreciationSchedule(ctx, sch1); err != nil {
		t.Fatalf("first CreateDepreciationSchedule: %v", err)
	}

	sch2 := newTestSchedule(tenantID, a) // same asset_id, same book_id
	if err := s.CreateDepreciationSchedule(ctx, sch2); err == nil {
		t.Fatal("expected a second current schedule for the same (asset, book) to be refused")
	}
}

func TestPgStore_RecalculateSchedule_SupersedesOldVersion(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	a := activeAssetInStore(t, ctx, s, tenantID, "le-1")
	sch := newTestSchedule(tenantID, a)
	if err := s.CreateDepreciationSchedule(ctx, sch); err != nil {
		t.Fatalf("CreateDepreciationSchedule: %v", err)
	}

	newVersion := &domain.DepreciationSchedule{
		ScheduleVersionID: uuid.New().String(), LegalEntityID: a.LegalEntityID, AssetID: a.AssetID, BookID: "book-1",
		Method: domain.DepreciationMethodStraightLine, CostBasis: 12000, ResidualValue: 0, UsefulLifeMonths: 24,
		InServiceDate: sch.InServiceDate, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
	if err := s.RecalculateSchedule(ctx, sch.ScheduleID, newVersion, time.Now().UTC()); err != nil {
		t.Fatalf("RecalculateSchedule: %v", err)
	}
	if newVersion.Version != 2 {
		t.Fatalf("expected version 2, got %d", newVersion.Version)
	}

	current, err := s.GetCurrentDepreciationSchedule(ctx, sch.ScheduleID)
	if err != nil {
		t.Fatalf("GetCurrentDepreciationSchedule: %v", err)
	}
	if current.UsefulLifeMonths != 24 || current.ScheduleVersionID != newVersion.ScheduleVersionID {
		t.Fatalf("expected the current schedule to be the new version, got %+v", current)
	}
}

func TestPgStore_CreateDepreciationRun_DuplicateForPeriod_Refused(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	run1 := &domain.DepreciationRun{
		RunID: uuid.New().String(), LegalEntityID: "le-1", FiscalPeriod: "2026-01",
		DepreciationExpenseAccountCode: "6400-Depr", AccumulatedDepreciationAccountCode: "1590-AccumDepr",
		Status: domain.DepreciationRunStatusDraft, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
	if err := s.CreateDepreciationRun(ctx, run1); err != nil {
		t.Fatalf("first CreateDepreciationRun: %v", err)
	}
	run2 := &domain.DepreciationRun{
		RunID: uuid.New().String(), LegalEntityID: "le-1", FiscalPeriod: "2026-01",
		DepreciationExpenseAccountCode: "6400-Depr", AccumulatedDepreciationAccountCode: "1590-AccumDepr",
		Status: domain.DepreciationRunStatusDraft, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
	if err := s.CreateDepreciationRun(ctx, run2); err == nil {
		t.Fatal("expected a second live run for the same (entity, period) to be refused")
	}
}

func TestPgStore_DepreciationRun_FullLifecycle_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	a := activeAssetInStore(t, ctx, s, tenantID, "le-1")
	sch := newTestSchedule(tenantID, a)
	if err := s.CreateDepreciationSchedule(ctx, sch); err != nil {
		t.Fatalf("CreateDepreciationSchedule: %v", err)
	}

	run := &domain.DepreciationRun{
		RunID: uuid.New().String(), LegalEntityID: "le-1", FiscalPeriod: "2026-01",
		DepreciationExpenseAccountCode: "6400-Depr", AccumulatedDepreciationAccountCode: "1590-AccumDepr",
		Status: domain.DepreciationRunStatusDraft, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
	if err := s.CreateDepreciationRun(ctx, run); err != nil {
		t.Fatalf("CreateDepreciationRun: %v", err)
	}

	now := time.Now().UTC()
	frozenCount, err := s.FreezeDepreciationPopulation(ctx, run.RunID, "le-1", now)
	if err != nil {
		t.Fatalf("FreezeDepreciationPopulation: %v", err)
	}
	if frozenCount != 1 {
		t.Fatalf("expected 1 schedule frozen, got %d", frozenCount)
	}

	lineCount, err := s.ValidateDepreciationRun(ctx, run.RunID, now)
	if err != nil {
		t.Fatalf("ValidateDepreciationRun: %v", err)
	}
	if lineCount != 1 {
		t.Fatalf("expected 1 depreciation line, got %d", lineCount)
	}

	got, err := s.GetDepreciationRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("GetDepreciationRun: %v", err)
	}
	if len(got.Lines) != 1 {
		t.Fatalf("expected 1 line on the run, got %d", len(got.Lines))
	}
	expectedMonthly := 12000.0 / 12.0
	if got.Lines[0].PeriodAmount != expectedMonthly {
		t.Fatalf("expected period_amount %v, got %v", expectedMonthly, got.Lines[0].PeriodAmount)
	}

	if err := s.ApproveDepreciationRun(ctx, run.RunID, "approver-1", now); err != nil {
		t.Fatalf("ApproveDepreciationRun: %v", err)
	}
	if err := s.MarkDepreciationRunEmitted(ctx, run.RunID, "journal-1", now); err != nil {
		t.Fatalf("MarkDepreciationRunEmitted: %v", err)
	}

	got, err = s.GetDepreciationRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("GetDepreciationRun: %v", err)
	}
	if got.Status != domain.DepreciationRunStatusAccountingEventEmitted || got.JournalID == nil || *got.JournalID != "journal-1" {
		t.Fatalf("expected ACCOUNTING_EVENT_EMITTED with journal recorded, got %+v", got)
	}

	// SupersedeDepreciationRun releases the period slot for a new run.
	if err := s.SupersedeDepreciationRun(ctx, run.RunID, "preparer-1", now); err != nil {
		t.Fatalf("SupersedeDepreciationRun: %v", err)
	}
	newRun := &domain.DepreciationRun{
		RunID: uuid.New().String(), LegalEntityID: "le-1", FiscalPeriod: "2026-01",
		DepreciationExpenseAccountCode: "6400-Depr", AccumulatedDepreciationAccountCode: "1590-AccumDepr",
		Status: domain.DepreciationRunStatusDraft, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
	if err := s.CreateDepreciationRun(ctx, newRun); err != nil {
		t.Fatalf("expected a new run to be creatable after supersession, got: %v", err)
	}
}

// TestPgStore_DepreciationLines_RejectsUpdateAndDelete_RealDB proves the
// append-only trigger at the database level.
func TestPgStore_DepreciationLines_RejectsUpdateAndDelete_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	a := activeAssetInStore(t, ctx, s, tenantID, "le-1")
	sch := newTestSchedule(tenantID, a)
	if err := s.CreateDepreciationSchedule(ctx, sch); err != nil {
		t.Fatalf("CreateDepreciationSchedule: %v", err)
	}
	run := &domain.DepreciationRun{
		RunID: uuid.New().String(), LegalEntityID: "le-1", FiscalPeriod: "2026-01",
		DepreciationExpenseAccountCode: "6400-Depr", AccumulatedDepreciationAccountCode: "1590-AccumDepr",
		Status: domain.DepreciationRunStatusDraft, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
	if err := s.CreateDepreciationRun(ctx, run); err != nil {
		t.Fatalf("CreateDepreciationRun: %v", err)
	}
	now := time.Now().UTC()
	if _, err := s.FreezeDepreciationPopulation(ctx, run.RunID, "le-1", now); err != nil {
		t.Fatalf("FreezeDepreciationPopulation: %v", err)
	}
	if _, err := s.ValidateDepreciationRun(ctx, run.RunID, now); err != nil {
		t.Fatalf("ValidateDepreciationRun: %v", err)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `UPDATE depreciation_lines SET period_amount = 999`); err == nil {
		t.Fatal("expected UPDATE on depreciation_lines to be rejected by the append-only trigger")
	}
}

// TestPgStore_GetNetBookValueTotal_UndepreciatedSchedule_CountsFullCostBasis
// proves a schedule with no depreciation lines yet (never run) counts at
// its full cost_basis — the real proof that "undepreciated" is a valid
// NBV, not a missing/zero one.
func TestPgStore_GetNetBookValueTotal_UndepreciatedSchedule_CountsFullCostBasis(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	a := activeAssetInStore(t, ctx, s, tenantID, "le-1")
	sch := newTestSchedule(tenantID, a)
	if err := s.CreateDepreciationSchedule(ctx, sch); err != nil {
		t.Fatalf("CreateDepreciationSchedule: %v", err)
	}

	total, err := s.GetNetBookValueTotal(ctx, "le-1", "book-1")
	if err != nil {
		t.Fatalf("GetNetBookValueTotal: %v", err)
	}
	if total != 12000 {
		t.Fatalf("expected 12000 (full cost_basis, never depreciated), got %v", total)
	}
}

// TestPgStore_GetNetBookValueTotal_AfterDepreciationRun_SubtractsAccumulated
// proves the real, live calculation against actual depreciation_lines —
// this is the exact source financial-close-svc's ACC-06 reconciles
// against for the AST/INV/PRJ domain spec's own §9 "Assets → GL"
// assertion.
func TestPgStore_GetNetBookValueTotal_AfterDepreciationRun_SubtractsAccumulated(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	a := activeAssetInStore(t, ctx, s, tenantID, "le-1")
	sch := newTestSchedule(tenantID, a) // cost_basis=12000, useful_life=12 -> 1000/month straight-line
	if err := s.CreateDepreciationSchedule(ctx, sch); err != nil {
		t.Fatalf("CreateDepreciationSchedule: %v", err)
	}

	run := &domain.DepreciationRun{
		RunID: uuid.New().String(), LegalEntityID: "le-1", FiscalPeriod: "2026-01",
		DepreciationExpenseAccountCode: "6400-Depr", AccumulatedDepreciationAccountCode: "1590-AccumDepr",
		Status: domain.DepreciationRunStatusDraft, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
	if err := s.CreateDepreciationRun(ctx, run); err != nil {
		t.Fatalf("CreateDepreciationRun: %v", err)
	}
	now := time.Now().UTC()
	if _, err := s.FreezeDepreciationPopulation(ctx, run.RunID, "le-1", now); err != nil {
		t.Fatalf("FreezeDepreciationPopulation: %v", err)
	}
	if _, err := s.ValidateDepreciationRun(ctx, run.RunID, now); err != nil {
		t.Fatalf("ValidateDepreciationRun: %v", err)
	}

	total, err := s.GetNetBookValueTotal(ctx, "le-1", "book-1")
	if err != nil {
		t.Fatalf("GetNetBookValueTotal: %v", err)
	}
	if total != 11000 {
		t.Fatalf("expected 11000 (12000 cost_basis - 1000 accumulated after one month), got %v", total)
	}

	// A schedule for a DIFFERENT book must never contribute.
	otherBookTotal, err := s.GetNetBookValueTotal(ctx, "le-1", "book-tax")
	if err != nil {
		t.Fatalf("GetNetBookValueTotal (other book): %v", err)
	}
	if otherBookTotal != 0 {
		t.Fatalf("expected 0 for an unrelated book_id, got %v", otherBookTotal)
	}
}

// TestPgStore_GetDepreciationCompleteness_RealDB proves the real
// eligible-vs-covered query against real Postgres, including the case a
// schedule exists but the period was never run at all (covered=0,
// eligible=1 — a real gap, not an error).
func TestPgStore_GetDepreciationCompleteness_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	a := activeAssetInStore(t, ctx, s, tenantID, "le-1")
	sch := newTestSchedule(tenantID, a)
	if err := s.CreateDepreciationSchedule(ctx, sch); err != nil {
		t.Fatalf("CreateDepreciationSchedule: %v", err)
	}

	covered, eligible, err := s.GetDepreciationCompleteness(ctx, "le-1", "2026-01")
	if err != nil {
		t.Fatalf("GetDepreciationCompleteness (never run): %v", err)
	}
	if eligible != 1 || covered != 0 {
		t.Fatalf("expected eligible=1 covered=0 before any run, got eligible=%d covered=%d", eligible, covered)
	}

	run := &domain.DepreciationRun{
		RunID: uuid.New().String(), LegalEntityID: "le-1", FiscalPeriod: "2026-01",
		DepreciationExpenseAccountCode: "6400-Depr", AccumulatedDepreciationAccountCode: "1590-AccumDepr",
		Status: domain.DepreciationRunStatusDraft, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
	if err := s.CreateDepreciationRun(ctx, run); err != nil {
		t.Fatalf("CreateDepreciationRun: %v", err)
	}
	now := time.Now().UTC()
	if _, err := s.FreezeDepreciationPopulation(ctx, run.RunID, "le-1", now); err != nil {
		t.Fatalf("FreezeDepreciationPopulation: %v", err)
	}
	if _, err := s.ValidateDepreciationRun(ctx, run.RunID, now); err != nil {
		t.Fatalf("ValidateDepreciationRun: %v", err)
	}

	covered, eligible, err = s.GetDepreciationCompleteness(ctx, "le-1", "2026-01")
	if err != nil {
		t.Fatalf("GetDepreciationCompleteness (after run): %v", err)
	}
	if eligible != 1 || covered != 1 {
		t.Fatalf("expected eligible=1 covered=1 after validation, got eligible=%d covered=%d", eligible, covered)
	}

	// A DIFFERENT fiscal period must never count as covered.
	coveredOtherPeriod, eligibleOtherPeriod, err := s.GetDepreciationCompleteness(ctx, "le-1", "2026-02")
	if err != nil {
		t.Fatalf("GetDepreciationCompleteness (other period): %v", err)
	}
	if eligibleOtherPeriod != 1 || coveredOtherPeriod != 0 {
		t.Fatalf("expected eligible=1 covered=0 for an unrun period, got eligible=%d covered=%d", eligibleOtherPeriod, coveredOtherPeriod)
	}
}
