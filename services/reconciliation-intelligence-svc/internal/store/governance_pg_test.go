//go:build integration

package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"zoiko.io/reconciliation-intelligence-svc/internal/domain"
	"zoiko.io/reconciliation-intelligence-svc/internal/store"
)

// The SoD, version and archive rules are enforced in the store transaction,
// on the locked job row, not only by the HTTP handler.
func TestPgStore_SoDAndVersionAtDataBoundary(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool, store.WithEventRegion("test"))
	ctx := context.Background()
	tenantID := uuid.New().String()

	job, items := twoItemJob()
	if err := s.CreateJob(ctx, tenantID, job, items, "requester-1", "corr-1"); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	got, err := s.GetJobByID(ctx, tenantID, job.ID)
	if err != nil || got.RequestedBy != "requester-1" || got.Version != 1 {
		t.Fatalf("job must persist requester and version 1: %+v %v", got, err)
	}
	item := job.UnmatchedItems[0].ID

	if _, err := s.ApplyResolution(ctx, tenantID, job.ID, item, domain.StatusApproved, "self", nil, job.LegalEntityID, "requester-1", "c", 0); !errors.Is(err, domain.ErrSegregationOfDuties) {
		t.Fatalf("requester approval at the store: %v, want ErrSegregationOfDuties", err)
	}
	if _, err := s.ApplyResolution(ctx, tenantID, job.ID, item, domain.StatusApproved, "ok", nil, job.LegalEntityID, "reviewer-1", "c", 5); !errors.Is(err, domain.ErrVersionMismatch) {
		t.Fatalf("stale version at the store: %v, want ErrVersionMismatch", err)
	}
	decided, err := s.ApplyResolution(ctx, tenantID, job.ID, item, domain.StatusApproved, "ok", nil, job.LegalEntityID, "reviewer-1", "c", 1)
	if err != nil || decided.DecidedBy != "reviewer-1" || decided.DecidedAt == nil {
		t.Fatalf("reviewer approval: %+v %v", decided, err)
	}
	if got, _ := s.GetJobByID(ctx, tenantID, job.ID); got.Version != 2 {
		t.Fatalf("version after a decision = %d, want 2", got.Version)
	}

	// Another tenant cannot see or act on the job (RLS + predicate).
	if _, err := s.ApplyResolution(ctx, uuid.New().String(), job.ID, item, domain.StatusRejected, "x", nil, job.LegalEntityID, "reviewer-1", "c", 0); !errors.Is(err, domain.ErrJobNotFound) {
		t.Fatalf("cross-tenant decision: %v, want ErrJobNotFound", err)
	}
	if _, err := s.ArchiveJob(ctx, uuid.New().String(), job.ID, "reviewer-1", "c", 0); !errors.Is(err, domain.ErrJobNotFound) {
		t.Fatalf("cross-tenant archive: %v, want ErrJobNotFound", err)
	}

	archived, err := s.ArchiveJob(ctx, tenantID, job.ID, "reviewer-1", "c", 2)
	if err != nil || archived.Status != "ARCHIVED" || archived.Version != 3 || archived.ArchivedBy != "reviewer-1" || archived.ArchivedAt == nil {
		t.Fatalf("archive: %+v %v", archived, err)
	}
	if _, err := s.ArchiveJob(ctx, tenantID, job.ID, "reviewer-1", "c", 0); !errors.Is(err, domain.ErrJobArchived) {
		t.Fatalf("second archive: %v, want ErrJobArchived", err)
	}
	if _, err := s.ApplyResolution(ctx, tenantID, job.ID, job.UnmatchedItems[1].ID, domain.StatusRejected, "x", nil, job.LegalEntityID, "reviewer-1", "c", 0); !errors.Is(err, domain.ErrJobArchived) {
		t.Fatalf("decision on archived job: %v, want ErrJobArchived", err)
	}
	var events int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eventing_outbox WHERE tenant_id = $1 AND event_type LIKE '%job.archived'`, tenantID).Scan(&events); err != nil || events != 1 {
		t.Fatalf("archive must emit exactly one archived event: %d %v", events, err)
	}
}

// Two reviewers deciding concurrently against the same If-Match version:
// the job row lock serializes them, so exactly one wins and the other gets
// ErrVersionMismatch — no lost update.
func TestPgStore_ConcurrentDecisions_OneWins(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool, store.WithEventRegion("test"))
	ctx := context.Background()
	tenantID := uuid.New().String()

	job, items := twoItemJob()
	if err := s.CreateJob(ctx, tenantID, job, items, "requester-1", "corr-1"); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if len(job.UnmatchedItems) < 2 {
		t.Fatalf("fixture needs two suggestions")
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.ApplyResolution(ctx, tenantID, job.ID, job.UnmatchedItems[i].ID, domain.StatusRejected, "parallel", nil, job.LegalEntityID, "reviewer-"+string(rune('a'+i)), "c", 1)
		}(i)
	}
	wg.Wait()
	won, lost := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, domain.ErrVersionMismatch):
			lost++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if won != 1 || lost != 1 {
		t.Fatalf("won=%d lost=%d, want exactly one of each", won, lost)
	}
}

// twoItemJob is newTestJob with a second suggestion on its own statement line.
func twoItemJob() (*domain.ReconciliationJob, []domain.ReconciliationSuggestion) {
	job, items := newTestJob("le-us")
	second := items[0]
	second.StatementLineID = uuid.New().String()
	second.SourceRecordA = "REF-2"
	job.TotalProcessedCount, job.UnmatchedCount = 2, 2
	return job, append(items, second)
}
