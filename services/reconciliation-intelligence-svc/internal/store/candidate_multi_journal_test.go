//go:build integration

package store_test

// Migration 000010 (SPEC_DEVIATIONS A2b part 2) against a real Postgres: a
// proposed two-journal combination round-trips (including the UUID[]
// column), the CHECKs refuse a row whose named counterpart shape and rule
// disagree, and the down migration refuses while any row carries a
// multi-journal result.

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"zoiko.io/reconciliation-intelligence-svc/internal/domain"
	"zoiko.io/reconciliation-intelligence-svc/internal/store"
)

func multiJournalAutoMatchJob(journalA, journalB string) (*domain.ReconciliationJob, []domain.ReconciliationSuggestion) {
	job, items := newTestJob("le-us")
	items[0].DiscrepancyType = domain.DiscrepancyMissingReference
	items[0].Recommendation = domain.RecommendationAutoMatch
	items[0].HeuristicRule = domain.HeuristicCandidateMultiJournalWithinDateTolerance
	items[0].CounterpartJournalIDs = []string{journalA, journalB}
	items[0].CandidateSearchOutcome = domain.CandidateWithinDateTolerance
	items[0].DateToleranceVersion = 4
	return job, items
}

func TestPgStore_MultiJournalProposal_RoundTrips(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool, store.WithEventRegion("test"))
	ctx := context.Background()
	tenantID := uuid.New().String()
	journalA, journalB := uuid.New().String(), uuid.New().String()

	job, items := multiJournalAutoMatchJob(journalA, journalB)
	ambiguous := items[0]
	ambiguous.StatementLineID = "1c2d3e4f-0000-4000-8000-00000000c0de"
	ambiguous.Recommendation = domain.RecommendationManualReview
	ambiguous.HeuristicRule = domain.HeuristicCandidateMultiJournalAmbiguous
	ambiguous.CounterpartJournalIDs = nil
	ambiguous.CandidateSearchOutcome = domain.CandidateAmbiguous
	items = append(items, ambiguous)
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
			if len(it.CounterpartJournalIDs) != 2 || it.CounterpartJournalID != "" || it.CounterpartTransactionID != "" ||
				it.HeuristicRule != domain.HeuristicCandidateMultiJournalWithinDateTolerance || it.CandidateSearchOutcome != domain.CandidateWithinDateTolerance {
				t.Fatalf("multi-journal AUTO_MATCH read back as %+v", it)
			}
			seen := map[string]bool{it.CounterpartJournalIDs[0]: true, it.CounterpartJournalIDs[1]: true}
			if !seen[journalA] || !seen[journalB] {
				t.Fatalf("expected both journal ids %s and %s, got %v", journalA, journalB, it.CounterpartJournalIDs)
			}
		default:
			if len(it.CounterpartJournalIDs) != 0 || it.CandidateSearchOutcome != domain.CandidateAmbiguous ||
				it.HeuristicRule != domain.HeuristicCandidateMultiJournalAmbiguous {
				t.Fatalf("multi-journal AMBIGUOUS read back as %+v", it)
			}
		}
	}
}

// The row itself refuses a proposal whose counterpart shape and rule
// disagree, or whose array does not name exactly two journals.
func TestPgStore_MultiJournalProposal_DatabaseRefusesInconsistentRows(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool, store.WithEventRegion("test"))
	ctx := context.Background()
	journalA, journalB := uuid.New().String(), uuid.New().String()

	for name, mutate := range map[string]func(*domain.ReconciliationSuggestion){
		"single journal and array both named": func(it *domain.ReconciliationSuggestion) { it.CounterpartJournalID = journalA },
		"transaction and array both named":    func(it *domain.ReconciliationSuggestion) { it.CounterpartTransactionID = journalA },
		"array under a single-journal rule": func(it *domain.ReconciliationSuggestion) {
			it.HeuristicRule = domain.HeuristicCandidateWithinDateTolerance
		},
		"single journal under a multi-journal rule": func(it *domain.ReconciliationSuggestion) {
			it.CounterpartJournalIDs = nil
			it.CounterpartJournalID = journalA
		},
		"array on a NO_CANDIDATE row": func(it *domain.ReconciliationSuggestion) {
			it.Recommendation = domain.RecommendationManualReview
			it.CandidateSearchOutcome = domain.CandidateNone
		},
		"AUTO_MATCH naming nothing": func(it *domain.ReconciliationSuggestion) { it.CounterpartJournalIDs = nil },
		"array of only one journal": func(it *domain.ReconciliationSuggestion) {
			it.CounterpartJournalIDs = []string{journalA}
		},
		"array of three journals": func(it *domain.ReconciliationSuggestion) {
			it.CounterpartJournalIDs = []string{journalA, journalB, uuid.New().String()}
		},
	} {
		t.Run(name, func(t *testing.T) {
			job, items := multiJournalAutoMatchJob(journalA, journalB)
			mutate(&items[0])
			if err := s.CreateJob(ctx, uuid.New().String(), job, items, "actor-1", "corr-1"); err == nil {
				t.Fatalf("expected the database to refuse: %+v", items[0])
			}
		})
	}

	// A single-journal proposal written the A2a way still stands under 000010.
	job, items := autoMatchJob()
	if err := s.CreateJob(ctx, uuid.New().String(), job, items, "actor-1", "corr-1"); err != nil {
		t.Fatalf("an A2a single-journal proposal was refused under 000010: %v", err)
	}
}

// Rolling 000010 back refuses while any suggestion carries a multi-journal
// result — including an AMBIGUOUS one that names nothing — and with none it
// drops the column and restores 000009's two-counterpart CHECKs.
func TestMigration000010_DownRefusesWhileResultsExist(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool, store.WithEventRegion("test"))
	ctx := context.Background()
	down := readMigration(t, "000010_suggestion_candidate_multi_journal.down.sql")
	journalA, journalB := uuid.New().String(), uuid.New().String()

	job, items := multiJournalAutoMatchJob(journalA, journalB)
	items[0].Recommendation = domain.RecommendationManualReview
	items[0].HeuristicRule = domain.HeuristicCandidateMultiJournalAmbiguous
	items[0].CounterpartJournalIDs = nil
	items[0].CandidateSearchOutcome = domain.CandidateAmbiguous
	if err := s.CreateJob(ctx, uuid.New().String(), job, items, "actor-1", "corr-1"); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if _, err := pool.Exec(ctx, down); err == nil || !strings.Contains(err.Error(), "cannot roll back 000010") {
		t.Fatalf("down with a recorded multi-journal result: expected refusal, got %v", err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM reconciliation_unmatched_items`); err != nil {
		t.Fatalf("clear: %v", err)
	}
	// A single-journal proposal is not a multi-journal result and does not
	// block it.
	jjob, jitems := autoMatchJob()
	if err := s.CreateJob(ctx, uuid.New().String(), jjob, jitems, "actor-1", "corr-1"); err != nil {
		t.Fatalf("CreateJob journal: %v", err)
	}
	if _, err := pool.Exec(ctx, down); err != nil {
		t.Fatalf("down with no multi-journal results: %v", err)
	}
	var cols int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.columns WHERE table_name = 'reconciliation_unmatched_items'
		AND column_name = 'counterpart_journal_ids'`).Scan(&cols); err != nil || cols != 0 {
		t.Fatalf("column after down: %d %v", cols, err)
	}
	// 000009's CHECK is back: an AUTO_MATCH must name a single journal or
	// transaction.
	if _, err := pool.Exec(ctx, `UPDATE reconciliation_unmatched_items SET counterpart_journal_id = NULL`); err == nil {
		t.Fatal("000009's counterpart CHECK was not restored")
	}
	// No restore here: openTestPool drops and reapplies every migration from
	// scratch for the next test, the same convention
	// TestMigration000009_DownRefusesWhileResultsExist already relies on.
}
