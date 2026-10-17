//go:build integration

package store_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/reconciliation-intelligence-svc/internal/domain"
	"zoiko.io/reconciliation-intelligence-svc/internal/store"
)

// openTestPool connects to a real Postgres and reapplies the migrations
// from a clean slate. Skips (not fails) if TEST_DATABASE_URL isn't set.
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

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS reconciliation_unmatched_items CASCADE;`)
	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS reconciliation_jobs CASCADE;`)
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

func newTestJob(legalEntityID string) (*domain.ReconciliationJob, []domain.UnmatchedItem) {
	job := &domain.ReconciliationJob{
		LegalEntityID:       legalEntityID,
		JobName:             "test-job",
		SourceSystemA:       domain.SourceGeneralLedger,
		SourceSystemB:       domain.SourceBankStatement,
		TotalProcessedCount: 2,
		MatchedCount:        1,
		UnmatchedCount:      1,
		ReconciliationRate:  50.0,
		Status:              "COMPLETED",
	}
	items := []domain.UnmatchedItem{{
		TransactionRefA:   "ref-a",
		TransactionRefB:   "ref-b",
		AmountA:           "100.25",
		AmountB:           "132.5",
		DiscrepancyAmount: "32.25",
		DiscrepancyType:   domain.DiscrepancyAmountMismatch,
		ConfidenceScore:   80,
		Recommendation:    domain.RecommendationManualReview,
		ResolutionStatus:  "RECOMMENDED",
		ResolutionNotes:   "heuristic: above tolerance",
	}}
	return job, items
}

// TestPgStore_CreateJob_EnqueuesAnalyzedAndUnmatchedEvents proves CreateJob
// writes both lifecycle events into eventing_outbox in the same transaction
// as the job insert — the durability this service had none of before.
func TestPgStore_CreateJob_EnqueuesAnalyzedAndUnmatchedEvents(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool, store.WithEventRegion("test"))

	tenantID := uuid.New().String()
	ctx := context.Background()

	job, items := newTestJob("le-us")
	if err := s.CreateJob(ctx, tenantID, job, items, "actor-1", "corr-1"); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	rows, err := pool.Query(ctx, `SELECT event_type FROM eventing_outbox WHERE tenant_id = $1 ORDER BY event_type`, tenantID)
	if err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	defer rows.Close()

	var types []string
	for rows.Next() {
		var et string
		if err := rows.Scan(&et); err != nil {
			t.Fatalf("scan: %v", err)
		}
		types = append(types, et)
	}
	if len(types) != 2 {
		t.Fatalf("expected 2 outbox events (analyzed + unmatched-flagged), got %d: %v", len(types), types)
	}
}

// TestPgStore_CreateJob_NoUnmatchedItems_OnlyEnqueuesAnalyzed proves the
// unmatched-flagged event is conditional on UnmatchedCount > 0, matching the
// handler's pre-migration behavior.
func TestPgStore_CreateJob_NoUnmatchedItems_OnlyEnqueuesAnalyzed(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool, store.WithEventRegion("test"))

	tenantID := uuid.New().String()
	ctx := context.Background()

	job, _ := newTestJob("le-us")
	job.UnmatchedCount = 0
	if err := s.CreateJob(ctx, tenantID, job, nil, "actor-1", "corr-1"); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM eventing_outbox WHERE tenant_id = $1`, tenantID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 outbox event (analyzed only), got %d", count)
	}
}

// TestPgStore_ApplyResolution_EnqueuesResolutionEvent proves ApplyResolution
// enqueues its event in the same transaction as the UPDATE.
func TestPgStore_ApplyResolution_EnqueuesResolutionEvent(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool, store.WithEventRegion("test"))

	tenantID := uuid.New().String()
	ctx := context.Background()

	job, items := newTestJob("le-us")
	if err := s.CreateJob(ctx, tenantID, job, items, "actor-1", "corr-1"); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	itemID := job.UnmatchedItems[0].ID

	if _, err := s.ApplyResolution(ctx, tenantID, job.ID, itemID, domain.StatusApproved, "looks fine", job.LegalEntityID, "actor-2", "corr-2"); err != nil {
		t.Fatalf("ApplyResolution: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM eventing_outbox WHERE tenant_id = $1 AND event_type LIKE '%resolution-recommended'`, tenantID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 resolution-recommended outbox event, got %d", count)
	}
}

// TestPgStore_AmountsRoundTripExactly proves money survives NUMERIC(18,4)
// storage exactly (ZS-DATA-001 D06) and reads back in the same canonical
// form the analyze response used, not Postgres's padded "100.2500".
func TestPgStore_AmountsRoundTripExactly(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool, store.WithEventRegion("test"))

	tenantID := uuid.New().String()
	ctx := context.Background()

	job, items := newTestJob("le-us")
	if err := s.CreateJob(ctx, tenantID, job, items, "actor-1", "corr-1"); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	itemID := job.UnmatchedItems[0].ID

	var rawA string
	if err := pool.QueryRow(ctx, `SELECT amount_a::text FROM reconciliation_unmatched_items WHERE id = $1`, itemID).Scan(&rawA); err != nil {
		t.Fatalf("raw select: %v", err)
	}
	if rawA != "100.2500" {
		t.Fatalf("stored amount_a = %q, want 100.2500", rawA)
	}

	got, err := s.GetJobByID(ctx, tenantID, job.ID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if len(got.UnmatchedItems) != 1 {
		t.Fatalf("expected 1 item, got %d", len(got.UnmatchedItems))
	}
	it := got.UnmatchedItems[0]
	if it.AmountA != "100.25" || it.AmountB != "132.5" || it.DiscrepancyAmount != "32.25" {
		t.Fatalf("GetJobByID amounts = %s / %s / %s, want 100.25 / 132.5 / 32.25", it.AmountA, it.AmountB, it.DiscrepancyAmount)
	}

	resolved, err := s.ApplyResolution(ctx, tenantID, job.ID, itemID, domain.StatusApproved, "", job.LegalEntityID, "actor-2", "corr-2")
	if err != nil {
		t.Fatalf("ApplyResolution: %v", err)
	}
	if resolved.AmountA != "100.25" || resolved.AmountB != "132.5" || resolved.DiscrepancyAmount != "32.25" {
		t.Fatalf("ApplyResolution amounts = %s / %s / %s, want 100.25 / 132.5 / 32.25", resolved.AmountA, resolved.AmountB, resolved.DiscrepancyAmount)
	}
}
