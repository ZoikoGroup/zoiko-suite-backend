package store_test

import (
	"errors"
	"testing"

	"zoiko.io/classification-svc/internal/domain"
)

// AI-02 Classification, against real Postgres as the NOSUPERUSER
// NOBYPASSRLS app role.

func baseClassifyRequest(objectRef string, protected bool, candidates []domain.CandidateInput) domain.ClassifyRequest {
	return domain.ClassifyRequest{
		ObjectRef: objectRef, ObjectType: "transaction", TaxonomyID: "expense-categories-v1", TaxonomyVersion: 1,
		ModelProvider: "internal-clf", ModelVersion: "v1.0", Protected: protected, ReviewConfidenceThresholdBP: 8000,
		ObservedDriftBP: 100, Features: map[string]string{"merchant": "Acme"}, Candidates: candidates,
	}
}

// Happy path: a non-protected, high-confidence suggestion auto-accepts
// with a recorded decision.
func TestClassification_HappyPath_AutoAccept(t *testing.T) {
	f := newFixture(t)
	f.registerModelRelease(orgA, "expense-categories-v1", "internal-clf", "v1.0", 500)

	job := f.classify(orgA, baseClassifyRequest("txn-1", false, []domain.CandidateInput{
		{Label: "Office Supplies", ConfidenceBP: 9500},
		{Label: "Software", ConfidenceBP: 300},
	}), "")
	if job.Status != domain.JobAccepted {
		t.Fatalf("job status = %s, want Accepted", job.Status)
	}

	decision, err := f.s.GetDecision(f.ctx, orgA, job.JobID)
	if err != nil {
		t.Fatalf("get decision: %v", err)
	}
	if decision.Decision != domain.DecisionAccepted || decision.FinalLabel != "Office Supplies" {
		t.Fatalf("decision: %+v", decision)
	}
}

// Doc-named acceptance test: high confidence does not bypass protected
// review — a protected job with a 99% confidence top candidate still
// lands ReviewRequired.
func TestClassification_HighConfidence_DoesNotBypassProtectedReview(t *testing.T) {
	f := newFixture(t)
	f.registerModelRelease(orgA, "expense-categories-v1", "internal-clf", "v1.0", 500)

	job := f.classify(orgA, baseClassifyRequest("txn-2", true, []domain.CandidateInput{
		{Label: "Related Party Transfer", ConfidenceBP: 9900},
	}), "")
	if job.Status != domain.JobReviewRequired {
		t.Fatalf("protected job with 99%% confidence: status = %s, want ReviewRequired", job.Status)
	}

	if _, err := f.s.GetDecision(f.ctx, orgA, job.JobID); err == nil {
		t.Fatalf("a job awaiting review should not already have a decision")
	}

	accepted, err := f.s.AcceptSuggestion(f.ctx, orgA, job.JobID, "reviewer-bob", f.claim("AcceptSuggestion", job.JobID))
	if err != nil {
		t.Fatalf("accept suggestion: %v", err)
	}
	if accepted.Status != domain.JobAccepted {
		t.Fatalf("accepted job: %+v", accepted)
	}
	decision, err := f.s.GetDecision(f.ctx, orgA, job.JobID)
	if err != nil || decision.FinalLabel != "Related Party Transfer" {
		t.Fatalf("decision after manual accept: %+v (err=%v)", decision, err)
	}
}

// Doc-named acceptance test: model drift beyond threshold blocks
// release — Classify refuses outright when observed drift exceeds the
// registered model release's maximum, before any job is created.
func TestClassification_ModelDriftBeyondThreshold_BlocksClassify(t *testing.T) {
	f := newFixture(t)
	f.registerModelRelease(orgA, "expense-categories-v1", "internal-clf", "v1.0", 500)

	req := baseClassifyRequest("txn-3", false, []domain.CandidateInput{{Label: "Travel", ConfidenceBP: 9000}})
	req.ObservedDriftBP = 900 // exceeds the registered 500bp max
	_, err := f.s.Classify(f.ctx, orgA, req, "test-operator", f.claim("Classify", "txn-3"))
	if !errors.Is(err, domain.ErrDriftExceedsThreshold) {
		t.Fatalf("classify with excessive drift: %v", err)
	}

	// Classifying against an unregistered model version fails closed too.
	req2 := baseClassifyRequest("txn-3b", false, []domain.CandidateInput{{Label: "Travel", ConfidenceBP: 9000}})
	req2.ModelVersion = "v9.9-unregistered"
	if _, err := f.s.Classify(f.ctx, orgA, req2, "test-operator", f.claim("Classify", "txn-3b")); !errors.Is(err, domain.ErrModelReleaseNotFound) {
		t.Fatalf("classify against unregistered model release: %v", err)
	}
}

// Doc-named acceptance test: a rejected suggestion cannot be reapplied
// invisibly by replay — once RejectSuggestion has run, replaying the
// ORIGINAL Classify request (same idempotency key) must return the
// existing job, never silently create a second, unreviewed attempt or
// resurrect the rejected suggestion into an accepted one.
func TestClassification_RejectedSuggestion_CannotBeReappliedByReplay(t *testing.T) {
	f := newFixture(t)
	f.registerModelRelease(orgA, "expense-categories-v1", "internal-clf", "v1.0", 500)

	req := baseClassifyRequest("txn-4", true, []domain.CandidateInput{{Label: "Related Party Transfer", ConfidenceBP: 9900}})
	claim := f.claim("Classify", "txn-4")
	job, err := f.s.Classify(f.ctx, orgA, req, "test-operator", claim)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}

	rejected, err := f.s.RejectSuggestion(f.ctx, orgA, job.JobID, domain.RejectSuggestionRequest{Reason: "not a related party"},
		"reviewer-bob", f.claim("RejectSuggestion", job.JobID))
	if err != nil {
		t.Fatalf("reject suggestion: %v", err)
	}
	if rejected.Status != domain.JobRejected {
		t.Fatalf("rejected job: %+v", rejected)
	}

	// Replaying the ORIGINAL classify call must not create a new job or
	// touch the rejected one.
	_, err = f.s.Classify(f.ctx, orgA, req, "test-operator", claim)
	var replay *domain.IdempotentReplayError
	if !errors.As(err, &replay) || replay.ResourceID != job.JobID {
		t.Fatalf("replay of original classify after rejection: %v", err)
	}

	// Trying to accept a rejected job's suggestion after the fact is refused.
	if _, err := f.s.AcceptSuggestion(f.ctx, orgA, job.JobID, "reviewer-bob", f.claim("AcceptSuggestion-late", job.JobID)); !errors.Is(err, domain.ErrJobNotReviewable) {
		t.Fatalf("accept suggestion on a rejected job: %v", err)
	}

	stillRejected, err := f.s.GetJob(f.ctx, orgA, job.JobID)
	if err != nil || stillRejected.Status != domain.JobRejected {
		t.Fatalf("job status after replay attempt: %+v (err=%v)", stillRejected, err)
	}
}

// OverrideWithReason accepts a human-chosen label different from the
// model's own top suggestion, always evidenced with a reason.
func TestClassification_OverrideWithReason(t *testing.T) {
	f := newFixture(t)
	f.registerModelRelease(orgA, "expense-categories-v1", "internal-clf", "v1.0", 500)

	job := f.classify(orgA, baseClassifyRequest("txn-5", true, []domain.CandidateInput{
		{Label: "Office Supplies", ConfidenceBP: 8500},
	}), "")
	if job.Status != domain.JobReviewRequired {
		t.Fatalf("job status = %s, want ReviewRequired", job.Status)
	}

	if _, err := f.s.OverrideWithReason(f.ctx, orgA, job.JobID, domain.OverrideWithReasonRequest{FinalLabel: "", Reason: "reason but no label"},
		"reviewer-bob", f.claim("OverrideWithReason-bad", job.JobID)); err == nil {
		t.Fatalf("override without a final_label should have been rejected")
	}

	overridden, err := f.s.OverrideWithReason(f.ctx, orgA, job.JobID, domain.OverrideWithReasonRequest{
		FinalLabel: "IT Equipment", Reason: "receipt shows laptop purchase, not general office supplies",
	}, "reviewer-bob", f.claim("OverrideWithReason", job.JobID))
	if err != nil {
		t.Fatalf("override with reason: %v", err)
	}
	if overridden.Status != domain.JobAccepted {
		t.Fatalf("overridden job status: %+v", overridden)
	}
	decision, err := f.s.GetDecision(f.ctx, orgA, job.JobID)
	if err != nil || decision.Decision != domain.DecisionOverridden || decision.FinalLabel != "IT Equipment" || decision.Reason == "" {
		t.Fatalf("override decision: %+v (err=%v)", decision, err)
	}
}

// Tenant isolation: cross-tenant reads are denied by RLS, not an
// application-level filter.
func TestClassification_TenantIsolation(t *testing.T) {
	f := newFixture(t)
	f.registerModelRelease(orgA, "expense-categories-v1", "internal-clf", "v1.0", 500)
	job := f.classify(orgA, baseClassifyRequest("txn-6", false, []domain.CandidateInput{{Label: "Travel", ConfidenceBP: 9000}}), "")

	if _, err := f.s.GetJob(f.ctx, orgB, job.JobID); !errors.Is(err, domain.ErrJobNotFound) {
		t.Fatalf("cross-tenant get job: %v", err)
	}
	candidates, err := f.s.GetCandidates(f.ctx, orgB, job.JobID)
	if err != nil {
		t.Fatalf("cross-tenant get candidates: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("cross-tenant read leaked candidates: %+v", candidates)
	}
}
