package store_test

// ACC-18's own closure of GetLineageAsOf/VerifyTracePath/
// QuarantineBrokenLineage is exercised only by handler_test.go's
// in-memory stub elsewhere in this package — this file is its
// real-Postgres coverage, against the actual append-only triggers and
// the migration 000014 UNIQUE constraint, not a stub's map mutation.
// Skips (not fails) if TEST_DATABASE_URL isn't set.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/financial-close-svc/internal/domain"
	svcmiddleware "zoiko.io/financial-close-svc/internal/middleware"
	"zoiko.io/financial-close-svc/internal/store"
)

func TestPgStore_ACC18_ListLineageEdgesToAsOf_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	early := time.Now().UTC().Add(-48 * time.Hour)
	late := time.Now().UTC()
	journalID := uuid.New().String()

	if err := s.RecordLineageEdge(ctx, &domain.LineageEdge{
		EdgeID: uuid.New().String(), LegalEntityID: "le-1",
		FromType: "allocation_run", FromID: "run-1", ToType: "journal", ToID: journalID, RecordedAt: early,
	}); err != nil {
		t.Fatalf("RecordLineageEdge (early): %v", err)
	}
	if err := s.RecordLineageEdge(ctx, &domain.LineageEdge{
		EdgeID: uuid.New().String(), LegalEntityID: "le-1",
		FromType: "allocation_run", FromID: "run-2", ToType: "journal", ToID: journalID, RecordedAt: late,
	}); err != nil {
		t.Fatalf("RecordLineageEdge (late): %v", err)
	}

	watermark := early.Add(1 * time.Hour)
	edges, err := s.ListLineageEdgesToAsOf(ctx, "journal", journalID, watermark)
	if err != nil {
		t.Fatalf("ListLineageEdgesToAsOf: %v", err)
	}
	if len(edges) != 1 || edges[0].FromID != "run-1" {
		t.Fatalf("expected only the early edge at this watermark, got %+v", edges)
	}
}

func TestPgStore_ACC18_TracePathVerification_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	v := &domain.TracePathVerification{
		VerificationID: uuid.New().String(), LegalEntityID: "le-1",
		FromType: "allocation_run", FromID: "run-1", ToType: "journal", ToID: "journal-1",
		Verified: true, VerifiedAt: time.Now().UTC(), VerifiedByPrincipalID: "auditor-1",
	}
	if err := s.CreateTracePathVerification(ctx, v); err != nil {
		t.Fatalf("CreateTracePathVerification: %v", err)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `UPDATE lineage_trace_verifications SET verified = false`); err == nil {
		t.Fatal("expected UPDATE on lineage_trace_verifications to be rejected by the append-only trigger")
	}
}

func TestPgStore_ACC18_QuarantinedLineageGap_IdempotentAndRealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	gap := &domain.QuarantinedLineageGap{
		QuarantineID: uuid.New().String(), LegalEntityID: "le-1",
		FromType: "allocation_run", FromID: "run-1", ToType: "journal", ToID: "journal-1",
		Reason: "predates lineage recording", QuarantinedAt: time.Now().UTC(), QuarantinedByPrincipalID: "preparer-1",
	}
	if err := s.CreateQuarantinedLineageGap(ctx, gap); err != nil {
		t.Fatalf("first CreateQuarantinedLineageGap: %v", err)
	}
	// A repeated quarantine of the SAME gap (different quarantine_id, same
	// from/to) must be a no-op, not a duplicate row — the migration's own
	// UNIQUE constraint.
	dup := &domain.QuarantinedLineageGap{
		QuarantineID: uuid.New().String(), LegalEntityID: "le-1",
		FromType: "allocation_run", FromID: "run-1", ToType: "journal", ToID: "journal-1",
		Reason: "repeated attempt", QuarantinedAt: time.Now().UTC(), QuarantinedByPrincipalID: "preparer-2",
	}
	if err := s.CreateQuarantinedLineageGap(ctx, dup); err != nil {
		t.Fatalf("second CreateQuarantinedLineageGap: %v", err)
	}

	gaps, err := s.ListQuarantinedLineageGaps(ctx, "le-1")
	if err != nil {
		t.Fatalf("ListQuarantinedLineageGaps: %v", err)
	}
	if len(gaps) != 1 {
		t.Fatalf("expected exactly 1 quarantined gap (idempotent), got %d", len(gaps))
	}
}
