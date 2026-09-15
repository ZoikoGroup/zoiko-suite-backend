package store_test

import (
	"testing"

	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/domain"
	"zoiko.io/workflow-svc/internal/store"
)

func TestPgStore_SignOff_RequiresAssignmentAndBindsFingerprint(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)
	eng := newAuditEngagementForTest(t, s, ctx, "eng-rev-1")

	wp, _, err := s.CreateWorkpaper(ctx, domain.CreateWorkpaperParams{
		EngagementID: eng.EngagementID, TenantID: testTenantID, Reference: "WP-REV-1", Purpose: "Test AP confirmations",
		Required: true, CreatedByPrincipalID: "preparer-1", CorrelationID: "wp-rev-create-1",
	})
	if err != nil {
		t.Fatalf("create workpaper: %v", err)
	}
	if _, _, err := s.MarkWorkpaperPrepared(ctx, domain.MarkWorkpaperPreparedParams{WorkpaperID: wp.WorkpaperID, TenantID: testTenantID, ActorPrincipalID: "preparer-1", CorrelationID: "wp-rev-prep-1"}); err != nil {
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
	locked, _, err := s.LockWorkpaper(ctx, domain.LockWorkpaperParams{WorkpaperID: wp.WorkpaperID, TenantID: testTenantID, ActorPrincipalID: "reviewer-1", CorrelationID: "wp-rev-lock-1"})
	if err != nil {
		t.Fatalf("lock workpaper: %v", err)
	}

	scope, created, err := s.OpenReview(ctx, domain.OpenReviewParams{
		EngagementID: eng.EngagementID, TenantID: testTenantID, TargetType: domain.ReviewTargetWorkpaper, TargetID: wp.WorkpaperID,
		InitiatedByPrincipalID: "preparer-1", CorrelationID: "review-open-1",
	})
	if err != nil || !created {
		t.Fatalf("open review: created=%v err=%v", created, err)
	}

	// AssignReviewer refuses the workpaper's own preparer — "no self-review."
	if _, err := s.AssignReviewer(ctx, domain.AssignReviewerParams{ReviewScopeID: scope.ReviewScopeID, TenantID: testTenantID, ReviewerPrincipalID: "preparer-1", Role: "REVIEWER"}); err != domain.ErrSelfReviewNotAllowed {
		t.Fatalf("expected ErrSelfReviewNotAllowed assigning the preparer as reviewer, got %v", err)
	}

	// SignOff by an unassigned reviewer is refused — "role/assignment
	// checked server-side."
	if _, err := s.SignOff(ctx, domain.SignOffParams{ReviewScopeID: scope.ReviewScopeID, TenantID: testTenantID, ActorPrincipalID: "reviewer-1", ContentFingerprint: *locked.LockDigest, CorrelationID: "signoff-unassigned-1"}); err != domain.ErrReviewAssignmentRequired {
		t.Fatalf("expected ErrReviewAssignmentRequired for an unassigned reviewer, got %v", err)
	}

	if _, err := s.AssignReviewer(ctx, domain.AssignReviewerParams{ReviewScopeID: scope.ReviewScopeID, TenantID: testTenantID, ReviewerPrincipalID: "reviewer-1", Role: "REVIEWER"}); err != nil {
		t.Fatalf("assign reviewer: %v", err)
	}

	signOff, err := s.SignOff(ctx, domain.SignOffParams{ReviewScopeID: scope.ReviewScopeID, TenantID: testTenantID, ActorPrincipalID: "reviewer-1", ContentFingerprint: *locked.LockDigest, CorrelationID: "signoff-1"})
	if err != nil {
		t.Fatalf("sign off: %v", err)
	}
	if signOff.Status != domain.SignOffValid || signOff.ContentFingerprint != *locked.LockDigest {
		t.Fatalf("expected a VALID sign-off bound to the lock digest, got %+v", signOff)
	}
	if signOff.WorkflowInstanceID == "" {
		t.Fatal("expected a real underlying workflow_instance_id")
	}
}

// TestPgStore_SignOff_BlockedByUnresolvedMandatoryNote is the real proof
// of "unresolved mandatory notes block sign-off."
func TestPgStore_SignOff_BlockedByUnresolvedMandatoryNote(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)
	eng := newAuditEngagementForTest(t, s, ctx, "eng-rev-2")

	wp, _, err := s.CreateWorkpaper(ctx, domain.CreateWorkpaperParams{
		EngagementID: eng.EngagementID, TenantID: testTenantID, Reference: "WP-REV-2", Purpose: "Test fixed asset additions",
		Required: true, CreatedByPrincipalID: "preparer-2", CorrelationID: "wp-rev-create-2",
	})
	if err != nil {
		t.Fatalf("create workpaper: %v", err)
	}
	if _, _, err := s.MarkWorkpaperPrepared(ctx, domain.MarkWorkpaperPreparedParams{WorkpaperID: wp.WorkpaperID, TenantID: testTenantID, ActorPrincipalID: "preparer-2", CorrelationID: "wp-rev-prep-2"}); err != nil {
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
	locked, _, err := s.LockWorkpaper(ctx, domain.LockWorkpaperParams{WorkpaperID: wp.WorkpaperID, TenantID: testTenantID, ActorPrincipalID: "reviewer-2", CorrelationID: "wp-rev-lock-2"})
	if err != nil {
		t.Fatalf("lock workpaper: %v", err)
	}
	scope, _, err := s.OpenReview(ctx, domain.OpenReviewParams{
		EngagementID: eng.EngagementID, TenantID: testTenantID, TargetType: domain.ReviewTargetWorkpaper, TargetID: wp.WorkpaperID,
		InitiatedByPrincipalID: "preparer-2", CorrelationID: "review-open-2",
	})
	if err != nil {
		t.Fatalf("open review: %v", err)
	}
	if _, err := s.AssignReviewer(ctx, domain.AssignReviewerParams{ReviewScopeID: scope.ReviewScopeID, TenantID: testTenantID, ReviewerPrincipalID: "reviewer-2", Role: "REVIEWER"}); err != nil {
		t.Fatalf("assign reviewer: %v", err)
	}
	note, err := s.RaiseReviewNote(ctx, domain.RaiseReviewNoteParams{ReviewScopeID: scope.ReviewScopeID, TenantID: testTenantID, RaisedByPrincipalID: "reviewer-2", Body: "Please clarify useful life assumption", Mandatory: true})
	if err != nil {
		t.Fatalf("raise review note: %v", err)
	}

	if _, err := s.SignOff(ctx, domain.SignOffParams{ReviewScopeID: scope.ReviewScopeID, TenantID: testTenantID, ActorPrincipalID: "reviewer-2", ContentFingerprint: *locked.LockDigest, CorrelationID: "signoff-blocked-1"}); err != domain.ErrReviewNoteMandatoryUnresolved {
		t.Fatalf("expected ErrReviewNoteMandatoryUnresolved with an unresolved mandatory note, got %v", err)
	}

	if _, err := s.ResolveReviewNote(ctx, domain.ResolveReviewNoteParams{ReviewNoteID: note.ReviewNoteID, TenantID: testTenantID, ActorPrincipalID: "reviewer-2"}); err != nil {
		t.Fatalf("resolve review note: %v", err)
	}
	if _, err := s.SignOff(ctx, domain.SignOffParams{ReviewScopeID: scope.ReviewScopeID, TenantID: testTenantID, ActorPrincipalID: "reviewer-2", ContentFingerprint: *locked.LockDigest, CorrelationID: "signoff-unblocked-1"}); err != nil {
		t.Fatalf("expected sign-off to succeed once the mandatory note is resolved: %v", err)
	}
}

// TestPgStore_AddPostLockAddendum_InvalidatesValidSignOff is the real
// proof of "changed protected content invalidates downstream sign-offs" —
// a post-lock addendum is the one way a locked workpaper's content can
// still change, and it must invalidate any currently-VALID sign-off in
// the SAME transaction.
func TestPgStore_AddPostLockAddendum_InvalidatesValidSignOff(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)
	eng := newAuditEngagementForTest(t, s, ctx, "eng-rev-3")

	wp, _, err := s.CreateWorkpaper(ctx, domain.CreateWorkpaperParams{
		EngagementID: eng.EngagementID, TenantID: testTenantID, Reference: "WP-REV-3", Purpose: "Test related party disclosures",
		Required: true, CreatedByPrincipalID: "preparer-3", CorrelationID: "wp-rev-create-3",
	})
	if err != nil {
		t.Fatalf("create workpaper: %v", err)
	}
	if _, _, err := s.MarkWorkpaperPrepared(ctx, domain.MarkWorkpaperPreparedParams{WorkpaperID: wp.WorkpaperID, TenantID: testTenantID, ActorPrincipalID: "preparer-3", CorrelationID: "wp-rev-prep-3"}); err != nil {
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
	locked, _, err := s.LockWorkpaper(ctx, domain.LockWorkpaperParams{WorkpaperID: wp.WorkpaperID, TenantID: testTenantID, ActorPrincipalID: "reviewer-3", CorrelationID: "wp-rev-lock-3"})
	if err != nil {
		t.Fatalf("lock workpaper: %v", err)
	}
	scope, _, err := s.OpenReview(ctx, domain.OpenReviewParams{
		EngagementID: eng.EngagementID, TenantID: testTenantID, TargetType: domain.ReviewTargetWorkpaper, TargetID: wp.WorkpaperID,
		InitiatedByPrincipalID: "preparer-3", CorrelationID: "review-open-3",
	})
	if err != nil {
		t.Fatalf("open review: %v", err)
	}
	if _, err := s.AssignReviewer(ctx, domain.AssignReviewerParams{ReviewScopeID: scope.ReviewScopeID, TenantID: testTenantID, ReviewerPrincipalID: "reviewer-3", Role: "REVIEWER"}); err != nil {
		t.Fatalf("assign reviewer: %v", err)
	}
	signOff, err := s.SignOff(ctx, domain.SignOffParams{ReviewScopeID: scope.ReviewScopeID, TenantID: testTenantID, ActorPrincipalID: "reviewer-3", ContentFingerprint: *locked.LockDigest, CorrelationID: "signoff-inv-1"})
	if err != nil {
		t.Fatalf("sign off: %v", err)
	}
	if signOff.Status != domain.SignOffValid {
		t.Fatalf("expected a valid sign-off before the addendum, got %+v", signOff)
	}

	if _, err := s.AddPostLockAddendum(ctx, domain.AddPostLockAddendumParams{
		WorkpaperID: wp.WorkpaperID, TenantID: testTenantID, ActorPrincipalID: "reviewer-3",
		Reason: "late-arriving related-party confirmation", Effect: "conclusion unchanged", Content: "Confirmation received",
	}); err != nil {
		t.Fatalf("add post-lock addendum: %v", err)
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM sign_offs WHERE sign_off_id=$1`, signOff.SignOffID).Scan(&status); err != nil {
		t.Fatalf("query sign_offs: %v", err)
	}
	if status != domain.SignOffInvalidated {
		t.Fatalf("expected the sign-off to be INVALIDATED after a post-lock addendum, got %q", status)
	}

	// AUD-01's own REPORT-stage gate must now see no valid sign-off.
	allValid, _, err := s.GetAuditEngagementReportGates(ctx, testTenantID, eng.EngagementID)
	if err != nil {
		t.Fatalf("get report gates: %v", err)
	}
	if allValid {
		t.Fatal("expected allSignOffsValid=false after the addendum invalidated the sign-off")
	}
}
