package domain_test

import (
	"testing"

	"zoiko.io/workflow-svc/internal/domain"
)

func TestWorkflowStatus_Constants(t *testing.T) {
	statuses := []string{
		domain.WorkflowStatusPending,
		domain.WorkflowStatusApproved,
		domain.WorkflowStatusRejected,
		domain.WorkflowStatusEscalated,
		domain.WorkflowStatusCancelled,
		domain.WorkflowStatusInvalidated,
	}

	for _, s := range statuses {
		if s == "" {
			t.Errorf("workflow status should not be empty")
		}
	}
}

func TestStageType_Constants(t *testing.T) {
	if domain.StageTypeSingle != "SINGLE" {
		t.Errorf("StageTypeSingle = %q, want SINGLE", domain.StageTypeSingle)
	}
	if domain.StageTypeQuorum != "QUORUM" {
		t.Errorf("StageTypeQuorum = %q, want QUORUM", domain.StageTypeQuorum)
	}
}

func TestQuorumDecision_Constants(t *testing.T) {
	if domain.QuorumDecisionApprove != "APPROVE" {
		t.Errorf("QuorumDecisionApprove = %q, want APPROVE", domain.QuorumDecisionApprove)
	}
	if domain.QuorumDecisionReject != "REJECT" {
		t.Errorf("QuorumDecisionReject = %q, want REJECT", domain.QuorumDecisionReject)
	}
}

func TestQuorumStageStatus_Resolved_Approved(t *testing.T) {
	qs := domain.QuorumStageStatus{
		PoolSize:          5,
		RequiredApprovals: 3,
		ApprovalCount:     3,
		RejectionCount:    0,
		VotesCast:         3,
	}
	outcome, resolved := qs.Resolved()
	if !resolved {
		t.Errorf("should be resolved when approvals >= required")
	}
	if outcome != "APPROVED" {
		t.Errorf("outcome = %q, want APPROVED", outcome)
	}
}

func TestQuorumStageStatus_Resolved_Rejected_MathematicallyImpossible(t *testing.T) {
	// 3 approvals needed, 1 approval cast, 3 rejections, 4 votes cast, pool of 5
	// 1 remaining voter, even if they approve: 1+1=2 < 3, so mathematically impossible
	qs := domain.QuorumStageStatus{
		PoolSize:          5,
		RequiredApprovals: 3,
		ApprovalCount:     1,
		RejectionCount:    3,
		VotesCast:         4,
	}
	outcome, resolved := qs.Resolved()
	if !resolved {
		t.Errorf("should be resolved when mathematically impossible")
	}
	if outcome != "REJECTED" {
		t.Errorf("outcome = %q, want REJECTED", outcome)
	}
}

func TestQuorumStageStatus_Resolved_Pending(t *testing.T) {
	// 3 approvals needed, 2 approvals, 1 rejection, 3 votes cast, pool of 5
	// 2 remaining voters, could still reach 3
	qs := domain.QuorumStageStatus{
		PoolSize:          5,
		RequiredApprovals: 3,
		ApprovalCount:     2,
		RejectionCount:    1,
		VotesCast:         3,
	}
	outcome, resolved := qs.Resolved()
	if resolved {
		t.Errorf("should not be resolved, waiting for more votes")
	}
	if outcome != "" {
		t.Errorf("outcome = %q, want empty", outcome)
	}
}

func TestQuorumStageStatus_Resolved_SingleVoterReached(t *testing.T) {
	qs := domain.QuorumStageStatus{
		PoolSize:          1,
		RequiredApprovals: 1,
		ApprovalCount:     1,
		RejectionCount:    0,
		VotesCast:         1,
	}
	outcome, resolved := qs.Resolved()
	if !resolved || outcome != "APPROVED" {
		t.Errorf("single voter approve should resolve to APPROVED")
	}
}

func TestQuorumStageStatus_Resolved_SingleVoterRejected(t *testing.T) {
	qs := domain.QuorumStageStatus{
		PoolSize:          1,
		RequiredApprovals: 1,
		ApprovalCount:     0,
		RejectionCount:    1,
		VotesCast:         1,
	}
	outcome, resolved := qs.Resolved()
	if !resolved || outcome != "REJECTED" {
		t.Errorf("single voter reject should resolve to REJECTED")
	}
}

func TestErrorConstants_ImplementError(t *testing.T) {
	errors := []error{
		domain.ErrWorkflowNotFound,
		domain.ErrNoStages,
		domain.ErrInvalidTransition,
		domain.ErrWrongApprover,
		domain.ErrStoreUnavailable,
		domain.ErrAuthorizationDenied,
		domain.ErrAuthorizationServiceUnavailable,
		domain.ErrConcurrencyConflict,
		domain.ErrIdempotencyMismatch,
		domain.ErrInitiatorCannotBeApprover,
		domain.ErrSelfApprovalNotAllowed,
		domain.ErrInvalidSubjectFingerprint,
		domain.ErrInvalidSubjectVersion,
		domain.ErrMissingSubjectField,
		domain.ErrWorkflowNotApproved,
		domain.ErrWorkflowInvalidated,
		domain.ErrSubjectFingerprintMismatch,
		domain.ErrSubjectVersionMismatch,
		domain.ErrWorkflowUnboundSubject,
		domain.ErrInvalidReasonCode,
		domain.ErrInvalidStageType,
		domain.ErrQuorumRequiresApprovers,
		domain.ErrQuorumThresholdExceedsPool,
		domain.ErrQuorumDuplicateApprover,
		domain.ErrNotQuorumStage,
		domain.ErrNotEligibleQuorumApprover,
	}

	for _, err := range errors {
		if err == nil {
			t.Errorf("error should not be nil")
		}
		if err.Error() == "" {
			t.Errorf("error.Error() should not be empty: %v", err)
		}
	}
}

func TestFormSubmissionStatus_Constants(t *testing.T) {
	statuses := []string{
		domain.FormSubmissionDraft,
		domain.FormSubmissionSubmitted,
		domain.FormSubmissionValidating,
		domain.FormSubmissionAccepted,
		domain.FormSubmissionRejected,
		domain.FormSubmissionSuperseded,
	}

	for _, s := range statuses {
		if s == "" {
			t.Errorf("form submission status should not be empty")
		}
	}
}

func TestFormFormStatus_Constants(t *testing.T) {
	// FormDefinition status constants (from form.go)
	// DRAFT, PUBLISHED, RETIRED - these are string constants used but not exported as typed constants
	// Just verify they're used consistently in the codebase
}

func TestRiskLevel_Constants(t *testing.T) {
	if domain.RiskLevelLow != "LOW" {
		t.Errorf("RiskLevelLow = %q, want LOW", domain.RiskLevelLow)
	}
	if domain.RiskLevelModerate != "MODERATE" {
		t.Errorf("RiskLevelModerate = %q, want MODERATE", domain.RiskLevelModerate)
	}
	if domain.RiskLevelHigh != "HIGH" {
		t.Errorf("RiskLevelHigh = %q, want HIGH", domain.RiskLevelHigh)
	}
}

func TestRiskStatus_Constants(t *testing.T) {
	if domain.RiskStatusIdentified != "IDENTIFIED" {
		t.Errorf("RiskStatusIdentified = %q, want IDENTIFIED", domain.RiskStatusIdentified)
	}
	if domain.RiskStatusAssessed != "ASSESSED" {
		t.Errorf("RiskStatusAssessed = %q, want ASSESSED", domain.RiskStatusAssessed)
	}
}

func TestRiskCoverage_Constants(t *testing.T) {
	if domain.RiskCoveragePending != "PENDING" {
		t.Errorf("RiskCoveragePending = %q, want PENDING", domain.RiskCoveragePending)
	}
	if domain.RiskCoverageCovered != "COVERED" {
		t.Errorf("RiskCoverageCovered = %q, want COVERED", domain.RiskCoverageCovered)
	}
	if domain.RiskCoveragePendingReassessment != "PENDING_REASSESSMENT" {
		t.Errorf("RiskCoveragePendingReassessment = %q, want PENDING_REASSESSMENT", domain.RiskCoveragePendingReassessment)
	}
}

func TestAuditEngagementStatus_Constants(t *testing.T) {
	statuses := []string{
		domain.AuditEngagementProposed,
		domain.AuditEngagementAcceptanceReview,
		domain.AuditEngagementAccepted,
		domain.AuditEngagementActive,
		domain.AuditEngagementFieldworkComplete,
		domain.AuditEngagementCompletionReview,
		domain.AuditEngagementReportReady,
		domain.AuditEngagementReleased,
		domain.AuditEngagementClosed,
		domain.AuditEngagementWithdrawn,
	}

	for _, s := range statuses {
		if s == "" {
			t.Errorf("audit engagement status should not be empty")
		}
	}
}

func TestAuditPlanStatus_Constants(t *testing.T) {
	if domain.AuditPlanDraft != "DRAFT" {
		t.Errorf("AuditPlanDraft = %q, want DRAFT", domain.AuditPlanDraft)
	}
	if domain.AuditPlanReviewed != "REVIEWED" {
		t.Errorf("AuditPlanReviewed = %q, want REVIEWED", domain.AuditPlanReviewed)
	}
	if domain.AuditPlanApproved != "APPROVED" {
		t.Errorf("AuditPlanApproved = %q, want APPROVED", domain.AuditPlanApproved)
	}
}

func TestWorkpaperStatus_Constants(t *testing.T) {
	statuses := []string{
		domain.WorkpaperDraft,
		domain.WorkpaperInProgress,
		domain.WorkpaperPrepared,
		domain.WorkpaperReviewed,
		domain.WorkpaperLocked,
	}

	for _, s := range statuses {
		if s == "" {
			t.Errorf("workpaper status should not be empty")
		}
	}
}

func TestReviewNoteStatus_Constants(t *testing.T) {
	statuses := []string{
		domain.ReviewNoteOpen,
		domain.ReviewNoteResponded,
		domain.ReviewNoteResolved,
	}

	for _, s := range statuses {
		if s == "" {
			t.Errorf("review note status should not be empty")
		}
	}
}

func TestSignOffStatus_Constants(t *testing.T) {
	statuses := []string{
		domain.SignOffValid,
		domain.SignOffWithdrawn,
		domain.SignOffInvalidated,
	}

	for _, s := range statuses {
		if s == "" {
			t.Errorf("sign off status should not be empty")
		}
	}
}

func TestQualityReviewStatus_Constants(t *testing.T) {
	statuses := []string{
		domain.QualityReviewInProgress,
		domain.QualityReviewCompleted,
	}

	for _, s := range statuses {
		if s == "" {
			t.Errorf("quality review status should not be empty")
		}
	}
}

func TestReviewTarget_Constants(t *testing.T) {
	if domain.ReviewTargetWorkpaper != "WORKPAPER" {
		t.Errorf("ReviewTargetWorkpaper = %q, want WORKPAPER", domain.ReviewTargetWorkpaper)
	}
	if domain.ReviewTargetEngagement != "ENGAGEMENT" {
		t.Errorf("ReviewTargetEngagement = %q, want ENGAGEMENT", domain.ReviewTargetEngagement)
	}
}
