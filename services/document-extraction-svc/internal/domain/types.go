// Package domain defines the authoritative domain types for
// document-extraction-svc (AI-01, ZS-SVC-N-001 §4). This service
// governs the review/acceptance workflow around candidate fields
// extracted from a document, preserving original evidence and the
// validation path — it never owns authoritative invoice/tax/payroll/
// legal facts, never deletes documents, and never grants final
// approvals; an accepted candidate still has to pass through its
// owning domain's own command validation.
//
// This pass does not implement an actual document-understanding model
// — no such infrastructure exists in this codebase. ExtractDocument
// accepts already-produced candidate fields, their evidence spans and
// their model/prompt provenance as caller-supplied evidence, the same
// scoping decision DATA-02 made for rule outcomes: this service
// governs the workflow, versioning and audit trail around extraction
// results, it does not perform extraction itself.
package domain

import (
	"fmt"
	"time"
)

const (
	PrefixJob        = "exj_"
	PrefixInvocation = "miv_"
	PrefixCandidate  = "exc_"
	PrefixSpan       = "evs_"
	PrefixDecision   = "exd_"
)

type errorString string

func (e errorString) Error() string { return string(e) }

type IdempotentReplayError struct {
	ResourceID string
}

func (e *IdempotentReplayError) Error() string {
	return fmt.Sprintf("idempotent replay: resource %s already exists", e.ResourceID)
}

type IdempotencyClaim struct {
	OwnerScope    string
	PrincipalID   string
	Key           string
	Operation     string
	RequestSHA256 string
	ResourceID    string
}

const SellerScope = "seller"

// ── ExtractionJob ────────────────────────────────────────────────────────────

type JobStatus string

const (
	JobQueued         JobStatus = "Queued"
	JobRunning        JobStatus = "Running"
	JobReviewRequired JobStatus = "ReviewRequired"
	JobAccepted       JobStatus = "Accepted"
	JobRejected       JobStatus = "Rejected"
	JobFailed         JobStatus = "Failed"
)

// ExtractionJob is one extraction attempt against one document version.
// Everything about it except status is immutable from creation — a
// prompt/model upgrade never edits an existing job, it only ever
// produces a brand new one via ReprocessWithVersion (see
// ReprocessedFromJobID), which is what keeps an already-accepted
// extraction's workflow from silently changing underneath it.
type ExtractionJob struct {
	JobID                       string    `json:"job_id"`
	TenantID                    string    `json:"tenant_id"`
	DocumentRef                 string    `json:"document_ref"`
	DocumentHash                string    `json:"document_hash"`
	ExtractionSchemaID          string    `json:"extraction_schema_id"`
	ExtractionSchemaVersion     int64     `json:"extraction_schema_version"`
	ReviewConfidenceThresholdBP int32     `json:"review_confidence_threshold_bp"`
	Classification              string    `json:"classification"`
	ResidencyRegion             string    `json:"residency_region"`
	Status                      JobStatus `json:"status"`
	ReprocessedFromJobID        *string   `json:"reprocessed_from_job_id,omitempty"`
	CreatedAt                   time.Time `json:"created_at"`
	CreatedBy                   string    `json:"created_by"`
}

// ModelInvocation is the immutable provenance record of the model call
// that produced a job's candidates — one per job.
type ModelInvocation struct {
	InvocationID        string    `json:"invocation_id"`
	TenantID            string    `json:"tenant_id"`
	JobID               string    `json:"job_id"`
	ModelProvider       string    `json:"model_provider"`
	ModelVersion        string    `json:"model_version"`
	PromptVersion       string    `json:"prompt_version"`
	RequestContentHash  string    `json:"request_content_hash"`
	ResponseContentHash string    `json:"response_content_hash"`
	InvokedAt           time.Time `json:"invoked_at"`
}

type CandidateInput struct {
	FieldName      string            `json:"field_name"`
	ExtractedValue string            `json:"extracted_value"`
	ConfidenceBP   int32             `json:"confidence_bp"`
	Protected      bool              `json:"protected"`
	Span           EvidenceSpanInput `json:"span"`
}

type EvidenceSpanInput struct {
	PageNumber  *int32 `json:"page_number,omitempty"`
	StartOffset int32  `json:"start_offset"`
	EndOffset   int32  `json:"end_offset"`
	SnippetText string `json:"snippet_text"`
}

type ExtractDocumentRequest struct {
	DocumentRef                 string           `json:"document_ref"`
	DocumentHash                string           `json:"document_hash"`
	ExtractionSchemaID          string           `json:"extraction_schema_id"`
	ExtractionSchemaVersion     int64            `json:"extraction_schema_version"`
	ReviewConfidenceThresholdBP int32            `json:"review_confidence_threshold_bp"`
	Classification              string           `json:"classification"`
	ResidencyRegion             string           `json:"residency_region"`
	ModelProvider               string           `json:"model_provider"`
	ModelVersion                string           `json:"model_version"`
	PromptVersion               string           `json:"prompt_version"`
	RequestContentHash          string           `json:"request_content_hash"`
	ResponseContentHash         string           `json:"response_content_hash"`
	Candidates                  []CandidateInput `json:"candidates"`
}

func (r ExtractDocumentRequest) Validate() error {
	if r.DocumentRef == "" {
		return fmt.Errorf("document_ref is required")
	}
	if r.DocumentHash == "" {
		return fmt.Errorf("document_hash is required")
	}
	if r.ExtractionSchemaID == "" {
		return fmt.Errorf("extraction_schema_id is required")
	}
	if r.ReviewConfidenceThresholdBP < 0 || r.ReviewConfidenceThresholdBP > 10000 {
		return fmt.Errorf("review_confidence_threshold_bp must be between 0 and 10000")
	}
	if r.ModelProvider == "" || r.ModelVersion == "" || r.PromptVersion == "" {
		return fmt.Errorf("model_provider, model_version and prompt_version are required")
	}
	if len(r.Candidates) == 0 {
		return fmt.Errorf("at least one candidate is required")
	}
	seen := map[string]bool{}
	for _, c := range r.Candidates {
		if c.FieldName == "" {
			return fmt.Errorf("candidate field_name is required")
		}
		if seen[c.FieldName] {
			return fmt.Errorf("duplicate candidate field_name %q", c.FieldName)
		}
		seen[c.FieldName] = true
		if c.ConfidenceBP < 0 || c.ConfidenceBP > 10000 {
			return fmt.Errorf("candidate confidence_bp must be between 0 and 10000")
		}
		if c.Span.SnippetText == "" {
			return fmt.Errorf("candidate %q requires an evidence span snippet", c.FieldName)
		}
	}
	return nil
}

type ReprocessWithVersionRequest struct {
	ModelProvider       string           `json:"model_provider"`
	ModelVersion        string           `json:"model_version"`
	PromptVersion       string           `json:"prompt_version"`
	RequestContentHash  string           `json:"request_content_hash"`
	ResponseContentHash string           `json:"response_content_hash"`
	Candidates          []CandidateInput `json:"candidates"`
}

func (r ReprocessWithVersionRequest) Validate() error {
	if r.ModelProvider == "" || r.ModelVersion == "" || r.PromptVersion == "" {
		return fmt.Errorf("model_provider, model_version and prompt_version are required")
	}
	if len(r.Candidates) == 0 {
		return fmt.Errorf("at least one candidate is required")
	}
	seen := map[string]bool{}
	for _, c := range r.Candidates {
		if c.FieldName == "" {
			return fmt.Errorf("candidate field_name is required")
		}
		if seen[c.FieldName] {
			return fmt.Errorf("duplicate candidate field_name %q", c.FieldName)
		}
		seen[c.FieldName] = true
		if c.ConfidenceBP < 0 || c.ConfidenceBP > 10000 {
			return fmt.Errorf("candidate confidence_bp must be between 0 and 10000")
		}
	}
	return nil
}

// ── ExtractionCandidate / EvidenceSpan ──────────────────────────────────────

type CandidateStatus string

const (
	CandidatePending  CandidateStatus = "Pending"
	CandidateAccepted CandidateStatus = "Accepted"
	CandidateRejected CandidateStatus = "Rejected"
)

// ExtractionCandidate is one extracted field. Protected+low-confidence
// fields land Pending and require an explicit AcceptCandidate/
// RejectCandidate; everything else is auto-accepted at ExtractDocument
// time. Its evidentiary columns (FieldName/ExtractedValue/
// ConfidenceBP/Protected) never change after creation — only Status
// (and the decision columns) can move, exactly once, Pending ->
// Accepted or Pending -> Rejected.
type ExtractionCandidate struct {
	CandidateID    string          `json:"candidate_id"`
	TenantID       string          `json:"tenant_id"`
	JobID          string          `json:"job_id"`
	FieldName      string          `json:"field_name"`
	ExtractedValue string          `json:"extracted_value"`
	ConfidenceBP   int32           `json:"confidence_bp"`
	Protected      bool            `json:"protected"`
	Status         CandidateStatus `json:"status"`
	CreatedAt      time.Time       `json:"created_at"`
	DecidedAt      *time.Time      `json:"decided_at,omitempty"`
	DecidedBy      string          `json:"decided_by,omitempty"`
}

// EvidenceSpan points at exactly where in the original document a
// candidate's value came from — immutable, and never deleted when its
// candidate is rejected (the doc's own "original document remains
// available after candidate rejection" acceptance test).
type EvidenceSpan struct {
	SpanID      string    `json:"span_id"`
	TenantID    string    `json:"tenant_id"`
	CandidateID string    `json:"candidate_id"`
	PageNumber  *int32    `json:"page_number,omitempty"`
	StartOffset int32     `json:"start_offset"`
	EndOffset   int32     `json:"end_offset"`
	SnippetText string    `json:"snippet_text"`
	CreatedAt   time.Time `json:"created_at"`
}

// ExtractionDecision is the append-only audit record of a human
// disposing of a Pending candidate.
type ExtractionDecision struct {
	DecisionID  string          `json:"decision_id"`
	TenantID    string          `json:"tenant_id"`
	CandidateID string          `json:"candidate_id"`
	Decision    CandidateStatus `json:"decision"`
	Reason      string          `json:"reason,omitempty"`
	Actor       string          `json:"actor"`
	DecidedAt   time.Time       `json:"decided_at"`
}

type RejectCandidateRequest struct {
	Reason string `json:"reason"`
}

func (r RejectCandidateRequest) Validate() error {
	if r.Reason == "" {
		return fmt.Errorf("reason is required")
	}
	return nil
}

var (
	ErrJobNotFound          = errorString("extraction job not found")
	ErrCandidateNotFound    = errorString("extraction candidate not found")
	ErrCandidateNotPending  = errorString("extraction candidate is not pending review")
	ErrJobNotFinal          = errorString("extraction job has not reached a final state")
	ErrIdempotencyKeyReused = errorString("idempotency key was already used for a different request")
)
