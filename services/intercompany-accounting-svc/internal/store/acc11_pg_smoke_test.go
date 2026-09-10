package store_test

// ACC-11's own lifecycle store methods (AcknowledgeCounterparty,
// DisputeIntercompany, ResolveMismatch) are exercised only by
// handler_test.go's in-memory stub elsewhere in this package — this file
// is their real-Postgres coverage, against the actual guarded UPDATE ...
// WHERE match_status = $n clauses and the migration 000003 CHECK
// constraint. Skips (not fails) if TEST_DATABASE_URL isn't set.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
	svcmiddleware "zoiko.io/intercompany-accounting-svc/internal/middleware"
	"zoiko.io/intercompany-accounting-svc/internal/store"
)

func TestPgStore_ACC11_FullLifecycle_AcknowledgeDisputeResolve_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	entry := newTestEntry(tenantID, uuid.New().String())
	if _, err := s.CreateEntry(ctx, entry); err != nil {
		t.Fatalf("CreateEntry: %v", err)
	}

	if err := s.AcknowledgeCounterparty(ctx, entry.IntercompanyEntryID, "counterparty-user"); err != nil {
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
	if err := s.UpdateMatch(ctx, entry.IntercompanyEntryID, uuid.New().String(), domain.MatchStatusMismatch, &reason); err != nil {
		t.Fatalf("UpdateMatch: %v", err)
	}

	if err := s.DisputeIntercompany(ctx, entry.IntercompanyEntryID, "disputer-1", "the counterparty amount looks wrong"); err != nil {
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

	if err := s.ResolveMismatch(ctx, entry.IntercompanyEntryID, "resolver-1", "confirmed FX rounding, accepted"); err != nil {
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
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	entry := newTestEntry(tenantID, uuid.New().String()) // starts UNMATCHED
	if _, err := s.CreateEntry(ctx, entry); err != nil {
		t.Fatalf("CreateEntry: %v", err)
	}

	err := s.DisputeIntercompany(ctx, entry.IntercompanyEntryID, "disputer-1", "premature dispute")
	if err == nil {
		t.Fatal("expected DisputeIntercompany to refuse a pair that is still UNMATCHED")
	}
}

// TestPgStore_ACC11_InvalidMatchStatus_RejectedByCheckConstraint proves
// migration 000003's CHECK constraint is real, not just documentation —
// negative-path control for the closed state set.
func TestPgStore_ACC11_InvalidMatchStatus_RejectedByCheckConstraint(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	entry := newTestEntry(tenantID, uuid.New().String())
	if _, err := s.CreateEntry(ctx, entry); err != nil {
		t.Fatalf("CreateEntry: %v", err)
	}

	if err := s.UpdateMatch(ctx, entry.IntercompanyEntryID, uuid.New().String(), "NOT_A_REAL_STATUS", nil); err == nil {
		t.Fatal("expected the CHECK constraint to reject an unrecognized match_status value")
	}
}
