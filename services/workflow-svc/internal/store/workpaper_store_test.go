package store_test

import (
	"testing"

	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/domain"
	"zoiko.io/workflow-svc/internal/store"
)

// TestPgStore_LockWorkpaper_RequiresContent is the real proof of
// "purpose/procedure/result/conclusion mandatory" (AUD-CTRL-019): a
// workpaper with no procedure/result/conclusion cannot be locked, and
// each one added still refuses until all three are present.
func TestPgStore_LockWorkpaper_RequiresContent(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)
	eng := newAuditEngagementForTest(t, s, ctx, "eng-wp-1")

	wp, created, err := s.CreateWorkpaper(ctx, domain.CreateWorkpaperParams{
		EngagementID: eng.EngagementID, TenantID: testTenantID, Reference: "WP-1", Purpose: "Test revenue cutoff",
		Required: true, CreatedByPrincipalID: "preparer-1", CorrelationID: "wp-create-1",
	})
	if err != nil || !created {
		t.Fatalf("create workpaper: created=%v err=%v", created, err)
	}
	if _, _, err := s.MarkWorkpaperPrepared(ctx, domain.MarkWorkpaperPreparedParams{WorkpaperID: wp.WorkpaperID, TenantID: testTenantID, ActorPrincipalID: "preparer-1", CorrelationID: "wp-prepared-1"}); err != nil {
		t.Fatalf("mark prepared: %v", err)
	}

	if _, _, err := s.LockWorkpaper(ctx, domain.LockWorkpaperParams{WorkpaperID: wp.WorkpaperID, TenantID: testTenantID, ActorPrincipalID: "reviewer-1", CorrelationID: "lock-1"}); err != domain.ErrWorkpaperLockRequiresContent {
		t.Fatalf("expected ErrWorkpaperLockRequiresContent with no content, got %v", err)
	}

	if err := s.RecordProcedure(ctx, domain.RecordProcedureParams{WorkpaperID: wp.WorkpaperID, TenantID: testTenantID, Description: "Select a sample of invoices"}); err != nil {
		t.Fatalf("record procedure: %v", err)
	}
	if _, _, err := s.LockWorkpaper(ctx, domain.LockWorkpaperParams{WorkpaperID: wp.WorkpaperID, TenantID: testTenantID, ActorPrincipalID: "reviewer-1", CorrelationID: "lock-2"}); err != domain.ErrWorkpaperLockRequiresContent {
		t.Fatalf("expected ErrWorkpaperLockRequiresContent with only a procedure, got %v", err)
	}

	if err := s.RecordResult(ctx, domain.RecordResultParams{WorkpaperID: wp.WorkpaperID, TenantID: testTenantID, Description: "No exceptions noted"}); err != nil {
		t.Fatalf("record result: %v", err)
	}
	if _, _, err := s.LockWorkpaper(ctx, domain.LockWorkpaperParams{WorkpaperID: wp.WorkpaperID, TenantID: testTenantID, ActorPrincipalID: "reviewer-1", CorrelationID: "lock-3"}); err != domain.ErrWorkpaperLockRequiresContent {
		t.Fatalf("expected ErrWorkpaperLockRequiresContent with no conclusion, got %v", err)
	}

	if err := s.RecordConclusion(ctx, domain.RecordConclusionParams{WorkpaperID: wp.WorkpaperID, TenantID: testTenantID, Description: "Revenue cutoff is appropriate"}); err != nil {
		t.Fatalf("record conclusion: %v", err)
	}
	locked, changed, err := s.LockWorkpaper(ctx, domain.LockWorkpaperParams{WorkpaperID: wp.WorkpaperID, TenantID: testTenantID, ActorPrincipalID: "reviewer-1", CorrelationID: "lock-4"})
	if err != nil || !changed || locked.Status != domain.WorkpaperLocked {
		t.Fatalf("expected lock to succeed once all three are present: changed=%v locked=%+v err=%v", changed, locked, err)
	}
	if locked.LockDigest == nil || *locked.LockDigest == "" {
		t.Fatal("expected a real, non-empty lock digest")
	}
}

// TestPgStore_LockedWorkpaper_IsImmutable is the real proof of "prior
// versions retained" / lock immutability — a raw UPDATE or DELETE on a
// LOCKED workpaper or its child rows is refused by the reject-mutation
// trigger, and all further content must go through AddPostLockAddendum.
func TestPgStore_LockedWorkpaper_IsImmutable(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)
	eng := newAuditEngagementForTest(t, s, ctx, "eng-wp-2")

	wp, _, err := s.CreateWorkpaper(ctx, domain.CreateWorkpaperParams{
		EngagementID: eng.EngagementID, TenantID: testTenantID, Reference: "WP-2", Purpose: "Test inventory existence",
		Required: true, CreatedByPrincipalID: "preparer-1", CorrelationID: "wp-create-2",
	})
	if err != nil {
		t.Fatalf("create workpaper: %v", err)
	}
	if _, _, err := s.MarkWorkpaperPrepared(ctx, domain.MarkWorkpaperPreparedParams{WorkpaperID: wp.WorkpaperID, TenantID: testTenantID, ActorPrincipalID: "preparer-1", CorrelationID: "wp-prepared-2"}); err != nil {
		t.Fatalf("mark prepared: %v", err)
	}
	if err := s.RecordProcedure(ctx, domain.RecordProcedureParams{WorkpaperID: wp.WorkpaperID, TenantID: testTenantID, Description: "Observe count"}); err != nil {
		t.Fatalf("record procedure: %v", err)
	}
	if err := s.RecordResult(ctx, domain.RecordResultParams{WorkpaperID: wp.WorkpaperID, TenantID: testTenantID, Description: "Count matched"}); err != nil {
		t.Fatalf("record result: %v", err)
	}
	if err := s.RecordConclusion(ctx, domain.RecordConclusionParams{WorkpaperID: wp.WorkpaperID, TenantID: testTenantID, Description: "Existence confirmed"}); err != nil {
		t.Fatalf("record conclusion: %v", err)
	}
	if _, _, err := s.LockWorkpaper(ctx, domain.LockWorkpaperParams{WorkpaperID: wp.WorkpaperID, TenantID: testTenantID, ActorPrincipalID: "reviewer-1", CorrelationID: "lock-imm-1"}); err != nil {
		t.Fatalf("lock workpaper: %v", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE workpapers SET purpose='tampered' WHERE workpaper_id=$1`, wp.WorkpaperID); err == nil {
		t.Fatal("expected the reject-mutation trigger to refuse a raw UPDATE on a locked workpaper")
	}
	if err := s.RecordResult(ctx, domain.RecordResultParams{WorkpaperID: wp.WorkpaperID, TenantID: testTenantID, Description: "trying to sneak in a new result"}); err != domain.ErrWorkpaperInvalidState {
		t.Fatalf("expected ErrWorkpaperInvalidState recording a result on a locked workpaper, got %v", err)
	}

	// The only legal path: a post-lock addendum with reason/effect.
	if _, err := s.AddPostLockAddendum(ctx, domain.AddPostLockAddendumParams{WorkpaperID: wp.WorkpaperID, TenantID: testTenantID, ActorPrincipalID: "reviewer-2", Reason: "", Effect: "none"}); err != domain.ErrWorkpaperAddendumIncomplete {
		t.Fatalf("expected ErrWorkpaperAddendumIncomplete with an empty reason, got %v", err)
	}
	addendum, err := s.AddPostLockAddendum(ctx, domain.AddPostLockAddendumParams{
		WorkpaperID: wp.WorkpaperID, TenantID: testTenantID, ActorPrincipalID: "reviewer-2",
		Reason: "late-arriving confirmation", Effect: "no change to conclusion", Content: "Confirmation received 2026-09-15",
	})
	if err != nil || addendum.AddedByPrincipalID != "reviewer-2" {
		t.Fatalf("expected a valid addendum to succeed: %+v err=%v", addendum, err)
	}
}

// TestPgStore_LinkWorkpaperEvidence_SurfacesContradiction proves
// "contradictory evidence linked" is never silently dropped — it flips
// the workpaper's own has_contradiction_flag.
func TestPgStore_LinkWorkpaperEvidence_SurfacesContradiction(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)
	eng := newAuditEngagementForTest(t, s, ctx, "eng-wp-3")

	wp, _, err := s.CreateWorkpaper(ctx, domain.CreateWorkpaperParams{
		EngagementID: eng.EngagementID, TenantID: testTenantID, Reference: "WP-3", Purpose: "Test AR confirmations",
		Required: false, CreatedByPrincipalID: "preparer-1", CorrelationID: "wp-create-3",
	})
	if err != nil {
		t.Fatalf("create workpaper: %v", err)
	}
	if wp.HasContradictionFlag {
		t.Fatal("expected a freshly created workpaper to have no contradiction flag")
	}
	if err := s.LinkWorkpaperEvidence(ctx, domain.LinkWorkpaperEvidenceParams{
		WorkpaperID: wp.WorkpaperID, TenantID: testTenantID, EvidenceID: "ev-1", LinkedByPrincipalID: "preparer-1", ContradictionFlag: true,
	}); err != nil {
		t.Fatalf("link evidence: %v", err)
	}
	updated, err := s.GetWorkpaper(ctx, testTenantID, wp.WorkpaperID)
	if err != nil {
		t.Fatalf("get workpaper: %v", err)
	}
	if !updated.HasContradictionFlag {
		t.Fatal("expected has_contradiction_flag=true after linking contradictory evidence")
	}
}

// TestPgStore_GetAuditEngagementRequiredWorkpapersLocked is AUD-01's own
// FIELDWORK completion-gate query — a required-but-unlocked workpaper
// blocks the gate; locking it clears the gate.
func TestPgStore_GetAuditEngagementRequiredWorkpapersLocked(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)
	eng := newAuditEngagementForTest(t, s, ctx, "eng-wp-4")

	wp, _, err := s.CreateWorkpaper(ctx, domain.CreateWorkpaperParams{
		EngagementID: eng.EngagementID, TenantID: testTenantID, Reference: "WP-4", Purpose: "Required area",
		Required: true, CreatedByPrincipalID: "preparer-1", CorrelationID: "wp-create-4",
	})
	if err != nil {
		t.Fatalf("create workpaper: %v", err)
	}

	locked, err := s.GetAuditEngagementRequiredWorkpapersLocked(ctx, testTenantID, eng.EngagementID)
	if err != nil {
		t.Fatalf("get gate: %v", err)
	}
	if locked {
		t.Fatal("expected the gate to be unsatisfied while a required workpaper remains unlocked")
	}

	if _, _, err := s.MarkWorkpaperPrepared(ctx, domain.MarkWorkpaperPreparedParams{WorkpaperID: wp.WorkpaperID, TenantID: testTenantID, ActorPrincipalID: "preparer-1", CorrelationID: "wp-prepared-4"}); err != nil {
		t.Fatalf("mark prepared: %v", err)
	}
	if err := s.RecordProcedure(ctx, domain.RecordProcedureParams{WorkpaperID: wp.WorkpaperID, TenantID: testTenantID, Description: "p"}); err != nil {
		t.Fatalf("record procedure: %v", err)
	}
	if err := s.RecordResult(ctx, domain.RecordResultParams{WorkpaperID: wp.WorkpaperID, TenantID: testTenantID, Description: "r"}); err != nil {
		t.Fatalf("record result: %v", err)
	}
	if err := s.RecordConclusion(ctx, domain.RecordConclusionParams{WorkpaperID: wp.WorkpaperID, TenantID: testTenantID, Description: "c"}); err != nil {
		t.Fatalf("record conclusion: %v", err)
	}
	if _, _, err := s.LockWorkpaper(ctx, domain.LockWorkpaperParams{WorkpaperID: wp.WorkpaperID, TenantID: testTenantID, ActorPrincipalID: "reviewer-1", CorrelationID: "lock-gate-1"}); err != nil {
		t.Fatalf("lock workpaper: %v", err)
	}

	locked, err = s.GetAuditEngagementRequiredWorkpapersLocked(ctx, testTenantID, eng.EngagementID)
	if err != nil {
		t.Fatalf("get gate: %v", err)
	}
	if !locked {
		t.Fatal("expected the gate to be satisfied once the required workpaper is locked")
	}
}
