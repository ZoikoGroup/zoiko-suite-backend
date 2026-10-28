//go:build integration

package store_test

// Migration 000008 (SPEC_DEVIATIONS A2a) against a real Postgres: the
// candidate-search fields round-trip, the CHECKs refuse an inconsistent
// proposal whoever writes it, and the down migration refuses while any row
// carries a candidate-search result.

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"zoiko.io/reconciliation-intelligence-svc/internal/domain"
	"zoiko.io/reconciliation-intelligence-svc/internal/store"
)

const proposedJournalID = "7a1b2c3d-0000-4000-8000-0000000a2a2a"

func autoMatchJob() (*domain.ReconciliationJob, []domain.ReconciliationSuggestion) {
	job, items := newTestJob("le-us")
	items[0].DiscrepancyType = domain.DiscrepancyMissingReference
	items[0].Recommendation = domain.RecommendationAutoMatch
	items[0].HeuristicRule = domain.HeuristicCandidateWithinDateTolerance
	items[0].CounterpartJournalID = proposedJournalID
	items[0].CandidateSearchOutcome = domain.CandidateWithinDateTolerance
	items[0].DateToleranceVersion = 4
	return job, items
}

func TestPgStore_CandidateProposal_RoundTrips(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool, store.WithEventRegion("test"))
	ctx := context.Background()
	tenantID := uuid.New().String()

	job, items := autoMatchJob()
	none := items[0]
	none.StatementLineID = "1c2d3e4f-0000-4000-8000-00000000c0de"
	none.Recommendation = domain.RecommendationManualReview
	none.HeuristicRule = domain.HeuristicMissingReferenceManualReview
	none.CounterpartJournalID = ""
	none.CandidateSearchOutcome = domain.CandidateNone
	items = append(items, none)
	if err := s.CreateJob(ctx, tenantID, job, items, "actor-1", "corr-1"); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	got, err := s.GetJobByID(ctx, tenantID, job.ID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	for _, it := range got.UnmatchedItems {
		switch it.StatementLineID {
		case testLineID:
			if it.CounterpartJournalID != proposedJournalID || it.CandidateSearchOutcome != domain.CandidateWithinDateTolerance || it.DateToleranceVersion != 4 {
				t.Fatalf("AUTO_MATCH read back as %+v", it)
			}
		default:
			if it.CounterpartJournalID != "" || it.CandidateSearchOutcome != domain.CandidateNone || it.DateToleranceVersion != 4 {
				t.Fatalf("NO_CANDIDATE read back as %+v", it)
			}
		}
	}
}

// The row itself refuses a proposal that is not one named journal under a
// recorded outcome and tolerance version.
func TestPgStore_CandidateProposal_DatabaseRefusesInconsistentRows(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool, store.WithEventRegion("test"))
	ctx := context.Background()

	for name, mutate := range map[string]func(*domain.ReconciliationSuggestion){
		"AUTO_MATCH without a journal": func(it *domain.ReconciliationSuggestion) {
			it.CounterpartJournalID = ""
			it.CandidateSearchOutcome = domain.CandidateNone
		},
		"journal without an outcome": func(it *domain.ReconciliationSuggestion) {
			it.Recommendation = domain.RecommendationManualReview
			it.CandidateSearchOutcome = ""
		},
		"journal on a NO_CANDIDATE row": func(it *domain.ReconciliationSuggestion) {
			it.Recommendation = domain.RecommendationManualReview
			it.CandidateSearchOutcome = domain.CandidateNone
		},
		"outcome without a version": func(it *domain.ReconciliationSuggestion) { it.DateToleranceVersion = 0 },
		"unknown outcome":           func(it *domain.ReconciliationSuggestion) { it.CandidateSearchOutcome = "PROBABLY" },
	} {
		t.Run(name, func(t *testing.T) {
			job, items := autoMatchJob()
			mutate(&items[0])
			if err := s.CreateJob(ctx, uuid.New().String(), job, items, "actor-1", "corr-1"); err == nil {
				t.Fatalf("expected the database to refuse: %+v", items[0])
			}
		})
	}
}

// Rolling 000008 back refuses while any suggestion carries a candidate-search
// result, leaving it in place; with none it drops the columns.
func TestMigration000008_DownRefusesWhileResultsExist(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool, store.WithEventRegion("test"))
	ctx := context.Background()
	down := readMigration(t, "000008_suggestion_candidate_match.down.sql")

	tenantID := uuid.New().String()
	job, items := autoMatchJob()
	if err := s.CreateJob(ctx, tenantID, job, items, "actor-1", "corr-1"); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	_, err := pool.Exec(ctx, down)
	if err == nil || !strings.Contains(err.Error(), "cannot roll back 000008") {
		t.Fatalf("down with a recorded proposal: expected refusal, got %v", err)
	}
	var journal string
	if err := pool.QueryRow(ctx, `SELECT counterpart_journal_id::text FROM reconciliation_unmatched_items WHERE job_id = $1`, job.ID).Scan(&journal); err != nil || journal != proposedJournalID {
		t.Fatalf("proposal after refused rollback: %q %v", journal, err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM reconciliation_unmatched_items`); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := pool.Exec(ctx, down); err != nil {
		t.Fatalf("down with no results: %v", err)
	}
	var cols int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.columns WHERE table_name = 'reconciliation_unmatched_items'
		AND column_name IN ('counterpart_journal_id', 'candidate_search_outcome', 'date_tolerance_version')`).Scan(&cols); err != nil || cols != 0 {
		t.Fatalf("columns after down: %d %v", cols, err)
	}
}
