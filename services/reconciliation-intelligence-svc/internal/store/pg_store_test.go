//go:build integration

package store_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
		"000003_bank_reconciliation_source.up.sql",
		"000004_suggestion_counterpart_line.up.sql",
		"000005_suggestion_score_provenance.up.sql",
		"000006_suggestion_data_model_names.up.sql",
		"000007_suggestion_authoritative_ref.up.sql",
		"000008_suggestion_candidate_match.up.sql",
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

const (
	testRunID  = "6f1c2a8e-3b7d-4c55-9a10-2d4e8f7b9c01"
	testLineID = "0b7e4c1a-9d2f-4e6b-8a3c-5f1d2e3c4b5a"
)

// newTestJob is a run-sourced job: one suggestion about one UNMATCHED
// bank-reconciliation statement line. Match counters are nil — not measured
// by this service.
func newTestJob(legalEntityID string) (*domain.ReconciliationJob, []domain.ReconciliationSuggestion) {
	job := &domain.ReconciliationJob{
		LegalEntityID:           legalEntityID,
		JobName:                 "test-job",
		BankReconciliationRunID: testRunID,
		SourceSystemA:           domain.SourceBankStatement,
		TotalProcessedCount:     1,
		UnmatchedCount:          1,
		Status:                  "COMPLETED",
	}
	items := []domain.ReconciliationSuggestion{{
		BankReconciliationRunID: testRunID,
		StatementLineID:         testLineID,
		SourceRecordA:           "REF-1",
		AmountA:                 "100.25",
		AmountB:                 "132.5",
		DiscrepancyAmount:       "32.25",
		DiscrepancyType:         domain.DiscrepancyMissingReference,
		ConfidenceScore:         60,
		HeuristicRule:           domain.HeuristicMissingReferenceManualReview,
		ToleranceVersion:        3,
		Recommendation:          domain.RecommendationManualReview,
		SuggestionStatus:        "RECOMMENDED",
		ResolutionNotes:         "heuristic: above tolerance",
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

	if _, err := s.ApplyResolution(ctx, tenantID, job.ID, itemID, domain.StatusApproved, "looks fine", nil, job.LegalEntityID, "actor-2", "corr-2"); err != nil {
		t.Fatalf("ApplyResolution: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM eventing_outbox WHERE tenant_id = $1 AND event_type LIKE '%resolution-recommended'`, tenantID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 resolution-recommended outbox event, got %d", count)
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT suggestion_status FROM reconciliation_unmatched_items WHERE id = $1`, itemID).Scan(&status); err != nil {
		t.Fatalf("status select: %v", err)
	}
	if status != string(domain.StatusApproved) {
		t.Fatalf("stored suggestion_status = %q, want APPROVED", status)
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

	resolved, err := s.ApplyResolution(ctx, tenantID, job.ID, itemID, domain.StatusApproved, "", nil, job.LegalEntityID, "actor-2", "corr-2")
	if err != nil {
		t.Fatalf("ApplyResolution: %v", err)
	}
	if resolved.AmountA != "100.25" || resolved.AmountB != "132.5" || resolved.DiscrepancyAmount != "32.25" {
		t.Fatalf("ApplyResolution amounts = %s / %s / %s, want 100.25 / 132.5 / 32.25", resolved.AmountA, resolved.AmountB, resolved.DiscrepancyAmount)
	}
}

// TestPgStore_ScoreProvenanceRoundTrips proves A8's two provenance fields
// survive storage and read-back: the named rule that fired and the exact
// tolerance-policy version the suggestion was evaluated against.
func TestPgStore_ScoreProvenanceRoundTrips(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool, store.WithEventRegion("test"))

	tenantID := uuid.New().String()
	ctx := context.Background()

	job, items := newTestJob("le-us")
	if err := s.CreateJob(ctx, tenantID, job, items, "actor-1", "corr-1"); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	var rule string
	var version int
	if err := pool.QueryRow(ctx, `SELECT heuristic_rule, tolerance_version
		FROM reconciliation_unmatched_items WHERE job_id = $1`, job.ID).Scan(&rule, &version); err != nil {
		t.Fatalf("raw select: %v", err)
	}
	if rule != string(domain.HeuristicMissingReferenceManualReview) || version != 3 {
		t.Fatalf("stored provenance = %q / %d, want %q / 3",
			rule, version, domain.HeuristicMissingReferenceManualReview)
	}

	got, err := s.GetJobByID(ctx, tenantID, job.ID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	it := got.UnmatchedItems[0]
	if it.HeuristicRule != domain.HeuristicMissingReferenceManualReview || it.ToleranceVersion != 3 {
		t.Fatalf("read-back provenance = %q / %d, want %q / 3",
			it.HeuristicRule, it.ToleranceVersion, domain.HeuristicMissingReferenceManualReview)
	}
}

// TestPgStore_SuggestionCarriesItsLegalEntity proves A9's item-level
// legal_entity_id is written from the job and survives the read path, not
// merely stored on the job.
func TestPgStore_SuggestionCarriesItsLegalEntity(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool, store.WithEventRegion("test"))

	tenantID := uuid.New().String()
	ctx := context.Background()

	job, items := newTestJob("le-suggestion")
	if err := s.CreateJob(ctx, tenantID, job, items, "actor-1", "corr-1"); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	var stored string
	if err := pool.QueryRow(ctx, `SELECT legal_entity_id FROM reconciliation_unmatched_items WHERE job_id = $1`, job.ID).Scan(&stored); err != nil {
		t.Fatalf("raw select: %v", err)
	}
	if stored != "le-suggestion" {
		t.Fatalf("stored legal_entity_id = %q, want le-suggestion", stored)
	}

	got, err := s.GetJobByID(ctx, tenantID, job.ID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if got.UnmatchedItems[0].LegalEntityID != "le-suggestion" {
		t.Fatalf("read-back legal_entity_id = %q, want le-suggestion", got.UnmatchedItems[0].LegalEntityID)
	}
}

// TestPgStore_RunSourcedJob_TraceableAndUnmeasuredCountersStayNull proves
// the A1 provenance survives storage — the run on the job, run and
// statement line on each suggestion — and that a run-sourced job's match
// counters are stored as NULL, not as a 0 / 0% nobody measured.
func TestPgStore_RunSourcedJob_TraceableAndUnmeasuredCountersStayNull(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool, store.WithEventRegion("test"))

	tenantID := uuid.New().String()
	ctx := context.Background()

	job, items := newTestJob("le-us")
	if err := s.CreateJob(ctx, tenantID, job, items, "actor-1", "corr-1"); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	var matchedNull, rateNull, sourceBNull bool
	if err := pool.QueryRow(ctx, `SELECT matched_count IS NULL, reconciliation_rate IS NULL, source_system_b IS NULL
		FROM reconciliation_jobs WHERE id = $1`, job.ID).Scan(&matchedNull, &rateNull, &sourceBNull); err != nil {
		t.Fatalf("raw select: %v", err)
	}
	if !matchedNull || !rateNull || !sourceBNull {
		t.Fatalf("expected NULL matched_count/reconciliation_rate/source_system_b, got null=%v/%v/%v", matchedNull, rateNull, sourceBNull)
	}

	got, err := s.GetJobByID(ctx, tenantID, job.ID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if got.BankReconciliationRunID != testRunID || got.MatchedCount != nil || got.ReconciliationRate != nil || got.SourceSystemB != "" {
		t.Fatalf("job read back as run %q matched %v rate %v source_b %q", got.BankReconciliationRunID, got.MatchedCount, got.ReconciliationRate, got.SourceSystemB)
	}
	it := got.UnmatchedItems[0]
	if it.BankReconciliationRunID != testRunID || it.StatementLineID != testLineID {
		t.Fatalf("item read back as run %q line %q, want %q %q", it.BankReconciliationRunID, it.StatementLineID, testRunID, testLineID)
	}

	listed, err := s.ListJobs(ctx, tenantID, "le-us", "", "")
	if err != nil || len(listed) != 1 || listed[0].BankReconciliationRunID != testRunID {
		t.Fatalf("ListJobs: %v %+v", err, listed)
	}

	resolved, err := s.ApplyResolution(ctx, tenantID, job.ID, it.ID, domain.StatusApproved, "", nil, job.LegalEntityID, "actor-2", "corr-2")
	if err != nil {
		t.Fatalf("ApplyResolution: %v", err)
	}
	if resolved.StatementLineID != testLineID {
		t.Fatalf("ApplyResolution returned line %q", resolved.StatementLineID)
	}
	var eventData string
	if err := pool.QueryRow(ctx, `SELECT convert_from(payload, 'UTF8') FROM eventing_outbox WHERE tenant_id = $1 AND event_type LIKE '%resolution-recommended'`, tenantID).Scan(&eventData); err != nil {
		t.Fatalf("resolution event: %v", err)
	}
	if !strings.Contains(eventData, testLineID) || !strings.Contains(eventData, testRunID) {
		t.Fatalf("resolution event does not name its statement line and run: %s", eventData)
	}
}

// TestPgStore_LegacyJob_CountersPreserved proves jobs analyzed before
// migration 000003 (caller-posted, two-sided) still read back with the
// counters they were stored with and no source record.
func TestPgStore_LegacyJob_CountersPreserved(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool, store.WithEventRegion("test"))

	tenantID := uuid.New().String()
	ctx := context.Background()

	matched, rate := 1, 50.0
	job := &domain.ReconciliationJob{
		LegalEntityID: "le-us", JobName: "legacy", SourceSystemA: domain.SourceGeneralLedger, SourceSystemB: domain.SourceBankStatement,
		TotalProcessedCount: 2, MatchedCount: &matched, UnmatchedCount: 1, ReconciliationRate: &rate, Status: "COMPLETED",
	}
	if err := s.CreateJob(ctx, tenantID, job, nil, "actor-1", "corr-1"); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	got, err := s.GetJobByID(ctx, tenantID, job.ID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if got.MatchedCount == nil || *got.MatchedCount != 1 || got.ReconciliationRate == nil || *got.ReconciliationRate != 50 ||
		got.SourceSystemB != domain.SourceBankStatement || got.BankReconciliationRunID != "" {
		t.Fatalf("legacy job read back as %+v", got)
	}
}

// TestPgStore_DuplicateEntry_CounterpartRoundTrips proves a two-record
// suggestion keeps both statement lines through storage, through the
// locked read in ApplyResolution, and onto the resolution event.
func TestPgStore_DuplicateEntry_CounterpartRoundTrips(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool, store.WithEventRegion("test"))

	tenantID := uuid.New().String()
	ctx := context.Background()
	const counterpart = "1c2d3e4f-0000-4000-8000-00000000c0de"

	job, items := newTestJob("le-us")
	items[0].DiscrepancyType = domain.DiscrepancyDuplicateEntry
	items[0].Recommendation = domain.RecommendationConfirmDuplicate
	items[0].CounterpartStatementLineID = counterpart
	single := items[0]
	single.StatementLineID = counterpart
	single.CounterpartStatementLineID = ""
	single.DiscrepancyType = domain.DiscrepancyMissingReference
	items = append(items, single)
	if err := s.CreateJob(ctx, tenantID, job, items, "actor-1", "corr-1"); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	var nullCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM reconciliation_unmatched_items
		WHERE job_id = $1 AND counterpart_statement_line_id IS NULL`, job.ID).Scan(&nullCount); err != nil {
		t.Fatalf("raw select: %v", err)
	}
	if nullCount != 1 {
		t.Fatalf("the single-line suggestion should store NULL, got %d NULL rows", nullCount)
	}

	got, err := s.GetJobByID(ctx, tenantID, job.ID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	var dupID string
	for _, it := range got.UnmatchedItems {
		if it.StatementLineID == testLineID {
			dupID = it.ID
			if it.CounterpartStatementLineID != counterpart || it.Recommendation != domain.RecommendationConfirmDuplicate {
				t.Fatalf("duplicate read back as counterpart %q / %s", it.CounterpartStatementLineID, it.Recommendation)
			}
		} else if it.CounterpartStatementLineID != "" {
			t.Fatalf("single-line suggestion read back with counterpart %q", it.CounterpartStatementLineID)
		}
	}

	resolved, err := s.ApplyResolution(ctx, tenantID, job.ID, dupID, domain.StatusApproved, "same BACS entry imported twice", nil, job.LegalEntityID, "actor-2", "corr-2")
	if err != nil {
		t.Fatalf("ApplyResolution: %v", err)
	}
	if resolved.CounterpartStatementLineID != counterpart {
		t.Fatalf("ApplyResolution returned counterpart %q", resolved.CounterpartStatementLineID)
	}
	var eventData string
	if err := pool.QueryRow(ctx, `SELECT convert_from(payload, 'UTF8') FROM eventing_outbox WHERE tenant_id = $1 AND event_type LIKE '%resolution-recommended'`, tenantID).Scan(&eventData); err != nil {
		t.Fatalf("resolution event: %v", err)
	}
	if !strings.Contains(eventData, counterpart) || !strings.Contains(eventData, testLineID) {
		t.Fatalf("resolution event does not name both lines: %s", eventData)
	}
}
