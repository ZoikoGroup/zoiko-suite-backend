//go:build integration

package store_test

// Migration 000009 (SPEC_DEVIATIONS A2b) against a real Postgres: a proposed
// canonical transaction round-trips, the CHECKs refuse a row whose named
// counterpart and rule disagree, and the down migration refuses while any
// row carries a canonical-transaction result.

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"zoiko.io/reconciliation-intelligence-svc/internal/domain"
	"zoiko.io/reconciliation-intelligence-svc/internal/store"
)

const proposedTransactionID = "7a1b2c3d-0000-4000-8000-0000000a2b2b"

func transactionAutoMatchJob() (*domain.ReconciliationJob, []domain.ReconciliationSuggestion) {
	job, items := newTestJob("le-us")
	items[0].DiscrepancyType = domain.DiscrepancyMissingReference
	items[0].Recommendation = domain.RecommendationAutoMatch
	items[0].HeuristicRule = domain.HeuristicCandidateTransactionWithinDateTolerance
	items[0].CounterpartTransactionID = proposedTransactionID
	items[0].CandidateSearchOutcome = domain.CandidateWithinDateTolerance
	items[0].DateToleranceVersion = 4
	return job, items
}

func TestPgStore_CanonicalTransactionProposal_RoundTrips(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool, store.WithEventRegion("test"))
	ctx := context.Background()
	tenantID := uuid.New().String()

	job, items := transactionAutoMatchJob()
	ambiguous := items[0]
	ambiguous.StatementLineID = "1c2d3e4f-0000-4000-8000-00000000c0de"
	ambiguous.Recommendation = domain.RecommendationManualReview
	ambiguous.HeuristicRule = domain.HeuristicCandidateTransactionAmbiguous
	ambiguous.CounterpartTransactionID = ""
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
			if it.CounterpartTransactionID != proposedTransactionID || it.CounterpartJournalID != "" ||
				it.HeuristicRule != domain.HeuristicCandidateTransactionWithinDateTolerance || it.CandidateSearchOutcome != domain.CandidateWithinDateTolerance {
				t.Fatalf("canonical AUTO_MATCH read back as %+v", it)
			}
		default:
			if it.CounterpartTransactionID != "" || it.CandidateSearchOutcome != domain.CandidateAmbiguous ||
				it.HeuristicRule != domain.HeuristicCandidateTransactionAmbiguous {
				t.Fatalf("canonical AMBIGUOUS read back as %+v", it)
			}
		}
	}
}

// The row itself refuses a proposal whose counterpart is not exactly one
// named record of the kind its rule says.
func TestPgStore_CanonicalTransactionProposal_DatabaseRefusesInconsistentRows(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool, store.WithEventRegion("test"))
	ctx := context.Background()

	for name, mutate := range map[string]func(*domain.ReconciliationSuggestion){
		"journal and transaction both named": func(it *domain.ReconciliationSuggestion) { it.CounterpartJournalID = proposedJournalID },
		"transaction under a journal rule": func(it *domain.ReconciliationSuggestion) {
			it.HeuristicRule = domain.HeuristicCandidateWithinDateTolerance
		},
		"journal under a transaction rule": func(it *domain.ReconciliationSuggestion) {
			it.CounterpartTransactionID = ""
			it.CounterpartJournalID = proposedJournalID
		},
		"transaction on a NO_CANDIDATE row": func(it *domain.ReconciliationSuggestion) {
			it.Recommendation = domain.RecommendationManualReview
			it.CandidateSearchOutcome = domain.CandidateNone
		},
		"AUTO_MATCH naming nothing": func(it *domain.ReconciliationSuggestion) { it.CounterpartTransactionID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			job, items := transactionAutoMatchJob()
			mutate(&items[0])
			if err := s.CreateJob(ctx, uuid.New().String(), job, items, "actor-1", "corr-1"); err == nil {
				t.Fatalf("expected the database to refuse: %+v", items[0])
			}
		})
	}

	// A journal proposal written the A2a way still stands under 000009.
	job, items := autoMatchJob()
	if err := s.CreateJob(ctx, uuid.New().String(), job, items, "actor-1", "corr-1"); err != nil {
		t.Fatalf("an A2a journal proposal was refused under 000009: %v", err)
	}
}

// Rolling 000009 back refuses while any suggestion carries a canonical-
// transaction result — including an AMBIGUOUS one that names nothing — and
// with none it drops the column and restores 000008's journal-only CHECKs.
func TestMigration000009_DownRefusesWhileResultsExist(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool, store.WithEventRegion("test"))
	ctx := context.Background()
	down := readMigration(t, "000009_suggestion_candidate_transaction.down.sql")

	job, items := transactionAutoMatchJob()
	items[0].Recommendation = domain.RecommendationManualReview
	items[0].HeuristicRule = domain.HeuristicCandidateTransactionAmbiguous
	items[0].CounterpartTransactionID = ""
	items[0].CandidateSearchOutcome = domain.CandidateAmbiguous
	if err := s.CreateJob(ctx, uuid.New().String(), job, items, "actor-1", "corr-1"); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if _, err := pool.Exec(ctx, down); err == nil || !strings.Contains(err.Error(), "cannot roll back 000009") {
		t.Fatalf("down with a recorded canonical result: expected refusal, got %v", err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM reconciliation_unmatched_items`); err != nil {
		t.Fatalf("clear: %v", err)
	}
	// A journal proposal is not a canonical result and does not block it.
	jjob, jitems := autoMatchJob()
	if err := s.CreateJob(ctx, uuid.New().String(), jjob, jitems, "actor-1", "corr-1"); err != nil {
		t.Fatalf("CreateJob journal: %v", err)
	}
	if _, err := pool.Exec(ctx, down); err != nil {
		t.Fatalf("down with no canonical results: %v", err)
	}
	var cols int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.columns WHERE table_name = 'reconciliation_unmatched_items'
		AND column_name = 'counterpart_transaction_id'`).Scan(&cols); err != nil || cols != 0 {
		t.Fatalf("column after down: %d %v", cols, err)
	}
	// 000008's CHECK is back: an AUTO_MATCH must name a journal.
	if _, err := pool.Exec(ctx, `UPDATE reconciliation_unmatched_items SET counterpart_journal_id = NULL`); err == nil {
		t.Fatal("000008's counterpart CHECK was not restored")
	}
}
