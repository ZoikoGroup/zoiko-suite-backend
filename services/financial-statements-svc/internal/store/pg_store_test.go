//go:build integration

package store_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/financial-statements-svc/internal/domain"
	"zoiko.io/financial-statements-svc/internal/store"
)

// openTestPool connects to a real Postgres and reapplies the migrations from
// a clean slate. Skips (not fails) if TEST_DATABASE_URL isn't set.
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

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS statement_facts CASCADE;`)
	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS financial_statement_sets CASCADE;`)
	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS eventing_outbox CASCADE;`)

	for _, migration := range []string{
		"000001_initial_schema.up.sql",
		"000002_add_eventing_outbox.up.sql",
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

func newTestSet(tenantID, legalEntityID, period string) *domain.FinancialStatementSet {
	now := time.Now().UTC()
	return &domain.FinancialStatementSet{
		StatementSetID:       uuid.NewString(),
		TenantID:             tenantID,
		LegalEntityID:        legalEntityID,
		ReportingPeriod:      period,
		Framework:            "IFRS",
		PresentationCurrency: "GBP",
		SourceSnapshotID:     "snap-1",
		SourceSnapshotHash:   "sha256:abc",
		MappingVersion:       domain.MappingVersionV1,
		Status:               domain.StateDraft,
		Version:              1,
		CreatedAt:            now,
		CreatedByPrincipalID: "preparer-1",
	}
}

func testFacts(setID, tenantID string) []domain.StatementFact {
	now := time.Now().UTC()
	return []domain.StatementFact{
		{FactID: uuid.NewString(), TenantID: tenantID, StatementSetID: setID, StatementType: domain.StatementBalanceSheet, Category: domain.CategoryAsset, AccountCode: "1000", AccountName: "Cash", Amount: "500", CreatedAt: now},
		{FactID: uuid.NewString(), TenantID: tenantID, StatementSetID: setID, StatementType: domain.StatementBalanceSheet, Category: domain.CategoryLiability, AccountCode: "2000", AccountName: "Payables", Amount: "100", CreatedAt: now},
		{FactID: uuid.NewString(), TenantID: tenantID, StatementSetID: setID, StatementType: domain.StatementBalanceSheet, Category: domain.CategoryEquity, AccountCode: "3000", AccountName: "Equity", Amount: "400", CreatedAt: now},
	}
}

func TestPgStore_CreateSet_EnqueuesCreatedEvent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("test"))
	ctx := context.Background()

	set := newTestSet("tenant-1", "le-1", "2026-Q1")
	if err := s.CreateSet(ctx, "tenant-1", set, "corr-1"); err != nil {
		t.Fatalf("CreateSet: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM eventing_outbox WHERE tenant_id = $1 AND event_type LIKE '%created'`, "tenant-1").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 created event, got %d", count)
	}
}

func TestPgStore_CreateSet_OnlyOneActivePerEntityPeriodFramework(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("test"))
	ctx := context.Background()

	set1 := newTestSet("tenant-1", "le-1", "2026-Q1")
	if err := s.CreateSet(ctx, "tenant-1", set1, "corr-1"); err != nil {
		t.Fatalf("CreateSet 1: %v", err)
	}
	set2 := newTestSet("tenant-1", "le-1", "2026-Q1")
	err := s.CreateSet(ctx, "tenant-1", set2, "corr-2")
	if err == nil {
		t.Fatal("expected a second active set for the same entity/period/framework to be refused")
	}
	de, ok := domain.AsError(err)
	if !ok || de.Code != domain.CodeContextInvalid {
		t.Fatalf("expected CodeContextInvalid, got %v", err)
	}
}

func TestPgStore_FullLifecycle_EnqueuesExpectedEvents(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("test"))
	ctx := context.Background()

	set := newTestSet("tenant-1", "le-1", "2026-Q1")
	if err := s.CreateSet(ctx, "tenant-1", set, "corr-1"); err != nil {
		t.Fatalf("CreateSet: %v", err)
	}

	facts := testFacts(set.StatementSetID, "tenant-1")
	if err := s.RefreshFacts(ctx, "tenant-1", set.StatementSetID, 1, facts, "preparer-1", "corr-2"); err != nil {
		t.Fatalf("RefreshFacts: %v", err)
	}
	if err := s.Validate(ctx, "tenant-1", set.StatementSetID, 2, nil, "corr-3"); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := s.SubmitForReview(ctx, "tenant-1", set.StatementSetID, 3, "preparer-1", "corr-4"); err != nil {
		t.Fatalf("SubmitForReview: %v", err)
	}
	if err := s.Approve(ctx, "tenant-1", set.StatementSetID, 4, "reviewer-1", "corr-5"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := s.Certify(ctx, "tenant-1", set.StatementSetID, 5, "cfo-1", "quarter end", "corr-6"); err != nil {
		t.Fatalf("Certify: %v", err)
	}
	if err := s.Publish(ctx, "tenant-1", set.StatementSetID, 6, "cfo-1", "corr-7"); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	got, err := s.GetSet(ctx, "tenant-1", set.StatementSetID)
	if err != nil {
		t.Fatalf("GetSet: %v", err)
	}
	if got.Status != domain.StatePublished {
		t.Fatalf("expected PUBLISHED, got %s", got.Status)
	}
	if got.Version != 7 {
		t.Fatalf("expected version 7 after 6 transitions from version 1, got %d", got.Version)
	}

	var eventCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM eventing_outbox WHERE tenant_id = $1`, "tenant-1").Scan(&eventCount); err != nil {
		t.Fatalf("count: %v", err)
	}
	// created, validated, submitted-for-review, approved, certified, published
	// (draft-refreshed also enqueues one) = 7.
	if eventCount != 7 {
		t.Fatalf("expected 7 outbox events across the full lifecycle, got %d", eventCount)
	}
}

func TestPgStore_VersionConflict_IsDetected(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("test"))
	ctx := context.Background()

	set := newTestSet("tenant-1", "le-1", "2026-Q1")
	if err := s.CreateSet(ctx, "tenant-1", set, "corr-1"); err != nil {
		t.Fatalf("CreateSet: %v", err)
	}

	// Stale expected version (0, not 1).
	err := s.RefreshFacts(ctx, "tenant-1", set.StatementSetID, 0, testFacts(set.StatementSetID, "tenant-1"), "preparer-1", "corr-2")
	if err == nil {
		t.Fatal("expected a version conflict")
	}
	de, ok := domain.AsError(err)
	if !ok || de.Code != domain.CodeVersionConflict {
		t.Fatalf("expected CodeVersionConflict, got %v", err)
	}
}

func TestPgStore_RLS_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("test"))
	ctx := context.Background()

	setA := newTestSet("tenant-A", "le-1", "2026-Q1")
	if err := s.CreateSet(ctx, "tenant-A", setA, "corr-1"); err != nil {
		t.Fatalf("CreateSet (tenant A): %v", err)
	}

	_, err := s.GetSet(ctx, "tenant-B", setA.StatementSetID)
	if err == nil {
		t.Fatal("RLS failure: tenant B's session was able to read tenant A's statement set")
	}
}

func TestPgStore_Approve_SoDEnforcedAtStoreLayer(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("test"))
	ctx := context.Background()

	set := newTestSet("tenant-1", "le-1", "2026-Q1")
	if err := s.CreateSet(ctx, "tenant-1", set, "corr-1"); err != nil {
		t.Fatalf("CreateSet: %v", err)
	}
	if err := s.RefreshFacts(ctx, "tenant-1", set.StatementSetID, 1, testFacts(set.StatementSetID, "tenant-1"), "preparer-1", "corr-2"); err != nil {
		t.Fatalf("RefreshFacts: %v", err)
	}
	if err := s.Validate(ctx, "tenant-1", set.StatementSetID, 2, nil, "corr-3"); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := s.SubmitForReview(ctx, "tenant-1", set.StatementSetID, 3, "preparer-1", "corr-4"); err != nil {
		t.Fatalf("SubmitForReview: %v", err)
	}

	// The preparer tries to approve their own work directly at the store
	// layer (bypassing the service layer's own check) — the store must
	// refuse it too, since it is the layer that actually holds the lock.
	err := s.Approve(ctx, "tenant-1", set.StatementSetID, 4, "preparer-1", "corr-5")
	if err == nil {
		t.Fatal("expected the store layer to refuse a preparer approving their own work")
	}
	de, ok := domain.AsError(err)
	if !ok || de.Code != domain.CodeSoDDenied {
		t.Fatalf("expected CodeSoDDenied, got %v", err)
	}
}

func TestPgStore_Supersede_MarksOldAndCreatesNew(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("test"))
	ctx := context.Background()

	old := newTestSet("tenant-1", "le-1", "2026-Q1")
	if err := s.CreateSet(ctx, "tenant-1", old, "corr-1"); err != nil {
		t.Fatalf("CreateSet: %v", err)
	}
	if err := s.RefreshFacts(ctx, "tenant-1", old.StatementSetID, 1, testFacts(old.StatementSetID, "tenant-1"), "preparer-1", "corr-2"); err != nil {
		t.Fatalf("RefreshFacts: %v", err)
	}
	if err := s.Validate(ctx, "tenant-1", old.StatementSetID, 2, nil, "corr-3"); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := s.SubmitForReview(ctx, "tenant-1", old.StatementSetID, 3, "preparer-1", "corr-4"); err != nil {
		t.Fatalf("SubmitForReview: %v", err)
	}
	if err := s.Approve(ctx, "tenant-1", old.StatementSetID, 4, "reviewer-1", "corr-5"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := s.Certify(ctx, "tenant-1", old.StatementSetID, 5, "cfo-1", "reason", "corr-6"); err != nil {
		t.Fatalf("Certify: %v", err)
	}

	newSet := newTestSet("tenant-1", "le-1", "2026-Q1")
	newSet.SourceSnapshotID = "snap-2"
	newSet.SourceSnapshotHash = "sha256:def"
	oldID := old.StatementSetID
	newSet.SupersedesSetID = &oldID

	oldAfterCertify, err := s.GetSet(ctx, "tenant-1", old.StatementSetID)
	if err != nil {
		t.Fatalf("GetSet: %v", err)
	}
	if err := s.Supersede(ctx, "tenant-1", old.StatementSetID, oldAfterCertify.Version, newSet, "preparer-2", "corr-7"); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	gotOld, err := s.GetSet(ctx, "tenant-1", old.StatementSetID)
	if err != nil {
		t.Fatalf("GetSet old: %v", err)
	}
	if gotOld.Status != domain.StateSuperseded {
		t.Fatalf("expected old set SUPERSEDED, got %s", gotOld.Status)
	}
	if gotOld.SupersededBySetID == nil || *gotOld.SupersededBySetID != newSet.StatementSetID {
		t.Fatal("expected old set to record its replacement's id")
	}

	gotNew, err := s.GetSet(ctx, "tenant-1", newSet.StatementSetID)
	if err != nil {
		t.Fatalf("GetSet new: %v", err)
	}
	if gotNew.Status != domain.StateDraft {
		t.Fatalf("expected new set DRAFT, got %s", gotNew.Status)
	}
	if gotNew.SupersedesSetID == nil || *gotNew.SupersedesSetID != old.StatementSetID {
		t.Fatal("expected new set to record which set it supersedes")
	}

	// Now that an active (non-superseded) replacement exists, creating yet
	// another set for the same entity/period/framework must still be refused
	// — the unique index only ignores SUPERSEDED rows, and the new set is
	// DRAFT, not superseded.
	another := newTestSet("tenant-1", "le-1", "2026-Q1")
	if err := s.CreateSet(ctx, "tenant-1", another, "corr-8"); err == nil {
		t.Fatal("expected a third set for the same entity/period/framework to be refused while the replacement is still active")
	}
}
