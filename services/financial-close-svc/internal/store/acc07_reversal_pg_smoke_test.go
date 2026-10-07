package store_test

// ACC-07's own closure of the "Auto-reversal duplicates" negative path is
// exercised only by handler_test.go's in-memory stub elsewhere in this
// package — this file is its real-Postgres coverage, against the actual
// append-only trigger and UNIQUE(recognition_instance_id) constraint
// (migration 000012), not a stub's map mutation. Skips (not fails) if
// TEST_DATABASE_URL isn't set, same convention as every other
// TestPgStore_* test in this package.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/financial-close-svc/internal/domain"
	svcmiddleware "zoiko.io/financial-close-svc/internal/middleware"
	"zoiko.io/financial-close-svc/internal/store"
)

func recognizedAccrualInstance(t *testing.T, ctx context.Context, s *store.PgStore, tenantID string) *domain.RecognitionInstance {
	t.Helper()
	sch := &domain.AccrualSchedule{
		ScheduleID: uuid.New().String(), TenantID: tenantID, LegalEntityID: "le-1",
		Description: "test accrual", PolicyVersion: "v1", TotalAmount: 900,
		StartFiscalPeriod: "2026-01", PeriodCount: 3,
		DebitAccountCode: "6100-AuditFee", CreditAccountCode: "2100-AccruedLiabilities",
		Status: domain.AccrualStatusApproved, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
	if err := s.CreateAccrualSchedule(ctx, sch); err != nil {
		t.Fatalf("CreateAccrualSchedule: %v", err)
	}
	inst := &domain.RecognitionInstance{
		RecognitionInstanceID: uuid.New().String(), ScheduleID: sch.ScheduleID, FiscalPeriod: "2026-01",
		RecognizedAmount: 300, JournalID: "journal-" + uuid.New().String(),
		RecognizedAt: time.Now().UTC(), RecognizedByPrincipalID: "preparer-1",
	}
	if _, err := s.CreateRecognitionInstance(ctx, inst); err != nil {
		t.Fatalf("CreateRecognitionInstance: %v", err)
	}
	return inst
}

func TestPgStore_ACC07_CreateRecognitionReversal_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	inst := recognizedAccrualInstance(t, ctx, s, tenantID)

	rev := &domain.RecognitionReversal{
		RecognitionReversalID: uuid.New().String(), ScheduleID: inst.ScheduleID,
		RecognitionInstanceID: inst.RecognitionInstanceID, ReversingJournalID: "reversing-journal-1",
		Reason: "estimate corrected", ReversedAt: time.Now().UTC(), ReversedByPrincipalID: "reverser-1",
	}
	created, err := s.CreateRecognitionReversal(ctx, rev)
	if err != nil {
		t.Fatalf("CreateRecognitionReversal: %v", err)
	}
	if !created {
		t.Fatal("expected created=true on the first reversal")
	}

	got, err := s.GetRecognitionReversalByInstance(ctx, inst.RecognitionInstanceID)
	if err != nil {
		t.Fatalf("GetRecognitionReversalByInstance: %v", err)
	}
	if got == nil || got.ReversingJournalID != "reversing-journal-1" {
		t.Fatalf("expected the stored reversal to round-trip, got %+v", got)
	}
}

// TestPgStore_ACC07_DuplicateReversal_ReturnsExisting_RealDB is the
// negative-path proof against the real UNIQUE constraint: a second
// CreateRecognitionReversal for the same recognition_instance_id must
// resolve to the ORIGINAL reversal, never a second row.
func TestPgStore_ACC07_DuplicateReversal_ReturnsExisting_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	inst := recognizedAccrualInstance(t, ctx, s, tenantID)

	rev1 := &domain.RecognitionReversal{
		RecognitionReversalID: uuid.New().String(), ScheduleID: inst.ScheduleID,
		RecognitionInstanceID: inst.RecognitionInstanceID, ReversingJournalID: "reversing-journal-1",
		Reason: "first attempt", ReversedAt: time.Now().UTC(), ReversedByPrincipalID: "reverser-1",
	}
	if _, err := s.CreateRecognitionReversal(ctx, rev1); err != nil {
		t.Fatalf("first CreateRecognitionReversal: %v", err)
	}

	// A second, independently-generated reversal attempt for the SAME
	// instance (simulating a retry with a fresh reversal id, as a real
	// client would generate).
	rev2 := &domain.RecognitionReversal{
		RecognitionReversalID: uuid.New().String(), ScheduleID: inst.ScheduleID,
		RecognitionInstanceID: inst.RecognitionInstanceID, ReversingJournalID: "reversing-journal-2",
		Reason: "retry attempt", ReversedAt: time.Now().UTC(), ReversedByPrincipalID: "reverser-2",
	}
	created2, err := s.CreateRecognitionReversal(ctx, rev2)
	if err != nil {
		t.Fatalf("second CreateRecognitionReversal: %v", err)
	}
	if created2 {
		t.Fatal("expected created=false — the UNIQUE constraint should have resolved to the original reversal")
	}
	if rev2.RecognitionReversalID != rev1.RecognitionReversalID || rev2.ReversingJournalID != "reversing-journal-1" {
		t.Fatalf("expected rev2 to be mutated in place to the ORIGINAL reversal, got %+v", rev2)
	}
}

// TestPgStore_ACC07_RecognitionReversals_RejectsUpdateAndDelete_RealDB
// proves the append-only trigger at the database level.
func TestPgStore_ACC07_RecognitionReversals_RejectsUpdateAndDelete_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	inst := recognizedAccrualInstance(t, ctx, s, tenantID)

	rev := &domain.RecognitionReversal{
		RecognitionReversalID: uuid.New().String(), ScheduleID: inst.ScheduleID,
		RecognitionInstanceID: inst.RecognitionInstanceID, ReversingJournalID: "reversing-journal-1",
		Reason: "estimate corrected", ReversedAt: time.Now().UTC(), ReversedByPrincipalID: "reverser-1",
	}
	if _, err := s.CreateRecognitionReversal(ctx, rev); err != nil {
		t.Fatalf("CreateRecognitionReversal: %v", err)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `UPDATE accrual_recognition_reversals SET reason = 'tampered'`); err == nil {
		t.Fatal("expected UPDATE on accrual_recognition_reversals to be rejected by the append-only trigger")
	}

	conn2, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer conn2.Release()
	if _, err := conn2.Exec(ctx, `DELETE FROM accrual_recognition_reversals`); err == nil {
		t.Fatal("expected DELETE on accrual_recognition_reversals to be rejected by the append-only trigger")
	}
}
