package store_test

// consolidation-svc had no internal/store tests at all before this — this
// file is both the general-purpose real-Postgres harness (openTestPool)
// and ACC-12's own coverage of the guarded UPDATE ... WHERE status = $n
// clauses and the migration 000003 CHECK constraints, which an in-memory
// stub cannot exercise. Skips (not fails) if TEST_DATABASE_URL isn't set,
// same convention as every other service in this platform.

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/consolidation-svc/internal/domain"
	svcmiddleware "zoiko.io/consolidation-svc/internal/middleware"
	"zoiko.io/consolidation-svc/internal/store"
)

func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to connect to postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	_, filename, _, _ := runtime.Caller(0)
	base := filepath.Dir(filename)

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS
		consolidation_adjustments, balance_contributions, balance_snapshots, consolidation_runs
		CASCADE;`)

	for _, migration := range []string{
		"000001_initial_schema.up.sql",
		"000002_add_balance_contributions.up.sql",
		"000003_add_consolidation_adjustments.up.sql",
	} {
		sql, err := os.ReadFile(filepath.Join(base, "../../deployments/migrations", migration))
		if err != nil {
			t.Fatalf("failed to read migration %s: %v", migration, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("failed to apply migration %s: %v", migration, err)
		}
	}

	return pool
}

func newTestRun(tenantID, groupLegalEntityID string) *domain.ConsolidationRun {
	return &domain.ConsolidationRun{
		ConsolidationRunID: uuid.New().String(), TenantID: tenantID,
		GroupLegalEntityID: groupLegalEntityID, FiscalPeriod: "2026-07",
		TargetCurrency: "USD", Status: "COMPLETED", StartedAt: time.Now().UTC(),
	}
}

func newTestAdjustment(tenantID, groupLegalEntityID, createdBy string) *domain.ConsolidationAdjustment {
	return &domain.ConsolidationAdjustment{
		ConsolidationAdjustmentID: uuid.New().String(), TenantID: tenantID,
		GroupLegalEntityID: groupLegalEntityID, FiscalPeriod: "2026-07",
		AdjustmentType: domain.AdjustmentTypeManual, Description: "test adjustment",
		Status: domain.AdjustmentStatusPendingApproval,
		Lines: []domain.ConsolidationAdjustmentLine{
			{AccountCode: "9000", DebitAmount: 100},
			{AccountCode: "9100", CreditAmount: 100},
		},
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: createdBy,
	}
}

func TestPgStore_ACC12_GroupEntityHasRun_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	hasRun, err := s.GroupEntityHasRun(ctx, "group-1")
	if err != nil {
		t.Fatalf("GroupEntityHasRun: %v", err)
	}
	if hasRun {
		t.Fatal("expected no run recorded yet")
	}

	if err := s.CreateRun(ctx, newTestRun(tenantID, "group-1")); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	hasRun, err = s.GroupEntityHasRun(ctx, "group-1")
	if err != nil {
		t.Fatalf("GroupEntityHasRun: %v", err)
	}
	if !hasRun {
		t.Fatal("expected group-1 to now have a run on record")
	}
}

func TestPgStore_ACC12_FullLifecycle_ApprovePostReverse_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	adj := newTestAdjustment(tenantID, "group-1", "preparer-1")
	if err := s.CreateAdjustment(ctx, adj); err != nil {
		t.Fatalf("CreateAdjustment: %v", err)
	}

	got, err := s.GetAdjustment(ctx, adj.ConsolidationAdjustmentID)
	if err != nil {
		t.Fatalf("GetAdjustment: %v", err)
	}
	if len(got.Lines) != 2 || got.Lines[0].AccountCode != "9000" {
		t.Fatalf("expected lines round-tripped through JSONB, got %+v", got.Lines)
	}

	if err := s.ApproveAdjustment(ctx, adj.ConsolidationAdjustmentID, "approver-1"); err != nil {
		t.Fatalf("ApproveAdjustment: %v", err)
	}
	journalID := uuid.New().String()
	if err := s.MarkAdjustmentPosted(ctx, adj.ConsolidationAdjustmentID, "poster-1", journalID); err != nil {
		t.Fatalf("MarkAdjustmentPosted: %v", err)
	}
	got, err = s.GetAdjustment(ctx, adj.ConsolidationAdjustmentID)
	if err != nil {
		t.Fatalf("GetAdjustment: %v", err)
	}
	if got.Status != domain.AdjustmentStatusPosted || got.ConsolidationBookJournalID == nil || *got.ConsolidationBookJournalID != journalID {
		t.Fatalf("expected POSTED with journal recorded, got %+v", got)
	}

	// The FK on superseded_by_adjustment_id requires the replacement to be
	// a real row — the same "no phantom supersession" guarantee that FK
	// gives for free.
	replacement := newTestAdjustment(tenantID, "group-1", "preparer-1")
	if err := s.CreateAdjustment(ctx, replacement); err != nil {
		t.Fatalf("CreateAdjustment (replacement): %v", err)
	}

	if err := s.ReverseAdjustment(ctx, adj.ConsolidationAdjustmentID, "reverser-1", "correction", &replacement.ConsolidationAdjustmentID); err != nil {
		t.Fatalf("ReverseAdjustment: %v", err)
	}
	got, err = s.GetAdjustment(ctx, adj.ConsolidationAdjustmentID)
	if err != nil {
		t.Fatalf("GetAdjustment: %v", err)
	}
	if got.Status != domain.AdjustmentStatusReversed || got.SupersededByAdjustmentID == nil || *got.SupersededByAdjustmentID != replacement.ConsolidationAdjustmentID {
		t.Fatalf("expected REVERSED with supersession recorded, got %+v", got)
	}
}

// TestPgStore_ACC12_ApproveFromWrongStatus_Refused proves the guarded
// UPDATE's WHERE clause against the real database.
func TestPgStore_ACC12_ApproveFromWrongStatus_Refused(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	adj := newTestAdjustment(tenantID, "group-1", "preparer-1")
	if err := s.CreateAdjustment(ctx, adj); err != nil {
		t.Fatalf("CreateAdjustment: %v", err)
	}
	if err := s.ApproveAdjustment(ctx, adj.ConsolidationAdjustmentID, "approver-1"); err != nil {
		t.Fatalf("first ApproveAdjustment: %v", err)
	}
	// A second approval attempt must fail — the adjustment is now APPROVED,
	// not PENDING_APPROVAL.
	if err := s.ApproveAdjustment(ctx, adj.ConsolidationAdjustmentID, "approver-2"); err == nil {
		t.Fatal("expected a second ApproveAdjustment to be refused")
	}
}

// TestPgStore_ACC12_InvalidAdjustmentType_RejectedByCheckConstraint proves
// migration 000003's CHECK constraints are real, not just documentation.
func TestPgStore_ACC12_InvalidAdjustmentType_RejectedByCheckConstraint(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	adj := newTestAdjustment(tenantID, "group-1", "preparer-1")
	adj.AdjustmentType = "NOT_A_REAL_TYPE"
	if err := s.CreateAdjustment(ctx, adj); err == nil {
		t.Fatal("expected the CHECK constraint to reject an unrecognized adjustment_type value")
	}
}
