package domain

import (
	"errors"
	"fmt"
	"math"
	"time"
)

var (
	ErrAnomalyRecordNotFound   = errors.New("anomaly record not found")
	ErrAnomalyRuleNotFound     = errors.New("anomaly rule not found")
	ErrInvalidStatusTransition = errors.New("invalid status transition")

	// ErrTenantMissing means the request carried no X-Tenant-ID. It is an
	// unauthenticated request, not an empty tenant named "tenant-default-001".
	ErrTenantMissing = errors.New("tenant scope missing")
)

type Severity string

const (
	SeverityLow      Severity = "LOW"
	SeverityMedium   Severity = "MEDIUM"
	SeverityHigh     Severity = "HIGH"
	SeverityCritical Severity = "CRITICAL"
)

type AnomalyStatus string

const (
	StatusOpen               AnomalyStatus = "OPEN"
	StatusUnderInvestigation AnomalyStatus = "UNDER_INVESTIGATION"
	StatusConfirmedAnomaly   AnomalyStatus = "CONFIRMED_ANOMALY"
	StatusFalsePositive      AnomalyStatus = "FALSE_POSITIVE"
	StatusResolved           AnomalyStatus = "RESOLVED"
)

// AnomalyRule defines configurable threshold/Z-score rules for anomaly detection.
type AnomalyRule struct {
	RuleID         string    `json:"rule_id"`
	TenantID       string    `json:"tenant_id"`
	RuleName       string    `json:"rule_name"`
	DomainName     string    `json:"domain_name"`
	MetricType     string    `json:"metric_type"`
	ThresholdValue float64   `json:"threshold_value"`
	ZScoreCutoff   float64   `json:"z_score_cutoff"`
	IsActive       bool      `json:"is_active"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// AnomalyRecord represents a detected variance or anomaly event.
type AnomalyRecord struct {
	AnomalyID       string        `json:"anomaly_id"`
	TenantID        string        `json:"tenant_id"`
	LegalEntityID   string        `json:"legal_entity_id"`
	DomainName      string        `json:"domain_name"`
	SourceEntityID  string        `json:"source_entity_id"`
	RuleID          string        `json:"rule_id,omitempty"`
	Severity        Severity      `json:"severity"`
	AnomalyScore    float64       `json:"anomaly_score"`
	ObservedValue   float64       `json:"observed_value"`
	ExpectedValue   float64       `json:"expected_value"`
	Description     string        `json:"description"`
	Status          AnomalyStatus `json:"status"`
	InvestigatedBy  string        `json:"investigated_by,omitempty"`
	InvestigatedAt  *time.Time    `json:"investigated_at,omitempty"`
	ResolutionNotes string        `json:"resolution_notes,omitempty"`
	DetectedAt      time.Time     `json:"detected_at"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
}

// DetectAnomalyRequest payload to analyze transaction metrics against baseline.
type DetectAnomalyRequest struct {
	LegalEntityID  string  `json:"legal_entity_id"`
	DomainName     string  `json:"domain_name"`
	SourceEntityID string  `json:"source_entity_id"`
	RuleID         string  `json:"rule_id,omitempty"`
	MetricType     string  `json:"metric_type"`
	ObservedValue  float64 `json:"observed_value"`
	ExpectedValue  float64 `json:"expected_value"`
	StdDeviation   float64 `json:"std_deviation,omitempty"`
	Description    string  `json:"description,omitempty"`
}

// UpdateStatusRequest payload to transition anomaly investigation state.
type UpdateStatusRequest struct {
	Status          AnomalyStatus `json:"status"`
	InvestigatedBy  string        `json:"investigated_by"`
	ResolutionNotes string        `json:"resolution_notes,omitempty"`
}

// CreateRuleRequest payload to register a new detection rule.
type CreateRuleRequest struct {
	RuleName       string  `json:"rule_name"`
	DomainName     string  `json:"domain_name"`
	MetricType     string  `json:"metric_type"`
	ThresholdValue float64 `json:"threshold_value"`
	ZScoreCutoff   float64 `json:"z_score_cutoff"`
}

// ─────────────────────────────────────────────────────────────────────────────
// AI-04 governed advisory layer (ZS-SVC-N-001 §4/§13 Wave 8)
//
// This section is additive to the Phase 6 rule-engine types above and
// does not alter them. It surfaces unusual records for review under a
// governed baseline/model registry with drift gating and an immutable
// evidence trail — it never performs an autonomous adverse action, a
// fraud accusation, a write-off, or a payment freeze without a
// governed rule/human decision. An anomaly score, however high, closes
// no financial action by itself: CloseSignal only ever writes a
// ReviewDisposition evidence row in this service's own tables.
//
// As with document-extraction-svc and classification-svc elsewhere in
// this platform, RunDetection accepts already-scored signal candidates
// (score, severity, feature evidence) as caller-supplied evidence
// against a governed baseline/model registration; this layer governs
// the review/disposition workflow, drift gating, and audit trail
// around them.
// ─────────────────────────────────────────────────────────────────────────────

const (
	PrefixAnomalyModel  = "amd_"
	PrefixDetectionRun  = "dtr_"
	PrefixAnomalySignal = "ans_"
	PrefixDisposition   = "rvd_"
)

type governedErrorString string

func (e governedErrorString) Error() string { return string(e) }

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

// AnomalyModel is the seller-managed baseline/model registration for
// one (domain, model provider, model version) combination — mutable in
// place. RunDetection refuses outright, before any signal is created,
// when the invocation's own observed drift exceeds MaxDriftBP.
type AnomalyModel struct {
	ModelID           string    `json:"model_id"`
	TenantID          string    `json:"tenant_id"`
	DomainName        string    `json:"domain_name"`
	ModelProvider     string    `json:"model_provider"`
	ModelVersion      string    `json:"model_version"`
	ReviewThresholdBP int32     `json:"review_threshold_bp"`
	MaxDriftBP        int32     `json:"max_drift_bp"`
	CreatedAt         time.Time `json:"created_at"`
	CreatedBy         string    `json:"created_by"`
}

type RegisterAnomalyModelRequest struct {
	DomainName        string `json:"domain_name"`
	ModelProvider     string `json:"model_provider"`
	ModelVersion      string `json:"model_version"`
	ReviewThresholdBP int32  `json:"review_threshold_bp"`
	MaxDriftBP        int32  `json:"max_drift_bp"`
}

func (r RegisterAnomalyModelRequest) Validate() error {
	if r.DomainName == "" || r.ModelProvider == "" || r.ModelVersion == "" {
		return fmt.Errorf("domain_name, model_provider and model_version are required")
	}
	if r.ReviewThresholdBP < 0 || r.ReviewThresholdBP > 10000 {
		return fmt.Errorf("review_threshold_bp must be between 0 and 10000")
	}
	if r.MaxDriftBP < 0 || r.MaxDriftBP > 10000 {
		return fmt.Errorf("max_drift_bp must be between 0 and 10000")
	}
	return nil
}

// DetectionRun is one RunDetection call's immutable provenance — the
// baseline/model version and the caller's observed drift are frozen
// here at creation time.
type DetectionRun struct {
	RunID           string    `json:"run_id"`
	TenantID        string    `json:"tenant_id"`
	ModelID         string    `json:"model_id"`
	DomainName      string    `json:"domain_name"`
	ModelProvider   string    `json:"model_provider"`
	ModelVersion    string    `json:"model_version"`
	ObservedDriftBP int32     `json:"observed_drift_bp"`
	BusinessContext string    `json:"business_context"`
	CreatedAt       time.Time `json:"created_at"`
	CreatedBy       string    `json:"created_by"`
}

type GovernedSignalInput struct {
	SourceEntityRef string            `json:"source_entity_ref"`
	Severity        string            `json:"severity"`
	AnomalyScoreBP  int32             `json:"anomaly_score_bp"`
	Features        map[string]string `json:"features"`
}

type RunDetectionRequest struct {
	DomainName      string                `json:"domain_name"`
	ModelProvider   string                `json:"model_provider"`
	ModelVersion    string                `json:"model_version"`
	ObservedDriftBP int32                 `json:"observed_drift_bp"`
	BusinessContext string                `json:"business_context"`
	Signals         []GovernedSignalInput `json:"signals"`
}

func (r RunDetectionRequest) Validate() error {
	if r.DomainName == "" || r.ModelProvider == "" || r.ModelVersion == "" {
		return fmt.Errorf("domain_name, model_provider and model_version are required")
	}
	if r.ObservedDriftBP < 0 || r.ObservedDriftBP > 10000 {
		return fmt.Errorf("observed_drift_bp must be between 0 and 10000")
	}
	if len(r.Signals) == 0 {
		return fmt.Errorf("at least one signal is required")
	}
	for _, s := range r.Signals {
		if s.SourceEntityRef == "" {
			return fmt.Errorf("signal source_entity_ref is required")
		}
		if s.Severity != "LOW" && s.Severity != "MEDIUM" && s.Severity != "HIGH" && s.Severity != "CRITICAL" {
			return fmt.Errorf("signal severity must be LOW, MEDIUM, HIGH or CRITICAL")
		}
		if s.AnomalyScoreBP < 0 || s.AnomalyScoreBP > 10000 {
			return fmt.Errorf("signal anomaly_score_bp must be between 0 and 10000")
		}
	}
	return nil
}

type GovernedSignalStatus string

const (
	SignalDetected      GovernedSignalStatus = "Detected"
	SignalReviewPending GovernedSignalStatus = "ReviewPending"
	SignalConfirmed     GovernedSignalStatus = "Confirmed"
	SignalDismissed     GovernedSignalStatus = "Dismissed"
	SignalEscalated     GovernedSignalStatus = "Escalated"
)

// GovernedAnomalySignal is one detected signal under the governed
// advisory layer (distinct from the legacy AnomalyRecord above).
// model_version/model_provider are frozen from the DetectionRun at
// creation — "feature/model version is retained with each signal" —
// and everything except status is immutable thereafter. Dismissed/
// Confirmed/Escalated are terminal: a dismissed signal keeps its full
// record, including score and feature evidence, as evidence for
// evaluation and drift review.
type GovernedAnomalySignal struct {
	SignalID        string               `json:"signal_id"`
	TenantID        string               `json:"tenant_id"`
	RunID           string               `json:"run_id"`
	DomainName      string               `json:"domain_name"`
	ModelProvider   string               `json:"model_provider"`
	ModelVersion    string               `json:"model_version"`
	SourceEntityRef string               `json:"source_entity_ref"`
	Severity        string               `json:"severity"`
	AnomalyScoreBP  int32                `json:"anomaly_score_bp"`
	Features        map[string]string    `json:"features"`
	ContentHash     string               `json:"content_hash"`
	Status          GovernedSignalStatus `json:"status"`
	CreatedAt       time.Time            `json:"created_at"`
}

type CloseSignalRequest struct {
	Outcome GovernedSignalStatus `json:"outcome"`
	Notes   string               `json:"notes"`
}

func (r CloseSignalRequest) Validate() error {
	if r.Outcome != SignalConfirmed && r.Outcome != SignalDismissed {
		return fmt.Errorf("outcome must be Confirmed or Dismissed")
	}
	if r.Notes == "" {
		return fmt.Errorf("notes are required")
	}
	return nil
}

type EscalateForReviewRequest struct {
	Reason string `json:"reason"`
}

func (r EscalateForReviewRequest) Validate() error {
	if r.Reason == "" {
		return fmt.Errorf("reason is required")
	}
	return nil
}

// ReviewDisposition is the append-only, exactly-once terminal
// disposition for a governed signal. It is pure evidence: nothing in
// this layer writes to any payment, write-off, or other domain's table
// as a side effect of a disposition, no matter the outcome or
// severity — "anomaly score alone cannot freeze payment/write off
// balance" holds because this layer has no command capable of doing
// so.
type ReviewDisposition struct {
	DispositionID string               `json:"disposition_id"`
	TenantID      string               `json:"tenant_id"`
	SignalID      string               `json:"signal_id"`
	Outcome       GovernedSignalStatus `json:"outcome"`
	Notes         string               `json:"notes"`
	Actor         string               `json:"actor"`
	DecidedAt     time.Time            `json:"decided_at"`
}

var (
	ErrAnomalyModelNotFound  = governedErrorString("anomaly model is not registered for this domain/provider/version")
	ErrDriftExceedsThreshold = governedErrorString("observed drift exceeds the registered anomaly model's maximum")
	ErrSignalNotFound        = governedErrorString("anomaly signal not found")
	ErrSignalNotDetected     = governedErrorString("anomaly signal is not in Detected status")
	ErrSignalNotReviewable   = governedErrorString("anomaly signal is not ReviewPending")
	ErrIdempotencyKeyReused  = governedErrorString("idempotency key was already used for a different request")
)

// CalculateAnomalyScore determines Z-score & severity rating from observed vs expected values.
func CalculateAnomalyScore(observed, expected, stdDev float64) (float64, Severity) {
	if stdDev <= 0 {
		stdDev = 1.0
	}
	diff := math.Abs(observed - expected)
	zScore := diff / stdDev

	score := math.Round(zScore*100.0) / 100.0

	var severity Severity
	switch {
	case score >= 4.0:
		severity = SeverityCritical
	case score >= 3.0:
		severity = SeverityHigh
	case score >= 2.0:
		severity = SeverityMedium
	default:
		severity = SeverityLow
	}

	return score, severity
}
