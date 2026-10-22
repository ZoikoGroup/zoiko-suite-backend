//go:build integration

package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/reconciliation-intelligence-svc/internal/domain"
	"zoiko.io/reconciliation-intelligence-svc/internal/store"
)

const testJournalID = "5b0f6a3e-2c1d-4e8f-9a7b-6c5d4e3f2a1b"

// approvedSuggestion stores one suggestion with the given recommendation and
// approves it, ready to be marked EXECUTED.
func approvedSuggestion(t *testing.T, s *store.PgStore, tenantID string, rec domain.ResolutionRecommendation) (jobID, itemID string) {
	t.Helper()
	ctx := context.Background()
	job, items := newTestJob("le-us")
	items[0].Recommendation = rec
	if err := s.CreateJob(ctx, tenantID, job, items, "actor-1", "corr-1"); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	itemID = job.UnmatchedItems[0].ID
	if _, err := s.ApplyResolution(ctx, tenantID, job.ID, itemID, domain.StatusApproved, "", nil, job.LegalEntityID, "actor-2", "corr-2"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	return job.ID, itemID
}

// A5: an EXECUTED WRITE_OFF or TIMING_ADJUSTMENT records the ledger journal
// that carried it out. The record is a column, not a note. It is read back
// and carried on the resolution event. Without it the decision is refused
// and the suggestion stays APPROVED.
func TestPgStore_ExecutedAccountingAction_RecordsJournalRef(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool, store.WithEventRegion("test"))
	ctx := context.Background()

	for _, rec := range []domain.ResolutionRecommendation{domain.RecommendationWriteOff, domain.RecommendationTimingAdjustment} {
		tenantID := uuid.New().String()
		jobID, itemID := approvedSuggestion(t, s, tenantID, rec)

		_, err := s.ApplyResolution(ctx, tenantID, jobID, itemID, domain.StatusExecuted, "posted JE-1042", nil, "le-us", "actor-3", "corr-3")
		if !errors.Is(err, domain.ErrAuthoritativeRefRequired) {
			t.Fatalf("%s EXECUTED without a journal: %v, want ErrAuthoritativeRefRequired", rec, err)
		}
		var status string
		if err := pool.QueryRow(ctx, `SELECT suggestion_status FROM reconciliation_unmatched_items WHERE id = $1`, itemID).Scan(&status); err != nil || status != "APPROVED" {
			t.Fatalf("%s after a refused execution: %q %v", rec, status, err)
		}

		ref := &domain.AuthoritativeRef{Kind: domain.AuthoritativeRefGLJournal, JournalID: testJournalID}
		executed, err := s.ApplyResolution(ctx, tenantID, jobID, itemID, domain.StatusExecuted, "", ref, "le-us", "actor-3", "corr-3")
		if err != nil || executed.AuthoritativeRef == nil || executed.AuthoritativeRef.JournalID != testJournalID {
			t.Fatalf("%s EXECUTED with a journal: %+v %v", rec, executed, err)
		}

		var kind, journal string
		if err := pool.QueryRow(ctx, `SELECT authoritative_ref_kind, authoritative_ref_id::text FROM reconciliation_unmatched_items WHERE id = $1`,
			itemID).Scan(&kind, &journal); err != nil || kind != "GL_JOURNAL" || journal != testJournalID {
			t.Fatalf("%s stored reference: %q %q %v", rec, kind, journal, err)
		}
		got, err := s.GetJobByID(ctx, tenantID, jobID)
		if err != nil || got.UnmatchedItems[0].AuthoritativeRef == nil || *got.UnmatchedItems[0].AuthoritativeRef != *ref {
			t.Fatalf("%s read back: %+v %v", rec, got, err)
		}
		var eventData string
		if err := pool.QueryRow(ctx, `SELECT convert_from(payload, 'UTF8') FROM eventing_outbox
			WHERE tenant_id = $1 AND event_type LIKE '%resolution-recommended' AND convert_from(payload, 'UTF8') LIKE '%EXECUTED%'`,
			tenantID).Scan(&eventData); err != nil {
			t.Fatalf("%s execution event: %v", rec, err)
		}
		if !strings.Contains(eventData, `"journal_id":"`+testJournalID+`"`) || !strings.Contains(eventData, `"kind":"GL_JOURNAL"`) {
			t.Fatalf("%s execution event does not carry the journal: %s", rec, eventData)
		}
	}
}

// A suggestion that is not EXECUTED reads back with no reference, and
// MANUAL_REVIEW may be executed either way.
func TestPgStore_ManualReviewReferenceIsOptional(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool, store.WithEventRegion("test"))
	ctx := context.Background()

	tenantID := uuid.New().String()
	jobID, itemID := approvedSuggestion(t, s, tenantID, domain.RecommendationManualReview)
	got, err := s.GetJobByID(ctx, tenantID, jobID)
	if err != nil || got.UnmatchedItems[0].AuthoritativeRef != nil {
		t.Fatalf("APPROVED suggestion read back with a reference: %+v %v", got, err)
	}
	if _, err := s.ApplyResolution(ctx, tenantID, jobID, itemID, domain.StatusExecuted, "reviewed; bank corrected the line", nil, "le-us", "actor-3", "corr-3"); err != nil {
		t.Fatalf("MANUAL_REVIEW EXECUTED without a journal: %v", err)
	}

	tenantID = uuid.New().String()
	jobID, itemID = approvedSuggestion(t, s, tenantID, domain.RecommendationManualReview)
	ref := &domain.AuthoritativeRef{Kind: domain.AuthoritativeRefGLJournal, JournalID: testJournalID}
	if _, err := s.ApplyResolution(ctx, tenantID, jobID, itemID, domain.StatusExecuted, "", ref, "le-us", "actor-3", "corr-3"); err != nil {
		t.Fatalf("MANUAL_REVIEW EXECUTED with a journal: %v", err)
	}

	tenantID = uuid.New().String()
	jobID, itemID = approvedSuggestion(t, s, tenantID, domain.RecommendationConfirmDuplicate)
	if _, err := s.ApplyResolution(ctx, tenantID, jobID, itemID, domain.StatusExecuted, "", ref, "le-us", "actor-3", "corr-3"); !errors.Is(err, domain.ErrInvalidAuthoritativeRef) {
		t.Fatalf("CONFIRM_DUPLICATE EXECUTED with a journal: %v, want ErrInvalidAuthoritativeRef", err)
	}
}

// The database holds the same rules for any writer, not only this store.
func TestPgStore_DatabaseRefusesUnreferencedExecution(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool, store.WithEventRegion("test"))
	ctx := context.Background()

	cases := []struct {
		name, rec, set, constraint string
	}{
		{"write-off executed with no journal", "WRITE_OFF", `suggestion_status = 'EXECUTED'`,
			"reconciliation_suggestion_accounting_execution_check"},
		{"timing adjustment executed with no journal", "TIMING_ADJUSTMENT", `suggestion_status = 'EXECUTED'`,
			"reconciliation_suggestion_accounting_execution_check"},
		{"journal on an approved suggestion", "WRITE_OFF", `authoritative_ref_kind = 'GL_JOURNAL', authoritative_ref_id = '` + testJournalID + `'`,
			"reconciliation_suggestion_authoritative_ref_check"},
		{"journal on an executed duplicate", "CONFIRM_DUPLICATE",
			`suggestion_status = 'EXECUTED', authoritative_ref_kind = 'GL_JOURNAL', authoritative_ref_id = '` + testJournalID + `'`,
			"reconciliation_suggestion_authoritative_ref_check"},
		{"kind with no id", "MANUAL_REVIEW", `suggestion_status = 'EXECUTED', authoritative_ref_kind = 'GL_JOURNAL'`,
			"reconciliation_suggestion_authoritative_ref_check"},
		{"unknown kind", "WRITE_OFF",
			`suggestion_status = 'EXECUTED', authoritative_ref_kind = 'FREE_TEXT', authoritative_ref_id = '` + testJournalID + `'`,
			"reconciliation_suggestion_authoritative_ref_check"},
	}
	for _, c := range cases {
		_, itemID := approvedSuggestion(t, s, uuid.New().String(), domain.ResolutionRecommendation(c.rec))
		_, err := pool.Exec(ctx, `UPDATE reconciliation_unmatched_items SET `+c.set+` WHERE id = $1`, itemID)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != c.constraint {
			t.Errorf("%s: %v, want check violation of %s", c.name, err, c.constraint)
		}
	}
}

func readMigration(t *testing.T, name string) string {
	t.Helper()
	_, filename, _, _ := runtime.Caller(0)
	sql, err := os.ReadFile(filepath.Join(filepath.Dir(filename), "../../deployments/migrations", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(sql)
}

// A suggestion EXECUTED before 000007 has no reference and keeps standing:
// the rule binds new writes only. Rolling 000007 back refuses while any
// reference is recorded, and leaves it in place.
func TestMigration000007_LegacyExecutionsStandAndDownRefuses(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool, store.WithEventRegion("test"))
	ctx := context.Background()
	up, down := readMigration(t, "000007_suggestion_authoritative_ref.up.sql"), readMigration(t, "000007_suggestion_authoritative_ref.down.sql")

	// Before 000007, a write-off could be EXECUTED with only a note.
	if _, err := pool.Exec(ctx, down); err != nil {
		t.Fatalf("down with no references: %v", err)
	}
	tenantID := uuid.New().String()
	job, items := newTestJob("le-us")
	items[0].Recommendation = domain.RecommendationWriteOff
	items[0].SuggestionStatus = domain.StatusExecuted
	items[0].ResolutionNotes = "posted JE-1042"
	if err := s.CreateJob(ctx, tenantID, job, items, "actor-1", "corr-1"); err != nil {
		t.Fatalf("legacy executed write-off: %v", err)
	}

	if _, err := pool.Exec(ctx, up); err != nil {
		t.Fatalf("up over a legacy executed write-off: %v", err)
	}
	got, err := s.GetJobByID(ctx, tenantID, job.ID)
	if err != nil || len(got.UnmatchedItems) != 1 || got.UnmatchedItems[0].SuggestionStatus != domain.StatusExecuted ||
		got.UnmatchedItems[0].AuthoritativeRef != nil {
		t.Fatalf("legacy row after 000007: %+v %v", got, err)
	}

	tenantID = uuid.New().String()
	jobID, itemID := approvedSuggestion(t, s, tenantID, domain.RecommendationWriteOff)
	ref := &domain.AuthoritativeRef{Kind: domain.AuthoritativeRefGLJournal, JournalID: testJournalID}
	if _, err := s.ApplyResolution(ctx, tenantID, jobID, itemID, domain.StatusExecuted, "", ref, "le-us", "actor-3", "corr-3"); err != nil {
		t.Fatalf("execute: %v", err)
	}
	_, err = pool.Exec(ctx, down)
	if err == nil || !strings.Contains(err.Error(), "cannot roll back 000007: recorded authoritative references exist") {
		t.Fatalf("down with a recorded reference: %v", err)
	}
	var journal string
	if err := pool.QueryRow(ctx, `SELECT authoritative_ref_id::text FROM reconciliation_unmatched_items WHERE id = $1`, itemID).Scan(&journal); err != nil || journal != testJournalID {
		t.Fatalf("reference after refused rollback: %q %v", journal, err)
	}
}
