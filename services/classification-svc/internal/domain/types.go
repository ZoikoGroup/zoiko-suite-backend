// Package domain defines the authoritative domain types for
// classification-svc (AI-02, ZS-SVC-N-001 §4). This service suggests
// transaction/document/category classifications with evidence and
// confidence — it never holds direct posting/tax/category authority
// where material, and never makes a hidden policy decision: thresholds
// are use-case-specific, and confidence never bypasses review for a
// protected use case, no matter how high.
//
// As with document-extraction-svc, this pass does not implement an
// actual classification model. Classify accepts already-scored label
// candidates and the feature evidence they were scored from as
// caller-supplied evidence; this service governs the review/acceptance
// workflow, model-release drift gating, and audit trail around it.
package domain

import (
	"fmt"
	"time"
)

const (
	PrefixJob          = "clj_"
	PrefixSnapshot     = "cfs_"
	PrefixCandidate    = "clc_"
	PrefixDecision     = "cld_"
	PrefixModelRelease = "cmr_"
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

// ── ModelRelease ─────────────────────────────────────────────────────────────

// ModelRelease is the seller-managed drift gate for one (taxonomy,
// model provider, model version) combination — mutable in place, same
// idiom as search_policies/reconciliation_definitions elsewhere in
// this platform. Classify refuses outright, before any job is created,
// when the invocation's own observed drift exceeds MaxDriftBP: "model
// drift beyond threshold blocks release" is enforced as a hard gate,
// not a warning a caller can ignore.
type ModelRelease struct {
	ModelReleaseID string    `json:"model_release_id"`
	TenantID       string    `json:"tenant_id"`
	TaxonomyID     string    `json:"taxonomy_id"`
	ModelProvider  string    `json:"model_provider"`
	ModelVersion   string    `json:"model_version"`
	MaxDriftBP     int32     `json:"max_drift_bp"`
	CreatedAt      time.Time `json:"created_at"`
	CreatedBy      string    `json:"created_by"`
}

type RegisterModelReleaseRequest struct {
	TaxonomyID    string `json:"taxonomy_id"`
	ModelProvider string `json:"model_provider"`
	ModelVersion  string `json:"model_version"`
	MaxDriftBP    int32  `json:"max_drift_bp"`
}

func (r RegisterModelReleaseRequest) Validate() error {
	if r.TaxonomyID == "" || r.ModelProvider == "" || r.ModelVersion == "" {
		return fmt.Errorf("taxonomy_id, model_provider and model_version are required")
	}
	if r.MaxDriftBP < 0 || r.MaxDriftBP > 10000 {
		return fmt.Errorf("max_drift_bp must be between 0 and 10000")
	}
	return nil
}

// ── ClassificationJob ────────────────────────────────────────────────────────

type JobStatus string

const (
	JobQueued         JobStatus = "Queued"
	JobScored         JobStatus = "Scored"
	JobReviewRequired JobStatus = "ReviewRequired"
	JobAccepted       JobStatus = "Accepted"
	JobRejected       JobStatus = "Rejected"
)

// ClassificationJob is one classification attempt against one object.
// Everything except status is immutable from creation. Protected is
// frozen onto the job at Classify time from the caller's declared
// use-case — a protected job ALWAYS requires review, regardless of how
// confident the top candidate is (the doc's own "high confidence does
// not bypass protected review" acceptance test).
type ClassificationJob struct {
	JobID           string    `json:"job_id"`
	TenantID        string    `json:"tenant_id"`
	ObjectRef       string    `json:"object_ref"`
	ObjectType      string    `json:"object_type"`
	TaxonomyID      string    `json:"taxonomy_id"`
	TaxonomyVersion int64     `json:"taxonomy_version"`
	ModelProvider   string    `json:"model_provider"`
	ModelVersion    string    `json:"model_version"`
	Protected       bool      `json:"protected"`
	Status          JobStatus `json:"status"`
	CreatedAt       time.Time `json:"created_at"`
	CreatedBy       string    `json:"created_by"`
}

type CandidateInput struct {
	Label        string `json:"label"`
	ConfidenceBP int32  `json:"confidence_bp"`
}

type ClassifyRequest struct {
	ObjectRef                   string            `json:"object_ref"`
	ObjectType                  string            `json:"object_type"`
	TaxonomyID                  string            `json:"taxonomy_id"`
	TaxonomyVersion             int64             `json:"taxonomy_version"`
	ModelProvider               string            `json:"model_provider"`
	ModelVersion                string            `json:"model_version"`
	Protected                   bool              `json:"protected"`
	ReviewConfidenceThresholdBP int32             `json:"review_confidence_threshold_bp"`
	ObservedDriftBP             int32             `json:"observed_drift_bp"`
	Features                    map[string]string `json:"features"`
	Candidates                  []CandidateInput  `json:"candidates"`
}

func (r ClassifyRequest) Validate() error {
	if r.ObjectRef == "" || r.ObjectType == "" || r.TaxonomyID == "" {
		return fmt.Errorf("object_ref, object_type and taxonomy_id are required")
	}
	if r.ModelProvider == "" || r.ModelVersion == "" {
		return fmt.Errorf("model_provider and model_version are required")
	}
	if r.ReviewConfidenceThresholdBP < 0 || r.ReviewConfidenceThresholdBP > 10000 {
		return fmt.Errorf("review_confidence_threshold_bp must be between 0 and 10000")
	}
	if r.ObservedDriftBP < 0 || r.ObservedDriftBP > 10000 {
		return fmt.Errorf("observed_drift_bp must be between 0 and 10000")
	}
	if len(r.Candidates) == 0 {
		return fmt.Errorf("at least one candidate label is required")
	}
	for _, c := range r.Candidates {
		if c.Label == "" {
			return fmt.Errorf("candidate label is required")
		}
		if c.ConfidenceBP < 0 || c.ConfidenceBP > 10000 {
			return fmt.Errorf("candidate confidence_bp must be between 0 and 10000")
		}
	}
	return nil
}

// ── FeatureSnapshot / ClassificationCandidate ───────────────────────────────

// FeatureSnapshot is the immutable evidence of the features a job's
// candidates were scored from — one per job.
type FeatureSnapshot struct {
	SnapshotID  string            `json:"snapshot_id"`
	TenantID    string            `json:"tenant_id"`
	JobID       string            `json:"job_id"`
	Features    map[string]string `json:"features"`
	ContentHash string            `json:"content_hash"`
	CreatedAt   time.Time         `json:"created_at"`
}

// ClassificationCandidate is one ranked label suggestion — immutable
// evidence, read-only. AcceptSuggestion/RejectSuggestion/
// OverrideWithReason all act at the job level, on rank 1.
type ClassificationCandidate struct {
	CandidateID  string    `json:"candidate_id"`
	TenantID     string    `json:"tenant_id"`
	JobID        string    `json:"job_id"`
	Rank         int32     `json:"rank"`
	Label        string    `json:"label"`
	ConfidenceBP int32     `json:"confidence_bp"`
	CreatedAt    time.Time `json:"created_at"`
}

// ── ClassificationDecision ───────────────────────────────────────────────────

type DecisionType string

const (
	DecisionAccepted   DecisionType = "Accepted"
	DecisionRejected   DecisionType = "Rejected"
	DecisionOverridden DecisionType = "Overridden"
)

// ClassificationDecision is the append-only, exactly-once terminal
// decision for a job — auto-recorded by Classify itself when review
// isn't required, or recorded by AcceptSuggestion/RejectSuggestion/
// OverrideWithReason otherwise.
type ClassificationDecision struct {
	DecisionID string       `json:"decision_id"`
	TenantID   string       `json:"tenant_id"`
	JobID      string       `json:"job_id"`
	Decision   DecisionType `json:"decision"`
	FinalLabel string       `json:"final_label,omitempty"`
	Reason     string       `json:"reason,omitempty"`
	Actor      string       `json:"actor"`
	DecidedAt  time.Time    `json:"decided_at"`
}

type RejectSuggestionRequest struct {
	Reason string `json:"reason"`
}

func (r RejectSuggestionRequest) Validate() error {
	if r.Reason == "" {
		return fmt.Errorf("reason is required")
	}
	return nil
}

type OverrideWithReasonRequest struct {
	FinalLabel string `json:"final_label"`
	Reason     string `json:"reason"`
}

func (r OverrideWithReasonRequest) Validate() error {
	if r.FinalLabel == "" {
		return fmt.Errorf("final_label is required")
	}
	if r.Reason == "" {
		return fmt.Errorf("reason is required")
	}
	return nil
}

var (
	ErrModelReleaseNotFound  = errorString("model release is not registered for this taxonomy/provider/version")
	ErrDriftExceedsThreshold = errorString("observed drift exceeds the registered model release's maximum")
	ErrJobNotFound           = errorString("classification job not found")
	ErrJobNotReviewable      = errorString("classification job is not awaiting review")
	ErrIdempotencyKeyReused  = errorString("idempotency key was already used for a different request")
)
