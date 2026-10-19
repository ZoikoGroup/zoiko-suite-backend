package store_test

// ACC-11's own lifecycle store methods (AcknowledgeCounterparty,
// DisputeIntercompany, ResolveMismatch) are exercised only by
// handler_test.go's in-memory stub elsewhere in this package — this file
// is their real-Postgres coverage, against the actual guarded UPDATE ...
// WHERE match_status = $n clauses and the migration 000003 CHECK
// constraint. Skips (not fails) if TEST_DATABASE_URL isn't set.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
	svcmiddleware "zoiko.io/intercompany-accounting-svc/internal/middleware"
	"zoiko.io/intercompany-accounting-svc/internal/store"
)

func TestPgStore_ACC11_FullLifecycle_AcknowledgeDisputeResolve_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("test"))

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	entry := newTestEntry(tenantID, uuid.New().String())
	if _, err := s.CreateEntry(ctx, entry, "actor-1", "corr-1"); err != nil {
		t.Fatalf("CreateEntry: %v", err)
	}

	if err := s.AcknowledgeCounterparty(ctx, entry.IntercompanyEntryID, "counterparty-user", "corr-ack"); err != nil {
		t.Fatalf("AcknowledgeCounterparty: %v", err)
	}
	got, err := s.GetEntry(ctx, entry.IntercompanyEntryID)
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if got.MatchStatus != domain.MatchStatusAwaitingCounterparty {
		t.Fatalf("expected AWAITING_COUNTERPARTY, got %q", got.MatchStatus)
	}
	if got.AcknowledgedAt == nil || got.AcknowledgedByPrincipalID == nil || *got.AcknowledgedByPrincipalID != "counterparty-user" {
		t.Fatalf("expected acknowledgement recorded, got %+v", got)
	}

	// Simulate the match producing a MISMATCH via the existing UpdateMatch
	// path (the real mismatch decision is the handler's job, not this
	// store method's — see handler.go's MatchEntry).
	reason := "amount_mismatch: test"
	targetJ := uuid.New().String()
	entry.TargetJournalID = &targetJ
	entry.MatchStatus = domain.MatchStatusMismatch
	entry.MismatchReason = &reason
	if err := s.UpdateMatch(ctx, entry, "actor-1", "corr-2"); err != nil {
		t.Fatalf("UpdateMatch: %v", err)
	}

	if err := s.DisputeIntercompany(ctx, entry.IntercompanyEntryID, "disputer-1", "the counterparty amount looks wrong", "corr-dispute"); err != nil {
		t.Fatalf("DisputeIntercompany: %v", err)
	}
	got, err = s.GetEntry(ctx, entry.IntercompanyEntryID)
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if got.MatchStatus != domain.MatchStatusDisputed {
		t.Fatalf("expected DISPUTED, got %q", got.MatchStatus)
	}
	if got.DisputeReason == nil || *got.DisputeReason != "the counterparty amount looks wrong" {
		t.Fatalf("expected dispute reason recorded, got %+v", got.DisputeReason)
	}

	if err := s.ResolveMismatch(ctx, entry.IntercompanyEntryID, "resolver-1", "confirmed FX rounding, accepted", domain.ResolutionAccepted, "corr-resolve"); err != nil {
		t.Fatalf("ResolveMismatch: %v", err)
	}
	got, err = s.GetEntry(ctx, entry.IntercompanyEntryID)
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if got.MatchStatus != domain.MatchStatusResolved {
		t.Fatalf("expected RESOLVED, got %q", got.MatchStatus)
	}
	if got.ResolvedByPrincipalID == nil || *got.ResolvedByPrincipalID != "resolver-1" {
		t.Fatalf("expected resolver recorded, got %+v", got.ResolvedByPrincipalID)
	}
}

// TestPgStore_ACC11_DisputeFromWrongStatus_Refused proves the guarded
// UPDATE's WHERE clause against the real database, not just the stub's
// if-statement.
func TestPgStore_ACC11_DisputeFromWrongStatus_Refused(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("test"))

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	entry := newTestEntry(tenantID, uuid.New().String()) // starts UNMATCHED
	if _, err := s.CreateEntry(ctx, entry, "actor-1", "corr-1"); err != nil {
		t.Fatalf("CreateEntry: %v", err)
	}

	err := s.DisputeIntercompany(ctx, entry.IntercompanyEntryID, "disputer-1", "premature dispute", "corr-2")
	if err == nil {
		t.Fatal("expected DisputeIntercompany to refuse a pair that is still UNMATCHED")
	}
}

// TestPgStore_ACC11_InvalidMatchStatus_RejectedByCheckConstraint proves
// migration 000003's CHECK constraint is real, not just documentation —
// negative-path control for the closed state set.
func TestPgStore_ACC11_InvalidMatchStatus_RejectedByCheckConstraint(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("test"))

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	entry := newTestEntry(tenantID, uuid.New().String())
	if _, err := s.CreateEntry(ctx, entry, "actor-1", "corr-1"); err != nil {
		t.Fatalf("CreateEntry: %v", err)
	}

	// Written directly: UpdateMatch refuses any target status other than
	// MATCHED/MISMATCH before reaching the database, and this test is about
	// the constraint itself.
	if _, err := pool.Exec(ctx, `UPDATE intercompany_entries SET match_status = 'NOT_A_REAL_STATUS' WHERE intercompany_entry_id = $1`,
		entry.IntercompanyEntryID); err == nil {
		t.Fatal("expected the CHECK constraint to reject an unrecognized match_status value")
	}

	entry.MatchStatus = "NOT_A_REAL_STATUS"
	if err := s.UpdateMatch(ctx, entry, "actor-1", "corr-2"); !errors.Is(err, domain.ErrInvalidPairTransition) {
		t.Fatalf("expected UpdateMatch to refuse a non-match outcome, got %v", err)
	}
}

// TestPgStore_ACC11_UpdateMatchOnDisputedPair_Refused proves UpdateMatch's
// status guard against the real database: a MatchIntercompany outcome that
// arrives after the pair was disputed must not overwrite DISPUTED.
func TestPgStore_ACC11_UpdateMatchOnDisputedPair_Refused(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("test"))

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	entry := newTestEntry(tenantID, uuid.New().String())
	if _, err := s.CreateEntry(ctx, entry, "actor-1", "corr-1"); err != nil {
		t.Fatalf("CreateEntry: %v", err)
	}
	reason := "amount_mismatch: test"
	targetJ := uuid.New().String()
	entry.TargetJournalID = &targetJ
	entry.MatchStatus = domain.MatchStatusMismatch
	entry.MismatchReason = &reason
	if err := s.UpdateMatch(ctx, entry, "actor-1", "corr-2"); err != nil {
		t.Fatalf("UpdateMatch to MISMATCH: %v", err)
	}
	if err := s.DisputeIntercompany(ctx, entry.IntercompanyEntryID, "disputer-1", "amount looks wrong", "corr-dispute"); err != nil {
		t.Fatalf("DisputeIntercompany: %v", err)
	}

	// A stale match result computed from the earlier MISMATCH read.
	entry.MatchStatus = domain.MatchStatusMatched
	entry.MismatchReason = nil
	err := s.UpdateMatch(ctx, entry, "actor-2", "corr-3")
	if !errors.Is(err, domain.ErrInvalidPairTransition) {
		t.Fatalf("expected ErrInvalidPairTransition, got %v", err)
	}
	got, err := s.GetEntry(ctx, entry.IntercompanyEntryID)
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if got.MatchStatus != domain.MatchStatusDisputed {
		t.Fatalf("expected the pair to stay DISPUTED, got %q", got.MatchStatus)
	}

	// An id that does not exist is still reported as not found.
	missing := *entry
	missing.IntercompanyEntryID = uuid.New().String()
	if err := s.UpdateMatch(ctx, &missing, "actor-2", "corr-4"); !errors.Is(err, domain.ErrEntryNotFound) {
		t.Fatalf("expected ErrEntryNotFound for an unknown id, got %v", err)
	}
}
