package store_test

import (
	"errors"
	"testing"

	"zoiko.io/document-extraction-svc/internal/domain"
)

// AI-01 Document Extraction, against real Postgres as the NOSUPERUSER
// NOBYPASSRLS app role.

func baseRequest(docRef string, candidates []domain.CandidateInput) domain.ExtractDocumentRequest {
	return domain.ExtractDocumentRequest{
		DocumentRef: docRef, DocumentHash: hashLike(docRef), ExtractionSchemaID: "invoice-v1", ExtractionSchemaVersion: 1,
		ReviewConfidenceThresholdBP: 8000, Classification: "internal", ResidencyRegion: "us-east",
		ModelProvider: "internal-ocr", ModelVersion: "v1.0", PromptVersion: "p1",
		RequestContentHash: hashLike(docRef + "-req"), ResponseContentHash: hashLike(docRef + "-resp"),
		Candidates: candidates,
	}
}

// Happy path: high-confidence, non-protected candidates auto-accept
// and the job reaches Accepted without any manual review.
func TestExtraction_HappyPath_AutoAccept(t *testing.T) {
	f := newFixture(t)
	job := f.extractDocument(orgA, baseRequest("doc-1", []domain.CandidateInput{
		basicCandidate("vendor_name", "Acme Corp", 9500, false),
		basicCandidate("invoice_total", "1250.00", 9900, false),
	}))
	if job.Status != domain.JobAccepted {
		t.Fatalf("job status = %s, want Accepted", job.Status)
	}

	candidates, err := f.s.GetCandidates(f.ctx, orgA, job.JobID)
	if err != nil {
		t.Fatalf("get candidates: %v", err)
	}
	for _, c := range candidates {
		if c.Status != domain.CandidateAccepted {
			t.Fatalf("candidate %s: %+v", c.FieldName, c)
		}
	}

	inv, err := f.s.GetModelInvocation(f.ctx, orgA, job.JobID)
	if err != nil || inv.ModelVersion != "v1.0" {
		t.Fatalf("model invocation: %+v (err=%v)", inv, err)
	}
}

// Doc-named acceptance test: low-confidence protected field requires
// validation — it lands Pending and blocks the job at ReviewRequired
// until a human disposes of it.
func TestExtraction_LowConfidenceProtectedField_RequiresReview(t *testing.T) {
	f := newFixture(t)
	job := f.extractDocument(orgA, baseRequest("doc-2", []domain.CandidateInput{
		basicCandidate("vendor_name", "Acme Corp", 9500, false),
		basicCandidate("tax_id", "12-3456789", 5000, true), // protected, below 8000bp threshold
	}))
	if job.Status != domain.JobReviewRequired {
		t.Fatalf("job status = %s, want ReviewRequired", job.Status)
	}

	candidates, err := f.s.GetCandidates(f.ctx, orgA, job.JobID)
	if err != nil {
		t.Fatalf("get candidates: %v", err)
	}
	var pendingID string
	for _, c := range candidates {
		if c.FieldName == "tax_id" {
			if c.Status != domain.CandidatePending {
				t.Fatalf("protected low-confidence field auto-decided: %+v", c)
			}
			pendingID = c.CandidateID
		} else if c.Status != domain.CandidateAccepted {
			t.Fatalf("non-protected field not auto-accepted: %+v", c)
		}
	}
	if pendingID == "" {
		t.Fatalf("expected a pending candidate")
	}

	accepted, err := f.s.AcceptCandidate(f.ctx, orgA, pendingID, "reviewer-bob", f.claim("AcceptCandidate", pendingID))
	if err != nil {
		t.Fatalf("accept candidate: %v", err)
	}
	if accepted.Status != domain.CandidateAccepted || accepted.DecidedBy != "reviewer-bob" {
		t.Fatalf("accepted candidate: %+v", accepted)
	}

	final, err := f.s.GetJob(f.ctx, orgA, job.JobID)
	if err != nil || final.Status != domain.JobAccepted {
		t.Fatalf("job after last review decision: %+v (err=%v)", final, err)
	}
}

// Doc-named acceptance test: prompt/model upgrade cannot silently
// change accepted extraction workflow — ReprocessWithVersion always
// creates a brand new job; the original job's decided candidates and
// their values must stay exactly as they were.
func TestExtraction_ModelUpgrade_DoesNotChangeAcceptedJob(t *testing.T) {
	f := newFixture(t)
	job := f.extractDocument(orgA, baseRequest("doc-3", []domain.CandidateInput{
		basicCandidate("vendor_name", "Acme Corp", 9500, false),
	}))
	if job.Status != domain.JobAccepted {
		t.Fatalf("job status = %s, want Accepted", job.Status)
	}
	before, err := f.s.GetCandidates(f.ctx, orgA, job.JobID)
	if err != nil || len(before) != 1 {
		t.Fatalf("candidates before reprocess: %v (err=%v)", before, err)
	}

	newJob, err := f.s.ReprocessWithVersion(f.ctx, orgA, job.JobID, domain.ReprocessWithVersionRequest{
		ModelProvider: "internal-ocr", ModelVersion: "v2.0", PromptVersion: "p2",
		RequestContentHash: hashLike("doc-3-req-v2"), ResponseContentHash: hashLike("doc-3-resp-v2"),
		Candidates: []domain.CandidateInput{basicCandidate("vendor_name", "Acme Corporation Ltd", 9900, false)},
	}, "test-operator", f.claim("ReprocessWithVersion", job.JobID))
	if err != nil {
		t.Fatalf("reprocess with version: %v", err)
	}
	if newJob.JobID == job.JobID {
		t.Fatalf("reprocess did not create a new job")
	}
	if newJob.ReprocessedFromJobID == nil || *newJob.ReprocessedFromJobID != job.JobID {
		t.Fatalf("new job's lineage not recorded: %+v", newJob)
	}

	oldJobAfter, err := f.s.GetJob(f.ctx, orgA, job.JobID)
	if err != nil {
		t.Fatalf("get old job after reprocess: %v", err)
	}
	if oldJobAfter.Status != domain.JobAccepted {
		t.Fatalf("old job status changed after reprocess: %+v", oldJobAfter)
	}
	after, err := f.s.GetCandidates(f.ctx, orgA, job.JobID)
	if err != nil || len(after) != 1 || after[0].ExtractedValue != "Acme Corp" {
		t.Fatalf("old job's candidate value changed after reprocess: %+v (err=%v)", after, err)
	}
}

// Doc-named acceptance test: original document remains available
// after candidate rejection — rejecting never touches the job's own
// document_ref/document_hash or the candidate's evidence span.
func TestExtraction_OriginalDocumentAvailable_AfterRejection(t *testing.T) {
	f := newFixture(t)
	job := f.extractDocument(orgA, baseRequest("doc-4", []domain.CandidateInput{
		basicCandidate("tax_id", "99-9999999", 4000, true), // protected, low confidence -> Pending
	}))
	if job.Status != domain.JobReviewRequired {
		t.Fatalf("job status = %s, want ReviewRequired", job.Status)
	}
	candidates, err := f.s.GetCandidates(f.ctx, orgA, job.JobID)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates: %v (err=%v)", candidates, err)
	}
	candID := candidates[0].CandidateID

	rejected, err := f.s.RejectCandidate(f.ctx, orgA, candID, domain.RejectCandidateRequest{Reason: "tax id does not match vendor record"},
		"reviewer-bob", f.claim("RejectCandidate", candID))
	if err != nil {
		t.Fatalf("reject candidate: %v", err)
	}
	if rejected.Status != domain.CandidateRejected {
		t.Fatalf("rejected candidate: %+v", rejected)
	}

	docAfter, err := f.s.GetJob(f.ctx, orgA, job.JobID)
	if err != nil {
		t.Fatalf("get job after rejection: %v", err)
	}
	if docAfter.DocumentRef != "doc-4" || docAfter.DocumentHash != hashLike("doc-4") {
		t.Fatalf("original document reference/hash changed after rejection: %+v", docAfter)
	}

	span, err := f.s.GetEvidenceSpan(f.ctx, orgA, candID)
	if err != nil {
		t.Fatalf("evidence span not available after rejection: %v", err)
	}
	if span.SnippetText != "99-9999999" {
		t.Fatalf("evidence span content changed: %+v", span)
	}
}

// Idempotent replay: retrying ExtractDocument with the same claim key
// must not create a second job.
func TestExtraction_ExtractDocument_IdempotentReplay(t *testing.T) {
	f := newFixture(t)
	req := baseRequest("doc-5", []domain.CandidateInput{basicCandidate("vendor_name", "Acme Corp", 9500, false)})
	claim := f.claim("ExtractDocument", "doc-5")
	first, err := f.s.ExtractDocument(f.ctx, orgA, req, "test-operator", claim)
	if err != nil {
		t.Fatalf("first extract: %v", err)
	}
	_, err = f.s.ExtractDocument(f.ctx, orgA, req, "test-operator", claim)
	var replay *domain.IdempotentReplayError
	if !errors.As(err, &replay) || replay.ResourceID != first.JobID {
		t.Fatalf("replay of ExtractDocument: %v", err)
	}
}

// Tenant isolation: cross-tenant reads are denied by RLS, not an
// application-level filter.
func TestExtraction_TenantIsolation(t *testing.T) {
	f := newFixture(t)
	job := f.extractDocument(orgA, baseRequest("doc-6", []domain.CandidateInput{basicCandidate("vendor_name", "Acme Corp", 9500, false)}))

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
